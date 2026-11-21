// Accounts and roles, for administrators.
import { h, icon, api, modal, menu, panel, empty, dataTable, when, ago, avatar, ROLES, codeBlock, alertBox, nextId, toast, chip } from "./lib.mjs";
import { state, subscribe, sync, can } from "./store.mjs";

const STATUS = { active: ["Active", "good"], invited: ["Invite pending", "info"], disabled: ["Disabled", "neutral"] };

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
      h("p", { class: "muted" }, `Send this link to ${u.name} privately. It works once and expires in 72 hours; they choose their own password.`),
      codeBlock(link, { label: "Copy link" }),
      alertBox(h("span", null, "The console only listens on the VPS, so they open the link through their own SSH tunnel, for example ",
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
        h("div", { class: "field" }, h("label", { class: "field-label", for: userField.id }, "Username"), userField, h("p", { class: "hint" }, "2–32 lowercase letters, digits, dots, hyphens or underscores."))),
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

function changeRole(u) {
  const roles = roleCards(u.role);
  modal({
    title: `Change ${u.name}’s role`, kicker: "Users", confirm: "Save role", wide: true, body: [roles],
    run: async (form) => {
      const role = form.querySelector("input[type=radio]:checked").value;
      if (role === u.role) return;
      await api(`users/${u.id}`, { method: "PATCH", body: { role } });
      toast(`${u.name} is now ${ROLES[role].label === "Admin" ? "an" : "a"} ${ROLES[role].label}.`, "success");
      await sync();
    },
  });
}

function rowActions(u) {
  const self = state.me?.id === u.id;
  const reason = self ? "This is you. Use Account settings." : "";
  return menu("", [
    { label: "Change role", icon: "key", disabled: self, reason, run: () => changeRole(u) },
    { label: u.status === "invited" ? "Issue a new invite link" : "Reset password", icon: "refresh", disabled: self, reason, run: () => modal({
      title: `Reset ${u.name}’s password?`, kicker: "Users", confirm: "Reset and create link",
      body: h("p", { class: "muted" }, `${u.name} is signed out everywhere and cannot sign in until they choose a new password from the link you send.`),
      run: async () => {
        const res = await api(`users/${u.id}/reset`, { method: "POST" });
        await sync();
        linkDialog(res.user, res.secret, true);
      },
    }) },
    { label: u.status === "disabled" ? "Enable" : "Disable", icon: u.status === "disabled" ? "play" : "stop", disabled: self, reason, run: () => modal({
      title: u.status === "disabled" ? `Enable ${u.name}?` : `Disable ${u.name}?`, kicker: "Users", confirm: u.status === "disabled" ? "Enable" : "Disable",
      body: h("p", { class: "muted" }, u.status === "disabled" ? "They can sign in again with their password." : "They are signed out at once and cannot sign in until you enable the account. Nothing else changes."),
      run: async () => { await api(`users/${u.id}`, { method: "PATCH", body: { disabled: u.status !== "disabled" } }); await sync(); },
    }) },
    "-",
    { label: "Delete", icon: "trash", danger: true, disabled: self, reason, run: () => modal({
      title: `Delete ${u.name}?`, kicker: "Users", confirm: "Delete user", tone: "danger", typed: u.username,
      body: h("p", { class: "muted" }, "The account is removed and its sessions end. Events keep the record of what it did."),
      run: async () => { await api(`users/${u.id}`, { method: "DELETE" }); toast(`${u.name} was deleted.`, "success"); await sync(); },
    }) },
  ], { cls: "small ghost icon-only", labelIcon: "more", ariaLabel: `Actions for ${u.name}`, chevron: false });
}

export function usersView() {
  if (!can("admin")) {
    return { el: empty("key", "Administrators only", "Ask an administrator if you need an account changed."), title: "Users", nav: "users", crumbs: [["Users"]] };
  }
  const list = h("div", { class: "flush" });
  const count = h("span", { class: "counter" });
  const el = h("div", { class: "stack-gap" },
    h("div", { class: "page-head" }, h("div", null, h("h1", null, "Users"), h("p", { class: "muted" }, "Who can sign in to OpenRDS, and what each person can do.")),
      h("div", { class: "page-actions" }, h("button", { type: "button", class: "btn primary", onclick: invite }, icon("plus"), "Invite user"))),
    state.me?.token ? alertBox("You are signed in with the admin token. It can repair any account, so keep it for automation and emergencies, and sign in with an account day to day.", "info") : null,
    h("section", { class: "panel" }, h("header", { class: "panel-head" }, h("div", null, h("h2", null, "Accounts", count))), list),
    panel("What each role can do", {}, h("div", { class: "table-wrap" }, h("table", { class: "matrix" },
      h("thead", null, h("tr", null, h("th", { scope: "col" }, "Task"), ...Object.values(ROLES).map(r => h("th", { scope: "col" }, r.label)))),
      h("tbody", null, [
        ["See databases, monitoring, logs and events", true, true, true],
        ["Reveal passwords and download backups", true, true, false],
        ["Create, modify, start, stop and delete databases", true, true, false],
        ["Take, restore and delete backups", true, true, false],
        ["Invite users and change roles", true, false, false],
      ].map(([task, ...allowed]) => h("tr", null, h("th", { scope: "row" }, task), ...allowed.map(ok => h("td", null, ok ? h("span", { class: "status good" }, icon("ok"), "Yes") : h("span", { class: "status neutral" }, icon("close"), "No"))))))))));

  function render() {
    const users = state.users;
    count.textContent = users ? String(users.length) : "—";
    if (!users) { list.replaceChildren(empty("key", "Reading accounts…", "")); return; }
    if (!users.length) { list.replaceChildren(empty("key", "No accounts yet", "Invite people so each has their own sign-in and role.", h("button", { type: "button", class: "btn primary", onclick: invite }, "Invite user"))); return; }
    list.replaceChildren(dataTable({ caption: "Accounts", rows: users, columns: [
      { key: "name", label: "User", value: u => h("div", { class: "db-cell" }, avatar(u.name), h("span", { class: "stack" }, h("strong", null, u.name, state.me?.id === u.id ? h("span", { class: "you" }, " (you)") : null), h("small", null, u.username))) },
      { key: "role", label: "Role", value: u => chip(ROLES[u.role]?.label || u.role, u.role === "admin" ? "info" : "neutral") },
      { key: "status", label: "Status", value: u => h("div", { class: "stack" }, chip(...STATUS[u.status]), u.status === "invited" && u.inviteExpiresAt ? h("small", null, `Link expires ${ago(u.inviteExpiresAt)}`) : null) },
      { key: "last", label: "Last sign-in", value: u => u.lastLoginAt ? h("span", { title: when(u.lastLoginAt) }, ago(u.lastLoginAt)) : h("span", { class: "muted" }, "Never") },
      { key: "created", label: "Created", value: u => h("span", { title: when(u.createdAt) }, ago(u.createdAt)) },
      { key: "actions", label: h("span", { class: "sr-only" }, "Actions"), value: rowActions },
    ] }));
  }
  render();
  const stop = subscribe(changed => { if (changed.has("users")) render(); });
  return { el, title: "Users", nav: "users", crumbs: [["Users"]], destroy: stop };
}
