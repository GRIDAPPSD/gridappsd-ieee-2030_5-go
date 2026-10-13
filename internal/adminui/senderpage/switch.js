"use strict";

// The Bus publishing page: the switch's state, and one button that sends
// the opposite of the state last read. The page talks only to its own
// origin and writes every server-supplied string with textContent (see
// sections.js).
(function () {
  const DATA = "data";
  const PUBLISHING = "publishing";
  const POLL_MS = 5000;
  const FETCH_TIMEOUT_MS = 10000;
  // An off flip waits for sends in flight, which can take a dozen seconds.
  const POST_TIMEOUT_MS = 30000;
  const { section, timeoutSignal, post } = BridgeSections;

  const $ = (id) => document.getElementById(id);

  // null until the first read: the button does not guess a direction.
  let on = null;
  let busy = false;
  let timer = null;
  let inflight = false;

  function syncToggle() {
    const b = $("toggle");
    b.disabled = busy || on === null;
    b.textContent = on ? "Turn publishing off" : "Turn publishing on";
  }

  function render(d) {
    if (typeof d.on !== "boolean") throw new Error("data has no switch state");
    document.title = d.title;
    $("title").textContent = d.title;
    $("content").replaceChildren(...d.sections.map(section));
    on = d.on;
    syncToggle();
  }

  function setStatus(text, isError) {
    const s = $("status");
    s.textContent = text;
    s.className = isError ? "error" : "muted";
  }

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
      on = null;
      syncToggle();
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

  function reload() {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
    if (!inflight) tick();
  }

  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && timer === null && !inflight) tick();
  });

  $("switch").addEventListener("submit", async (ev) => {
    ev.preventDefault();
    if (busy || on === null) return;
    busy = true;
    syncToggle();
    const a = $("answer");
    a.textContent = "sending";
    a.className = "result muted";
    const res = await post(PUBLISHING, { on: !on }, POST_TIMEOUT_MS);
    a.textContent = res.text;
    a.className = res.ok ? "result ok" : "result error";
    busy = false;
    syncToggle();
    reload();
  });

  tick();
})();
