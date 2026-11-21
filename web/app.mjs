// Sign-in, the workspace shell and the hash router.
import { h, icon, api, onUnauthorized, setAlert, empty } from "./lib.mjs";
import { state, sync, startPolling, reset, subscribe, operations, loadEngines } from "./store.mjs";
import { dashboardView } from "./dashboard.mjs";
import { databasesView } from "./databases.mjs";
import { instanceView } from "./instance.mjs";
import { createView, modifyView, restoreView } from "./forms.mjs";
import { backupsView } from "./backups.mjs";
import { eventsView } from "./events.mjs";

const $ = (id) => document.getElementById(id);

// ---------- theme: light, dark, or follow the system

const THEME = "openrds-theme";
function applyTheme(choice) {
  if (!["light", "dark", "system"].includes(choice)) choice = "system";
  if (choice === "system") delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = choice;
  document.querySelectorAll("[data-theme-choice]").forEach(b => b.setAttribute("aria-pressed", String(b.dataset.themeChoice === choice)));
  try { localStorage.setItem(THEME, choice); } catch { /* private mode */ }
}
let savedTheme = "system";
try { savedTheme = localStorage.getItem(THEME) || "system"; } catch { /* private mode */ }
applyTheme(savedTheme);
document.querySelectorAll("[data-theme-choice]").forEach(b => b.addEventListener("click", () => applyTheme(b.dataset.themeChoice)));

// ---------- routes

const ROUTES = [
  [/^\/?$|^\/dashboard$/, () => dashboardView()],
  [/^\/databases$/, () => databasesView()],
  [/^\/databases\/new$/, () => createView()],
  [/^\/databases\/([a-z0-9-]+)\/modify$/, m => modifyView(m[1])],
  [/^\/databases\/([a-z0-9-]+)(?:\/([a-z]+))?$/, m => instanceView(m[1], m[2]), "instance"],
  [/^\/snapshots\/([a-f0-9]{32})\/restore$/, m => restoreView(m[1])],
  [/^\/snapshots$/, () => backupsView("manual")],
  [/^\/automated-backups$/, () => backupsView("automated")],
  [/^\/events$/, () => eventsView()],
];
const LEGACY = { overview: "/", databases: "/databases", backups: "/snapshots", activity: "/events" };

let view = null, routeID = 0;

function crumbs(list = []) {
  const nav = $("crumbs");
  nav.replaceChildren(h("ol", null, h("li", null, h("a", { href: "#/" }, "OpenRDS")),
    list.map(([label, href], i) => h("li", null, href && i < list.length - 1 ? h("a", { href }, label) : h("span", { "aria-current": "page" }, label)))));
}

async function route() {
  if (!state.signedIn) return;
  const path = location.hash.replace(/^#/, "");
  if (Object.hasOwn(LEGACY, path)) { location.replace(`#${LEGACY[path]}`); return; }
  const id = ++routeID;
  closeNav();
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
    link.lastElementChild.textContent = bad ? "Degraded" : "Connected";
    link.title = `Last checked ${new Date(state.syncedAt).toLocaleTimeString()}`;
    setAlert($("sync-error"), bad ? `Some data could not be refreshed, so older values are shown. ${state.problems.join(" · ")}` : "", "warn");
    if (state.host) $("host-label").lastElementChild.textContent = state.host.name || "Single VPS";
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

const menuToggle = $("menu-toggle");
function closeNav() { document.body.classList.remove("nav-open"); menuToggle.setAttribute("aria-expanded", "false"); }
menuToggle.addEventListener("click", () => {
  const open = !document.body.classList.contains("nav-open");
  document.body.classList.toggle("nav-open", open);
  menuToggle.setAttribute("aria-expanded", String(open));
});

// ---------- session

function showLogin(message = "") {
  state.signedIn = false;
  reset();
  view?.destroy?.(); view = null;
  document.querySelectorAll("dialog[open]").forEach(d => d.close());
  $("content").replaceChildren();
  $("app").hidden = $("boot").hidden = true;
  $("login").hidden = false;
  $("token").value = "";
  setAlert($("login-note"), message, "error");
  $("token").focus();
}
onUnauthorized(() => { if (state.signedIn) showLogin("Your session expired. Sign in again."); });

async function showWorkspace() {
  state.signedIn = true;
  $("app").hidden = false;
  $("boot").hidden = $("login").hidden = true;
  await Promise.allSettled([sync(), loadEngines()]);
  startPolling();
  await route();
}

$("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const submit = e.submitter;
  submit.disabled = true;
  setAlert($("login-note"), "Signing in…", "info");
  try {
    await api("session", { method: "POST", token: $("token").value.trim() });
    $("token").value = "";
    await showWorkspace();
  } catch (err) { setAlert($("login-note"), err.message, "error"); }
  finally { submit.disabled = false; }
});

$("logout").addEventListener("click", async () => {
  try { await api("session", { method: "DELETE" }); } catch { /* signed out locally either way */ }
  showLogin();
});

(async () => {
  try { await api("session"); await showWorkspace(); }
  catch (err) { showLogin(err.status === 401 ? "" : err.message); }
})();
