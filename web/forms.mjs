// Create, restore and modify pages: numbered sections like a console wizard,
// with a summary that shows what the request will do to the host.
import { h, icon, btn, api, alertBox, setAlert, mib, nextId, windowLabel, scheduleLabel, plural, bytes, keyValues, isLive, modal, toast, help } from "./lib.mjs";
import { state, loadEngines, engine, instance, track, sync, go } from "./store.mjs";

const SIZES = [
  { value: "micro", title: "Micro", cpus: 0.5, memoryMiB: 512 },
  { value: "small", title: "Small", cpus: 1, memoryMiB: 1024 },
  { value: "medium", title: "Medium", cpus: 2, memoryMiB: 4096 },
  { value: "large", title: "Large", cpus: 4, memoryMiB: 8192 },
  { value: "xlarge", title: "XLarge", cpus: 8, memoryMiB: 16384 },
];
const ENGINE_TEXT = {
  postgres: "Powerful SQL with JSON and extensions.",
  mysql: "The popular SQL database. LTS release.",
  valkey: "Fast key-value store. Redis compatible.",
};
const TEMPLATES = {
  production: { title: "Production", detail: "7-day backups, deletion protection, 2 vCPU · 4 GiB.", size: "medium", retention: 7, protection: true },
  dev: { title: "Dev and test", detail: "1-day backups, 0.5 vCPU · 512 MiB.", size: "micro", retention: 1, protection: false },
};
const RETENTION = [0, 1, 2, 3, 5, 7, 10, 14, 21, 30, 35];
const LENGTHS = [30, 60, 120, 180];
const INTERVALS = [24, 12, 6, 4, 3, 2, 1];
const frequencyLabel = (n) => !n || n === 24 ? "Once a day" : n === 1 ? "Every hour" : `Every ${n} hours`;

// How often automated backups run, and what that keeps on disk.
function frequencyField(value, retention) {
  const control = select(INTERVALS.map(n => [n, frequencyLabel(n)]), value || 24);
  const wrap = field("Frequency", control, { hint: " ", explain: "More frequent backups lose less data, and each is a full copy, so they use more disk." });
  const hint = wrap.querySelector(".hint");
  wrap.update = () => {
    const days = Number(retention.value), hours = Number(control.value);
    hint.textContent = hours === 24 ? "Starts in the window." : `Repeats from the window start. Keeps about ${plural(Math.round(days * 24 / hours), "backup")}.`;
  };
  [control, retention].forEach(c => c.addEventListener("change", wrap.update));
  wrap.update();
  wrap.control = control;
  return wrap;
}
const namePattern = /^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$/;
// "PostgreSQL 18 · Stable (recommended)", "MySQL 9.7 LTS · Latest".
const versionLabel = (eng, x) => `${eng.title} ${x.version}${x.lts ? " LTS" : ""}${x.status === "latest" ? " · Latest" : x.status === "stable" ? " · Stable (recommended)" : ""}`;
const newerOrSame = (a, b) => {
  const pa = a.split(".").map(Number), pb = b.split(".").map(Number);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) if ((pa[i] || 0) !== (pb[i] || 0)) return (pa[i] || 0) > (pb[i] || 0);
  return true;
};
const delay = (ms) => new Promise(resolve => setTimeout(resolve, ms));
const hhmm = (minutes) => `${String(Math.floor(minutes / 60) % 24).padStart(2, "0")}:${String(minutes % 60).padStart(2, "0")}`;

export const sizeName = (cpus, memoryMiB) => {
  if (!cpus && !memoryMiB) return "shared";
  return SIZES.find(z => z.cpus === cpus && z.memoryMiB === memoryMiB)?.value || "custom";
};

function parseWindow(w) {
  const m = /^(\d\d):(\d\d)-(\d\d):(\d\d)$/.exec(w || "");
  if (!m) return { start: 180, length: 30 };
  const start = +m[1] * 60 + +m[2], end = +m[3] * 60 + +m[4];
  return { start, length: (end - start + 1440) % 1440 || 30 };
}
const formatWindow = (start, length) => `${hhmm(start)}-${hhmm(start + length)}`;

// Stagger new windows so databases on one host do not all back up at once.
function freeWindow() {
  const used = new Set((state.instances || []).filter(d => d.backupRetentionDays > 0).map(d => parseWindow(d.backupWindow).start));
  for (let start = 120; start < 120 + 8 * 60; start += 30) if (!used.has(start % 1440)) return start % 1440;
  return 180;
}

function suggestPort(key, taken = new Set((state.instances || []).map(d => d.port))) {
  let port = engine(key)?.defaultPort || 15432;
  while (taken.has(port)) port++;
  return port;
}

// ---------- building blocks

function field(label, control, { hint = "", optional = false, explain = "" } = {}) {
  if (!control.id) control.id = nextId("field");
  const id = control.id;
  const error = h("p", { class: "field-error", id: `${id}-error`, hidden: true });
  const hintEl = hint ? h("p", { class: "hint", id: `${id}-hint` }, hint) : null;
  control.setAttribute("aria-describedby", [hintEl && `${id}-hint`, `${id}-error`].filter(Boolean).join(" "));
  const labelEl = h("label", { for: id, class: "field-label" }, label, optional && h("span", { class: "optional" }, " · optional"));
  const wrap = h("div", { class: "field" }, explain ? h("div", { class: "field-head" }, labelEl, help(explain, `About ${label}`)) : labelEl, control, hintEl, error);
  wrap.setError = (msg) => { error.textContent = msg || ""; error.hidden = !msg; control.setAttribute("aria-invalid", msg ? "true" : "false"); };
  wrap.control = control;
  return wrap;
}

function choiceCards(name, options, value, onChange, { label, cls = "" } = {}) {
  const group = h("div", { class: `cards ${cls}`.trim(), role: "radiogroup", "aria-label": label });
  const inputs = options.map(o => {
    const input = h("input", { type: "radio", name, value: o.value, checked: o.value === value, disabled: o.disabled, id: nextId(name) });
    input.addEventListener("change", () => onChange(o.value));
    group.append(h("label", { class: "card-choice", for: input.id }, input,
      h("span", { class: "card-body" }, o.badge && h("span", { class: `engine-badge ${o.value}` }, o.badge),
        h("strong", null, o.title), o.detail && h("small", null, o.detail), o.disabled && o.reason && h("small", { class: "reason" }, o.reason))));
    return input;
  });
  group.set = (v) => inputs.forEach(i => { i.checked = i.value === v; });
  group.disable = (fn) => inputs.forEach(i => { i.disabled = fn(i.value); i.closest(".card-choice").classList.toggle("disabled", i.disabled); });
  return group;
}

function section(n, title, description, ...content) {
  return h("section", { class: "panel form-section" }, h("header", { class: "panel-head" },
    h("div", null, h("h2", null, h("span", { class: "step" }, String(n)), title), description && h("p", { class: "muted" }, description))),
  h("div", { class: "panel-body" }, ...content));
}

const select = (options, value, attrs = {}) => h("select", attrs, options.map(([v, label]) => h("option", { value: String(v), selected: String(v) === String(value) }, label)));

// Editor for an engine's allowlisted parameters. Values are strings in the
// engine's own syntax; the server validates them again before use. `tuned`
// holds the memory settings the server sized to a database's memory limit.
export function parameterEditor(eng, initial = {}, tuned = {}) {
  const fallback = (p) => tuned?.[p.name] ?? p.default;
  const defaultCell = (p) => tuned?.[p.name]
    ? [h("code", { class: "muted" }, tuned[p.name]), h("small", null, "Sized to the memory limit")]
    : [h("code", { class: "muted" }, p.default === "" ? "(empty)" : p.default), p.auto ? h("small", null, `With a memory limit: ${p.auto}`) : null];
  const rows = new Map();
  const body = h("tbody");
  const table = h("table", { class: "param-table" }, h("thead", null, h("tr", null, h("th", { scope: "col" }, "Parameter"), h("th", { scope: "col" }, "Value"), h("th", { scope: "col" }, "Default"), h("th", { scope: "col" }, h("span", { class: "sr-only" }, "Remove")))), body);
  const picker = h("select", { "aria-label": "Parameter to add" });
  const add = btn([icon("plus"), "Add parameter"], () => { if (picker.value) { addRow(picker.value); refreshPicker(); rows.get(picker.dataset.last)?.focus(); } }, "small");
  const emptyNote = h("p", { class: "muted", hidden: true }, "Using engine defaults.");
  const el = h("div", { class: "param-editor" }, emptyNote, h("div", { class: "table-wrap" }, table), h("div", { class: "row" }, picker, add));
  const changed = new Set();
  const notify = () => changed.forEach(fn => fn());

  function control(p, value) {
    const label = `${p.name} value`;
    if (p.type === "enum") return { node: select(p.values.map(v => [v, v]), value ?? fallback(p), { "aria-label": label }), read: (n) => n.value };
    if (p.type === "list") {
      const set = new Set((value ?? fallback(p)).split(",").filter(Boolean));
      const boxes = p.values.map(v => h("label", { class: "check" }, h("input", { type: "checkbox", value: v, checked: set.has(v) }), v));
      return { node: h("fieldset", { class: "checks" }, h("legend", { class: "sr-only" }, label), boxes), read: (n) => [...n.querySelectorAll("input:checked")].map(i => i.value).join(",") };
    }
    if (p.type === "size") {
      const units = eng.sizeUnits;
      const m = /^(\d+)(\D+)$/.exec(value ?? fallback(p)) || [null, "", units[1]];
      const num = h("input", { type: "number", min: "1", step: "1", value: m[1], "aria-label": label, class: "num" });
      const unit = select(units.map(u => [u, u]), m[2], { "aria-label": `${p.name} unit` });
      return { node: h("span", { class: "combo" }, num, unit), read: () => `${num.value}${unit.value}` };
    }
    if (p.type === "integer" || p.type === "real") {
      const input = h("input", { type: "number", min: String(p.min), max: String(p.max), step: p.type === "real" ? "any" : "1", value: value ?? fallback(p), "aria-label": label, class: "num" });
      return { node: h("span", { class: "combo" }, input, p.unit && h("span", { class: "unit" }, p.unit)), read: () => input.value.trim() };
    }
    const input = h("input", { type: "text", value: value ?? fallback(p), "aria-label": label, spellcheck: "false", autocomplete: "off", placeholder: p.type === "schedule" ? "3600 1 300 100" : "Ex" });
    return { node: input, read: () => input.value };
  }

  function addRow(name, value) {
    const p = eng.parameters.find(x => x.name === name);
    if (!p || rows.has(name)) return;
    const c = control(p, value);
    const remove = h("button", { type: "button", class: "icon-btn ghost", "aria-label": `Remove ${name}`, onclick: () => { rows.delete(name); tr.remove(); refreshPicker(); notify(); } }, icon("trash"));
    const tr = h("tr", null, h("td", null, h("code", null, p.name), h("small", null, p.description)), h("td", null, c.node), h("td", null, ...defaultCell(p), p.unit && p.type !== "size" ? h("span", { class: "muted" }, ` ${p.unit}`) : null), h("td", null, remove));
    c.node.addEventListener("input", notify);
    c.node.addEventListener("change", notify);
    rows.set(name, { read: () => c.read(c.node), focus: () => c.node.querySelector?.("input,select")?.focus() || c.node.focus?.() });
    picker.dataset.last = name;
    body.append(tr);
    notify();
  }

  function refreshPicker() {
    const free = eng.parameters.filter(p => !rows.has(p.name)).sort((a, b) => a.name.localeCompare(b.name));
    picker.replaceChildren(...free.map(p => h("option", { value: p.name }, p.name)));
    picker.disabled = add.disabled = !free.length;
    table.closest(".table-wrap").hidden = !rows.size;
    emptyNote.hidden = rows.size > 0;
  }
  Object.entries(initial || {}).sort().forEach(([k, v]) => addRow(k, v));
  refreshPicker();
  return {
    el,
    value: () => Object.fromEntries([...rows].map(([k, r]) => [k, r.read()])),
    onChange: (fn) => changed.add(fn),
  };
}

export function tagEditor(initial = {}) {
  const list = h("div", { class: "tag-rows" });
  const changed = new Set();
  const notify = () => changed.forEach(fn => fn());
  const addBtn = btn([icon("plus"), "Add tag"], () => { addRow("", ""); list.lastElementChild?.querySelector("input")?.focus(); }, "small");
  function addRow(key, value) {
    if (list.children.length >= 50) return;
    const k = h("input", { type: "text", value: key, maxlength: "128", "aria-label": "Tag key", placeholder: "Key", spellcheck: "false", autocomplete: "off" });
    const v = h("input", { type: "text", value, maxlength: "256", "aria-label": "Tag value", placeholder: "Value (optional)", spellcheck: "false", autocomplete: "off" });
    const row = h("div", { class: "tag-row" }, k, v, h("button", { type: "button", class: "icon-btn ghost", "aria-label": "Remove tag", onclick: () => { row.remove(); notify(); } }, icon("trash")));
    k.addEventListener("input", notify); v.addEventListener("input", notify);
    list.append(row);
    notify();
  }
  Object.entries(initial || {}).sort().forEach(([k, v]) => addRow(k, v));
  return {
    el: h("div", { class: "tag-editor" }, list, addBtn),
    value: () => Object.fromEntries([...list.children].map(r => [...r.querySelectorAll("input")].map(i => i.value.trim())).filter(([k]) => k)),
    onChange: (fn) => changed.add(fn),
  };
}

// ---------- the shared form

function databaseForm({ mode, eng, initial, source }) {
  const host = state.host;
  const others = (state.instances || []).filter(d => d.name !== initial.name);
  const taken = new Set(others.map(d => d.port));
  const v = { ...initial };
  const note = h("div", { class: "alert error", role: "alert", hidden: true });
  const fields = {};
  const sections = [];
  const changes = new Set();
  const changed = () => changes.forEach(fn => fn());
  let n = 0;

  // The engine version: chosen when creating; same or newer when restoring.
  const versionSelect = h("select", { id: nextId("version") });
  versionSelect.addEventListener("change", () => { v.version = versionSelect.value; changed(); });
  function fillVersions() {
    const floor = mode === "restore" ? source.version : null;
    const physical = mode === "restore" && source.format === "physical"; // a base backup starts only its own version
    const list = eng.versions.filter(x => physical ? x.version === floor : !floor || newerOrSame(x.version, floor));
    if (!list.some(x => x.version === v.version)) v.version = (list.find(x => x.version === floor) || list.find(x => x.status === "stable") || list[0])?.version;
    versionSelect.replaceChildren(...list.map(x => h("option", { value: x.version, selected: x.version === v.version }, versionLabel(eng, x))));
  }
  fillVersions();
  const versionField = () => field("Engine version", versionSelect, { hint: mode === "restore"
    ? source.format === "physical" ? "A base backup restores into its own version. Upgrade the new database afterwards if you need a newer one." : "The same version or newer. Backups can’t move to an older version."
    : "Stable is the safe choice. Upgrade it in place later from Configuration." });

  // 1. Engine and template (create only)
  let engineCards = null, templateCards = null;
  if (mode === "create") {
    engineCards = choiceCards("engine", state.engines.map(e => ({ value: e.key, title: e.title, detail: ENGINE_TEXT[e.key], badge: { postgres: "PG", mysql: "My", valkey: "Vk" }[e.key] })),
      v.engine, (key) => { v.engine = key; eng = engine(key); v.version = eng.defaultVersion; fillVersions(); fields.port.control.value = suggestPort(key, taken); v.port = Number(fields.port.control.value); rebuildParams(); updateSizes(); changed(); }, { label: "Engine", cls: "cols-3" });
    templateCards = choiceCards("template", Object.entries(TEMPLATES).map(([value, t]) => ({ value, title: t.title, detail: t.detail })), v.template, applyTemplate, { label: "Template", cls: "cols-2" });
    sections.push(section(++n, "Engine options", "Each database gets its own container and volume.", engineCards, versionField()),
      section(++n, "Templates", "Good defaults. Change anything below.", templateCards));
  } else if (mode === "restore") {
    sections.push(section(++n, "Engine", null, keyValues([["Engine", eng.title], ["Source snapshot", `${source.name} · ${source.version ? `${eng.title} ${source.version} · ` : ""}${new Date(source.createdAt).toLocaleString()}`]]), versionField()));
  } else {
    sections.push(section(++n, "Engine", null, keyValues([["Engine", `${eng.title} ${initial.version}`], ["DB identifier", initial.name]]),
      h("p", { class: "muted" }, "To upgrade, take a snapshot and restore it into a new database on a newer version.")));
  }

  // Settings
  if (mode !== "modify") {
    const name = h("input", { type: "text", value: v.name || "", maxlength: "40", autocomplete: "off", spellcheck: "false", placeholder: "orders-prod", required: true });
    name.addEventListener("input", () => { v.name = name.value.trim(); fields.name.setError(""); changed(); });
    fields.name = field("DB instance identifier", name, { hint: "Lowercase letters, digits and hyphens." });
    sections.push(section(++n, "Settings", null, fields.name,
      alertBox(h("span", null, "We create the credentials: user ", h("code", null, eng.user), ", database ", h("code", null, eng.database), "."), "info")));
  }

  // Instance configuration
  const fits = (z) => !host || (z.cpus <= host.cpus && z.memoryMiB * 1048576 <= host.memoryBytes);
  const sizeOptions = () => [...SIZES.map(z => ({ value: z.value, title: z.title, detail: `${z.cpus} vCPU · ${mib(z.memoryMiB)}`, disabled: !fits(z) || z.memoryMiB < eng.minMemoryMiB, reason: !fits(z) ? "Larger than this host" : "Below the engine minimum" })),
    { value: "custom", title: "Custom", detail: "Choose CPU and memory limits." },
    { value: "shared", title: "Shared", detail: "No limits. Shares the whole host." }];
  const sizeCards = choiceCards("size", sizeOptions(), v.size, (key) => { v.size = key; applySize(); changed(); }, { label: "Instance size", cls: "cols-4" });
  const cpus = h("input", { type: "number", min: "0.1", max: "1024", step: "0.1", value: v.cpus || "", placeholder: "Unlimited" });
  const memory = h("input", { type: "number", min: String(eng.minMemoryMiB), step: "1", value: v.memoryMiB || "", placeholder: "Unlimited" });
  fields.cpus = field("CPU limit (vCPU)", cpus, { hint: host ? `This host has ${host.cpus} vCPU.` : "" });
  fields.memory = field("Memory limit (MiB)", memory, { hint: host ? `This host has ${bytes(host.memoryBytes)}.` : "" });
  const custom = h("div", { class: "form-grid" }, fields.cpus, fields.memory);
  cpus.addEventListener("input", () => { v.cpus = Number(cpus.value) || 0; fields.cpus.setError(""); changed(); });
  memory.addEventListener("input", () => { v.memoryMiB = Number(memory.value) || 0; fields.memory.setError(""); changed(); });
  function applySize() {
    const z = SIZES.find(x => x.value === v.size);
    if (z) { v.cpus = z.cpus; v.memoryMiB = z.memoryMiB; }
    if (v.size === "shared") { v.cpus = 0; v.memoryMiB = 0; }
    cpus.value = v.cpus || ""; memory.value = v.memoryMiB || "";
    custom.hidden = v.size !== "custom";
  }
  function updateSizes() {
    memory.min = String(eng.minMemoryMiB);
    sizeCards.disable(key => { const z = SIZES.find(x => x.value === key); return Boolean(z) && (!fits(z) || z.memoryMiB < eng.minMemoryMiB); });
    if (sizeCards.querySelector("input:checked")?.disabled) { v.size = "custom"; sizeCards.set("custom"); applySize(); }
  }
  applySize();
  updateSizes();
  sections.push(section(++n, "Instance configuration", "Not sure? Start small. You can resize later.", sizeCards, custom));

  // Storage
  if (mode !== "modify") {
    const disk = h("input", { type: "number", min: "1", max: "65536", step: "1", value: v.diskGiB || "", placeholder: "Uncapped" });
    disk.addEventListener("input", () => { v.diskGiB = Number(disk.value) || 0; fields.disk.setError(""); changed(); });
    const quota = host?.volumeQuota;
    const supported = quota?.state === "supported";
    fields.disk = field("Data volume limit (GiB)", disk, { optional: true, explain: "Stops one database from filling the disk.", hint: supported
      ? "Writes fail once the volume is full."
      : `${quota?.message || "Checking disk limit support…"} Leave blank for no limit.` });
    sections.push(section(++n, "Storage", null, h("p", { class: "row" }, h("span", { class: `chip ${supported ? "good" : quota?.state === "unsupported" ? "warn" : "neutral"}` }, supported ? "Limits enforceable" : quota?.state === "unsupported" ? "Limits not enforceable here" : "Checking host")), fields.disk));
  } else {
    sections.push(section(++n, "Storage", null, keyValues([["Data volume limit", v.diskGiB ? `${v.diskGiB} GiB` : "Uncapped"]]),
      h("p", { class: "muted" }, v.diskGiB ? "Resize it on the Configuration tab while stopped." : "To add a limit, restore a snapshot into a new database.")));
  }

  // Connectivity
  const port = h("input", { type: "number", min: "1024", max: "65535", step: "1", value: v.port, required: true });
  port.addEventListener("input", () => { v.port = Number(port.value); fields.port.setError(""); changed(); });
  fields.port = field("Host port", port, { explain: "Any free port from 1024 to 65535.", hint: "Private to this server." });
  sections.push(section(++n, "Connectivity", null, fields.port));

  // Backups
  const retention = select([...new Set([...RETENTION, v.retention])].sort((a, b) => a - b).map(d => [d, d === 0 ? "Off — no automated backups" : plural(d, "day")]), v.retention);
  const win = parseWindow(v.window);
  const start = select(Array.from({ length: 48 }, (_, i) => [i * 30, `${hhmm(i * 30)} UTC`]), win.start);
  const length = select([...new Set([...LENGTHS, win.length])].map(m => [m, m < 60 ? `${m} minutes` : plural(m / 60, "hour")]), win.length);
  const windowFields = h("div", { class: "form-grid" }, field("Backup window start", start, { explain: "In UTC. Pick a quiet hour." }), field("Window length", length));
  fields.retention = field("Backup retention period", retention, { explain: "How long automated backups are kept.", hint: "Snapshots you take are kept until you delete them." });
  const frequency = frequencyField(v.interval, retention);
  const syncBackup = () => {
    v.retention = Number(retention.value); v.window = formatWindow(Number(start.value), Number(length.value)); v.interval = Number(frequency.control.value);
    windowFields.hidden = frequency.hidden = v.retention === 0; changed();
  };
  [retention, start, length, frequency.control].forEach(c => c.addEventListener("change", syncBackup));
  syncBackup();
  sections.push(section(++n, "Backup", null, fields.retention, frequency, windowFields));

  // Additional configuration
  let params = parameterEditor(eng, v.parameters);
  const paramSlot = h("div", null, params.el);
  params.onChange(changed);
  function rebuildParams() {
    params = parameterEditor(eng, {});
    params.onChange(changed);
    paramSlot.replaceChildren(params.el);
  }
  const protection = h("input", { type: "checkbox", class: "switch", role: "switch", checked: v.protection, id: nextId("protect") });
  protection.addEventListener("change", () => { v.protection = protection.checked; changed(); });
  const tls = h("input", { type: "checkbox", class: "switch", role: "switch", checked: Boolean(v.tls), id: nextId("tls") });
  tls.addEventListener("change", () => { v.tls = tls.checked; changed(); });
  const tlsText = () => eng.key === "valkey"
    ? "Clients must use rediss://. Plain connections stop working."
    : "Refuse connections without TLS.";
  const tlsDetail = h("small", null, tlsText());
  changes.add(() => { tlsDetail.textContent = tlsText(); });
  const tags = tagEditor(v.tags);
  tags.onChange(changed);
  sections.push(section(++n, "Additional configuration", null,
    h("h3", { class: "sub" }, icon("sliders"), "Engine parameters", help("Tuning settings. The defaults suit most apps.", "About engine parameters")),
    h("p", { class: "muted" }, mode === "modify" ? "Changes need a restart. You choose when." : "Applied at start."),
    paramSlot,
    h("h3", { class: "sub" }, icon("shield"), "Deletion protection", help("While on, nobody can delete the database.", "About deletion protection")),
    h("label", { class: "check", for: protection.id }, protection, h("span", null, h("strong", null, "Enable deletion protection"), h("small", null, "Blocks deletion until turned off."))),
    h("h3", { class: "sub" }, icon("lock"), "Encryption in transit", help("Encrypts traffic between your app and the database.", "About TLS")),
    h("label", { class: "check", for: tls.id }, tls, h("span", null, h("strong", null, "Require TLS"), tlsDetail)),
    h("h3", { class: "sub" }, icon("tag"), "Tags"),
    h("p", { class: "muted" }, "Labels you can search by."),
    tags.el));

  function applyTemplate(key) {
    const t = TEMPLATES[key];
    v.template = key;
    if (!SIZES.find(z => z.value === t.size && fits(z))) v.size = "custom"; else v.size = t.size;
    sizeCards.set(v.size); applySize(); updateSizes();
    retention.value = String(t.retention); syncBackup();
    protection.checked = v.protection = t.protection;
    changed();
  }

  function read() {
    return {
      name: v.name, engine: eng.key, version: v.version, port: v.port, cpus: v.cpus || 0, memoryMiB: v.memoryMiB || 0, diskGiB: v.diskGiB || 0,
      backupRetentionDays: v.retention, backupWindow: v.window, backupIntervalHours: v.interval || 24, deletionProtection: v.protection, requireTLS: Boolean(v.tls),
      tags: tags.value(), parameters: params.value(),
    };
  }

  function validate(spec) {
    const errors = [];
    const flag = (f, msg) => { f?.setError(msg); errors.push([f, msg]); };
    if (mode !== "modify") {
      if (!namePattern.test(spec.name || "")) flag(fields.name, "Use 1–40 lowercase letters, digits or hyphens. Start with a letter.");
      else if (others.some(d => d.name === spec.name)) flag(fields.name, "This name is taken.");
    }
    if (!Number.isInteger(spec.port) || spec.port < 1024 || spec.port > 65535) flag(fields.port, "Choose a port from 1024 to 65535.");
    else if (taken.has(spec.port)) flag(fields.port, `Port ${spec.port} is used by ${others.find(d => d.port === spec.port).name}.`);
    if (spec.cpus && (spec.cpus < 0.1 || spec.cpus > 1024)) flag(fields.cpus, "Use at least 0.1, or leave empty.");
    if (spec.memoryMiB && spec.memoryMiB < eng.minMemoryMiB) flag(fields.memory, `${eng.title} needs at least ${eng.minMemoryMiB} MiB.`);
    if (fields.disk && spec.diskGiB && (!Number.isInteger(spec.diskGiB) || spec.diskGiB < 1 || spec.diskGiB > 65536)) flag(fields.disk, "Use a whole number from 1 to 65536, or leave empty.");
    const firstBad = errors.find(([f]) => f)?.[0];
    firstBad?.control.focus();
    return errors;
  }

  // Summary
  const summary = h("div", { class: "summary-body" });
  const capacity = h("div", { class: "capacity" });
  const submit = h("button", { type: "submit", class: "btn primary wide" }, { create: "Create database", restore: "Restore database", modify: "Continue" }[mode]);
  const aside = h("aside", { class: "form-summary panel" }, h("header", { class: "panel-head" }, h("h2", null, "Summary")), summary, capacity, h("div", { class: "summary-actions" }, submit, h("a", { class: "btn wide", href: mode === "modify" ? `#/databases/${initial.name}` : mode === "restore" ? "#/snapshots" : "#/databases" }, "Cancel")));

  function renderSummary() {
    const spec = read();
    summary.replaceChildren(keyValues([
      ["Engine", `${eng.title} ${spec.version || ""}`],
      mode !== "modify" && ["Identifier", spec.name || "—"],
      ["Size", spec.cpus || spec.memoryMiB ? `${spec.cpus ? `${spec.cpus} vCPU` : "Shared CPU"} · ${spec.memoryMiB ? mib(spec.memoryMiB) : "shared memory"}` : "Shared host"],
      ["Storage", spec.diskGiB ? `${spec.diskGiB} GiB limit` : "Uncapped"],
      ["Endpoint", `127.0.0.1:${spec.port || "—"}`],
      ["Automated backups", spec.backupRetentionDays ? `${plural(spec.backupRetentionDays, "day")} · ${scheduleLabel(spec.backupWindow, spec.backupIntervalHours)}` : "Off"],
      ["Deletion protection", spec.deletionProtection ? "On" : "Off"],
      ["Encryption", spec.requireTLS ? "TLS required" : eng.key === "valkey" ? "Plain text" : "TLS available"],
      ["Parameters", Object.keys(spec.parameters).length ? plural(Object.keys(spec.parameters).length, "custom value") : "Engine defaults"],
      ["Tags", Object.keys(spec.tags).length || "None"],
    ], "stacked"));
    if (!host) { capacity.replaceChildren(); return; }
    const alloc = (key, extra) => others.filter(isLive).reduce((sum, d) => sum + (d[key] || 0), 0) + extra;
    const cpu = alloc("cpus", spec.cpus), memBytes = alloc("memoryMiB", spec.memoryMiB) * 1048576;
    const bar = (used, total) => { const fill = h("span", { class: "meter-fill" }); fill.style.width = `${Math.min(100, used / total * 100).toFixed(1)}%`; return h("span", { class: `meter ${used > total ? "critical" : used / total > 0.8 ? "warn" : "ok"}` }, fill); };
    capacity.replaceChildren(h("h3", null, "Host allocation after this change"),
      h("div", { class: "cap-row" }, h("span", null, "CPU limits"), h("strong", null, `${Number(cpu.toFixed(2))} of ${host.cpus} vCPU`), bar(cpu, host.cpus)),
      h("div", { class: "cap-row" }, h("span", null, "Memory limits"), h("strong", null, `${bytes(memBytes)} of ${bytes(host.memoryBytes)}`), bar(memBytes, host.memoryBytes)),
      h("p", { class: "muted" }, "Unlimited databases share spare capacity."));
  }
  changes.add(renderSummary);
  renderSummary();

  const form = h("form", { class: "form-layout", novalidate: true }, h("div", { class: "form-main" }, note, ...sections), aside);
  return { form, read, validate, note, submit, onChange: (fn) => changes.add(fn), get engine() { return eng; }, refresh: renderSummary };
}

// Wait briefly for a quick validation answer; otherwise follow the long
// operation from the database page while the request finishes.
async function launch({ request, label, success, then, note, submit }) {
  submit.disabled = true;
  const original = submit.textContent;
  submit.textContent = "Submitting…";
  setAlert(note, "");
  const first = await Promise.race([request.then(() => "done", () => "failed"), delay(1500).then(() => "slow")]);
  if (first === "failed") {
    try { await request; } catch (err) { setAlert(note, err.message); note.scrollIntoView({ block: "nearest" }); }
    submit.disabled = false; submit.textContent = original;
    return;
  }
  track(label, request, { success });
  await sync();
  go(then);
}

function heading(title, description) {
  return h("div", { class: "page-head" }, h("div", null, h("h1", null, title), description && h("p", { class: "muted" }, description)));
}

// The dashboard can preselect an engine for the next create form.
let preferred = null;
export const preferEngine = (key) => { preferred = key; };

export async function createView() {
  await loadEngines();
  const first = state.engines.find(e => e.key === preferred) || state.engines[0];
  preferred = null;
  const start = freeWindow();
  const t = TEMPLATES.production;
  const f = databaseForm({ mode: "create", eng: first, initial: { engine: first.key, version: first.defaultVersion, template: "production", size: t.size, cpus: 2, memoryMiB: 4096, port: suggestPort(first.key), retention: t.retention, window: formatWindow(start, 30), protection: t.protection, tags: {}, parameters: {} } });
  f.form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const spec = f.read();
    if (f.validate(spec).length) return;
    launch({ request: api("instances", { method: "POST", body: spec }), label: `Create ${spec.name}`, success: `${spec.name} is ready to accept connections.`, then: `#/databases/${spec.name}`, note: f.note, submit: f.submit });
  });
  return { el: h("div", null, heading("Create database", "A private database with backups."), f.form), title: "Create database", nav: "databases", crumbs: [["Databases", "#/databases"], ["Create database"]], update: f.refresh };
}

export async function restoreView(id) {
  await loadEngines();
  if (!state.backups) await sync();
  const b = state.backups?.find(x => x.id === id);
  if (!b) return { el: h("div", null, heading("Snapshot not found", "It may have been deleted or expired."), h("a", { class: "btn", href: "#/snapshots" }, "Back to snapshots")), title: "Restore", nav: "snapshots", crumbs: [["Snapshots", "#/snapshots"], ["Restore"]] };
  const eng = engine(b.engine);
  let name = `${b.name.slice(0, 32)}-restore`;
  for (let i = 2; state.instances?.some(d => d.name === name); i++) name = `${b.name.slice(0, 30)}-restore${i}`;
  const f = databaseForm({ mode: "restore", eng, source: b, initial: { name, engine: b.engine, version: b.version, size: sizeName(b.cpus || 0, b.memoryMiB || 0), cpus: b.cpus || 0, memoryMiB: b.memoryMiB || 0, diskGiB: b.diskGiB || 0, port: suggestPort(b.engine), retention: 7, window: formatWindow(freeWindow(), 30), protection: false, tags: {}, parameters: b.parameters || {} } });
  f.form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const spec = f.read();
    if (f.validate(spec).length) return;
    delete spec.engine;
    launch({ request: api(`backups/${b.id}/restore`, { method: "POST", body: spec }), label: `Restore ${spec.name}`, success: `${spec.name} was restored from the snapshot.`, then: `#/databases/${spec.name}`, note: f.note, submit: f.submit });
  });
  return { el: h("div", null, heading(`Restore snapshot`, `From the ${new Date(b.createdAt).toLocaleString()} snapshot of ${b.name}.`), f.form), title: "Restore snapshot", nav: b.kind === "automated" ? "automated" : "snapshots", crumbs: [[b.kind === "automated" ? "Automated backups" : "Snapshots", b.kind === "automated" ? "#/automated-backups" : "#/snapshots"], ["Restore"]], update: f.refresh };
}

const describeRuntime = (c) => ({ cpu: c.cpus ? `${c.cpus} vCPU` : "Unlimited", memory: c.memoryMiB ? mib(c.memoryMiB) : "Unlimited" });

export async function modifyView(name) {
  await loadEngines();
  if (!state.instances) await sync();
  const d = instance(name);
  if (!d) return { el: h("div", null, heading("Database not found", "It may have been deleted."), h("a", { class: "btn", href: "#/databases" }, "Back to databases")), title: "Modify", nav: "databases", crumbs: [["Databases", "#/databases"], ["Modify"]] };
  const eng = engine(d.engine);
  const f = databaseForm({ mode: "modify", eng, initial: { name: d.name, engine: d.engine, version: d.version, size: sizeName(d.cpus, d.memoryMiB), cpus: d.cpus, memoryMiB: d.memoryMiB, diskGiB: d.diskGiB, port: d.port, retention: d.backupRetentionDays, window: d.backupWindow, interval: d.backupIntervalHours, protection: d.deletionProtection, tls: d.requireTLS, tags: d.tags || {}, parameters: d.parameters || {} } });
  f.form.addEventListener("submit", (e) => {
    e.preventDefault();
    const spec = f.read();
    if (f.validate(spec).length) return;
    review(d, spec);
  });
  return { el: h("div", null, heading(`Modify ${d.name}`, "You review changes before they apply."), f.form), title: `Modify ${d.name}`, nav: "databases", crumbs: [["Databases", "#/databases"], [d.name, `#/databases/${d.name}`], ["Modify"]], update: f.refresh };
}

const same = (a, b) => JSON.stringify(Object.entries(a || {}).sort()) === JSON.stringify(Object.entries(b || {}).sort());
const mapLabel = (m) => Object.keys(m || {}).length ? Object.entries(m).sort().map(([k, v]) => v === "" ? k : `${k}=${v}`).join(", ") : "None";

// The RDS-style second step: a table of changes and when to apply them.
function review(d, spec) {
  const rows = [], body = {};
  const diff = (label, current, next, key, value) => { if (current !== next) { rows.push([label, current, next]); body[key] = value; } };
  const was = describeRuntime(d), now = describeRuntime(spec);
  diff("CPU limit", was.cpu, now.cpu, "cpus", spec.cpus);
  diff("Memory limit", was.memory, now.memory, "memoryMiB", spec.memoryMiB);
  diff("Host port", String(d.port), String(spec.port), "port", spec.port);
  diff("Backup retention", d.backupRetentionDays ? plural(d.backupRetentionDays, "day") : "Off", spec.backupRetentionDays ? plural(spec.backupRetentionDays, "day") : "Off", "backupRetentionDays", spec.backupRetentionDays);
  if (spec.backupRetentionDays) {
    diff("Backup window", windowLabel(d.backupWindow), windowLabel(spec.backupWindow), "backupWindow", spec.backupWindow);
    diff("Backup frequency", frequencyLabel(d.backupIntervalHours), frequencyLabel(spec.backupIntervalHours), "backupIntervalHours", spec.backupIntervalHours);
  }
  diff("Deletion protection", d.deletionProtection ? "On" : "Off", spec.deletionProtection ? "On" : "Off", "deletionProtection", spec.deletionProtection);
  diff("Require TLS", d.requireTLS ? "On" : "Off", spec.requireTLS ? "On" : "Off", "requireTLS", spec.requireTLS);
  if (!same(d.parameters, spec.parameters)) { rows.push(["Engine parameters", mapLabel(d.parameters), mapLabel(spec.parameters)]); body.parameters = spec.parameters; }
  if (!same(d.tags, spec.tags)) { rows.push(["Tags", mapLabel(d.tags), mapLabel(spec.tags)]); body.tags = spec.tags; }
  if (!rows.length) {
    toast("Nothing changed. Adjust a setting first.", "info");
    return;
  }
  const restartNeeded = "cpus" in body || "memoryMiB" in body || "parameters" in body || "requireTLS" in body;
  const portChange = "port" in body;
  const running = isLive(d);
  const choices = h("fieldset", { class: "apply-choice" }, h("legend", null, "When to apply"));
  const radio = (value, title, detail, checked) => {
    const input = h("input", { type: "radio", name: "apply", value, checked, id: nextId("apply") });
    choices.append(h("label", { class: "check", for: input.id }, input, h("span", null, h("strong", null, title), h("small", null, detail))));
  };
  let explain;
  if (portChange && running) explain = alertBox("A port change restarts the database now.", "warn");
  else if (restartNeeded && running) {
    radio("later", "Apply during the next restart", "Applied when you next reboot.", true);
    radio("now", "Apply immediately", "Restarts now, usually under a minute.", false);
  } else if (restartNeeded) explain = alertBox("Applies when the database starts.", "info");
  else explain = alertBox("Applies now, no restart.", "info");
  const table = h("table", { class: "diff" }, h("thead", null, h("tr", null, h("th", { scope: "col" }, "Attribute"), h("th", { scope: "col" }, "Current value"), h("th", { scope: "col" }, "New value"))),
    h("tbody", null, rows.map(([a, c, n]) => h("tr", null, h("th", { scope: "row" }, a), h("td", null, c), h("td", null, h("strong", null, n))))));
  modal({
    title: "Summary of modifications", kicker: d.name, wide: true, confirm: "Modify database",
    body: [h("div", { class: "table-wrap" }, table), explain, choices.children.length > 1 ? choices : null],
    run: async (form) => {
      body.applyImmediately = form.querySelector("input[name=apply]:checked")?.value === "now";
      const request = api(`instances/${d.name}/modify`, { method: "POST", body });
      const first = await Promise.race([request.then(() => "done", () => "failed"), delay(1500).then(() => "slow")]);
      if (first === "failed") await request; // the modal shows the error
      track(`Modify ${d.name}`, request, { success: (r) => r?.restarted ? `${d.name} restarted with the new settings.` : r?.pendingRestart ? `Saved. Applies at the next restart.` : `${d.name} was modified.` });
      go(`#/databases/${d.name}`);
      if (first === "slow") toast(`${d.name} is restarting with the new settings…`, "info");
    },
  });
}

// Shared with the settings editors on the database page.
export { SIZES, RETENTION, LENGTHS, parseWindow, formatWindow, hhmm, field, choiceCards, select, frequencyField };
