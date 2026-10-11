"use strict";
// Runs socpage/app.js against a minimal fake DOM and a fake fetch, then prints
// what the cards show as JSON. Driven by TestCardsPage*.
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
    this.parent = null;
  }
  get textContent() {
    if (this._text !== null) return this._text;
    return this.children.map((c) => (typeof c === "string" ? c : c.textContent)).join("");
  }
  set textContent(v) { this._text = String(v); this.children = []; }
  append(...kids) { this._text = null; this.children.push(...kids); this._adopt(kids); }
  replaceChildren(...kids) { this._text = null; this.children = kids; this._adopt(kids); }
  _adopt(kids) { for (const k of kids) if (typeof k !== "string") k.parent = this; }
  // Shown only when no node on the way up is hidden, as a browser lays it out.
  get shown() { return !this.hidden && (!this.parent || this.parent.shown); }
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
let cardsReply = { status: 200, body: null };
let postReply = { kind: "ok" };
const timers = [];
let replaced = 0;
function reply(obj, status = 200, headers = {}) {
  return { ok: status < 400, status, headers: { get: (k) => (k in headers ? headers[k] : null) }, json: async () => obj, text: async () => JSON.stringify(obj), statusText: "" };
}
async function fakeFetch(url, opts) {
  if (opts && opts.method === "POST") {
    posts.push({ url, body: JSON.parse(opts.body) });
    if (postReply.kind === "throw") throw new Error("offline");
    if (postReply.kind === "http") return reply({ error: "boom" }, 500, { "Retry-After": "7" });
    return reply({ id: "x" });
  }
  if (url.endsWith("/cards")) return reply(cardsReply.body, cardsReply.status);
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

// dt/dd pairs of a list, as label -> value.
function pairs(dl) {
  const out = {};
  for (let i = 0; i + 1 < dl.children.length; i += 2) out[dl.children[i].textContent] = dl.children[i + 1].textContent;
  return out;
}

// What a reader sees: texts of nodes with no hidden ancestor go to texts, the
// rest to hiddenTexts; lists are reported with their effective visibility.
function cardView(card) {
  const out = { className: card.className, ariaLabel: card.attrs["aria-label"], texts: [], hiddenTexts: [], lists: [] };
  walk(card, (n) => {
    if (n.tagName === "button") out.stop = { hidden: !n.shown, disabled: n.disabled, ariaLabel: n.attrs["aria-label"] };
    if (["dd", "strong", "span", "div", "p"].includes(n.tagName)) {
      if (n.children.length === 0 || n._text !== null) (n.shown ? out.texts : out.hiddenTexts).push(n.textContent);
    }
    if (n.tagName === "dl") out.lists.push({ hidden: !n.shown, fields: pairs(n) });
  });
  return out;
}

const statusLine = () => {
  const e = document.getElementById("cards-status");
  return { text: e.textContent, error: e.attrs["aria-live"] === "polite" };
};
const errorBox = () => {
  const e = document.getElementById("cards-error");
  return { text: e.textContent, hidden: !e.shown };
};

// Fires the re-armed pollCards timer itself, not any other 5 s timer.
async function pollNow(body, status = 200) {
  cardsReply = { status, body };
  const t = timers.filter((x) => x.fn.name === "pollCards").pop();
  if (!t) throw new Error("no pollCards timer armed");
  t.fn();
  await settle(); await settle();
}

function snapshot(name, list, firstEls) {
  return {
    name,
    replaced,
    order: list.children.map((c) => c.attrs["aria-label"]),
    views: list.children.map(cardView),
    firstIndex: list.children.map((c) => firstEls.indexOf(c)),
    empty: !document.getElementById("cards-empty").shown,
    status: statusLine(),
  };
}

function stopButton(list, label) {
  let btn = null;
  for (const c of list.children) walk(c, (n) => { if (n.tagName === "button" && n.attrs["aria-label"] === label) btn = n; });
  if (!btn) throw new Error("no button " + label);
  return btn;
}

async function pressStop(list, label, kind) {
  postReply = { kind };
  const before = posts.length;
  stopButton(list, label).click();
  await settle(); await settle();
  return { label, kind, posts: posts.slice(before), error: errorBox(), disabled: stopButton(list, label).disabled };
}

async function main() {
  const [appPath, fixturePath] = process.argv.slice(2);
  const fixture = JSON.parse(fs.readFileSync(fixturePath, "utf8"));
  const clone = () => JSON.parse(JSON.stringify(fixture));
  cardsReply = { status: 200, body: clone() };
  const cardsList = document.getElementById("cards");
  const realReplace = cardsList.replaceChildren.bind(cardsList);
  cardsList.replaceChildren = (...kids) => { replaced++; realReplace(...kids); };
  vm.runInContext(fs.readFileSync(appPath, "utf8"), ctx);
  await settle(); await settle();
  const list = document.getElementById("cards");
  const firstEls = list.children.slice();
  const polls = [snapshot("initial", list, firstEls)];

  // Same order, new values: the cards must be updated in place.
  let b = clone();
  b.cards[0].status.stateOfChargePercent = 50;
  b.cards[0].status.readingAgeSeconds = 65;
  await pollNow(b);
  polls.push(snapshot("values", list, firstEls));

  // A device disappears and another moves to the front.
  b = clone();
  b.total = 5;
  b.cards = [b.cards[5], b.cards[0], b.cards[2], b.cards[3], b.cards[4]];
  await pollNow(b);
  polls.push(snapshot("reorder", list, firstEls));

  // The summary line with every warning on.
  const after = clone();
  after.total = 9; after.truncated = true; after.skippedDevices = 2; after.statusUnavailable = true;
  after.cards = b.cards;
  await pollNow(after);
  polls.push(snapshot("warnings", list, firstEls));

  await pollNow({}, 503);
  polls.push(snapshot("busy", list, firstEls));
  await pollNow({}, 500);
  polls.push(snapshot("http500", list, firstEls));
  await pollNow(b);
  polls.push(snapshot("recovered", list, firstEls));

  const stops = [];
  stops.push(await pressStop(list, "Stop pv_a", "http"));
  stops.push(await pressStop(list, "Stop pv_a", "throw"));
  stops.push(await pressStop(list, "Stop bat_d", "ok"));
  console.log(JSON.stringify({ polls, stops }));
}
main().catch((e) => { console.error(e); process.exit(1); });
