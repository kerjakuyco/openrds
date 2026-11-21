// The screens before sign-in: first-run setup, sign-in, the admin token, and invitations.
import { h, icon, api, setAlert, nextId } from "./lib.mjs";
import { passwordField } from "./account.mjs";

const TOKEN_HELP = () => h("details", { class: "disclosure" }, h("summary", null, "Where do I find the admin token?"),
  h("p", null, "From the installation directory on your VPS, run:"), h("pre", null, "docker compose exec openrds cat /var/lib/openrds/admin-token"));

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
    setAlert(note, "Signing in…", "info");
    try { await run(); }
    catch (err) { setAlert(note, err.message, "error"); }
    finally { submit.disabled = false; }
  });
  return form;
}

const link = (label, onclick) => h("button", { type: "button", class: "link-btn", onclick }, label);

// mode: setup, login, token or invite. done() receives the new identity.
export async function renderAuth(target, { mode, message = "", secret = "", done }) {
  const show = (m, extra = {}) => renderAuth(target, { mode: m, done, ...extra });
  let content;
  if (mode === "invite") {
    let info;
    try { info = await api(`invites/${secret}`); }
    catch (err) {
      target.replaceChildren(...card("Invitation", "This link does not work", err.message, h("div", null), [h("p", null, link("Go to sign-in", () => { location.hash = "#/"; show("login"); }))]));
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
    const token = h("input", { type: "password", autocomplete: "off", required: true });
    const name = h("input", { type: "text", autocomplete: "name", maxlength: "64", placeholder: "Ada Lovelace" });
    const username = h("input", { type: "text", autocomplete: "username", spellcheck: "false", maxlength: "32", required: true, placeholder: "ada" });
    const pw = passwordField("Password", { hint: "At least 10 characters." }), again = passwordField("Confirm password");
    content = card("Welcome", "Set up OpenRDS", "Prove you control this VPS with the admin token, then create the administrator account you will sign in with.",
      authForm([field("Admin token", token), h("div", { class: "form-grid" }, field("Your name", name), field("Username", username)), pw, again], "Create administrator account", async () => {
        if (pw.input.value !== again.input.value) throw new Error("The passwords do not match.");
        const id = await api("setup", { method: "POST", token: token.value.trim(), body: { name: name.value, username: username.value, password: pw.input.value } });
        done(id);
      }), [TOKEN_HELP(), h("p", { class: "auth-alt" }, link("Continue with the admin token only", () => show("token", { from: "setup" })))]);
  } else if (mode === "token") {
    const token = h("input", { type: "password", autocomplete: "current-password", required: true });
    content = card("Admin token", "Sign in with the admin token", "The token has every permission. Use it for automation and emergencies, such as resetting a forgotten password.",
      authForm([field("Admin token", token)], "Sign in", async () => {
        await api("session", { method: "POST", token: token.value.trim() });
        done(await api("session"));
      }), [TOKEN_HELP(), h("p", { class: "auth-alt" }, link("Sign in with an account instead", () => show("login")))]);
  } else {
    const username = h("input", { type: "text", autocomplete: "username", spellcheck: "false", required: true });
    const pw = passwordField("Password", { autocomplete: "current-password" });
    content = card("Sign in", "Open your workspace", "Sign in to the database control plane on this VPS.",
      authForm([field("Username", username), pw], "Sign in", async () => {
        done(await api("session", { method: "POST", body: { username: username.value, password: pw.input.value } }));
      }), [h("p", { class: "auth-alt" }, "Forgot your password? Ask an administrator for a reset link, or ", link("use the admin token", () => show("token")), ".")]);
  }
  target.replaceChildren(...content);
  if (message) setAlert(target.querySelector("#login-note"), message, "error");
  target.querySelector("input")?.focus();
}
