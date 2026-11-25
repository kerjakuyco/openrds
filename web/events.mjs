// The operation history kept by the control plane.
import { h, icon, dataTable, empty, when, ago } from "./lib.mjs";
import { state, subscribe, instance } from "./store.mjs";
import { resultChip } from "./instance.mjs";

const ui = { name: "", result: "", q: "", limit: 50 };

export function eventsView() {
  const nameSel = h("select", { "aria-label": "Filter by database or user" });
  const resultSel = h("select", { "aria-label": "Filter by outcome", id: "result-filter" }, [["", "All outcomes"], ["started", "Started"], ["completed", "Completed"], ["failed", "Failed"]]
    .map(([v, label]) => h("option", { value: v, selected: v === ui.result }, label)));
  const search = h("input", { type: "search", id: "search", value: ui.q, placeholder: "Filter by operation, detail or person", "aria-label": "Filter events by operation, detail or person", autocomplete: "off" });
  nameSel.addEventListener("change", () => { ui.name = nameSel.value; ui.limit = 50; render(); });
  resultSel.addEventListener("change", () => { ui.result = resultSel.value; ui.limit = 50; render(); });
  search.addEventListener("input", () => { ui.q = search.value.trim().toLowerCase(); ui.limit = 50; render(); });
  const count = h("span", { class: "counter" });
  const list = h("div", { id: "activity-list" });
  const more = h("div", { class: "more-row" });
  const el = h("div", null,
    h("div", { class: "page-head" }, h("div", null, h("h1", null, "Events"), h("p", { class: "muted" }, "The last 1,000 actions."))),
    h("section", { class: "panel" },
      h("header", { class: "panel-head" }, h("div", null, h("h2", null, "Operation history", count))),
      h("div", { class: "toolbar" }, h("label", { class: "search" }, icon("search"), search, h("kbd", null, "/")), nameSel, resultSel),
      list, more));

  function render() {
    const all = state.activity;
    if (!all) { list.replaceChildren(empty("bell", "Reading history…", "")); return; }
    const names = [...new Set(all.map(e => e.name))].sort();
    if (ui.name && !names.includes(ui.name)) ui.name = "";
    nameSel.replaceChildren(h("option", { value: "" }, "All databases and users"), ...names.map(n => h("option", { value: n, selected: n === ui.name }, n)));
    const shown = all.filter(e => (!ui.name || e.name === ui.name) && (!ui.result || e.result === ui.result)
      && (!ui.q || `${e.action} ${e.detail || ""} ${e.name} ${e.actor || ""}`.toLowerCase().includes(ui.q)));
    count.textContent = shown.length === all.length ? String(all.length) : `${shown.length} of ${all.length}`;
    list.replaceChildren(shown.length ? dataTable({ caption: "Events", rows: shown.slice(0, ui.limit), columns: [
      { key: "at", label: "Time", value: e => h("div", { class: "stack" }, h("span", null, when(e.at)), h("small", null, ago(e.at))) },
      { key: "name", label: "Resource", value: e => e.kind ? h("span", { class: "resource" }, icon({ user: "users", notifications: "alert", disk: "disk" }[e.kind] || "cloud"), e.name)
        : instance(e.name) ? h("a", { href: `#/databases/${e.name}` }, e.name) : h("span", null, e.name) },
      { key: "action", label: "Operation", value: e => e.action },
      { key: "result", label: "Outcome", value: e => resultChip(e.result) },
      { key: "actor", label: "By", value: e => e.actor && e.actor !== "system" ? e.actor : h("span", { class: "muted" }, e.actor ? "OpenRDS" : "—") },
      { key: "detail", label: "Detail", cls: "wrap", value: e => e.detail || h("span", { class: "muted" }, "—") },
    ] }) : empty("bell", all.length ? "No matching events" : "No operations yet", all.length ? "Try another filter." : "Actions show up here."));
    more.replaceChildren(shown.length > ui.limit ? h("button", { type: "button", class: "btn", onclick: () => { ui.limit += 50; render(); } }, `Show more (${shown.length - ui.limit} left)`) : "");
  }
  render();
  const stop = subscribe(changed => { if (changed.has("activity") || changed.has("instances")) render(); });
  return { el, title: "Events", nav: "events", crumbs: [["Events"]], destroy: stop };
}
