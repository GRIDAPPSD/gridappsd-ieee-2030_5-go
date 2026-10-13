"use strict";

// The Bus sender page: the four control forms and the raw JSON box, then the
// switch, the recent sends and the refusals. The page talks only to its own
// origin and writes every server-supplied string with textContent (see
// sections.js). The server checks every submission again, so the checks
// here only save a round trip.
(function () {
  const DATA = "data";
  const POLL_MS = 5000;
  const FETCH_TIMEOUT_MS = 10000;
  const POST_TIMEOUT_MS = 30000;
  const { el, section, timeoutSignal, post } = BridgeSections;

  const $ = (id) => document.getElementById(id);

  // Each form posts to the route named by its id. A field's element id is
  // the form id, a dash and the field's name, which is also its JSON key.
  const FORMS = [
    { id: "active-power", fields: [["device", "choice"], ["multiplier", "integer"], ["value", "integer"]] },
    { id: "reactive-power", fields: [["device", "choice"], ["multiplier", "integer"], ["value", "integer"]] },
    { id: "connect", fields: [["device", "choice"], ["connect", "boolean"]] },
    { id: "energize", fields: [["device", "choice"], ["energize", "boolean"]] },
    { id: "raw", fields: [["json", "text"]] },
  ];

  function setResult(formId, text, kind) {
    const r = $(formId + "-result");
    r.textContent = text;
    r.className = "result " + kind;
  }

  // collect reads a form into the object to post, or returns an error text.
  function collect(form) {
    const body = {};
    for (const [name, kind] of form.fields) {
      const node = $(form.id + "-" + name);
      if (kind === "choice") {
        if (node.value === "") return { error: "Pick a device." };
        body[name] = node.value;
      } else if (kind === "integer") {
        const text = node.value.trim();
        const n = Number(text);
        if (!/^-?[0-9]+$/.test(text) || !Number.isSafeInteger(n)) return { error: name + " must be a whole number." };
        body[name] = n;
      } else if (kind === "boolean") {
        body[name] = Boolean(node.checked);
      } else {
        body[name] = node.value;
      }
    }
    return { body };
  }

  const submitting = new Set();

  async function submit(form) {
    if (submitting.has(form.id)) return;
    const got = collect(form);
    if (got.error) {
      setResult(form.id, got.error, "error");
      return;
    }
    submitting.add(form.id);
    $(form.id + "-submit").disabled = true;
    setResult(form.id, "sending", "muted");
    const res = await post(form.id, got.body, POST_TIMEOUT_MS);
    setResult(form.id, res.text, res.ok ? "ok" : "error");
    $(form.id + "-submit").disabled = false;
    submitting.delete(form.id);
    reload();
  }

  // ---- the data ----

  let devicesSig = "";

  // renderDevices rebuilds the device lists only when the devices change, so
  // a poll never resets the choice an operator has made.
  function renderDevices(devices) {
    const sig = JSON.stringify(devices);
    if (sig === devicesSig) return;
    devicesSig = sig;
    for (const form of FORMS) {
      const select = $(form.id + "-device");
      if (!select) continue;
      const kept = select.value;
      const options = devices.map((d) => {
        const o = el("option", d.label);
        o.value = d.id;
        return o;
      });
      if (options.length === 0) {
        const none = el("option", "No devices registered");
        none.value = "";
        options.push(none);
      }
      select.replaceChildren(...options);
      select.disabled = devices.length === 0;
      if (devices.some((d) => d.id === kept)) select.value = kept;
    }
  }

  function renderLimits(l) {
    for (const id of ["active-power", "reactive-power"]) {
      $(id + "-multiplier").min = String(l.multiplierMin);
      $(id + "-multiplier").max = String(l.multiplierMax);
      $(id + "-value").min = String(l.valueMin);
      $(id + "-value").max = String(l.valueMax);
    }
  }

  function render(d) {
    document.title = d.title;
    $("title").textContent = d.title;
    renderDevices(d.devices);
    renderLimits(d.limits);
    $("content").replaceChildren(...d.sections.map(section));
  }

  function setStatus(text, isError) {
    const s = $("status");
    s.textContent = text;
    s.className = isError ? "error" : "muted";
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
  for (const form of FORMS) {
    $(form.id).addEventListener("submit", (ev) => {
      ev.preventDefault();
      submit(form);
    });
  }

  tick();
})();
