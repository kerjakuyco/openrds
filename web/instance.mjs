// One database: summary, actions and tabs, like a managed-database console.
import { h, icon, btn, api, toast, modal, menu, status, isLive, isRunning, keyValues, panel, empty, dataTable, bytes, rate, mib, percent, when, ago, plural, copyText, alertBox, sizeLabel, meter, nextId, clock, chip } from "./lib.mjs";
import { state, subscribe, instance, engine, loadEngines, sync, go, track } from "./store.mjs";
import { timeChart, chartGroup } from "./charts.mjs";
import { databaseActions, takeSnapshot, confirmReboot } from "./actions.mjs";
import { backupTable, backupSummary } from "./backups.mjs";
import { tagEditor } from "./forms.mjs";

const TABS = [["connectivity", "Connectivity & security"], ["monitoring", "Monitoring"], ["logs", "Logs & events"],
  ["configuration", "Configuration"], ["backups", "Maintenance & backups"], ["tags", "Tags"]];
let chartRange = "1h";

function code(text, label = "Copy") {
  const copy = btn([icon("copy"), label], async () => {
    const ok = await copyText(text);
    toast(ok ? "Copied to the clipboard." : "Copying failed. Select the text and copy it.", ok ? "success" : "warn");
  }, "small ghost");
  return h("div", { class: "code" }, h("pre", null, text), copy);
}

const pendingChanges = (d) => {
  if (!d.applied) return [];
  const out = [];
  if (d.applied.cpus !== d.cpus) out.push(`CPU ${d.applied.cpus ? `${d.applied.cpus} vCPU` : "unlimited"} → ${d.cpus ? `${d.cpus} vCPU` : "unlimited"}`);
  if (d.applied.memoryMiB !== d.memoryMiB) out.push(`memory ${d.applied.memoryMiB ? mib(d.applied.memoryMiB) : "unlimited"} → ${d.memoryMiB ? mib(d.memoryMiB) : "unlimited"}`);
  if (JSON.stringify(Object.entries(d.applied.parameters || {}).sort()) !== JSON.stringify(Object.entries(d.parameters || {}).sort())) out.push("engine parameters");
  return out;
};

function connectivityTab(d) {
  const eng = engine(d.engine);
  const uri = h("div"), container = h("div");
  let c = null;
  async function reveal(b) {
    b.disabled = true;
    try { c = await api(`instances/${d.name}/connection`); render(); }
    catch (err) { toast(err.message, "error"); b.disabled = false; }
  }
  function render() {
    if (!c) {
      uri.replaceChildren(h("p", { class: "muted" }, "The URI includes the password, so it is fetched only when you ask for it."), btn([icon("eye"), "Reveal connection URI"], reveal));
      container.replaceChildren(h("p", { class: "muted" }, "Reveal the connection URI to see the network to join and the internal address."));
      return;
    }
    uri.replaceChildren(code(c.uri, "Copy URI"), h("div", { class: "row" }, btn("Hide", () => { c = null; render(); }, "small ghost")));
    container.replaceChildren(h("p", { class: "muted" }, "Attach your application container to this private network, then use the internal URI."),
      code(`docker network connect ${c.network} YOUR_APP_CONTAINER`), code(c.networkURI, "Copy container URI"));
  }
  render();
  const el = h("div", { class: "tab-grid" },
    panel("Endpoint & port", {}, keyValues([["Endpoint", h("code", null, "127.0.0.1")], ["Port", h("code", null, String(d.port))],
      ["Master user", h("code", null, eng.user)], [d.engine === "valkey" ? "Database number" : "Database name", h("code", null, eng.database)],
      ["Container DNS name", h("code", null, `openrds-${d.name}`)], ["Protocol", d.engine === "valkey" ? "Redis (RESP)" : d.engineLabel]], "cols-2")),
    panel("Connection URI", { description: d.engine === "valkey" ? "Any Redis client can use this URI." : "" }, uri),
    panel("Connect from your computer", { description: "Replace user@your-vps with your SSH login, keep the tunnel open, then use the URI locally." },
      code(`ssh -N -L ${d.port}:127.0.0.1:${d.port} user@your-vps`)),
    panel("Connect an application container", {}, container),
    panel("Security", {}, h("ul", { class: "facts" },
      h("li", null, icon("shield"), "The port is published on the VPS loopback interface only; nothing listens publicly."),
      h("li", null, icon("key"), "The password was generated for this database and is kept in the control volume."),
      h("li", null, icon("terminal"), "Traffic between the tunnel and the database is not encrypted inside the VPS."))));
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
    timeChart({ title: "Memory in use", subtitle: d.memoryMiB ? `Against the ${mib(d.memoryMiB)} limit, excluding reclaimable cache` : "Excluding reclaimable cache. No memory limit is set.",
      series: [{ label: "Memory", cls: "s1", value: p => p.memory }], format: bytes, binary: true, floor: 1048576,
      domainMax: limit ? () => limit : null, reference: limit ? { value: limit, label: `Limit ${mib(d.memoryMiB)}` } : null }),
    timeChart({ title: "Network throughput", subtitle: "Bytes per second on the database’s network",
      series: [{ label: "In", cls: "s1", value: p => p.netIn }, { label: "Out", cls: "s2", value: p => p.netOut }], format: rate, binary: true, floor: 1024 }),
    timeChart({ title: "Disk throughput", subtitle: "Bytes per second read from and written to storage",
      series: [{ label: "Read", cls: "s1", value: p => p.read }, { label: "Write", cls: "s2", value: p => p.write }], format: rate, binary: true, floor: 1024 }),
  ];
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
      charts.forEach(c => c.update({ points: v.points, from: v.now - v.range, to: v.now, step }));
      note.textContent = `A reading every ${v.interval} seconds, averaged to fit the range. Updated ${clock(Date.now())}.`;
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
  const follow = h("input", { type: "checkbox", id: nextId("follow") });
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
    panel("Database log", { description: "The latest 120 lines, up to 64 KiB. Passwords are redacted.",
      actions: [h("label", { class: "check inline", for: follow.id }, follow, "Follow"), btn([icon("refresh"), "Refresh"], load, "small")] }, output),
    panel("Recent events", { actions: [h("a", { class: "btn small", href: "#/events" }, "All events")] }, events));
  return { el, update: renderEvents, destroy: () => clearInterval(timer) };
}

export const resultChip = (r) => h("span", { class: `status ${r === "completed" ? "good" : r === "failed" ? "critical" : "info"}` },
  icon(r === "completed" ? "ok" : r === "failed" ? "error" : "pending"), r.charAt(0).toUpperCase() + r.slice(1));

function storagePanel(d) {
  const measured = h("span", { role: "status", class: "muted" });
  const measure = btn([icon("disk"), "Measure volume usage"], async (b) => {
    b.disabled = true; measured.textContent = "Measuring allocated files…";
    try {
      const { usedBytes } = await api(`instances/${d.name}/storage`);
      measured.textContent = `${bytes(usedBytes)} used${d.diskGiB ? ` of ${d.diskGiB} GiB` : " · uncapped"}`;
    } catch (err) { measured.textContent = err.message; }
    finally { b.disabled = false; }
  }, "small");
  measure.disabled = !isLive(d) || d.busy;
  const body = [keyValues([["Data volume limit", d.diskGiB ? `${d.diskGiB} GiB` : "Uncapped"]]), h("div", { class: "row" }, measure, measured)];
  if (d.diskGiB) {
    const stopped = ["stopped", "not provisioned"].includes(d.status) && !d.busy;
    const input = h("input", { type: "number", min: "1", max: "65536", step: "1", value: String(d.diskGiB), class: "num", "aria-label": `New data volume size for ${d.name} in GiB` });
    const apply = btn("Apply size", async (b) => {
      const next = Number(input.value);
      if (!Number.isInteger(next) || next < 1) { toast("Enter a whole number of GiB.", "error"); return; }
      b.disabled = true;
      try {
        await track(`Resize ${d.name}`, api(`instances/${d.name}/disk`, { method: "POST", body: { diskGiB: next } }), { success: `${d.name} now has a ${next} GiB data volume.` });
      } catch { /* reported by the toast */ }
      finally { b.disabled = false; }
    }, "small");
    apply.disabled = !stopped;
    body.push(h("div", { class: "resize" }, h("div", { class: "row" }, input, h("span", { class: "muted" }, "GiB"), apply),
      h("p", { class: "muted" }, stopped ? "Grow or shrink the ext4 volume. A smaller size is refused when the data does not fit." : "Stop the database to change this limit.")));
  } else {
    body.push(h("p", { class: "muted" }, "A limit cannot be added to an existing volume. Restore a snapshot into a new instance with a data volume limit."));
  }
  return panel("Storage", {}, ...body);
}

function configurationTab(d) {
  const eng = engine(d.engine);
  const params = Object.entries(d.parameters || {}).sort();
  const applied = d.applied?.parameters || {};
  const removed = d.applied ? Object.keys(applied).filter(k => !(k in (d.parameters || {}))) : [];
  const describe = (name) => eng.parameters.find(p => p.name === name);
  const paramRows = [...params.map(([k, v]) => ({ k, v, pending: Boolean(d.applied) && applied[k] !== v })), ...removed.map(k => ({ k, v: null, pending: true }))];
  const el = h("div", { class: "stack-gap" },
    d.applied && alertBox(`Pending until the next restart: ${pendingChanges(d).join(", ")}.`, "info"),
    h("div", { class: "tab-grid" },
      panel("Instance configuration", { actions: [h("a", { class: "btn small", href: `#/databases/${d.name}/modify` }, icon("edit"), "Modify")] }, keyValues([
        ["Engine", d.engineLabel], ["Created", when(d.createdAt)],
        ["CPU limit", d.cpus ? `${d.cpus} vCPU` : "Unlimited"], ["Memory limit", d.memoryMiB ? mib(d.memoryMiB) : "Unlimited"],
        ["Host port", String(d.port)], ["Deletion protection", d.deletionProtection ? "On" : "Off"],
      ], "cols-2")),
      storagePanel(d)),
    panel("Engine parameters", { counter: params.length, description: "Values passed to the engine at start-up. Unlisted settings use the engine default.",
      actions: [h("a", { class: "btn small", href: `#/databases/${d.name}/modify` }, icon("sliders"), "Edit parameters")] },
    paramRows.length ? dataTable({ caption: "Engine parameters", rows: paramRows, columns: [
      { key: "k", label: "Parameter", cls: "wrap", value: r => h("div", { class: "stack" }, h("code", null, r.k), h("small", null, describe(r.k)?.description || "")) },
      { key: "v", label: "Value", value: r => r.v === null ? h("span", { class: "muted" }, "Default") : h("code", null, r.v === "" ? "(empty)" : r.v) },
      { key: "d", label: "Default", value: r => h("code", { class: "muted" }, describe(r.k)?.default || "(empty)") },
      { key: "s", label: "Status", value: r => r.pending ? h("span", { class: "status info" }, icon("pending"), "Pending restart") : h("span", { class: "status good" }, icon("ok"), "Applied") },
    ] }) : empty("sliders", "Engine defaults", `This database uses the default ${eng.title} configuration.`)));
  return { el };
}

function backupsTab(d) {
  const list = h("div", { class: "flush" });
  function render(cur) {
    const mine = (state.backups || []).filter(b => b.name === cur.name && Date.parse(b.createdAt) >= Date.parse(cur.createdAt));
    list.replaceChildren(mine.length ? backupTable(mine, { showInstance: false }) : empty("camera", "No backups of this database yet", isRunning(cur) ? "Take a snapshot now, or turn on automated backups." : "Start the database to take a snapshot."));
  }
  const toggle = btn(d.deletionProtection ? "Turn off" : "Turn on", () => modal({
    title: d.deletionProtection ? "Turn off deletion protection?" : "Turn on deletion protection?", kicker: d.name, confirm: d.deletionProtection ? "Turn off" : "Turn on",
    body: h("p", { class: "muted" }, d.deletionProtection ? "Anyone with the admin token will be able to delete this database." : "Deleting the database is refused until protection is turned off again."),
    run: async () => {
      await api(`instances/${d.name}/modify`, { method: "POST", body: { deletionProtection: !d.deletionProtection } });
      toast(`Deletion protection is ${d.deletionProtection ? "off" : "on"} for ${d.name}.`, "success");
      await sync();
    },
  }), "small");
  const snap = btn([icon("camera"), "Take snapshot"], () => takeSnapshot(d), "small primary");
  snap.disabled = d.busy || !isRunning(d);
  const el = h("div", { class: "stack-gap" },
    h("div", { class: "tab-grid" },
      panel("Automated backups", { actions: [h("a", { class: "btn small", href: `#/databases/${d.name}/modify` }, icon("edit"), "Change")] }, backupSummary(d)),
      panel("Deletion protection", { actions: [toggle] }, h("p", { class: "protection" }, icon("shield"), d.deletionProtection ? "On. The database cannot be deleted until protection is turned off." : "Off. The database can be deleted, with an optional final snapshot."))),
    panel("Snapshots and backups", { description: "Every recovery point of this database. Restoring always creates a new database.", actions: [snap] }, list));
  render(d);
  return { el, update: render };
}

function tagsTab(d) {
  const tags = Object.entries(d.tags || {}).sort();
  const manage = btn([icon("tag"), "Manage tags"], () => {
    const editor = tagEditor(d.tags);
    modal({ title: "Manage tags", kicker: d.name, confirm: "Save tags", wide: true, body: [h("p", { class: "muted" }, "Up to 50 tags. Keys use letters, digits, spaces and _ . : / = + - @."), editor.el],
      run: async () => {
        await api(`instances/${d.name}/modify`, { method: "POST", body: { tags: editor.value() } });
        toast(`Tags saved for ${d.name}.`, "success");
        await sync();
      } });
  }, "small");
  const el = panel("Tags", { counter: tags.length, description: "Labels for organising databases. Search the database list by tag.", actions: [manage] },
    tags.length ? dataTable({ caption: "Tags", rows: tags, columns: [{ key: "k", label: "Key", value: ([k]) => h("code", null, k) }, { key: "v", label: "Value", value: ([, v]) => v || h("span", { class: "muted" }, "(empty)") }] })
      : empty("tag", "No tags", "Add tags such as env = production or team = payments."));
  return { el };
}

const BUILD = { connectivity: connectivityTab, monitoring: monitoringTab, logs: logsTab, configuration: configurationTab, backups: backupsTab, tags: tagsTab };
// What each tab shows from the inventory. A tab is rebuilt only when this
// changes, so a revealed URI, a log scroll position or a chart range survives a refresh.
const SIGNATURE = {
  connectivity: d => JSON.stringify([d.port, d.status]),
  monitoring: d => JSON.stringify([d.cpus, d.memoryMiB, state.host?.cpus]),
  logs: () => "",
  configuration: d => JSON.stringify([d.status, d.busy, d.cpus, d.memoryMiB, d.diskGiB, d.port, d.parameters, d.applied, d.deletionProtection]),
  backups: d => JSON.stringify([d.busy, d.status, d.deletionProtection, d.backupRetentionDays, d.backupWindow, d.latestBackupAt, d.nextBackupAt]),
  tags: d => JSON.stringify(d.tags || {}),
};

function summary(d) {
  const base = d.cpus || state.host?.cpus || 1;
  const live = d.live;
  const cpu = live ? live.cpu / base : null;
  const memShare = live && d.memoryMiB ? live.memory / (d.memoryMiB * 1048576) * 100 : null;
  return panel("Summary", {}, keyValues([
    ["Status", status(d)],
    ["Engine", d.engineLabel],
    ["Endpoint", h("code", null, `127.0.0.1:${d.port}`)],
    ["Size", sizeLabel(d)],
    ["CPU", live ? h("span", { class: "with-meter" }, percent(cpu), meter(cpu, 100, `CPU ${percent(cpu)} of ${d.cpus ? "the limit" : "the host"}`)) : "—"],
    ["Memory", live ? h("span", { class: "with-meter" }, `${bytes(live.memory)}${d.memoryMiB ? ` of ${mib(d.memoryMiB)}` : ""}`, memShare !== null && meter(memShare, 100, `Memory ${percent(memShare)} of the limit`)) : "—"],
    ["Storage", d.diskGiB ? `${d.diskGiB} GiB limit` : "Uncapped volume"],
    ["Automated backups", d.backupRetentionDays ? `${plural(d.backupRetentionDays, "day")} retention` : h("span", { class: "status warn" }, icon("alert"), "Off")],
  ], "cols-4"));
}

function banners(d) {
  const out = [];
  if (d.busy) out.push(alertBox("An operation is running on this database. Actions are available again when it finishes.", "info"));
  if (d.pendingRestore && !d.busy) out.push(alertBox("The restore did not finish, so this data is incomplete. Check Logs & events, then restore the snapshot into a fresh database.", "error"));
  if (d.status === "unhealthy" && !d.busy) out.push(alertBox("The health check is failing. Check Logs & events for the database’s own messages.", "error"));
  if (d.status === "not provisioned" && !d.busy) out.push(alertBox("The container does not exist. Choose Actions → Start to retry provisioning, or delete the database to free its name and port.", "warn"));
  if (d.applied) {
    const reboot = btn([icon("reboot"), "Reboot now"], () => confirmReboot(d), "small");
    reboot.disabled = d.busy || !isLive(d);
    out.push(alertBox(`Pending modifications apply at the next restart: ${pendingChanges(d).join(", ")}.`, "info", " ", reboot));
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
        body.replaceChildren(empty("db", "This database no longer exists", "It was deleted, or it has not been created yet.", h("a", { class: "btn", href: "#/databases" }, "Back to databases")));
      }
      return;
    }
    if (gone) { gone = false; headSig = bannerSig = ""; body.replaceChildren(alerts, summarySlot, tablist, panelEl); current = null; }
    const sig = JSON.stringify([d.status, d.busy, d.pendingRestore, d.deletionProtection, d.applied, d.engineLabel]);
    if (sig !== headSig) {
      headSig = sig;
      const locked = d.busy || d.pendingRestore;
      head.replaceChildren(
        h("div", null, h("p", { class: "kicker" }, d.engineLabel), h("h1", null, d.name, " ", status(d))),
        h("div", { class: "page-actions" },
          h("a", { class: `btn${locked ? " disabled" : ""}`, href: locked ? null : `#/databases/${d.name}/modify`, "aria-disabled": locked ? "true" : null }, icon("edit"), "Modify"),
          menu("Actions", databaseActions(d), { cls: "primary" })));
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
