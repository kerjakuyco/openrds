// Performance insights for one database: what runs now, the statements that
// take the most time, and Valkey's slow log. Statement text can hold data,
// so this needs the Operator role.
import { h, icon, btn, api, toast, modal, panel, dataTable, empty, alertBox, ago, when, clock, isRunning } from "./lib.mjs";
import { can } from "./store.mjs";

const duration = (seconds) => seconds < 60 ? `${seconds.toFixed(seconds < 10 ? 1 : 0)} s`
  : seconds < 3600 ? `${Math.floor(seconds / 60)} min ${Math.round(seconds % 60)} s` : `${Math.floor(seconds / 3600)} h ${Math.round(seconds % 3600 / 60)} min`;
const millis = (ms) => ms < 1 ? `${(ms * 1000).toFixed(0)} µs` : ms < 1000 ? `${ms.toFixed(ms < 10 ? 2 : 1)} ms` : duration(ms / 1000);
const count = (n) => Number(n).toLocaleString();
const statementCell = (text) => h("code", { class: "statement", title: text || "" }, text || "—");

// The engine's names for its statistics, used in titles and notes.
export const ENGINE_TERMS = {
  postgres: { ops: "Transactions", hit: "Buffer cache hit ratio", hitWhat: "table and index reads served from shared_buffers" },
  mysql: { ops: "Statements", hit: "Buffer pool hit ratio", hitWhat: "InnoDB reads served from the buffer pool" },
  valkey: { ops: "Commands", hit: "Keyspace hit ratio", hitWhat: "key lookups that found the key" },
};

export function insightsTab(d) {
  if (!can("operator")) {
    return { el: panel("Performance insights", {}, empty("key", "This needs the Operator role", "Statement text can include data, so viewers see the charts on Monitoring instead.")) };
  }
  if (!isRunning(d)) {
    return { el: panel("Performance insights", {}, empty("stopped", "The database isn’t running", "Start it to see what it runs.")) };
  }
  const valkey = d.engine === "valkey";
  const note = h("p", { class: "muted small" });
  const running = h("div", { class: "flush" }, empty("pulse", "Loading…", ""));
  const statements = h("div", { class: "flush" }, empty("list", "Loading…", ""));
  const notice = h("div", { hidden: true });
  const refresh = btn([icon("refresh"), "Refresh"], () => load(), "small");
  const reset = btn(valkey ? "Clear slow log" : "Reset statistics", () => confirmReset(), "small");

  function confirmReset() {
    modal({
      title: valkey ? "Clear the slow log?" : "Reset statement statistics?", kicker: d.name, confirm: valkey ? "Clear" : "Reset",
      body: h("p", { class: "muted" }, valkey ? "Entries start again from now." : "Counts and times start again from now, for example to see the effect of a new index."),
      run: async () => {
        await api(`instances/${d.name}/insights/reset`, { method: "POST" });
        toast(valkey ? "Slow log cleared." : "Statistics reset.", "success");
        load();
      },
    });
  }

  function confirmCancel(s) {
    modal({
      title: "Cancel this statement?", kicker: `Session ${s.id}`, confirm: "Cancel statement", tone: "danger", cancel: "Keep running",
      body: [h("p", { class: "muted" }, "The statement stops with an error and its transaction rolls back. The session stays connected."), statementCell(s.query)],
      run: async () => {
        await api(`instances/${d.name}/sessions/${s.id}/cancel`, { method: "POST" });
        toast(`Statement in session ${s.id} cancelled.`, "success");
        load();
      },
    });
  }

  function render(v) {
    notice.hidden = true;
    if (!valkey) {
      running.replaceChildren(v.running.length ? dataTable({ caption: "Statements running now", rows: v.running, columns: [
        { key: "id", label: "Session", value: s => h("code", null, s.id) },
        { key: "user", label: "User", value: s => s.user || "—" },
        { key: "wait", label: "Doing", cls: "wrap", value: s => h("div", { class: "stack" }, h("span", null, s.wait || s.state || "—"), s.wait && s.state ? h("small", null, s.state) : null) },
        { key: "seconds", label: "For", cls: "num", value: s => h("span", { class: s.seconds >= 60 ? "status warn" : "" }, duration(s.seconds)) },
        { key: "query", label: "Statement", cls: "wrap", value: s => statementCell(s.query) },
        { key: "cancel", label: h("span", { class: "sr-only" }, "Cancel"), cls: "actions", value: s => btn("Cancel", () => confirmCancel(s), "small") },
      ] }) : empty("ok", "Nothing running", "No statements are running right now."));
    }
    if (valkey) {
      statements.replaceChildren(v.slow?.length ? dataTable({ caption: "Slow log", rows: v.slow, columns: [
        { key: "at", label: "When", value: e => h("span", { title: when(e.at) }, ago(e.at)) },
        { key: "micros", label: "Took", cls: "num", value: e => millis(e.micros / 1000) },
        { key: "command", label: "Command", cls: "wrap", value: e => statementCell(e.command) },
        { key: "client", label: "Client", value: e => h("code", null, e.client || "—") },
      ] }) : empty("ok", "No slow commands", "Commands slower than slowlog-log-slower-than appear here."));
    } else if (v.statements) {
      statements.replaceChildren(v.statements.length ? dataTable({ caption: "Top statements by total time", rows: v.statements, columns: [
        { key: "query", label: "Statement", cls: "wrap", value: s => statementCell(s.query) },
        { key: "calls", label: "Calls", cls: "num", value: s => count(s.calls) },
        { key: "total", label: "Total time", cls: "num", value: s => millis(s.totalMs) },
        { key: "mean", label: "Mean", cls: "num", value: s => millis(s.meanMs) },
        { key: "rows", label: "Rows", cls: "num", value: s => count(s.rows) },
      ] }) : empty("list", "No statements yet", "Statements appear as clients run them."));
    } else {
      statements.replaceChildren(empty("sliders", "Statement statistics are off", v.statementsNote || ""));
    }
    note.textContent = `Updated ${clock(Date.now())}`;
  }

  let busy = false;
  async function load() {
    if (busy) return;
    busy = true;
    try { render(await api(`instances/${d.name}/insights`)); }
    catch (err) { notice.replaceChildren(alertBox(err.message, "error")); notice.hidden = false; }
    finally { busy = false; }
  }

  const el = h("div", { class: "stack-gap" },
    h("div", { class: "filter-row" }, note, h("div", { class: "row" }, refresh)),
    notice,
    valkey ? null : panel("Running now", { description: "Statements in progress, longest first. Cancel stops a statement and keeps its session." }, running),
    panel(valkey ? "Slow log" : "Top statements", {
      description: valkey ? "The slowest recent commands, newest first." : "Where the database spends its time, since the statistics were last reset. Values are replaced by placeholders.",
      actions: [reset],
    }, statements));
  load();
  const timer = setInterval(() => { if (!document.hidden) load(); }, 10000);
  return { el, destroy() { clearInterval(timer); } };
}
