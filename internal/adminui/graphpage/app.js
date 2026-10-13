"use strict";

// The page talks only to its own origin and writes every server-supplied
// string with textContent, so a battery name can never become markup. No
// style is set from script, so the page runs under style-src 'self'; the
// chart library (uPlot) sets styles through the CSSOM, which that policy
// allows.
(function () {
  const DATA = "data";
  const POLL_MS = 5000;
  const FETCH_TIMEOUT_MS = 10000;
  const CHART_HEIGHT = 300;
  const SUMMARY_LINES = 20;
  const PALETTE = ["#4cc9f0", "#f72585", "#b8de29", "#ffb703", "#9d7bff", "#2ec4b6", "#ff7f50", "#8b949e", "#80ed99", "#ff99c8"];
  const AXIS = "#8b949e";
  const GRID = "rgba(139,148,158,0.25)";

  const $ = (id) => document.getElementById(id);

  function el(tag, text, className) {
    const e = document.createElement(tag);
    if (text !== undefined) e.textContent = text;
    if (className) e.className = className;
    return e;
  }

  // AbortSignal.timeout is missing from older browsers; the fallback aborts
  // a controller from a timer, and done() clears that timer.
  function timeoutSignal(ms) {
    if (typeof AbortSignal !== "undefined" && typeof AbortSignal.timeout === "function") {
      return { signal: AbortSignal.timeout(ms), done() {} };
    }
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), ms);
    return { signal: ctrl.signal, done() { clearTimeout(timer); } };
  }

  const colorOf = new Map();
  function colorFor(key) {
    if (!colorOf.has(key)) colorOf.set(key, PALETTE[colorOf.size % PALETTE.length]);
    return colorOf.get(key);
  }

  // ---- the latest table, in the table pages' shape ----

  function fill(node, c) {
    if (c.kind === "time") {
      const t = el("time", c.text);
      if (c.datetime) t.dateTime = c.datetime;
      node.append(t);
      return node;
    }
    node.textContent = c.text;
    return node;
  }

  function latestSection(s) {
    const root = el("section", undefined, "section");
    root.append(el("h2", s.heading));
    for (const p of s.prose || []) root.append(el("p", p, "prose"));
    const body = s.body || {};
    const rows = body.rows || [];
    if (rows.length === 0) {
      if (s.empty) root.append(el("p", s.empty, "empty"));
      return root;
    }
    const headRow = el("tr");
    for (const name of body.columns || []) headRow.append(el("th", name));
    const head = el("thead");
    head.append(headRow);
    const tbody = el("tbody");
    for (const row of rows) {
      const tr = el("tr");
      for (const c of row) tr.append(fill(el("td"), c));
      tbody.append(tr);
    }
    const t = el("table");
    t.append(head, tbody);
    root.append(t);
    return root;
  }

  // ---- the chart ----

  let chart = null;
  let chartSig = "";

  function summarize(series, empty) {
    const box = $("chart-summary");
    if (series.length === 0) {
      box.textContent = empty;
      return;
    }
    const parts = series.slice(0, SUMMARY_LINES).map((s) => {
      const last = s.points.length > 0 ? s.points[s.points.length - 1][1] : null;
      return s.name + ": " + (last === null ? "no value" : last);
    });
    let text = series.length + " series. Latest values: " + parts.join("; ");
    if (series.length > SUMMARY_LINES) text += "; and " + (series.length - SUMMARY_LINES) + " more.";
    box.textContent = text;
  }

  function drawChart(series, unit, empty) {
    const box = $("chart");
    summarize(series, empty);
    if (series.length === 0) {
      if (chart) chart.destroy();
      chart = null;
      chartSig = "";
      box.textContent = empty;
      return;
    }
    if (typeof uPlot === "undefined") {
      box.textContent = "The chart library did not load.";
      return;
    }
    // All lines share one timestamp axis, with null where a line has no
    // point; spanGaps joins them.
    const times = new Set();
    const byLine = series.map((s) => {
      const m = new Map();
      for (const p of s.points) { m.set(p[0], p[1]); times.add(p[0]); }
      return m;
    });
    const xs = [...times].sort((a, b) => a - b);
    const data = [xs, ...byLine.map((m) => xs.map((t) => (m.has(t) ? m.get(t) : null)))];
    const sig = unit + "\n" + series.map((s) => s.mrid + "|" + s.name).join("\n");
    if (chart && sig === chartSig) {
      chart.setData(data);
      return;
    }
    if (chart) chart.destroy();
    box.textContent = "";
    const axes = [
      { stroke: AXIS, grid: { stroke: GRID }, ticks: { stroke: GRID } },
      { label: unit, stroke: AXIS, grid: { stroke: GRID }, ticks: { stroke: GRID } },
    ];
    const lines = [{}, ...series.map((s) => ({ label: s.name, stroke: colorFor(s.mrid), width: 1.5, spanGaps: true }))];
    chart = new uPlot({ width: box.clientWidth || 600, height: CHART_HEIGHT, scales: { x: { time: true } }, axes, series: lines }, data, box);
    chartSig = sig;
  }

  // ---- the picker ----

  let selected = [];
  let choicesSig = "";

  function renderChoices(d) {
    const known = new Set(d.choices.map((c) => c.id));
    selected = selected.filter((id) => known.has(id));
    const atMax = selected.length >= d.max;
    const sig = JSON.stringify([d.choices, selected, d.max]);
    $("picker-hint").textContent = selected.length === 0
      ? "Nothing picked: the chart shows the newest batteries. Pick up to " + d.max + "."
      : selected.length + " of " + d.max + " picked.";
    if (sig === choicesSig) return;
    choicesSig = sig;
    const kids = d.choices.map((c) => {
      const label = el("label", undefined, selected.includes(c.id) || !atMax ? "" : "off");
      const box = el("input");
      box.type = "checkbox";
      box.value = c.id;
      box.checked = selected.includes(c.id);
      box.disabled = atMax && !box.checked;
      box.addEventListener("change", () => onPick(c.id, box.checked));
      label.append(box, " " + c.label);
      return label;
    });
    $("choices").replaceChildren(...kids);
  }

  function onPick(id, on) {
    selected = selected.filter((s) => s !== id);
    if (on) selected.push(id);
    choicesSig = "";
    reload();
  }

  // ---- loading ----

  function query() {
    return selected.length === 0 ? "" : "?" + selected.map((id) => "sel=" + encodeURIComponent(id)).join("&");
  }

  function setStatus(text, isError) {
    const s = $("status");
    s.textContent = text;
    s.className = isError ? "error" : "muted";
  }

  function render(d) {
    document.title = d.title;
    $("title").textContent = d.title;
    $("prose").replaceChildren(...d.prose.map((p) => el("p", p, "prose")));
    renderChoices(d);
    drawChart(d.series, d.unit, d.empty);
    $("latest").replaceChildren(latestSection(d.latest));
  }

  let timer = null;
  let inflight = false;
  let again = false;

  async function load() {
    inflight = true;
    let limit = null;
    try {
      limit = timeoutSignal(FETCH_TIMEOUT_MS);
      const r = await fetch(DATA + query(), { cache: "no-store", signal: limit.signal });
      if (!r.ok) throw new Error("HTTP " + r.status);
      render(await r.json());
      setStatus("updated " + new Date().toISOString(), false);
    } catch (e) {
      $("latest").replaceChildren();
      drawChart([], "", "");
      setStatus("unavailable: " + e.message, true);
    } finally {
      if (limit) limit.done();
    }
    inflight = false;
  }

  // A hidden tab stops polling and starts again, at once, when it shows. A
  // pick while a load is out loads again as soon as that one ends.
  async function tick() {
    timer = null;
    if (document.hidden) return;
    do {
      again = false;
      await load();
    } while (again && !document.hidden);
    timer = setTimeout(tick, POLL_MS);
  }

  function reload() {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
    if (inflight) {
      again = true;
      return;
    }
    tick();
  }

  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && timer === null && !inflight) tick();
  });
  $("clear").addEventListener("click", () => {
    selected = [];
    choicesSig = "";
    reload();
  });
  if (typeof window !== "undefined" && window.addEventListener) {
    window.addEventListener("resize", () => {
      if (chart) chart.setSize({ width: $("chart").clientWidth || 600, height: CHART_HEIGHT });
    });
  }

  tick();
})();
