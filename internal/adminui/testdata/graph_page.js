"use strict";
// Runs the graph page script on a minimal fake DOM, a fake fetch and a fake
// uPlot, then prints what the page shows as JSON. Driven by TestGraphPage*.
// Stdin is {"source": <page script>, "scenarios": {name: {...}}} so a test
// can run a mutated copy of the script without writing a file. A scenario
// has: body (the answer to any query), bodies (answers by query string,
// "" for none), status, hidden, noTimeout, noChart (no uPlot at all) and
// steps: "hide", "show", "tick", "clear", "pick:<id>", "unpick:<id>".
const vm = require("vm");

class Node_ {
  constructor(tag, counters) {
    this.tagName = tag;
    this.children = [];
    this.className = "";
    this.dateTime = "";
    this.value = "";
    this.checked = false;
    this.disabled = false;
    this.clientWidth = 640;
    this._text = null;
    this._c = counters;
    this._listeners = {};
  }
  get textContent() {
    if (this._text !== null) return this._text;
    return this.children.map((c) => (typeof c === "string" ? c : c.textContent)).join("");
  }
  set textContent(v) { this._text = String(v); this.children = []; }
  set innerHTML(v) { this._c.innerHTML++; }
  get style() { return new Proxy({}, { set: () => { this._c.style++; return true; } }); }
  append(...kids) { this._text = null; this.children.push(...kids); }
  replaceChildren(...kids) { this._text = null; this.children = kids; }
  addEventListener(name, fn) { (this._listeners[name] = this._listeners[name] || []).push(fn); }
  fire(name) { for (const fn of this._listeners[name] || []) fn(); }
}

function walk(node, fn) {
  for (const c of node.children) {
    if (typeof c === "string") continue;
    fn(c);
    walk(c, fn);
  }
}

function dumpLatest(root) {
  const out = { heading: "", prose: [], empty: "", columns: null, rows: null };
  const sec = root.children[0];
  if (!sec) return out;
  for (const c of sec.children) {
    if (c.tagName === "h2") out.heading = c.textContent;
    else if (c.tagName === "p" && c.className === "prose") out.prose.push(c.textContent);
    else if (c.tagName === "p" && c.className === "empty") out.empty = c.textContent;
    else if (c.tagName === "table") {
      const [head, body] = c.children;
      out.columns = head.children[0].children.map((th) => th.textContent);
      out.rows = body.children.map((tr) => tr.children.map((td) => {
        const t = td.children.find((k) => typeof k !== "string" && k.tagName === "time");
        return { text: td.textContent, dateTime: t ? t.dateTime : "" };
      }));
    }
  }
  return out;
}

async function settle() {
  for (let i = 0; i < 6; i++) await new Promise((r) => setImmediate(r));
}

async function run(source, sc) {
  const counters = { innerHTML: 0, style: 0 };
  const byId = new Map();
  const listeners = {};
  const document = {
    hidden: Boolean(sc.hidden),
    title: "",
    getElementById(id) {
      if (!byId.has(id)) byId.set(id, new Node_("div", counters));
      return byId.get(id);
    },
    createElement(tag) { return new Node_(tag, counters); },
    addEventListener(name, fn) { (listeners[name] = listeners[name] || []).push(fn); },
  };
  const urls = [];
  let pending = [];
  let nextTimer = 0;
  let timeoutCalls = 0;
  let controllers = 0;
  let status_ = sc.status || 200;
  async function fetch(url) {
    urls.push(url);
    const q = url.startsWith("data") ? url.slice(4) : null;
    const status = q === null ? 404 : status_;
    const body = sc.bodies && Object.prototype.hasOwnProperty.call(sc.bodies, q) ? sc.bodies[q] : sc.body;
    return { ok: status < 400, status, json: async () => body };
  }
  const chartLog = { created: 0, destroyed: 0, setData: 0, opts: null, data: null };
  class UPlot {
    constructor(opts, data, container) {
      chartLog.created++;
      chartLog.opts = opts;
      chartLog.data = data;
      this.container = container;
    }
    setData(data) { chartLog.setData++; chartLog.data = data; }
    destroy() { chartLog.destroyed++; }
    setSize() {}
  }
  class AbortController_ {
    constructor() { controllers++; this.signal = { aborted: false }; }
    abort() { this.signal.aborted = true; }
  }
  const ctx = {
    document, fetch, Date, String, Error, Array, Map, Set, Object, JSON, encodeURIComponent,
    AbortSignal: sc.noTimeout ? {} : { timeout: () => { timeoutCalls++; return undefined; } },
    AbortController: AbortController_,
    setTimeout: (fn) => { const id = ++nextTimer; pending.push({ id, fn }); return id; },
    clearTimeout: (id) => { pending = pending.filter((t) => t.id !== id); },
  };
  if (!sc.noChart) ctx.uPlot = UPlot;
  vm.createContext(ctx);
  vm.runInContext(source, ctx);
  await settle();

  function box(id) {
    let found = null;
    walk(document.getElementById("choices"), (n) => { if (n.tagName === "input" && n.value === id) found = n; });
    return found;
  }

  const steps = [];
  for (const step of sc.steps || []) {
    if (step === "hide") document.hidden = true;
    else if (step.startsWith("status:")) status_ = Number(step.slice(7));
    else if (step === "show") {
      document.hidden = false;
      for (const fn of listeners.visibilitychange || []) fn();
    } else if (step === "tick") {
      const due = pending;
      pending = [];
      for (const t of due) t.fn();
    } else if (step === "clear") document.getElementById("clear").fire("click");
    else if (step.startsWith("pick:") || step.startsWith("unpick:")) {
      const id = step.slice(step.indexOf(":") + 1);
      const b = box(id);
      if (!b) throw new Error("no checkbox " + id);
      b.checked = step.startsWith("pick:");
      b.fire("change");
    }
    await settle();
    steps.push({ step, fetches: urls.length, timers: pending.length });
  }

  const inputs = [];
  walk(document.getElementById("choices"), (n) => {
    if (n.tagName === "input") inputs.push({ value: n.value, checked: n.checked, disabled: n.disabled });
  });
  const labels = [];
  walk(document.getElementById("choices"), (n) => { if (n.tagName === "label") labels.push(n.textContent); });
  return {
    urls,
    title: document.getElementById("title").textContent,
    docTitle: document.title,
    status: document.getElementById("status").textContent,
    statusClass: document.getElementById("status").className,
    prose: document.getElementById("prose").children.map((p) => p.textContent),
    hint: document.getElementById("picker-hint").textContent,
    inputs,
    labels,
    chartText: document.getElementById("chart").textContent,
    summary: document.getElementById("chart-summary").textContent,
    chart: {
      created: chartLog.created,
      destroyed: chartLog.destroyed,
      setData: chartLog.setData,
      data: chartLog.data,
      labels: chartLog.opts ? chartLog.opts.series.map((s) => s.label || "") : null,
      gaps: chartLog.opts ? chartLog.opts.series.map((s) => Boolean(s.spanGaps)) : null,
      strokes: chartLog.opts ? chartLog.opts.series.map((s) => s.stroke || "") : null,
      yLabel: chartLog.opts ? chartLog.opts.axes[1].label : "",
    },
    latest: dumpLatest(document.getElementById("latest")),
    steps,
    timers: pending.length,
    innerHTML: counters.innerHTML,
    style: counters.style,
    timeoutCalls,
    controllers,
  };
}

(async () => {
  let raw = "";
  for await (const chunk of process.stdin) raw += chunk;
  const { source, scenarios } = JSON.parse(raw);
  const out = {};
  for (const [name, sc] of Object.entries(scenarios)) out[name] = await run(source, sc);
  process.stdout.write(JSON.stringify(out));
})();
