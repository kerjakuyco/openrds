// Moving a database to another server: pick where, see the new address,
// confirm. A replica there copies it, writes pause for a few seconds, and
// the copy takes over. The database page follows each step.
import { h, icon, api, modal, bytes, nextId, toast, copyText, isRunning, alertBox } from "./lib.mjs";
import { track, can } from "./store.mjs";
import { servers, serverOf, roomOf } from "./servers.mjs";
import { routeTo } from "./replicas.mjs";

// Why the database can't move now, if it can't.
export function moveBlocker(d) {
  if (!can("operator")) return "Viewers can’t change this.";
  if (d.replicaOf) return `It’s a replica. Create one of ${d.replicaOf} on the other server instead.`;
  if ((d.replicas || []).length) return "Promote or delete its read replicas first.";
  if (d.busy) return "Another operation is running.";
  if (d.pendingRestore) return "The restore didn’t finish.";
  if (!isRunning(d)) return "Start it first.";
  if (!d.tlsReady) return "Reboot it once first.";
  if (!targetsFor(d).length) return "Add another server first.";
  return "";
}

// Servers it can move to: reachable ones it has a route to first, then the
// one with the most room.
function targetsFor(d) {
  return servers().filter(s => s.id !== (d.server || "local") && (s.reachable || s.local))
    .sort((a, b) => Boolean(routeTo(d, a.id).error) - Boolean(routeTo(d, b.id).error) || roomOf(b) - roomOf(a));
}

function copyChip(text) {
  return h("button", { type: "button", class: "endpoint", title: `Copy ${text}`, "aria-label": `Copy ${text}`,
    onclick: async () => { const ok = await copyText(text); toast(ok ? `Copied ${text}.` : "Copy failed.", ok ? "success" : "warn"); } },
  h("span", null, text), icon("copy"));
}

export const MOVE_STEPS = ["Copy", "Catch up", "Switch", "Clean up"];

export function moveDatabase(d) {
  const from = serverOf(d);
  const targets = targetsFor(d);
  let chosen = targets[0];
  const group = nextId("to");
  const port = h("input", { type: "number", min: "1024", max: "65535", value: String(d.port), class: "num", id: nextId("port") });
  const address = h("dl", { class: "move-address" });
  let submit = null;

  const cards = h("div", { class: "move-targets", role: "radiogroup", "aria-label": "Move to" }, targets.map(s => {
    const r = routeTo(d, s.id);
    const radio = h("input", { type: "radio", name: group, value: s.id, checked: s === chosen, disabled: Boolean(r.error), id: nextId("target") });
    radio.addEventListener("change", () => { chosen = s; render(); });
    return h("label", { class: `card-choice${r.error ? " disabled" : ""}`, for: radio.id }, radio,
      h("span", { class: "card-body" },
        h("span", { class: "move-target-head" }, h("strong", null, s.name), r.error ? null : h("span", { class: `chip ${r.route === "private" ? "good" : "info"}` }, r.label)),
        h("small", null, s.info ? `${bytes(roomOf(s))} memory free · ${s.info.cpus} vCPU` : ""),
        r.error ? h("small", { class: "reason" }, r.error) : null));
  }));

  function render() {
    if (!chosen) return;
    const p = Number(port.value) || d.port;
    const rows = [[`On ${chosen.name}`, copyChip(`127.0.0.1:${p}`)]];
    if (d.privateAccess && chosen.privateAddress) rows.push(["Private network", copyChip(`${chosen.privateAddress}:${p}`)]);
    if (d.publicAccess) rows.push(["Internet", chosen.publicAddress || d.publicHost ? copyChip(`${d.publicHost && d.publicHost !== from?.publicAddress ? d.publicHost : chosen.publicAddress}:${d.publicPort}`) : h("span", { class: "muted" }, `${chosen.name}’s address, port ${d.publicPort}`)]);
    address.replaceChildren(...rows.map(([k, v]) => h("div", null, h("dt", null, k), h("dd", null, v))));
    if (submit) submit.textContent = `Move to ${chosen.name}`;
  }
  port.addEventListener("input", render);

  const dlg = modal({
    title: `Move ${d.name}`, kicker: `From ${from?.name || "this server"}`, confirm: "Move", wide: true,
    body: targets.length ? [
      h("p", { class: "field-label" }, "Move to"),
      cards,
      h("p", { class: "field-label" }, "New address"),
      address,
      h("p", { class: "hint" }, "Update your apps to use it after the move."),
      h("ol", { class: "move-plan" },
        h("li", null, h("strong", null, "Copy. "), `${d.name} keeps running while it’s copied.`),
        h("li", null, h("strong", null, "Switch. "), "Writes pause for a few seconds while the copy catches up."),
        h("li", null, h("strong", null, "Done. "), `Same name, users, settings and backups. The old copy on ${from?.name || "this server"} is removed.`)),
      h("details", { class: "disclosure" }, h("summary", null, "Options"),
        h("div", { class: "field" }, h("label", { class: "field-label", for: port.id }, "Port"), port, h("p", { class: "hint" }, "The same port, unless it’s taken there."))),
    ] : [alertBox("Add another server first.", "info")],
    run: async () => {
      if (!chosen) throw new Error("Add another server first.");
      const r = routeTo(d, chosen.id);
      if (r.error) throw new Error(r.error);
      const to = chosen;
      track(`Move ${d.name} to ${to.name}`, api(`instances/${d.name}/move`, { method: "POST", body: { server: to.id, port: Number(port.value) } }),
        { success: `${d.name} now runs on ${to.name}.` });
    },
  });
  submit = dlg.querySelector("button[type=submit]");
  render();
}

// The steps of a running move, for the database page.
export function moveProgress(d) {
  if (!d.move) return null;
  return h("div", { class: "alert info move-progress", role: "status" },
    h("div", { class: "stack" },
      h("strong", null, `Moving to ${d.move.to}`),
      h("ol", { class: "move-steps" }, MOVE_STEPS.map((label, i) => h("li", { class: i + 1 < d.move.step ? "done" : i + 1 === d.move.step ? "current" : "" },
        i + 1 < d.move.step ? icon("check") : i + 1 === d.move.step ? icon("spinner", "icon spin") : h("span", { class: "move-dot" }), label))),
      h("small", null, d.move.step === 3 ? "Writes are paused for a few seconds." : "It keeps running. Writes pause for a few seconds near the end.")));
}
