// One database: summary, actions and tabs, like a managed-database console.
import { h, icon, btn, api, toast, modal, menu, status, isLive, isRunning, keyValues, panel, infoPanel, scheduleLabel, empty, dataTable, bytes, rate, mib, percent, when, ago, plural, copyText, alertBox, sizeLabel, meter, nextId, clock, chip, query, serverPager } from "./lib.mjs";
import { endpointChip, engineBadge } from "./databases.mjs";
import { state, subscribe, instance, engine, loadEngines, sync, go, track, can } from "./store.mjs";
import { connectGuide } from "./connect.mjs";
import { timeChart, chartGroup, sparkline } from "./charts.mjs";
import { databaseActions, takeSnapshot, confirmReboot, start } from "./actions.mjs";
import { backupTable } from "./backups.mjs";
import { tagEditor } from "./forms.mjs";
import { connectivityPanel, firewallPanel, usersTab } from "./security.mjs";
import { insightsTab, ENGINE_TERMS } from "./insights.mjs";
import { poolerPanel } from "./pooler.mjs";
import { settingsPanel, editParameters, editBackups, restoreToTime, PITR_ENGINES } from "./settings.mjs";
import { replicationPanel, lagLabel, replicationChip } from "./replicas.mjs";
import { multiServer, serverLabel, serverOf } from "./servers.mjs";
import { moveDatabase, moveBlocker, moveProgress } from "./move.mjs";

const TABS = [["overview", "Overview"], ["connectivity", "Connect"], ["users", "Users"], ["monitoring", "Monitoring"], ["insights", "Performance"],
  ["backups", "Backups"], ["configuration", "Settings"], ["logs", "Logs"]];
const ALIASES = { tags: "configuration" }; // older links
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
  const [notice, ...network] = connectivityPanel(d, all);
  const el = h("div", { class: "stack-gap" },
    notice,
    connectGuide(d, eng, { canReveal: can("operator") }).el,
    ...network,
    poolerPanel(d),
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
  if (d.replicaOf && d.engine !== "valkey") {
    charts.push(engineChart({ title: "Replica lag", subtitle: `Seconds behind ${d.replicaOf}`,
      series: [{ label: "Lag", cls: "s1", value: p => p.db.lag }], format: lagLabel, axisFormat: v => `${v}s`,
      empty: "No lag readings yet." }, pts => pts.filter(p => p.db && p.db.lag != null)));
  }
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
  const events = h("div", { class: "flush" }, empty("bell", "Reading events…", ""));
  const refreshEvents = databaseEvents(d.name, (list, err) => {
    events.replaceChildren(err ? empty("error", "Could not read events", err.message) : list.length ? dataTable({ caption: "Events", rows: list, columns: [
      { key: "at", label: "Time", value: e => h("span", { title: when(e.at) }, ago(e.at)) },
      { key: "action", label: "Operation", value: e => e.action },
      { key: "result", label: "Outcome", value: e => resultChip(e.result) },
      { key: "detail", label: "Detail", cls: "wrap", value: e => e.detail || h("span", { class: "muted" }, "—") },
    ] }) : empty("bell", "No events yet", "Operations on this database appear here."));
  });
  load();
  const el = h("div", { class: "stack-gap" },
    panel("Database log", { description: "Last 120 lines. Passwords hidden.",
      actions: [h("label", { class: "check inline", for: follow.id }, follow, "Follow"), btn([icon("refresh"), "Refresh"], load, "small")] }, output),
    panel("Recent events", { actions: [h("a", { class: "btn small", href: "#/events" }, "All events")] }, events));
  return { el, update: (_, changed) => refreshEvents(changed), destroy: () => clearInterval(timer) };
}

// The latest events of one database, read again when new events arrive.
function databaseEvents(name, show) {
  let seq = 0;
  async function load() {
    const mine = ++seq;
    try {
      const page = await api(`activity?${query({ name, limit: 25 })}`);
      if (mine === seq) show(page.items, null);
    } catch (err) {
      if (mine === seq) show(null, err);
    }
  }
  load();
  return (changed) => { if (changed?.has("activity")) load(); };
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
    tagsTab(d).el);
  return { el };
}

// A button that opens a settings editor, unless the database is busy.
function editButton(d, label, run) {
  const b = btn(label, run, "small");
  b.disabled = d.busy || d.pendingRestore;
  return b;
}

function backupsTab(d) {
  const list = h("div", { class: "flush" }, empty("camera", "Loading backups…", ""));
  const pages = h("div");
  const ui = { sort: { key: "created", dir: "desc" }, offset: 0 };
  let cur = d, seq = 0;
  // This database's backups, not those of an earlier one with its name.
  async function load() {
    const mine = ++seq;
    try {
      const page = await api(`backups?${query({ name: cur.name, since: cur.createdAt, sort: ui.sort.key, order: ui.sort.dir, offset: ui.offset, limit: 20 })}`);
      if (mine !== seq) return;
      if (page.offset > 0 && page.offset >= page.total) { ui.offset = Math.max(0, Math.floor((page.total - 1) / 20) * 20); load(); return; }
      list.replaceChildren(page.items.length ? backupTable(page.items, { showInstance: false, sort: ui.sort, onSort: (key) => {
        ui.sort = { key, dir: ui.sort.key === key && ui.sort.dir === "asc" ? "desc" : "asc" };
        ui.offset = 0;
        list.replaceChildren(empty("camera", "Loading backups…", "")); // not the old order meanwhile
        pages.replaceChildren();
        load();
      } }) : empty("camera", "No backups of this database yet", isRunning(cur) ? "Take a snapshot or turn on backups." : "Start the database to take a snapshot."));
      pages.replaceChildren(serverPager(page, 20, (offset) => { ui.offset = offset; load(); }, "Backup pages"));
    } catch (err) {
      if (mine === seq) list.replaceChildren(empty("error", "Could not read backups", err.message));
    }
  }
  function render(next, changed) {
    cur = next;
    if (changed?.has("backups") || changed?.has("storage")) load();
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
        ["Next automated backup", d.nextBackupAt ? `${when(d.nextBackupAt)} (${ago(d.nextBackupAt)})` : "—"],
        ...(PITR_ENGINES.includes(d.engine) && !d.replicaOf ? [["Point-in-time recovery", !d.pitr ? "Off" : d.recovery?.earliest
          ? h("span", null, `${when(d.recovery.earliest)} – ${when(d.recovery.latest)}`) : h("span", { class: "muted" }, "Starting: the first base backup is on its way")]] : [])]],
      ["Protection", [["Deletion protection", h("span", { class: "row tight" }, d.deletionProtection ? "On" : "Off", can("operator") ? toggle : null)]]],
    ], { actions: can("operator") ? [editButton(d, [icon("edit"), "Change"], () => editBackups(d)), snap,
      d.recovery?.earliest ? editButton(d, [icon("restore"), "Restore to point in time"], () => restoreToTime(d)) : null].filter(Boolean) : [] }),
    panel("Snapshots and backups", { description: "Restoring creates a new database." }, list, pages));
  load();
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

// ---------- overview: health at a glance, how to connect, replication, backups, recent events

function healthTile(label, now, values, { max = null, hint = "", tone = "" } = {}) {
  return h("div", { class: `tile ${tone}`.trim() },
    h("div", { class: "tile-head" }, h("span", { class: "tile-label" }, label), hint ? h("small", null, hint) : null),
    h("strong", { class: "tile-now" }, now),
    sparkline(values, { max, label: `${label}, last hour` }));
}

function connectCard(d) {
  const eng = engine(d.engine);
  const rows = [
    [multiServer() ? `From ${serverLabel(d)}` : "From this server", endpointChip(d)],
    ["From containers", h("span", { class: "row tight" }, endpointChip(d, { network: true }), h("small", null, `port ${{ postgres: 5432, mysql: 3306, valkey: 6379 }[d.engine]}`))],
    d.privateHost ? ["From the private network", h("code", null, `${d.privateHost}:${d.port}`)] : null,
    d.pooler ? ["Through the pooler", h("code", null, `127.0.0.1:${d.pooler.port}`)] : null,
    d.publicAccess ? ["From the internet", h("code", null, `${d.publicHost || serverOf(d)?.publicAddress || "<server address>"}:${d.publicPort}`)] : null,
    ["User · database", h("code", null, `${eng.user} · ${eng.database}`)],
  ].filter(Boolean);
  return panel("Connect", { description: d.requireTLS ? "TLS required." : "TLS optional.",
    actions: [h("a", { class: "btn small", href: `#/databases/${d.name}/connectivity` }, icon("link"), "Connection strings")] },
  h("dl", { class: "rows" }, rows.map(([k, v]) => h("div", null, h("dt", null, k), h("dd", null, v)))));
}

function backupsCard(d) {
  const on = d.backupRetentionDays > 0;
  const rows = [
    ["Automated", on ? `${plural(d.backupRetentionDays, "day")} · ${scheduleLabel(d.backupWindow, d.backupIntervalHours)}` : h("span", { class: "status warn" }, icon("alert"), "Off")],
    ["Latest", d.latestBackupAt ? h("span", { title: when(d.latestBackupAt) }, ago(d.latestBackupAt)) : h("span", { class: "muted" }, "None yet")],
    on ? ["Next", d.nextBackupAt ? h("span", { title: when(d.nextBackupAt) }, ago(d.nextBackupAt)) : "—"] : null,
    PITR_ENGINES.includes(d.engine) && !d.replicaOf ? ["Point in time", !d.pitr ? h("span", { class: "muted" }, "Off") : d.recovery?.earliest ? `Any second since ${ago(d.recovery.earliest)}` : h("span", { class: "muted" }, "Starting")] : null,
    ["Protection", d.deletionProtection ? h("span", { class: "with-icon" }, icon("shield"), "Deletion protection on") : h("span", { class: "muted" }, "Can be deleted")],
  ].filter(Boolean);
  const actions = [h("a", { class: "btn small ghost", href: `#/databases/${d.name}/backups` }, "All backups")];
  if (can("operator")) {
    const snap = btn([icon("camera"), "Snapshot"], () => takeSnapshot(d), "small");
    snap.disabled = d.busy || !isRunning(d);
    actions.unshift(on ? snap : editButton(d, [icon("plus"), "Turn on"], () => editBackups(d)));
    if (!on) actions.splice(1, 0, snap);
  }
  return panel("Backups", { actions }, h("dl", { class: "rows" }, rows.map(([k, v]) => h("div", null, h("dt", null, k), h("dd", null, v)))));
}

function eventsCard(name) {
  const list = h("ul", { class: "activity" }, h("li", { class: "none" }, h("span"), h("small", null, "Reading events…")));
  // One line per action: its outcome, or "started" while it still runs.
  const refresh = databaseEvents(name, (mine, err) => {
    if (err) { list.replaceChildren(h("li", { class: "none" }, h("span"), h("small", null, err.message))); return; }
    const items = mine.filter((e, i) => e.result !== "started" || !mine.slice(0, i).some(x => x.action === e.action)).slice(0, 5);
    list.replaceChildren(...(items.length ? items.map(e => h("li", { class: e.result },
      icon(e.result === "completed" ? "ok" : e.result === "failed" ? "error" : "pending"),
      h("div", null, h("strong", null, e.action), h("small", { title: when(e.at) }, `${ago(e.at)}${e.detail ? ` · ${e.detail}` : ""}`)))) : [h("li", { class: "none" }, h("span"), h("small", null, "No events yet."))]));
  });
  return { el: panel("Recent events", { actions: [h("a", { class: "btn small ghost", href: `#/databases/${name}/logs` }, "All events")] }, h("div", { class: "flush" }, list)), update: refresh };
}

function detailsCard(d) {
  const eng = engine(d.engine);
  return panel("Details", {}, h("dl", { class: "rows" }, [
    ["Engine", d.engineLabel],
    ["Created", h("span", { title: when(d.createdAt) }, when(d.createdAt))],
    [d.engine === "valkey" ? "Database number" : "Database", h("code", null, eng.database)],
    ["Users", d.replicaOf ? `As on ${d.replicaOf}` : plural(1 + (d.users || []).length, "user")],
    ["Parameters", Object.keys(d.parameters || {}).length ? plural(Object.keys(d.parameters).length, "custom setting") : "Engine defaults"],
    ["Tags", Object.keys(d.tags || {}).length ? h("span", { class: "tag-chips" }, Object.entries(d.tags).map(([k, v]) => h("span", { class: "tag-chip" }, v ? `${k}: ${v}` : k))) : h("span", { class: "muted" }, "None")],
  ].map(([k, v]) => h("div", null, h("dt", null, k), h("dd", null, v)))));
}

function overviewTab(d) {
  const hostCPUs = state.host?.cpus || 1;
  const base = d.cpus || hostCPUs;
  const limit = d.memoryMiB * 1048576;
  const showLag = d.replicaOf && d.engine !== "valkey";
  const terms = ENGINE_TERMS[d.engine];
  const tiles = h("div", { class: "health", "aria-label": "Last hour" });
  const note = h("p", { class: "muted small health-note" });
  function draw(points, live) {
    const db = live?.db;
    const amount = (n) => n < 10 ? n.toFixed(1) : Math.round(n).toLocaleString();
    tiles.replaceChildren(
      healthTile("CPU", live ? percent(live.cpu / base) : "—", points.map(p => p.cpu / base), { max: 100, hint: d.cpus ? `of ${d.cpus} vCPU` : "of the host" }),
      healthTile("Memory", live ? bytes(live.memory) : "—", points.map(p => p.memory), { max: limit || null, hint: d.memoryMiB ? `of ${mib(d.memoryMiB)}` : "no limit" }),
      healthTile("Connections", db ? String(Math.round(db.conns)) : "—", points.map(p => p.db ? p.db.conns : NaN), { hint: db?.maxConns ? `of ${db.maxConns.toLocaleString()}` : "" }),
      showLag
        ? healthTile("Replica lag", lagLabel(d.replication?.lagSeconds), points.map(p => p.db?.lag ?? NaN), { hint: `behind ${d.replicaOf}`, tone: d.replication?.lagSeconds > 60 ? "warn" : "" })
        : healthTile(`${terms.ops}/s`, db ? amount(db.ops) : "—", points.map(p => p.db ? p.db.ops : NaN)));
  }
  draw([], d.live);
  let timer = 0;
  async function load() {
    if (!isLive(d)) { note.textContent = "Start the database to see live readings."; return; }
    try {
      const v = await api(`instances/${d.name}/history?range=1h`);
      const pts = v.available ? v.points || [] : [];
      draw(pts, pts.at(-1) || d.live);
      note.textContent = v.available ? `Last hour · every ${v.interval}s · updated ${clock(Date.now())}` : v.message;
    } catch (err) { note.textContent = err.message; }
  }
  load();
  timer = setInterval(() => { if (!document.hidden) load(); }, 30000);
  const events = eventsCard(d.name);
  const el = h("div", { class: "overview" },
    h("section", { class: "health-wrap" }, tiles, note),
    h("div", { class: "overview-grid" },
      h("div", { class: "stack-gap" }, connectCard(d), replicationPanel(d), poolerSummary(d)),
      h("div", { class: "stack-gap" }, backupsCard(d), events.el, detailsCard(d))));
  return { el, update: (_, changed) => events.update(changed), destroy: () => clearInterval(timer) };
}

function poolerSummary(d) {
  if (d.engine !== "postgres" || !d.pooler) return null;
  return panel("Connection pooling", { description: `${d.pooler.mode === "session" ? "Session" : "Transaction"} mode · ${plural(d.pooler.poolSize, "server connection")} per user`,
    actions: [h("a", { class: "btn small ghost", href: `#/databases/${d.name}/connectivity` }, "Settings")] },
  h("dl", { class: "rows" }, h("div", null, h("dt", null, "Status"), h("dd", null, d.poolerStatus === "running" ? h("span", { class: "status good" }, icon("ok"), "Running") : h("span", { class: "status warn" }, icon("alert"), d.poolerStatus || "Unknown"))),
    h("div", null, h("dt", null, "Endpoint"), h("dd", null, h("code", null, `127.0.0.1:${d.pooler.port}`)))));
}

const BUILD = { overview: overviewTab, connectivity: connectivityTab, users: usersTab, monitoring: monitoringTab, insights: insightsTab, logs: logsTab, configuration: configurationTab, backups: backupsTab };
// What each tab shows from the inventory. A tab is rebuilt only when this
// changes, so a revealed URI, a log scroll position or a chart range survives a refresh.
const SIGNATURE = {
  overview: d => JSON.stringify([d.status, d.busy, d.pendingRestore, d.port, d.cpus, d.memoryMiB, d.requireTLS, d.publicAccess, d.publicPort, d.publicHost, d.pooler, d.poolerStatus,
    d.backupRetentionDays, d.backupWindow, d.backupIntervalHours, d.latestBackupAt, d.nextBackupAt, d.pitr, d.recovery, d.deletionProtection, d.users, d.parameters, d.tags,
    d.replicaOf, d.replication?.state, d.replication?.lagSeconds, (d.replicas || []).map(n => [n, instance(n)?.replication?.state, instance(n)?.replication?.lagSeconds, instance(n)?.port]), state.host?.cpus, state.me?.role,
    d.privateHost, d.serverName, (state.servers || []).length]),
  connectivity: d => JSON.stringify([d.port, d.status, d.busy, d.users, d.publicAccess, d.publicPort, d.publicHost, d.firewall, d.requireTLS, d.tlsReady, d.pooler, d.poolerStatus,
    d.privateAccess, d.privateHost, d.privateFirewall, d.serverName, (state.servers || []).map(s => [s.id, s.privateAddress, s.publicAddress]),
    d.replicaOf, (d.replicas || []).map(n => [n, instance(n)?.replication?.state, instance(n)?.replication?.lagSeconds, instance(n)?.port]), state.me?.role]),
  users: d => JSON.stringify([d.status, d.busy, d.users, d.tlsReady, d.requireTLS, d.port, d.replicaOf, state.me?.role]),
  monitoring: d => JSON.stringify([d.cpus, d.memoryMiB, d.replicaOf, state.host?.cpus]),
  insights: d => JSON.stringify([d.status, d.pendingRestore, state.me?.role]),
  logs: () => "",
  configuration: d => JSON.stringify([d.version, d.pitr, d.replicaOf, d.replicas, d.status, d.busy, d.pendingRestore, d.cpus, d.memoryMiB, d.diskGiB, d.port, d.parameters, d.applied, d.deletionProtection,
    d.requireTLS, d.tlsReady, d.backupRetentionDays, d.backupWindow, d.backupIntervalHours, d.users, d.tags, state.me?.role]),
  backups: d => JSON.stringify([d.busy, d.status, d.deletionProtection, d.backupRetentionDays, d.backupWindow, d.backupIntervalHours, d.latestBackupAt, d.nextBackupAt, d.pitr, d.recovery]),
};

function factsStrip(d) {
  const fact = (label, value) => h("div", { class: "fact" }, h("span", { class: "fact-label" }, label), h("span", { class: "fact-value" }, value));
  return h("div", { class: "facts-strip" },
    multiServer() ? fact("Server", h("span", { class: "row tight" }, h("a", { href: "#/servers", class: "with-icon" }, icon("host"), serverLabel(d)),
      can("operator") && !moveBlocker(d) ? h("button", { type: "button", class: "link-btn", onclick: () => moveDatabase(d) }, "Move") : null)) : null,
    fact("Endpoint", endpointChip(d)),
    fact("Size", sizeLabel(d)),
    fact("Storage", d.diskGiB ? `${d.diskGiB} GiB` : "Uncapped"),
    fact("Access", h("span", { class: "with-icon" }, icon(d.publicAccess ? "globe" : "lock"), d.publicAccess ? `Public :${d.publicPort}` : "Private", d.requireTLS ? " · TLS" : "")),
    fact("Backups", d.backupRetentionDays ? plural(d.backupRetentionDays, "day") : h("span", { class: "status warn" }, icon("alert"), "Off")),
    d.replicaOf ? fact("Source", h("a", { href: `#/databases/${d.replicaOf}` }, d.replicaOf))
      : (d.replicas || []).length ? fact("Replicas", plural(d.replicas.length, "replica")) : null);
}

function banners(d) {
  const out = [];
  if (d.move) out.push(moveProgress(d));
  else if (d.busy) out.push(alertBox("An operation is running. Please wait.", "info"));
  if (d.pendingRestore && !d.busy) out.push(alertBox("The restore didn’t finish, so data is incomplete. Check the logs, then restore again.", "error"));
  if (d.status === "unhealthy" && !d.busy) out.push(alertBox("Health check failing. Check the logs.", "error"));
  if (d.status === "not provisioned" && !d.busy) out.push(alertBox("The container is missing. Use Actions → Start to retry, or delete it.", "warn"));
  if (d.status === "unreachable") out.push(alertBox(`OpenRDS can’t reach ${serverLabel(d)} over SSH, so it can’t manage, back up or monitor ${d.name} right now. The database may still be running. Servers shows why.`, "warn"));
  const rs = d.replication;
  if (d.replicaOf && !d.busy && !d.pendingRestore && (rs?.state === "broken" || rs?.state === "connecting")) {
    out.push(alertBox(`${rs.state === "broken" ? "Not replicating" : "Not receiving changes"} from ${d.replicaOf}.${rs.detail ? ` ${rs.detail}` : ""}`, rs.state === "broken" ? "error" : "warn"));
  }
  if (d.applied) {
    const reboot = can("operator") ? btn([icon("reboot"), "Reboot now"], () => confirmReboot(d), "small") : null;
    if (reboot) reboot.disabled = d.busy || !isLive(d);
    out.push(alertBox(`Changes apply at the next restart: ${pendingChanges(d).join(", ")}.`, "info", " ", reboot));
  }
  return out;
}

export async function instanceView(name, tab = "overview") {
  await loadEngines();
  if (!state.instances) await sync();
  const head = h("div", { class: "page-head" });
  const alerts = h("div", { class: "stack-gap" });
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
  const body = h("div", null, alerts, tablist, panelEl);
  const el = h("div", { class: "instance-page" }, head, body);
  let current = null, currentKey = "", currentSig = "", headSig = "", bannerSig = "", gone = false;

  function setTab(key) {
    key = ALIASES[key] || key;
    if (!BUILD[key]) key = "overview";
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

  function update(changed = new Set()) {
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
    if (gone) { gone = false; headSig = bannerSig = ""; body.replaceChildren(alerts, tablist, panelEl); current = null; }
    const sig = JSON.stringify([d.status, d.busy, d.pendingRestore, d.deletionProtection, d.applied, d.engineLabel, d.replicaOf, d.replicas, d.port, d.cpus, d.memoryMiB, d.diskGiB,
      d.publicAccess, d.publicPort, d.requireTLS, d.backupRetentionDays, d.tlsReady, state.me?.role, d.serverName, d.move?.step, (state.servers || []).length]);
    if (sig !== headSig) {
      headSig = sig;
      const locked = d.busy || d.pendingRestore;
      const stopped = !isLive(d) && !d.busy;
      const lead = can("operator") && stopped && d.status !== "not provisioned"
        ? btn([icon("play"), "Start"], () => start(d), "primary")
        : h("a", { class: `btn${isRunning(d) ? " primary" : ""}`, href: `#/databases/${d.name}/connectivity` }, icon("link"), "Connect");
      head.replaceChildren(
        h("div", { class: "title-block" }, engineBadge(d, "xl"),
          h("div", { class: "stack" },
            h("p", { class: "kicker" }, d.engineLabel, d.replicaOf ? [" · Read replica of ", h("a", { href: `#/databases/${d.replicaOf}` }, d.replicaOf)] : null),
            h("h1", null, d.name, " ", status(d)))),
        can("operator") ? h("div", { class: "page-actions" }, lead,
          h("a", { class: `btn${locked ? " disabled" : ""}`, href: locked ? null : `#/databases/${d.name}/modify`, "aria-disabled": locked ? "true" : null }, icon("edit"), "Modify"),
          menu("Actions", databaseActions(d)))
          : h("div", { class: "page-actions" }, lead, h("span", { class: "chip neutral", title: "Viewers can look but not change." }, icon("eye"), "Read only")),
        factsStrip(d));
    }
    const bsig = JSON.stringify([d.busy, d.pendingRestore, d.status, d.applied, d.cpus, d.memoryMiB, d.parameters, d.replication?.state, d.replication?.detail, d.move, d.serverName]);
    if (bsig !== bannerSig) { bannerSig = bsig; alerts.replaceChildren(...banners(d)); }
    if (!current) { setTab(currentKey || tab); return; }
    if (SIGNATURE[currentKey](d) !== currentSig) setTab(currentKey);
    else current.update?.(d, changed);
  }

  update();
  const stop = subscribe(update);
  return {
    el, title: name, nav: "databases", crumbs: [["Databases", "#/databases"], [name]],
    route(nextName, nextTab) {
      if (nextName !== name) return false;
      setTab(nextTab || "overview");
      return true;
    },
    destroy() { stop(); current?.destroy?.(); },
  };
}
