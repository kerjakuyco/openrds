// The landing page: headline counts, recommendations and host capacity.
import { h, icon, panel, empty, dataTable, status, keyValues, bytes, plural, ago, when, isLive, isRunning, needsAttention, meter } from "./lib.mjs";
import { state, subscribe, can } from "./store.mjs";
import { startCard } from "./start.mjs";
import { cpuCell, showDatabases } from "./databases.mjs";
import { resultChip } from "./instance.mjs";

const DAY = 86400000;

function tile(label, value, hint, symbol, href, filter = null) {
  const open = filter === null ? null : (e) => { e.preventDefault(); showDatabases(filter); };
  return h("a", { class: "stat", href, onclick: open }, h("span", { class: "stat-label" }, icon(symbol), label), h("span", { class: "stat-value" }, String(value)), h("small", null, hint));
}

// Recommendations, most severe first, each with the place to fix it.
function recommendations(dbs, now) {
  const out = [];
  const add = (tone, title, detail, href, action) => out.push({ tone, title, detail, href, action });
  for (const d of dbs.filter(needsAttention)) {
    add("critical", `${d.name} needs attention`, d.pendingRestore ? "Its restore did not finish." : `Its status is ${d.status}.`, `#/databases/${d.name}/logs`, "Open logs");
  }
  for (const d of dbs.filter(d => d.backupRetentionDays && isRunning(d))) {
    const last = d.latestBackupAt ? Date.parse(d.latestBackupAt) : 0;
    if (now - Math.max(last, Date.parse(d.createdAt)) > DAY + 2 * 3600000) {
      add("warn", `${d.name} has no backup from the last day`, last ? `The latest is from ${ago(d.latestBackupAt, now)}. Check its events for a failed automated backup.` : "No backup has completed since it was created.", `#/databases/${d.name}/backups`, "Review backups");
    }
  }
  const unprotected = dbs.filter(d => !d.backupRetentionDays);
  if (unprotected.length) {
    add("warn", `Automated backups are off for ${plural(unprotected.length, "database")}`, unprotected.slice(0, 4).map(d => d.name).join(", ") + (unprotected.length > 4 ? "…" : ""),
      unprotected.length === 1 ? `#/databases/${unprotected[0].name}/modify` : "#/automated-backups", unprotected.length === 1 ? "Turn on" : "Review schedule");
  }
  for (const d of dbs.filter(d => d.applied)) add("info", `${d.name} has changes waiting for a restart`, "They apply at the next reboot, or stop and start.", `#/databases/${d.name}/configuration`, "Review");
  const prod = dbs.filter(d => !d.deletionProtection && Object.entries(d.tags || {}).some(([k, v]) => /^env(ironment)?$/i.test(k) && /^prod(uction)?$/i.test(v)));
  for (const d of prod) add("info", `Protect ${d.name} from deletion`, "It is tagged as production but deletion protection is off.", `#/databases/${d.name}/backups`, "Turn on");
  const uncapped = dbs.filter(d => isLive(d) && !d.memoryMiB);
  if (uncapped.length) add("info", `${plural(uncapped.length, "running database")} without a memory limit`, "One busy database can then starve the others. Set a limit under Modify.", uncapped.length === 1 ? `#/databases/${uncapped[0].name}/modify` : "#/databases", "Review");
  return out;
}

export function dashboardView() {
  const tiles = h("div", { class: "stats" });
  const recs = h("div");
  const host = h("div");
  const fleet = h("div");
  const recent = h("div");
  const start = h("div");
  const el = h("div", null,
    h("div", { class: "page-head" }, h("div", null, h("h1", null, "Dashboard"), h("p", { class: "muted" }, "Databases, recovery points and capacity on this VPS.")),
      can("operator") ? h("div", { class: "page-actions" }, h("a", { class: "btn primary", href: "#/databases/new" }, icon("plus"), "Create database")) : null),
    start,
    tiles,
    h("div", { class: "dash-grid" },
      h("div", { class: "stack-gap" }, recs, fleet),
      h("div", { class: "stack-gap" }, host, recent)));

  function render() {
    const dbs = state.instances, backups = state.backups, now = Date.now();
    const card = dbs ? startCard() : null;
    start.replaceChildren(...(card ? [card] : []));
    start.className = card ? "dash-start" : "";
    const manual = backups?.filter(b => b.kind !== "automated") || [];
    const auto = backups?.filter(b => b.kind === "automated") || [];
    tiles.replaceChildren(
      tile("Databases", dbs?.length ?? "—", dbs ? `${dbs.filter(isRunning).length} running` : "Reading inventory…", "db", "#/databases", ""),
      tile("Needs attention", dbs?.filter(needsAttention).length ?? "—", "Unhealthy or incomplete", "alert", "#/databases", "attention"),
      tile("Snapshots", backups ? manual.length : "—", backups ? `${bytes(manual.reduce((n, b) => n + b.bytes, 0))} on this VPS` : "Reading…", "camera", "#/snapshots"),
      tile("Automated backups", backups ? auto.length : "—", dbs ? `${dbs.filter(d => d.backupRetentionDays).length} of ${dbs.length} databases scheduled` : "Reading…", "history", "#/automated-backups"));

    const list = dbs ? recommendations(dbs, now) : [];
    recs.replaceChildren(panel("Recommendations", { counter: dbs ? list.length : null },
      !dbs ? empty("pulse", "Reading inventory…", "") : list.length ? h("ul", { class: "recs" }, list.map(r => h("li", { class: r.tone },
        icon(r.tone === "critical" ? "error" : r.tone === "warn" ? "alert" : "info", "icon rec-icon"),
        h("div", null, h("strong", null, r.title), h("p", { class: "muted" }, r.detail)),
        can("operator") || r.href.endsWith("/logs") ? h("a", { class: "btn small", href: r.href }, r.action) : null)))
        : h("div", { class: "all-good" }, icon("ok"), h("div", null, h("strong", null, "No recommendations"), h("p", { class: "muted" }, "Every database is healthy and backed up.")))));

    fleet.replaceChildren(panel("Databases", { counter: dbs?.length ?? null, actions: [h("a", { class: "btn small", href: "#/databases" }, "View all")] },
      !dbs ? empty("host", "Reading inventory…", "") : !dbs.length ? empty("db", "No databases yet", "Create one to see its status and usage here.", can("operator") ? h("a", { class: "btn primary", href: "#/databases/new" }, "Create database") : null)
        : dataTable({ caption: "Databases", rows: [...dbs].sort((a, b) => a.name.localeCompare(b.name)).slice(0, 8), columns: [
          { key: "name", label: "DB identifier", value: d => h("div", { class: "stack" }, h("a", { href: `#/databases/${d.name}` }, d.name), h("small", null, d.engineLabel)) },
          { key: "status", label: "Status", value: status },
          { key: "cpu", label: "CPU", value: cpuCell },
          { key: "mem", label: "Memory", value: d => d.live ? `${bytes(d.live.memory)}` : h("span", { class: "muted" }, "—") },
          { key: "backup", label: "Latest backup", value: d => d.latestBackupAt ? h("span", { title: when(d.latestBackupAt) }, ago(d.latestBackupAt, now)) : h("span", { class: "muted" }, "None") },
        ] })));

    const hi = state.host;
    if (!hi) host.replaceChildren(panel("Host capacity", {}, empty("host", "Reading the Docker host…", "")));
    else {
      const live = (dbs || []).filter(isLive);
      const cpu = live.reduce((n, d) => n + (d.cpus || 0), 0), mem = live.reduce((n, d) => n + (d.memoryMiB || 0), 0) * 1048576;
      const uncapped = live.filter(d => !d.cpus || !d.memoryMiB).length;
      const used = live.reduce((n, d) => n + (d.live?.memory || 0), 0);
      const row = (label, value, share, aria) => h("div", { class: "cap-row" }, h("span", null, label), h("strong", null, value), meter(share, 100, aria));
      host.replaceChildren(panel("Host capacity", { description: hi.name },
        row("CPU limits", `${Number(cpu.toFixed(2))} of ${hi.cpus} vCPU`, cpu / hi.cpus * 100, "CPU limits allocated"),
        row("Memory limits", `${bytes(mem)} of ${bytes(hi.memoryBytes)}`, mem / hi.memoryBytes * 100, "Memory limits allocated"),
        row("Memory in use", `${bytes(used)} of ${bytes(hi.memoryBytes)}`, used / hi.memoryBytes * 100, "Memory used by databases"),
        uncapped ? h("p", { class: "muted" }, `${plural(uncapped, "running database")} without full limits can use any spare capacity.`) : null,
        keyValues([["Docker Engine", hi.dockerVersion], ["Volume limits", hi.volumeQuota?.state === "supported" ? "Enforceable" : hi.volumeQuota?.state === "unsupported" ? "Not enforceable here" : "Unknown"]], "cols-2")));
    }

    const events = state.activity?.slice(0, 6) || [];
    recent.replaceChildren(panel("Recent events", { actions: [h("a", { class: "btn small", href: "#/events" }, "View all")] },
      !state.activity ? empty("bell", "Reading history…", "") : events.length ? h("ul", { class: "timeline" }, events.map(e => h("li", null,
        resultChip(e.result), h("div", null, h("strong", null, e.action), h("small", null, e.kind === "user" ? e.name : h("a", { href: `#/databases/${e.name}` }, e.name), e.actor && e.actor !== "system" && e.actor !== e.name ? ` by ${e.actor}` : "", ` · ${ago(e.at, now)}`))))) : empty("bell", "No operations yet", "Database operations appear here as they run.")));
  }
  render();
  const stop = subscribe(render);
  window.addEventListener("openrds:start", render);
  return { el, title: "Dashboard", nav: "dashboard", crumbs: [["Dashboard"]], destroy: () => { stop(); window.removeEventListener("openrds:start", render); } };
}
