// Database operations shared by the list and the database page.
import { h, api, modal, alertBox, isLive, isRunning, nextId } from "./lib.mjs";
import { track, go } from "./store.mjs";

const post = (path) => api(path, { method: "POST" });

export function start(d) {
  return track(`Start ${d.name}`, post(`instances/${d.name}/start`), { success: `${d.name} is running.` });
}

export function takeSnapshot(d) {
  return track(`Snapshot of ${d.name}`, post(`instances/${d.name}/backups`), { success: `Snapshot of ${d.name} saved.` });
}

export function confirmStop(d) {
  modal({
    title: `Stop ${d.name}?`, kicker: "Stop temporarily", confirm: "Stop database",
    body: h("p", { class: "muted" }, "Connections close. Data stays. Backups pause until you start it."),
    run: async () => { track(`Stop ${d.name}`, post(`instances/${d.name}/stop`), { success: `${d.name} stopped.` }); },
  });
}

export function confirmReboot(d) {
  modal({
    title: `Reboot ${d.name}?`, kicker: "Reboot", confirm: "Reboot database",
    body: [h("p", { class: "muted" }, "Back in about a minute. Clients reconnect."),
      d.applied && alertBox("Pending changes apply now.", "info")],
    run: async () => { track(`Reboot ${d.name}`, post(`instances/${d.name}/reboot`), { success: `${d.name} rebooted.` }); },
  });
}

export function confirmDelete(d, { after = "#/databases" } = {}) {
  if (d.deletionProtection) {
    modal({
      title: `${d.name} is protected`, kicker: "Deletion protection", confirm: "Modify database",
      body: h("p", { class: "muted" }, "Turn off deletion protection in Modify first."),
      run: async () => go(`#/databases/${d.name}/modify`),
    });
    return;
  }
  const canSnapshot = isRunning(d);
  const final = h("input", { type: "checkbox", class: "switch", role: "switch", checked: canSnapshot, disabled: !canSnapshot, id: nextId("final") });
  modal({
    title: `Delete ${d.name}?`, kicker: "Delete database", confirm: "Delete", tone: "danger", typed: d.name,
    body: [
      h("p", { class: "muted" }, "Removes the database and its data. Backups are kept."),
      h("label", { class: "check", for: final.id }, final, h("span", null, h("strong", null, "Create a final snapshot"),
        h("small", null, canSnapshot ? "If it fails, nothing is deleted." : "Needs a running database."))),
      !canSnapshot && isLive(d) ? alertBox("Not healthy, so the snapshot may fail.", "warn") : null,
    ],
    run: async () => {
      const query = final.checked ? "?finalSnapshot=true" : "";
      track(`Delete ${d.name}`, api(`instances/${d.name}${query}`, { method: "DELETE" }), { success: final.checked ? `${d.name} was deleted after a final snapshot.` : `${d.name} was deleted.` });
      go(after);
    },
  });
}

export function databaseActions(d) {
  const live = isLive(d);
  return [
    { label: "Start", icon: "play", disabled: d.busy || live, reason: live ? "Already running" : d.busy ? "Another operation is running" : "", run: () => start(d) },
    { label: "Stop temporarily", icon: "stop", disabled: d.busy || !live, reason: !live ? "Not running" : "", run: () => confirmStop(d) },
    { label: "Reboot", icon: "reboot", disabled: d.busy || !live, reason: !live ? "Not running" : "", run: () => confirmReboot(d) },
    "-",
    { label: "Take snapshot", icon: "camera", disabled: d.busy || !isRunning(d), reason: isRunning(d) ? "" : "The database must be running", run: () => takeSnapshot(d) },
    { label: "Modify", icon: "edit", disabled: d.busy || d.pendingRestore, run: () => go(`#/databases/${d.name}/modify`) },
    "-",
    { label: "Delete", icon: "trash", danger: true, disabled: d.busy, run: () => confirmDelete(d) },
  ];
}
