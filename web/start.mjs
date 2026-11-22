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
      text: "Sign in with a username and password, then invite your team.", action: "Create account" });
  }
  list.push(
    { id: "create", title: "Create your first database", done: dbs.length > 0, href: "#/databases/new", action: "Create database",
      text: "Pick PostgreSQL, MySQL or Valkey and a template. OpenRDS creates the container, the volume and the credentials." },
    { id: "connect", title: "Connect an application", done: flag("openrds-connected"), href: first ? `#/databases/${first.name}` : "#/databases/new", action: "Open the guide",
      text: "Copy a ready-made snippet for your language from a database’s Connectivity tab." },
    { id: "backups", title: "Turn on automated backups", done: dbs.length > 0 && !unprotected, href: unprotected ? `#/databases/${unprotected.name}/modify` : "#/automated-backups", action: "Set a schedule",
      text: "Daily backups in a quiet hour, kept for the number of days you choose." },
    { id: "offsite", title: "Keep a copy off this VPS", done: flag("openrds-downloaded"), href: "#/snapshots", action: "View snapshots",
      text: "Backups are stored on this VPS. Download one to separate storage so you can recover if the server is lost." },
  );
  if (can("admin") && !state.me?.token) {
    list.push({ id: "team", title: "Invite your team", done: (state.users?.length || 0) > 1, href: "#/users", action: "Invite people",
      text: "Give each teammate their own sign-in and a role instead of sharing one password." });
  }
  return list;
}

export const remaining = () => steps().filter(s => !s.done).length;
export const dismissed = () => flag("openrds-start-dismissed");

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

// The dashboard card, until every step is done or it is dismissed.
export function startCard() {
  const list = steps();
  if (dismissed() || list.every(s => s.done)) return null;
  const close = h("button", { type: "button", class: "icon-btn ghost", "aria-label": "Hide getting started", title: "Hide. You can reopen it from the sidebar." }, icon("close"));
  const card = panel("Get started with OpenRDS", { actions: [h("a", { class: "btn small", href: "#/getting-started" }, "Open the guide"), close],
    description: "A few steps to a database your applications can rely on." }, progress(list), checklist(list, { compact: true }));
  close.addEventListener("click", () => { setFlag("openrds-start-dismissed", true); card.remove(); window.dispatchEvent(new Event("openrds:start")); });
  card.classList.add("start-card");
  return card;
}

const CONCEPTS = [
  ["db", "Database instance", "One PostgreSQL, MySQL or Valkey server running in its own container, with a private data volume, a port on this VPS and a generated password."],
  ["camera", "Snapshot", "A copy of one database that you take, or that is taken when you delete a database. It stays until you delete it."],
  ["history", "Automated backup", "A daily copy taken in the backup window and removed after the retention period. Keep one to turn it into a snapshot."],
  ["shield", "Deletion protection", "A switch that refuses to delete a database until it is turned off, so a slip cannot remove production data."],
  ["sliders", "Engine parameters", "Settings such as max_connections that tune the database. They apply when it restarts."],
  ["terminal", "SSH tunnel", "Databases listen only inside the VPS. From your computer, an SSH tunnel forwards a local port to the database securely."],
  ["key", "Roles", "Admins manage everything including accounts, operators manage databases, and viewers can look but not change or download."],
];

export function startView() {
  const body = h("div", { class: "stack-gap" });
  function render() {
    const list = steps();
    body.replaceChildren(
      panel("Your checklist", { description: "Steps tick themselves off as OpenRDS sees them done. Some are remembered in this browser." },
        progress(list), checklist(list)),
      panel("Key ideas", {}, h("div", { class: "concepts" }, CONCEPTS.map(([symbol, title, text]) => h("div", { class: "concept" },
        h("span", { class: "empty-icon" }, icon(symbol)), h("div", null, h("h3", null, title), h("p", { class: "muted" }, text)))))),
      panel("Reaching the console and databases", { description: "Everything listens on the VPS’s loopback interface, so nothing is exposed to the internet." },
        keyValues([["Console", "ssh -N -L 8080:127.0.0.1:8080 user@your-vps, then open http://127.0.0.1:8080"], ["A database", "Each database’s Connectivity tab shows its own tunnel command and snippets."]], "cols-2")));
    if (dismissed()) body.prepend(h("div", { class: "row" }, h("span", { class: "muted" }, "The dashboard card is hidden."), btn("Show it on the dashboard again", () => { setFlag("openrds-start-dismissed", false); window.dispatchEvent(new Event("openrds:start")); render(); }, "small")));
  }
  render();
  const stop = subscribe(render);
  return { el: h("div", null, h("div", { class: "page-head" }, h("div", null, h("h1", null, "Getting started"), h("p", { class: "muted" }, "From an empty VPS to a database your applications can rely on, step by step."))), body),
    title: "Getting started", nav: "start", crumbs: [["Getting started"]], destroy: stop };
}
