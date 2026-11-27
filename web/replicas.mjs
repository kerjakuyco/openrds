// Read replicas: create one, see whether it keeps up, promote it.
import { h, icon, btn, api, modal, panel, alertBox, dataTable, empty, keyValues, mib, sizeLabel, ago, isRunning } from "./lib.mjs";
import { state, instance, track, can } from "./store.mjs";
import { SIZES, field, select } from "./forms.mjs";
import { servers, multiServer, serverOf } from "./servers.mjs";

// How a copy on another server reaches d, or why it can't, in a few words.
export function routeTo(d, serverId) {
  const from = serverOf(d), to = servers().find(s => s.id === serverId);
  if (!to || to.id === (d.server || "local")) return { route: "", label: "Same server" };
  if (from?.privateAddress && to.privateAddress) return { route: "private", label: "Private network", detail: `${from.privateAddress}:${d.port}` };
  const host = d.publicHost || from?.publicAddress;
  if (!d.publicAccess) return { error: "No private network between them. Add private addresses on Servers, or turn on public access." };
  if (!host) return { error: `Set ${from?.name || "its server"}’s public address on Servers.` };
  if (!to.publicAddress) return { error: `Set ${to.name}’s public address on Servers.` };
  return { route: "public", label: "Internet", detail: `${host}:${d.publicPort}` };
}
export const ROUTE_LABEL = { private: "Private network", public: "Internet" };

const MAX_REPLICAS = 5;

const STATES = {
  replicating: ["Replicating", "good", "ok"],
  syncing: ["Copying data", "info", "spinner"],
  connecting: ["Connecting", "warn", "alert"],
  broken: ["Not replicating", "critical", "error"],
  stopped: ["Stopped", "neutral", "stopped"],
  unknown: ["No answer", "warn", "alert"],
};

export function replicationChip(st) {
  const [label, tone, name] = STATES[st?.state] || ["Checking", "neutral", "pending"];
  return h("span", { class: `status ${tone}` }, icon(name, `icon${name === "spinner" ? " spin" : ""}`), label);
}

export const lagLabel = (s) => s == null ? "—" : s < 1 ? "Under a second" : s < 120 ? `${Math.round(s)} s` : s < 7200 ? `${Math.round(s / 60)} min` : `${(s / 3600).toFixed(1)} h`;

// Why this database can't get a new replica now, if it can't.
export function replicaBlocker(d) {
  if (!can("operator")) return "Viewers can’t change this.";
  if (d.replicaOf) return `Create replicas from its source, ${d.replicaOf}.`;
  if (d.busy) return "Another operation is running.";
  if (d.pendingRestore) return "The restore didn’t finish.";
  if (!isRunning(d)) return "Start the database first.";
  if (!d.tlsReady) return "Reboot once first.";
  if ((d.replicas || []).length >= MAX_REPLICAS) return `It has ${MAX_REPLICAS} replicas, the most it can have.`;
  return "";
}

export function createReplica(d) {
  const all = state.instances || [];
  const used = new Set(all.flatMap(x => [x.port, x.publicPort, x.pooler?.port]).filter(Boolean));
  const where = select(servers().filter(s => s.reachable || s.local).map(s => [s.id, `${s.name}${s.id === (d.server || "local") ? ` · with ${d.name}` : ""}`]), d.server || "local");
  const routeNote = h("p", { class: "hint" });
  const showRoute = () => {
    const r = routeTo(d, where.value);
    routeNote.className = r.error ? "hint warn-text" : "hint";
    routeNote.textContent = r.error || (r.route ? `Follows over the ${r.label.toLowerCase()} (${r.detail}). Only that server gets in.` : "Follows over the server’s own Docker network.");
  };
  where.addEventListener("change", showRoute);
  showRoute();
  let free = d.port + 1;
  while (used.has(free)) free++;
  const names = new Set(all.map(x => x.name));
  let suggested = `${d.name.slice(0, 32)}-replica`;
  for (let n = 2; names.has(suggested); n++) suggested = `${d.name.slice(0, 29)}-replica-${n}`;
  const name = h("input", { type: "text", value: suggested, spellcheck: "false", autocomplete: "off" });
  const port = h("input", { type: "number", min: "1024", max: "65535", value: String(free), class: "num" });
  const same = SIZES.find(z => z.cpus === d.cpus && z.memoryMiB === d.memoryMiB);
  const size = select([["same", `Same as ${d.name} · ${sizeLabel(d)}`],
    ...SIZES.filter(z => z !== same).map(z => [z.value, `${z.title} · ${z.cpus} vCPU · ${mib(z.memoryMiB)}`])], "same");
  modal({
    title: "Create read replica", kicker: d.name, confirm: "Create replica", wide: true,
    body: [
      h("p", { class: "muted" }, `A read-only copy of ${d.name} that follows its changes within seconds. Send reads to it to take load off ${d.name}.`),
      h("div", { class: "form-grid" }, field("Replica DB identifier", name), field("Host port", port)),
      multiServer() ? h("div", null, field("Server", where, { hint: "On another server, it keeps your data safe if this server is lost." }), routeNote) : null,
      field("Instance size", size, { hint: d.engine === "valkey" ? "Keep at least the source’s memory: Valkey holds all data in memory." : "Smaller is fine for light read traffic." }),
      h("ul", { class: "plain-list" },
        h("li", null, h("strong", null, "Same users. "), `It has the users and passwords of ${d.name}. Manage them there.`),
        h("li", null, h("strong", null, "Same settings. "), `${d.diskGiB ? `${d.diskGiB} GiB storage, ` : ""}parameters and TLS as on ${d.name}.`),
        h("li", null, h("strong", null, "No downtime. "), `${d.name} keeps running while its data is copied. Other changes to it wait until then.`)),
    ],
    run: async () => {
      const body = { name: name.value.trim(), port: Number(port.value) };
      if (multiServer() && where.value !== (d.server || "local")) {
        const r = routeTo(d, where.value);
        if (r.error) throw new Error(r.error);
        body.server = where.value;
      }
      if (size.value !== "same") {
        const z = SIZES.find(x => x.value === size.value);
        Object.assign(body, { cpus: z.cpus, memoryMiB: z.memoryMiB });
      }
      track(`Create read replica ${body.name}`, api(`instances/${d.name}/replicas`, { method: "POST", body }),
        { success: `${body.name} is replicating from ${d.name}.`, onDone: (err) => { if (!err) location.hash = `#/databases/${body.name}`; } });
    },
  });
}

export function confirmPromote(d) {
  const behind = d.replication?.lagSeconds;
  modal({
    title: `Promote ${d.name}?`, kicker: "Promote read replica", confirm: "Promote", tone: "danger", typed: d.name,
    body: [
      h("p", { class: "muted" }, `${d.name} stops following ${d.replicaOf} and becomes a standalone database that accepts writes. To replicate again, create a new replica.`),
      h("ul", { class: "plain-list" },
        h("li", null, h("strong", null, "Same endpoint. "), "Its port, data and settings stay."),
        h("li", null, h("strong", null, "Its own users. "), `It starts with the users and passwords of ${d.replicaOf}, then they change separately.`),
        h("li", null, h("strong", null, `${d.replicaOf} is not changed. `), "Writes to it no longer reach this database.")),
      behind > 5 ? alertBox(`It is ${lagLabel(behind)} behind. Changes it hasn’t received yet won’t be in it.`, "warn") : null,
    ],
    run: async () => {
      track(`Promote ${d.name}`, api(`instances/${d.name}/promote`, { method: "POST" }), { success: `${d.name} is now a standalone database.` });
    },
  });
}

// The Replication panel: a replica's source and state, or a source's replicas.
export function replicationPanel(d) {
  if (d.replicaOf) return replicaPanel(d);
  const why = replicaBlocker(d);
  const create = btn([icon("copy"), "Create read replica"], () => createReplica(d), "small");
  create.disabled = Boolean(why);
  if (why) create.title = why;
  const rows = (d.replicas || []).map(name => instance(name) || { name });
  return panel("Read replicas", { counter: rows.length, description: "Read-only copies that follow this database. Send reads to them.",
    actions: can("operator") ? [create] : [] },
  rows.length ? dataTable({ caption: "Read replicas", rows, columns: [
    { key: "name", label: "Replica", value: r => h("span", { class: "stack" }, h("a", { href: `#/databases/${r.name}` }, r.name),
      r.sourceRoute ? h("small", null, `On ${r.serverName} · ${ROUTE_LABEL[r.sourceRoute]}`) : null) },
    { key: "state", label: "Replication", value: r => replicationChip(r.replication) },
    { key: "lag", label: "Lag", value: r => lagLabel(r.replication?.lagSeconds) },
    { key: "port", label: "Port", cls: "num", value: r => r.port ? h("code", null, String(r.port)) : "—" },
  ] }) : empty("copy", "No read replicas", "Up to 5, each on its own port."));
}

function replicaPanel(d) {
  const chipSlot = h("span", null, replicationChip(d.replication));
  const lag = h("span", null, lagLabel(d.replication?.lagSeconds));
  const checked = h("span", { class: "muted" }, d.replication?.at && !d.replication.at.startsWith("0001") ? ago(d.replication.at) : "—");
  const detail = h("div");
  function show(st) {
    chipSlot.replaceChildren(replicationChip(st));
    lag.textContent = lagLabel(st?.lagSeconds);
    checked.textContent = st?.at && !st.at.startsWith("0001") ? ago(st.at) : "—";
    const tone = st?.state === "broken" ? "error" : st?.state === "connecting" || st?.state === "unknown" ? "warn" : "";
    detail.replaceChildren(tone && st.detail ? alertBox(st.detail, tone) : "");
  }
  async function refresh(b) {
    if (b) b.disabled = true;
    try { show(await api(`instances/${d.name}/replication`)); }
    catch (err) { detail.replaceChildren(alertBox(err.message, "warn")); }
    finally { if (b) b.disabled = false; }
  }
  const promote = btn([icon("arrow"), "Promote"], () => confirmPromote(d), "small");
  promote.disabled = d.busy || !isRunning(d);
  if (promote.disabled) promote.title = d.busy ? "Another operation is running." : "Start the replica first.";
  show(d.replication);
  if (isRunning(d)) refresh();
  return panel("Replication", { description: "Read-only. Writes go to the source.",
    actions: [btn([icon("refresh"), "Refresh"], (e) => refresh(e.currentTarget), "small ghost"), can("operator") ? promote : null].filter(Boolean) },
  keyValues([["Role", "Read replica"], ["Source", h("a", { href: `#/databases/${d.replicaOf}` }, d.replicaOf)], ["State", chipSlot], ["Lag", lag], ["Checked", checked],
    d.sourceRoute ? ["Connects over", ROUTE_LABEL[d.sourceRoute]] : null], "cols-4"),
  detail);
}
