"use strict";
// Runs socpage/app.js against a minimal fake DOM and a fake fetch, then prints
// what the cards show as JSON. Driven by TestCardsPageRendersAndStops.
const fs = require("fs");
const vm = require("vm");

class Node_ {
  constructor(tag) {
    this.tagName = tag;
    this.children = [];
    this.attrs = {};
    this.listeners = {};
    this.className = "";
    this.hidden = false;
    this.disabled = false;
    this._text = null;
  }
  get textContent() {
    if (this._text !== null) return this._text;
    return this.children.map((c) => (typeof c === "string" ? c : c.textContent)).join("");
  }
  set textContent(v) { this._text = String(v); this.children = []; }
  append(...kids) { this._text = null; this.children.push(...kids); }
  replaceChildren(...kids) { this._text = null; this.children = kids; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); }
  click() { for (const fn of this.listeners.click || []) fn({}); }
  get selectedOptions() { return []; }
}

const byId = new Map();
const document = {
  getElementById(id) {
    if (!byId.has(id)) byId.set(id, new Node_("div"));
    return byId.get(id);
  },
  createElement(tag) { return new Node_(tag); },
  querySelector() { return null; },
};

const posts = [];
let cardsBody = null;
const timers = [];
function reply(obj, status = 200) {
  return { ok: status < 400, status, headers: { get: () => null }, json: async () => obj, text: async () => JSON.stringify(obj), statusText: "" };
}
async function fakeFetch(url, opts) {
  if (opts && opts.method === "POST") { posts.push({ url, body: JSON.parse(opts.body) }); return reply({ id: "x" }); }
  if (url.endsWith("/cards")) return reply(cardsBody);
  if (url.endsWith("/devices")) return reply([]);
  return reply({ series: [], now: 0 });
}

const ctx = {
  document, console, JSON, Date, Math, Map, Set, Number, String, Array, Error, Promise, AbortController,
  window: { addEventListener() {} },
  fetch: fakeFetch,
  setTimeout(fn, ms) { timers.push({ fn, ms }); return timers.length; },
  clearTimeout() {},
  setInterval() { return 0; },
  uPlot: function () {},
};
vm.createContext(ctx);

const settle = () => new Promise((r) => setImmediate(r));
const walk = (n, f) => { f(n); for (const c of n.children) if (typeof c !== "string") walk(c, f); };
function cardView(card) {
  const out = { className: card.className, texts: [], hiddenTexts: [] };
  walk(card, (n) => {
    if (n.tagName === "button") out.stop = { hidden: n.hidden, disabled: n.disabled };
    if (n.tagName === "dd" || n.tagName === "strong" || n.tagName === "span" || n.tagName === "div" || n.tagName === "p") {
      if (n.children.length === 0 || n._text !== null) (n.hidden ? out.hiddenTexts : out.texts).push(n.textContent);
    }
    if (n.tagName === "dl") out["dl_hidden_" + (out.dls = (out.dls || 0) + 1)] = n.hidden;
  });
  return out;
}

async function main() {
  const [appPath, fixturePath] = process.argv.slice(2);
  cardsBody = JSON.parse(fs.readFileSync(fixturePath, "utf8"));
  vm.runInContext(fs.readFileSync(appPath, "utf8"), ctx);
  await settle(); await settle();
  const list = document.getElementById("cards");
  const first = list.children.slice();
  const views = first.map(cardView);
  const status = document.getElementById("cards-status").textContent;

  // Second poll: the same cards come back and the same elements are reused.
  const pollTimer = timers.filter((t) => t.ms === 5000).pop();
  pollTimer.fn();
  await settle(); await settle();
  const reused = list.children.length === first.length && list.children.every((c, i) => c === first[i]);

  // Press Stop on the first card that has a Stop button showing.
  let stopped = null;
  for (const c of list.children) {
    let btn = null;
    walk(c, (n) => { if (n.tagName === "button") btn = n; });
    if (btn && !btn.hidden) { btn.click(); stopped = c; break; }
  }
  await settle(); await settle();
  console.log(JSON.stringify({ views, status, reused, posts, stopIndex: stopped ? first.indexOf(stopped) : -1 }));
}
main().catch((e) => { console.error(e); process.exit(1); });
