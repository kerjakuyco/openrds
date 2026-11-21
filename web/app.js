"use strict";

const $ = (id) => document.getElementById(id);
const make = (tag, cls, ...children) => {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  node.append(...children);
  return node;
};
const icon = (name) => {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("class", "icon");
  svg.setAttribute("aria-hidden", "true");
  const use = document.createElementNS(ns, "use");
  use.setAttribute("href", `/icons.svg#${name}`);
  svg.append(use);
  return svg;
};
const glyph = (name, color) => make("span", `glyph ${color}`, icon(name));
const button = (title, action, cls = "secondary") => {
  const b = make("button", cls, title);
  b.type = "button";
  b.addEventListener("click", () => action(b));
  return b;
};
const chip = (label, tone) => make("span", `chip ${tone}`, label);
const empty = (symbol, title, hint, ...actions) => make("div", "empty",
  glyph(symbol, "mint"), make("h3", "", title), make("p", "", hint), ...actions);
function table(headers, rows) {
  const head = make("tr", "", ...headers.map(text => {
    const th = make("th", "", text); th.scope = "col"; return th;
  }));
  const body = make("tbody", "", ...rows.map(cells => make("tr", "", ...cells.map((c, i) => {
    const td = make("td", "", c); td.dataset.label = headers[i]; return td;
  }))));
  return make("div", "scroll", make("table", "", make("thead", "", head), body));
}
function note(id, message = "", error = false) {
  $(id).classList.toggle("error", error);
  if ($(id).textContent !== message) $(id).textContent = message;
}
const when = (v) => new Date(v).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
function bytes(n) {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${Number(n.toFixed(i ? 1 : 0))} ${units[i]}`;
}
const engineLabel = (key) => key === "postgres" ? "PostgreSQL 17" : "MySQL 8.4";
const pretty = (v) => v.charAt(0).toUpperCase() + v.slice(1);
const attention = (d) => !d.busy && (d.pendingRestore || !["running", "stopped", "starting"].includes(d.status));
const running = (d) => d.status === "running" && !d.pendingRestore;
function statusChip(d) {
  if (d.busy) return chip("In progress", "warn");
  if (d.pendingRestore) return chip("Restore incomplete", "bad");
  return chip(pretty(d.status), running(d) ? "good" : attention(d) ? "bad" : d.status === "starting" ? "warn" : "quiet");
}
const resultChip = (r) => chip(pretty(r), r === "completed" ? "good" : r === "failed" ? "bad" : "quiet");
const data = { instances: null, backups: null, activity: null, host: null };
const pages = {
  overview: ["Overview", "A live view of the databases and recovery points on your VPS."],
  databases: ["Databases", "Inspect an instance, connect an application, or manage its lifecycle."],
  backups: ["Backups", "Download a recovery point or restore it into a separate database."],
  activity: ["Activity", "The latest 200 operation events from this control plane."],
};
let signedIn = false, syncID = 0, page = "overview";
let selected = "", tab = "overview", detailID = 0;
let restoreFrom = null, saving = false;

function showLogin(message = "") {
  signedIn = false; syncID++;
  for (const d of document.querySelectorAll("dialog[open]")) d.close();
  Object.keys(data).forEach(k => { data[k] = null; });
  $("app").hidden = $("boot").hidden = true; $("login").hidden = false;
  $("token").value = ""; note("login-note", message, Boolean(message)); $("token").focus();
}
async function api(path, { method = "GET", body, token } = {}) {
  const headers = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (token) headers.Authorization = `Bearer ${token}`;
  const res = await fetch(`/api/${path}`, { method, headers, credentials: "same-origin", cache: "no-store", body: JSON.stringify(body) });
  if (!res.ok) {
    let message;
    try { message = (await res.json()).error; } catch { /* non-JSON proxy errors */ }
    if (res.status === 401 && signedIn) showLogin("Your session expired. Sign in again.");
    const err = new Error(message || `Request failed (HTTP ${res.status}).`);
    err.status = res.status; throw err;
  }
  return res.status === 204 ? null : res.json();
}
async function showWorkspace() {
  signedIn = true;
  $("app").hidden = false; $("boot").hidden = $("login").hidden = true;
  note("notice"); note("sync-error"); route();
  await sync();
}
$("login-form").addEventListener("submit", async (e) => {
  e.preventDefault(); const submit = e.submitter;
  submit.disabled = true; note("login-note", "Signing in…");
  try {
    await api("session", { method: "POST", token: $("token").value.trim() });
    $("token").value = ""; await showWorkspace();
  } catch (err) { note("login-note", err.message, true); }
  finally { submit.disabled = false; }
});
$("logout").onclick = async () => {
  try { await api("session", { method: "DELETE" }); showLogin(); }
  catch (err) { note("notice", err.message, true); }
};
async function sync() {
  if (!signedIn) return;
  const id = ++syncID, keys = Object.keys(data);
  const results = await Promise.allSettled(keys.map(k => api(k)));
  if (id !== syncID || !signedIn) return;
  const previousDB = JSON.stringify(data.instances?.find(d => d.name === selected));
  const missing = []; let changed = false;
  results.forEach((res, i) => {
    if (res.status === "fulfilled") {
      if (JSON.stringify(data[keys[i]]) !== JSON.stringify(res.value)) changed = true;
      data[keys[i]] = res.value;
    } else { missing.push(`${pretty(keys[i])}: ${res.reason.message}`); }
  });
  note("sync-error", missing.length ? `Refresh incomplete. Showing the last available data.\n${missing.join("\n")}` : "", true);
  $("link-state").className = `chip ${missing.length ? "warn" : "good"}`;
  $("link-state").textContent = missing.length ? "Degraded" : "Host connected";
  $("link-state").title = `Last checked ${new Date().toLocaleTimeString()}`;
  if (changed) render();
  if ($("detail").open && data.instances) {
    const current = data.instances.find(d => d.name === selected);
    if (!current) $("detail").close();
    else if (tab === "overview" && previousDB !== JSON.stringify(current)) renderDetail();
    else if (tab === "overview") sampleMetrics(selected, detailID);
  }
}
$("refresh").onclick = async () => {
  $("refresh").disabled = true; try { await sync(); } finally { $("refresh").disabled = false; }
};
setInterval(() => { if (signedIn && !document.hidden) sync(); }, 15000);
document.addEventListener("visibilitychange", () => { if (!document.hidden && signedIn) sync(); });

function route() {
  const key = location.hash.slice(1);
  page = Object.hasOwn(pages, key) ? key : "overview";
  const [title, description] = pages[page];
  $("page-title").textContent = $("crumb").textContent = title;
  document.title = `${title} · OpenRDS`; $("page-description").textContent = description;
  document.querySelectorAll("[data-page]").forEach(a => {
    if (a.dataset.page === page) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  });
  $("stats").hidden = $("overview-side").hidden = $("host-panel").hidden = page !== "overview";
  $("board").hidden = !["overview", "databases"].includes(page);
  $("board").classList.toggle("wide", page === "databases");
  $("backup-panel").hidden = page !== "backups"; $("activity-panel").hidden = page !== "activity";
  render();
}
window.addEventListener("hashchange", () => {
  route();
  if (signedIn) { $("content").focus({ preventScroll: true }); window.scrollTo(0, 0); }
});
document.querySelector(".skip").onclick = e => { e.preventDefault(); $("content").focus(); };
function openFilter(status) {
  $("search").value = $("engine-filter").value = ""; $("state-filter").value = status;
  location.hash = "databases"; renderFleet();
}
function stat(label, value, hint, symbol, color, action) {
  const b = button("", action, "stat");
  b.append(make("span", "stat-label", label), glyph(symbol, color), make("span", "stat-value", String(value)), make("small", "", hint));
  return b;
}
function render() {
  if (page === "overview") {
    const list = data.instances, bk = data.backups;
    const host = data.host;
    const hostLine = (label, value) => make("div", "host-line", make("span", "muted", label), make("strong", "", value));
    $("host-info").replaceChildren(...(host ? [hostLine("Node", host.name), hostLine("CPU", `${host.cpus} cores`), hostLine("Memory", bytes(host.memoryBytes)), hostLine("Docker Engine", host.dockerVersion)] : [make("p", "muted", "Waiting for Docker host information…")]));
    $("stats").replaceChildren(
      stat("Databases", list?.length ?? "—", "Provisioned instances", "db", "blue", () => openFilter("")),
      stat("Running", list?.filter(running).length ?? "—", "Online instances", "pulse", "mint", () => openFilter("running")),
      stat("Needs attention", list?.filter(attention).length ?? "—", "Instances to inspect", "alert", "amber", () => openFilter("attention")),
      stat("Backups", bk?.length ?? "—", bk ? `${bytes(bk.reduce((n, b) => n + b.bytes, 0))} stored locally` : "Reading storage…", "archive", "purple", () => { location.hash = "backups"; }),
    );
    const items = data.activity?.slice(0, 4) || [];
    $("recent").replaceChildren(...(items.length ? items.map(e =>
      make("div", "event", glyph(e.result === "completed" ? "check" : e.result === "failed" ? "alert" : "pulse", e.result === "failed" ? "amber" : "mint"),
        make("div", "", make("b", "", e.action), make("small", "", `${e.name} · ${e.result}`), make("small", "", when(e.at))))) :
      [empty("pulse", data.activity ? "No operations yet" : "Reading history…", "Database operations appear here as they run.")]));
  }
  if (page === "overview" || page === "databases") renderFleet();
  if (page === "backups") renderBackups();
  if (page === "activity") renderActivity();
}
function renderFleet() {
  const all = data.instances, target = $("fleet-list");
  $("fleet-count").textContent = all?.length ?? "—";
  if (!all) { target.replaceChildren(empty("host", "Waiting for instance status", "The inventory is read from the local Docker engine.")); return; }
  if (!all.length) { target.replaceChildren(empty("db", "Create your first database", "Choose an engine and a host port. OpenRDS creates the data volume and credentials for you.", button("New database", () => openEditor()))); return; }
  const q = $("search").value.trim().toLowerCase(), eng = $("engine-filter").value, state = $("state-filter").value;
  const list = all.filter(d => (!eng || eng === d.engine) && (!state || (state === "attention" ? attention(d) : state === "running" ? running(d) : state === d.status))
    && (!q || d.name.includes(q) || String(d.port).includes(q)));
  $("fleet-count").textContent = q || eng || state ? `${list.length} of ${all.length}` : all.length;
  if (!list.length) {
    target.replaceChildren(empty("search", "No matching databases", "Try another name, port or filter.", button("Clear filters", () => {
      $("search").value = $("engine-filter").value = $("state-filter").value = ""; renderFleet();
    }))); return;
  }
  target.replaceChildren(table(["Instance", "Status", "Host port", "Details"], list.map(d => {
    const name = button("", () => showDetail(d.name), "db-name");
    name.append(glyph("db", d.engine === "postgres" ? "blue" : "amber"), make("span", "", make("strong", "", d.name), make("small", "", d.engineLabel)));
    const open = button("Inspect", () => showDetail(d.name), "text-btn"); open.setAttribute("aria-label", `Inspect ${d.name}`);
    return [name, statusChip(d), make("code", "", String(d.port)), open];
  })));
}
for (const id of ["search", "engine-filter", "state-filter"]) $(id).addEventListener("input", renderFleet);
document.addEventListener("keydown", e => {
  if (e.key === "/" && signedIn && !$("board").hidden && !document.querySelector("dialog[open]") && !e.target.closest("input,select,textarea")) {
    e.preventDefault(); $("search").focus();
  }
});
function renderBackups() {
  const list = data.backups, target = $("backup-list");
  $("backup-count").textContent = list?.length ?? "—";
  if (!list?.length) { target.replaceChildren(empty("archive", list ? "No backups yet" : "Reading backups…", "Inspect a running database and choose Create backup to save a recovery point.")); return; }
  target.replaceChildren(table(["Source database", "Created", "Size", "Actions"], list.map(b => {
    const download = make("a", "text-btn", "Download"); download.href = `/api/backups/${b.id}/download`; download.download = "";
    return [make("div", "stack", make("strong", "", b.name), make("small", "", engineLabel(b.engine))), when(b.createdAt), bytes(b.bytes),
      make("div", "cell-actions", download, button("Restore", () => openEditor(b), "text-btn"),
        button("Delete", () => confirmDelete(`Delete this backup?`, `The ${when(b.createdAt)} recovery point for ${b.name} will be removed from this VPS.`, () => api(`backups/${b.id}`, { method: "DELETE" })), "text-btn destructive"))];
  })));
}
function renderActivity() {
  const value = $("result-filter").value;
  const list = (data.activity || []).filter(e => !value || e.result === value);
  $("activity-list").replaceChildren(!list.length ? empty("pulse", "No matching operations", "Provisioning, lifecycle and backup operations are recorded here.") :
    table(["Operation", "Instance", "Outcome", "Time"], list.map(e => [e.action, make("strong", "", e.name), resultChip(e.result), when(e.at)])));
}
$("result-filter").oninput = renderActivity;

function suggestedPort(engine) {
  let port = engine === "postgres" ? 15432 : 13306;
  while (data.instances?.some(d => d.port === port)) port++;
  return port;
}
function engineChanged() {
  const eng = $("db-engine").value;
  $("db-port").value = suggestedPort(eng); $("db-memory").min = eng === "mysql" ? 512 : 128;
}
$("db-engine").onchange = engineChanged;
function openEditor(backup = null) {
  if (!saving) {
    restoreFrom = backup; $("editor-form").reset(); note("editor-note");
    $("editor-form").querySelector("details").open = false;
    $("editor-kicker").textContent = backup ? "Recovery" : "Provisioning";
    $("editor-title").textContent = backup ? "Restore into a new database" : "New database";
    $("editor-description").textContent = backup ? `Restore the ${when(backup.createdAt)} backup of ${backup.name}. The source database stays available.` : "Provision a private database with its own persistent volume and credentials.";
    $("editor-submit").textContent = backup ? "Restore database" : "Create database";
    $("db-name").value = backup ? `${backup.name.slice(0, 32)}-restore` : "";
    $("db-engine").value = backup?.engine || "postgres"; $("db-engine").disabled = Boolean(backup);
    engineChanged();
  }
  $("editor").showModal(); if (!saving) $("db-name").focus();
}
$("new-db").onclick = () => openEditor();
$("editor-form").onsubmit = async (e) => {
  e.preventDefault(); if (saving) return;
  const b = restoreFrom, spec = { name: $("db-name").value.trim(), engine: $("db-engine").value, port: Number($("db-port").value), cpus: Number($("db-cpus").value), memoryMiB: Number($("db-memory").value) };
  const fields = $("editor-form").querySelectorAll("input,select,button[type=submit]");
  saving = true; fields.forEach(f => { f.disabled = true; });
  const progress = `${b ? "Restoring" : "Provisioning"} ${spec.name}… The first image pull may take a few minutes. You can close this dialog while it runs.`;
  note("editor-note", progress); note("notice", progress);
  let inspect = false;
  try {
    await api(b ? `backups/${b.id}/restore` : "instances", { method: "POST", body: spec });
    inspect = $("editor").open; $("editor").close(); note("notice", `${spec.name} is ready.`);
  } catch (err) { note("editor-note", err.message, true); note("notice", err.message, true); }
  finally {
    saving = false; fields.forEach(f => { f.disabled = false; }); $("db-engine").disabled = Boolean(b);
    await sync();
  }
  if (inspect && signedIn && data.instances?.some(d => d.name === spec.name)) showDetail(spec.name);
};
document.querySelectorAll(".close").forEach(b => b.onclick = () => b.closest("dialog").close());

const fact = (title, value) => make("div", "fact", make("small", "", title), make("strong", "", value));
const block = (title, ...nodes) => make("section", "block", make("h3", "", title), ...nodes);
function showDetail(name) {
  selected = name; tab = "overview"; note("detail-note");
  if (!$("detail").open) $("detail").showModal();
  renderDetail();
}
$("detail").addEventListener("close", () => { detailID++; selected = ""; $("detail-body").replaceChildren(); });
document.querySelectorAll("[data-tab]").forEach(b => b.onclick = () => { tab = b.dataset.tab; note("detail-note"); renderDetail(); });
function renderDetail() {
  const d = data.instances?.find(db => db.name === selected);
  if (!d || !$("detail").open) return;
  const id = ++detailID, body = $("detail-body");
  $("detail-title").textContent = d.name; $("detail-engine").textContent = d.engineLabel;
  document.querySelectorAll("[data-tab]").forEach(b => b.setAttribute("aria-pressed", String(b.dataset.tab === tab)));
  if (tab === "connection") {
    const secret = make("div"), reveal = button("Reveal connection URI", async b => {
      b.disabled = true;
      try {
        const c = await api(`instances/${d.name}/connection`);
        if (id !== detailID) return;
        const code = make("pre", "", c.uri);
        const copy = (label, uri) => button(label, async () => {
          try { await navigator.clipboard.writeText(uri); note("detail-note", "Connection URI copied."); }
          catch { note("detail-note", "Select and copy the URI above."); }
        });
        secret.replaceChildren(code, make("div", "row", copy("Copy URI", c.uri), button("Hide", renderDetail, "text-btn")),
          make("details", "", make("summary", "", "Connect an application container"), make("p", "muted", "Attach your application container to this private network, then use its internal connection URI."),
            make("pre", "", `docker network connect ${c.network} YOUR_APP_CONTAINER`), make("pre", "", c.networkURI), copy("Copy container URI", c.networkURI)));
      } catch (err) { if (id === detailID) note("detail-note", err.message, true); }
      finally { b.disabled = false; }
    });
    secret.append(reveal);
    body.replaceChildren(make("div", "facts", fact("Host on VPS", "127.0.0.1"), fact("Port", String(d.port)), fact("Database", "app"), fact("User", "openrds")),
      block("Connection URI", secret), block("Connect from your computer",
        make("p", "muted", "Replace user@your-vps with your SSH login. Keep the tunnel open, then use the connection URI locally."),
        make("pre", "", `ssh -N -L ${d.port}:127.0.0.1:${d.port} user@your-vps`)));
  } else if (tab === "logs") {
    const output = make("pre", "", "Reading database logs…");
    const reload = button("Refresh logs", async b => {
      b.disabled = true;
      try {
        const v = await api(`instances/${d.name}/logs`);
        if (id === detailID) { output.textContent = v.text || "No logs available yet."; output.scrollTop = output.scrollHeight; }
      } catch (err) { if (id === detailID) output.textContent = err.message; }
      finally { b.disabled = false; }
    });
    body.replaceChildren(make("p", "muted", "Latest 120 container log lines, up to 64 KiB."), output, reload); reload.click();
  } else {
    const metrics = make("div", "meters", make("p", "muted", "Reading live usage…")); metrics.id = "metrics";
    const active = ["running", "starting", "unhealthy", "restarting"].includes(d.status);
    const toggle = button(active ? "Stop database" : "Start database", b => runAction(b, d, active ? "stop" : "start", active ? "Stopping" : "Starting"));
    const backup = button("Create backup", b => runAction(b, d, "backups", "Backing up"), "primary");
    toggle.disabled = d.busy; backup.disabled = d.busy || !running(d);
    body.replaceChildren(statusChip(d));
    if (d.pendingRestore && !d.busy) body.append(make("p", "notice error", "Restore incomplete. Inspect the logs and restore the backup into a fresh instance before using this data."));
    body.append(block("Instance configuration", make("div", "facts", fact("Private endpoint", `127.0.0.1:${d.port}`), fact("Created", when(d.createdAt)),
      fact("CPU limit", d.cpus ? `${d.cpus} cores` : "Uncapped"), fact("Memory limit", d.memoryMiB ? `${d.memoryMiB} MiB` : "Uncapped"))),
      block("Live resource usage", metrics), block("Operations", make("div", "row", backup, toggle)),
      make("div", "bottom-action", button("Delete database…", () => confirmDelete(`Delete ${d.name}?`, "This removes the database container and its data volume. Saved backups remain in the backup library.", () => api(`instances/${d.name}`, { method: "DELETE" })), "text-btn destructive")));
    sampleMetrics(d.name, id);
  }
}
async function sampleMetrics(name, id) {
  const target = $("metrics");
  if (!target) return;
  try {
    const u = await api(`instances/${name}/metrics`);
    if (id !== detailID) return;
    if (!u) { target.replaceChildren(make("p", "muted", "Live readings are available when the database is running.")); return; }
    const memory = fact("Memory / limit", u.MemUsage), meter = make("progress");
    meter.max = 100; meter.value = Number.parseFloat(u.MemPerc) || 0;
    meter.setAttribute("aria-label", "Memory in use"); meter.setAttribute("aria-valuetext", u.MemPerc); memory.append(meter);
    target.replaceChildren(fact("CPU", u.CPUPerc), memory, fact("Network in / out", u.NetIO), fact("Disk read / write", u.BlockIO));
  } catch (err) { if (id === detailID) target.replaceChildren(make("p", "muted", err.message)); }
}
async function runAction(btn, db, action, verb) {
  btn.disabled = true;
  const tell = (message, failed = false) => {
    if (selected === db.name && $("detail").open) note("detail-note", message, failed);
    note("notice", message, failed);
  };
  tell(`${verb} ${db.name}…`);
  try {
    await api(`instances/${db.name}/${action}`, { method: "POST" });
    tell(action === "backups" ? `Backup saved for ${db.name}. Open Backups to download or restore it.` : `${db.name} ${action === "start" ? "started" : "stopped"}.`);
  } catch (err) { tell(err.message, true); }
  finally { btn.disabled = false; await sync(); }
}
function confirmDelete(title, description, remove) {
  const dialog = $("confirm"), submit = $("confirm-submit");
  $("confirm-title").textContent = title; $("confirm-description").textContent = description; note("confirm-note");
  submit.onclick = async () => {
    submit.disabled = true; note("confirm-note", "Deleting…");
    try { await remove(); dialog.close(); note("notice", "Deletion complete."); }
    catch (err) { note("confirm-note", err.message, true); note("notice", err.message, true); }
    finally { submit.disabled = false; await sync(); }
  };
  dialog.showModal();
}

(async () => {
  try { await api("session"); await showWorkspace(); }
  catch (err) { showLogin(err.status === 401 ? "" : err.message); }
})();
