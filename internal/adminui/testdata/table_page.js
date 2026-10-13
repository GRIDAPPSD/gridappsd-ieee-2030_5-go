"use strict";
// Runs the table page script on a minimal fake DOM and a fake fetch, then
// prints what the page shows as JSON. Driven by TestTablePage*. Stdin is
// {"source": <page script>, "scenarios": {name: {status, body, hidden, steps}}}
// so a test can run a mutated copy of the script without writing a file.
const vm = require("vm");

class Node_ {
  constructor(tag, counters) {
    this.tagName = tag;
    this.children = [];
    this.className = "";
    this.dateTime = "";
    this._text = null;
    this._c = counters;
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
}

function cellOf(n) {
  const t = n.children.find((c) => typeof c !== "string" && c.tagName === "time");
  return { text: n.textContent, cls: n.className, dateTime: t ? t.dateTime : "" };
}

function dumpSection(s) {
  const out = { heading: "", prose: [], empty: "", columns: null, rows: null, groups: [] };
  let pending = "";
  for (const c of s.children) {
    if (c.tagName === "h2") out.heading = c.textContent;
    else if (c.tagName === "h3") pending = c.textContent;
    else if (c.tagName === "p" && c.className === "prose") out.prose.push(c.textContent);
    else if (c.tagName === "p" && c.className === "empty") out.empty = c.textContent;
    else if (c.tagName === "table") {
      const [head, body] = c.children;
      out.columns = head.children[0].children.map((th) => th.textContent);
      out.rows = body.children.map((tr) => tr.children.map(cellOf));
    } else if (c.tagName === "dl") {
      const entries = [];
      for (let i = 0; i + 1 < c.children.length; i += 2) {
        entries.push({ key: c.children[i].textContent, value: cellOf(c.children[i + 1]) });
      }
      out.groups.push({ heading: pending, entries });
      pending = "";
    }
  }
  return out;
}

async function settle() {
  for (let i = 0; i < 4; i++) await new Promise((r) => setImmediate(r));
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
  let fetches = 0;
  let pending = [];
  let cleared = 0;
  async function fetch(url) {
    fetches++;
    const status = url === "data" ? sc.status || 200 : 404;
    return { ok: status < 400, status, json: async () => sc.body };
  }
  let timeoutCalls = 0;
  let controllers = 0;
  let aborts = 0;
  class AbortController_ {
    constructor() { controllers++; this.signal = { aborted: false }; }
    abort() { aborts++; this.signal.aborted = true; }
  }
  // noTimeout is a browser whose AbortSignal has no timeout().
  const abortSignal = sc.noTimeout ? {} : { timeout: () => { timeoutCalls++; return undefined; } };
  const ctx = {
    document, fetch, Date, String, Error, Array, Map, Set, Object,
    AbortSignal: abortSignal,
    AbortController: AbortController_,
    setTimeout: (fn) => { pending.push(fn); return pending.length; },
    clearTimeout: () => { cleared++; },
  };
  vm.createContext(ctx);
  vm.runInContext(source, ctx);
  await settle();

  const steps = [];
  for (const step of sc.steps || []) {
    if (step === "hide") document.hidden = true;
    if (step === "show") {
      document.hidden = false;
      for (const fn of listeners.visibilitychange || []) fn();
    }
    if (step === "tick") {
      const due = pending;
      pending = [];
      for (const fn of due) fn();
    }
    await settle();
    steps.push({ step, fetches, timers: pending.length });
  }

  const content = byId.get("content");
  return {
    title: byId.has("title") ? byId.get("title").textContent : "",
    docTitle: document.title,
    status: byId.has("status") ? byId.get("status").textContent : "",
    statusClass: byId.has("status") ? byId.get("status").className : "",
    sections: content ? content.children.map(dumpSection) : [],
    fetches,
    timers: pending.length,
    steps,
    innerHTML: counters.innerHTML,
    style: counters.style,
    timeoutCalls,
    controllers,
    cleared,
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
