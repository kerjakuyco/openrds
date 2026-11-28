// The operation history kept by the control plane.
import { h, icon, api, dataTable, empty, when, ago, query, serverPager } from "./lib.mjs";
import { subscribe, instance } from "./store.mjs";
import { resultChip } from "./instance.mjs";

const PAGE = 50;
const ui = { name: "", result: "", q: "", offset: 0 };

export function eventsView() {
  const nameSel = h("select", { "aria-label": "Filter by database or user" });
  const resultSel = h("select", { "aria-label": "Filter by outcome", id: "result-filter" }, [["", "All outcomes"], ["started", "Started"], ["completed", "Completed"], ["failed", "Failed"]]
    .map(([v, label]) => h("option", { value: v, selected: v === ui.result }, label)));
  const search = h("input", { type: "search", id: "search", value: ui.q, placeholder: "Filter by operation, detail or person", "aria-label": "Filter events by operation, detail or person", autocomplete: "off" });
  let typing = 0;
  nameSel.addEventListener("change", () => { ui.name = nameSel.value; refilter(); });
  resultSel.addEventListener("change", () => { ui.result = resultSel.value; refilter(); });
  search.addEventListener("input", () => {
    clearTimeout(typing);
    typing = setTimeout(() => { ui.q = search.value.trim(); refilter(); }, 250);
  });
  const count = h("span", { class: "counter" });
  const list = h("div", { id: "activity-list" });
  const pages = h("div");
  const el = h("div", null,
    h("div", { class: "page-head" }, h("div", null, h("h1", null, "Events"), h("p", { class: "muted" }, "The last 1,000 actions."))),
    h("section", { class: "panel" },
      h("header", { class: "panel-head" }, h("div", null, h("h2", null, "Operation history", count))),
      h("div", { class: "toolbar" }, h("label", { class: "search" }, icon("search"), search, h("kbd", null, "/")), nameSel, resultSel),
      list, pages));

  let page = null, seq = 0, failed = "";
  // Rows of the old filter never show under the new one.
  function refilter() {
    ui.offset = 0;
    page = null;
    failed = "";
    render();
    load();
  }
  async function load() {
    const mine = ++seq;
    try {
      const next = await api(`activity?${query({ name: ui.name, result: ui.result, q: ui.q, offset: ui.offset, limit: PAGE })}`);
      if (mine !== seq) return;
      page = next;
      failed = "";
      if (page.offset > 0 && page.offset >= page.total) { ui.offset = Math.max(0, Math.floor((page.total - 1) / PAGE) * PAGE); load(); return; }
    } catch (err) {
      if (mine !== seq) return;
      failed = err.message;
    }
    render();
  }

  function render() {
    if (!page) { list.replaceChildren(failed ? empty("error", "Could not read the history", failed) : empty("bell", "Reading history…", "")); pages.replaceChildren(); return; }
    if (ui.name && !page.names.includes(ui.name)) page.names = [...page.names, ui.name].sort();
    nameSel.replaceChildren(h("option", { value: "" }, "All databases and users"), ...page.names.map(n => h("option", { value: n, selected: n === ui.name }, n)));
    const filtered = ui.name || ui.result || ui.q;
    count.textContent = filtered ? `${page.total.toLocaleString()} of ${page.count.toLocaleString()}` : page.count.toLocaleString();
    list.replaceChildren(page.items.length ? dataTable({ caption: "Events", rows: page.items, columns: [
      { key: "at", label: "Time", value: e => h("div", { class: "stack" }, h("span", null, when(e.at)), h("small", null, ago(e.at))) },
      { key: "name", label: "Resource", value: e => e.kind ? h("span", { class: "resource" }, icon({ user: "users", notifications: "alert", disk: "disk", server: "host" }[e.kind] || "cloud"), e.kind === "server" ? h("a", { href: "#/servers" }, e.name) : e.name)
        : instance(e.name) ? h("a", { href: `#/databases/${e.name}` }, e.name) : h("span", null, e.name) },
      { key: "action", label: "Operation", value: e => e.action },
      { key: "result", label: "Outcome", value: e => resultChip(e.result) },
      { key: "actor", label: "By", value: e => e.actor && e.actor !== "system" ? e.actor : h("span", { class: "muted" }, e.actor ? "OpenRDS" : "—") },
      { key: "detail", label: "Detail", cls: "wrap", value: e => e.detail || h("span", { class: "muted" }, "—") },
    ] }) : empty("bell", page.count ? "No matching events" : "No operations yet", page.count ? "Try another filter." : "Actions show up here."));
    pages.replaceChildren(serverPager(page, PAGE, (offset) => { ui.offset = offset; load(); list.scrollIntoView({ block: "nearest" }); }, "Event pages"));
  }
  render();
  load();
  // New events arrive with each refresh; the page follows them.
  const stop = subscribe(changed => { if (changed.has("activity")) load(); else if (changed.has("instances")) render(); });
  return { el, title: "Events", nav: "events", crumbs: [["Events"]], destroy: () => { stop(); clearTimeout(typing); } };
}
