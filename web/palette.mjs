// Jump to: one box to reach any page, database, server or common action.
// Opened from the sidebar or with Ctrl/Cmd K.
import { h, icon, nextId, statusOf } from "./lib.mjs";
import { state, can } from "./store.mjs";

const PAGES = [
  ["Dashboard", "#/", "overview"],
  ["Databases", "#/databases", "db"],
  ["Servers", "#/servers", "host"],
  ["Events", "#/events", "bell"],
  ["Backups", "#/backups", "archive"],
  ["Snapshots", "#/backups/snapshots", "camera"],
  ["Backup schedules", "#/backups/schedules", "history"],
  ["Backup storage", "#/backup-storage", "cloud"],
  ["Getting started", "#/getting-started", "spark"],
  ["Account settings", "#/account", "key"],
];

function entries() {
  const list = [];
  if (can("operator")) list.push({ group: "Actions", label: "Create database", hint: "New PostgreSQL, MySQL or Valkey", href: "#/databases/new", icon: "plus" });
  if (can("admin")) list.push({ group: "Actions", label: "Add a server", hint: "Run databases on another machine", href: "#/servers", icon: "plus", open: "add-server" });
  for (const [label, href, symbol] of PAGES) list.push({ group: "Pages", label, href, icon: symbol });
  if (can("admin")) list.push({ group: "Pages", label: "Users", href: "#/users", icon: "users" }, { group: "Pages", label: "Notifications", href: "#/notifications", icon: "alert" });
  for (const d of [...(state.instances || [])].sort((a, b) => a.name.localeCompare(b.name))) {
    const st = statusOf(d);
    list.push({ group: "Databases", label: d.name, hint: `${d.engineLabel} · ${st.label}${(state.servers || []).length > 1 ? ` · ${d.serverName}` : ""}`,
      href: `#/databases/${d.name}`, icon: "db", tone: st.tone, keywords: `${d.engine} ${d.serverName || ""} ${d.port}` });
  }
  for (const s of state.servers || []) {
    list.push({ group: "Servers", label: s.name, hint: `${s.reachable ? "Online" : "Offline"} · ${s.databases.length} database${s.databases.length === 1 ? "" : "s"}`,
      href: "#/servers", icon: "host", tone: s.reachable ? "good" : "critical", keywords: `${s.privateAddress || ""} ${s.publicAddress || ""}` });
  }
  return list;
}

// Every typed word appears in the label or its keywords; label starts rank first.
function match(entry, words) {
  const text = `${entry.label} ${entry.hint || ""} ${entry.keywords || ""} ${entry.group}`.toLowerCase();
  if (!words.every(w => text.includes(w))) return -1;
  return entry.label.toLowerCase().startsWith(words[0] || "") ? 0 : 1;
}

let open = null;

export function openPalette() {
  if (open) { open.querySelector("input").focus(); return; }
  const listID = nextId("jump-list");
  const input = h("input", { type: "text", class: "palette-input", placeholder: "Jump to a page, database or server", autocomplete: "off", spellcheck: "false",
    role: "combobox", "aria-expanded": "true", "aria-controls": listID, "aria-autocomplete": "list", "aria-label": "Jump to" });
  const list = h("ul", { class: "palette-list", id: listID, role: "listbox", "aria-label": "Results" });
  const empty = h("p", { class: "palette-empty", hidden: true }, "Nothing matches.");
  const dlg = h("dialog", { class: "palette", "aria-label": "Jump to" },
    h("div", { class: "palette-search" }, icon("search"), input),
    list, empty,
    h("footer", { class: "palette-foot" }, h("span", null, "Arrow keys to move"), h("span", null, h("kbd", null, "Enter"), " to open"), h("span", null, h("kbd", null, "Esc"), " to close")));
  let shown = [], active = 0;

  function go(entry) {
    dlg.close();
    location.hash = entry.href;
    if (entry.open) setTimeout(() => document.getElementById(entry.open)?.click(), 250);
  }
  function render() {
    const words = input.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
    shown = entries().map(e => [e, match(e, words)]).filter(([, rank]) => rank >= 0)
      .sort((a, b) => a[1] - b[1]).map(([e]) => e).slice(0, 40);
    active = Math.min(active, Math.max(shown.length - 1, 0));
    empty.hidden = shown.length > 0;
    let group = "";
    list.replaceChildren(...shown.flatMap((e, i) => {
      const items = [];
      if (e.group !== group && !words.length) { group = e.group; items.push(h("li", { class: "palette-group", role: "presentation" }, group)); }
      const option = h("li", { id: `${listID}-${i}`, role: "option", class: "palette-item", "aria-selected": String(i === active) },
        h("span", { class: `palette-icon${e.tone ? ` ${e.tone}` : ""}` }, icon(e.icon)),
        h("span", { class: "palette-text" }, h("strong", null, e.label), e.hint ? h("small", null, e.hint) : null),
        i === active ? h("kbd", null, "Enter") : null);
      option.addEventListener("click", () => go(e));
      option.addEventListener("mousemove", () => { if (active !== i) { active = i; render(); } });
      items.push(option);
      return items;
    }));
    input.setAttribute("aria-activedescendant", shown.length ? `${listID}-${active}` : "");
    list.querySelector("[aria-selected=true]")?.scrollIntoView({ block: "nearest" });
  }
  input.addEventListener("input", () => { active = 0; render(); });
  input.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { e.preventDefault(); active = (active + 1) % Math.max(shown.length, 1); render(); }
    else if (e.key === "ArrowUp") { e.preventDefault(); active = (active - 1 + shown.length) % Math.max(shown.length, 1); render(); }
    else if (e.key === "Enter" && shown[active]) { e.preventDefault(); go(shown[active]); }
  });
  dlg.addEventListener("click", (e) => { if (e.target === dlg) dlg.close(); });
  dlg.addEventListener("close", () => { dlg.remove(); open = null; });
  document.body.append(dlg);
  open = dlg;
  dlg.showModal();
  render();
  input.focus();
}
