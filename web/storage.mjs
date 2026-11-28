// Backup storage: a copy of every backup in S3-compatible object storage,
// and a way to bring copies back after losing a server.
import { h, icon, btn, api, toast, modal, panel, infoPanel, dataTable, empty, alertBox, when, ago, bytes, nextId, menu, setAlert, pager } from "./lib.mjs";
import { state, subscribe, sync, can, engine, track } from "./store.mjs";

const PROVIDERS = [
  { value: "aws", title: "Amazon S3", endpoint: "https://s3.us-east-1.amazonaws.com", region: "us-east-1", pathStyle: false, hint: "Use your bucket’s region, for example https://s3.eu-west-1.amazonaws.com." },
  { value: "r2", title: "Cloudflare R2", endpoint: "https://ACCOUNT_ID.r2.cloudflarestorage.com", region: "auto", pathStyle: true, hint: "Replace ACCOUNT_ID with your Cloudflare account ID." },
  { value: "b2", title: "Backblaze B2", endpoint: "https://s3.us-west-004.backblazeb2.com", region: "us-west-004", pathStyle: true, hint: "Use the S3 endpoint shown on your bucket’s page." },
  { value: "wasabi", title: "Wasabi", endpoint: "https://s3.us-east-1.wasabisys.com", region: "us-east-1", pathStyle: true, hint: "Use your bucket’s region." },
  { value: "other", title: "MinIO or other", endpoint: "", region: "us-east-1", pathStyle: true, hint: "Any S3-compatible service." },
];
const providerOf = (key) => PROVIDERS.find(p => p.value === key) || PROVIDERS.at(-1);

// A copy's state, for the backup tables.
export function storageCell(b) {
  if (b.offsite?.status === "copied") return h("span", { class: "status good", title: `Copied ${when(b.offsite.at)}` }, icon("ok"), "Copied");
  if (b.offsite?.status === "failed") return h("span", { class: "status critical", title: b.offsite.error }, icon("error"), "Failed");
  return h("span", { class: "status neutral" }, icon("pending"), "Waiting");
}

function field(label, input, hint = "") {
  input.id ||= nextId("st");
  const hintEl = h("p", { class: "hint" }, hint);
  const el = h("div", { class: "field" }, h("label", { class: "field-label", for: input.id }, label), input, hintEl);
  el.hint = hintEl;
  return el;
}

// The connection form, filled from the current settings when editing.
function storageForm(current = {}) {
  let provider = current.provider || "aws";
  const p0 = providerOf(provider);
  const endpoint = h("input", { type: "url", value: current.endpoint || p0.endpoint, placeholder: "https://…", spellcheck: "false" });
  const region = h("input", { type: "text", value: current.region || p0.region, spellcheck: "false" });
  const bucket = h("input", { type: "text", value: current.bucket || "", placeholder: "my-backups", spellcheck: "false" });
  const prefix = h("input", { type: "text", value: current.prefix ?? "openrds/", placeholder: "openrds/", spellcheck: "false" });
  const access = h("input", { type: "text", value: current.accessKeyId || "", autocomplete: "off", spellcheck: "false" });
  const secret = h("input", { type: "password", autocomplete: "new-password", placeholder: current.enabled ? "Leave empty to keep the saved key" : "" });
  const pathStyle = h("input", { type: "checkbox", class: "switch", role: "switch", checked: current.enabled ? current.pathStyle : p0.pathStyle, id: nextId("ps") });
  const endpointField = field("Endpoint URL", endpoint, p0.hint);
  const id = nextId("provider");
  const cards = h("div", { class: "cards cols-5 provider-cards", role: "radiogroup", "aria-label": "Provider" }, PROVIDERS.map(p => {
    const input = h("input", { type: "radio", name: id, value: p.value, checked: p.value === provider, id: `${id}-${p.value}` });
    input.addEventListener("change", () => {
      const before = providerOf(provider);
      provider = p.value;
      // Replace the defaults of the previous provider, never a typed value.
      if (!endpoint.value || endpoint.value === before.endpoint) endpoint.value = p.endpoint;
      if (!region.value || region.value === before.region) region.value = p.region;
      pathStyle.checked = p.pathStyle;
      endpointField.hint.textContent = p.hint;
    });
    return h("label", { class: "card-choice", for: input.id }, input, h("span", { class: "card-body" }, h("strong", null, p.title)));
  }));
  const el = h("div", { class: "storage-form" },
    h("span", { class: "field-label" }, "Provider"), cards,
    h("div", { class: "form-grid" }, endpointField, field("Region", region, "auto for Cloudflare R2.")),
    h("div", { class: "form-grid" }, field("Bucket", bucket, "Create it first. Keep it private."), field("Folder", prefix, "Copies go under this folder.")),
    h("div", { class: "form-grid" }, field("Access key ID", access), field("Secret access key", secret, "Needs read, write, list and delete on the bucket.")),
    h("label", { class: "check", for: pathStyle.id }, pathStyle, h("span", null, h("strong", null, "Path-style URLs"), h("small", null, "Needed by most services other than Amazon S3."))));
  const read = () => ({ provider, endpoint: endpoint.value.trim(), region: region.value.trim(), bucket: bucket.value.trim(), prefix: prefix.value.trim(),
    accessKeyId: access.value.trim(), secretAccessKey: secret.value, pathStyle: pathStyle.checked });
  return { el, read };
}

function testButton(form, note) {
  return btn([icon("refresh"), "Test connection"], async (b) => {
    b.disabled = true;
    setAlert(note, "Testing…", "info");
    try {
      await api("storage/test", { method: "POST", body: form.read() });
      setAlert(note, "Connected. OpenRDS can write, read and delete in this bucket.", "success");
    } catch (err) { setAlert(note, err.message, "error"); }
    finally { b.disabled = false; }
  });
}

function editStorage(current) {
  const form = storageForm(current);
  const note = h("div", { class: "alert", role: "status", hidden: true });
  modal({
    title: "Edit backup storage", confirm: "Save", wide: true,
    body: [form.el, h("div", { class: "row" }, testButton(form, note)), note],
    run: async () => {
      await api("storage", { method: "PUT", body: form.read() });
      toast("Backup storage saved.", "success");
      await sync();
    },
  });
}

function disconnect() {
  modal({
    title: "Disconnect backup storage?", confirm: "Disconnect", tone: "danger",
    body: h("p", { class: "muted" }, "New backups stay on this server only. Copies already in the bucket are kept."),
    run: async () => {
      await api("storage", { method: "DELETE" });
      toast("Backup storage disconnected.", "success");
      await sync();
    },
  });
}

function setupPanel() {
  const form = storageForm();
  const note = h("div", { class: "alert", role: "status", hidden: true });
  const save = btn("Save and start copying", async (b) => {
    b.disabled = true;
    setAlert(note, "Checking the bucket…", "info");
    try {
      await api("storage", { method: "PUT", body: form.read() });
      toast("Backup storage connected. Copying backups now.", "success");
      await sync();
    } catch (err) { setAlert(note, err.message, "error"); b.disabled = false; }
  }, "primary");
  return panel("Connect storage", { description: "Every backup gets a copy in your bucket. If this server is lost, a new one can bring them back." },
    form.el, note, h("div", { class: "row" }, testButton(form, note), save));
}

const PAGE = 50;

function remotePanel(onChange) {
  const body = h("div", { class: "flush" }, empty("cloud", "Loading…", ""));
  const refresh = btn([icon("refresh"), "Refresh"], () => load(), "small");
  let list = [], from = 0;
  async function load() {
    try {
      list = await api("storage/backups");
      from = Math.min(from, Math.max(0, Math.floor((list.length - 1) / PAGE) * PAGE));
      show();
    } catch (err) { body.replaceChildren(alertBox(err.message, "error")); }
  }
  // The bucket can hold many copies; the table shows a page of them.
  function show() {
    const to = Math.min(from + PAGE, list.length);
    const turn = (step) => { from = Math.max(0, from + step * PAGE); show(); };
    body.replaceChildren(list.length ? dataTable({ caption: "Backups in storage", rows: list.slice(from, to), columns: [
      { key: "created", label: "Backup", value: b => h("div", { class: "stack" }, h("strong", null, when(b.createdAt)), h("small", null, ago(b.createdAt))) },
      { key: "name", label: "Database", value: b => h("div", { class: "stack" }, h("span", null, b.name), h("small", null, `${engine(b.engine)?.title || b.engine} ${b.version || ""}`.trim())) },
      { key: "size", label: "Size", cls: "num", value: b => bytes(b.bytes) },
      { key: "where", label: "Location", value: b => b.local ? h("span", { class: "status good" }, icon("ok"), "Also on this server") : h("span", { class: "status info" }, icon("cloud"), "Storage only") },
      { key: "actions", label: h("span", { class: "sr-only" }, "Actions"), cls: "actions", value: b => b.local ? null : h("div", { class: "cell-actions" },
        btn([icon("download"), "Import"], () => importBackup(b), "small"),
        menu("", [{ label: "Delete from storage", icon: "trash", danger: true, run: () => deleteRemote(b) }],
          { cls: "small ghost icon-only", labelIcon: "more", chevron: false, ariaLabel: `More actions for the ${when(b.createdAt)} backup of ${b.name}` })) },
    ] }) : empty("cloud", "No backups in storage yet", "Copies appear here as they finish."),
      pager({ from, to, total: list.length, label: "Storage pages", onPrev: () => turn(-1), onNext: () => turn(1) }));
  }
  function importBackup(b) {
    track(`Import ${b.name} backup`, api(`storage/backups/${b.id}/import`, { method: "POST" }), {
      success: "Imported. Restore it from Snapshots.",
      onDone: () => { load(); onChange(); },
    });
  }
  function deleteRemote(b) {
    modal({
      title: "Delete this copy?", kicker: b.name, confirm: "Delete", tone: "danger",
      body: h("p", { class: "muted" }, `The ${when(b.createdAt)} backup is removed from the bucket for good.`),
      run: async () => {
        await api(`storage/backups/${b.id}`, { method: "DELETE" });
        toast("Deleted from storage.", "success");
        load();
      },
    });
  }
  load();
  return panel("Backups in storage", { description: "Including copies from other servers. Import one to restore it here.", actions: [refresh] }, body);
}

export function storageView() {
  const body = h("div", { class: "stack-gap" });
  const el = h("div", null,
    h("div", { class: "page-head" }, h("div", null, h("h1", null, "Backup storage"), h("p", { class: "muted" }, "Keep a copy of every backup off this server."))),
    body);
  let lastSig = "";

  function render() {
    const s = state.storage;
    const sig = JSON.stringify([s, state.me?.role]);
    if (sig === lastSig) return;
    lastSig = sig;
    if (!s) { body.replaceChildren(panel("Backup storage", {}, empty("cloud", "Loading…", ""))); return; }
    if (!s.enabled) {
      body.replaceChildren(
        h("ul", { class: "how-it-works storage-points" }, [
          ["cloud", "A copy of every backup", "Copied automatically after each backup."],
          ["history", "Same retention", "Copies are removed when backups expire."],
          ["restore", "Restore anywhere", "A new server can import them."],
        ].map(([symbol, title, text]) => h("li", null, h("span", { class: "how-icon" }, icon(symbol)), h("span", null, h("strong", null, title), h("span", { class: "muted" }, text))))),
        can("admin") ? setupPanel() : panel("Backup storage", {}, empty("cloud", "Not connected", "Ask an admin to connect S3-compatible storage.")));
      return;
    }
    const total = s.copied + s.pending + s.failed;
    const provider = providerOf(s.provider);
    body.replaceChildren(...[
      s.lastError ? alertBox(`Some copies failed: ${s.lastError}`, "error") : null,
      infoPanel("Backup storage", [
        ["Destination", [["Provider", provider.title], ["Bucket", h("code", null, s.bucket)], ["Folder", s.prefix ? h("code", null, s.prefix) : "Bucket root"], ["Endpoint", h("code", null, s.endpoint)]]],
        ["Copies", [["Copied", `${s.copied} of ${total}`], ["Waiting", String(s.pending)],
          ["Failed", s.failed ? h("span", { class: "status critical" }, icon("error"), String(s.failed)) : "0"]]],
        ["Sync", [["Status", s.pending || s.failed ? h("span", { class: "status info" }, icon("pending"), "Copying") : h("span", { class: "status good" }, icon("ok"), "Up to date")],
          ["Last check", s.lastSyncAt ? ago(s.lastSyncAt) : "Not yet"], ["Access key ID", s.accessKeyId ? h("code", null, s.accessKeyId) : "—"]]],
      ], { actions: can("admin") ? [btn([icon("edit"), "Edit"], () => editStorage(s), "small"), btn("Disconnect", disconnect, "small")] : [] }),
      can("operator") ? remotePanel(() => sync()) : null,
    ].filter(Boolean));
  }
  render();
  const stop = subscribe((changed) => { if (changed.has("storage") || changed.has("backups")) render(); });
  return { el, title: "Backup storage", nav: "storage", crumbs: [["Backup storage"]], destroy: stop };
}
