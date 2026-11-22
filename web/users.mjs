// Accounts and roles, for administrators.
import { h, icon, api, modal, menu, empty, dataTable, when, ago, avatar, ROLES, codeBlock, alertBox, nextId, toast, help } from "./lib.mjs";
import { state, subscribe, sync, can } from "./store.mjs";

function roleCards(selected) {
  const name = nextId("role");
  return h("div", { class: "cards cols-3", role: "radiogroup", "aria-label": "Role" }, Object.entries(ROLES).map(([key, r]) => {
    const input = h("input", { type: "radio", name, value: key, checked: key === selected, id: nextId("role") });
    return h("label", { class: "card-choice", for: input.id }, input, h("span", { class: "card-body" }, h("strong", null, r.label), h("small", null, r.text)));
  }));
}

export function linkDialog(u, secret, reset) {
  const link = `${location.origin}/#/invite/${secret}`;
  modal({
    title: reset ? `New sign-in link for ${u.name}` : `${u.name} is invited`, kicker: reset ? "Password reset" : "Invitation", confirm: "Done", cancel: false, wide: true,
    body: [
      h("p", { class: "muted" }, `Send this to ${u.name} privately. It works once, for 72 hours.`),
      codeBlock(link, { label: "Copy link" }),
      alertBox(h("span", null, "They open it through an SSH tunnel, for example ",
        h("code", null, `ssh -N -L ${location.port || 80}:127.0.0.1:${location.port || 80} user@your-vps`), "."), "info"),
    ],
    run: async () => {},
  });
}

function invite() {
  const nameField = h("input", { type: "text", id: nextId("name"), autocomplete: "off", maxlength: "64", placeholder: "Grace Hopper" });
  const userField = h("input", { type: "text", id: nextId("user"), autocomplete: "off", spellcheck: "false", maxlength: "32", placeholder: "grace", required: true });
  const roles = roleCards("operator");
  nameField.addEventListener("input", () => {
    if (!userField.dataset.touched) userField.value = nameField.value.trim().toLowerCase().split(/\s+/)[0]?.replace(/[^a-z0-9._-]/g, "") || "";
  });
  userField.addEventListener("input", () => { userField.dataset.touched = "1"; });
  modal({
    title: "Invite a user", kicker: "Users", confirm: "Create invite link", wide: true,
    body: [
      h("div", { class: "form-grid" },
        h("div", { class: "field" }, h("label", { class: "field-label", for: nameField.id }, "Name"), nameField),
        h("div", { class: "field" }, h("label", { class: "field-label", for: userField.id }, "Username"), userField, h("p", { class: "hint" }, "Lowercase letters, digits, dots, dashes or underscores."))),
      h("p", { class: "field-label" }, "Role"), roles,
    ],
    run: async (form) => {
      const role = form.querySelector("input[type=radio]:checked").value;
      const res = await api("users", { method: "POST", body: { name: nameField.value, username: userField.value, role } });
      await sync();
      linkDialog(res.user, res.secret, false);
    },
  });
}

// The role, changed right in the list. Your own role is shown, not edited.
function roleCell(u) {
  if (state.me?.id === u.id) return h("span", { class: `chip ${u.role === "admin" ? "info" : "neutral"}` }, ROLES[u.role]?.label || u.role);
  const select = h("select", { class: "role-select", "aria-label": `Role for ${u.name}` },
    Object.entries(ROLES).map(([key, r]) => h("option", { value: key, selected: key === u.role }, r.label)));
  select.addEventListener("change", async () => {
    const role = select.value;
    select.disabled = true;
    try {
      await api(`users/${u.id}`, { method: "PATCH", body: { role } });
      toast(`${u.name} is now ${role === "admin" ? "an" : "a"} ${ROLES[role].label}.`, "success");
      await sync();
    } catch (err) { select.value = u.role; toast(err.message, "error"); }
    finally { select.disabled = false; }
  });
  return select;
}

function statusCell(u) {
  if (u.status === "invited") {
    return h("div", { class: "stack" }, h("span", { class: "status info" }, icon("pending"), "Invite pending"),
      u.inviteExpiresAt ? h("small", null, `Expires ${ago(u.inviteExpiresAt)}`) : null);
  }
  return u.status === "disabled" ? h("span", { class: "status neutral" }, icon("stopped"), "Disabled") : h("span", { class: "status good" }, icon("ok"), "Active");
}

function resetLink(u) {
  const invited = u.status === "invited";
  modal({
    title: invited ? `Send ${u.name} a new invite?` : `Reset ${u.name}’s password?`, kicker: "Users", confirm: invited ? "Create new link" : "Reset and create link",
    body: h("p", { class: "muted" }, invited ? "The old link stops working." : `${u.name} is signed out until they set a new password.`),
    run: async () => {
      const res = await api(`users/${u.id}/reset`, { method: "POST" });
      await sync();
      linkDialog(res.user, res.secret, !invited);
    },
  });
}

function rowActions(u) {
  if (state.me?.id === u.id) return h("a", { class: "btn small ghost", href: "#/account" }, "Your account");
  const disabled = u.status === "disabled";
  return h("div", { class: "cell-actions" },
    u.status === "invited" ? h("button", { type: "button", class: "btn small", onclick: () => resetLink(u) }, icon("refresh"), "Resend invite") : null,
    menu("", [
      u.status !== "invited" ? { label: "Reset password", icon: "key", run: () => resetLink(u) } : null,
      { label: disabled ? "Enable" : "Disable", icon: disabled ? "play" : "stop", run: () => modal({
        title: disabled ? `Enable ${u.name}?` : `Disable ${u.name}?`, kicker: "Users", confirm: disabled ? "Enable" : "Disable",
        body: h("p", { class: "muted" }, disabled ? "They can sign in again." : "They’re signed out until you enable the account."),
        run: async () => { await api(`users/${u.id}`, { method: "PATCH", body: { disabled: !disabled } }); await sync(); },
      }) },
      "-",
      { label: "Delete user", icon: "trash", danger: true, run: () => modal({
        title: `Delete ${u.name}?`, kicker: "Users", confirm: "Delete user", tone: "danger", typed: u.username,
        body: h("p", { class: "muted" }, "The account and its sessions are removed. Events are kept."),
        run: async () => { await api(`users/${u.id}`, { method: "DELETE" }); toast(`${u.name} was deleted.`, "success"); await sync(); },
      }) },
    ].filter(Boolean), { cls: "small ghost icon-only", labelIcon: "more", ariaLabel: `More actions for ${u.name}`, chevron: false }));
}

const FILTERS = [["all", "All"], ["active", "Active"], ["invited", "Invited"], ["disabled", "Disabled"]];

export function usersView() {
  if (!can("admin")) {
    return { el: empty("key", "Administrators only", "Ask an admin to change accounts."), title: "Users", nav: "users", crumbs: [["Users"]] };
  }
  const ui = { filter: "all", query: "" };
  const list = h("div", { class: "flush" });
  const search = h("input", { type: "search", class: "user-search", placeholder: "Search users", "aria-label": "Search users" });
  search.addEventListener("input", () => { ui.query = search.value.trim().toLowerCase(); render(); });
  const filters = h("div", { class: "segmented", role: "group", "aria-label": "Show" });
  const roleHelp = help(Object.values(ROLES).map(r => `${r.label}: ${r.text}`).join(" "), "About roles");
  const el = h("div", { class: "stack-gap" },
    h("div", { class: "page-head" }, h("div", null, h("h1", null, "Users"), h("p", { class: "muted" }, "Who can sign in to OpenRDS.")),
      h("div", { class: "page-actions" }, h("button", { type: "button", class: "btn primary", onclick: invite }, icon("plus"), "Invite user"))),
    state.me?.token ? alertBox("You’re using the API key, which is for scripts. Sign in with an account instead.", "info") : null,
    h("section", { class: "panel" },
      h("header", { class: "panel-head users-head" }, filters, search),
      list));

  function render() {
    const users = state.users;
    if (!users) { list.replaceChildren(empty("key", "Loading accounts…", "")); return; }
    const count = (f) => f === "all" ? users.length : users.filter(u => u.status === f).length;
    filters.replaceChildren(...FILTERS.map(([key, label]) => h("button", { type: "button", "aria-pressed": String(ui.filter === key),
      onclick: () => { ui.filter = key; render(); } }, label, h("span", { class: "seg-count" }, String(count(key))))));
    if (!users.length) {
      list.replaceChildren(empty("users", "No accounts yet", "Invite people to give each their own sign-in.", h("button", { type: "button", class: "btn primary", onclick: invite }, "Invite user")));
      return;
    }
    const shown = users.filter(u => (ui.filter === "all" || u.status === ui.filter)
      && (!ui.query || u.name.toLowerCase().includes(ui.query) || u.username.includes(ui.query)));
    list.replaceChildren(shown.length ? dataTable({ caption: "Accounts", rows: shown, columns: [
      { key: "name", label: "User", value: u => h("div", { class: "db-cell" }, avatar(u.name), h("span", { class: "stack" },
        h("strong", null, u.name, state.me?.id === u.id ? h("span", { class: "chip neutral you-chip" }, "You") : null), h("small", null, u.username))) },
      { key: "role", label: h("span", { class: "th-help" }, "Role", roleHelp), value: roleCell },
      { key: "status", label: "Status", value: statusCell },
      { key: "last", label: "Last sign-in", value: u => u.lastLoginAt ? h("span", { title: when(u.lastLoginAt) }, ago(u.lastLoginAt)) : h("span", { class: "muted" }, "Never") },
      { key: "actions", label: h("span", { class: "sr-only" }, "Actions"), cls: "actions", value: rowActions },
    ] }) : empty("search", "No users match", "Try another filter or search."));
  }
  render();
  const stop = subscribe(changed => { if (changed.has("users")) render(); });
  return { el, title: "Users", nav: "users", crumbs: [["Users"]], destroy: stop };
}
