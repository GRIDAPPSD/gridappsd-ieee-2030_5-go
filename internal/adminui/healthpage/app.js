"use strict";

// The page talks only to its own origin and writes every server-supplied
// string with textContent: the value of the server-of-record link comes from
// configuration, so it is checked for a web scheme before it becomes a link.
(function () {
  const DATA = "data";
  const POLL_MS = 5000;
  const FETCH_TIMEOUT_MS = 10000;

  const $ = (id) => document.getElementById(id);

  function el(tag, text, className) {
    const e = document.createElement(tag);
    if (text !== undefined) e.textContent = text;
    if (className) e.className = className;
    return e;
  }

  function webURL(value) {
    try {
      const u = new URL(value);
      return u.protocol === "http:" || u.protocol === "https:" ? u.href : null;
    } catch (e) {
      return null;
    }
  }

  function linkOrText(href, label) {
    const url = webURL(href);
    if (!url) return el("span", href);
    const a = el("a", label);
    a.href = url;
    a.rel = "noopener noreferrer";
    return a;
  }

  function entries(d) {
    const stomp = d.stompConnected ? el("span", "connected", "ok") : el("span", "disconnected", "warn");
    const rows = [
      ["Status", el("span", d.status, "ok")],
      ["STOMP connection", stomp],
      ["mTLS listener", el("span", d.mtlsListener)],
      ["Server SFDI", el("span", d.serverSfdi)],
      ["Server LFDI", el("span", d.serverLfdi)],
      ["Feeder mRID", el("span", d.feederMrid)],
      ["Simulation ID", el("span", d.simulationId)],
      ["Registry entries", el("span", String(d.registryCount))],
      ["Placeholder identities", el("span", String(d.placeholderCount))],
      ["Certificate derived identities", el("span", String(d.certificateCount))],
      ["Uptime (seconds)", el("span", String(d.uptimeSeconds))],
      ["Server of record", d.sorLink ? linkOrText(d.sorLink, "Server of record dashboard") : el("span", "not set")],
    ];
    const p = d.publishing;
    if (p) {
      rows.push(
        ["Publishing", el("span", p.on ? "ON" : "OFF", p.on ? "warn" : "muted")],
        ["Last changed", el("span", p.changedAt || "-")],
        ["Changed from", el("span", p.changedBy || "-")],
      );
    }
    return rows;
  }

  function render(d) {
    const kids = [];
    for (const [key, value] of entries(d)) {
      kids.push(el("dt", key));
      const dd = el("dd");
      dd.append(value);
      kids.push(dd);
    }
    $("health").replaceChildren(...kids);
  }

  function setStatus(text, isError) {
    const s = $("health-status");
    s.textContent = text;
    s.className = isError ? "error" : "muted";
  }

  async function poll() {
    try {
      const r = await fetch(DATA, { cache: "no-store", signal: AbortSignal.timeout(FETCH_TIMEOUT_MS) });
      if (!r.ok) throw new Error("HTTP " + r.status);
      render(await r.json());
      setStatus("updated " + new Date().toISOString(), false);
    } catch (e) {
      $("health").replaceChildren();
      setStatus("health unavailable: " + e.message, true);
    }
    setTimeout(poll, POLL_MS);
  }

  poll();
})();
