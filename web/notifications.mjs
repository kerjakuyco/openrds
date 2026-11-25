// Notifications: send problems to Slack, Discord, Telegram, a webhook or
// email, like RDS event subscriptions. Admins only: channels hold secrets.
import { h, icon, btn, api, toast, modal, panel, keyValues, dataTable, empty, alertBox, ago, when, menu, chip, nextId } from "./lib.mjs";
import { can, sync } from "./store.mjs";
import { field, choiceCards, select } from "./forms.mjs";

const KINDS = [
  { value: "slack", title: "Slack", detail: "Incoming webhook" },
  { value: "discord", title: "Discord", detail: "Channel webhook" },
  { value: "telegram", title: "Telegram", detail: "Bot in a chat" },
  { value: "webhook", title: "Webhook", detail: "JSON to any URL" },
  { value: "email", title: "Email", detail: "Through SMTP" },
];
const CATEGORIES = [
  { value: "health", title: "Database health", short: "Health", symbol: "pulse", detail: "Unhealthy, crashed, out of memory or stopped, and healthy again." },
  { value: "failures", title: "Failed operations", short: "Failures", symbol: "error", detail: "Backups, restores, copies to storage and changes that failed." },
  { value: "disk", title: "Disk space", short: "Disk", symbol: "disk", detail: "Free space for backups runs low, and recovers." },
];
const URL_HINTS = {
  slack: "In Slack, add the Incoming Webhooks app to a channel and copy its URL.",
  discord: "In Discord: channel settings → Integrations → Webhooks → New webhook → Copy URL.",
  webhook: "OpenRDS posts JSON with title, detail, severity, category, database and text.",
};
const kindTitle = (k) => KINDS.find(x => x.value === k)?.title || k;

function channelForm(current, smtpReady) {
  let kind = current?.kind || "slack";
  const name = h("input", { type: "text", value: current?.name || "", placeholder: "#ops-alerts", maxlength: "60", autocomplete: "off" });
  const url = h("input", { type: "url", spellcheck: "false", autocomplete: "off", placeholder: current ? "Leave empty to keep the saved URL" : "https://…" });
  const token = h("input", { type: "password", autocomplete: "new-password", placeholder: current ? "Leave empty to keep the saved token" : "123456789:AAH…" });
  const chat = h("input", { type: "text", value: current?.chatId || "", spellcheck: "false", placeholder: "-1001234567890" });
  const to = h("textarea", { rows: "2", spellcheck: "false", placeholder: "ops@example.com, oncall@example.com" });
  to.value = (current?.to || []).join(", ");
  const urlField = field("Webhook URL", url, { hint: URL_HINTS[kind] || "" });
  const telegram = h("div", { class: "form-grid" },
    field("Bot token", token, { hint: "Create a bot with @BotFather and copy its token." }),
    field("Chat ID", chat, { hint: "Add the bot to a group and use its ID (starts with -100), or @name of a public channel." }));
  const toField = field("Send to", to, { hint: "Up to 20 addresses, separated by commas." });
  const boxes = CATEGORIES.map(c => {
    const input = h("input", { type: "checkbox", value: c.value, checked: current ? current.categories.includes(c.value) : true, id: nextId("cat") });
    return h("label", { class: "check", for: input.id }, input, h("span", null, h("strong", null, c.title), h("small", null, c.detail)));
  });
  const show = () => {
    urlField.hidden = !URL_HINTS[kind];
    const hint = urlField.querySelector(".hint");
    if (hint) hint.textContent = URL_HINTS[kind] || "";
    url.placeholder = current ? "Leave empty to keep the saved URL" : kind === "discord" ? "https://discord.com/api/webhooks/…" : kind === "slack" ? "https://hooks.slack.com/services/…" : "https://…";
    telegram.hidden = kind !== "telegram";
    toField.hidden = kind !== "email";
  };
  const cards = current ? null : choiceCards(nextId("kind"), KINDS.map(k => ({ ...k, disabled: k.value === "email" && !smtpReady, reason: "Set up the email server first." })),
    kind, (v) => { kind = v; show(); }, { label: "Channel type", cls: "cols-5" });
  show();
  const el = h("div", { class: "stack-gap" },
    cards && [h("span", { class: "field-label" }, "Send to"), cards],
    field("Name", name, { hint: "How the channel shows in OpenRDS." }),
    urlField, telegram, toField,
    h("fieldset", { class: "category-checks" }, h("legend", { class: "field-label" }, "Send these events"), boxes));
  const read = () => ({
    kind, name: name.value.trim(), url: url.value.trim(), token: token.value.trim(), chatId: chat.value.trim(),
    to: to.value.split(/[\s,;]+/).filter(Boolean),
    categories: boxes.map(b => b.querySelector("input")).filter(i => i.checked).map(i => i.value),
  });
  return { el, read };
}

function editChannel(current, smtpReady, onChange) {
  const form = channelForm(current, smtpReady);
  modal({
    title: current ? "Edit channel" : "Add notification channel", kicker: current ? current.name : "", confirm: current ? "Save" : "Send test and add", wide: true,
    body: [form.el, h("p", { class: "muted" }, current ? "A changed destination gets a test message before it is saved." : "OpenRDS sends a test message first, and adds the channel when it arrives.")],
    run: async () => {
      const body = form.read();
      if (current) await api(`notifications/channels/${current.id}`, { method: "PATCH", body });
      else await api("notifications/channels", { method: "POST", body });
      toast(current ? "Channel saved." : "Channel added. Check that the test message arrived.", "success");
      onChange();
    },
  });
}

function deleteChannel(ch, onChange) {
  modal({
    title: "Delete this channel?", kicker: ch.name, confirm: "Delete", tone: "danger",
    body: h("p", { class: "muted" }, "It stops receiving alerts. Other channels are not affected."),
    run: async () => {
      await api(`notifications/channels/${ch.id}`, { method: "DELETE" });
      toast("Channel deleted.", "success");
      onChange();
    },
  });
}

function editSMTP(current, onChange) {
  const host = h("input", { type: "text", value: current?.host || "", placeholder: "smtp.gmail.com", spellcheck: "false" });
  const port = h("input", { type: "number", min: "1", max: "65535", value: String(current?.port || 587), class: "num" });
  const security = select([["starttls", "STARTTLS (usually port 587)"], ["tls", "TLS (usually port 465)"], ["none", "None (local relay only)"]], current?.security || "starttls");
  security.addEventListener("change", () => {
    if (["587", "465", "25"].includes(port.value)) port.value = { starttls: "587", tls: "465", none: "25" }[security.value];
  });
  const user = h("input", { type: "text", value: current?.username || "", autocomplete: "off", spellcheck: "false" });
  const pass = h("input", { type: "password", autocomplete: "new-password", placeholder: current?.passwordSet ? "Leave empty to keep the saved password" : "" });
  const from = h("input", { type: "text", value: current?.from || "", placeholder: "OpenRDS <alerts@example.com>", spellcheck: "false" });
  modal({
    title: current ? "Edit email server" : "Set up email server", confirm: "Check and save", wide: true,
    body: [
      h("div", { class: "form-grid" }, field("SMTP server", host), field("Port", port)),
      field("Security", security),
      h("div", { class: "form-grid" }, field("User name", user, { optional: true }), field("Password", pass, { optional: true, hint: "For Gmail and others, an app password." })),
      field("From", from, { hint: "The sender of alert emails." }),
      h("p", { class: "muted" }, "OpenRDS connects and signs in before saving."),
    ],
    run: async () => {
      await api("notifications/smtp", { method: "PUT", body: { host: host.value.trim(), port: Number(port.value), security: security.value, username: user.value.trim(), password: pass.value, from: from.value.trim() } });
      toast("Email server saved.", "success");
      onChange();
    },
  });
}

function removeSMTP(onChange) {
  modal({
    title: "Remove the email server?", confirm: "Remove", tone: "danger",
    body: h("p", { class: "muted" }, "Email channels need it, so delete them first."),
    run: async () => {
      await api("notifications/smtp", { method: "DELETE" });
      toast("Email server removed.", "success");
      onChange();
    },
  });
}

function lastMessage(ch) {
  if (ch.lastError) return h("div", { class: "stack" }, h("span", { class: "status critical" }, icon("error"), "Failed"), h("small", { title: ch.lastError }, ch.lastError));
  if (ch.lastSentAt) return h("div", { class: "stack" }, h("span", { class: "status good" }, icon("ok"), "Sent"), h("small", { title: when(ch.lastSentAt) }, ago(ch.lastSentAt)));
  return h("span", { class: "muted" }, "Not yet");
}

export function notificationsView() {
  const body = h("div", { class: "stack-gap" }, panel("Notifications", {}, empty("bell", "Loading…", "")));
  const el = h("div", null,
    h("div", { class: "page-head" }, h("div", null, h("h1", null, "Notifications"), h("p", { class: "muted" }, "Get a message when a database or a backup needs attention."))),
    body);
  if (!can("admin")) {
    body.replaceChildren(panel("Notifications", {}, empty("key", "Admins set up notifications", "Ask an admin to send alerts to Slack, Telegram or email.")));
    return { el, title: "Notifications", nav: "notifications", crumbs: [["Notifications"]] };
  }

  async function load() {
    try { render(await api("notifications")); }
    catch (err) { body.replaceChildren(alertBox(err.message, "error")); }
    sync(); // the dashboard counts channels
  }

  function render(v) {
    const smtpReady = Boolean(v.smtp);
    const add = btn([icon("plus"), "Add channel"], () => editChannel(null, smtpReady, load), "primary small");
    const channels = panel("Channels", { counter: v.channels.length || null, description: "Each channel receives the events you choose.", actions: [add] },
      v.channels.length ? dataTable({ caption: "Notification channels", rows: v.channels, columns: [
        { key: "name", label: "Channel", value: c => h("div", { class: "stack" }, h("strong", null, c.name), h("small", null, kindTitle(c.kind))) },
        { key: "target", label: "Sends to", cls: "wrap", value: c => h("code", null, c.target) },
        { key: "categories", label: "Events", value: c => h("div", { class: "chips" }, c.categories.map(k => chip(CATEGORIES.find(x => x.value === k)?.short || k, "info"))) },
        { key: "last", label: "Last message", value: lastMessage },
        { key: "actions", label: h("span", { class: "sr-only" }, "Actions"), cls: "actions", value: c => h("div", { class: "cell-actions" },
          btn([icon("play"), "Send test"], async (b) => {
            b.disabled = true;
            try { await api(`notifications/channels/${c.id}/test`, { method: "POST" }); toast(`Test message sent to ${c.name}.`, "success"); }
            catch (err) { toast(err.message, "error"); }
            finally { load(); }
          }, "small"),
          menu("", [
            { label: "Edit", icon: "edit", run: () => editChannel(c, smtpReady, load) },
            { label: "Delete", icon: "trash", danger: true, run: () => deleteChannel(c, load) },
          ], { cls: "small ghost icon-only", labelIcon: "more", chevron: false, ariaLabel: `More actions for ${c.name}` })) },
      ] }) : empty("bell", "No channels yet", "Send alerts to Slack, Discord, Telegram, a webhook or email.", add));
    const s = v.smtp;
    const smtp = panel("Email server", {
      description: "Needed for email channels.",
      actions: s ? [btn([icon("edit"), "Edit"], () => editSMTP(s, load), "small"), btn("Remove", () => removeSMTP(load), "small")] : [],
    }, s ? keyValues([["Server", h("code", null, `${s.host}:${s.port}`)], ["Security", { starttls: "STARTTLS", tls: "TLS", none: "None" }[s.security]],
      ["Sign-in", s.username || "None"], ["From", s.from]])
      : empty("globe", "Not set up", "Use any SMTP server, such as Gmail, Amazon SES or your provider’s.", btn("Set up email server", () => editSMTP(null, load), "small")));
    body.replaceChildren(
      h("ul", { class: "how-it-works" }, CATEGORIES.map(c => h("li", null, h("span", { class: "how-icon" }, icon(c.symbol)), h("span", null, h("strong", null, c.title), h("span", { class: "muted" }, c.detail))))),
      channels, smtp);
  }

  load();
  return { el, title: "Notifications", nav: "notifications", crumbs: [["Notifications"]] };
}
