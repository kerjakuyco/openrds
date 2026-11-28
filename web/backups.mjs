// Snapshots and automated backups: the recovery library.
import { h, icon, api, modal, dataTable, empty, panel, bytes, when, ago, plural, scheduleLabel, toast, btn, menu, isLive, query, serverPager } from "./lib.mjs";
import { state, subscribe, sync, instance, engine, can } from "./store.mjs";
import { markDownloaded } from "./start.mjs";
import { editBackups } from "./settings.mjs";
import { storageCell } from "./storage.mjs";

const kindLabel = (b) => b.final ? "Final snapshot" : b.format === "physical" || b.binlogFile ? "Base backup" : b.kind === "automated" ? "Automated" : "Manual";
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

// With onSort, the rows are a page the server sorted.
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
  return dataTable({ columns, rows: list, sortKey: sort.key, sortDir: sort.dir, onSort, sorted: Boolean(onSort), caption: "Backups" });
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

function schedules(dbs) {
  const on = dbs.filter(d => d.backupRetentionDays > 0).length;
  const fill = h("span");
  fill.style.width = `${dbs.length ? (on / dbs.length * 100).toFixed(1) : 0}%`;
  const all = on === dbs.length;
  return panel("Schedules", { description: "Times in UTC." },
    h("div", { class: `coverage${all ? " full" : ""}` },
      h("span", { class: "coverage-text" }, icon(all ? "ok" : "alert"), h("strong", null, `${on} of ${plural(dbs.length, "database")}`), on === 1 && dbs.length === 1 ? " backs up automatically" : " back up automatically"),
      h("span", { class: "coverage-bar", "aria-hidden": "true" }, fill)),
    h("div", { class: "sched-list" }, dbs.map(d => scheduleRow(d, d.automatedBackups || 0))));
}

// One Backups page: every backup, snapshots, automated ones, and the
// schedules that take them, as tabs.
const BACKUP_TABS = [["", "All"], ["snapshots", "Snapshots"], ["automated", "Automated"], ["schedules", "Schedules"]];

const PAGE = 50;
const KIND = { snapshots: "manual", automated: "automated" };

export function backupsView(tab = "") {
  if (!BACKUP_TABS.some(([key]) => key === tab)) tab = "";
  const ui = { instance: "", sort: { key: "created", dir: "desc" }, offset: 0 };
  const filter = h("select", { "aria-label": "Filter by database" });
  filter.addEventListener("change", () => { ui.instance = filter.value; refilter(); });
  const body = h("div", { class: "stack-gap" });
  const count = h("span", { class: "counter" });
  const totals = h("p", { class: "muted" });
  const listSlot = h("div");
  const pages = h("div");
  const main = h("section", { class: "panel" },
    h("header", { class: "panel-head" }, h("div", null, h("h2", null, { "": "All backups", snapshots: "Snapshots", automated: "Automated backups" }[tab] || "", count), totals),
      h("div", { class: "panel-actions" }, filter)),
    listSlot, pages);
  const tabs = h("div", { class: "tabs", role: "tablist", "aria-label": "Backups" }, BACKUP_TABS.map(([key, label]) =>
    h("a", { role: "tab", href: key ? `#/backups/${key}` : "#/backups", "aria-selected": String(key === tab) }, label)));
  const el = h("div", null,
    h("div", { class: "page-head" }, h("div", null, h("h1", null, "Backups"),
      h("p", { class: "muted" }, "Snapshots you take, and backups on a schedule."))),
    tabs, body);

  function onSort(key) {
    ui.sort = { key, dir: ui.sort.key === key && ui.sort.dir === "asc" ? "desc" : "asc" };
    refilter();
  }

  // The rows of this page, sorted and cut on the server. Rows of an old
  // filter or order never show under the new one.
  let page = null, seq = 0, failed = "";
  function refilter() {
    ui.offset = 0;
    page = null;
    failed = "";
    render();
    load();
  }
  async function load() {
    if (tab === "schedules") return;
    const mine = ++seq;
    try {
      const next = await api(`backups?${query({ kind: KIND[tab], name: ui.instance, sort: ui.sort.key, order: ui.sort.dir, offset: ui.offset, limit: PAGE })}`);
      if (mine !== seq) return;
      page = next;
      failed = "";
      if (page.offset > 0 && page.offset >= page.total) { ui.offset = Math.max(0, Math.floor((page.total - 1) / PAGE) * PAGE); load(); return; }
    } catch (err) {
      if (mine !== seq) return;
      failed = err.message;
    }
    render();
  }

  function render() {
    const dbs = state.instances || [];
    if (tab === "schedules") {
      body.replaceChildren(dbs.length ? schedules(dbs) : panel("Schedules", {}, empty("db", "No databases yet", "Create a database to back it up.")));
      return;
    }
    body.replaceChildren(main);
    if (!page) { listSlot.replaceChildren(failed ? empty("error", "Could not read backups", failed) : empty("archive", "Loading backups…", "")); pages.replaceChildren(); return; }
    const names = page.names.includes(ui.instance) || !ui.instance ? page.names : [...page.names, ui.instance].sort();
    filter.hidden = names.length < 2 && !ui.instance;
    filter.replaceChildren(h("option", { value: "" }, "All databases"), ...names.map(n => h("option", { value: n, selected: n === ui.instance }, n)));
    count.textContent = page.total.toLocaleString();
    totals.textContent = page.total ? `${bytes(page.bytes)} on this server. Download copies to keep them safe.` : "";
    const next = dbs.filter(d => d.nextBackupAt && isLive(d)).map(d => d.nextBackupAt).sort()[0];
    listSlot.replaceChildren(page.items.length ? backupTable(page.items, { showType: tab !== "automated", sort: ui.sort, onSort })
      : ui.instance ? empty("archive", "No backups of this database", "Choose another database.")
        : tab === "automated" ? empty("history", "No automated backups yet", next ? `The first one runs ${ago(next)}.` : "Turn one on under Schedules.")
          : tab === "snapshots" ? empty("camera", "No snapshots yet", "Use Actions → Take snapshot on a database.")
            : empty("archive", "No backups yet", "Take a snapshot, or turn on a schedule."));
    pages.replaceChildren(serverPager(page, PAGE, (offset) => { ui.offset = offset; load(); main.scrollIntoView({ block: "nearest" }); }, "Backup pages"));
  }

  render();
  load();
  // A backup taken, kept or deleted changes the counts, and the page follows.
  const stop = subscribe((changed) => {
    if (changed.has("backups") || changed.has("storage")) load();
    else if (changed.has("instances")) render();
  });
  const title = BACKUP_TABS.find(([key]) => key === tab)[1];
  return { el, title: tab ? `${title} · Backups` : "Backups", nav: "backups", crumbs: tab ? [["Backups", "#/backups"], [title]] : [["Backups"]], destroy: stop };
}
