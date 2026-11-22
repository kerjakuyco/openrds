// DOM, formatting and request helpers shared by every view. Text always enters
// the document as text nodes, never as markup.

const SVG = "http://www.w3.org/2000/svg";
const PROPS = new Set(["value", "checked", "disabled", "hidden", "selected", "required", "readOnly", "open", "tabIndex", "indeterminate"]);

const flat = (children) => children.flat(Infinity).filter(c => c !== null && c !== undefined && c !== false)
  .map(c => typeof c === "number" ? String(c) : c);

export function h(tag, props = null, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props || {})) {
    if (value === null || value === undefined || value === false) continue;
    if (key === "class") el.className = value;
    else if (key === "style") Object.assign(el.style, value);
    else if (key === "dataset") Object.assign(el.dataset, value);
    else if (key.startsWith("on") && typeof value === "function") el.addEventListener(key.slice(2), value);
    else if (PROPS.has(key)) el[key] = value;
    else el.setAttribute(key, value === true ? "" : String(value));
  }
  el.append(...flat(children));
  return el;
}

export function s(tag, attrs = null, ...children) {
  const el = document.createElementNS(SVG, tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value !== null && value !== undefined && value !== false) el.setAttribute(key, String(value));
  }
  el.append(...flat(children));
  return el;
}

export function icon(name, cls = "icon") {
  return s("svg", { class: cls, "aria-hidden": "true" }, s("use", { href: `/icons.svg#${name}` }));
}

export const btn = (label, onclick, cls = "", attrs = {}) =>
  h("button", { type: "button", class: `btn ${cls}`.trim(), onclick, ...attrs }, label);

export const chip = (label, tone = "neutral") => h("span", { class: `chip ${tone}` }, label);

let uid = 0;
export const nextId = (prefix = "id") => `${prefix}-${++uid}`;

// ---------- formatting

const UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
export function bytes(n) {
  if (!Number.isFinite(n)) return "—";
  let i = 0;
  while (Math.abs(n) >= 1024 && i < UNITS.length - 1) { n /= 1024; i++; }
  const digits = i === 0 ? 0 : Math.abs(n) < 10 ? 1 : 0;
  return `${n.toFixed(digits)} ${UNITS[i]}`;
}
export const rate = (n) => `${bytes(n)}/s`;
export const mib = (n) => n >= 1024 && n % 1024 === 0 ? `${n / 1024} GiB` : `${n} MiB`;
export const cores = (n) => `${Number(n.toFixed(2))} vCPU`;
export const percent = (n) => `${n < 10 ? n.toFixed(1) : Math.round(n)}%`;
export const when = (v) => new Date(v).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
export const clock = (v) => new Date(v).toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
export const pretty = (v) => v ? v.charAt(0).toUpperCase() + v.slice(1) : "";
export const plural = (n, word, many = `${word}s`) => `${n} ${n === 1 ? word : many}`;

export function ago(v, now = Date.now()) {
  const sec = Math.round((now - new Date(v).getTime()) / 1000);
  const future = sec < 0, a = Math.abs(sec);
  const text = a < 45 ? "moments" : a < 3600 ? plural(Math.round(a / 60), "min", "min")
    : a < 86400 ? plural(Math.round(a / 3600), "hour") : plural(Math.round(a / 86400), "day");
  if (text === "moments") return future ? "in a moment" : "just now";
  return future ? `in ${text}` : `${text} ago`;
}

export function windowLabel(w) {
  return w ? `${w.replace("-", "–")} UTC` : "—";
}

export function sizeLabel(d) {
  const cpu = d.cpus ? cores(d.cpus) : "Shared CPU";
  const mem = d.memoryMiB ? mib(d.memoryMiB) : "shared memory";
  return `${cpu} · ${mem}`;
}

// ---------- requests

export class ApiError extends Error {
  constructor(message, status) { super(message); this.status = status; }
}
let unauthorized = () => {};
export const onUnauthorized = (fn) => { unauthorized = fn; };

export async function api(path, { method = "GET", body, token } = {}) {
  const headers = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (token) headers.Authorization = `Bearer ${token}`;
  let res;
  try {
    res = await fetch(`/api/${path}`, { method, headers, credentials: "same-origin", cache: "no-store",
      body: body === undefined ? undefined : JSON.stringify(body) });
  } catch {
    throw new ApiError("No response. Is your SSH tunnel open?", 0);
  }
  if (!res.ok) {
    let message;
    try { message = (await res.json()).error; } catch { /* a proxy page rather than JSON */ }
    if (res.status === 401) unauthorized();
    throw new ApiError(message || `Request failed (HTTP ${res.status}).`, res.status);
  }
  return res.status === 204 ? null : res.json();
}

export async function copyText(text) {
  try { await navigator.clipboard.writeText(text); return true; } catch { return false; }
}

// ---------- feedback

const toneIcon = { info: "info", success: "ok", error: "error", warn: "alert" };

export function toast(message, tone = "info", { title, sticky = tone === "error" } = {}) {
  const item = h("div", { class: `toast ${tone}`, role: tone === "error" ? "alert" : "status" });
  const close = h("button", { type: "button", class: "icon-btn ghost", "aria-label": "Dismiss notification", onclick: () => item.remove() }, icon("close"));
  item.append(icon(toneIcon[tone] || "info", "icon toast-icon"), h("div", { class: "toast-body" }, title && h("strong", null, title), h("p", null, message)), close);
  document.getElementById("toasts").append(item);
  if (!sticky) setTimeout(() => item.remove(), 7000);
  return item;
}

export function alertBox(message, tone = "info", ...extra) {
  return h("div", { class: `alert ${tone}`, role: tone === "error" ? "alert" : "status" }, icon(toneIcon[tone] || "info"), h("div", null, message, ...extra));
}

export function setAlert(el, message = "", tone = "error") {
  el.hidden = !message;
  el.className = `alert ${tone}`;
  el.replaceChildren(...(message ? [icon(toneIcon[tone] || "info"), h("div", null, message)] : []));
}

// A modal for a decision or a short task. run() resolving closes it; a
// rejection stays inside the dialog so the operator can correct and retry.
export function modal({ title, kicker = "", body = [], confirm = "Confirm", tone = "primary", typed = "", run, cancel = "Cancel", wide = false }) {
  const titleID = nextId("modal-title");
  const note = h("div", { class: "alert error", role: "alert", hidden: true });
  const submit = h("button", { type: "submit", class: `btn ${tone}` }, confirm);
  const form = h("form", { class: "modal-form", method: "dialog" });
  let gate = null;
  if (typed) {
    const inputID = nextId("confirm");
    gate = h("input", { id: inputID, type: "text", autocomplete: "off", spellcheck: "false", placeholder: typed });
    form.append(h("label", { for: inputID, class: "field-label" }, "To confirm, type ", h("code", null, typed)), gate);
    gate.addEventListener("input", () => { submit.disabled = gate.value.trim() !== typed; });
    submit.disabled = true;
  }
  const dlg = h("dialog", { class: `modal${wide ? " wide" : ""}`, "aria-labelledby": titleID },
    h("header", { class: "modal-head" }, h("div", null, kicker && h("p", { class: "kicker" }, kicker), h("h2", { id: titleID }, title)),
      h("button", { type: "button", class: "icon-btn ghost", "aria-label": "Close", onclick: () => dlg.close() }, icon("close"))),
    form);
  form.prepend(...flat([body]));
  form.append(note, h("footer", { class: "modal-foot" }, cancel && h("button", { type: "button", class: "btn", onclick: () => dlg.close() }, cancel), submit));
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    if (submit.disabled) return;
    submit.disabled = true; note.hidden = true;
    const label = submit.textContent; submit.textContent = "Working…";
    try { await run?.(form); dlg.close(); }
    catch (err) { setAlert(note, err.message); }
    finally { submit.textContent = label; submit.disabled = Boolean(gate) && gate.value.trim() !== typed; }
  });
  dlg.addEventListener("close", () => dlg.remove());
  document.body.append(dlg);
  dlg.showModal();
  (gate || submit).focus();
  return dlg;
}

// ---------- menus

// A menu button (WAI-ARIA menu pattern): arrow keys move, Escape closes.
export function menu(label, items, { cls = "", align = "end", labelIcon = null, trigger: custom = null, up = false, ariaLabel = null, chevron = true } = {}) {
  const listID = nextId("menu");
  const trigger = custom || h("button", { type: "button", class: `btn ${cls}`.trim() },
    labelIcon && icon(labelIcon), label, chevron && icon("chevron", "icon chevron"));
  trigger.setAttribute("aria-haspopup", "menu");
  trigger.setAttribute("aria-expanded", "false");
  trigger.setAttribute("aria-controls", listID);
  if (ariaLabel) trigger.setAttribute("aria-label", ariaLabel);
  const list = h("div", { id: listID, class: `menu ${align}${up ? " up" : ""}`, role: "menu", hidden: true });
  const wrap = h("div", { class: "menu-wrap" }, trigger, list);
  const entries = () => [...list.querySelectorAll("[role^=menuitem]:not([aria-disabled=true])")];
  const outside = (e) => { if (!wrap.contains(e.target)) close(false); };
  const away = () => close(false);
  // Inside a scrolling table or the sidebar the menu would be clipped, so it
  // floats over the page next to its button, above it when there is no room below.
  function place() {
    if (!wrap.closest(".table-wrap, .sidebar")) return false;
    list.classList.add("floating");
    const r = trigger.getBoundingClientRect(), w = list.offsetWidth, ht = list.offsetHeight;
    const left = align === "end" ? r.right - w : r.left;
    list.style.left = `${Math.max(8, Math.min(left, innerWidth - w - 8))}px`;
    list.style.top = `${r.bottom + 6 + ht <= innerHeight ? r.bottom + 6 : Math.max(8, r.top - 6 - ht)}px`;
    return true;
  }
  function open() {
    list.hidden = false; trigger.setAttribute("aria-expanded", "true");
    document.addEventListener("pointerdown", outside);
    if (place()) { addEventListener("scroll", away, true); addEventListener("resize", away); }
    entries()[0]?.focus();
  }
  function close(focus = true) {
    if (list.hidden) return;
    list.hidden = true; trigger.setAttribute("aria-expanded", "false");
    document.removeEventListener("pointerdown", outside);
    removeEventListener("scroll", away, true); removeEventListener("resize", away);
    if (focus) trigger.focus();
  }
  trigger.addEventListener("click", () => list.hidden ? open() : close());
  trigger.addEventListener("keydown", (e) => { if (e.key === "ArrowDown") { e.preventDefault(); open(); } });
  list.addEventListener("keydown", (e) => {
    const all = entries(), i = all.indexOf(document.activeElement);
    if (e.key === "Escape") { e.preventDefault(); close(); }
    else if (e.key === "Tab") close(false);
    else if (e.key === "ArrowDown") { e.preventDefault(); all[(i + 1) % all.length]?.focus(); }
    else if (e.key === "ArrowUp") { e.preventDefault(); all[(i - 1 + all.length) % all.length]?.focus(); }
    else if (e.key === "Home") { e.preventDefault(); all[0]?.focus(); }
    else if (e.key === "End") { e.preventDefault(); all.at(-1)?.focus(); }
  });
  for (const item of items.filter(Boolean)) {
    if (item === "-") { list.append(h("div", { class: "menu-sep", role: "separator" })); continue; }
    if (item.heading) { list.append(h("p", { class: "menu-heading", role: "presentation" }, item.heading)); continue; }
    if (item.element) { list.append(item.element); continue; } // custom content, such as a profile or a theme switch
    const radio = item.checked !== undefined;
    const entry = h("button", { type: "button", role: radio ? "menuitemradio" : "menuitem", tabindex: "-1", class: item.danger ? "danger-item" : "",
      "aria-checked": radio ? String(item.checked) : null,
      "aria-disabled": item.disabled ? "true" : null, title: item.disabled && item.reason ? item.reason : null },
      item.icon && icon(item.icon), h("span", null, item.label, item.disabled && item.reason && h("small", null, item.reason)),
      radio && item.checked ? icon("check", "icon tick") : null);
    entry.addEventListener("click", () => { if (item.disabled) return; close(); item.run(); });
    list.append(entry);
  }
  return wrap;
}

// ---------- status

export function statusOf(d) {
  if (d.busy) return { label: d.status === "not provisioned" ? "Creating" : "In progress", tone: "info", icon: "spinner" };
  if (d.pendingRestore) return { label: "Restore incomplete", tone: "critical", icon: "error" };
  switch (d.status) {
    case "running": return { label: "Running", tone: "good", icon: "ok" };
    case "starting": return { label: "Starting", tone: "info", icon: "spinner" };
    case "stopped": return { label: "Stopped", tone: "neutral", icon: "stopped" };
    case "unhealthy": return { label: "Unhealthy", tone: "critical", icon: "error" };
    case "restarting": return { label: "Restarting", tone: "warn", icon: "spinner" };
    case "not provisioned": return { label: "Not provisioned", tone: "warn", icon: "alert" };
    default: return { label: pretty(d.status), tone: "warn", icon: "alert" };
  }
}

export function status(d) {
  const st = statusOf(d);
  return h("span", { class: `status ${st.tone}` }, icon(st.icon, `icon${st.icon === "spinner" ? " spin" : ""}`), st.label);
}

export const isLive = (d) => ["running", "starting", "unhealthy", "restarting"].includes(d.status);
export const isRunning = (d) => d.status === "running" && !d.pendingRestore;
export const needsAttention = (d) => !d.busy && (d.pendingRestore || !["running", "stopped", "starting"].includes(d.status));

// A meter: the filled share reads against a lighter track of the same ramp.
export function meter(value, max, label) {
  const share = max > 0 ? Math.min(1, Math.max(0, value / max)) : 0;
  const tone = share >= 0.9 ? "critical" : share >= 0.75 ? "warn" : "ok";
  const bar = h("span", { class: "meter-fill" });
  bar.style.width = `${(share * 100).toFixed(1)}%`;
  return h("span", { class: `meter ${tone}`, role: "img", "aria-label": label }, bar);
}

export function keyValues(pairs, cls = "") {
  return h("dl", { class: `kv ${cls}`.trim() }, pairs.filter(Boolean).map(([k, v]) => h("div", null, h("dt", null, k), h("dd", null, v ?? "—"))));
}

// Tables, empty states and .flush slots run edge to edge; everything else
// is grouped into padded .panel-body blocks.
const FLUSH = ".table-wrap, .empty, .flush, .recs, .timeline, .all-good";
export function panel(title, { actions = [], counter = null, description = "", id = null, cls = "" } = {}, ...body) {
  const section = h("section", { class: `panel ${cls}`.trim(), id },
    h("header", { class: "panel-head" },
      h("div", null, h("h2", null, title, counter !== null && h("span", { class: "counter" }, String(counter))), description && h("p", { class: "muted" }, description)),
      actions.length ? h("div", { class: "panel-actions" }, actions) : null));
  let group = null;
  for (const item of flat(body)) {
    if (item instanceof Element && item.matches(FLUSH)) { section.append(item); group = null; continue; }
    if (!group) { group = h("div", { class: "panel-body" }); section.append(group); }
    group.append(item);
  }
  return section;
}

export function empty(symbol, title, hint, ...actions) {
  return h("div", { class: "empty" }, h("span", { class: "empty-icon" }, icon(symbol)), h("h3", null, title), hint && h("p", null, hint), actions.length ? h("div", { class: "row center" }, actions) : null);
}

// A sortable data table. columns: [{ key, label, value(row) -> node, sort(row) -> comparable, cls }]
// A details panel in the style of a cloud console: columns, each with a
// heading and stacked label/value pairs, separated by thin rules.
export function infoPanel(title, columns, options = {}) {
  const cols = columns.filter(Boolean);
  return panel(title, { ...options, cls: `info-panel ${options.cls || ""}`.trim() },
    h("div", { class: `info-cols n${cols.length}` }, cols.map(([heading, pairs]) => h("section", { class: "info-col" },
      heading ? h("h3", null, heading) : null,
      h("dl", null, pairs.filter(Boolean).map(([k, v]) => h("div", null, h("dt", null, k), h("dd", null, v ?? "—"))))))));
}

export function dataTable({ columns, rows, sortKey, sortDir = "asc", onSort, rowClass, caption, id = null }) {
  const head = h("tr", null, columns.map(c => {
    const th = h("th", { scope: "col", class: c.cls || null });
    if (c.sort && onSort) {
      const active = sortKey === c.key;
      th.setAttribute("aria-sort", active ? (sortDir === "asc" ? "ascending" : "descending") : "none");
      th.append(h("button", { type: "button", class: "sort-btn", onclick: () => onSort(c.key) }, c.label,
        icon(active ? (sortDir === "asc" ? "sort-up" : "sort-down") : "sort", "icon sort-icon")));
    } else th.append(c.label);
    return th;
  }));
  let list = rows;
  const col = columns.find(c => c.key === sortKey);
  if (col?.sort) {
    list = [...rows].sort((a, b) => {
      const x = col.sort(a), y = col.sort(b);
      const order = typeof x === "string" ? x.localeCompare(y) : (x > y) - (x < y);
      return sortDir === "asc" ? order : -order;
    });
  }
  const body = h("tbody", null, list.map(r => h("tr", { class: rowClass?.(r) || null }, columns.map(c => h("td", { class: c.cls || null, "data-label": typeof c.label === "string" ? c.label : null }, c.value(r))))));
  return h("div", { class: "table-wrap" }, h("table", { id }, caption && h("caption", { class: "sr-only" }, caption), h("thead", null, head), body));
}

// A copyable block of text such as a command, a URI or a snippet.
export function codeBlock(text, { label = "Copy", onCopy = null, caption = "" } = {}) {
  const copy = btn([icon("copy"), label], async () => {
    const ok = await copyText(text);
    toast(ok ? "Copied to the clipboard." : "Copy failed. Select and copy it.", ok ? "success" : "warn");
    if (ok) onCopy?.();
  }, "small ghost");
  return h("div", { class: "code-wrap" }, caption && h("p", { class: "code-caption" }, caption), h("div", { class: "code" }, h("pre", null, text), copy));
}

// A small toggletip: a "?" button that reveals a short explanation.
export function help(text, label = "More information") {
  const tipID = nextId("tip");
  const tip = h("span", { id: tipID, class: "tip-bubble", role: "status", hidden: true });
  const button = h("button", { type: "button", class: "help-btn", "aria-label": label, "aria-expanded": "false", "aria-controls": tipID }, "?");
  const close = (e) => { if (!wrap.contains(e.target)) toggle(false); };
  function toggle(open) {
    tip.hidden = !open;
    button.setAttribute("aria-expanded", String(open));
    tip.textContent = open ? text : "";
    if (open) document.addEventListener("pointerdown", close); else document.removeEventListener("pointerdown", close);
  }
  button.addEventListener("click", () => toggle(tip.hidden));
  button.addEventListener("keydown", (e) => { if (e.key === "Escape") toggle(false); });
  const wrap = h("span", { class: "help" }, button, tip);
  return wrap;
}

export function initials(name = "") {
  const parts = name.trim().split(/[\s._-]+/).filter(Boolean);
  return ((parts[0]?.[0] || "?") + (parts[1]?.[0] || "")).toUpperCase();
}

export const avatar = (name, cls = "") => h("span", { class: `avatar ${cls}`.trim(), "aria-hidden": "true" }, initials(name));

export const ROLES = {
  admin: { label: "Admin", text: "Everything, including accounts." },
  operator: { label: "Operator", text: "Manage databases, backups, users and access." },
  viewer: { label: "Viewer", text: "Look only. No passwords or downloads." },
};
