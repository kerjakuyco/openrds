// One database: summary, actions and tabs, like a managed-database console.
import { h, icon, btn, api, toast, modal, menu, status, isLive, isRunning, keyValues, panel, infoPanel, scheduleLabel, empty, dataTable, bytes, rate, mib, percent, when, ago, plural, copyText, alertBox, sizeLabel, meter, nextId, clock, chip } from "./lib.mjs";
import { state, subscribe, instance, engine, loadEngines, sync, go, track, can } from "./store.mjs";
import { connectGuide } from "./connect.mjs";
import { timeChart, chartGroup } from "./charts.mjs";
import { databaseActions, takeSnapshot, confirmReboot } from "./actions.mjs";
import { backupTable } from "./backups.mjs";
import { tagEditor } from "./forms.mjs";
import { connectivityPanel, firewallPanel, usersTab } from "./security.mjs";
import { insightsTab, ENGINE_TERMS } from "./insights.mjs";
import { settingsPanel, editParameters, editBackups } from "./settings.mjs";

const TABS = [["connectivity", "Connectivity & security"], ["users", "Database users"], ["monitoring", "Monitoring"], ["insights", "Performance"], ["logs", "Logs & events"],
  ["configuration", "Configuration"], ["backups", "Maintenance & backups"], ["tags", "Tags"]];
let chartRange = "1h";

const pendingChanges = (d) => {
  if (!d.applied) return [];
  const out = [];
  if (d.applied.cpus !== d.cpus) out.push(`CPU ${d.applied.cpus ? `${d.applied.cpus} vCPU` : "unlimited"} → ${d.cpus ? `${d.cpus} vCPU` : "unlimited"}`);
  if (d.applied.memoryMiB !== d.memoryMiB) out.push(`memory ${d.applied.memoryMiB ? mib(d.applied.memoryMiB) : "unlimited"} → ${d.memoryMiB ? mib(d.memoryMiB) : "unlimited"}`);
  if (JSON.stringify(Object.entries(d.applied.parameters || {}).sort()) !== JSON.stringify(Object.entries(d.parameters || {}).sort())) out.push("engine parameters");
  if (Boolean(d.applied.requireTLS) !== Boolean(d.requireTLS)) out.push(d.requireTLS ? "require TLS" : "TLS optional");
  if ((d.applied.publicHost || "") !== (d.publicHost || "")) out.push("new public address");
  return out;
};

function connectivityTab(d) {
  const eng = engine(d.engine);
  const all = state.instances || [];
  const el = h("div", { class: "stack-gap" },
    ...connectivityPanel(d, all),
    connectGuide(d, eng, { canReveal: can("operator") }).el,
    firewallPanel(d, all));
  return { el };
}

function monitoringTab(d) {
  const hostCPUs = state.host?.cpus || 1;
  const base = d.cpus || hostCPUs;
  const group = chartGroup();
  const limit = d.memoryMiB * 1048576;
  const charts = [
    timeChart({ title: "CPU utilization", subtitle: d.cpus ? `Percent of the ${d.cpus} vCPU limit` : `Percent of the host’s ${hostCPUs} vCPU`,
      series: [{ label: "CPU", cls: "s1", value: p => p.cpu / base }], format: percent, axisFormat: v => `${v}%`, domainMax: () => 100 }),
    timeChart({ title: "Memory in use", subtitle: d.memoryMiB ? `Of the ${mib(d.memoryMiB)} limit, without cache` : "Without cache. No limit set.",
      series: [{ label: "Memory", cls: "s1", value: p => p.memory }], format: bytes, binary: true, floor: 1048576,
      domainMax: limit ? () => limit : null, reference: limit ? { value: limit, label: `Limit ${mib(d.memoryMiB)}` } : null }),
    timeChart({ title: "Network throughput", subtitle: "Bytes per second",
      series: [{ label: "In", cls: "s1", value: p => p.netIn }, { label: "Out", cls: "s2", value: p => p.netOut }], format: rate, binary: true, floor: 1024 }),
    timeChart({ title: "Disk throughput", subtitle: "Bytes per second read and written",
      series: [{ label: "Read", cls: "s1", value: p => p.read }, { label: "Write", cls: "s2", value: p => p.write }], format: rate, binary: true, floor: 1024 }),
  ];
  // What the engine reports about itself; readings it did not answer are gaps.
  const terms = ENGINE_TERMS[d.engine];
  const amount = (n) => n < 10 ? n.toFixed(1) : Math.round(n).toLocaleString();
  const engineChart = (options, pick) => Object.assign(timeChart(options), { pick });
  charts.push(
    engineChart({ title: "Connections", subtitle: d.engine === "valkey" ? "Connected clients" : "Client connections, and those running a statement",
      series: [{ label: "Connected", cls: "s1", value: p => p.db.conns }, ...(d.engine === "valkey" ? [] : [{ label: "Running", cls: "s2", value: p => p.db.active ?? 0 }])],
      format: amount, axisFormat: v => String(v) }, pts => pts.filter(p => p.db)),
    engineChart({ title: `${terms.ops} per second`, subtitle: "Throughput reported by the engine",
      series: [{ label: terms.ops, cls: "s1", value: p => p.db.ops }], format: amount, axisFormat: v => String(v) }, pts => pts.filter(p => p.db)),
    engineChart({ title: terms.hit, subtitle: `Share of ${terms.hitWhat}`,
      series: [{ label: "Hit ratio", cls: "s1", value: p => p.db.hit }], format: v => `${v.toFixed(1)}%`, axisFormat: v => `${v}%`, domainMax: () => 100,
      empty: "No reads in this range yet." }, pts => pts.filter(p => p.db && p.db.hit != null)),
  );
  charts.forEach(c => group.add(c));
  const ranges = ["1h", "3h", "6h", "12h", "24h"];
  const buttons = ranges.map(r => h("button", { type: "button", "aria-pressed": String(r === chartRange), onclick: () => { chartRange = r; buttons.forEach(b => b.setAttribute("aria-pressed", String(b.textContent === r))); load(); } }, r));
  const note = h("p", { class: "muted small" });
  const notice = h("div");
  const el = h("div", null, h("div", { class: "filter-row" }, h("div", { class: "segmented", role: "group", "aria-label": "Time range" }, buttons), note),
    notice, h("div", { class: "chart-grid" }, charts.map(c => c.el)));
  let busy = false;
  async function load() {
    if (busy) return;
    busy = true;
    charts.forEach(c => c.setLoading(true));
    try {
      const v = await api(`instances/${d.name}/history?range=${chartRange}`);
      if (!v.available) {
        const u = isLive(d) ? await api(`instances/${d.name}/metrics`).catch(() => null) : null;
        notice.replaceChildren(alertBox(`${v.message} Live readings are shown instead.`, "warn"),
          u ? panel("Live readings", {}, keyValues([["CPU", u.CPUPerc], ["Memory", u.MemUsage], ["Network in / out", u.NetIO], ["Disk read / write", u.BlockIO]], "cols-4")) : null);
      } else notice.replaceChildren();
      const step = Math.max(v.interval, v.range / 240);
      charts.forEach(c => c.update({ points: c.pick ? c.pick(v.points || []) : v.points || [], from: v.now - v.range, to: v.now, step }));
      note.textContent = `Every ${v.interval}s · updated ${clock(Date.now())}`;
    } catch (err) {
      charts.forEach(c => c.setLoading(false));
      note.textContent = err.message;
    } finally { busy = false; }
  }
  load();
  const timer = setInterval(() => { if (!document.hidden) load(); }, 30000);
  return { el, destroy() { clearInterval(timer); charts.forEach(c => c.destroy()); } };
}

function logsTab(d) {
  const output = h("pre", { class: "logs", tabindex: "0", "aria-label": "Container log" }, "Reading the log…");
  let first = true, timer = 0;
  async function load() {
    try {
      const v = await api(`instances/${d.name}/logs`);
      const atEnd = output.scrollTop + output.clientHeight >= output.scrollHeight - 8;
      output.textContent = v.text || "No log lines yet.";
      if (first || atEnd) output.scrollTop = output.scrollHeight;
      first = false;
    } catch (err) { output.textContent = err.message; }
  }
  const follow = h("input", { type: "checkbox", class: "switch", role: "switch", id: nextId("follow") });
  follow.addEventListener("change", () => { clearInterval(timer); if (follow.checked) { load(); timer = setInterval(() => { if (!document.hidden) load(); }, 10000); } });
  const events = h("div", { class: "flush" });
  function renderEvents() {
    const list = (state.activity || []).filter(e => e.name === d.name).slice(0, 25);
    events.replaceChildren(list.length ? dataTable({ caption: "Events", rows: list, columns: [
      { key: "at", label: "Time", value: e => h("span", { title: when(e.at) }, ago(e.at)) },
      { key: "action", label: "Operation", value: e => e.action },
      { key: "result", label: "Outcome", value: e => resultChip(e.result) },
      { key: "detail", label: "Detail", cls: "wrap", value: e => e.detail || h("span", { class: "muted" }, "—") },
    ] }) : empty("bell", "No events yet", "Operations on this database appear here."));
  }
  renderEvents();
  load();
  const el = h("div", { class: "stack-gap" },
    panel("Database log", { description: "Last 120 lines. Passwords hidden.",
      actions: [h("label", { class: "check inline", for: follow.id }, follow, "Follow"), btn([icon("refresh"), "Refresh"], load, "small")] }, output),
    panel("Recent events", { actions: [h("a", { class: "btn small", href: "#/events" }, "All events")] }, events));
  return { el, update: renderEvents, destroy: () => clearInterval(timer) };
}

export const resultChip = (r) => h("span", { class: `status ${r === "completed" ? "good" : r === "failed" ? "critical" : "info"}` },
  icon(r === "completed" ? "ok" : r === "failed" ? "error" : "pending"), r.charAt(0).toUpperCase() + r.slice(1));

// Which memory settings the server sized to the memory limit, for settings
// the user left unset.
function tunedNote(d) {
  const auto = Object.entries(d.tuned || {}).filter(([k]) => !(k in (d.parameters || {}))).sort();
  if (!auto.length) return "Unlisted settings use the default.";
  return `Unlisted settings use the default. Sized to the memory limit: ${auto.map(([k, v]) => `${k} ${v}`).join(", ")}.`;
}

function configurationTab(d) {
  const eng = engine(d.engine);
  const params = Object.entries(d.parameters || {}).sort();
  const applied = d.applied?.parameters || {};
  const removed = d.applied ? Object.keys(applied).filter(k => !(k in (d.parameters || {}))) : [];
  const describe = (name) => eng.parameters.find(p => p.name === name);
  const paramRows = [...params.map(([k, v]) => ({ k, v, pending: Boolean(d.applied) && applied[k] !== v })), ...removed.map(k => ({ k, v: null, pending: true }))];
  const el = h("div", { class: "stack-gap" },
    d.applied && alertBox(`Applies at next restart: ${pendingChanges(d).join(", ")}.`, "info"),
    settingsPanel(d),
    panel("Engine parameters", { counter: params.length, description: tunedNote(d),
      actions: can("operator") ? [editButton(d, [icon("sliders"), "Edit parameters"], () => editParameters(d))] : [] },
    paramRows.length ? dataTable({ caption: "Engine parameters", rows: paramRows, columns: [
      { key: "k", label: "Parameter", cls: "wrap", value: r => h("div", { class: "stack" }, h("code", null, r.k), h("small", null, describe(r.k)?.description || "")) },
      { key: "v", label: "Value", value: r => r.v === null ? h("span", { class: "muted" }, "Default") : h("code", null, r.v === "" ? "(empty)" : r.v) },
      { key: "d", label: "Default", value: r => h("code", { class: "muted" }, d.tuned?.[r.k] || describe(r.k)?.default || "(empty)") },
      { key: "s", label: "Status", value: r => r.pending ? h("span", { class: "status info" }, icon("pending"), "Pending restart") : h("span", { class: "status good" }, icon("ok"), "Applied") },
    ] }) : empty("sliders", "Engine defaults", "No custom settings.")),
    infoPanel("Details", [
      ["Instance", [["DB identifier", d.name], ["Engine", d.engineLabel], ["Created", when(d.createdAt)]]],
      ["Database", [[d.engine === "valkey" ? "Database number" : "Database name", h("code", null, eng.database)], ["Owner user", h("code", null, eng.user)], ["Database users", String(1 + (d.users || []).length)]]],
    ]));
  return { el };
}

// A button that opens a settings editor, unless the database is busy.
function editButton(d, label, run) {
  const b = btn(label, run, "small");
  b.disabled = d.busy || d.pendingRestore;
  return b;
}

function backupsTab(d) {
  const list = h("div", { class: "flush" });
  function render(cur) {
    const mine = (state.backups || []).filter(b => b.name === cur.name && Date.parse(b.createdAt) >= Date.parse(cur.createdAt));
    list.replaceChildren(mine.length ? backupTable(mine, { showInstance: false }) : empty("camera", "No backups of this database yet", isRunning(cur) ? "Take a snapshot or turn on backups." : "Start the database to take a snapshot."));
  }
  const toggle = btn(d.deletionProtection ? "Turn off" : "Turn on", () => modal({
    title: d.deletionProtection ? "Turn off deletion protection?" : "Turn on deletion protection?", kicker: d.name, confirm: d.deletionProtection ? "Turn off" : "Turn on",
    body: h("p", { class: "muted" }, d.deletionProtection ? "Operators will be able to delete it." : "It can’t be deleted until you turn this off."),
    run: async () => {
      await api(`instances/${d.name}/modify`, { method: "POST", body: { deletionProtection: !d.deletionProtection } });
      toast(`Deletion protection is ${d.deletionProtection ? "off" : "on"} for ${d.name}.`, "success");
      await sync();
    },
  }), "small");
  const snap = btn([icon("camera"), "Take snapshot"], () => takeSnapshot(d), "small primary");
  snap.disabled = d.busy || !isRunning(d);
  const on = d.backupRetentionDays > 0;
  const el = h("div", { class: "stack-gap" },
    infoPanel("Maintenance & backups", [
      ["Backup", [["Automated backups", on ? h("span", { class: "status good" }, icon("ok"), "On") : h("span", { class: "status warn" }, icon("alert"), "Off")],
        ["Retention period", on ? plural(d.backupRetentionDays, "day") : "—"], ["Schedule", on ? scheduleLabel(d.backupWindow, d.backupIntervalHours) : "—"]]],
      ["Recovery points", [["Latest backup", d.latestBackupAt ? `${when(d.latestBackupAt)} (${ago(d.latestBackupAt)})` : "None yet"],
        ["Next automated backup", d.nextBackupAt ? `${when(d.nextBackupAt)} (${ago(d.nextBackupAt)})` : "—"]]],
      ["Protection", [["Deletion protection", h("span", { class: "row tight" }, d.deletionProtection ? "On" : "Off", can("operator") ? toggle : null)]]],
    ], { actions: can("operator") ? [editButton(d, [icon("edit"), "Change"], () => editBackups(d)), snap] : [] }),
    panel("Snapshots and backups", { description: "Restoring creates a new database." }, list));
  render(d);
  return { el, update: render };
}

function tagsTab(d) {
  const tags = Object.entries(d.tags || {}).sort();
  const manage = btn([icon("tag"), "Manage tags"], () => {
    const editor = tagEditor(d.tags);
    modal({ title: "Manage tags", kicker: d.name, confirm: "Save tags", wide: true, body: [h("p", { class: "muted" }, "Up to 50 tags."), editor.el],
      run: async () => {
        await api(`instances/${d.name}/modify`, { method: "POST", body: { tags: editor.value() } });
        toast(`Tags saved for ${d.name}.`, "success");
        await sync();
      } });
  }, "small");
  const el = panel("Tags", { counter: tags.length, description: "Labels you can search by.", actions: can("operator") ? [manage] : [] },
    tags.length ? dataTable({ caption: "Tags", rows: tags, columns: [{ key: "k", label: "Key", value: ([k]) => h("code", null, k) }, { key: "v", label: "Value", value: ([, v]) => v || h("span", { class: "muted" }, "(empty)") }] })
      : empty("tag", "No tags", "For example env = production."));
  return { el };
}

const BUILD = { connectivity: connectivityTab, users: usersTab, monitoring: monitoringTab, insights: insightsTab, logs: logsTab, configuration: configurationTab, backups: backupsTab, tags: tagsTab };
// What each tab shows from the inventory. A tab is rebuilt only when this
// changes, so a revealed URI, a log scroll position or a chart range survives a refresh.
const SIGNATURE = {
  connectivity: d => JSON.stringify([d.port, d.status, d.busy, d.users, d.publicAccess, d.publicPort, d.publicHost, d.firewall, d.requireTLS, d.tlsReady, state.me?.role]),
  users: d => JSON.stringify([d.status, d.busy, d.users, d.tlsReady, d.requireTLS, d.port, state.me?.role]),
  monitoring: d => JSON.stringify([d.cpus, d.memoryMiB, state.host?.cpus]),
  insights: d => JSON.stringify([d.status, d.pendingRestore, state.me?.role]),
  logs: () => "",
  configuration: d => JSON.stringify([d.status, d.busy, d.pendingRestore, d.cpus, d.memoryMiB, d.diskGiB, d.port, d.parameters, d.applied, d.deletionProtection,
    d.requireTLS, d.tlsReady, d.backupRetentionDays, d.backupWindow, d.backupIntervalHours, d.users, state.me?.role]),
  backups: d => JSON.stringify([d.busy, d.status, d.deletionProtection, d.backupRetentionDays, d.backupWindow, d.backupIntervalHours, d.latestBackupAt, d.nextBackupAt]),
  tags: d => JSON.stringify(d.tags || {}),
};

function summary(d) {
  const base = d.cpus || state.host?.cpus || 1;
  const live = d.live;
  const cpu = live ? live.cpu / base : null;
  const memShare = live && d.memoryMiB ? live.memory / (d.memoryMiB * 1048576) * 100 : null;
  return infoPanel("Summary", [
    [null, [["DB identifier", d.name], ["Engine", d.engineLabel]]],
    [null, [["Status", status(d)], ["Access", h("span", { class: "with-icon" }, icon(d.publicAccess ? "globe" : "lock"), d.publicAccess ? `Public · port ${d.publicPort}` : "Private", d.requireTLS ? " · TLS" : "")]]],
    [null, [["Size", sizeLabel(d)], ["CPU", live ? h("span", { class: "with-meter" }, percent(cpu), meter(cpu, 100, `CPU ${percent(cpu)} of ${d.cpus ? "the limit" : "the host"}`)) : "—"]]],
    [null, [["Memory", live ? h("span", { class: "with-meter" }, `${bytes(live.memory)}${d.memoryMiB ? ` of ${mib(d.memoryMiB)}` : ""}`, memShare !== null && meter(memShare, 100, `Memory ${percent(memShare)} of the limit`)) : "—"],
      ["Automated backups", d.backupRetentionDays ? `${plural(d.backupRetentionDays, "day")} retention` : h("span", { class: "status warn" }, icon("alert"), "Off")]]],
  ], { cls: "summary-panel" });
}

function banners(d) {
  const out = [];
  if (d.busy) out.push(alertBox("An operation is running. Please wait.", "info"));
  if (d.pendingRestore && !d.busy) out.push(alertBox("The restore didn’t finish, so data is incomplete. Check the logs, then restore again.", "error"));
  if (d.status === "unhealthy" && !d.busy) out.push(alertBox("Health check failing. Check the logs.", "error"));
  if (d.status === "not provisioned" && !d.busy) out.push(alertBox("The container is missing. Use Actions → Start to retry, or delete it.", "warn"));
  if (d.applied) {
    const reboot = can("operator") ? btn([icon("reboot"), "Reboot now"], () => confirmReboot(d), "small") : null;
    if (reboot) reboot.disabled = d.busy || !isLive(d);
    out.push(alertBox(`Changes apply at the next restart: ${pendingChanges(d).join(", ")}.`, "info", " ", reboot));
  }
  return out;
}

export async function instanceView(name, tab = "connectivity") {
  await loadEngines();
  if (!state.instances) await sync();
  const head = h("div", { class: "page-head" });
  const alerts = h("div", { class: "stack-gap" });
  const summarySlot = h("div");
  const tabButtons = TABS.map(([key, label]) => h("button", { type: "button", role: "tab", id: `tab-${key}`, "aria-controls": "tab-panel", onclick: () => go(`#/databases/${name}/${key}`) }, label));
  const tablist = h("div", { class: "tabs", role: "tablist", "aria-label": "Database sections" }, tabButtons);
  tablist.addEventListener("keydown", (e) => {
    const i = tabButtons.indexOf(document.activeElement);
    if (i < 0 || !["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key)) return;
    e.preventDefault();
    const next = e.key === "Home" ? 0 : e.key === "End" ? tabButtons.length - 1 : (i + (e.key === "ArrowRight" ? 1 : -1) + tabButtons.length) % tabButtons.length;
    tabButtons[next].focus();
    go(`#/databases/${name}/${TABS[next][0]}`);
  });
  const panelEl = h("div", { id: "tab-panel", role: "tabpanel", class: "tab-panel", tabindex: "-1" });
  const body = h("div", null, alerts, summarySlot, tablist, panelEl);
  const el = h("div", { class: "instance-page" }, head, body);
  let current = null, currentKey = "", currentSig = "", headSig = "", bannerSig = "", gone = false;

  function setTab(key) {
    if (!BUILD[key]) key = "connectivity";
    const d = instance(name);
    tabButtons.forEach((b, i) => {
      const on = TABS[i][0] === key;
      b.setAttribute("aria-selected", String(on));
      b.tabIndex = on ? 0 : -1;
    });
    panelEl.setAttribute("aria-labelledby", `tab-${key}`);
    if (!d) return;
    current?.destroy?.();
    currentKey = key;
    currentSig = SIGNATURE[key](d);
    current = BUILD[key](d);
    panelEl.replaceChildren(current.el);
  }

  function update() {
    const d = instance(name);
    if (!d) {
      if (state.instances && !gone) {
        gone = true;
        current?.destroy?.();
        head.replaceChildren(h("div", null, h("h1", null, name)));
        body.replaceChildren(empty("db", "This database no longer exists", "It was deleted or isn’t created yet.", h("a", { class: "btn", href: "#/databases" }, "Back to databases")));
      }
      return;
    }
    if (gone) { gone = false; headSig = bannerSig = ""; body.replaceChildren(alerts, summarySlot, tablist, panelEl); current = null; }
    const sig = JSON.stringify([d.status, d.busy, d.pendingRestore, d.deletionProtection, d.applied, d.engineLabel, state.me?.role]);
    if (sig !== headSig) {
      headSig = sig;
      const locked = d.busy || d.pendingRestore;
      head.replaceChildren(
        h("div", null, h("p", { class: "kicker" }, d.engineLabel), h("h1", null, d.name, " ", status(d))),
        can("operator") ? h("div", { class: "page-actions" },
          h("a", { class: `btn${locked ? " disabled" : ""}`, href: locked ? null : `#/databases/${d.name}/modify`, "aria-disabled": locked ? "true" : null }, icon("edit"), "Modify"),
          menu("Actions", databaseActions(d), { cls: "primary" }))
          : h("span", { class: "chip neutral", title: "Viewers can look but not change." }, icon("eye"), "Read only"));
    }
    const bsig = JSON.stringify([d.busy, d.pendingRestore, d.status, d.applied, d.cpus, d.memoryMiB, d.parameters]);
    if (bsig !== bannerSig) { bannerSig = bsig; alerts.replaceChildren(...banners(d)); }
    summarySlot.replaceChildren(summary(d));
    if (!current) { setTab(currentKey || tab); return; }
    if (SIGNATURE[currentKey](d) !== currentSig) setTab(currentKey);
    else current.update?.(d);
  }

  update();
  const stop = subscribe(update);
  return {
    el, title: name, nav: "databases", crumbs: [["Databases", "#/databases"], [name]],
    route(nextName, nextTab) {
      if (nextName !== name) return false;
      setTab(nextTab || "connectivity");
      return true;
    },
    destroy() { stop(); current?.destroy?.(); },
  };
}
