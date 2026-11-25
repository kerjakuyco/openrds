// Who can reach a database and with which credentials: network access with a
// firewall, encryption in transit, and database users.
import { h, icon, btn, api, toast, modal, panel, infoPanel, keyValues, dataTable, alertBox, empty, when, ago, chip, codeBlock, nextId, help, menu, isLive, isRunning } from "./lib.mjs";
import { engine, sync, can, go } from "./store.mjs";
import { confirmReboot } from "./actions.mjs";
import { connectAs } from "./connect.mjs";

const SCHEME = { postgres: "postgresql", mysql: "mysql", valkey: "redis" };
const ACCESS = {
  owner: ["Owner", "neutral"],
  "read-write": ["Read and write", "info"],
  "read-only": ["Read only", "good"],
};
const ACCESS_TEXT = {
  postgres: { owner: "Everything, including table changes.", "read-write": "Read and change rows. No table changes.", "read-only": "Read only." },
  mysql: { owner: "Everything, including schema changes.", "read-write": "Read and change rows. No schema changes.", "read-only": "Read only." },
  valkey: { owner: "Every command.", "read-write": "Read and write keys. No admin or risky commands.", "read-only": "Read keys only." },
};
const passwordPattern = /^[A-Za-z0-9._~-]{16,128}$/;
const userPattern = /^[a-z][a-z0-9_]{0,31}$/;
const everyone = (rules) => (rules || []).some(r => r.cidr === "0.0.0.0/0" || r.cidr === "::/0");

// Why credential changes are unavailable right now, if they are.
export function accessBlocker(d) {
  if (!can("operator")) return "Viewers can’t change this.";
  if (d.busy) return "Another operation is running.";
  if (!isRunning(d)) return "Start the database first.";
  if (!d.tlsReady) return "Reboot once to turn this on.";
  return "";
}

export function upgradeNotice(d) {
  if (d.tlsReady || d.status === "not provisioned") return null;
  const reboot = can("operator") ? btn([icon("reboot"), "Reboot now"], () => confirmReboot(d), "small") : null;
  if (reboot) reboot.disabled = d.busy || !isLive(d);
  return alertBox("Reboot once to turn on TLS and database users.", "info", " ", reboot);
}

// A newly set password, shown with a ready connection string.
function showSecret(d, user, password, title) {
  const uri = `${d.engine === "valkey" && d.requireTLS ? "rediss" : SCHEME[d.engine]}://${user}:${password}@127.0.0.1:${d.port}/${engine(d.engine).database}`;
  modal({
    title, kicker: d.name, confirm: "Done", cancel: false, wide: true,
    body: [
      h("p", { class: "muted" }, "Update your apps. You can reveal it again on the Connectivity tab."),
      codeBlock(password, { caption: `Password for ${user}` }),
      codeBlock(uri, { caption: "Connection string" }),
    ],
    run: async () => {},
  });
}

function passwordChoice() {
  const id = nextId("pw");
  const generate = h("input", { type: "radio", name: id, value: "generate", checked: true, id: `${id}-g` });
  const own = h("input", { type: "radio", name: id, value: "own", id: `${id}-o` });
  const input = h("input", { type: "text", autocomplete: "off", spellcheck: "false", maxlength: "128", placeholder: "At least 16 characters", hidden: true, "aria-label": "Your password" });
  const sync = () => { input.hidden = !own.checked; if (own.checked) input.focus(); };
  generate.addEventListener("change", sync);
  own.addEventListener("change", sync);
  const el = h("fieldset", { class: "apply-choice" }, h("legend", null, "Password"),
    h("label", { class: "check", for: generate.id }, generate, h("span", null, h("strong", null, "Generate one"), h("small", null, "Recommended."))),
    h("label", { class: "check", for: own.id }, own, h("span", null, h("strong", null, "Choose my own"), h("small", null, "16–128 letters, digits and . _ ~ -"))),
    input);
  return {
    el,
    value() {
      if (!own.checked) return "";
      const v = input.value.trim();
      if (!passwordPattern.test(v)) throw new Error("Use 16–128 letters, digits and . _ ~ -");
      return v;
    },
  };
}

export function changePassword(d, user) {
  const owner = user === engine(d.engine).user;
  const choice = passwordChoice();
  modal({
    title: `Change the password for ${user}?`, kicker: d.name, confirm: "Change password",
    body: [
      h("p", { class: "muted" }, owner ? "The old password stops working now. Update your apps." : "The old password stops working now."),
      choice.el,
    ],
    run: async () => {
      const path = owner ? `instances/${d.name}/password` : `instances/${d.name}/users/${user}/password`;
      const { password } = await api(path, { method: "POST", body: { password: choice.value() } });
      await sync();
      showSecret(d, user, password, `New password for ${user}`);
    },
  });
}

function createUser(d) {
  const name = h("input", { type: "text", maxlength: "32", autocomplete: "off", spellcheck: "false", placeholder: "reporting", required: true, id: nextId("user") });
  const id = nextId("access");
  const options = ["read-write", "read-only"].map((value, i) => {
    const input = h("input", { type: "radio", name: id, value, checked: i === 0, id: `${id}-${i}` });
    return h("label", { class: "check", for: input.id }, input, h("span", null, h("strong", null, ACCESS[value][0]), h("small", null, ACCESS_TEXT[d.engine][value])));
  });
  const choice = passwordChoice();
  modal({
    title: "Create database user", kicker: d.name, confirm: "Create user", wide: true,
    body: [
      h("label", { class: "field-label", for: name.id }, "Username"), name,
      h("p", { class: "hint" }, "Lowercase letters, digits and underscores. One user per app is easiest to revoke."),
      h("fieldset", { class: "apply-choice" }, h("legend", null, "Access"), ...options),
      choice.el,
    ],
    run: async (form) => {
      const user = name.value.trim();
      if (!userPattern.test(user)) throw new Error("Start with a letter. Use lowercase letters, digits and underscores.");
      const access = form.querySelector(`input[name="${id}"]:checked`).value;
      const { password } = await api(`instances/${d.name}/users`, { method: "POST", body: { name: user, access, password: choice.value() } });
      await sync();
      showSecret(d, user, password, `${user} is ready`);
    },
  });
}

function deleteUser(d, user) {
  modal({
    title: `Delete ${user}?`, kicker: d.name, confirm: "Delete user", tone: "danger", typed: user,
    body: h("p", { class: "muted" }, "It can’t sign in anymore and its connections end now. Its data stays."),
    run: async () => {
      await api(`instances/${d.name}/users/${user}`, { method: "DELETE" });
      toast(`${user} was deleted from ${d.name}.`, "success");
      await sync();
    },
  });
}

export function usersTab(d) {
  const eng = engine(d.engine);
  const blocker = accessBlocker(d);
  const rows = [{ name: eng.user, access: "owner", createdAt: d.createdAt }, ...(d.users || [])];
  const actions = (u) => {
    const items = [
      { label: "Connection details", icon: "link", run: () => { connectAs(d.name, u.name); go(`#/databases/${d.name}/connectivity`); } },
      { label: "Change password", icon: "key", disabled: Boolean(blocker), reason: blocker, run: () => changePassword(d, u.name) },
    ];
    if (u.access !== "owner") items.push("-", { label: "Delete user", icon: "trash", danger: true, disabled: Boolean(blocker), reason: blocker, run: () => deleteUser(d, u.name) });
    return menu("Actions", items, { cls: "small ghost" });
  };
  const create = btn([icon("plus"), "Create user"], () => createUser(d), "small primary");
  create.disabled = Boolean(blocker);
  if (blocker) create.title = blocker;
  const el = h("div", { class: "stack-gap" },
    upgradeNotice(d),
    panel("Database users", { counter: rows.length, description: "Give each app its own user.", actions: can("operator") ? [create] : [] },
      h("div", { class: "flush" }, dataTable({ caption: "Database users", rows, columns: [
        { key: "name", label: "User", value: u => h("code", null, u.name) },
        { key: "access", label: "Access", value: u => chip(ACCESS[u.access][0], ACCESS[u.access][1]) },
        { key: "created", label: "Created", value: u => when(u.createdAt) },
        { key: "actions", label: h("span", { class: "sr-only" }, "Actions"), cls: "actions", value: u => can("operator") ? actions(u) : null },
      ] }))),
    panel("Access levels", {}, keyValues(Object.keys(ACCESS).map(k => [ACCESS[k][0], ACCESS_TEXT[d.engine][k]]), "stacked")),
    alertBox("Snapshots don’t include users. Restores start with the owner only.", "info"));
  return { el };
}

// ---- network access

function rulesEditor(initial) {
  const rows = h("div", { class: "rule-rows" });
  function addRow(rule = { cidr: "", description: "" }) {
    const cidr = h("input", { type: "text", value: rule.cidr, placeholder: "203.0.113.7 or 203.0.113.0/24", spellcheck: "false", autocomplete: "off", "aria-label": "Source address or range" });
    const desc = h("input", { type: "text", value: rule.description || "", placeholder: "Office, CI runner…", maxlength: "100", "aria-label": "Description" });
    const row = h("div", { class: "rule-row" }, cidr, desc, h("button", { type: "button", class: "icon-btn ghost", "aria-label": "Remove source", onclick: () => row.remove() }, icon("trash")));
    rows.append(row);
    return cidr;
  }
  (initial || []).forEach(addRow);
  return {
    el: h("div", null, rows, h("button", { type: "button", class: "btn small", onclick: () => addRow().focus() }, icon("plus"), "Add source")),
    value: () => [...rows.children].map(r => ({ cidr: r.children[0].value.trim(), description: r.children[1].value.trim() })).filter(r => r.cidr),
  };
}

const STANDARD_PORT = { postgres: 5432, mysql: 3306, valkey: 6379 };

function suggestPublicPort(d, all) {
  const taken = new Set(all.flatMap(o => o.name === d.name ? [] : [o.port, o.publicAccess ? o.publicPort : 0]));
  for (let p = STANDARD_PORT[d.engine]; p < 65535; p++) {
    if (p !== d.port && !taken.has(p)) return p;
  }
  return 0;
}

function editNetwork(d, all, { addSource = "" } = {}) {
  const on = h("input", { type: "checkbox", class: "switch", role: "switch", checked: d.publicAccess || Boolean(addSource), id: nextId("public") });
  const port = h("input", { type: "number", min: "1024", max: "65535", value: String(d.publicPort || suggestPublicPort(d, all)), id: nextId("pport") });
  const local = /^(localhost|127\.|\[?::1\]?$)/.test(location.hostname) ? "" : location.hostname;
  const address = h("input", { type: "text", value: d.publicHost || "", placeholder: local || "db.example.com or 203.0.113.10", spellcheck: "false", id: nextId("paddr") });
  const rules = rulesEditor([...(d.firewall || []), ...(addSource ? [{ cidr: addSource, description: "" }] : [])]);
  const details = h("div", { class: "stack-gap", hidden: !on.checked },
    h("div", { class: "form-grid" },
      h("div", null, h("label", { class: "field-label", for: port.id }, "Public port"), port, h("p", { class: "hint" }, "Open on all interfaces.")),
      h("div", null, h("label", { class: "field-label", for: address.id }, "Public address ", h("span", { class: "muted" }, "(optional)")), address,
        h("p", { class: "hint" }, "Hostname or IP your apps use."))),
    h("div", null, h("span", { class: "field-label" }, "Allowed sources ", help("Only these addresses can connect. 0.0.0.0/0 allows anyone.", "About allowed sources")),
      rules.el),
    alertBox("Docker bypasses host firewalls like ufw. These rules protect the port.", "info"));
  on.addEventListener("change", () => { details.hidden = !on.checked; });
  modal({
    title: "Network access", kicker: d.name, confirm: "Save network access", wide: true,
    body: [
      h("label", { class: "check", for: on.id }, on, h("span", null, h("strong", null, "Public access"), h("small", null, "Allow the sources below to connect from outside."))),
      details,
    ],
    run: async () => {
      const body = { publicAccess: on.checked, firewall: rules.value(), publicHost: address.value.trim() };
      if (on.checked) body.publicPort = Number(port.value);
      const r = await api(`instances/${d.name}/modify`, { method: "POST", body });
      toast(r?.pendingRestart ? "Saved. Reboot to update the certificate." : on.checked ? `${d.name} is public on port ${body.publicPort}.` : `${d.name} is private.`, "success");
      await sync();
    },
  });
}

// The RDS-style overview: endpoint and port, networking, security.
export function connectivityPanel(d, all) {
  const eng = engine(d.engine);
  const rules = d.firewall || [];
  const tls = !d.tlsReady ? chip("After a reboot", "neutral")
    : d.requireTLS ? chip("Required", "good") : d.engine === "valkey" ? chip("Off", "warn") : chip("Optional", "info");
  const names = ["localhost", "127.0.0.1", `openrds-${d.name}`, d.engine === "postgres" && `openrds-${d.name}-pooler`, d.publicHost].filter(Boolean);
  const actions = [];
  if (can("operator")) actions.push(btn([icon("globe"), "Edit network"], () => editNetwork(d, all), "small"));
  actions.push(h("a", { class: "btn small", href: "/api/tls/ca.pem", download: "openrds-ca.pem" }, icon("download"), "CA certificate"));
  const warnings = [];
  if (d.publicAccess && !rules.length) warnings.push(alertBox("No sources yet, so nothing can connect.", "warn"));
  if (d.publicAccess && everyone(rules)) warnings.push(alertBox("Anyone can try to connect. Limit it if you can.", "warn"));
  if (d.publicAccess && !d.requireTLS) warnings.push(alertBox("TLS is not required, so traffic isn’t encrypted. Turn it on under Modify.", "warn"));
  return [upgradeNotice(d), infoPanel("Connectivity & security", [
    ["Endpoint & port", [
      ["Endpoint", h("code", null, "127.0.0.1")],
      ["Port", h("code", null, String(d.port))],
      ["Container DNS name", h("code", null, `openrds-${d.name}`)],
    ]],
    ["Networking", [
      ["Public access", d.publicAccess ? h("span", { class: "status info" }, icon("globe"), `On · port ${d.publicPort}`) : h("span", { class: "status good" }, icon("lock"), "Off")],
      ["Public endpoint", d.publicAccess ? h("code", null, `${d.publicHost || "<server address>"}:${d.publicPort}`) : "—"],
      ["Allowed sources", d.publicAccess ? String(rules.length) : "—"],
    ]],
    ["Security", [
      ["Require TLS", tls],
      ["Certificate valid for", h("span", null, ...names.flatMap((n, i) => [i ? ", " : "", h("code", null, n)]))],
      ["Owner user", h("code", null, eng.user)],
    ]],
  ], { actions }), ...warnings].filter(Boolean);
}

// Like security group rules: the sources allowed through the public port,
// and who has tried it.
export function firewallPanel(d, all) {
  const rules = d.firewall || [];
  if (!d.publicAccess && !rules.length) return null;
  const attempts = h("div", { class: "attempts" });
  async function loadAttempts() {
    if (!d.publicAccess || !can("operator")) return;
    try {
      const list = await api(`instances/${d.name}/connections`);
      attempts.replaceChildren(h("h3", { class: "sub" }, icon("pulse"), "Recent connection attempts"),
        list.length ? dataTable({ caption: "Recent connection attempts", rows: list, columns: [
          { key: "ip", label: "Source", value: a => h("code", null, a.ip) },
          { key: "allowed", label: "Result", value: a => a.allowed ? h("span", { class: "status good" }, icon("ok"), "Allowed") : h("span", { class: "status critical" }, icon("error"), "Blocked") },
          { key: "attempts", label: "Attempts", value: a => String(a.attempts) },
          { key: "last", label: "Last seen", value: a => ago(a.lastAt) },
          { key: "act", label: h("span", { class: "sr-only" }, "Actions"), cls: "actions", value: a => a.allowed ? null : btn("Allow", () => editNetwork(d, all, { addSource: a.ip }), "small") },
        ] }) : h("p", { class: "muted" }, "No attempts yet."));
    } catch { /* the panel works without the log */ }
  }
  loadAttempts();
  return panel("Firewall rules", {
    counter: rules.length, description: d.publicAccess ? "Only these sources can connect." : "Used when public access is on.",
    actions: can("operator") ? [btn([icon("edit"), "Edit"], () => editNetwork(d, all), "small")] : [],
  },
  rules.length ? dataTable({ caption: "Allowed sources", rows: rules, columns: [
    { key: "cidr", label: "Source", value: r => h("code", null, r.cidr) },
    { key: "description", label: "Description", value: r => r.description || h("span", { class: "muted" }, "—") },
  ] }) : h("p", { class: "muted" }, "No sources yet."),
  attempts);
}
