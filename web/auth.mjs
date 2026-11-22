// The screens before sign-in: first-run setup, sign-in and invitations.
// Each has a short introduction beside the form and checks fields as you type.
import { h, icon, api, setAlert, nextId } from "./lib.mjs";

const USERNAME = /^[a-z0-9][a-z0-9._-]{1,31}$/;
const MIN_PASSWORD = 10;

// ---- introduction beside the form

const POINTS = [
  ["db", "Databases in a minute", "PostgreSQL, MySQL and Valkey."],
  ["history", "Backups built in", "Daily backups, easy restores."],
  ["users", "Made for teams", "A sign-in for everyone."],
];

function intro(mode, info) {
  const copy = {
    setup: ["Welcome", "Your databases, on your server.", "Create your account, then your first database."],
    login: ["Welcome back", "Your databases, on your server.", "Sign in to continue."],
    invite: [info?.reset ? "Password reset" : "You’re invited", info?.reset ? "Set a new password." : "Join your team.",
      info?.reset ? "You’re signed in right after." : "Choose a password to start."],
  }[mode];
  const steps = mode === "setup" ? h("ol", { class: "intro-steps", "aria-label": "Setup steps" },
    ["Create your account", "Create a database", "Connect your app"].map((text, i) =>
      h("li", { class: i === 0 ? "current" : null, "aria-current": i === 0 ? "step" : null }, h("span", { class: "dot", "aria-hidden": "true" }, String(i + 1)), text))) : null;
  const points = h("ul", { class: "intro-points" }, POINTS.map(([symbol, title, text]) =>
    h("li", null, h("span", { class: "intro-icon" }, icon(symbol)), h("span", null, h("strong", null, title), h("small", null, text)))));
  return [
    h("div", { class: "intro-copy" }, h("p", { class: "kicker" }, copy[0]), h("h2", null, copy[1]), h("p", null, copy[2])),
    steps, steps ? h("div", { class: "intro-included" }, h("p", { class: "kicker" }, "Included"), points) : points,
  ].filter(Boolean);
}

// ---- fields

// A labelled field with a hint line that turns into a message as you type.
function field(label, input, { hint = "", optional = false } = {}) {
  input.id ||= nextId("auth");
  const message = h("p", { class: "hint", id: `${input.id}-hint` }, hint);
  if (hint) input.setAttribute("aria-describedby", message.id);
  const el = h("div", { class: "field" },
    h("label", { class: "field-label", for: input.id }, label, optional ? h("span", { class: "optional" }, " (optional)") : null), input, message);
  el.input = input;
  el.say = (text = hint, tone = "") => {
    message.textContent = text;
    message.className = `hint${tone ? ` ${tone}` : ""}`;
    input.setAttribute("aria-invalid", String(tone === "error"));
    if (text) input.setAttribute("aria-describedby", message.id);
  };
  return el;
}

function passwordInput(label, autocomplete) {
  const input = h("input", { type: "password", autocomplete, required: true, maxlength: "128", spellcheck: "false" });
  const toggle = h("button", { type: "button", class: "icon-btn ghost reveal", "aria-label": `Show ${label.toLowerCase()}`, "aria-pressed": "false" }, icon("eye"));
  toggle.addEventListener("click", () => {
    const show = input.type === "password";
    input.type = show ? "text" : "password";
    toggle.setAttribute("aria-pressed", String(show));
    toggle.setAttribute("aria-label", `${show ? "Hide" : "Show"} ${label.toLowerCase()}`);
    input.focus();
  });
  return { input, wrap: h("div", { class: "password" }, input, toggle) };
}

function passwordField(label, { autocomplete = "new-password", hint = "" } = {}) {
  const { input, wrap } = passwordInput(label, autocomplete);
  input.id = nextId("pw");
  const caps = h("p", { class: "hint warn", hidden: true }, icon("alert"), " Caps Lock is on.");
  const message = h("p", { class: "hint", id: `${input.id}-hint` }, hint);
  const el = h("div", { class: "field" }, h("label", { class: "field-label", for: input.id }, label), wrap, caps, message);
  const capsCheck = (e) => { caps.hidden = !e.getModifierState?.("CapsLock"); };
  input.addEventListener("keydown", capsCheck);
  input.addEventListener("keyup", capsCheck);
  input.addEventListener("blur", () => { caps.hidden = true; });
  input.setAttribute("aria-describedby", message.id);
  el.input = input;
  el.message = message;
  el.say = (text = hint, tone = "") => {
    message.textContent = text;
    message.className = `hint${tone ? ` ${tone}` : ""}`;
    input.setAttribute("aria-invalid", String(tone === "error"));
  };
  return el;
}

// How hard a password is to guess, from 0 (empty) to 4.
function strength(pw) {
  if (!pw) return 0;
  if (pw.length < MIN_PASSWORD) return 1;
  let score = 2;
  if (pw.length >= 14) score++;
  if ([/[a-z]/, /[A-Z]/, /\d/, /[^A-Za-z0-9]/].filter(r => r.test(pw)).length >= 3 || pw.length >= 20) score++;
  if (/^(.)\1+$/.test(pw) || /^(?:0123456789|1234567890|password|qwerty)/i.test(pw)) score = 1;
  return Math.min(score, 4);
}

// A new password with a strength meter and its rules, checked as you type.
function newPassword(label, usernameOf) {
  const pw = passwordField(label);
  const meter = h("div", { class: "strength", "data-score": "0", "aria-hidden": "true" }, h("i"), h("i"), h("i"), h("i"));
  const word = h("span", { class: "strength-word" });
  const rule = (text) => h("li", null, icon("check"), text);
  const length = rule(`At least ${MIN_PASSWORD} characters`);
  const notName = rule("Not your username");
  pw.message.replaceWith(h("div", { class: "pw-feedback", id: pw.message.id, "aria-live": "polite" },
    h("div", { class: "strength-row" }, meter, word), h("ul", { class: "rules" }, length, notName)));
  const update = () => {
    const v = pw.input.value, score = strength(v);
    meter.dataset.score = String(score);
    word.textContent = v ? ["", "Too weak", "Fair", "Good", "Strong"][score] : "";
    word.className = `strength-word s${score}`;
    length.classList.toggle("met", v.length >= MIN_PASSWORD);
    const name = usernameOf?.() || "";
    notName.classList.toggle("met", Boolean(v) && v.toLowerCase() !== name.toLowerCase());
    if (pw.input.getAttribute("aria-invalid") === "true" && problem() === "") pw.input.setAttribute("aria-invalid", "false");
  };
  const problem = () => {
    const v = pw.input.value;
    if (v.length < MIN_PASSWORD) return `Use at least ${MIN_PASSWORD} characters.`;
    if (usernameOf && v.toLowerCase() === (usernameOf() || "").toLowerCase()) return "Can’t be your username.";
    return "";
  };
  pw.input.addEventListener("input", update);
  pw.update = update;
  pw.problem = problem;
  pw.fail = (text) => { pw.input.setAttribute("aria-invalid", "true"); word.textContent = text; word.className = "strength-word s1"; };
  return pw;
}

// The confirmation, which says as soon as it matches.
function confirmPassword(first) {
  const again = passwordField("Confirm password");
  const update = () => {
    const v = again.input.value;
    if (!v) again.say("");
    else if (v === first.input.value) again.say("Passwords match.", "ok");
    else if (first.input.value.startsWith(v)) again.say("");
    else again.say("Passwords don’t match.", "error");
  };
  again.input.addEventListener("input", update);
  first.input.addEventListener("input", () => { if (again.input.value) update(); });
  again.problem = () => again.input.value === first.input.value ? "" : "Passwords don’t match.";
  return again;
}

// A username from a display name: "Ada Lovelace" becomes "ada".
function suggestUsername(name) {
  const first = name.normalize("NFKD").replace(/[̀-ͯ]/g, "").toLowerCase().trim().split(/\s+/)[0] || "";
  const clean = first.replace(/[^a-z0-9._-]/g, "").replace(/^[._-]+/, "").slice(0, 32);
  return clean.length >= 2 ? clean : "";
}

// ---- forms

function authForm(fields, submitLabel, busyLabel, run, footer = null) {
  const note = h("div", { class: "alert", role: "status", id: "login-note", hidden: true });
  const label = h("span", null, submitLabel);
  const submit = h("button", { type: "submit", class: "btn primary wide" }, label, icon("arrow"));
  const form = h("form", { class: "auth-form", novalidate: true }, note, ...fields, submit, footer);
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    if (submit.disabled) return;
    note.hidden = true;
    submit.disabled = true;
    submit.classList.add("busy");
    label.textContent = busyLabel;
    try { await run(); }
    catch (err) { if (err.message) setAlert(note, err.message, "error"); }
    finally { submit.disabled = false; submit.classList.remove("busy"); label.textContent = submitLabel; }
  });
  return form;
}

// Field problems stop the submit and focus the first one.
function check(problems) {
  const first = problems.find(([, text]) => text);
  for (const [f, text] of problems) if (text) (f.fail || f.say)(text, "error");
  if (first) { first[0].input.focus(); throw new Error(""); }
}

function heading(kicker, title, description = "") {
  return [h("p", { class: "kicker" }, kicker), h("h1", null, title), description ? h("p", { class: "muted lead" }, description) : null].filter(Boolean);
}

const forgotten = () => h("details", { class: "disclosure" }, h("summary", null, "Forgot your password?"),
  h("p", null, "Ask an admin for a reset link. The only admin? Run this on the server:"),
  h("pre", null, "docker compose exec openrds openrds reset-password YOUR_USERNAME"));

function setupForm(done) {
  const nameInput = h("input", { type: "text", autocomplete: "name", maxlength: "64", placeholder: "Ada Lovelace" });
  const userInput = h("input", { type: "text", autocomplete: "username", spellcheck: "false", autocapitalize: "none", maxlength: "32", required: true, placeholder: "ada" });
  const name = field("Your name", nameInput, { hint: "Shown to your team." });
  const usernameHint = "You sign in with this.";
  const username = field("Username", userInput, { hint: usernameHint });
  const userValue = () => userInput.value.trim().toLowerCase();
  const pw = newPassword("Password", userValue);
  const again = confirmPassword(pw);
  let typedUsername = false;
  const userProblem = () => {
    const v = userValue();
    if (!v) return "Choose a username.";
    if (v.length < 2) return "Use at least 2 characters.";
    if (!USERNAME.test(v)) return "Use lowercase letters, digits, dots, dashes or underscores.";
    return "";
  };
  const checkUser = () => {
    const problem = userInput.value ? userProblem() : "";
    if (problem) username.say(problem, "error");
    else if (userInput.value) username.say(`You’ll sign in as ${userValue()}.`, "ok");
    else username.say(usernameHint);
    pw.update();
  };
  nameInput.addEventListener("input", () => {
    if (typedUsername) return;
    userInput.value = suggestUsername(nameInput.value);
    checkUser();
  });
  userInput.addEventListener("input", () => { typedUsername = userInput.value !== ""; checkUser(); });
  userInput.addEventListener("blur", () => { userInput.value = userValue(); });
  return authForm([h("div", { class: "form-grid" }, name, username), pw, again], "Create account and continue", "Creating your account…", async () => {
    check([[username, userProblem()], [pw, pw.problem()], [again, again.problem()]]);
    done(await api("setup", { method: "POST", body: { name: nameInput.value.trim(), username: userValue(), password: pw.input.value } }));
  }, h("p", { class: "auth-note" }, icon("shield"), h("span", null, "Only the first visitor can do this. Passwords are stored as salted hashes.")));
}

function loginForm(done) {
  const userInput = h("input", { type: "text", autocomplete: "username", spellcheck: "false", autocapitalize: "none", required: true });
  const username = field("Username", userInput);
  const pw = passwordField("Password", { autocomplete: "current-password" });
  const form = authForm([username, pw], "Sign in", "Signing in…", async () => {
    check([[username, userInput.value.trim() ? "" : "Enter your username."], [pw, pw.input.value ? "" : "Enter your password."]]);
    done(await api("session", { method: "POST", body: { username: userInput.value.trim(), password: pw.input.value } }));
  }, forgotten());
  form.classList.add("compact"); // no hints to reserve room for
  return form;
}

function inviteForm(info, secret, done) {
  const pw = newPassword("Choose a password", () => info.username);
  const again = confirmPassword(pw);
  return authForm([pw, again], info.reset ? "Save password" : "Create my account", "Saving…", async () => {
    check([[pw, pw.problem()], [again, again.problem()]]);
    const id = await api(`invites/${secret}`, { method: "POST", body: { password: pw.input.value } });
    location.hash = "#/";
    done(id);
  });
}

// mode: setup, login or invite. done() receives the new identity.
export async function renderAuth(target, { mode, message = "", secret = "", done }) {
  const aside = document.getElementById("auth-intro");
  let info = null, content;
  if (mode === "invite") {
    try { info = await api(`invites/${secret}`); }
    catch (err) {
      const back = h("button", { type: "button", class: "btn primary wide", onclick: () => { location.hash = "#/"; location.reload(); } }, "Go to sign-in");
      aside?.replaceChildren(...intro("login"));
      target.replaceChildren(...heading("Invitation", "This link doesn’t work", err.message), back);
      return;
    }
    content = [...heading(info.reset ? "Password reset" : "Invitation", info.reset ? `New password for ${info.name}` : `Welcome, ${info.name}`,
      `Your username is ${info.username}.`), inviteForm(info, secret, done)];
  } else if (mode === "setup") {
    content = [...heading("Step 1 of 3", "Create your administrator account", "You can invite your team later."), setupForm(done)];
  } else {
    content = [...heading("Sign in", "Open your workspace"), loginForm(done)];
  }
  aside?.replaceChildren(...intro(mode, info));
  target.replaceChildren(...content);
  if (message) setAlert(target.querySelector("#login-note"), message, "error");
  target.querySelector("input")?.focus();
}
