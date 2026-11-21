// Sign-in, the workspace shell and the hash router.
import { h, icon, api, onUnauthorized, setAlert, empty } from "./lib.mjs";
import { state, sync, startPolling, reset, subscribe, operations, loadEngines, setMe, can } from "./store.mjs";
import { dashboardView } from "./dashboard.mjs";
import { databasesView } from "./databases.mjs";
import { instanceView } from "./instance.mjs";
import { createView, modifyView, restoreView } from "./forms.mjs";
import { backupsView } from "./backups.mjs";
import { eventsView } from "./events.mjs";
import { usersView } from "./users.mjs";
import { accountView } from "./account.mjs";
import { startView } from "./start.mjs";
import { renderAuth } from "./auth.mjs";
import { initNav, markEventsSeen } from "./nav.mjs";

const $ = (id) => document.getElementById(id);

// ---------- theme: light, dark, or follow the system

const THEME = "openrds-theme";
let theme = "system";
try { theme = localStorage.getItem(THEME) || "system"; } catch { /* private mode */ }
function setTheme(choice) {
  theme = ["light", "dark", "system"].includes(choice) ? choice : "system";
  if (theme === "system") delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = theme;
  try { localStorage.setItem(THEME, theme); } catch { /* private mode */ }
}
setTheme(theme);

// ---------- routes

const operatorOnly = (make) => (m) => can("operator") ? make(m)
  : { el: empty("key", "This needs the Operator role", "Viewers can look at databases but not change them. Ask an administrator for access.", h("a", { class: "btn", href: "#/databases" }, "Back to databases")), title: "Not allowed", nav: "databases", crumbs: [["Not allowed"]] };

const ROUTES = [
  [/^\/?$|^\/dashboard$/, () => dashboardView()],
  [/^\/getting-started$/, () => startView()],
  [/^\/databases$/, () => databasesView()],
  [/^\/databases\/new$/, operatorOnly(() => createView())],
  [/^\/databases\/([a-z0-9-]+)\/modify$/, operatorOnly(m => modifyView(m[1]))],
  [/^\/databases\/([a-z0-9-]+)(?:\/([a-z]+))?$/, m => instanceView(m[1], m[2]), "instance"],
  [/^\/snapshots\/([a-f0-9]{32})\/restore$/, operatorOnly(m => restoreView(m[1]))],
  [/^\/snapshots$/, () => backupsView("manual")],
  [/^\/automated-backups$/, () => backupsView("automated")],
  [/^\/events$/, () => { markEventsSeen(); return eventsView(); }],
  [/^\/users$/, () => usersView()],
  [/^\/account$/, () => accountView({ onIdentity })],
];
const LEGACY = { overview: "/", databases: "/databases", backups: "/snapshots", activity: "/events" };

let view = null, routeID = 0;

function crumbs(list = []) {
  $("crumbs").replaceChildren(h("ol", null, h("li", null, h("a", { href: "#/" }, "OpenRDS")),
    list.map(([label, href], i) => h("li", null, href && i < list.length - 1 ? h("a", { href }, label) : h("span", { "aria-current": "page" }, label)))));
}

async function route() {
  const path = location.hash.replace(/^#/, "");
  if (path.startsWith("/invite/")) { showAuth("invite", { secret: path.slice(8) }); return; }
  if (!state.signedIn) return;
  if (Object.hasOwn(LEGACY, path)) { location.replace(`#${LEGACY[path]}`); return; }
  const id = ++routeID;
  for (const [pattern, make, kind] of ROUTES) {
    const m = pattern.exec(path);
    if (!m) continue;
    if (kind === "instance" && view?.route?.(m[1], m[2])) return; // another tab of the same database
    view?.destroy?.();
    view = null;
    let next;
    try { next = await make(m); }
    catch (err) { next = { el: empty("alert", "This page could not load", err.message), title: "Error", crumbs: [["Error"]] }; }
    if (id !== routeID) { next.destroy?.(); return; }
    view = next;
    $("content").replaceChildren(view.el);
    document.title = `${view.title} · OpenRDS`;
    crumbs(view.crumbs);
    document.querySelectorAll("[data-nav]").forEach(a => a.dataset.nav === view.nav ? a.setAttribute("aria-current", "page") : a.removeAttribute("aria-current"));
    $("content").focus({ preventScroll: true });
    window.scrollTo(0, 0);
    return;
  }
  view?.destroy?.();
  view = { el: empty("search", "Page not found", "Check the address, or start from the dashboard.", h("a", { class: "btn", href: "#/" }, "Go to the dashboard")), title: "Not found" };
  $("content").replaceChildren(view.el);
  crumbs([["Not found"]]);
}
window.addEventListener("hashchange", route);

// ---------- shell status

subscribe((changed) => {
  if (changed.has("problems") || changed.has("instances") || changed.has("host")) {
    const bad = state.problems.length > 0;
    const link = $("link-state");
    link.className = `link-state ${bad ? "warn" : "good"}`;
    $("host-name").textContent = bad ? "Degraded" : (state.host?.name || "Connected");
    link.title = `${bad ? "Some data could not be refreshed" : "Connected to the Docker host"} · last checked ${new Date(state.syncedAt).toLocaleTimeString()}`;
    setAlert($("sync-error"), bad ? `Some data could not be refreshed, so older values are shown. ${state.problems.join(" · ")}` : "", "warn");
  }
  if (changed.has("operations")) {
    const ops = $("ops");
    ops.hidden = operations.size === 0;
    ops.title = [...operations.values()].join("\n");
    ops.replaceChildren(icon("spinner", "icon spin"), `${operations.size} in progress`);
  }
  if (view?.update && !changed.has("operations")) view.update(changed);
});

$("refresh").addEventListener("click", async () => {
  const b = $("refresh");
  b.disabled = true;
  try { await sync(); } finally { b.disabled = false; }
});

document.addEventListener("keydown", (e) => {
  if (e.key === "/" && state.signedIn && !document.querySelector("dialog[open]") && !e.target.closest("input,select,textarea,[contenteditable]")) {
    const search = $("search");
    if (search) { e.preventDefault(); search.focus(); }
  }
});
document.querySelector(".skip").addEventListener("click", (e) => { e.preventDefault(); $("content").focus(); });

// ---------- session

const nav = initNav({ onSignOut: signOut, theme: () => theme, setTheme });

function onIdentity(id) {
  setMe(id);
  nav.render();
  sync();
}

async function showAuth(mode, extra = {}) {
  state.signedIn = false;
  reset();
  nav.reset();
  view?.destroy?.(); view = null;
  document.querySelectorAll("dialog[open]").forEach(d => d.close());
  $("content").replaceChildren();
  $("app").hidden = $("boot").hidden = true;
  $("login").hidden = false;
  await renderAuth($("auth-panel"), { mode, ...extra, done: showWorkspace });
}

async function showLogin(message = "") {
  let setup = false;
  try { setup = (await api("status")).setup; } catch { /* show sign-in */ }
  showAuth(setup ? "setup" : "login", { message });
}
onUnauthorized(() => { if (state.signedIn) showLogin("Your session ended. Sign in again."); });

async function showWorkspace(id) {
  setMe(id);
  state.signedIn = true;
  $("auth-panel").replaceChildren(); // drop the sign-in forms, including any typed token
  $("app").hidden = false;
  $("boot").hidden = $("login").hidden = true;
  await Promise.allSettled([sync(), loadEngines()]);
  nav.render();
  startPolling();
  if (location.hash.startsWith("#/invite/")) location.hash = "#/";
  await route();
}

async function signOut() {
  try { await api("session", { method: "DELETE" }); } catch { /* signed out locally either way */ }
  showLogin();
}

(async () => {
  if (location.hash.startsWith("#/invite/")) { route(); return; }
  try { await showWorkspace(await api("session")); }
  catch (err) { if (err.status === 401) showLogin(); else showAuth("login", { message: err.message }); }
})();
