// The database inventory: select a row to act on it, or open it for details.
import { h, icon, menu, status, dataTable, empty, isRunning, needsAttention, percent, meter, sizeLabel, plural, ago, when } from "./lib.mjs";
import { state, subscribe, instance, can } from "./store.mjs";
import { databaseActions } from "./actions.mjs";

// Kept across visits so filters and selection survive navigation.
const ui = { q: "", engine: "", status: "", sort: { key: "name", dir: "asc" }, selected: "" };

const matches = (d, q) => !q || d.name.includes(q) || String(d.port).includes(q) || d.engineLabel.toLowerCase().includes(q)
  || Object.entries(d.tags || {}).some(([k, v]) => `${k}=${v}`.toLowerCase().includes(q));

const STATUS_FILTERS = { running: isRunning, stopped: d => d.status === "stopped" && !d.busy, attention: needsAttention, busy: d => d.busy };

// Open the inventory with one status filter applied, such as from a dashboard tile.
export function showDatabases(status = "") {
  Object.assign(ui, { q: "", engine: "", status, selected: "" });
  location.hash = "#/databases";
}

export const cpuShare = (d) => d.live ? d.live.cpu / (d.cpus || state.host?.cpus || 1) : null;

export function cpuCell(d) {
  const share = cpuShare(d);
  if (share === null) return h("span", { class: "muted" }, "—");
  return h("span", { class: "with-meter" }, percent(share), meter(share, 100, `CPU ${percent(share)}`));
}

export function databasesView() {
  const search = h("input", { type: "search", id: "search", value: ui.q, placeholder: "Filter by name, port or tag", "aria-label": "Filter databases by name, port or tag", autocomplete: "off" });
  const engineSel = h("select", { "aria-label": "Filter by engine" }, h("option", { value: "" }, "All engines"), (state.engines || []).map(e => h("option", { value: e.key, selected: e.key === ui.engine }, e.title)));
  const statusSel = h("select", { "aria-label": "Filter by status" }, [["", "All statuses"], ["running", "Running"], ["stopped", "Stopped"], ["attention", "Needs attention"], ["busy", "In progress"]]
    .map(([v, label]) => h("option", { value: v, selected: v === ui.status }, label)));
  search.addEventListener("input", () => { ui.q = search.value.trim().toLowerCase(); render(); });
  engineSel.addEventListener("change", () => { ui.engine = engineSel.value; render(); });
  statusSel.addEventListener("change", () => { ui.status = statusSel.value; render(); });
  const count = h("span", { class: "counter" });
  const actions = h("div", { class: "panel-actions" });
  const list = h("div", { id: "fleet-list" });
  const el = h("div", null,
    h("div", { class: "page-head" }, h("div", null, h("h1", null, "Databases"), h("p", { class: "muted" }, "Every database on this server.")),
      can("operator") ? h("div", { class: "page-actions" }, h("a", { class: "btn primary", id: "new-db", href: "#/databases/new" }, icon("plus"), "Create database")) : null),
    h("section", { class: "panel" },
      h("header", { class: "panel-head" }, h("div", null, h("h2", null, "Databases", count)), actions),
      h("div", { class: "toolbar" }, h("label", { class: "search" }, icon("search"), search, h("kbd", null, "/")), engineSel, statusSel),
      list));

  function onSort(key) {
    ui.sort = { key, dir: ui.sort.key === key && ui.sort.dir === "asc" ? "desc" : "asc" };
    render();
  }

  function renderActions() {
    if (!can("operator")) { actions.replaceChildren(h("span", { class: "chip neutral", title: "Viewers can look but not change." }, icon("eye"), "Read only")); return; }
    const d = instance(ui.selected);
    const items = d ? databaseActions(d) : [{ label: "Select a database first", disabled: true, run: () => {} }];
    actions.replaceChildren(
      d ? h("a", { class: "btn", href: `#/databases/${d.name}/modify` }, icon("edit"), "Modify") : h("button", { type: "button", class: "btn", disabled: true }, icon("edit"), "Modify"),
      menu("Actions", items));
  }

  function render() {
    const all = state.instances;
    if (!all) { count.textContent = "—"; list.replaceChildren(empty("host", "Reading the inventory…", "Status comes from Docker.")); renderActions(); return; }
    if (!instance(ui.selected)) ui.selected = "";
    const shown = all.filter(d => (!ui.engine || d.engine === ui.engine) && (!ui.status || STATUS_FILTERS[ui.status](d)) && matches(d, ui.q));
    count.textContent = ui.q || ui.engine || ui.status ? `${shown.length} of ${all.length}` : String(all.length);
    renderActions();
    if (!all.length) {
      list.replaceChildren(can("operator") ? empty("db", "Create your first database", "Ready in about a minute.", h("a", { class: "btn primary", href: "#/databases/new" }, icon("plus"), "Create database"))
        : empty("db", "No databases yet", "Ask an operator to create one."));
      return;
    }
    if (!shown.length) {
      list.replaceChildren(empty("search", "No matching databases", "Try another name, port, tag or filter.", h("button", { type: "button", class: "btn", onclick: () => {
        ui.q = ui.engine = ui.status = ""; search.value = ""; engineSel.value = ""; statusSel.value = ""; render();
      } }, "Clear filters")));
      return;
    }
    const columns = [
      can("operator") && { key: "select", label: h("span", { class: "sr-only" }, "Select"), cls: "select", value: d => {
        const radio = h("input", { type: "radio", name: "selected-db", value: d.name, checked: d.name === ui.selected, "aria-label": `Select ${d.name}` });
        radio.addEventListener("change", () => { ui.selected = d.name; render(); });
        return radio;
      } },
      { key: "name", label: "DB identifier", sort: d => d.name, value: d => h("div", { class: "db-cell" }, h("span", { class: `engine-badge ${d.engine}`, "aria-hidden": "true" }, { postgres: "PG", mysql: "My", valkey: "Vk" }[d.engine]),
        h("span", { class: "stack" }, h("a", { href: `#/databases/${d.name}`, class: "db-link", "aria-label": `Open ${d.name}` }, d.name), h("small", null, d.engineLabel),
          Object.keys(d.tags || {}).length ? h("span", { class: "tag-chips" }, Object.entries(d.tags).slice(0, 2).map(([k, v]) => h("span", { class: "tag-chip" }, v ? `${k}: ${v}` : k))) : null)) },
      { key: "status", label: "Status", sort: d => (d.busy ? "0" : "") + d.status, value: status },
      { key: "size", label: "Size", sort: d => d.cpus * 1e6 + d.memoryMiB, value: d => sizeLabel(d) },
      { key: "cpu", label: "CPU", sort: d => cpuShare(d) ?? -1, value: cpuCell },
      { key: "storage", label: "Storage", sort: d => d.diskGiB, value: d => d.diskGiB ? `${d.diskGiB} GiB` : h("span", { class: "muted" }, "Uncapped") },
      { key: "port", label: "Port", sort: d => d.port, cls: "num", value: d => h("code", null, String(d.port)) },
      { key: "backups", label: "Backups", sort: d => d.backupRetentionDays, value: d => h("div", { class: "stack" },
        d.backupRetentionDays ? h("span", null, plural(d.backupRetentionDays, "day")) : h("span", { class: "status warn" }, icon("alert"), "Off"),
        d.latestBackupAt ? h("small", { title: when(d.latestBackupAt) }, `Latest ${ago(d.latestBackupAt)}`) : null) },
    ];
    list.replaceChildren(dataTable({ caption: "Databases", columns: columns.filter(Boolean), rows: shown, sortKey: ui.sort.key, sortDir: ui.sort.dir, onSort, rowClass: d => d.name === ui.selected ? "selected" : "" }));
  }

  render();
  const stop = subscribe(changed => { if (changed.has("instances") || changed.has("host")) render(); });
  return { el, title: "Databases", nav: "databases", crumbs: [["Databases"]], destroy: stop };
}

