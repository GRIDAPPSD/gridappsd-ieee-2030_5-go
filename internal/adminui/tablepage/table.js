"use strict";

// One renderer for the Bridge table pages. The page talks only to its own
// origin and writes every server-supplied string with textContent, so a
// value can never become markup. Badge variants are matched against a fixed
// set before they become a class name, and no style is set from script, so
// the page runs under style-src 'self'.
(function () {
  const DATA = "data";
  const POLL_MS = 5000;
  const FETCH_TIMEOUT_MS = 10000;
  const BADGES = new Set(["ok", "warn", "error", "neutral"]);

  const $ = (id) => document.getElementById(id);

  function el(tag, text, className) {
    const e = document.createElement(tag);
    if (text !== undefined) e.textContent = text;
    if (className) e.className = className;
    return e;
  }

  function fill(node, c) {
    if (c.kind === "time") {
      const t = el("time", c.text);
      if (c.datetime) t.dateTime = c.datetime;
      node.append(t);
      return node;
    }
    node.textContent = c.text;
    if (c.kind === "badge") node.className = "badge" + (BADGES.has(c.badge) ? " badge-" + c.badge : "");
    return node;
  }

  function table(columns, rows) {
    const headRow = el("tr");
    for (const name of columns) headRow.append(el("th", name));
    const head = el("thead");
    head.append(headRow);
    const body = el("tbody");
    for (const row of rows) {
      const tr = el("tr");
      for (const c of row) tr.append(fill(el("td"), c));
      body.append(tr);
    }
    const t = el("table");
    t.append(head, body);
    return t;
  }

  function list(entries) {
    const dl = el("dl");
    for (const e of entries) {
      dl.append(el("dt", e.key), fill(el("dd"), e.value));
    }
    return dl;
  }

  function section(s) {
    const root = el("section", undefined, "section");
    root.append(el("h2", s.heading));
    for (const p of s.prose || []) root.append(el("p", p, "prose"));
    const body = s.body || {};
    if (s.kind === "table") {
      const rows = body.rows || [];
      if (rows.length > 0) root.append(table(body.columns || [], rows));
      else if (s.empty) root.append(el("p", s.empty, "empty"));
    } else if (s.kind === "definitionList") {
      const groups = body.groups || [];
      if (groups.length === 0 && s.empty) root.append(el("p", s.empty, "empty"));
      for (const g of groups) {
        if (g.heading) root.append(el("h3", g.heading));
        root.append(list(g.entries || []));
      }
    } else {
      root.append(el("p", "unsupported section kind: " + s.kind, "error"));
    }
    return root;
  }

  function render(d) {
    document.title = d.title;
    $("title").textContent = d.title;
    $("content").replaceChildren(...(d.sections || []).map(section));
  }

  function setStatus(text, isError) {
    const s = $("status");
    s.textContent = text;
    s.className = isError ? "error" : "muted";
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

  let timer = null;
  let inflight = false;

  async function load() {
    inflight = true;
    let limit = null;
    try {
      limit = timeoutSignal(FETCH_TIMEOUT_MS);
      const r = await fetch(DATA, { cache: "no-store", signal: limit.signal });
      if (!r.ok) throw new Error("HTTP " + r.status);
      render(await r.json());
      setStatus("updated " + new Date().toISOString(), false);
    } catch (e) {
      $("content").replaceChildren();
      setStatus("unavailable: " + e.message, true);
    } finally {
      if (limit) limit.done();
    }
    inflight = false;
  }

  // A hidden tab stops polling and starts again, at once, when it shows.
  async function tick() {
    timer = null;
    if (document.hidden) return;
    await load();
    timer = setTimeout(tick, POLL_MS);
  }

  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && timer === null && !inflight) tick();
  });

  tick();
})();
