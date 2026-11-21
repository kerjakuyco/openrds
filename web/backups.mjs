// Snapshots and automated backups: the recovery library.
import { h, icon, api, modal, dataTable, empty, panel, bytes, when, ago, plural, windowLabel, toast, keyValues } from "./lib.mjs";
import { state, subscribe, sync, instance, engine, can } from "./store.mjs";
import { markDownloaded } from "./start.mjs";

const kindLabel = (b) => b.final ? "Final snapshot" : b.kind === "automated" ? "Automated" : "Manual";

function confirmDeleteBackup(b) {
  modal({
    title: "Delete this backup?", kicker: kindLabel(b), confirm: "Delete backup", tone: "danger",
    body: h("p", { class: "muted" }, `The ${when(b.createdAt)} ${kindLabel(b).toLowerCase()} backup of ${b.name} (${bytes(b.bytes)}) is removed from this VPS. This cannot be undone.`),
    run: async () => {
      await api(`backups/${b.id}`, { method: "DELETE" });
      toast("Backup deleted.", "success");
      await sync();
    },
  });
}

async function keep(b) {
  try {
    await api(`backups/${b.id}/keep`, { method: "POST" });
    toast(`The ${when(b.createdAt)} backup of ${b.name} is now a manual snapshot and will not expire.`, "success");
  } catch (err) { toast(err.message, "error", { title: "Could not keep the backup" }); }
  await sync();
}

export function backupTable(list, { showInstance = true, sort = { key: "created", dir: "desc" }, onSort } = {}) {
  const columns = [
    { key: "created", label: "Created", sort: b => Date.parse(b.createdAt), value: b => h("div", { class: "stack" }, h("strong", null, when(b.createdAt)), h("small", null, ago(b.createdAt))) },
    showInstance && { key: "name", label: "DB instance", sort: b => b.name, value: b => h("div", { class: "stack" },
      instance(b.name) ? h("a", { href: `#/databases/${b.name}` }, b.name) : h("span", null, b.name, h("small", null, " (deleted)")), h("small", null, engine(b.engine)?.title || b.engine)) },
    { key: "kind", label: "Type", sort: kindLabel, value: b => h("span", { class: `chip ${b.final ? "info" : b.kind === "automated" ? "neutral" : "good"}` }, kindLabel(b)) },
    { key: "size", label: "Size", sort: b => b.bytes, cls: "num", value: b => bytes(b.bytes) },
    { key: "expires", label: "Expires", sort: b => b.expiresAt ? Date.parse(b.expiresAt) : Infinity, value: b => b.expiresAt ? h("span", { title: when(b.expiresAt) }, ago(b.expiresAt)) : h("span", { class: "muted" }, "Never") },
    can("operator") && { key: "actions", label: "Actions", value: b => h("div", { class: "cell-actions" },
      h("a", { class: "text-btn", href: `#/snapshots/${b.id}/restore`, "aria-label": `Restore the ${when(b.createdAt)} backup of ${b.name}` }, icon("restore"), "Restore"),
      h("a", { class: "text-btn", href: `/api/backups/${b.id}/download`, download: "", onclick: markDownloaded, "aria-label": `Download the ${when(b.createdAt)} backup of ${b.name}` }, icon("download"), "Download"),
      b.kind === "automated" && h("button", { type: "button", class: "text-btn", onclick: () => keep(b), title: "Convert to a manual snapshot that never expires" }, icon("shield"), "Keep"),
      h("button", { type: "button", class: "text-btn danger", onclick: () => confirmDeleteBackup(b), "aria-label": `Delete the ${when(b.createdAt)} backup of ${b.name}` }, icon("trash"), "Delete")) },
  ].filter(Boolean);
  return dataTable({ columns, rows: list, sortKey: sort.key, sortDir: sort.dir, onSort, caption: "Backups" });
}

export function backupsView(kind) {
  const automated = kind === "automated";
  const ui = { instance: "", sort: { key: "created", dir: "desc" } };
  const filter = h("select", { "aria-label": "Filter by database" });
  filter.addEventListener("change", () => { ui.instance = filter.value; render(); });
  const listSlot = h("div");
  const settingsSlot = h("div");
  const count = h("span", { class: "counter" });
  const totals = h("p", { class: "muted" });
  const main = h("section", { class: "panel" },
    h("header", { class: "panel-head" }, h("div", null, h("h2", null, automated ? "Automated backups" : "Manual snapshots", count), totals),
      h("div", { class: "panel-actions" }, filter)),
    listSlot);
  const el = h("div", null,
    h("div", { class: "page-head" }, h("div", null, h("h1", null, automated ? "Automated backups" : "Snapshots"),
      h("p", { class: "muted" }, automated
        ? "Daily backups taken in each database’s backup window and deleted after its retention period."
        : "Point-in-time copies you take, including final snapshots of deleted databases. They stay until you delete them.")),
      h("div", { class: "page-actions" }, h("a", { class: "btn", href: automated ? "#/snapshots" : "#/automated-backups" }, icon(automated ? "camera" : "history"), automated ? "View snapshots" : "View automated backups"))),
    h("div", { class: "stack-gap" }, automated ? settingsSlot : null, main));

  function onSort(key) {
    ui.sort = { key, dir: ui.sort.key === key && ui.sort.dir === "asc" ? "desc" : "asc" };
    render();
  }

  function render() {
    const all = state.backups;
    if (!all) { listSlot.replaceChildren(empty("archive", "Reading backups…", "")); return; }
    const mine = all.filter(b => automated ? b.kind === "automated" : b.kind !== "automated");
    const names = [...new Set(mine.map(b => b.name))].sort();
    if (ui.instance && !names.includes(ui.instance)) ui.instance = "";
    filter.replaceChildren(h("option", { value: "" }, "All databases"), ...names.map(n => h("option", { value: n, selected: n === ui.instance }, n)));
    const list = mine.filter(b => !ui.instance || b.name === ui.instance);
    count.textContent = String(list.length);
    totals.textContent = `${bytes(list.reduce((n, b) => n + b.bytes, 0))} stored on this VPS. Download copies to independent storage to survive losing the host.`;
    listSlot.replaceChildren(list.length ? backupTable(list, { sort: ui.sort, onSort }) : empty(automated ? "history" : "camera",
      automated ? "No automated backups yet" : "No snapshots yet",
      automated ? "Turn on automated backups for a database by setting a retention period under Modify." : "Open a running database and choose Actions → Take snapshot."));
    if (automated) renderSettings();
  }

  function renderSettings() {
    const dbs = state.instances || [];
    const rows = dbs.map(d => ({ d, count: (state.backups || []).filter(b => b.kind === "automated" && b.name === d.name && Date.parse(b.createdAt) >= Date.parse(d.createdAt)).length }));
    settingsSlot.replaceChildren(panel("Backup schedule by database", { counter: dbs.length, description: "Times are UTC. A stopped database is not backed up." },
      dbs.length ? dataTable({
        caption: "Backup schedule", rows,
        columns: [
          { key: "name", label: "DB instance", value: r => h("a", { href: `#/databases/${r.d.name}/backups` }, r.d.name) },
          { key: "retention", label: "Retention", value: r => r.d.backupRetentionDays ? plural(r.d.backupRetentionDays, "day") : h("span", { class: "chip warn" }, "Off") },
          { key: "window", label: "Window", value: r => r.d.backupRetentionDays ? windowLabel(r.d.backupWindow) : "—" },
          { key: "latest", label: "Latest backup", value: r => r.d.latestBackupAt ? h("span", { title: when(r.d.latestBackupAt) }, ago(r.d.latestBackupAt)) : "None" },
          { key: "next", label: "Next automated", value: r => r.d.nextBackupAt ? h("span", { title: when(r.d.nextBackupAt) }, ago(r.d.nextBackupAt)) : "—" },
          { key: "count", label: "Kept", cls: "num", value: r => String(r.count) },
        ],
      }) : empty("db", "No databases", "Create a database to schedule backups.")));
  }

  render();
  const stop = subscribe((changed) => { if (changed.has("backups") || changed.has("instances")) render(); });
  return { el, title: automated ? "Automated backups" : "Snapshots", nav: automated ? "automated" : "snapshots", crumbs: [[automated ? "Automated backups" : "Snapshots"]], destroy: stop };
}

export function backupSummary(d) {
  return keyValues([
    ["Automated backups", d.backupRetentionDays ? `On · kept ${plural(d.backupRetentionDays, "day")}` : "Off"],
    ["Backup window", d.backupRetentionDays ? windowLabel(d.backupWindow) : "—"],
    ["Latest backup", d.latestBackupAt ? `${when(d.latestBackupAt)} (${ago(d.latestBackupAt)})` : "None yet"],
    ["Next automated backup", d.nextBackupAt ? `${when(d.nextBackupAt)} (${ago(d.nextBackupAt)})` : "—"],
  ]);
}
