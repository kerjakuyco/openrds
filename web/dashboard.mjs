// The landing page: how everything is doing at a glance, and where to go next.
import { h, icon, panel, empty, status, statusOf, bytes, mib, plural, ago, isLive, isRunning, needsAttention, percent } from "./lib.mjs";
import { state, subscribe, can } from "./store.mjs";
import { startCard } from "./start.mjs";
import { showDatabases, cpuShare } from "./databases.mjs";

const DAY = 86400000;
const CARDS = 6;

function greeting(me) {
  const hour = new Date().getHours();
  const part = hour < 12 ? "Good morning" : hour < 18 ? "Good afternoon" : "Good evening";
  return me && !me.token ? `${part}, ${me.name.split(/\s+/)[0]}` : part;
}

function tile(label, value, hint, symbol, href, filter = null) {
  const open = filter === null ? null : (e) => { e.preventDefault(); showDatabases(filter); };
  return h("a", { class: "stat", href, onclick: open }, h("span", { class: "stat-label" }, label, icon(symbol)), h("span", { class: "stat-value" }, String(value)), h("small", null, hint));
}

function bar(share) {
  const s = Math.max(0, Math.min(1, share || 0));
  const fill = h("span");
  fill.style.width = `${(s * 100).toFixed(1)}%`;
  return h("span", { class: `bar${s >= 0.9 ? " critical" : s >= 0.75 ? " warn" : ""}`, "aria-hidden": "true" }, fill);
}

// Recommendations, most severe first, each with the place to fix it.
function recommendations(dbs, now) {
  const out = [];
  const add = (tone, title, detail, href, action) => out.push({ tone, title, detail, href, action });
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

export function dashboardView() {
  const hero = h("div", { class: "dash-hero" });
  const start = h("div");
  const tiles = h("div", { class: "stats" });
  const cards = h("section", { "aria-labelledby": "dash-dbs" });
  const attention = h("div"), capacity = h("div"), activity = h("div");
  const el = h("div", null, hero, start, tiles, h("div", { class: "dash-main" }, cards, h("div", { class: "side-stack" }, attention, capacity, activity)));

  function render() {
    const dbs = state.instances, backups = state.backups, host = state.host, now = Date.now();
    const trouble = dbs?.filter(needsAttention).length || 0, up = dbs?.filter(isRunning).length || 0;
    const summary = !dbs ? [h("span", null, "Reading your databases…")]
      : !dbs.length ? [h("span", null, "No databases yet.")]
        : trouble ? [h("span", { class: "dot critical" }), h("span", null, `${plural(trouble, "database")} ${trouble === 1 ? "needs" : "need"} attention`)]
          : up === dbs.length ? [h("span", { class: "dot good" }), h("span", null, dbs.length === 1 ? "Your database is running" : `All ${dbs.length} databases are running`)]
            : [h("span", { class: "dot neutral" }), h("span", null, `${up} of ${plural(dbs.length, "database")} running`)];
    hero.replaceChildren(
      h("div", null, h("p", { class: "kicker" }, "Dashboard"), h("h1", null, greeting(state.me)), h("p", { class: "summary" }, summary)),
      can("operator") ? h("div", { class: "page-actions" }, h("a", { class: "btn primary", href: "#/databases/new" }, icon("plus"), "Create database")) : "");

    const card = dbs ? startCard() : null;
    start.replaceChildren(...(card ? [card] : []));
    start.className = card ? "dash-start" : "";

    const manual = backups?.filter(b => b.kind !== "automated") || [];
    const auto = backups?.filter(b => b.kind === "automated") || [];
    tiles.replaceChildren(
      tile("Databases", dbs?.length ?? "—", dbs ? `${up} running` : "Reading…", "db", "#/databases", ""),
      tile("Needs attention", dbs ? trouble : "—", trouble ? "Check these first" : "All healthy", "alert", "#/databases", "attention"),
      tile("Snapshots", backups ? manual.length : "—", backups ? `${bytes(manual.reduce((n, b) => n + b.bytes, 0))} stored` : "Reading…", "camera", "#/snapshots"),
      tile("Automated backups", backups ? auto.length : "—", dbs ? `${dbs.filter(d => d.backupRetentionDays).length} of ${dbs.length} scheduled` : "Reading…", "history", "#/automated-backups"));

    const sorted = [...(dbs || [])].sort((a, b) => needsAttention(b) - needsAttention(a) || a.name.localeCompare(b.name));
    cards.replaceChildren(
      h("div", { class: "section-head" }, h("h2", { id: "dash-dbs" }, "Your databases"), dbs?.length ? h("a", { href: "#/databases" }, `View all ${dbs.length}`) : ""),
      !dbs ? h("div", { class: "panel" }, empty("db", "Reading your databases…", ""))
        : h("div", { class: "db-cards" }, sorted.slice(0, CARDS).map(d => databaseCard(d, host)),
          can("operator") && dbs.length < CARDS ? h("a", { class: "db-card add", href: "#/databases/new" }, icon("plus"), dbs.length ? "Create another database" : "Create your first database") : null,
          !dbs.length && !can("operator") ? h("div", { class: "panel" }, empty("db", "No databases yet", "Ask an operator to create one.")) : null));

    const list = dbs ? recommendations(dbs, now) : [];
    attention.replaceChildren(panel("Needs attention", { counter: dbs && list.length ? list.length : null },
      !dbs ? empty("pulse", "Checking…", "") : list.length ? h("ul", { class: "attention" }, list.slice(0, 5).map(r => h("li", { class: r.tone },
        icon(r.tone === "critical" ? "error" : r.tone === "warn" ? "alert" : "info"),
        h("div", null, h("strong", null, r.title), h("p", { class: "muted" }, r.detail)),
        can("operator") || r.href.endsWith("/logs") ? h("a", { class: "btn small", href: r.href }, r.action) : null)))
        : h("div", { class: "all-good" }, icon("ok"), h("div", null, h("strong", null, "All clear"), h("p", { class: "muted" }, "Nothing needs you.")))));

    if (!host) capacity.replaceChildren(panel("Server capacity", {}, empty("host", "Reading the server…", "")));
    else {
      const live = (dbs || []).filter(isLive);
      const cpu = live.reduce((n, d) => n + (d.cpus || 0), 0), mem = live.reduce((n, d) => n + (d.memoryMiB || 0), 0) * 1048576;
      const used = live.reduce((n, d) => n + (d.live?.memory || 0), 0);
      const line = (label, value, share) => h("div", { class: "cap-line" }, h("div", null, h("span", null, label), h("strong", null, value)), bar(share));
      capacity.replaceChildren(panel("Server capacity", { description: host.name },
        h("div", { class: "cap" },
          line("Memory in use", `${bytes(used)} of ${bytes(host.memoryBytes)}`, used / host.memoryBytes),
          line("Memory limits", `${bytes(mem)} of ${bytes(host.memoryBytes)}`, mem / host.memoryBytes),
          line("CPU limits", `${Number(cpu.toFixed(2))} of ${host.cpus} vCPU`, cpu / host.cpus)),
        h("p", { class: "muted small" }, `Docker ${host.dockerVersion} · disk limits ${host.volumeQuota?.state === "supported" ? "on" : "unavailable"}`)));
    }

    // One line per action: its outcome, or "started" while it still runs.
    const all = state.activity || [];
    const key = (e) => `${e.name}|${e.action}|${e.actor}`; // newest first, so an outcome comes before its start
    const events = all.filter((e, i) => e.result !== "started" || !all.slice(0, i).some(x => x.result !== "started" && key(x) === key(e))).slice(0, 6);
    activity.replaceChildren(panel("Recent activity", { actions: [h("a", { class: "btn small ghost", href: "#/events" }, "View all")] },
      !state.activity ? empty("bell", "Reading history…", "") : events.length ? h("ul", { class: "activity" }, events.map(e => h("li", { class: e.result },
        icon(e.result === "completed" ? "ok" : e.result === "failed" ? "error" : "pending"),
        h("div", null, h("strong", null, e.action),
          h("small", { class: "muted" }, e.kind === "user" ? e.name : h("a", { href: `#/databases/${e.name}` }, e.name),
            e.actor && e.actor !== "system" && e.actor !== e.name ? ` · ${e.actor}` : "", ` · ${ago(e.at, now)}`)))))
        : empty("bell", "Nothing yet", "Actions show up here.")));
  }
  render();
  const stop = subscribe(render);
  window.addEventListener("openrds:start", render);
  return { el, title: "Dashboard", nav: "dashboard", crumbs: [["Dashboard"]], destroy: () => { stop(); window.removeEventListener("openrds:start", render); } };
}
