// Shared state: the inventory read from the control plane, refreshed on an
// interval, plus operations this browser started and is still waiting on.
// Long lists are not part of it: backups and events come as counts and the
// latest few, and pages read their own rows.
import { api, toast } from "./lib.mjs";

export const state = {
  instances: null, backups: null, activity: null, failures: null, host: null, engines: null, users: null, servers: null,
  me: null, signedIn: false, syncedAt: 0, problems: [],
};

// When this browser last opened Events; failures since then get a badge.
const SEEN = "openrds-events-seen";
export function eventsSeen() {
  try { return localStorage.getItem(SEEN) || ""; } catch { return ""; }
}
export function markEventsSeen() {
  try { localStorage.setItem(SEEN, new Date().toISOString()); } catch { /* private mode */ }
  if (state.failures) { state.failures = { ...state.failures, total: 0 }; emit(new Set(["failures"])); }
}

const LEVEL = { viewer: 1, operator: 2, admin: 3 };
// Whether the signed-in person has at least this role. The server enforces the same rule.
export const can = (role) => (LEVEL[state.me?.role] || 0) >= LEVEL[role];

const listeners = new Set();
export const subscribe = (fn) => { listeners.add(fn); return () => listeners.delete(fn); };
const emit = (changed) => listeners.forEach(fn => fn(changed));

let syncID = 0;
const RESOURCES = {
  instances: () => "instances",
  backups: () => "backups?limit=1", // counts and sizes by kind, and the newest backup
  activity: () => "activity?limit=20", // the latest events
  failures: () => `activity?result=failed&limit=0&since=${encodeURIComponent(eventsSeen() || new Date(Date.now() - 86400000).toISOString())}`,
  host: () => "host",
  storage: () => "storage",
  servers: () => "servers",
  users: () => "users",
};

export async function sync() {
  if (!state.signedIn) return;
  const id = ++syncID;
  const keys = Object.keys(RESOURCES).filter(k => k !== "users" || can("admin"));
  const results = await Promise.allSettled(keys.map(k => api(RESOURCES[k]())));
  if (id !== syncID || !state.signedIn) return;
  const changed = new Set(), problems = [];
  results.forEach((res, i) => {
    const key = keys[i];
    if (res.status === "fulfilled") {
      if (JSON.stringify(state[key]) !== JSON.stringify(res.value)) changed.add(key);
      state[key] = res.value;
    } else problems.push(`${key}: ${res.reason.message}`);
  });
  if (JSON.stringify(problems) !== JSON.stringify(state.problems)) changed.add("problems");
  state.problems = problems;
  state.syncedAt = Date.now();
  emit(changed);
}

export async function loadEngines() {
  if (!state.engines) state.engines = await api("engines");
  return state.engines;
}

export const go = (hash) => { location.hash = hash; };
export function setMe(id) { state.me = id; emit(new Set(["me"])); }
export const engine = (key) => state.engines?.find(e => e.key === key);
export const instance = (name) => state.instances?.find(d => d.name === name);

export function reset() {
  syncID++;
  for (const key of Object.keys(RESOURCES)) state[key] = null;
  state.me = null;
  state.problems = [];
  operations.clear();
}

// Operations run synchronously on the server and can take minutes. Track them
// here so a toast reports the outcome after the operator navigates elsewhere.
export const operations = new Map();
let opID = 0;

export function track(label, promise, { success, onDone } = {}) {
  const id = ++opID;
  operations.set(id, label);
  emit(new Set(["operations"]));
  sync();
  promise.then(
    (value) => { if (success !== false) toast(typeof success === "function" ? success(value) : (success || `${label} finished.`), "success"); onDone?.(null, value); },
    (err) => { toast(err.message, "error", { title: `${label} failed` }); onDone?.(err); },
  ).finally(() => {
    operations.delete(id);
    emit(new Set(["operations"]));
    sync();
  });
  return promise;
}

let timer = 0;
export function startPolling() {
  clearInterval(timer);
  timer = setInterval(() => { if (state.signedIn && !document.hidden) sync(); }, 15000);
}
document.addEventListener("visibilitychange", () => { if (!document.hidden && state.signedIn) sync(); });
