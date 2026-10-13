"use strict";
// Runs the bus monitor page script on a minimal fake DOM, a fake fetch and a
// fake EventSource, then prints what the page shows as JSON. Driven by
// TestMonitorPage*. Stdin is {"source": <page script>, "scenarios": {...}}
// so a test can run a mutated copy. A scenario has: body (the topics
// answer), status, hidden, noTimeout, topic (typed before the steps), and
// steps: "start", "start:<topic>", "emit:<json>", "error:<readyState>",
// "stop", "hide", "show", "tick".
const vm = require("vm");

class Node_ {
  constructor(tag, counters) {
    this.tagName = tag;
    this.children = [];
    this.className = "";
    this.value = "";
    this.disabled = false;
    this.scrollTop = 0;
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
  removeChild(c) { this.children = this.children.filter((k) => k !== c); this._c.removed++; return c; }
  addEventListener(name, fn) { (this._listeners[name] = this._listeners[name] || []).push(fn); }
  fire(name, ev) { for (const fn of this._listeners[name] || []) fn(ev); }
}

function table(node) {
  const t = node.children.find((c) => typeof c !== "string" && c.tagName === "table");
  if (!t) return null;
  const [head, body] = t.children;
  return {
    columns: head.children[0].children.map((th) => th.textContent),
    rows: body.children.map((tr) => tr.children.map((td) => td.textContent)),
  };
}

async function settle() {
  for (let i = 0; i < 6; i++) await new Promise((r) => setImmediate(r));
}

async function run(source, sc) {
  const counters = { innerHTML: 0, style: 0, removed: 0 };
  const byId = new Map();
  const listeners = {};
  const document = {
    hidden: Boolean(sc.hidden),
    getElementById(id) {
      if (!byId.has(id)) byId.set(id, new Node_("div", counters));
      return byId.get(id);
    },
    createElement(tag) { return new Node_(tag, counters); },
    addEventListener(name, fn) { (listeners[name] = listeners[name] || []).push(fn); },
  };
  const urls = [];
  const sources = [];
  let pending = [];
  let nextTimer = 0;
  let timeoutCalls = 0;
  let controllers = 0;
  let prevented = 0;
  let thrown = 0;
  async function fetch(url) {
    urls.push(url);
    const status = url === "topics" ? sc.status || 200 : 404;
    return { ok: status < 400, status, json: async () => sc.body };
  }
  class EventSource_ {
    constructor(url) {
      this.url = url;
      this.readyState = 0;
      this.closed = false;
      this.onmessage = null;
      this.onerror = null;
      sources.push(this);
    }
    close() { this.closed = true; this.readyState = 2; }
  }
  class AbortController_ {
    constructor() { controllers++; this.signal = { aborted: false }; }
    abort() { this.signal.aborted = true; }
  }
  const ctx = {
    document, fetch, Date, String, Error, Array, Map, Set, Object, JSON, encodeURIComponent,
    EventSource: EventSource_,
    AbortSignal: sc.noTimeout ? {} : { timeout: () => { timeoutCalls++; return undefined; } },
    AbortController: AbortController_,
    setTimeout: (fn) => { const id = ++nextTimer; pending.push({ id, fn }); return id; },
    clearTimeout: (id) => { pending = pending.filter((t) => t.id !== id); },
  };
  vm.createContext(ctx);
  vm.runInContext(source, ctx);
  await settle();
  if (sc.topic !== undefined) document.getElementById("topic").value = sc.topic;

  const steps = [];
  for (const step of sc.steps || []) {
    const last = sources[sources.length - 1];
    if (step === "hide") document.hidden = true;
    else if (step === "show") {
      document.hidden = false;
      for (const fn of listeners.visibilitychange || []) fn();
    } else if (step === "tick") {
      const due = pending;
      pending = [];
      for (const t of due) t.fn();
    } else if (step === "stop") document.getElementById("stop").fire("click");
    else if (step === "start" || step.startsWith("start:")) {
      if (step !== "start") document.getElementById("topic").value = step.slice(6);
      document.getElementById("watch").fire("submit", { preventDefault() { prevented++; } });
    } else if (step.startsWith("emit:")) {
      // A throw from the page's handler is what a browser reports as an
      // uncaught error; it is counted, not left to end the run.
      try { last.onmessage({ data: step.slice(5) }); } catch (e) { thrown++; }
    }
    else if (step.startsWith("error:")) {
      last.readyState = Number(step.slice(6));
      last.onerror({});
    }
    await settle();
    steps.push({ step, fetches: urls.length, timers: pending.length, sources: sources.length });
  }

  const log = document.getElementById("log");
  return {
    urls,
    sources: sources.map((s) => ({ url: s.url, closed: s.closed })),
    log: log.children.map((d) => ({ text: d.textContent, cls: d.className })),
    status: document.getElementById("status").textContent,
    statusClass: document.getElementById("status").className,
    stopDisabled: document.getElementById("stop").disabled,
    topicsStatus: document.getElementById("topics-status").textContent,
    topicsStatusClass: document.getElementById("topics-status").className,
    topics: table(document.getElementById("topics")),
    topicsEmpty: document.getElementById("topics").children.map((c) => c.textContent),
    prose: document.getElementById("prose").children.map((p) => p.textContent),
    steps,
    timers: pending.length,
    prevented,
    thrown,
    removed: counters.removed,
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
