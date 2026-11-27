// Servers: the machines your databases run on. One card per server, and a
// three-step wizard to add one.
import { h, icon, btn, api, modal, menu, bytes, plural, codeBlock, alertBox, nextId, toast, chip, copyText } from "./lib.mjs";
import { state, subscribe, sync, can } from "./store.mjs";

export const servers = () => state.servers || [];
export const multiServer = () => servers().length > 1;
export const serverOf = (d) => servers().find(s => s.id === (d.server || "local"));
export const serverLabel = (d) => serverOf(d)?.name || d.serverName || "This server";

// Memory not yet given to databases, in bytes.
export const roomOf = (s) => s.info ? Math.max(0, s.info.memoryBytes - s.allocatedMemoryMiB * 1048576) : 0;

function input(attrs) {
  return h("input", { type: "text", spellcheck: "false", autocomplete: "off", id: nextId("srv"), ...attrs });
}

function field(label, control, hint = "") {
  return h("div", { class: "field" }, h("label", { class: "field-label", for: control.id }, label), control, hint ? h("p", { class: "hint" }, hint) : null);
}

// ---------- adding a server: prepare, connect, verify

async function addServer() {
  let key;
  try { key = await api("servers/key"); }
  catch (err) { toast(err.message, "error"); return; }
  const name = input({ placeholder: "db-2", maxlength: "40" });
  const address = input({ placeholder: "openrds@203.0.113.10" });
  const priv = input({ placeholder: "Found automatically" });
  const pub = input({ placeholder: "Found automatically" });
  const trustID = nextId("trust");
  const trust = h("input", { type: "checkbox", id: trustID });
  const fingerprints = h("ul", { class: "fingerprints" });
  let step = 1, scanned = null, submit = null;

  const steps = ["Prepare", "Connect", "Verify"];
  const rail = h("ol", { class: "wizard-steps" });
  const pages = {
    1: h("div", { class: "wizard-page" },
      h("p", null, "Run this once on the new server. It creates a user that lets OpenRDS manage Docker there."),
      codeBlock(key.setup, { label: "Copy" }),
      h("p", { class: "hint" }, "The server needs Docker 28 or newer, and SSH open to this server.")),
    2: h("div", { class: "wizard-page" },
      h("div", { class: "form-grid" }, field("Name", name, "Anything you like."), field("SSH address", address, "user@host, or user@host:port.")),
      h("details", { class: "disclosure" }, h("summary", null, "Addresses (optional)"),
        h("div", { class: "form-grid" },
          field("Private address", priv, "On your private network, such as a VPC or Tailscale."),
          field("Public address", pub, "What apps on the internet use.")))),
    3: h("div", { class: "wizard-page" },
      h("p", null, "Make sure you’re connecting to the right server. On the server, run:"),
      codeBlock("ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub", { label: "Copy" }),
      h("p", null, "Its output must match:"),
      fingerprints,
      h("label", { class: "check", for: trustID }, trust, h("span", null, h("strong", null, "They match"), h("small", null, "OpenRDS will only connect to this key.")))),
  };
  const back = btn([icon("back"), "Back"], () => show(step - 1), "ghost small");
  const body = h("div", { class: "wizard" }, rail, ...Object.values(pages), h("div", { class: "wizard-back" }, back));
  function show(n) {
    step = n;
    rail.replaceChildren(...steps.map((label, i) => h("li", { class: i + 1 < n ? "done" : i + 1 === n ? "current" : "" },
      h("span", { class: "wizard-num" }, i + 1 < n ? icon("check") : String(i + 1)), label)));
    Object.entries(pages).forEach(([k, el]) => { el.hidden = Number(k) !== n; });
    back.hidden = n === 1;
    submit.textContent = { 1: "Next", 2: "Check key", 3: "Add server" }[n];
    (n === 2 ? name : n === 3 ? trust : submit).focus();
  }
  const dlg = modal({
    title: "Add a server", kicker: "Servers", confirm: "Next", wide: true, body: [body],
    run: async () => {
      if (step === 1) { show(2); return false; }
      if (step === 2) {
        if (!name.value.trim()) throw new Error("Give the server a name.");
        scanned = await api("servers/scan", { method: "POST", body: { address: address.value.trim() } });
        trust.checked = false;
        fingerprints.replaceChildren(...scanned.fingerprints.map(f => h("li", null, h("code", null, f))));
        show(3);
        return false;
      }
      if (!trust.checked) throw new Error("Check that the key matches, then tick “They match”.");
      const s = await api("servers", { method: "POST", body: { name: name.value.trim(), address: address.value.trim(), hostKeys: scanned.hostKeys,
        privateAddress: priv.value.trim(), publicAddress: pub.value.trim() } });
      toast(`${s.name} is ready. Choose it when you create or move a database.`, "success");
      await sync();
    },
  });
  submit = dlg.querySelector("button[type=submit]");
  show(1);
}

// ---------- changing and removing

function editServer(s) {
  const name = input({ value: s.name, maxlength: "40" });
  const priv = input({ value: s.privateAddress || "", placeholder: "10.0.0.5" });
  const pub = input({ value: s.publicAddress || "", placeholder: "203.0.113.10 or db2.example.com" });
  const find = btn([icon("search"), "Find automatically"], async (b) => {
    b.disabled = true;
    try {
      const found = await api(`servers/${s.id}/detect`, { method: "POST" });
      if (found.privateAddress && !priv.value) priv.value = found.privateAddress;
      if (found.publicAddress && !pub.value) pub.value = found.publicAddress;
      toast(found.privateAddress || found.publicAddress ? "Filled in what was found." : "Nothing found. Enter them yourself.", found.privateAddress ? "success" : "info");
    } catch (err) { toast(err.message, "error"); }
    finally { b.disabled = false; }
  }, "small");
  modal({
    title: `Edit ${s.name}`, kicker: "Servers", confirm: "Save", wide: true,
    body: [field("Name", name),
      h("div", { class: "form-grid" }, field("Private address", priv, "For private access and replicas."), field("Public address", pub, "For public access.")),
      h("div", { class: "row tight" }, find),
      h("p", { class: "hint" }, "Databases pick up new addresses at their next restart.")],
    run: async () => {
      await api(`servers/${s.id}`, { method: "PUT", body: { name: name.value.trim(), privateAddress: priv.value.trim(), publicAddress: pub.value.trim() } });
      toast("Saved.", "success");
      await sync();
    },
  });
}

function removeServer(s) {
  modal({
    title: `Remove ${s.name}?`, kicker: "Servers", confirm: "Remove", tone: "danger", typed: s.name,
    body: [h("p", null, "OpenRDS forgets this server. Nothing on it changes."),
      h("p", { class: "hint" }, "To lock OpenRDS out, also remove its key from the openrds user’s authorized_keys.")],
    run: async () => {
      await api(`servers/${s.id}`, { method: "DELETE" });
      toast(`${s.name} removed.`, "success");
      await sync();
    },
  });
}

// ---------- the page

export function serverStatus(s) {
  return s.reachable ? h("span", { class: "status good" }, icon("ok"), "Online") : h("span", { class: "status critical" }, icon("error"), "Offline");
}

function copyable(text) {
  return h("button", { type: "button", class: "endpoint", title: `Copy ${text}`, "aria-label": `Copy ${text}`,
    onclick: async () => { const ok = await copyText(text); toast(ok ? `Copied ${text}.` : "Copy failed.", ok ? "success" : "warn"); } },
  h("span", null, text), icon("copy"));
}

function memoryBar(s) {
  if (!s.info) return null;
  const used = s.allocatedMemoryMiB * 1048576, total = s.info.memoryBytes, share = Math.min(1, used / total);
  const fill = h("span", { class: "meter-fill" });
  fill.style.width = `${(share * 100).toFixed(1)}%`;
  return h("div", { class: "srv-memory" },
    h("div", { class: "srv-memory-head" }, h("span", null, "Memory for databases"), h("strong", null, `${bytes(used)} of ${bytes(total)}`)),
    h("span", { class: `meter ${share > 0.9 ? "critical" : share > 0.75 ? "warn" : "ok"}`, role: "img", "aria-label": `${bytes(used)} of ${bytes(total)} given to databases` }, fill));
}

function serverCard(s) {
  const actions = can("admin") ? menu("", [
    { label: "Edit", icon: "edit", run: () => editServer(s) },
    !s.local && { label: "Remove", icon: "trash", danger: true, disabled: s.databases.length > 0, reason: s.databases.length ? "Move or delete its databases first" : "", run: () => removeServer(s) },
  ].filter(Boolean), { cls: "icon-only small ghost", labelIcon: "more", chevron: false, ariaLabel: `Actions for ${s.name}` }) : null;
  const rows = [
    !s.local && s.address ? ["SSH", h("code", null, s.address)] : null,
    ["Private", s.privateAddress ? copyable(s.privateAddress) : h("span", { class: "muted" }, "Not set")],
    ["Public", s.publicAddress ? copyable(s.publicAddress) : h("span", { class: "muted" }, "Not set")],
  ].filter(Boolean);
  const dbs = s.databases;
  return h("article", { class: `srv-card${s.reachable ? "" : " down"}`, "aria-label": s.name },
    h("header", { class: "srv-head" },
      h("span", { class: "srv-icon", "aria-hidden": "true" }, icon("host")),
      h("div", { class: "stack" },
        h("h2", null, s.name, s.local ? h("span", { class: "chip neutral" }, "Console") : null),
        h("small", null, s.info ? `${s.info.cpus} vCPU · Docker ${s.info.dockerVersion}` : s.reachable ? "" : "Can’t connect over SSH")),
      h("div", { class: "srv-head-end" }, serverStatus(s), actions)),
    s.reachable ? memoryBar(s) : alertBox((s.problem || "No answer.").slice(0, 200), "error"),
    h("dl", { class: "srv-rows" }, rows.map(([k, v]) => h("div", null, h("dt", null, k), h("dd", null, v)))),
    h("footer", { class: "srv-foot" },
      dbs.length ? h("div", { class: "srv-dbs" }, dbs.slice(0, 4).map(n => h("a", { class: "tag-chip", href: `#/databases/${n}` }, n)),
        dbs.length > 4 ? h("span", { class: "tag-chip more" }, `+${dbs.length - 4}`) : null)
        : h("span", { class: "muted" }, "No databases yet"),
      s.reachable ? chip(s.volumeQuota?.state === "supported" ? "Disk limits" : "No disk limits", s.volumeQuota?.state === "supported" ? "good" : "neutral") : null));
}

function addCard() {
  return h("button", { type: "button", class: "srv-card srv-add", id: "add-server", onclick: addServer },
    h("span", { class: "srv-add-icon" }, icon("plus")),
    h("strong", null, "Add a server"),
    h("small", null, "Run databases on more machines."));
}

// Why add servers, for someone with just one.
function intro() {
  return h("section", { class: "panel srv-intro" },
    h("h2", null, "Why add servers?"),
    h("ul", { class: "srv-reasons" },
      h("li", null, icon("db"), h("span", null, h("strong", null, "More room. "), "Spread databases over machines.")),
      h("li", null, icon("copy"), h("span", null, h("strong", null, "Safer data. "), "Keep a read replica on another machine.")),
      h("li", null, icon("arrow"), h("span", null, h("strong", null, "Easy moves. "), "Move a database in a few clicks."))));
}

export function serversView() {
  const grid = h("div", { class: "srv-grid" });
  const summary = h("p", { class: "muted fleet-summary" });
  const head = h("div", { class: "page-head" }, h("div", null, h("h1", null, "Servers"), summary),
    can("admin") ? h("div", { class: "page-actions" }, h("button", { type: "button", class: "btn primary", onclick: addServer }, icon("plus"), "Add server")) : null);
  const top = h("div");
  const el = h("div", { class: "stack-gap" }, head, top, grid);

  function render() {
    const list = state.servers;
    if (!list) { summary.textContent = "Checking servers…"; return; }
    const online = list.filter(s => s.reachable).length, dbs = list.reduce((n, s) => n + s.databases.length, 0);
    summary.textContent = `${plural(list.length, "server")} · ${online} online · ${plural(dbs, "database")}`;
    top.replaceChildren(list.length === 1 && can("admin") ? intro() : "");
    grid.replaceChildren(...list.map(serverCard), ...(can("admin") ? [addCard()] : []));
  }
  render();
  sync();
  const stop = subscribe(changed => { if (changed.has("servers") || changed.has("instances")) render(); });
  return { el, title: "Servers", nav: "servers", crumbs: [["Servers"]], destroy: stop };
}
