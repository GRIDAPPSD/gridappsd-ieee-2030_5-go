"use strict";
// Runs the Bus publishing or Bus sender page script on a minimal fake DOM and
// a fake fetch, then prints what the page shows and what it posted, as JSON.
// Driven by TestSwitchPage* and TestSenderPage*. Stdin is
// {"lib": <sections.js>, "source": <page script>, "scenarios": {...}} so a
// test can run a mutated copy of either script. A scenario has: body and
// status (the data route's answer; body2 replaces body at a "swap" step), post (the POST answer: status, body,
// reject), holdPost (POSTs wait for a release step), hidden, noTimeout, and
// steps: "tick", "hide", "show", "set:<id>=<value>", "check:<id>=<bool>",
// "submit:<form id>", "release", "swap".
const vm = require("vm");

class Node_ {
  constructor(tag, counters, isSelect) {
    this.tagName = tag;
    this.children = [];
    this.className = "";
    this.value = "";
    this.disabled = false;
    this.checked = false;
    this.min = "";
    this.max = "";
    this.dateTime = "";
    this._text = null;
    this._c = counters;
    this._select = Boolean(isSelect);
    this.rebuilds = 0;
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
  replaceChildren(...kids) {
    this._text = null;
    this.children = kids;
    if (this._select) {
      this.rebuilds++;
      this.value = kids.length > 0 ? kids[0].value : "";
    }
  }
  addEventListener(name, fn) { (this._listeners[name] = this._listeners[name] || []).push(fn); }
  fire(name, ev) { for (const fn of this._listeners[name] || []) fn(ev); }
}

function cellOf(n) {
  const t = n.children.find((c) => typeof c !== "string" && c.tagName === "time");
  return { text: n.textContent, cls: n.className, dateTime: t ? t.dateTime : "" };
}

function dumpSection(s) {
  const out = { heading: "", prose: [], empty: "", columns: null, rows: null, groups: [] };
  for (const c of s.children) {
    if (c.tagName === "h2") out.heading = c.textContent;
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
      out.groups.push({ entries });
    }
  }
  return out;
}

async function settle() {
  for (let i = 0; i < 8; i++) await new Promise((r) => setImmediate(r));
}

async function run(lib, source, sc) {
  const counters = { innerHTML: 0, style: 0 };
  const byId = new Map();
  const listeners = {};
  const document = {
    hidden: Boolean(sc.hidden),
    title: "",
    getElementById(id) {
      if (!byId.has(id)) byId.set(id, new Node_("div", counters, id.endsWith("-device")));
      return byId.get(id);
    },
    createElement(tag) { return new Node_(tag, counters, false); },
    addEventListener(name, fn) { (listeners[name] = listeners[name] || []).push(fn); },
  };
  const urls = [];
  const posts = [];
  const held = [];
  let pending = [];
  let nextTimer = 0;
  let timeoutCalls = 0;
  let controllers = 0;
  let prevented = 0;
  async function fetch(url, opts) {
    if (opts && opts.method === "POST") {
      posts.push({ url, contentType: (opts.headers || {})["Content-Type"] || "", body: opts.body });
      if (sc.holdPost) await new Promise((resolve) => held.push(resolve));
      const p = sc.post || {};
      if (p.reject) throw new Error(p.reject);
      const status = p.status || 200;
      const body = p.body === undefined ? { ok: true, message: "Done." } : p.body;
      return { ok: status < 400, status, json: async () => { if (p.notJSON) throw new Error("not json"); return body; } };
    }
    urls.push(url);
    const status = url === "data" ? sc.status || 200 : 404;
    return { ok: status < 400, status, json: async () => sc.body };
  }
  class AbortController_ {
    constructor() { controllers++; this.signal = { aborted: false }; }
    abort() { this.signal.aborted = true; }
  }
  const ctx = {
    document, fetch, Date, String, Error, Array, Map, Set, Object, JSON, Number, Boolean, RegExp, Math, Promise,
    AbortSignal: sc.noTimeout ? {} : { timeout: () => { timeoutCalls++; return undefined; } },
    AbortController: AbortController_,
    setTimeout: (fn) => { const id = ++nextTimer; pending.push({ id, fn }); return id; },
    clearTimeout: (id) => { pending = pending.filter((t) => t.id !== id); },
  };
  vm.createContext(ctx);
  vm.runInContext(lib + "\n" + source, ctx);
  await settle();

  const steps = [];
  for (const step of sc.steps || []) {
    if (step === "hide") document.hidden = true;
    else if (step === "show") {
      document.hidden = false;
      for (const fn of listeners.visibilitychange || []) fn();
    } else if (step === "tick") {
      const due = pending;
      pending = [];
      for (const t of due) t.fn();
    } else if (step === "swap") {
      sc.body = sc.body2;
    } else if (step === "release") {
      for (const resolve of held.splice(0)) resolve();
    } else if (step.startsWith("set:")) {
      const rest = step.slice(4);
      const at = rest.indexOf("=");
      document.getElementById(rest.slice(0, at)).value = rest.slice(at + 1);
    } else if (step.startsWith("check:")) {
      const rest = step.slice(6);
      const at = rest.indexOf("=");
      document.getElementById(rest.slice(0, at)).checked = rest.slice(at + 1) === "true";
    } else if (step.startsWith("submit:")) {
      document.getElementById(step.slice(7)).fire("submit", { preventDefault() { prevented++; } });
    } else {
      throw new Error("unknown step " + step);
    }
    await settle();
    steps.push({ step, fetches: urls.length, posts: posts.length, timers: pending.length });
  }

  const dumpNode = (id) => (byId.has(id) ? { text: byId.get(id).textContent, cls: byId.get(id).className, disabled: byId.get(id).disabled } : null);
  const content = byId.get("content");
  const results = {};
  const selects = {};
  const limits = {};
  for (const [id, n] of byId) {
    if (id.endsWith("-result")) results[id.slice(0, -7)] = dumpNode(id);
    if (id.endsWith("-submit")) results[id.slice(0, -7) + ":submit"] = dumpNode(id);
    if (id.endsWith("-device")) {
      selects[id] = { value: n.value, disabled: n.disabled, rebuilds: n.rebuilds, options: n.children.map((o) => ({ value: o.value, text: o.textContent })) };
    }
    if (id.endsWith("-multiplier") || id.endsWith("-value")) limits[id] = { min: n.min, max: n.max };
  }
  return {
    urls,
    posts,
    title: byId.has("title") ? byId.get("title").textContent : "",
    docTitle: document.title,
    status: byId.has("status") ? byId.get("status").textContent : "",
    statusClass: byId.has("status") ? byId.get("status").className : "",
    sections: content ? content.children.map(dumpSection) : [],
    toggle: dumpNode("toggle"),
    answer: dumpNode("answer"),
    results,
    selects,
    limits,
    steps,
    timers: pending.length,
    prevented,
    innerHTML: counters.innerHTML,
    style: counters.style,
    timeoutCalls,
    controllers,
  };
}

(async () => {
  let raw = "";
  for await (const chunk of process.stdin) raw += chunk;
  const { lib, source, scenarios } = JSON.parse(raw);
  const out = {};
  for (const [name, sc] of Object.entries(scenarios)) out[name] = await run(lib, source, sc);
  process.stdout.write(JSON.stringify(out));
})();
