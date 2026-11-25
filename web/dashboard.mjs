// The landing page: a guided start for a new server, then how everything is
// doing and the one thing to do next.
import { h, icon, panel, empty, status, statusOf, bytes, mib, plural, ago, isLive, isRunning, needsAttention, percent } from "./lib.mjs";
import { state, subscribe, can, go } from "./store.mjs";
import { steps, dismissed, hideStart } from "./start.mjs";
import { cpuShare } from "./databases.mjs";
import { preferEngine } from "./forms.mjs";

const DAY = 86400000;
const CARDS = 6;
const ENGINES = [
  ["postgres", "PG", "PostgreSQL", "Powerful SQL with JSON."],
  ["mysql", "My", "MySQL", "The popular SQL database."],
  ["valkey", "Vk", "Valkey", "Fast cache and key-value store."],
];

function greeting(me) {
  const hour = new Date().getHours();
  const part = hour < 12 ? "Good morning" : hour < 18 ? "Good afternoon" : "Good evening";
  return me && !me.token ? `${part}, ${me.name.split(/\s+/)[0]}` : part;
}

function bar(share) {
  const s = Math.max(0, Math.min(1, share || 0));
  const fill = h("span");
  fill.style.width = `${(s * 100).toFixed(1)}%`;
  return h("span", { class: `bar${s >= 0.9 ? " critical" : s >= 0.75 ? " warn" : ""}`, "aria-hidden": "true" }, fill);
}

// Recommendations, most severe first, each with the place to fix it.
function recommendations(dbs, now, host) {
  const out = [];
  const add = (tone, title, detail, href, action) => out.push({ tone, title, detail, href, action });
  const disk = host?.backupDisk;
  if (disk && disk.freeBytes < 2 * disk.reserveBytes) {
    add(disk.freeBytes < disk.reserveBytes ? "critical" : "warn", disk.freeBytes < disk.reserveBytes ? "Backups are paused: disk almost full" : "Disk space is running low",
      `${bytes(disk.freeBytes)} free. Backups stop below ${bytes(disk.reserveBytes)} so databases keep running.`, "#/snapshots", "Review snapshots");
  }
  for (const d of dbs.filter(needsAttention)) {
    add("critical", `${d.name} needs attention`, d.pendingRestore ? "Restore didn’t finish." : `Status: ${d.status}.`, `#/databases/${d.name}/logs`, "Open logs");
  }
  for (const d of dbs.filter(d => d.backupRetentionDays && isRunning(d))) {
    const last = d.latestBackupAt ? Date.parse(d.latestBackupAt) : 0;
    if (now - Math.max(last, Date.parse(d.createdAt)) > DAY + 2 * 3600000) {
      add("warn", `${d.name} has no recent backup`, last ? `Last one ${ago(d.latestBackupAt, now)}.` : "No backup yet.", `#/databases/${d.name}/backups`, "Review");
    }
  }
  const unprotected = dbs.filter(d => !d.backupRetentionDays);
  if (unprotected.length) {
    add("warn", `Backups off for ${plural(unprotected.length, "database")}`, unprotected.slice(0, 4).map(d => d.name).join(", ") + (unprotected.length > 4 ? "…" : ""),
      unprotected.length === 1 ? `#/databases/${unprotected[0].name}/modify` : "#/automated-backups", unprotected.length === 1 ? "Turn on" : "Review");
  }
  for (const d of dbs.filter(d => d.applied)) add("info", `${d.name} needs a restart`, "To apply new settings.", `#/databases/${d.name}/configuration`, "Review");
  const prod = dbs.filter(d => !d.deletionProtection && Object.entries(d.tags || {}).some(([k, v]) => /^env(ironment)?$/i.test(k) && /^prod(uction)?$/i.test(v)));
  for (const d of prod) add("info", `Protect ${d.name} from deletion`, "Tagged production.", `#/databases/${d.name}/backups`, "Turn on");
  for (const d of dbs.filter(d => d.publicAccess)) {
    const rules = d.firewall || [];
    if (!d.requireTLS) add("warn", `${d.name} is public without TLS`, "Traffic isn’t encrypted.", `#/databases/${d.name}/modify`, "Require TLS");
    if (rules.some(r => r.cidr === "0.0.0.0/0" || r.cidr === "::/0")) add("warn", `${d.name} is open to everyone`, "The firewall allows any address.", `#/databases/${d.name}/connectivity`, "Review");
    else if (!rules.length) add("info", `${d.name} allows no sources`, "Nothing can connect yet.", `#/databases/${d.name}/connectivity`, "Add one");
  }
  const older = dbs.filter(d => isRunning(d) && !d.tlsReady);
  if (older.length) add("info", `${plural(older.length, "database")} ${older.length === 1 ? "needs" : "need"} a reboot`, `To turn on TLS and users: ${older.slice(0, 4).map(d => d.name).join(", ")}${older.length > 4 ? "…" : ""}.`,
    `#/databases/${older[0].name}/connectivity`, "Review");
  if (dbs.length && state.storage && !state.storage.enabled) add("info", "Backups are only on this server", "Copy them to S3-compatible storage.", "#/backup-storage", "Set up");
  if (state.storage?.failed) add("warn", `${plural(state.storage.failed, "backup")} not copied to storage`, state.storage.lastError || "Copies failed.", "#/backup-storage", "Review");
  const uncapped = dbs.filter(d => isLive(d) && !d.memoryMiB);
  if (uncapped.length) add("info", `${plural(uncapped.length, "database")} without a memory limit`, "One could slow the others.", uncapped.length === 1 ? `#/databases/${uncapped[0].name}/modify` : "#/databases", "Review");
  return out;
}

function databaseCard(d, host) {
  const st = statusOf(d);
  const running = isLive(d) && d.live;
  const cpu = cpuShare(d);
  const memLimit = d.memoryMiB ? d.memoryMiB * 1048576 : host?.memoryBytes;
  return h("article", { class: "db-card" },
    h("div", { class: "db-card-head" },
      h("span", { class: `engine-badge ${d.engine}`, "aria-hidden": "true" }, { postgres: "PG", mysql: "My", valkey: "Vk" }[d.engine]),
      h("span", { class: "stack" }, h("a", { class: "open-link", href: `#/databases/${d.name}` }, h("strong", null, d.name)), h("small", null, d.engineLabel)),
      status(d)),
    running ? h("div", { class: "usage" },
      h("div", { class: "usage-row" }, h("span", null, "CPU"), bar(cpu / 100), h("strong", null, percent(cpu))),
      h("div", { class: "usage-row" }, h("span", null, "Memory"), bar(memLimit ? d.live.memory / memLimit : 0), h("strong", null, d.memoryMiB ? `${bytes(d.live.memory)} / ${mib(d.memoryMiB)}` : bytes(d.live.memory))))
      : h("p", { class: "muted usage-empty" }, d.busy ? "Working…" : st.tone === "neutral" ? "Stopped." : "No usage yet."),
    h("div", { class: "db-card-foot" },
      h("span", null, `Port ${d.port} · ${d.backupRetentionDays ? `backups ${plural(d.backupRetentionDays, "day")}` : "no backups"}`),
      h("a", { class: "connect", href: `#/databases/${d.name}/connectivity` }, "Connect", icon("arrow"))));
}

// A new server: pick an engine and go.
function firstRun() {
  if (!can("operator")) return h("div", { class: "panel" }, empty("db", "No databases yet", "Ask an operator to create one."));
  const pick = (key) => { preferEngine(key); go("#/databases/new"); };
  return h("div", { class: "first-run" },
    h("section", { class: "panel first-pick", "aria-labelledby": "first-title" },
      h("div", { class: "first-head" }, h("h2", { id: "first-title" }, "Create your first database"), h("p", { class: "muted" }, "Pick an engine. It’s ready in about a minute.")),
      h("div", { class: "engine-picks" }, ENGINES.map(([key, badge, title, text]) =>
        h("button", { type: "button", class: "engine-pick", onclick: () => pick(key) },
          h("span", { class: `engine-badge ${key}`, "aria-hidden": "true" }, badge),
          h("span", { class: "stack" }, h("strong", null, title), h("small", null, text)),
          icon("arrow"))))),
    h("ol", { class: "how-it-works", "aria-label": "How it works" },
      [["db", "Create", "Choose an engine and a size."], ["link", "Connect", "Copy a ready-made snippet."], ["history", "Relax", "Backups run every day."]].map(([symbol, title, text], i) =>
        h("li", null, h("span", { class: "how-icon" }, icon(symbol)), h("span", null, h("small", null, `Step ${i + 1}`), h("strong", null, title), h("span", { class: "muted" }, text))))));
}

// The next unfinished getting-started step, one at a time.
function nextStep() {
  const list = steps();
  const next = list.find(s => !s.done);
  if (!next || dismissed()) return null;
  const done = list.filter(s => s.done).length;
  const operate = can("operator") || ["connect", "offsite", "account"].includes(next.id);
  return h("section", { class: "next-step", "aria-label": "Next step" },
    h("span", { class: "next-icon" }, icon("spark")),
    h("div", null, h("p", { class: "kicker" }, `Next step · ${done + 1} of ${list.length}`), h("strong", null, next.title), h("p", { class: "muted" }, next.text)),
    h("div", { class: "next-actions" },
      operate ? h("a", { class: "btn primary small", href: next.href }, next.action) : null,
      h("a", { class: "btn ghost small", href: "#/getting-started" }, "All steps"),
      h("button", { type: "button", class: "icon-btn ghost", "aria-label": "Hide the next step", title: "Hide. Reopen it from Getting started.", onclick: hideStart }, icon("close"))));
}

export function dashboardView() {
  const hero = h("div", { class: "dash-hero" });
  const body = h("div");
  const el = h("div", null, hero, body);

  function render() {
    const dbs = state.instances, backups = state.backups, host = state.host, now = Date.now();
    const trouble = dbs?.filter(needsAttention).length || 0, up = dbs?.filter(isRunning).length || 0;
    const summary = !dbs ? [h("span", null, "Loading…")]
      : !dbs.length ? [h("span", null, "Welcome to OpenRDS.")]
        : trouble ? [h("span", { class: "dot critical" }), h("span", null, `${plural(trouble, "database")} ${trouble === 1 ? "needs" : "need"} attention`)]
          : up === dbs.length ? [h("span", { class: "dot good" }), h("span", null, dbs.length === 1 ? "Your database is running" : `All ${dbs.length} databases are running`)]
            : [h("span", { class: "dot neutral" }), h("span", null, `${up} of ${plural(dbs.length, "database")} running`)];
    hero.replaceChildren(
      h("div", null, h("p", { class: "kicker" }, "Dashboard"), h("h1", null, greeting(state.me)), h("p", { class: "summary" }, summary)),
      can("operator") && dbs?.length ? h("div", { class: "page-actions" }, h("a", { class: "btn primary", href: "#/databases/new" }, icon("plus"), "Create database")) : "");

    if (!dbs) { body.replaceChildren(h("div", { class: "panel" }, empty("db", "Loading your databases…", ""))); return; }
    if (!dbs.length) { body.replaceChildren(firstRun()); return; }

    const sorted = [...dbs].sort((a, b) => needsAttention(b) - needsAttention(a) || a.name.localeCompare(b.name));
    const cards = h("section", { "aria-labelledby": "dash-dbs" },
      h("div", { class: "section-head" }, h("h2", { id: "dash-dbs" }, "Your databases"), h("a", { href: "#/databases" }, `View all ${dbs.length}`)),
      h("div", { class: "db-cards" }, sorted.slice(0, CARDS).map(d => databaseCard(d, host)),
        can("operator") && dbs.length < CARDS ? h("a", { class: "db-card add", href: "#/databases/new" }, icon("plus"), "Create database") : null));

    const list = recommendations(dbs, now, host);
    const attention = panel("Needs attention", { counter: list.length || null },
      list.length ? h("ul", { class: "attention" }, list.slice(0, 4).map(r => h("li", { class: r.tone },
        icon(r.tone === "critical" ? "error" : r.tone === "warn" ? "alert" : "info"),
        h("div", null, h("strong", null, r.title), h("p", { class: "muted" }, r.detail)),
        can("operator") || r.href.endsWith("/logs") ? h("a", { class: "btn small", href: r.href }, r.action) : null)))
        : h("div", { class: "all-good" }, icon("ok"), h("div", null, h("strong", null, "All clear"), h("p", { class: "muted" }, "Nothing needs you."))));

    const manual = backups?.filter(b => b.kind !== "automated") || [];
    const scheduled = dbs.filter(d => d.backupRetentionDays).length;
    const row = (symbol, label, value, href) => h("a", { class: "res-row", href }, icon(symbol), h("span", null, label), h("strong", null, value), icon("chevron-right"));
    const live = dbs.filter(isLive);
    const cpu = live.reduce((n, d) => n + (d.cpus || 0), 0), mem = live.reduce((n, d) => n + (d.memoryMiB || 0), 0) * 1048576;
    const line = (label, value, share) => h("div", { class: "cap-line" }, h("div", null, h("span", null, label), h("strong", null, value)), bar(share));
    const resources = panel("Resources", {},
      h("div", { class: "res-list" },
        row("db", "Databases", `${up} of ${dbs.length} running`, "#/databases"),
        row("camera", "Snapshots", backups ? `${manual.length} · ${bytes(manual.reduce((n, b) => n + b.bytes, 0))}` : "—", "#/snapshots"),
        row("history", "Automated backups", `${scheduled} of ${dbs.length} on`, "#/automated-backups"),
        row("cloud", "Backup storage", state.storage?.enabled ? `${state.storage.copied} copied` : "Off", "#/backup-storage")),
      host ? h("div", { class: "cap" },
        line("Memory limits", `${bytes(mem)} of ${bytes(host.memoryBytes)}`, mem / host.memoryBytes),
        line("CPU limits", `${Number(cpu.toFixed(2))} of ${host.cpus} vCPU`, cpu / host.cpus)) : null);

    // One line per action: its outcome, or "started" while it still runs.
    const all = state.activity || [];
    const key = (e) => `${e.name}|${e.action}|${e.actor}`; // newest first, so an outcome comes before its start
    const events = all.filter((e, i) => e.result !== "started" || !all.slice(0, i).some(x => x.result !== "started" && key(x) === key(e))).slice(0, 5);
    const activity = panel("Recent activity", { actions: [h("a", { class: "btn small ghost", href: "#/events" }, "View all")] },
      events.length ? h("ul", { class: "activity" }, events.map(e => h("li", { class: e.result },
        icon(e.result === "completed" ? "ok" : e.result === "failed" ? "error" : "pending"),
        h("div", null, h("strong", null, e.action),
          h("small", { class: "muted" }, e.kind ? e.name : h("a", { href: `#/databases/${e.name}` }, e.name),
            e.actor && e.actor !== "system" && e.actor !== e.name ? ` · ${e.actor}` : "", ` · ${ago(e.at, now)}`)))))
        : empty("bell", "Nothing yet", "Actions show up here."));

    body.replaceChildren(...[nextStep(), h("div", { class: "dash-main" }, cards, h("div", { class: "side-stack" }, attention, resources, activity))].filter(Boolean));
  }
  render();
  const stop = subscribe(render);
  window.addEventListener("openrds:start", render);
  return { el, title: "Dashboard", nav: "dashboard", crumbs: [["Dashboard"]], destroy: () => { stop(); window.removeEventListener("openrds:start", render); } };
}
