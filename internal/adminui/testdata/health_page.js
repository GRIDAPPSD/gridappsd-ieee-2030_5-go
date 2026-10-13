"use strict";
// Runs healthpage/app.js against a minimal fake DOM and a fake fetch, then
// prints what the page shows as JSON. Driven by TestHealthPage*.
const fs = require("fs");
const vm = require("vm");

class Node_ {
  constructor(tag) {
    this.tagName = tag;
    this.children = [];
    this.className = "";
    this._text = null;
  }
  get textContent() {
    if (this._text !== null) return this._text;
    return this.children.map((c) => (typeof c === "string" ? c : c.textContent)).join("");
  }
  set textContent(v) { this._text = String(v); this.children = []; }
  append(...kids) { this._text = null; this.children.push(...kids); }
  replaceChildren(...kids) { this._text = null; this.children = kids; }
}

function findLink(node) {
  for (const c of node.children) {
    if (typeof c === "string") continue;
    if (c.tagName === "a") return c;
    const inner = findLink(c);
    if (inner) return inner;
  }
  return null;
}

const base = {
  status: "ok", stompConnected: true, mtlsListener: "127.0.0.1:8443",
  serverSfdi: "999888777", serverLfdi: "FEDCBA9876543210",
  feederMrid: "feeder-1", simulationId: "sim-1",
  registryCount: 3, placeholderCount: 1, certificateCount: 2,
  uptimeSeconds: 42, sorLink: "https://sor.example/dash",
};

const scenarios = {
  full: { body: { ...base, publishing: { on: true, changedAt: "2026-07-18T12:30:00.000Z", changedBy: "127.0.0.1:40000" } } },
  unsafe: { body: { ...base, sorLink: "javascript:alert(1)" } },
  bare: { body: { ...base, stompConnected: false, sorLink: "" } },
  failed: { status: 500, body: { error: "boom" } },
};

async function run(scenario) {
  const byId = new Map();
  const document = {
    getElementById(id) {
      if (!byId.has(id)) byId.set(id, new Node_("div"));
      return byId.get(id);
    },
    createElement(tag) { return new Node_(tag); },
  };
  let fetches = 0;
  let timers = 0;
  async function fetch(url) {
    fetches++;
    const status = url === "data" ? scenario.status || 200 : 404;
    return { ok: status < 400, status, json: async () => scenario.body };
  }
  const ctx = {
    document, fetch, URL, Date, String, Error,
    AbortSignal: { timeout: () => undefined },
    setTimeout: () => { timers++; return 0; },
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), ctx);
  // The page's first poll has awaited fetch and json by the time these run.
  await new Promise((r) => setImmediate(r));
  await new Promise((r) => setImmediate(r));

  const kids = byId.get("health").children;
  const rows = {};
  let link = null;
  for (let i = 0; i + 1 < kids.length; i += 2) {
    rows[kids[i].textContent] = kids[i + 1].textContent;
    if (kids[i].textContent === "Server of record") link = findLink(kids[i + 1]);
  }
  return {
    rows,
    link: link && link.href ? link.href : "",
    linkSet: Boolean(link && link.href),
    status: byId.get("health-status").textContent,
    polls: Math.min(fetches, timers),
  };
}

(async () => {
  const out = {};
  for (const [name, sc] of Object.entries(scenarios)) out[name] = await run(sc);
  process.stdout.write(JSON.stringify(out));
})();
