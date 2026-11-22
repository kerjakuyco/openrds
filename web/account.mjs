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
        h("div", { class: "page-head" }, h("div", null, h("h1", null, "Account"), h("p", { class: "muted" }, "You are signed in with the API key."))),
        alertBox("The API key is for scripts. Create an account to sign in with.", "info"),
        setup ? form("Create your administrator account", "Use it from now on.", [
          h("div", { class: "form-grid" }, h("div", { class: "field" }, h("label", { class: "field-label", for: name.id }, "Name"), name),
            h("div", { class: "field" }, h("label", { class: "field-label", for: username.id }, "Username"), username)), pw, confirm,
        ], "Create account", async () => {
          if (pw.input.value !== confirm.input.value) throw new Error("The passwords do not match.");
          const id = await api("setup", { method: "POST", body: { name: name.value, username: username.value, password: pw.input.value } });
          toast(`Welcome, ${id.name}.`, "success");
          onIdentity(id);
          location.hash = "#/getting-started";
        }) : panel("Accounts", {}, h("p", { class: "muted" }, "Accounts already exist. Sign out and sign in with one."), h("a", { class: "btn", href: "#/users" }, "Open Users")));
      return;
    }
    const name = h("input", { type: "text", id: nextId("n"), value: me.name, autocomplete: "name", maxlength: "64" });
    const current = passwordField("Current password", { autocomplete: "current-password" });
    const next = passwordField("New password", { hint: "At least 10 characters." });
    const again = passwordField("Confirm new password");
    const role = ROLES[me.role];
    el.replaceChildren(
      h("div", { class: "page-head" }, h("div", { class: "row" }, avatar(me.name, "large"), h("div", null, h("h1", null, me.name), h("p", { class: "muted" }, `${me.username} · ${role.label}`)))),
      h("div", { class: "tab-grid" },
        form("Profile", "Shown in the sidebar and events.", [h("div", { class: "field" }, h("label", { class: "field-label", for: name.id }, "Display name"), name)], "Save", async () => {
          const id = await api("account", { method: "PATCH", body: { name: name.value } });
          onIdentity(id);
          return "Saved.";
        }),
        panel("Your access", {}, keyValues([["Username", me.username], ["Role", role.label]], "cols-2"), h("p", { class: "muted" }, role.text),
          h("p", { class: "muted" }, "Only an admin can change it."))),
      h("div", { class: "tab-grid" },
        form("Password", "Signs you out everywhere else.", [current, next, again], "Change password", async () => {
          if (next.input.value !== again.input.value) throw new Error("The new passwords do not match.");
          await api("account/password", { method: "POST", body: { current: current.input.value, password: next.input.value } });
          [current, next, again].forEach(f => { f.input.value = ""; });
          return "Password changed.";
        }),
        panel("Sessions", { description: "Sessions last 12 hours." }, h("p", { class: "muted" }, "Sign out everywhere except here."),
          h("div", { class: "row" }, h("button", { type: "button", class: "btn", onclick: () => modal({
            title: "Sign out everywhere else?", kicker: "Sessions", confirm: "Sign out other sessions",
            body: h("p", { class: "muted" }, "Other sessions are signed out."),
            run: async () => { await api("account/signout", { method: "POST" }); toast("Other sessions were signed out.", "success"); },
          }) }, icon("exit"), "Sign out other sessions")))));
  }
  render();
  const stop = subscribe(changed => { if (changed.has("users") || changed.has("me")) render(); });
  return { el, title: "Account", nav: "account", crumbs: [["Account"]], destroy: stop };
}
