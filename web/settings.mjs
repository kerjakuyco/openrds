// Change one setting at a time, right where it is shown. Changes that need a
// restart ask whether to apply now or at the next restart.
import { h, icon, btn, api, toast, modal, panel, alertBox, mib, plural, bytes, scheduleLabel, isLive, nextId, sizeLabel } from "./lib.mjs";
import { state, engine, sync, can, track } from "./store.mjs";
import { SIZES, RETENTION, LENGTHS, parseWindow, formatWindow, hhmm, field, choiceCards, select, sizeName, parameterEditor, frequencyField } from "./forms.mjs";

const delay = (ms) => new Promise(resolve => setTimeout(resolve, ms));

// Why settings cannot change right now, if they cannot.
function blocked(d) {
  if (!can("operator")) return "Viewers can’t change settings.";
  if (d.busy) return "An operation is running.";
  if (d.pendingRestore) return "The restore didn’t finish.";
  return "";
}

// "Apply now or at the next restart", for changes that need a new container.
function applyChoice(d) {
  if (!isLive(d)) return { el: alertBox("Applies when the database starts.", "info"), now: () => false };
  const id = nextId("apply");
  const now = h("input", { type: "radio", name: id, value: "now", checked: true, id: `${id}-now` });
  const later = h("input", { type: "radio", name: id, value: "later", id: `${id}-later` });
  return {
    el: h("fieldset", { class: "apply-choice" }, h("legend", null, "When to apply"),
      h("label", { class: "check", for: now.id }, now, h("span", null, h("strong", null, "Apply now"), h("small", null, "Restarts the database, usually under a minute."))),
      h("label", { class: "check", for: later.id }, later, h("span", null, h("strong", null, "At the next restart"), h("small", null, "Saved now, applied when you reboot.")))),
    now: () => now.checked,
  };
}

// Send a change. A quick answer closes the dialog with its result; a restart
// carries on in the background and reports when it finishes.
async function submit(d, body, what) {
  const request = api(`instances/${d.name}/modify`, { method: "POST", body });
  const first = await Promise.race([request.then(() => "done", () => "failed"), delay(1500).then(() => "slow")]);
  if (first === "failed") await request; // the dialog shows the error
  track(`Change ${what} of ${d.name}`, request, {
    success: (r) => r?.restarted ? `${d.name} restarted with the new ${what}.` : r?.pendingRestart ? `Saved. The new ${what} applies at the next restart.` : `${what[0].toUpperCase()}${what.slice(1)} saved.`,
  });
  if (first === "slow") toast(`${d.name} is restarting…`, "info");
}

const unchanged = () => { throw new Error("Nothing changed."); };

export function editSize(d) {
  const eng = engine(d.engine), host = state.host;
  const fits = (z) => !host || (z.cpus <= host.cpus && z.memoryMiB * 1048576 <= host.memoryBytes);
  let choice = sizeName(d.cpus, d.memoryMiB);
  const cpus = h("input", { type: "number", min: "0.1", max: "1024", step: "0.1", value: d.cpus || "", placeholder: "Unlimited" });
  const memory = h("input", { type: "number", min: String(eng.minMemoryMiB), step: "1", value: d.memoryMiB || "", placeholder: "Unlimited" });
  const custom = h("div", { class: "form-grid", hidden: choice !== "custom" },
    field("CPU limit (vCPU)", cpus, { hint: host ? `This host has ${host.cpus} vCPU.` : "" }),
    field("Memory limit (MiB)", memory, { hint: host ? `This host has ${bytes(host.memoryBytes)}.` : "" }));
  const options = [
    ...SIZES.map(z => ({ value: z.value, title: z.title, detail: `${z.cpus} vCPU · ${mib(z.memoryMiB)}`, disabled: !fits(z) || z.memoryMiB < eng.minMemoryMiB, reason: !fits(z) ? "Larger than this host" : "Below the engine minimum" })),
    { value: "custom", title: "Custom", detail: "Your own limits." },
    { value: "shared", title: "Shared", detail: "No limits." },
  ];
  const cards = choiceCards("resize", options, choice, (v) => { choice = v; custom.hidden = v !== "custom"; }, { label: "Instance size", cls: "cols-3" });
  cards.disable(v => Boolean(options.find(o => o.value === v)?.disabled));
  const apply = applyChoice(d);
  modal({
    title: "Change instance size", kicker: d.name, confirm: "Save size", wide: true,
    body: [h("p", { class: "muted" }, `Now ${sizeLabel(d)}.`), cards, custom, apply.el],
    run: async () => {
      const preset = SIZES.find(z => z.value === choice);
      const next = choice === "shared" ? { cpus: 0, memoryMiB: 0 } : preset ? { cpus: preset.cpus, memoryMiB: preset.memoryMiB } : { cpus: Number(cpus.value) || 0, memoryMiB: Number(memory.value) || 0 };
      if (next.cpus && next.cpus < 0.1) throw new Error("Use at least 0.1 vCPU, or leave it empty.");
      if (next.memoryMiB && next.memoryMiB < eng.minMemoryMiB) throw new Error(`${eng.title} needs at least ${eng.minMemoryMiB} MiB.`);
      if (next.cpus === (d.cpus || 0) && next.memoryMiB === (d.memoryMiB || 0)) unchanged();
      await submit(d, { ...next, applyImmediately: apply.now() }, "size");
    },
  });
}

export function editPort(d) {
  const taken = new Map((state.instances || []).filter(o => o.name !== d.name).flatMap(o => [[o.port, o.name], ...(o.publicAccess ? [[o.publicPort, o.name]] : [])]));
  const input = h("input", { type: "number", min: "1024", max: "65535", step: "1", value: String(d.port) });
  const f = field("Host port", input, { hint: "Private to this server. Any free port from 1024 to 65535." });
  modal({
    title: "Change host port", kicker: d.name, confirm: "Save port",
    body: [f, isLive(d) ? alertBox("The database restarts now. Apps must use the new port.", "warn") : null],
    run: async () => {
      const port = Number(input.value);
      if (!Number.isInteger(port) || port < 1024 || port > 65535) throw new Error("Choose a port from 1024 to 65535.");
      if (taken.has(port)) throw new Error(`Port ${port} is used by ${taken.get(port)}.`);
      if (port === d.port) unchanged();
      await submit(d, { port }, "port");
    },
  });
}

// suggest: the retention offered when backups are off, so "Turn on" is one click.
export function editBackups(d, { suggest = 0 } = {}) {
  const win = parseWindow(d.backupWindow);
  const off = !d.backupRetentionDays;
  const initial = d.backupRetentionDays || suggest;
  const retention = select([...new Set([...RETENTION, initial])].sort((a, b) => a - b).map(n => [n, n === 0 ? "Off" : plural(n, "day")]), initial);
  const start = select(Array.from({ length: 48 }, (_, i) => [i * 30, `${hhmm(i * 30)} UTC`]), win.start);
  const length = select([...new Set([...LENGTHS, win.length])].map(m => [m, m < 60 ? `${m} minutes` : plural(m / 60, "hour")]), win.length);
  const windowFields = h("div", { class: "form-grid" }, field("Window start", start, { hint: "Pick a quiet hour." }), field("Window length", length));
  const frequency = frequencyField(d.backupIntervalHours, retention);
  const toggle = () => { windowFields.hidden = frequency.hidden = Number(retention.value) === 0; };
  retention.addEventListener("change", toggle);
  toggle();
  modal({
    title: off && suggest ? "Turn on automated backups" : "Automated backups", kicker: d.name, confirm: off && suggest ? "Turn on" : "Save backups",
    body: [field("Keep automated backups for", retention, { hint: "Snapshots you take are kept until you delete them." }), frequency, windowFields],
    run: async () => {
      const days = Number(retention.value);
      const body = { backupRetentionDays: days };
      if (days) {
        body.backupWindow = formatWindow(Number(start.value), Number(length.value));
        body.backupIntervalHours = Number(frequency.control.value);
      }
      if (days === d.backupRetentionDays && (!days || body.backupWindow === d.backupWindow && body.backupIntervalHours === d.backupIntervalHours)) unchanged();
      await submit(d, body, "backup settings");
    },
  });
}

export function editParameters(d) {
  const eng = engine(d.engine);
  const editor = parameterEditor(eng, d.parameters || {}, d.tuned);
  const apply = applyChoice(d);
  modal({
    title: "Engine parameters", kicker: d.name, confirm: "Save parameters", wide: true,
    body: [h("p", { class: "muted" }, "Unlisted settings use the engine default."), editor.el, apply.el],
    run: async () => {
      const next = editor.value();
      const same = JSON.stringify(Object.entries(next).sort()) === JSON.stringify(Object.entries(d.parameters || {}).sort());
      if (same) unchanged();
      await submit(d, { parameters: next, applyImmediately: apply.now() }, "parameters");
    },
  });
}

function resizeStorage(d) {
  const input = h("input", { type: "number", min: "1", max: "65536", step: "1", value: String(d.diskGiB) });
  modal({
    title: "Resize data volume", kicker: d.name, confirm: "Resize",
    body: [field("Size (GiB)", input, { hint: "Shrinking needs room for the data." })],
    run: async () => {
      const gib = Number(input.value);
      if (!Number.isInteger(gib) || gib < 1) throw new Error("Enter a whole number of GiB.");
      if (gib === d.diskGiB) unchanged();
      track(`Resize ${d.name}`, api(`instances/${d.name}/disk`, { method: "POST", body: { diskGiB: gib } }), { success: `${d.name} now has a ${gib} GiB data volume.` });
    },
  });
}

// On/off settings save as soon as they are switched.
function protectionSwitch(d, why) {
  const input = h("input", { type: "checkbox", class: "switch", role: "switch", checked: d.deletionProtection, disabled: Boolean(why), "aria-label": "Deletion protection" });
  input.addEventListener("change", async () => {
    input.disabled = true;
    try {
      await api(`instances/${d.name}/modify`, { method: "POST", body: { deletionProtection: input.checked } });
      toast(`Deletion protection is ${input.checked ? "on" : "off"}.`, "success");
      await sync();
    } catch (err) { input.checked = !input.checked; toast(err.message, "error"); }
    finally { input.disabled = Boolean(why); }
  });
  return input;
}

// Requiring TLS restarts the database and changes how clients connect, so it asks first.
function tlsSwitch(d, why) {
  const input = h("input", { type: "checkbox", class: "switch", role: "switch", checked: d.requireTLS, disabled: Boolean(why), "aria-label": "Require TLS" });
  input.addEventListener("change", () => {
    const on = input.checked;
    const apply = applyChoice(d);
    let saved = false;
    const dlg = modal({
      title: on ? "Require TLS?" : "Allow connections without TLS?", kicker: d.name, confirm: on ? "Require TLS" : "Allow",
      body: [h("p", { class: "muted" }, on
        ? (d.engine === "valkey" ? "Clients must use rediss://. Plain connections stop working." : "Connections without TLS are refused.")
        : "Clients can connect without encryption again."), apply.el],
      run: async () => { await submit(d, { requireTLS: on, applyImmediately: apply.now() }, "TLS setting"); saved = true; },
    });
    dlg.addEventListener("close", () => { if (!saved) input.checked = !on; });
  });
  return input;
}

// Point-in-time recovery starts with the server, so it asks when to restart.
function pitrSwitch(d, why) {
  const input = h("input", { type: "checkbox", class: "switch", role: "switch", checked: Boolean(d.pitr), disabled: Boolean(why) || !d.backupRetentionDays, "aria-label": "Point-in-time recovery" });
  if (!d.backupRetentionDays) input.title = "Turn on automated backups first.";
  input.addEventListener("change", () => {
    const on = input.checked;
    const apply = applyChoice(d);
    let saved = false;
    const dlg = modal({
      title: on ? "Turn on point-in-time recovery?" : "Turn off point-in-time recovery?", kicker: d.name, confirm: on ? "Turn on" : "Turn off", tone: on ? "primary" : "danger",
      body: [h("p", { class: "muted" }, on
        ? `PostgreSQL archives its WAL at least once a minute, and automated backups become physical base backups. You can then restore to any second of the last ${plural(d.backupRetentionDays, "day")}. The first base backup is taken right after the restart.`
        : "WAL stops being archived. Existing base backups and WAL stay until the retention period ends."), apply.el],
      run: async () => { await submit(d, { pitr: on, applyImmediately: apply.now() }, "point-in-time recovery setting"); saved = true; },
    });
    dlg.addEventListener("close", () => { if (!saved) input.checked = !on; });
  });
  return input;
}

const utc = (iso) => iso ? `${iso.slice(0, 19).replace("T", " ")} UTC` : "—";

// Restore a database as it was at a moment, into a new database.
export function restoreToTime(d) {
  const w = d.recovery || {};
  const toLocalInput = (iso) => iso.slice(0, 19);
  const at = h("input", { type: "datetime-local", step: "1", min: toLocalInput(w.earliest), max: toLocalInput(w.latest), value: toLocalInput(w.latest) });
  const name = h("input", { type: "text", value: `${d.name.slice(0, 33)}-pitr`, spellcheck: "false", autocomplete: "off" });
  const used = new Set((state.instances || []).flatMap(x => [x.port, x.publicPort, x.pooler?.port]).filter(Boolean));
  let free = d.port + 100;
  while (used.has(free)) free++;
  const port = h("input", { type: "number", min: "1024", max: "65535", value: String(free), class: "num" });
  modal({
    title: "Restore to point in time", kicker: d.name, confirm: "Restore", wide: true,
    body: [
      h("p", { class: "muted" }, `A new database is created as ${d.name} was at that second. ${d.name} keeps running unchanged.`),
      field("Restore to (UTC)", at, { hint: `Any second from ${utc(w.earliest)} to ${utc(w.latest)}.` }),
      h("div", { class: "form-grid" }, field("New DB identifier", name), field("Host port", port)),
      h("p", { class: "muted" }, "It gets the source’s size, storage, parameters and TLS setting, and its own owner password. Database users from the source are turned off; create them again."),
    ],
    run: async () => {
      if (!at.value) throw new Error("Choose the time to restore to.");
      const targetTime = new Date(`${at.value.length === 16 ? `${at.value}:00` : at.value}Z`).toISOString();
      const spec = { name: name.value.trim(), port: Number(port.value), targetTime };
      track(`Restore ${d.name} to ${utc(targetTime)}`, api(`instances/${d.name}/restore-to-time`, { method: "POST", body: spec }),
        { success: `${spec.name} is ready, as ${d.name} was at ${utc(targetTime)}.`, onDone: (err) => { if (!err) location.hash = `#/databases/${spec.name}`; } });
    },
  });
}

function storageUsage(d) {
  const measured = h("span", { role: "status", class: "muted" });
  const measure = btn("Measure usage", async (b) => {
    b.disabled = true; measured.textContent = "Measuring…";
    try {
      const { usedBytes } = await api(`instances/${d.name}/storage`);
      measured.textContent = `${bytes(usedBytes)} used${d.diskGiB ? ` of ${d.diskGiB} GiB` : " · uncapped"}`;
    } catch (err) { measured.textContent = err.message; }
    finally { b.disabled = false; }
  }, "small ghost");
  measure.disabled = !isLive(d) || d.busy;
  return [measure, measured];
}

// Dotted versions compared numerically: 9.10 is newer than 9.7.
const newer = (a, b) => {
  const x = a.split(".").map(Number), y = b.split(".").map(Number);
  for (let i = 0; i < Math.max(x.length, y.length); i++) if ((x[i] || 0) !== (y[i] || 0)) return (x[i] || 0) > (y[i] || 0);
  return false;
};

// Upgrade to a newer engine version, keeping the endpoint, like RDS's
// "Modify engine version".
export function upgradeEngine(d) {
  const eng = engine(d.engine);
  const options = (eng?.versions || []).filter(v => newer(v.version, d.version));
  if (!options.length) {
    modal({ title: "Engine version", kicker: d.name, confirm: "Close", cancel: "", body: h("p", { class: "muted" }, `${d.engineLabel} is the newest version OpenRDS offers.`), run: async () => {} });
    return;
  }
  const suggested = options.find(v => v.status === "stable") || options[0]; // the catalog lists newest first
  const version = select(options.map(v => [v.version, `${eng.title} ${v.version}${v.lts ? " LTS" : ""}${v.status === "latest" ? " · Latest" : v.status === "stable" ? " · Stable" : ""}`]), suggested.version);
  modal({
    title: "Upgrade engine version", kicker: d.name, confirm: "Upgrade", tone: "danger", typed: d.name, wide: true,
    body: [
      field("New version", version, { hint: `From ${d.engineLabel}. Versions only go up.` }),
      h("ul", { class: "plain-list" },
        h("li", null, h("strong", null, "Downtime. "), "Clients are disconnected while OpenRDS copies the data, about as long as a snapshot and a restore of this database."),
        h("li", null, h("strong", null, "Same endpoint. "), "The port, network name, passwords, users, pooler and settings stay as they are."),
        h("li", null, h("strong", null, "Safe to try. "), "A snapshot is taken first and kept. If anything fails, the current version comes back with all its data.")),
      d.applied ? alertBox("Pending changes apply during the upgrade.", "info") : null,
    ],
    run: async () => {
      const target = version.value;
      track(`Upgrade ${d.name} to ${eng.title} ${target}`, api(`instances/${d.name}/upgrade`, { method: "POST", body: { version: target } }),
        { success: `${d.name} now runs ${eng.title} ${target}.` });
    },
  });
}

// The settings list on the Configuration tab.
export function settingsPanel(d) {
  const why = blocked(d);
  const change = (label, run, disabled = false) => {
    if (!can("operator")) return null;
    const b = btn(label, () => run(d), "small");
    b.disabled = Boolean(why) || disabled;
    if (why) b.title = why;
    return b;
  };
  const row = (title, hint, value, action) => h("div", { class: "set-row" },
    h("div", { class: "set-label" }, h("strong", null, title), h("small", null, hint)),
    h("div", { class: "set-value" }, value),
    h("div", { class: "set-action" }, action));
  const stopped = ["stopped", "not provisioned"].includes(d.status);
  return panel("Settings", { description: "Change one thing at a time." },
    h("div", { class: "set-list" },
      row("Engine version", d.replicaOf ? `Follows ${d.replicaOf}.` : d.replicas?.length ? "Promote or delete read replicas first." : "Upgrades keep the endpoint.", d.engineLabel,
        change("Upgrade", upgradeEngine, !isLive(d) || d.pendingRestore || Boolean(d.replicaOf) || Boolean(d.replicas?.length))),
      row("Instance size", "CPU and memory limits.", sizeLabel(d), change("Change", editSize)),
      row("Host port", "Private to this server.", h("code", null, String(d.port)), change("Change", editPort)),
      row("Storage", d.diskGiB ? "Resize while stopped." : "No size limit.", h("span", { class: "row tight" }, d.diskGiB ? `${d.diskGiB} GiB limit` : "Uncapped", ...storageUsage(d)),
        d.diskGiB ? change("Resize", resizeStorage, !stopped) : null),
      row("Automated backups", "On a schedule, in a quiet window.", d.backupRetentionDays ? `${plural(d.backupRetentionDays, "day")} · ${scheduleLabel(d.backupWindow, d.backupIntervalHours)}` : h("span", { class: "status warn" }, icon("alert"), "Off"),
        change("Change", editBackups)),
      row("Require TLS", d.tlsReady ? "Refuse unencrypted connections." : "Reboot once to turn this on.", d.requireTLS ? "On" : "Off", tlsSwitch(d, why || (!d.tlsReady ? "Reboot first." : ""))),
      d.engine === "postgres" && !d.replicaOf ? row("Point-in-time recovery", d.backupRetentionDays ? "Restore to any second of the retention period." : "Needs automated backups.", d.pitr ? "On" : "Off", pitrSwitch(d, why)) : null,
      row("Deletion protection", "Blocks deleting the database.", d.deletionProtection ? "On" : "Off", protectionSwitch(d, why))));
}
