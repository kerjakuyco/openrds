// Getting started: a checklist that ticks itself off from what OpenRDS sees,
// plus plain-language explanations of the main ideas.
import { h, icon, btn, panel, keyValues } from "./lib.mjs";
import { state, can, subscribe } from "./store.mjs";

const flag = (key) => { try { return localStorage.getItem(key) === "1"; } catch { return false; } };
const setFlag = (key, on) => { try { on ? localStorage.setItem(key, "1") : localStorage.removeItem(key); } catch { /* private mode */ } };
export const markDownloaded = () => setFlag("openrds-downloaded", true);

export function steps() {
  const dbs = state.instances || [];
  const first = [...dbs].sort((a, b) => a.name.localeCompare(b.name))[0];
  const unprotected = dbs.find(d => !d.backupRetentionDays);
  const list = [];
  if (state.me?.token && state.users && !state.users.length) {
    list.push({ id: "account", title: "Create your administrator account", done: false, href: "#/account",
      text: "Sign in with your own account.", action: "Create account" });
  }
  list.push(
    { id: "create", title: "Create your first database", done: dbs.length > 0, href: "#/databases/new", action: "Create database",
      text: "PostgreSQL, MySQL or Valkey, ready in a minute." },
    { id: "connect", title: "Connect your app", done: flag("openrds-connected"), href: first ? `#/databases/${first.name}` : "#/databases/new", action: "Open the guide",
      text: "Copy a snippet for your language." },
    { id: "backups", title: "Turn on automated backups", done: dbs.length > 0 && !unprotected, href: unprotected ? `#/databases/${unprotected.name}/modify` : "#/automated-backups", action: "Set a schedule",
      text: "Daily backups, kept as long as you choose." },
    { id: "offsite", title: "Keep an offsite copy", done: flag("openrds-downloaded"), href: "#/snapshots", action: "View snapshots",
      text: "Download a backup in case the server is lost." },
  );
  if (can("admin") && !state.me?.token) {
    list.push({ id: "team", title: "Invite your team", done: (state.users?.length || 0) > 1, href: "#/users", action: "Invite people",
      text: "Give everyone their own sign-in." });
  }
  return list;
}

export const remaining = () => steps().filter(s => !s.done).length;
export const dismissed = () => flag("openrds-start-dismissed");
export const hideStart = () => { setFlag("openrds-start-dismissed", true); window.dispatchEvent(new Event("openrds:start")); };

function progress(list) {
  const done = list.filter(s => s.done).length;
  const fill = h("span", { class: "progress-fill" });
  fill.style.width = `${Math.round(done / list.length * 100)}%`;
  return h("div", { class: "progress-row" }, h("span", { class: "progress", role: "progressbar", "aria-valuemin": "0", "aria-valuemax": String(list.length), "aria-valuenow": String(done), "aria-label": "Getting started progress" }, fill),
    h("span", { class: "muted" }, `${done} of ${list.length} done`));
}

function checklist(list, { compact = false } = {}) {
  const operate = can("operator");
  return h("ol", { class: `checklist${compact ? " compact" : ""}` }, list.map((s, i) => h("li", { class: s.done ? "done" : "" },
    h("span", { class: "check-mark", "aria-hidden": "true" }, s.done ? icon("check") : String(i + 1)),
    h("div", null, h("strong", null, s.title, s.done ? h("span", { class: "sr-only" }, " (done)") : null), !compact || !s.done ? h("p", { class: "muted" }, s.text) : null),
    !s.done && (operate || ["connect", "offsite", "account"].includes(s.id)) ? h("a", { class: "btn small", href: s.href }, s.action) : null)));
}

const CONCEPTS = [
  ["db", "Database", "A PostgreSQL, MySQL or Valkey server in its own container, with its own volume, port and password."],
  ["camera", "Snapshot", "A backup you take. It stays until you delete it."],
  ["history", "Automated backup", "A daily backup, removed after the retention period."],
  ["shield", "Deletion protection", "Blocks deleting a database until you turn it off."],
  ["sliders", "Engine parameters", "Tuning settings such as max_connections. Applied on restart."],
  ["terminal", "SSH tunnel", "Reach a private database from your computer."],
  ["key", "Roles", "Admins manage everything, operators manage databases, viewers can only look."],
];

export function startView() {
  const body = h("div", { class: "stack-gap" });
  function render() {
    const list = steps();
    body.replaceChildren(
      panel("Your checklist", { description: "Steps tick off as you go." },
        progress(list), checklist(list)),
      panel("Key ideas", {}, h("div", { class: "concepts" }, CONCEPTS.map(([symbol, title, text]) => h("div", { class: "concept" },
        h("span", { class: "empty-icon" }, icon(symbol)), h("div", null, h("h3", null, title), h("p", { class: "muted" }, text)))))),
      panel("Reaching the console", { description: "Nothing is public unless you turn on public access." },
        keyValues([["Console", "ssh -N -L 8080:127.0.0.1:8080 user@your-vps, then open http://127.0.0.1:8080"], ["A database", "See its Connectivity tab."]], "cols-2")));
    if (dismissed()) body.prepend(h("div", { class: "row" }, h("span", { class: "muted" }, "The dashboard card is hidden."), btn("Show it again", () => { setFlag("openrds-start-dismissed", false); window.dispatchEvent(new Event("openrds:start")); render(); }, "small")));
  }
  render();
  const stop = subscribe(render);
  return { el: h("div", null, h("div", { class: "page-head" }, h("div", null, h("h1", null, "Getting started"), h("p", { class: "muted" }, "From empty server to working database."))), body),
    title: "Getting started", nav: "start", crumbs: [["Getting started"]], destroy: stop };
}
