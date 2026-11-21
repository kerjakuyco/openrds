// The signed-in person's own account.
import { h, icon, api, panel, keyValues, alertBox, setAlert, toast, avatar, ROLES, nextId, modal } from "./lib.mjs";
import { state, subscribe } from "./store.mjs";

export function passwordField(label, { autocomplete = "new-password", hint = "" } = {}) {
  const input = h("input", { type: "password", id: nextId("pw"), autocomplete, required: true, maxlength: "128" });
  const toggle = h("button", { type: "button", class: "icon-btn ghost reveal", "aria-label": `Show ${label.toLowerCase()}`, "aria-pressed": "false" }, icon("eye"));
  toggle.addEventListener("click", () => {
    const show = input.type === "password";
    input.type = show ? "text" : "password";
    toggle.setAttribute("aria-pressed", String(show));
    toggle.setAttribute("aria-label", `${show ? "Hide" : "Show"} ${label.toLowerCase()}`);
  });
  const field = h("div", { class: "field" }, h("label", { class: "field-label", for: input.id }, label), h("div", { class: "password" }, input, toggle), hint && h("p", { class: "hint" }, hint));
  field.input = input;
  return field;
}

function form(title, description, fields, submitLabel, run) {
  const note = h("div", { class: "alert", role: "status", hidden: true });
  const submit = h("button", { type: "submit", class: "btn primary" }, submitLabel);
  const f = h("form", { class: "stack-form", novalidate: true }, ...fields, note, h("div", { class: "row" }, submit));
  f.addEventListener("submit", async (e) => {
    e.preventDefault();
    submit.disabled = true; setAlert(note, "");
    try { const message = await run(); if (message) setAlert(note, message, "success"); }
    catch (err) { setAlert(note, err.message, "error"); }
    finally { submit.disabled = false; }
  });
  return panel(title, { description }, f);
}

export function accountView({ onIdentity }) {
  const el = h("div", { class: "stack-gap" });
  function render() {
    const me = state.me;
    if (!me) return;
    if (me.token) {
      const setup = state.users && !state.users.length;
      const name = h("input", { type: "text", id: nextId("n"), autocomplete: "name", maxlength: "64" });
      const username = h("input", { type: "text", id: nextId("u"), autocomplete: "username", spellcheck: "false", maxlength: "32", required: true });
      const pw = passwordField("Password", { hint: "At least 10 characters." }), confirm = passwordField("Confirm password");
      el.replaceChildren(
        h("div", { class: "page-head" }, h("div", null, h("h1", null, "Account"), h("p", { class: "muted" }, "You are signed in with the admin token."))),
        alertBox("The admin token has every permission and no account of its own. Keep it for automation and emergencies, such as resetting a forgotten password.", "info"),
        setup ? form("Create your administrator account", "Sign in with this account from now on, and invite your team from Users.", [
          h("div", { class: "form-grid" }, h("div", { class: "field" }, h("label", { class: "field-label", for: name.id }, "Name"), name),
            h("div", { class: "field" }, h("label", { class: "field-label", for: username.id }, "Username"), username)), pw, confirm,
        ], "Create account", async () => {
          if (pw.input.value !== confirm.input.value) throw new Error("The passwords do not match.");
          const id = await api("setup", { method: "POST", body: { name: name.value, username: username.value, password: pw.input.value } });
          toast(`Welcome, ${id.name}. You are signed in with your new account.`, "success");
          onIdentity(id);
          location.hash = "#/getting-started";
        }) : panel("Accounts", {}, h("p", { class: "muted" }, "Accounts already exist. Manage them from Users, or sign out and sign in with one."), h("a", { class: "btn", href: "#/users" }, "Open Users")));
      return;
    }
    const name = h("input", { type: "text", id: nextId("n"), value: me.name, autocomplete: "name", maxlength: "64" });
    const current = passwordField("Current password", { autocomplete: "current-password" });
    const next = passwordField("New password", { hint: "At least 10 characters, and not your username." });
    const again = passwordField("Confirm new password");
    const role = ROLES[me.role];
    el.replaceChildren(
      h("div", { class: "page-head" }, h("div", { class: "row" }, avatar(me.name, "large"), h("div", null, h("h1", null, me.name), h("p", { class: "muted" }, `${me.username} · ${role.label}`)))),
      h("div", { class: "tab-grid" },
        form("Profile", "Your name appears in the sidebar and in events.", [h("div", { class: "field" }, h("label", { class: "field-label", for: name.id }, "Display name"), name)], "Save", async () => {
          const id = await api("account", { method: "PATCH", body: { name: name.value } });
          onIdentity(id);
          return "Saved.";
        }),
        panel("Your access", {}, keyValues([["Username", me.username], ["Role", role.label]], "cols-2"), h("p", { class: "muted" }, role.text),
          h("p", { class: "muted" }, "Only an administrator can change your role."))),
      h("div", { class: "tab-grid" },
        form("Password", "Changing it signs you out everywhere else.", [current, next, again], "Change password", async () => {
          if (next.input.value !== again.input.value) throw new Error("The new passwords do not match.");
          await api("account/password", { method: "POST", body: { current: current.input.value, password: next.input.value } });
          [current, next, again].forEach(f => { f.input.value = ""; });
          return "Password changed. Other sessions were signed out.";
        }),
        panel("Sessions", { description: "A session lasts 12 hours in each browser." }, h("p", { class: "muted" }, "Lost a laptop, or signed in on a shared computer? End every session except this one."),
          h("div", { class: "row" }, h("button", { type: "button", class: "btn", onclick: () => modal({
            title: "Sign out everywhere else?", kicker: "Sessions", confirm: "Sign out other sessions",
            body: h("p", { class: "muted" }, "Every other browser signed in to your account is signed out. This one stays signed in."),
            run: async () => { await api("account/signout", { method: "POST" }); toast("Other sessions were signed out.", "success"); },
          }) }, icon("exit"), "Sign out other sessions")))));
  }
  render();
  const stop = subscribe(changed => { if (changed.has("users") || changed.has("me")) render(); });
  return { el, title: "Account", nav: "account", crumbs: [["Account"]], destroy: stop };
}
