// The database inventory: one dense table with live load, replicas under
// their source, and a preview beside it, so most questions are answered
// without leaving the list. Select a row to preview it; open it for details.
import { h, icon, btn, menu, status, empty, isRunning, isLive, needsAttention, percent, bytes, sizeLabel, plural, ago, when, copyText, toast, api, chip } from "./lib.mjs";
import { state, subscribe, instance, can, go } from "./store.mjs";
import { databaseActions, start, confirmReboot } from "./actions.mjs";
import { sparkline } from "./charts.mjs";
import { replicationChip, lagLabel } from "./replicas.mjs";
import { multiServer, serverLabel, servers } from "./servers.mjs";

// Kept across visits so filters and the preview survive navigation.
const ui = { q: "", engine: "", server: "", status: "", sort: { key: "name", dir: "asc" }, selected: "" };

const matches = (d, q) => !q || d.name.includes(q) || String(d.port).includes(q) || d.engineLabel.toLowerCase().includes(q) || (d.serverName || "").toLowerCase().includes(q)
  || Object.entries(d.tags || {}).some(([k, v]) => `${k}=${v}`.toLowerCase().includes(q));

const STATUS_FILTERS = {
  running: { label: "Running", test: isRunning },
  stopped: { label: "Stopped", test: d => d.status === "stopped" && !d.busy },
  attention: { label: "Needs attention", test: needsAttention },
  busy: { label: "In progress", test: d => d.busy },
};

const BADGE = { postgres: "PG", mysql: "My", valkey: "Vk" };
export const engineBadge = (d, cls = "") => h("span", { class: `engine-badge ${d.engine} ${cls}`.trim(), "aria-hidden": "true" }, BADGE[d.engine]);

// Open the inventory with one status filter applied, such as from a dashboard tile.
export function showDatabases(status = "") {
  Object.assign(ui, { q: "", engine: "", server: "", status, selected: "" });
  location.hash = "#/databases";
}

export const cpuShare = (d) => d.live ? d.live.cpu / (d.cpus || state.host?.cpus || 1) : null;
const memShare = (d) => d.live && d.memoryMiB ? d.live.memory / (d.memoryMiB * 1048576) * 100 : null;

// A copyable endpoint: the loopback address, with the port emphasised.
export function endpointChip(d, { network = false } = {}) {
  const text = network ? `openrds-${d.name}` : `127.0.0.1:${d.port}`;
  return h("button", { type: "button", class: "endpoint", title: `Copy ${text}`, "aria-label": `Copy ${text}`,
    onclick: async (e) => {
      e.stopPropagation();
      const ok = await copyText(text);
      toast(ok ? `Copied ${text}.` : "Copy failed. Select and copy it.", ok ? "success" : "warn");
    } },
  network ? h("span", null, text) : [h("span", { class: "host" }, "127.0.0.1:"), h("strong", null, String(d.port))], icon("copy"));
}

function bar(share) {
  const tone = share >= 90 ? "critical" : share >= 75 ? "warn" : "";
  return h("span", { class: `bar ${tone}`.trim(), "aria-hidden": "true" }, h("span", { style: { width: `${Math.min(100, Math.max(share ?? 0, 0)).toFixed(1)}%` } }));
}

function loadCell(d) {
  if (!d.live) return h("span", { class: "muted" }, isLive(d) ? "Measuring…" : "—");
  const cpu = cpuShare(d), mem = memShare(d);
  return h("div", { class: "load", title: `CPU ${percent(cpu)} of ${d.cpus ? "its limit" : "the host"} · memory ${bytes(d.live.memory)}${d.memoryMiB ? ` of ${sizeMem(d)}` : ""}` },
    h("span", { class: "load-label" }, "CPU"), bar(cpu), h("span", { class: "load-val" }, percent(cpu)),
    h("span", { class: "load-label" }, "MEM"), bar(mem ?? 0), h("span", { class: "load-val" }, mem === null ? bytes(d.live.memory) : percent(mem)));
}
const sizeMem = (d) => d.memoryMiB >= 1024 && d.memoryMiB % 1024 === 0 ? `${d.memoryMiB / 1024} GiB` : `${d.memoryMiB} MiB`;

function backupsCell(d) {
  if (!d.backupRetentionDays) return h("span", { class: "status warn" }, icon("alert"), "Off");
  return h("span", { class: "stack" }, h("span", null, plural(d.backupRetentionDays, "day")),
    d.latestBackupAt ? h("small", { title: when(d.latestBackupAt) }, `Latest ${ago(d.latestBackupAt)}`) : h("small", null, "None yet"));
}

function rowMenu(d) {
  if (!can("operator")) return null;
  return menu("", databaseActions(d), { cls: "icon-only small ghost", labelIcon: "more", chevron: false, ariaLabel: `Actions for ${d.name}` });
}

// Sources in sort order, each followed by its replicas. A replica whose
// source is filtered out stands on its own.
function arrange(list) {
  const { key, dir } = ui.sort;
  const value = {
    name: d => d.name, status: d => (d.busy ? "0" : "") + d.status, size: d => d.cpus * 1e6 + d.memoryMiB,
    load: d => cpuShare(d) ?? -1, backups: d => d.backupRetentionDays, port: d => d.port,
  }[key] || (d => d.name);
  const sorted = (rows) => [...rows].sort((a, b) => {
    const x = value(a), y = value(b);
    const order = typeof x === "string" ? x.localeCompare(y) : (x > y) - (x < y);
    return dir === "asc" ? order : -order;
  });
  const shown = new Set(list.map(d => d.name));
  const out = [];
  for (const d of sorted(list.filter(d => !d.replicaOf || !shown.has(d.replicaOf)))) {
    out.push({ d, child: false });
    for (const r of sorted(list.filter(x => x.replicaOf === d.name))) out.push({ d: r, child: true });
  }
  return out;
}

// ---------- preview

const history = new Map(); // name -> { at, points }

async function loadHistory(name) {
  const cached = history.get(name);
  if (cached && Date.now() - cached.at < 25000) return cached.points;
  try {
    const v = await api(`instances/${name}/history?range=1h`);
    const points = v.available ? v.points || [] : [];
    history.set(name, { at: Date.now(), points });
    return points;
  } catch { return cached?.points || []; }
}

function trendTile(label, now, values, { max = null, hint = "" } = {}) {
  return h("div", { class: "trend" },
    h("div", { class: "trend-head" }, h("span", { class: "trend-label" }, label), h("strong", { class: "trend-now" }, now), hint ? h("small", null, hint) : null),
    sparkline(values, { max, label: `${label}, last hour` }));
}

function preview(d, close) {
  const hostCPUs = state.host?.cpus || 1;
  const base = d.cpus || hostCPUs;
  const limit = d.memoryMiB * 1048576;
  const tiles = (points) => [
    trendTile("CPU", d.live ? percent(cpuShare(d)) : "—", points.map(p => p.cpu / base), { max: 100, hint: d.cpus ? `Of the ${d.cpus} vCPU limit` : "Of the host" }),
    trendTile("Memory", d.live ? bytes(d.live.memory) : "—", points.map(p => p.memory), { max: limit || null, hint: d.memoryMiB ? `Of ${sizeMem(d)}` : "No limit" }),
    trendTile("Connections", d.live?.db ? String(Math.round(d.live.db.conns)) : "—", points.map(p => p.db ? p.db.conns : NaN), { hint: "Clients" }),
  ];
  const trends = h("div", { class: "trends" }, tiles(history.get(d.name)?.points || []));
  if (isLive(d)) loadHistory(d.name).then(points => { if (trends.isConnected) trends.replaceChildren(...tiles(points)); });
  const actions = [h("a", { class: "btn primary small", href: `#/databases/${d.name}` }, "Open", icon("arrow"))];
  if (isRunning(d)) actions.push(h("a", { class: "btn small", href: `#/databases/${d.name}/connectivity` }, icon("link"), "Connect"));
  if (can("operator")) {
    if (!isLive(d) && !d.busy && d.status !== "not provisioned") actions.push(btn([icon("play"), "Start"], () => start(d), "small"));
    else if (isLive(d)) { const b = btn([icon("reboot"), "Reboot"], () => confirmReboot(d), "small"); b.disabled = d.busy; actions.push(b); }
    actions.push(menu("", databaseActions(d), { cls: "icon-only small", labelIcon: "more", chevron: false, ariaLabel: `More actions for ${d.name}` }));
  }
  const facts = [
    multiServer() ? ["Server", h("a", { href: "#/servers" }, serverLabel(d))] : null,
    ["Endpoint", endpointChip(d)],
    d.privateHost ? ["Private network", h("code", null, `${d.privateHost}:${d.port}`)] : null,
    ["Network name", endpointChip(d, { network: true })],
    ["Size", sizeLabel(d)],
    ["Storage", d.diskGiB ? `${d.diskGiB} GiB limit` : "Uncapped"],
    ["Access", h("span", { class: "with-icon" }, icon(d.publicAccess ? "globe" : "lock"), d.publicAccess ? `Public · port ${d.publicPort}` : "Private", d.requireTLS ? " · TLS" : "")],
    ["Backups", d.backupRetentionDays ? `${plural(d.backupRetentionDays, "day")}${d.latestBackupAt ? ` · latest ${ago(d.latestBackupAt)}` : ""}` : h("span", { class: "status warn" }, icon("alert"), "Off")],
    d.pooler ? ["Pooler", h("code", null, `:${d.pooler.port}`)] : null,
    ["Created", ago(d.createdAt)],
  ].filter(Boolean);
  const replication = d.replicaOf
    ? h("section", { class: "pv-section" }, h("h3", null, "Replication"),
      h("div", { class: "pv-repl" }, h("span", null, "From ", h("a", { href: `#/databases/${d.replicaOf}` }, d.replicaOf)), replicationChip(d.replication),
        h("span", { class: "muted" }, `Lag ${lagLabel(d.replication?.lagSeconds)}`)))
    : (d.replicas || []).length ? h("section", { class: "pv-section" }, h("h3", null, "Read replicas"),
      h("ul", { class: "pv-list" }, d.replicas.map(n => { const r = instance(n); return h("li", null, h("a", { href: `#/databases/${n}` }, n), replicationChip(r?.replication), h("span", { class: "muted" }, lagLabel(r?.replication?.lagSeconds))); })))
      : null;
  const tags = Object.entries(d.tags || {});
  return h("aside", { class: "preview", "aria-label": `${d.name} preview` },
    h("header", { class: "pv-head" },
      h("div", { class: "pv-title" }, engineBadge(d, "large"),
        h("div", { class: "stack" }, h("p", { class: "kicker" }, d.engineLabel, d.replicaOf ? " · replica" : ""), h("h2", null, d.name), status(d))),
      h("button", { type: "button", class: "icon-btn ghost", "aria-label": "Close preview", onclick: close }, icon("close"))),
    h("div", { class: "pv-actions" }, actions),
    trends,
    h("dl", { class: "pv-facts" }, facts.map(([k, v]) => h("div", null, h("dt", null, k), h("dd", null, v)))),
    replication,
    tags.length ? h("section", { class: "pv-section" }, h("h3", null, "Tags"), h("div", { class: "tag-chips" }, tags.map(([k, v]) => h("span", { class: "tag-chip" }, v ? `${k}: ${v}` : k)))) : null);
}

// ---------- the view

export function databasesView() {
  const search = h("input", { type: "search", id: "search", value: ui.q, placeholder: "Filter by name, port, engine or tag", "aria-label": "Filter databases by name, port, engine or tag", autocomplete: "off" });
  const engineSel = h("select", { "aria-label": "Filter by engine" }, h("option", { value: "" }, "All engines"), (state.engines || []).map(e => h("option", { value: e.key, selected: e.key === ui.engine }, e.title)));
  search.addEventListener("input", () => { ui.q = search.value.trim().toLowerCase(); render(); });
  engineSel.addEventListener("change", () => { ui.engine = engineSel.value; render(); });
  const serverSel = h("select", { "aria-label": "Filter by server" });
  serverSel.addEventListener("change", () => { ui.server = serverSel.value; render(); });
  const fillServers = () => {
    serverSel.hidden = !multiServer();
    if (!servers().some(s => s.id === ui.server)) ui.server = "";
    serverSel.replaceChildren(h("option", { value: "" }, "All servers"), ...servers().map(s => h("option", { value: s.id, selected: s.id === ui.server }, s.name)));
  };
  const chips = h("div", { class: "segmented status-filter", role: "group", "aria-label": "Filter by status" });
  const summary = h("p", { class: "muted fleet-summary" });
  const list = h("div", { class: "fleet-list" });
  const side = h("div", { class: "fleet-side" });
  const layout = h("div", { class: "fleet-layout" }, h("section", { class: "panel fleet-panel" },
    h("div", { class: "toolbar fleet-toolbar" }, chips, h("div", { class: "toolbar-end" }, h("label", { class: "search" }, icon("search"), search, h("kbd", null, "/")), engineSel, serverSel)),
    list), side);
  const el = h("div", { class: "fleet-page" },
    h("div", { class: "page-head" }, h("div", null, h("h1", null, "Databases"), summary),
      can("operator") ? h("div", { class: "page-actions" }, h("a", { class: "btn primary", id: "new-db", href: "#/databases/new" }, icon("plus"), "Create database")) : null),
    layout);

  const wide = matchMedia("(min-width: 1180px)");
  function select(name) {
    if (!wide.matches) { go(`#/databases/${name}`); return; }
    ui.selected = ui.selected === name ? "" : name;
    render();
  }
  function closePreview() {
    const name = ui.selected;
    ui.selected = "";
    render();
    list.querySelector(`tr[data-name="${name}"]`)?.focus();
  }

  function renderChips(all) {
    const items = [["", "All", all.length], ...Object.entries(STATUS_FILTERS).map(([k, f]) => [k, f.label, all.filter(f.test).length])]
      .filter(([k, , n]) => !k || n > 0 || ui.status === k || k === "running" || k === "stopped");
    chips.replaceChildren(...items.map(([k, label, n]) => h("button", { type: "button", "aria-pressed": String(ui.status === k), class: k === "attention" && n ? "attention" : null,
      onclick: () => { ui.status = ui.status === k ? "" : k; render(); } }, label, h("span", { class: "seg-count" }, String(n)))));
  }

  function headCell(key, label, cls = "") {
    if (!key) return h("th", { scope: "col", class: cls || null }, label);
    const active = ui.sort.key === key;
    return h("th", { scope: "col", class: cls || null, "aria-sort": active ? (ui.sort.dir === "asc" ? "ascending" : "descending") : "none" },
      h("button", { type: "button", class: "sort-btn", onclick: () => { ui.sort = { key, dir: active && ui.sort.dir === "asc" ? "desc" : "asc" }; render(); } },
        label, icon(active ? (ui.sort.dir === "asc" ? "sort-up" : "sort-down") : "sort", "icon sort-icon")));
  }

  function row({ d, child }) {
    const tags = Object.entries(d.tags || {});
    const tr = h("tr", { class: `fleet-row${child ? " child" : ""}${d.name === ui.selected ? " selected" : ""}`, tabindex: "0", "data-name": d.name },
      h("td", { class: "c-name" }, h("div", { class: "db-cell" }, child ? h("span", { class: "tree", "aria-hidden": "true" }) : null, engineBadge(d),
        h("span", { class: "stack" }, h("a", { href: `#/databases/${d.name}`, class: "db-link", "aria-label": `Open ${d.name}` }, d.name),
          h("small", null, d.engineLabel, d.replicaOf ? [" · replica of ", h("span", { class: "of" }, d.replicaOf)] : null),
          tags.length ? h("span", { class: "tag-chips" }, tags.slice(0, 2).map(([k, v]) => h("span", { class: "tag-chip" }, v ? `${k}: ${v}` : k)), tags.length > 2 ? h("span", { class: "tag-chip more" }, `+${tags.length - 2}`) : null) : null))),
      h("td", { class: "c-status" }, h("span", { class: "stack" }, status(d),
        d.replicaOf && isRunning(d) ? h("small", { class: "repl-line" }, d.replication?.state === "replicating" ? `Lag ${lagLabel(d.replication.lagSeconds)}` : replicationChip(d.replication)) : null,
        (d.replicas || []).length ? h("small", null, plural(d.replicas.length, "replica")) : null)),
      h("td", { class: "c-endpoint" }, multiServer() ? h("span", { class: "stack" }, endpointChip(d), h("small", { class: "server-line" }, icon("host"), serverLabel(d))) : endpointChip(d)),
      h("td", { class: "c-size" }, h("span", { class: "stack" }, h("span", null, sizeLabel(d)), h("small", null, d.diskGiB ? `${d.diskGiB} GiB disk` : "Uncapped disk"))),
      h("td", { class: "c-load" }, loadCell(d)),
      h("td", { class: "c-backups" }, backupsCell(d)),
      h("td", { class: "c-actions" }, rowMenu(d)));
    tr.addEventListener("click", (e) => {
      if (e.target.closest("a, button, .menu-wrap, input, select")) return;
      select(d.name);
    });
    tr.addEventListener("keydown", (e) => {
      if (e.target !== tr) return;
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); select(d.name); }
      else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        const next = e.key === "ArrowDown" ? tr.nextElementSibling : tr.previousElementSibling;
        next?.focus();
        if (next && ui.selected) { ui.selected = next.dataset.name; render(); list.querySelector(`tr[data-name="${ui.selected}"]`)?.focus(); }
      } else if (e.key === "Escape" && ui.selected) closePreview();
    });
    return tr;
  }

  function render() {
    const all = state.instances;
    if (!all) {
      summary.textContent = "Reading the inventory…";
      list.replaceChildren(empty("host", "Reading the inventory…", "Status comes from Docker."));
      side.replaceChildren();
      return;
    }
    const running = all.filter(isRunning).length, replicas = all.filter(d => d.replicaOf).length;
    summary.textContent = all.length ? `${plural(all.length, "database")} · ${running} running${replicas ? ` · ${plural(replicas, "read replica")}` : ""}` : "None yet.";
    renderChips(all);
    fillServers();
    if (!instance(ui.selected)) ui.selected = "";
    const filter = STATUS_FILTERS[ui.status];
    const shown = all.filter(d => (!ui.engine || d.engine === ui.engine) && (!ui.server || (d.server || "local") === ui.server) && (!filter || filter.test(d)) && matches(d, ui.q));
    if (!all.length) {
      list.replaceChildren(can("operator") ? empty("db", "Create your first database", "Ready in about a minute.", h("a", { class: "btn primary", href: "#/databases/new" }, icon("plus"), "Create database"))
        : empty("db", "No databases yet", "Ask an operator to create one."));
    } else if (!shown.length) {
      list.replaceChildren(empty("search", "No matching databases", "Try another name, port, tag or filter.", btn("Clear filters", () => {
        ui.q = ui.engine = ui.server = ui.status = ""; search.value = ""; engineSel.value = ""; render();
      })));
    } else {
      const focused = document.activeElement?.closest?.("tr[data-name]")?.dataset.name;
      list.replaceChildren(h("div", { class: "table-wrap fleet-wrap" }, h("table", { id: "fleet-list", class: "fleet" },
        h("caption", { class: "sr-only" }, "Databases. Select a row to preview it."),
        h("thead", null, h("tr", null, headCell("name", "Database", "c-name"), headCell("status", "Status", "c-status"), headCell("port", "Endpoint", "c-endpoint"),
          headCell("size", "Size", "c-size"), headCell("load", "Load", "c-load"), headCell("backups", "Backups", "c-backups"), headCell("", h("span", { class: "sr-only" }, "Actions"), "c-actions"))),
        h("tbody", null, arrange(shown).map(row)))));
      if (focused) list.querySelector(`tr[data-name="${focused}"]`)?.focus({ preventScroll: true });
    }
    const d = instance(ui.selected);
    layout.classList.toggle("with-preview", Boolean(d));
    side.replaceChildren(d ? preview(d, closePreview) : "");
  }

  const onKey = (e) => { if (e.key === "Escape" && ui.selected && !document.querySelector("dialog[open]")) closePreview(); };
  document.addEventListener("keydown", onKey);
  const onWidth = () => { if (!wide.matches && ui.selected) { ui.selected = ""; render(); } };
  wide.addEventListener("change", onWidth);
  render();
  const stop = subscribe(changed => { if (changed.has("instances") || changed.has("host") || changed.has("engines") || changed.has("servers")) render(); });
  return { el, title: "Databases", nav: "databases", crumbs: [["Databases"]], destroy() { stop(); document.removeEventListener("keydown", onKey); wide.removeEventListener("change", onWidth); } };
}
