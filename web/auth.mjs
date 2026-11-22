// The screens before sign-in: first-run setup, sign-in and invitations.
import { h, icon, api, setAlert, nextId } from "./lib.mjs";
import { passwordField } from "./account.mjs";

function card(kicker, title, description, form, footer = []) {
  return [h("p", { class: "kicker" }, kicker), h("h1", null, title), h("p", { class: "muted" }, description), form, ...footer];
}

function field(label, input, hint = "") {
  input.id ||= nextId("auth");
  return h("div", { class: "field" }, h("label", { class: "field-label", for: input.id }, label), input, hint && h("p", { class: "hint" }, hint));
}

function authForm(fields, submitLabel, run) {
  const note = h("div", { class: "alert", role: "status", id: "login-note", hidden: true });
  const submit = h("button", { type: "submit", class: "btn primary wide" }, submitLabel, icon("arrow"));
  const form = h("form", { class: "auth-form", novalidate: true }, ...fields, submit, note);
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    submit.disabled = true;
    setAlert(note, "One moment…", "info");
    try { await run(); }
    catch (err) { setAlert(note, err.message, "error"); }
    finally { submit.disabled = false; }
  });
  return form;
}

const forgotten = () => h("details", { class: "disclosure" }, h("summary", null, "Forgot your password?"),
  h("p", null, "Ask an administrator to send you a reset link from Users."),
  h("p", null, "If you are the only administrator, run this on the server, in the OpenRDS folder, and open the link it prints:"),
  h("pre", null, "docker compose exec openrds openrds reset-password YOUR_USERNAME"));

// mode: setup, login or invite. done() receives the new identity.
export async function renderAuth(target, { mode, message = "", secret = "", done }) {
  let content;
  if (mode === "invite") {
    let info;
    try { info = await api(`invites/${secret}`); }
    catch (err) {
      const back = h("button", { type: "button", class: "btn wide", onclick: () => { location.hash = "#/"; location.reload(); } }, "Go to sign-in");
      target.replaceChildren(...card("Invitation", "This link does not work", err.message, back));
      return;
    }
    const pw = passwordField("Choose a password", { hint: "At least 10 characters, and not your username." }), again = passwordField("Confirm password");
    content = card(info.reset ? "Password reset" : "Invitation", info.reset ? `Choose a new password, ${info.name}` : `Welcome, ${info.name}`,
      `Your username is ${info.username}. Choose a password to finish${info.reset ? "" : " setting up your account"}.`,
      authForm([pw, again], info.reset ? "Save password" : "Create my account", async () => {
        if (pw.input.value !== again.input.value) throw new Error("The passwords do not match.");
        const id = await api(`invites/${secret}`, { method: "POST", body: { password: pw.input.value } });
        location.hash = "#/";
        done(id);
      }));
  } else if (mode === "setup") {
    const name = h("input", { type: "text", autocomplete: "name", maxlength: "64", placeholder: "Ada Lovelace" });
    const username = h("input", { type: "text", autocomplete: "username", spellcheck: "false", maxlength: "32", required: true, placeholder: "ada" });
    const pw = passwordField("Password", { hint: "At least 10 characters." }), again = passwordField("Confirm password");
    content = card("Welcome to OpenRDS", "Create your administrator account", "This is the account you sign in with. You can invite your team afterwards.",
      authForm([h("div", { class: "form-grid" }, field("Your name", name), field("Username", username, "Lowercase letters and digits.")), pw, again], "Create account and continue", async () => {
        if (pw.input.value !== again.input.value) throw new Error("The passwords do not match.");
        done(await api("setup", { method: "POST", body: { name: name.value, username: username.value, password: pw.input.value } }));
      }));
  } else {
    const username = h("input", { type: "text", autocomplete: "username", spellcheck: "false", required: true });
    const pw = passwordField("Password", { autocomplete: "current-password" });
    content = card("Sign in", "Open your workspace", "Sign in to the database control plane on this server.",
      authForm([field("Username", username), pw], "Sign in", async () => {
        done(await api("session", { method: "POST", body: { username: username.value, password: pw.input.value } }));
      }), [forgotten()]);
  }
  target.replaceChildren(...content);
  if (message) setAlert(target.querySelector("#login-note"), message, "error");
  target.querySelector("input")?.focus();
}
