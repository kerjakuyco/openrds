// Connection pooling for PostgreSQL, like RDS Proxy: PgBouncer in front of
// the database, so many clients share a few server connections.
import { h, icon, btn, api, toast, modal, infoPanel, nextId, plural } from "./lib.mjs";
import { can, sync } from "./store.mjs";
import { field, select } from "./forms.mjs";

const MODES = {
  transaction: "Transaction",
  session: "Session",
};

function editPooler(d) {
  const p = d.pooler || {};
  const on = h("input", { type: "checkbox", class: "switch", role: "switch", checked: true, id: nextId("pool") });
  const mode = select([["transaction", "Transaction (recommended)"], ["session", "Session"]], p.mode || "transaction");
  const size = h("input", { type: "number", min: "1", max: "1000", step: "1", value: String(p.poolSize || 20), class: "num" });
  const clients = h("input", { type: "number", min: "10", max: "50000", step: "1", value: String(p.maxClients || 1000), class: "num" });
  const port = h("input", { type: "number", min: "1024", max: "65535", step: "1", value: p.port ? String(p.port) : "", placeholder: "6432 or the next free port", class: "num" });
  const modeHint = h("p", { class: "hint" });
  const explain = () => {
    modeHint.textContent = mode.value === "transaction"
      ? "Each transaction borrows a server connection, so a few serve many clients. Session state (SET, LISTEN, advisory locks, temporary tables) does not carry over between transactions."
      : "Each client keeps a server connection until it disconnects. Everything works, but it only saves the cost of connecting.";
  };
  mode.addEventListener("change", explain);
  explain();
  modal({
    title: d.pooler ? "Connection pooling" : "Turn on connection pooling", kicker: d.name, confirm: d.pooler ? "Save" : "Turn on", wide: true,
    body: [
      d.pooler ? h("label", { class: "check", for: on.id }, on, h("span", null, h("strong", null, "Pooling on"), h("small", null, "Turn off to remove the pooler. Connect to the database directly again."))) : null,
      field("Pooling mode", mode), modeHint,
      h("div", { class: "form-grid" },
        field("Pool size", size, { hint: "Server connections per user and database." }),
        field("Client connections", clients, { hint: "The most clients the pooler accepts at once." })),
      field("Host port", port, { hint: "On this server’s loopback interface, like the database’s port." }),
      h("p", { class: "muted" }, "The database keeps running. Clients connected through the pooler reconnect when its settings change."),
    ],
    run: async () => {
      const body = on.checked
        ? { enabled: true, mode: mode.value, poolSize: Number(size.value), maxClients: Number(clients.value), port: Number(port.value) || 0 }
        : { enabled: false };
      await api(`instances/${d.name}/modify`, { method: "POST", body: { pooler: body } });
      toast(on.checked ? `Connection pooling is on for ${d.name}.` : `Connection pooling is off for ${d.name}.`, "success");
      await sync();
    },
  });
}

const statusChip = (s) => s === "running" ? h("span", { class: "status good" }, icon("ok"), "Running")
  : s === "starting" ? h("span", { class: "status info" }, icon("pending"), "Starting")
  : h("span", { class: "status warn" }, icon("alert"), s ? s.charAt(0).toUpperCase() + s.slice(1) : "Not running");

export function poolerPanel(d) {
  if (d.engine !== "postgres") return null;
  const p = d.pooler;
  const actions = [];
  if (can("operator")) {
    const b = btn(p ? [icon("edit"), "Edit pooling"] : [icon("plus"), "Turn on pooling"], () => editPooler(d), p ? "small" : "small primary");
    b.disabled = d.busy || d.pendingRestore;
    actions.push(b);
  }
  if (!p) {
    return infoPanel("Connection pooling", [
      ["Pooling", [["Status", h("span", { class: "muted" }, "Off")], ["What it does", "Many clients share a few server connections, like RDS Proxy."]]],
      ["When to use it", [["Good for", "Apps with many processes, workers or serverless functions, or that connect often."]]],
    ], { actions });
  }
  return infoPanel("Connection pooling", [
    ["Pooler", [["Status", statusChip(d.poolerStatus)], ["Mode", MODES[p.mode] || p.mode], ["Pool", `${plural(p.poolSize, "server connection")} per user`]]],
    ["Endpoint & port", [["Endpoint", h("code", null, "127.0.0.1")], ["Port", h("code", null, String(p.port))], ["Container DNS name", h("code", null, `openrds-${d.name}-pooler:6432`)]]],
    ["Limits", [["Client connections", `Up to ${p.maxClients.toLocaleString()}`], ["Prepared statements", "Supported"]]],
  ], { actions });
}
