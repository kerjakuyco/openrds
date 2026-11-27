// The sidebar: navigation with live counts, quick links to databases, the
// account menu, a collapsible rail on desktop and a drawer on small screens.
import { h, icon, menu, avatar, ROLES, statusOf } from "./lib.mjs";
import { state, can, subscribe } from "./store.mjs";
import { remaining, dismissed } from "./start.mjs";

const $ = (id) => document.getElementById(id);
const QUICK = 6;
const read = (key) => { try { return localStorage.getItem(key); } catch { return null; } };
const write = (key, value) => { try { localStorage.setItem(key, value); } catch { /* private mode */ } };

export const markEventsSeen = () => write("openrds-events-seen", new Date().toISOString());

function newFailures() {
  const seen = Date.parse(read("openrds-events-seen") || "") || Date.now() - 86400000;
  return (state.activity || []).filter(e => e.result === "failed" && Date.parse(e.at) > seen).length;
}

function badge(el, value, label) {
  el.hidden = !value;
  el.textContent = value ? String(value) : "";
  el.title = value ? label : "";
}

export function initNav({ onSignOut, theme, setTheme }) {
  const shell = $("app");
  const toggle = $("collapse");
  function collapse(on) {
    shell.classList.toggle("collapsed", on);
    toggle.setAttribute("aria-pressed", String(on));
    toggle.setAttribute("aria-label", on ? "Expand the sidebar" : "Collapse the sidebar");
    write("openrds-collapsed", on ? "1" : "0");
  }
  collapse(read("openrds-collapsed") === "1");
  toggle.addEventListener("click", () => collapse(!shell.classList.contains("collapsed")));

  const opener = $("menu-toggle");
  function drawer(open) {
    document.body.classList.toggle("nav-open", open);
    opener.setAttribute("aria-expanded", String(open));
    $("scrim").hidden = !open;
    if (open) $("nav").querySelector("a")?.focus();
  }
  opener.addEventListener("click", () => drawer(!document.body.classList.contains("nav-open")));
  $("nav-close").addEventListener("click", () => { drawer(false); opener.focus(); });
  $("scrim").addEventListener("click", () => drawer(false));
  document.addEventListener("keydown", (e) => { if (e.key === "Escape" && document.body.classList.contains("nav-open")) { drawer(false); opener.focus(); } });

  let accountSig = "";
  function render() {
    if (!state.me) return;
    document.querySelectorAll(".admin-only").forEach(el => { el.hidden = !can("admin"); });
    $("nav-create").hidden = !can("operator");
    const dbs = [...(state.instances || [])].sort((a, b) => a.name.localeCompare(b.name));
    $("db-count").textContent = state.instances ? String(dbs.length) : "";
    const down = (state.servers || []).filter(s => !s.reachable).length;
    $("server-count").textContent = (state.servers || []).length > 1 ? String(state.servers.length) : "";
    $("server-count").classList.toggle("warn", down > 0);
    $("server-count").title = down ? `${down} unreachable` : "";
    const current = /^#\/databases\/([a-z0-9-]+)/.exec(location.hash)?.[1];
    $("nav-dbs").replaceChildren(...dbs.slice(0, QUICK).map(d => {
      const st = statusOf(d);
      return h("a", { href: `#/databases/${d.name}`, class: "db-quick", title: `${d.name} · ${st.label}`, "aria-current": d.name === current ? "page" : null },
        h("span", { class: `dot ${st.tone}`, "aria-hidden": "true" }), h("span", { class: "label" }, d.name), h("span", { class: "sr-only" }, `, ${st.label}`));
    }), dbs.length > QUICK ? h("a", { href: "#/databases", class: "db-quick more" }, h("span", { class: "label" }, `${dbs.length - QUICK} more…`)) : "");
    badge($("events-badge"), newFailures(), "New failed operations");
    badge($("start-badge"), !dismissed() && state.instances ? remaining() : 0, "Steps left in getting started");
    const me = state.me, chosen = theme();
    const sig = JSON.stringify([me, chosen]);
    if (sig === accountSig) return;
    accountSig = sig;
    const trigger = h("button", { type: "button", class: "account-btn", title: `${me.name} · ${ROLES[me.role]?.label}` },
      avatar(me.name), h("span", { class: "label account-text" }, h("strong", null, me.name), h("small", null, me.token ? "API key" : ROLES[me.role]?.label)), icon("chevron", "icon chevron label"));
    const profile = h("div", { class: "menu-profile", role: "presentation" }, avatar(me.name),
      h("span", { class: "stack" }, h("strong", null, me.name), h("small", null, me.token ? "Signed in with the API key" : me.username)),
      me.token ? null : h("span", { class: `chip ${me.role === "admin" ? "info" : "neutral"}` }, ROLES[me.role]?.label));
    // Light, auto or dark in one row; choosing one closes the menu like any item.
    const themes = h("div", { class: "menu-theme", role: "group", "aria-label": "Theme" },
      [["light", "Light", "sun", "Light"], ["system", "Match system", "monitor", "Auto"], ["dark", "Dark", "moon", "Dark"]].map(([value, label, symbol, short]) => {
        const b = h("button", { type: "button", role: "menuitemradio", tabindex: "-1", "aria-checked": String(chosen === value), "aria-label": label, title: label }, icon(symbol), h("span", null, short));
        b.addEventListener("click", () => { setTheme(value); accountSig = ""; render(); $("account").querySelector("button")?.focus(); });
        return b;
      }));
    $("account").replaceChildren(menu("", [
      { element: profile },
      "-",
      { label: "Account settings", icon: "key", run: () => { location.hash = "#/account"; } },
      can("admin") && { label: "Users", icon: "users", run: () => { location.hash = "#/users"; } },
      can("admin") && { label: "Notifications", icon: "alert", run: () => { location.hash = "#/notifications"; } },
      "-",
      { element: h("div", { class: "menu-theme-row", role: "presentation" }, h("span", { class: "menu-heading" }, "Theme"), themes) },
      "-",
      { label: "Sign out", icon: "exit", run: onSignOut },
    ], { trigger, up: true, align: "start", ariaLabel: `Account menu for ${me.name}` }));
  }
  subscribe(render);
  window.addEventListener("openrds:start", render);
  window.addEventListener("hashchange", () => { drawer(false); render(); });
  return { render, reset: () => { accountSig = ""; $("account").replaceChildren(); $("nav-dbs").replaceChildren(); } };
}
