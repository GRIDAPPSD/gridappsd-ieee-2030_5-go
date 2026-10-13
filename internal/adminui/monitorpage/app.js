"use strict";

// The page talks only to its own origin and writes every server-supplied
// string with textContent, so a message body can never become markup. No
// style is set from script, so the page runs under style-src 'self'. The
// feed is a server-sent event stream: the browser resumes it after a drop
// with Last-Event-ID, and an event marked final ends it for good.
(function () {
  const TOPICS = "topics";
  const STREAM = "stream";
  const POLL_MS = 5000;
  const FETCH_TIMEOUT_MS = 10000;
  // The log keeps the newest lines; older ones leave the page.
  const MAX_LINES = 500;
  // EventSource.CLOSED: the browser will not reconnect, as after a refusal.
  const CLOSED = 2;
  const NEAR_BOTTOM_PX = 40;

  const $ = (id) => document.getElementById(id);

  function el(tag, text, className) {
    const e = document.createElement(tag);
    if (text !== undefined) e.textContent = text;
    if (className) e.className = className;
    return e;
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

  function setStatus(id, text, isError) {
    const s = $(id);
    s.textContent = text;
    s.className = isError ? "error" : "muted";
  }

  // ---- the feed ----

  let source = null;

  function syncButtons() {
    $("stop").disabled = source === null;
  }

  function addLine(text, className) {
    const log = $("log");
    const stick = !(log.scrollHeight - log.scrollTop - log.clientHeight > NEAR_BOTTOM_PX);
    log.append(el("div", text, className));
    while (log.children.length > MAX_LINES) log.removeChild(log.children[0]);
    if (stick) log.scrollTop = log.scrollHeight;
  }

  function endFeed() {
    if (source) source.close();
    source = null;
    syncButtons();
  }

  function onEvent(data) {
    let ev;
    try {
      ev = JSON.parse(data);
    } catch (e) {
      addLine("unreadable event", "status");
      return;
    }
    if (ev.kind === "message") {
      addLine(ev.time + " " + ev.text);
    } else {
      addLine(ev.time + " [status] " + ev.text, "status");
      setStatus("status", ev.text, false);
    }
    if (ev.final) endFeed();
  }

  function start() {
    const topic = $("topic").value.trim();
    if (topic === "") {
      setStatus("status", "Enter a topic first.", true);
      return;
    }
    endFeed();
    $("log").replaceChildren();
    const es = new EventSource(STREAM + "?param=" + encodeURIComponent(topic));
    source = es;
    syncButtons();
    setStatus("status", "connecting to " + topic, false);
    es.onmessage = (m) => onEvent(m.data);
    es.onerror = () => {
      if (es.readyState === CLOSED) {
        if (source === es) endFeed();
        setStatus("status", "the monitor refused or ended this stream; check the topic and press Start to try again", true);
      } else {
        setStatus("status", "connection lost; retrying", true);
      }
    };
  }

  // ---- the watched topics ----

  function renderTopics(d) {
    $("prose").replaceChildren(...d.prose.map((p) => el("p", p, "prose")));
    const holder = $("topics");
    if (d.topics.length === 0) {
      holder.replaceChildren(el("p", "No topic is being watched.", "empty"));
      return;
    }
    const headRow = el("tr");
    for (const name of ["Topic", "State", "Viewers", "Buffered"]) headRow.append(el("th", name));
    const head = el("thead");
    head.append(headRow);
    const body = el("tbody");
    for (const t of d.topics) {
      const tr = el("tr");
      for (const v of [t.name, t.state, String(t.viewers), String(t.buffered)]) tr.append(el("td", v));
      body.append(tr);
    }
    const table = el("table");
    table.append(head, body);
    holder.replaceChildren(table);
  }

  let timer = null;
  let inflight = false;

  async function load() {
    inflight = true;
    let limit = null;
    try {
      limit = timeoutSignal(FETCH_TIMEOUT_MS);
      const r = await fetch(TOPICS, { cache: "no-store", signal: limit.signal });
      if (!r.ok) throw new Error("HTTP " + r.status);
      renderTopics(await r.json());
      setStatus("topics-status", "updated " + new Date().toISOString(), false);
    } catch (e) {
      $("topics").replaceChildren();
      setStatus("topics-status", "unavailable: " + e.message, true);
    } finally {
      if (limit) limit.done();
    }
    inflight = false;
  }

  // The topics list stops polling in a hidden tab and starts again, at once,
  // when it shows. The feed itself keeps running: it is live, not polled.
  async function tick() {
    timer = null;
    if (document.hidden) return;
    await load();
    timer = setTimeout(tick, POLL_MS);
  }

  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && timer === null && !inflight) tick();
  });
  $("watch").addEventListener("submit", (ev) => {
    ev.preventDefault();
    start();
  });
  $("stop").addEventListener("click", () => {
    endFeed();
    setStatus("status", "stopped", false);
  });

  tick();
})();
