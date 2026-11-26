// Snapshots and automated backups: the recovery library.
import { h, icon, api, modal, dataTable, empty, panel, bytes, when, ago, plural, scheduleLabel, toast, btn, menu, isLive } from "./lib.mjs";
import { state, subscribe, sync, instance, engine, can } from "./store.mjs";
import { markDownloaded } from "./start.mjs";
import { editBackups } from "./settings.mjs";
import { storageCell } from "./storage.mjs";

const kindLabel = (b) => b.final ? "Final snapshot" : b.kind === "automated" ? "Automated" : "Manual";
const BADGE = { postgres: "PG", mysql: "My", valkey: "Vk" };

function confirmDeleteBackup(b) {
  modal({
    title: "Delete this backup?", kicker: kindLabel(b), confirm: "Delete backup", tone: "danger",
    body: h("p", { class: "muted" }, `The ${when(b.createdAt)} backup of ${b.name} (${bytes(b.bytes)}) is removed for good.`),
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
    toast("Kept as a snapshot. It won’t expire.", "success");
  } catch (err) { toast(err.message, "error", { title: "Could not keep the backup" }); }
  await sync();
}

function download(b) {
  markDownloaded();
  location.href = `/api/backups/${b.id}/download`; // an attachment, so the page stays
}

// Restore is the main action; the rest sit in a small menu.
function backupActions(b) {
  return h("div", { class: "cell-actions" },
    h("a", { class: "text-btn", href: `#/snapshots/${b.id}/restore`, "aria-label": `Restore the ${when(b.createdAt)} backup of ${b.name}` }, icon("restore"), "Restore"),
    menu("", [
      { label: "Download", icon: "download", run: () => download(b) },
      b.kind === "automated" ? { label: "Keep as snapshot", icon: "shield", run: () => keep(b) } : null,
      "-",
      { label: "Delete", icon: "trash", danger: true, run: () => confirmDeleteBackup(b) },
    ].filter(Boolean), { cls: "small ghost icon-only", labelIcon: "more", chevron: false, ariaLabel: `More actions for the ${when(b.createdAt)} backup of ${b.name}` }));
}

export function backupTable(list, { showInstance = true, showType = true, sort = { key: "created", dir: "desc" }, onSort } = {}) {
  const columns = [
    { key: "created", label: "Backup", sort: b => Date.parse(b.createdAt), value: b => h("div", { class: "stack" }, h("strong", null, when(b.createdAt)), h("small", null, ago(b.createdAt))) },
    showInstance && { key: "name", label: "Database", sort: b => b.name, value: b => h("div", { class: "stack" },
      instance(b.name) ? h("a", { href: `#/databases/${b.name}` }, b.name) : h("span", null, b.name, h("small", null, " (deleted)")), h("small", null, `${engine(b.engine)?.title || b.engine} ${b.version || ""}`.trim())) },
    showType && { key: "kind", label: "Type", sort: kindLabel, value: b => h("div", { class: "stack" },
      h("span", { class: `chip ${b.final ? "info" : b.kind === "automated" ? "neutral" : "good"}` }, kindLabel(b)), b.note ? h("small", null, b.note) : null) },
    { key: "size", label: "Size", sort: b => b.bytes, cls: "num", value: b => bytes(b.bytes) },
    { key: "expires", label: "Expires", sort: b => b.expiresAt ? Date.parse(b.expiresAt) : Infinity, value: b => b.expiresAt ? h("span", { title: when(b.expiresAt) }, ago(b.expiresAt)) : h("span", { class: "muted" }, "Never") },
    state.storage?.enabled && { key: "offsite", label: "Storage", value: storageCell },
    can("operator") && { key: "actions", label: h("span", { class: "sr-only" }, "Actions"), cls: "actions", value: backupActions },
  ].filter(Boolean);
  return dataTable({ columns, rows: list, sortKey: sort.key, sortDir: sort.dir, onSort, caption: "Backups" });
}

// One database's schedule: what runs, when it last ran and when it runs next.
function scheduleRow(d, stored) {
  const on = d.backupRetentionDays > 0;
  const paused = on && !isLive(d);
  const when2 = (label, value) => h("div", { class: "sched-when" }, h("small", null, label), h("span", null, value));
  let action = null;
  if (can("operator")) {
    action = btn(on ? "Change" : "Turn on", () => editBackups(d, { suggest: 7 }), on ? "small" : "small primary");
    action.disabled = d.busy || d.pendingRestore;
  }
  return h("div", { class: "sched-row" },
    h("div", { class: "sched-db" },
      h("span", { class: `engine-badge ${d.engine}`, "aria-hidden": "true" }, BADGE[d.engine]),
      h("span", { class: "stack" }, h("a", { href: `#/databases/${d.name}/backups` }, h("strong", null, d.name)), h("small", null, d.engineLabel))),
    h("div", { class: "sched-plan" }, on
      ? [h("strong", null, scheduleLabel(d.backupWindow, d.backupIntervalHours)), h("small", null, `Kept ${plural(d.backupRetentionDays, "day")} · ${plural(stored, "backup")} stored`)]
      : [h("span", { class: "status warn" }, icon("alert"), "Off"), h("small", null, "No automatic backups.")]),
    when2("Last backup", d.latestBackupAt ? h("span", { title: when(d.latestBackupAt) }, ago(d.latestBackupAt)) : "None yet"),
    when2("Next backup", !on ? "—" : paused ? h("span", { class: "muted" }, "Paused while stopped") : d.nextBackupAt ? h("span", { title: when(d.nextBackupAt) }, ago(d.nextBackupAt)) : "—"),
    h("div", { class: "sched-action" }, action));
}

function schedules(dbs, backups) {
  const on = dbs.filter(d => d.backupRetentionDays > 0).length;
  const fill = h("span");
  fill.style.width = `${dbs.length ? (on / dbs.length * 100).toFixed(1) : 0}%`;
  const all = on === dbs.length;
  return panel("Schedules", { description: "Times in UTC." },
    h("div", { class: `coverage${all ? " full" : ""}` },
      h("span", { class: "coverage-text" }, icon(all ? "ok" : "alert"), h("strong", null, `${on} of ${plural(dbs.length, "database")}`), on === 1 && dbs.length === 1 ? " backs up automatically" : " back up automatically"),
      h("span", { class: "coverage-bar", "aria-hidden": "true" }, fill)),
    h("div", { class: "sched-list" }, dbs.map(d => scheduleRow(d, backups.filter(b => b.kind === "automated" && b.name === d.name && Date.parse(b.createdAt) >= Date.parse(d.createdAt)).length))));
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
    h("header", { class: "panel-head" }, h("div", null, h("h2", null, automated ? "Backups" : "Snapshots", count), totals),
      h("div", { class: "panel-actions" }, filter)),
    listSlot);
  const el = h("div", null,
    h("div", { class: "page-head" }, h("div", null, h("h1", null, automated ? "Automated backups" : "Snapshots"),
      h("p", { class: "muted" }, automated ? "Backups on a schedule, from daily to hourly, kept for the days you choose." : "Backups you take. They stay until you delete them.")),
      h("div", { class: "page-actions" }, h("a", { class: "btn", href: automated ? "#/snapshots" : "#/automated-backups" }, icon(automated ? "camera" : "history"), automated ? "View snapshots" : "View automated backups"))),
    h("div", { class: "stack-gap" }, automated ? settingsSlot : null, main));

  function onSort(key) {
    ui.sort = { key, dir: ui.sort.key === key && ui.sort.dir === "asc" ? "desc" : "asc" };
    render();
  }

  function render() {
    const all = state.backups, dbs = state.instances || [];
    if (automated) settingsSlot.replaceChildren(dbs.length ? schedules(dbs, all || []) : panel("Schedules", {}, empty("db", "No databases yet", "Create a database to back it up.")));
    if (!all) { listSlot.replaceChildren(empty("archive", "Loading backups…", "")); return; }
    const mine = all.filter(b => automated ? b.kind === "automated" : b.kind !== "automated");
    const names = [...new Set(mine.map(b => b.name))].sort();
    if (ui.instance && !names.includes(ui.instance)) ui.instance = "";
    filter.hidden = names.length < 2;
    filter.replaceChildren(h("option", { value: "" }, "All databases"), ...names.map(n => h("option", { value: n, selected: n === ui.instance }, n)));
    const list = mine.filter(b => !ui.instance || b.name === ui.instance);
    count.textContent = String(list.length);
    totals.textContent = list.length ? `${bytes(list.reduce((n, b) => n + b.bytes, 0))} on this server. Download copies to keep them safe.` : "";
    const next = dbs.filter(d => d.nextBackupAt && isLive(d)).map(d => d.nextBackupAt).sort()[0];
    listSlot.replaceChildren(list.length ? backupTable(list, { showType: !automated, sort: ui.sort, onSort }) : automated
      ? empty("history", "No automated backups yet", next ? `The first one runs ${ago(next)}.` : "Turn on a schedule above.")
      : empty("camera", "No snapshots yet", "Use Actions → Take snapshot on a database."));
  }

  render();
  const stop = subscribe((changed) => { if (changed.has("backups") || changed.has("instances")) render(); });
  return { el, title: automated ? "Automated backups" : "Snapshots", nav: automated ? "automated" : "snapshots", crumbs: [[automated ? "Automated backups" : "Snapshots"]], destroy: stop };
}
