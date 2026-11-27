// Time-series charts drawn as SVG. Thin 2px lines, a light area wash for a
// single series, hairline grid, a crosshair tooltip shared by a chart group,
// keyboard reading, and a table view as the accessible twin of every chart.
import { h, s, icon, clock, nextId } from "./lib.mjs";

const HEIGHT = 196;
const PAD = { top: 16, right: 50, bottom: 28, left: 58 };

function niceStep(raw) {
  const p = 10 ** Math.floor(Math.log10(raw));
  const f = raw / p;
  return (f <= 1 ? 1 : f <= 2 ? 2 : f <= 2.5 ? 2.5 : f <= 5 ? 5 : 10) * p;
}

// Round the axis to clean steps; byte axes step in powers of 1024.
function yScale(max, binary) {
  let unit = 1;
  if (binary) while (max / unit >= 1024) unit *= 1024;
  const step = niceStep(max / unit / 4) * unit;
  const top = Math.max(step, Math.ceil(max / step - 1e-9) * step);
  const ticks = [];
  for (let v = 0; v <= top + step / 2; v += step) ticks.push(v);
  return { top, ticks };
}

const TICK_SPACING = { 3600: 900, 10800: 1800, 21600: 3600, 43200: 7200, 86400: 14400 };

function nearest(points, t) {
  let lo = 0, hi = points.length - 1;
  if (hi < 0) return -1;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (points[mid].t < t) lo = mid + 1; else hi = mid;
  }
  return lo > 0 && Math.abs(points[lo - 1].t - t) <= Math.abs(points[lo].t - t) ? lo - 1 : lo;
}

export function chartGroup() {
  const charts = new Set();
  return {
    add(chart) { charts.add(chart); chart.group = this; return chart; },
    hover(t) { charts.forEach(c => c.showAt(t)); },
  };
}

export function timeChart({ title, subtitle = "", series, format, axisFormat = format, binary = false, floor = 1, domainMax = null, reference = null, empty = "No readings in this range yet." }) {
  const titleID = nextId("chart-title");
  const legend = series.length > 1 ? h("ul", { class: "legend" }, series.map(sr => h("li", null, h("span", { class: `key ${sr.cls}` }), sr.label))) : null;
  const headline = h("span", { class: "chart-now" });
  const tableBtn = h("button", { type: "button", class: "btn small ghost", "aria-pressed": "false" }, icon("table"), "Table");
  const plot = h("div", { class: "plot", tabindex: "0", role: "group", "aria-labelledby": titleID });
  const tip = h("div", { class: "chart-tip", role: "status", hidden: true });
  const table = h("div", { class: "chart-table", hidden: true });
  const fig = h("figure", { class: "chart" },
    h("figcaption", { class: "chart-head" },
      h("div", null, h("h3", { id: titleID }, title), subtitle && h("p", { class: "muted" }, subtitle)),
      h("div", { class: "chart-tools" }, headline, tableBtn)),
    legend, plot, table);
  plot.append(tip);
  let data = { points: [], from: 0, to: 1, step: 30 }, width = 0, layer = null, hoverIndex = -1;
  let geometry = null;

  const values = (p) => series.map(sr => sr.value(p));

  function render() {
    width = plot.clientWidth || 640;
    const { points, from, to, step } = data;
    const max = Math.max(floor, ...points.flatMap(values), reference?.value || 0);
    const { top, ticks } = yScale(domainMax ? Math.max(domainMax(points) || 0, max) : max * 1.12, binary);
    const innerW = Math.max(10, width - PAD.left - PAD.right), innerH = HEIGHT - PAD.top - PAD.bottom;
    const x = (t) => PAD.left + (t - from) / (to - from) * innerW;
    const y = (v) => PAD.top + (1 - Math.min(v, top) / top) * innerH;
    geometry = { x, y, from, to, innerW };
    const svg = s("svg", { width, height: HEIGHT, viewBox: `0 0 ${width} ${HEIGHT}`, "aria-hidden": "true" });
    const grid = s("g", { class: "grid" });
    for (const v of ticks) {
      grid.append(s("line", { x1: PAD.left, x2: width - PAD.right, y1: y(v), y2: y(v), class: v === 0 ? "axis" : "" }),
        s("text", { x: PAD.left - 8, y: y(v) + 4, "text-anchor": "end", class: "tick" }, axisFormat(v)));
    }
    const spacing = TICK_SPACING[to - from] || Math.round((to - from) / 4);
    const offset = -new Date().getTimezoneOffset() * 60;
    for (let t = Math.ceil((from + offset) / spacing) * spacing - offset; t <= to; t += spacing) {
      const tx = x(t);
      if (tx < PAD.left + 18 || tx > width - PAD.right - 12) continue;
      grid.append(s("text", { x: tx, y: HEIGHT - 8, "text-anchor": "middle", class: "tick" }, clock(t * 1000)));
    }
    svg.append(grid);
    if (reference && reference.value <= top) {
      svg.append(s("g", { class: "reference" }, s("line", { x1: PAD.left, x2: width - PAD.right, y1: y(reference.value), y2: y(reference.value) }),
        s("text", { x: width - PAD.right, y: y(reference.value) - 6, "text-anchor": "end", class: "tick" }, reference.label)));
    }
    const gap = Math.max(step, 1) * 2.5;
    const ends = [];
    series.forEach((sr, i) => {
      let line = "", area = "", run = [];
      const closeRun = () => {
        if (run.length > 1 && series.length === 1) {
          area += `M${run[0][0]},${y(0)}L${run.map(p => p.join(",")).join("L")}L${run.at(-1)[0]},${y(0)}Z`;
        }
        run = [];
      };
      points.forEach((p, j) => {
        const px = x(p.t).toFixed(1), py = y(sr.value(p)).toFixed(1);
        const broken = j === 0 || p.t - points[j - 1].t > gap;
        if (broken) closeRun();
        line += `${broken ? "M" : "L"}${px},${py}`;
        run.push([px, py]);
      });
      closeRun();
      if (area) svg.append(s("path", { d: area, class: `area ${sr.cls}` }));
      if (line) svg.append(s("path", { d: line, class: `line ${sr.cls}` }));
      const last = points.at(-1);
      if (last && to - last.t <= gap) ends.push({ sr, i, cx: x(last.t), cy: y(sr.value(last)) });
    });
    for (const e of ends) svg.append(s("circle", { cx: e.cx, cy: e.cy, r: 4, class: `dot ${e.sr.cls}` }));
    // Direct end labels only when they cannot collide; the legend carries identity otherwise.
    if (ends.length > 1 && ends.every((e, i) => ends.every((o, j) => i === j || Math.abs(e.cy - o.cy) >= 14))) {
      for (const e of ends) svg.append(s("text", { x: e.cx + 8, y: e.cy + 4, class: "end-label" }, e.sr.label));
    }
    layer = s("g", { class: "hover" });
    svg.append(layer);
    plot.querySelector("svg")?.remove();
    plot.querySelector(".chart-empty")?.remove();
    plot.prepend(svg);
    if (!points.length) plot.append(h("p", { class: "chart-empty" }, empty));
    const latest = points.at(-1);
    headline.textContent = latest && to - latest.t <= gap ? series.map(sr => (series.length > 1 ? `${sr.label} ` : "") + format(sr.value(latest))).join(" · ") : "";
    plot.setAttribute("aria-description", latest ? `Latest at ${clock(latest.t * 1000)}: ${series.map(sr => `${sr.label} ${format(sr.value(latest))}`).join(", ")}. Use the arrow keys to read earlier values.` : empty);
    if (hoverIndex >= 0) showIndex(Math.min(hoverIndex, points.length - 1));
    if (!table.hidden) renderTable();
  }

  function showIndex(i) {
    const p = data.points[i];
    if (!p || !geometry) { hide(); return; }
    hoverIndex = i;
    const cx = geometry.x(p.t);
    layer.replaceChildren(s("line", { x1: cx, x2: cx, y1: PAD.top, y2: HEIGHT - PAD.bottom, class: "crosshair" }),
      ...series.map(sr => s("circle", { cx, cy: geometry.y(sr.value(p)), r: 4, class: `dot ${sr.cls}` })));
    tip.replaceChildren(h("p", { class: "tip-time" }, new Date(p.t * 1000).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" })),
      ...series.map(sr => h("p", { class: "tip-row" }, h("span", { class: `key line ${sr.cls}` }), h("strong", null, format(sr.value(p))), h("span", null, sr.label))));
    tip.hidden = false;
    const tipW = tip.offsetWidth || 160;
    const left = cx + 12 + tipW > width ? cx - 12 - tipW : cx + 12;
    tip.style.left = `${Math.max(4, left)}px`;
    tip.style.top = `${PAD.top}px`;
  }

  function hide() {
    hoverIndex = -1;
    layer?.replaceChildren();
    tip.hidden = true;
  }

  const chart = {
    el: fig,
    group: null,
    update(next) { data = next; fig.classList.remove("loading"); render(); },
    setLoading(on) { fig.classList.toggle("loading", on); },
    showAt(t) {
      if (t === null) { hide(); return; }
      const i = nearest(data.points, t);
      if (i < 0 || Math.abs(data.points[i].t - t) > Math.max(data.step, 1) * 1.5) hide(); else showIndex(i);
    },
    destroy() { observer.disconnect(); },
  };
  const share = (t) => chart.group ? chart.group.hover(t) : chart.showAt(t);

  plot.addEventListener("pointermove", (e) => {
    if (!geometry || !data.points.length) return;
    const box = plot.getBoundingClientRect();
    const px = e.clientX - box.left;
    if (px < PAD.left || px > width - PAD.right) { share(null); return; }
    const t = geometry.from + (px - PAD.left) / geometry.innerW * (geometry.to - geometry.from);
    share(data.points[nearest(data.points, t)].t);
  });
  plot.addEventListener("pointerleave", () => share(null));
  plot.addEventListener("blur", () => share(null));
  plot.addEventListener("focus", () => { if (data.points.length) share(data.points.at(-1).t); });
  plot.addEventListener("keydown", (e) => {
    const n = data.points.length;
    if (!n) return;
    const moves = { ArrowLeft: -1, ArrowRight: 1, Home: -n, End: n };
    if (e.key === "Escape") { share(null); return; }
    if (!(e.key in moves)) return;
    e.preventDefault();
    const i = Math.max(0, Math.min(n - 1, (hoverIndex < 0 ? n - 1 : hoverIndex) + moves[e.key]));
    share(data.points[i].t);
  });

  function renderTable() {
    const rows = [...data.points].reverse();
    table.replaceChildren(rows.length ? h("table", null,
      h("caption", { class: "sr-only" }, title),
      h("thead", null, h("tr", null, h("th", { scope: "col" }, "Time"), series.map(sr => h("th", { scope: "col" }, sr.label)))),
      h("tbody", null, rows.map(p => h("tr", null, h("td", null, new Date(p.t * 1000).toLocaleString(undefined, { dateStyle: "short", timeStyle: "short" })),
        series.map(sr => h("td", null, format(sr.value(p)))))))) : h("p", { class: "muted" }, empty));
  }
  tableBtn.addEventListener("click", () => {
    const on = table.hidden;
    table.hidden = !on; plot.hidden = on;
    if (legend) legend.hidden = on;
    tableBtn.setAttribute("aria-pressed", String(on));
    if (on) renderTable();
  });

  let frame = 0;
  const observer = new ResizeObserver(() => {
    cancelAnimationFrame(frame);
    frame = requestAnimationFrame(() => { if (plot.clientWidth && plot.clientWidth !== width) render(); });
  });
  observer.observe(plot);
  return chart;
}

// A small trend line of recent readings for previews and summary tiles. Gaps
// (readings the engine did not answer) break the line instead of dropping to 0.
export function sparkline(values, { max = null, label = "", cls = "" } = {}) {
  const W = 120, H = 34;
  const svg = s("svg", { class: `spark ${cls}`.trim(), viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", role: "img", "aria-label": label || null, "aria-hidden": label ? null : "true" });
  const known = values.filter(Number.isFinite);
  if (known.length < 2) {
    svg.append(s("line", { class: "spark-base", x1: 0, x2: W, y1: H - 1, y2: H - 1 }));
    return svg;
  }
  const top = max ?? (Math.max(...known) * 1.2 || 1);
  const x = (i) => (i / (values.length - 1)) * W;
  const y = (v) => H - 2 - Math.min(1, Math.max(0, v / top)) * (H - 4);
  let line = "", area = "", run = [];
  const flush = () => {
    if (run.length > 1) {
      line += run.map(([i, v], k) => `${k ? "L" : "M"}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join("");
      area += `M${x(run[0][0]).toFixed(1)},${H}` + run.map(([i, v]) => `L${x(i).toFixed(1)},${y(v).toFixed(1)}`).join("") + `L${x(run.at(-1)[0]).toFixed(1)},${H}Z`;
    }
    run = [];
  };
  values.forEach((v, i) => Number.isFinite(v) ? run.push([i, v]) : flush());
  flush();
  svg.append(s("path", { class: "spark-area", d: area }), s("path", { class: "spark-line", d: line }));
  return svg;
}
