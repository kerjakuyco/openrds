// Servers: where databases run. The console manages each server's Docker
// over SSH; this page adds them, shows their capacity, and changes their
// addresses.
import { h, icon, btn, api, modal, menu, empty, dataTable, meter, bytes, plural, ago, codeBlock, alertBox, nextId, toast, chip } from "./lib.mjs";
import { state, subscribe, sync, can } from "./store.mjs";

// The servers, and whether there is more than the console's own.
export const servers = () => state.servers || [];
export const multiServer = () => servers().length > 1;
export const serverOf = (d) => servers().find(s => s.id === (d.server || "local"));

function field(label, input, hint = "", optional = false) {
  input.id ||= nextId("srv");
  return h("div", { class: "field" }, h("label", { class: "field-label", for: input.id }, label, optional ? h("span", { class: "muted" }, " (optional)") : null),
    input, hint ? h("p", { class: "hint" }, hint) : null);
}

const text = (attrs) => h("input", { type: "text", spellcheck: "false", autocomplete: "off", ...attrs });

function addressFields(s = {}) {
  const priv = text({ value: s.privateAddress || "", placeholder: "10.0.0.5" });
  const pub = text({ value: s.publicAddress || "", placeholder: "db2.example.com or 203.0.113.20" });
  return {
    priv, pub,
    el: h("div", { class: "form-grid" },
      field("Private address", priv, "Its IPv4 address on your private network (VPC, WireGuard or Tailscale). Private access listens here.", true),
      field("Public address", pub, "The hostname or IP clients on the internet use. Public access shows it.", true)),
  };
}

// Adding a server: run one command there, check its host key, connect.
async function addServer() {
  let key;
  try { key = await api("servers/key"); }
  catch (err) { toast(err.message, "error"); return; }
  const name = text({ placeholder: "db-2", maxlength: "40" });
  const address = text({ placeholder: "openrds@10.0.0.5" });
  const addresses = addressFields();
  const keys = h("div", { class: "host-keys" });
  const confirmID = nextId("trust");
  const trust = h("input", { type: "checkbox", id: confirmID });
  let scanned = null;
  const check = btn([icon("shield"), "Check host key"], async (b) => {
    b.disabled = true;
    keys.replaceChildren(h("p", { class: "muted" }, "Reading its SSH host key…"));
    try {
      scanned = await api("servers/scan", { method: "POST", body: { address: address.value.trim() } });
      trust.checked = false;
      keys.replaceChildren(
        h("p", { class: "field-label" }, "Host key fingerprints"),
        h("ul", { class: "plain-list fingerprints" }, scanned.fingerprints.map(f => h("li", null, h("code", null, f)))),
        h("p", { class: "hint" }, "On the server, ", h("code", null, "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub"), " prints its own. They must match."),
        h("label", { class: "check", for: confirmID }, trust, h("span", null, h("strong", null, "The fingerprint matches"), h("small", null, "OpenRDS will only connect to this key."))));
    } catch (err) { scanned = null; keys.replaceChildren(alertBox(err.message, "error")); }
    finally { b.disabled = false; sync(); }
  }, "small");
  address.addEventListener("input", () => { scanned = null; keys.replaceChildren(); });
  modal({
    title: "Add a server", kicker: "Servers", confirm: "Add server", wide: true,
    body: [
      h("ol", { class: "steps" },
        h("li", null, h("strong", null, "Prepare the server. "), "It needs Docker Engine 28 or newer. Run this there once: it creates an openrds user in the docker group that accepts the console’s key.", codeBlock(key.setup, { label: "Copy command" })),
        h("li", null, h("strong", null, "Connect. "), "The console reaches it over SSH, so port 22 (or the one you give) must be open to the console."),
      ),
      h("div", { class: "form-grid" }, field("Name", name, "Shown next to its databases."), field("SSH address", address, "user@host or user@host:port.")),
      h("div", { class: "row tight" }, check),
      keys,
      addresses.el,
      alertBox("Being in the docker group is like root on that server. Keep the console’s data volume private.", "info"),
    ],
    run: async () => {
      if (!scanned) throw new Error("Check the host key first.");
      if (!trust.checked) throw new Error("Confirm that the fingerprint matches.");
      const s = await api("servers", { method: "POST", body: { name: name.value.trim(), address: address.value.trim(), hostKeys: scanned.hostKeys,
        privateAddress: addresses.priv.value.trim(), publicAddress: addresses.pub.value.trim() } });
      toast(`${s.name} is ready for databases.`, "success");
      await sync();
    },
  });
}

function editServer(s) {
  const name = text({ value: s.name, maxlength: "40" });
  const addresses = addressFields(s);
  modal({
    title: `Edit ${s.name}`, kicker: "Servers", confirm: "Save", wide: true,
    body: [field("Name", name), addresses.el,
      h("p", { class: "muted" }, "Certificates name the new addresses after each database’s next restart.")],
    run: async () => {
      await api(`servers/${s.id}`, { method: "PUT", body: { name: name.value.trim(), privateAddress: addresses.priv.value.trim(), publicAddress: addresses.pub.value.trim() } });
      toast(`${name.value.trim()} saved.`, "success");
      await sync();
    },
  });
}

function removeServer(s) {
  modal({
    title: `Remove ${s.name}?`, kicker: "Servers", confirm: "Remove", tone: "danger", typed: s.name,
    body: [h("p", { class: "muted" }, "OpenRDS forgets the server and its host key. Nothing on the server changes. Remove the console’s key from its authorized_keys too.")],
    run: async () => {
      await api(`servers/${s.id}`, { method: "DELETE" });
      toast(`${s.name} removed.`, "success");
      await sync();
    },
  });
}

export function serverStatus(s) {
  if (s.reachable) return h("span", { class: "status good" }, icon("ok"), "Reachable");
  return h("span", { class: "status critical", title: s.problem || "" }, icon("error"), "Unreachable");
}

function capacity(s) {
  if (!s.info) return h("span", { class: "muted" }, "—");
  const total = s.info.memoryBytes / 1048576;
  return h("div", { class: "stack server-capacity" },
    h("span", null, `${s.info.cpus} vCPU · ${bytes(s.info.memoryBytes)}`),
    meter(s.allocatedMemoryMiB, total, `${s.allocatedMemoryMiB} MiB of ${Math.round(total)} MiB given to databases`),
    h("small", null, `${bytes(s.allocatedMemoryMiB * 1048576)} given to databases`));
}

function addressesCell(s) {
  const lines = [
    s.local ? h("span", null, "Console’s own server") : s.address ? h("code", null, s.address) : null,
    s.privateAddress ? h("small", null, "Private ", h("code", null, s.privateAddress)) : null,
    s.publicAddress ? h("small", null, "Public ", h("code", null, s.publicAddress)) : null,
  ].filter(Boolean);
  return h("span", { class: "stack" }, lines.length ? lines : h("span", { class: "muted" }, "—"));
}

function databasesCell(s) {
  if (!s.databases.length) return h("span", { class: "muted" }, "None");
  return h("span", { class: "stack" }, h("span", null, plural(s.databases.length, "database")),
    h("small", null, s.databases.slice(0, 3).flatMap((n, i) => [i ? ", " : "", h("a", { href: `#/databases/${n}` }, n)]), s.databases.length > 3 ? ` +${s.databases.length - 3}` : ""));
}

export function serversView() {
  const list = h("div", { class: "flush" });
  const add = can("admin") ? h("button", { type: "button", class: "btn primary", id: "add-server", onclick: addServer }, icon("plus"), "Add server") : null;
  const el = h("div", { class: "stack-gap" },
    h("div", { class: "page-head" }, h("div", null, h("h1", null, "Servers"), h("p", { class: "muted" }, "Where your databases run. The console manages each server’s Docker over SSH.")),
      add ? h("div", { class: "page-actions" }, add) : null),
    h("section", { class: "panel" }, list));

  function render() {
    const rows = state.servers;
    if (!rows) { list.replaceChildren(empty("host", "Checking servers…", "")); return; }
    list.replaceChildren(dataTable({ caption: "Servers", rows, columns: [
      { key: "name", label: "Server", value: s => h("div", { class: "db-cell" }, h("span", { class: "server-mark", "aria-hidden": "true" }, icon("host")),
        h("span", { class: "stack" }, h("strong", null, s.name, s.local ? h("span", { class: "chip neutral you-chip" }, "Console") : null, s.suggested && multiServer() ? h("span", { class: "chip info you-chip" }, "Most room") : null),
          h("small", null, s.info ? `${s.info.hostname} · Docker ${s.info.dockerVersion}` : s.problem ? s.problem.slice(0, 120) : ""))) },
      { key: "status", label: "Status", value: serverStatus },
      { key: "addresses", label: "Addresses", value: addressesCell },
      { key: "capacity", label: "Capacity", value: capacity },
      { key: "databases", label: "Databases", value: databasesCell },
      { key: "disk", label: "Disk limits", value: s => s.reachable ? chip(s.volumeQuota?.state === "supported" ? "Available" : "Unavailable", s.volumeQuota?.state === "supported" ? "good" : "neutral") : h("span", { class: "muted" }, "—") },
      { key: "actions", label: h("span", { class: "sr-only" }, "Actions"), cls: "actions", value: s => can("admin") ? menu("", [
        { label: "Edit", icon: "edit", run: () => editServer(s) },
        !s.local && { label: "Remove", icon: "trash", danger: true, disabled: s.databases.length > 0, run: () => removeServer(s) },
      ].filter(Boolean), { cls: "icon-only small ghost", labelIcon: "more", chevron: false, ariaLabel: `Actions for ${s.name}` }) : null },
    ] }), ...(rows.length === 1 && can("admin") ? [h("div", { class: "servers-hint" }, empty("host", "Run databases on more servers",
      "Add a server to spread databases across machines. Each needs Docker and SSH.", btn([icon("plus"), "Add server"], addServer, "primary")))] : []));
  }
  render();
  sync();
  const stop = subscribe(changed => { if (changed.has("servers") || changed.has("instances")) render(); });
  return { el, title: "Servers", nav: "servers", crumbs: [["Servers"]], destroy: stop };
}

export const serverLabel = (d) => serverOf(d)?.name || d.serverName || "This server";
export const lastChecked = (s) => s?.checkedAt ? ago(s.checkedAt) : "";
