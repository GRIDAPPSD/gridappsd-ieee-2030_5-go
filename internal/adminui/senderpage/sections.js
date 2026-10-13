"use strict";

// The renderer the Bus publishing and Bus sender pages share. Every
// server-supplied string is written with textContent, so a value can never
// become markup. Badge variants are matched against a fixed set before they
// become a class name, and no style is set from script, so the pages run
// under style-src 'self'.
var BridgeSections = (function () {
  const BADGES = new Set(["ok", "warn", "error", "neutral"]);

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

  // answerText is what an operator reads for a POST's answer: the plane's
  // message on success, and the refusal or failure text otherwise. A body
  // that is not the expected JSON falls back to the status.
  async function answerText(r) {
    let j = null;
    try {
      j = await r.json();
    } catch (e) {
      j = null;
    }
    if (r.ok) return { ok: true, text: j && typeof j.message === "string" ? j.message : "Done." };
    let text = j && typeof j.error === "string" ? j.error : "HTTP " + r.status;
    if (j && typeof j.field === "string" && j.field !== "") text += " (" + j.field + ")";
    return { ok: false, text };
  }

  // post sends one JSON object and resolves to {ok, text}. It never rejects:
  // a network failure or a timeout is a refusal the operator can read, and
  // it says the send may still have happened, since the server can have
  // acted before the answer was lost.
  async function post(path, obj, timeoutMs) {
    let limit = null;
    try {
      limit = timeoutSignal(timeoutMs);
      const r = await fetch(path, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(obj),
        cache: "no-store",
        signal: limit.signal,
      });
      return await answerText(r);
    } catch (e) {
      return { ok: false, text: "no answer: " + e.message + ". Check Recent sends before trying again." };
    } finally {
      if (limit) limit.done();
    }
  }

  return { el, section, timeoutSignal, post };
})();
