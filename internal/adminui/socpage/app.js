"use strict";

// The page talks only to its own origin and writes every server-supplied
// string with textContent: the CSP allows no inline script and the routes
// take no credential, so a name from the registry must never become markup.
(function () {
  const API = "/apps/soc/api/";
  const POLL_MS = 5000;
  const CONTROL_POLL_MS = 3000;
  const MIN_SECONDS = 60;
  const MAX_SECONDS = 3600;
  // A control still being served; the page polls while it is one of these.
  const LIVE_STATES = new Set(["scheduled", "active"]);
  const VERDICT_TEXT = {
    waiting: "no report yet from the device",
    moving: "the state of charge is moving the commanded way",
    reached: "the state of charge reached a limit or the watch percent",
    not_moving: "no movement seen",
    wrong_way: "the state of charge moved the other way",
    stopped: "a stop was sent",
  };
  const MAX_POINTS = 2000;
  const FETCH_TIMEOUT_MS = 10000;
  // Series asked of each route. Points per series shrink as the series count
  // grows (the body is bounded), so these are the defaults: 147 mirror series
  // of a 49-device fleet fit in 200 with 250 points each, and its 343 output
  // series fit in 500 with 100 points each.
  const MIRROR_SERIES_WANTED = 200;
  const OUTPUT_SERIES_WANTED = 500;
  const SUMMARY_LINES = 20;
  const SOC_ATTR = "DERStatus.stateOfChargeStatus";
  const OUTPUT_PREFIX = "DERStatus.";
  const PALETTE = ["#4cc9f0", "#f72585", "#b8de29", "#ffb703", "#9d7bff", "#2ec4b6", "#ff7f50", "#e0e0e0", "#80ed99", "#ff99c8"];
  const AXIS = "#cfd3d8";
  const GRID = "rgba(255,255,255,0.08)";

  const $ = (id) => document.getElementById(id);

  function el(tag, text, className) {
    const e = document.createElement(tag);
    if (text !== undefined) e.textContent = text;
    if (className) e.className = className;
    return e;
  }

  // Writes only when the text changed, so a live region is not re-announced
  // by a poll that found nothing new.
  function setText(e, text) {
    if (e.textContent !== text) e.textContent = text;
  }

  // Status lines are silent to a screen reader except while they show an error.
  function setStatus(e, text, isError) {
    e.setAttribute("aria-live", isError ? "polite" : "off");
    setText(e, text);
  }

  // Runs handle(resp) under a deadline that also covers reading the body, so a
  // request that never answers cannot freeze its poller.
  async function timedFetch(url, opts, handle) {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), FETCH_TIMEOUT_MS);
    try {
      const resp = await fetch(url, { ...opts, signal: ctrl.signal });
      return await handle(resp);
    } catch (err) {
      if (err && err.name === "AbortError") throw new Error("no answer within " + FETCH_TIMEOUT_MS / 1000 + " s");
      throw err;
    } finally {
      clearTimeout(timer);
    }
  }

  function uomLabel(uom) {
    switch (uom) {
      case 29: return "29 V";
      case 38: return "38 W";
      case 63: return "63 var";
      default: return "uom " + uom;
    }
  }

  function uomUnit(uom, unitFromServer) {
    return unitFromServer || uomLabel(uom);
  }

  function clock(iso) {
    if (!iso) return "-";
    const d = new Date(iso);
    return isNaN(d.getTime()) ? String(iso) : d.toLocaleTimeString();
  }

  // ---- selection state ----

  const knownDevices = new Map(); // key -> {key, name}
  const knownUoms = new Set([29, 38, 63]);
  const knownAttrs = new Set();
  const selDevices = new Set();
  const selUoms = new Set([29, 38, 63]);
  const selAttrs = new Set();
  let deviceFilter = "";
  let defaultsPicked = false;

  const colorOf = new Map();
  function colorFor(key) {
    if (!colorOf.has(key)) colorOf.set(key, PALETTE[colorOf.size % PALETTE.length]);
    return colorOf.get(key);
  }

  function renderPicker(listEl, items, selected, onChange) {
    const rows = items.map((it) => {
      const label = el("label");
      const box = document.createElement("input");
      box.type = "checkbox";
      box.checked = selected.has(it.key);
      box.addEventListener("change", () => {
        if (box.checked) selected.add(it.key); else selected.delete(it.key);
        onChange();
      });
      label.append(box, " " + it.label);
      return label;
    });
    listEl.replaceChildren(...rows);
  }

  function renderDevicePicker() {
    const f = deviceFilter.toLowerCase();
    const items = [...knownDevices.values()]
      .filter((d) => f === "" || d.name.toLowerCase().includes(f))
      .sort((a, b) => a.name.localeCompare(b.name))
      .map((d) => ({ key: d.key, label: d.name }));
    renderPicker($("device-list"), items, selDevices, redrawAll);
  }

  function renderUomPicker() {
    const items = [...knownUoms].sort((a, b) => a - b).map((u) => ({ key: u, label: uomLabel(u) }));
    renderPicker($("uom-list"), items, selUoms, redrawAll);
  }

  function renderAttrPicker() {
    const items = [...knownAttrs].sort().map((a) => ({ key: a, label: a.slice(OUTPUT_PREFIX.length) }));
    renderPicker($("attr-list"), items, selAttrs, redrawAll);
  }

  // ---- charts ----

  function newChart(container, summary) {
    return { container, summary, u: null, sig: "" };
  }

  // The canvas says nothing to a screen reader, so each chart carries a text
  // summary: the series count and the latest value of the first few lines.
  function summarize(chart, lines, emptyText) {
    if (lines.length === 0) {
      setText(chart.summary, emptyText);
      return;
    }
    const parts = lines.slice(0, SUMMARY_LINES).map((ln) => {
      const last = ln.pts.length > 0 ? ln.pts[ln.pts.length - 1].v : null;
      return ln.label + ": " + (last === null ? "no value" : last);
    });
    let text = lines.length + " series. Latest values: " + parts.join("; ");
    if (lines.length > SUMMARY_LINES) text += "; and " + (lines.length - SUMMARY_LINES) + " more.";
    setText(chart.summary, text);
  }

  // lines is [{key, label, scale, pts: [{t, v}]}]. All lines share one
  // timestamp axis, with null where a line has no point, and spanGaps joins them.
  function drawChart(chart, lines, emptyText) {
    summarize(chart, lines, emptyText);
    if (lines.length === 0) {
      if (chart.u) chart.u.destroy();
      chart.u = null;
      chart.sig = "";
      chart.container.textContent = emptyText;
      return;
    }
    const times = new Set();
    const byLine = lines.map((ln) => {
      const m = new Map();
      for (const p of ln.pts) { m.set(p.t, p.v); times.add(p.t); }
      return m;
    });
    const xs = [...times].sort((a, b) => a - b);
    const data = [xs, ...byLine.map((m) => xs.map((t) => (m.has(t) ? m.get(t) : null)))];
    const sig = lines.map((ln) => ln.key + "|" + ln.label + "|" + ln.scale).join("\n");
    if (chart.u && sig === chart.sig) {
      chart.u.setData(data);
      return;
    }
    if (chart.u) chart.u.destroy();
    chart.container.textContent = "";
    const scales = [...new Set(lines.map((ln) => ln.scale))];
    const scaleOpts = { x: { time: true } };
    for (const s of scales) scaleOpts[s] = {};
    const axes = [{ stroke: AXIS, grid: { stroke: GRID }, ticks: { stroke: GRID } }];
    scales.forEach((s, i) => {
      axes.push({ scale: s, side: i % 2 === 0 ? 3 : 1, label: s, stroke: AXIS, grid: { show: i === 0, stroke: GRID }, ticks: { stroke: GRID } });
    });
    const series = [{}, ...lines.map((ln) => ({ label: ln.label, scale: ln.scale, stroke: colorFor(ln.key), width: 1.5, spanGaps: true }))];
    chart.u = new uPlot({ width: chart.container.clientWidth || 600, height: 300, scales: scaleOpts, axes, series }, data, chart.container);
    chart.sig = sig;
  }

  function resizeChart(chart) {
    if (chart.u) chart.u.setSize({ width: chart.container.clientWidth || 600, height: 300 });
  }

  // ---- mirror series ----

  const mirrorStore = new Map(); // key -> {key, deviceKey, name, uom, unit, desc, pts, ids}
  const mirrorChart = newChart($("mirror-chart"), $("mirror-summary"));

  function mirrorKey(s, deviceKey) {
    return [deviceKey, s.uom, s.kind, s.phase, s.description].join("|");
  }

  function noteDevice(key, name) {
    if (knownDevices.has(key)) return false;
    knownDevices.set(key, { key, name });
    return true;
  }

  function ingestMirror(body) {
    let newDevice = false;
    let newUom = false;
    for (const s of body.series || []) {
      const deviceKey = s.registered ? s.mrid : "lfdi:" + (s.deviceLfdi || "mirror:" + s.mirror);
      const name = s.registered ? (s.name || s.mrid) : "unregistered " + (s.deviceLfdi || s.mirror);
      if (noteDevice(deviceKey, name)) newDevice = true;
      if (!knownUoms.has(s.uom)) { knownUoms.add(s.uom); selUoms.add(s.uom); newUom = true; }
      const key = mirrorKey(s, deviceKey);
      let st = mirrorStore.get(key);
      if (!st) {
        st = { key, deviceKey, name, uom: s.uom, unit: uomUnit(s.uom, s.unit), desc: s.description || "", pts: [], ids: new Set() };
        mirrorStore.set(key, st);
      }
      let unordered = false;
      for (const p of s.points || []) {
        if (st.ids.has(p.id)) continue;
        st.ids.add(p.id);
        if (st.pts.length > 0 && st.pts[st.pts.length - 1].t > p.t) unordered = true;
        st.pts.push({ t: p.t, v: p.v, id: p.id });
      }
      if (unordered) st.pts.sort((a, b) => a.t - b.t);
      while (st.pts.length > MAX_POINTS) st.ids.delete(st.pts.shift().id);
    }
    if (newDevice) renderDevicePicker();
    if (newUom) renderUomPicker();
    drawMirror();
  }

  function isFlat(pts) {
    return pts.length >= 2 && pts.every((p) => p.v === pts[0].v);
  }

  function drawMirror() {
    const chosen = [...mirrorStore.values()]
      .filter((st) => selDevices.has(st.deviceKey) && selUoms.has(st.uom))
      .sort((a, b) => a.key.localeCompare(b.key));
    const sameType = new Map();
    for (const st of chosen) sameType.set(st.deviceKey + "|" + st.uom, (sameType.get(st.deviceKey + "|" + st.uom) || 0) + 1);
    const flat = [];
    const lines = chosen.map((st) => {
      const f = isFlat(st.pts);
      let label = st.name + " " + st.unit;
      if (st.desc && sameType.get(st.deviceKey + "|" + st.uom) > 1) label += " (" + st.desc + ")";
      if (f) { label += " [flat]"; flat.push(st.name + " " + st.unit); }
      return { key: st.key, label, scale: st.unit, pts: st.pts };
    });
    drawChart(mirrorChart, lines, "Pick at least one device and one reading type.");
    const note = $("mirror-note");
    if (flat.length > 0) {
      note.textContent = "Flat, never changed across their points: " + flat.join(", ") +
        ". Values that never change are probably the test agents' fixed placeholder values. That is a guess; nothing on the server knows.";
      note.hidden = false;
    } else {
      note.hidden = true;
    }
  }

  // ---- output topic series ----

  const outputStore = new Map(); // key -> {key, mrid, attr, name, pts, seen}
  const outputChart = newChart($("output-chart"), $("output-summary"));

  function ingestOutput(body) {
    let newDevice = false;
    let newAttr = false;
    for (const s of body.series || []) {
      if (noteDevice(s.mrid, s.name || s.mrid)) newDevice = true;
      if (!knownAttrs.has(s.attribute)) {
        knownAttrs.add(s.attribute);
        if (s.attribute === SOC_ATTR) selAttrs.add(s.attribute);
        newAttr = true;
      }
      const key = s.mrid + "|" + s.attribute;
      let st = outputStore.get(key);
      if (!st) {
        st = { key, mrid: s.mrid, attr: s.attribute, name: s.name || s.mrid, pts: [], seen: new Set() };
        outputStore.set(key, st);
      }
      let unordered = false;
      for (const p of s.points || []) {
        const id = p.t + ":" + p.v;
        if (st.seen.has(id)) continue;
        st.seen.add(id);
        if (st.pts.length > 0 && st.pts[st.pts.length - 1].t > p.t) unordered = true;
        st.pts.push({ t: p.t, v: p.v, id });
      }
      if (unordered) st.pts.sort((a, b) => a.t - b.t);
      while (st.pts.length > MAX_POINTS) st.seen.delete(st.pts.shift().id);
    }
    if (newDevice) renderDevicePicker();
    if (newAttr) renderAttrPicker();
    drawOutput();
  }

  function drawOutput() {
    const chosen = [...outputStore.values()]
      .filter((st) => selDevices.has(st.mrid) && selAttrs.has(st.attr))
      .sort((a, b) => a.key.localeCompare(b.key));
    const lines = chosen.map((st) => ({
      key: "out|" + st.key,
      label: st.name + " " + st.attr.slice(OUTPUT_PREFIX.length),
      scale: st.attr === SOC_ATTR ? "percent" : "status code",
      pts: st.pts,
    }));
    drawChart(outputChart, lines, "Pick at least one device that the output topic has reported on.");
  }

  function redrawAll() {
    drawMirror();
    drawOutput();
  }

  // ---- polling ----

  // A poll is scheduled after the previous one finishes, so a slow answer
  // never overlaps the next request. A 503 means the route is busy and is
  // skipped quietly; any other failure is shown. The cursor is the server's
  // own clock from the last answer, and the repeats it brings are dropped by
  // the ingest functions.
  function startPoller(route, seriesWanted, statusEl, ingest, describe) {
    let since = 0;
    let lastOk = "";
    async function tick() {
      const url = API + route + "?since=" + since + "&series=" + seriesWanted;
      try {
        await timedFetch(url, { cache: "no-store" }, async (resp) => {
          if (resp.status === 503) {
            setStatus(statusEl, (lastOk ? lastOk + ". " : "") + route + ": server busy, retrying", true);
          } else if (!resp.ok) {
            setStatus(statusEl, route + ": HTTP " + resp.status, true);
          } else {
            const body = await resp.json();
            ingest(body);
            if (typeof body.now === "number") since = body.now;
            lastOk = describe(body);
            setStatus(statusEl, lastOk, body.seriesTruncated === true);
          }
        });
      } catch (err) {
        setStatus(statusEl, route + ": " + err.message, true);
      }
      setTimeout(tick, POLL_MS);
    }
    tick();
  }

  // Names what the server cut. It does not say which series it left out, so
  // the page reports how many are missing and which shown series hold only
  // their newest points.
  function describeSeries(what) {
    return (body) => {
      const shown = (body.series || []).length;
      let t = what + ": " + shown + " of " + (body.totalSeries || 0) + " series, updated " + new Date().toLocaleTimeString();
      if (body.seriesTruncated) {
        t += ". The server cut the list of series, so " + Math.max(0, (body.totalSeries || 0) - shown) + " are missing";
      }
      if (body.evictions > 0) {
        t += ". History is being dropped: the server has evicted " + body.evictions + " series";
      }
      const cut = (body.series || []).filter((s) => s.truncated);
      if (cut.length > 0) {
        const names = cut.slice(0, 3).map((s) => (s.name || s.mrid || s.deviceLfdi || "series") + (s.attribute ? " " + s.attribute.slice(OUTPUT_PREFIX.length) : ""));
        t += ". " + cut.length + " series show only their newest points (" + names.join(", ") + (cut.length > 3 ? ", ..." : "") + ")";
      }
      return t;
    };
  }

  // ---- devices and the SoC form ----

  function showDevicesError(text) {
    const e = $("devices-status");
    setText(e, text);
    e.hidden = text === "";
  }

  let deviceSig = null;

  async function loadDevices() {
    try {
      const list = await timedFetch(API + "devices", { cache: "no-store" }, async (resp) => {
        if (!resp.ok) throw new Error("HTTP " + resp.status);
        return resp.json();
      });
      showDevicesError("");
      const sig = list.map((d) => d.mrid + "|" + (d.name || "")).join("\n");
      if (sig === deviceSig) return;
      deviceSig = sig;
      const select = $("soc-device");
      const keep = select.value;
      const opts = list.map((d) => {
        const o = el("option", d.name || d.mrid);
        o.value = d.mrid;
        return o;
      });
      select.replaceChildren(...opts);
      if (keep && list.some((d) => d.mrid === keep)) select.value = keep;
      let added = false;
      for (const d of list) {
        if (noteDevice(d.mrid, d.name || d.mrid)) added = true;
      }
      if (!defaultsPicked && list.length > 0) {
        defaultsPicked = true;
        list.slice(0, 3).forEach((d) => selDevices.add(d.mrid));
      }
      if (added) renderDevicePicker();
      redrawAll();
    } catch (err) {
      showDevicesError("Device list not loaded: " + err.message + ". Showing the last list.");
    }
  }

  function showControlError(text) {
    const e = $("soc-error");
    e.textContent = text;
    e.hidden = text === "";
  }

  // Sends one control. The watch percent is kept by the page, not the server:
  // it rides on each status request and only marks when a report crossed it.
  async function postControl(body, watch) {
    const buttons = [$("soc-send"), $("soc-stop")];
    buttons.forEach((b) => { b.disabled = true; });
    showControlError("");
    try {
      await timedFetch(API + "control", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      }, async (resp) => {
        let payload = null;
        let text = "";
        try {
          text = await resp.text();
          payload = JSON.parse(text);
        } catch (err) {
          payload = null;
        }
        if (!resp.ok) {
          let msg = "HTTP " + resp.status + ": " + ((payload && payload.error) || text.trim() || resp.statusText);
          const retry = resp.headers.get("Retry-After");
          if (retry) msg += " (retry after " + retry + " s)";
          showControlError(msg);
          return;
        }
        if (!payload || !payload.id) {
          showControlError("unexpected answer from the server");
          return;
        }
        followControl(payload, watch);
      });
    } catch (err) {
      showControlError("send failed: " + err.message);
    } finally {
      buttons.forEach((b) => { b.disabled = false; });
    }
  }

  function parseWhole(raw, lo, hi) {
    if (raw.trim() === "") return null;
    const n = Number(raw);
    return Number.isInteger(n) && n >= lo && n <= hi ? n : null;
  }

  // An empty watch box means no watch; anything else must be a percent.
  function parseWatch(raw) {
    if (raw.trim() === "") return { ok: true, value: null };
    const n = Number(raw);
    if (Number.isFinite(n) && n >= 0 && n <= 100) return { ok: true, value: n };
    return { ok: false, value: null };
  }

  function onSend(ev) {
    ev.preventDefault();
    const mrid = $("soc-device").value;
    if (!mrid) { showControlError("pick a device"); return; }
    const dir = document.querySelector('input[name="soc-dir"]:checked');
    if (!dir) { showControlError("pick Discharge or Charge"); return; }
    const watts = parseWhole($("soc-watts").value, 1, Number.MAX_SAFE_INTEGER);
    if (watts === null) { showControlError("watts must be a whole number above 0; use Stop for 0 W"); return; }
    const seconds = parseWhole($("soc-seconds").value, MIN_SECONDS, MAX_SECONDS);
    if (seconds === null) { showControlError("duration must be a whole number of seconds from " + MIN_SECONDS + " to " + MAX_SECONDS); return; }
    const watch = parseWatch($("soc-watch").value);
    if (!watch.ok) { showControlError("watch percent must be a number from 0 to 100, or empty"); return; }
    // Discharge is positive and charge negative on the wire.
    postControl({ mrid, watts: dir.value === "charge" ? -watts : watts, durationSeconds: seconds }, watch.value);
  }

  function onStop() {
    const mrid = $("soc-device").value;
    if (!mrid) { showControlError("pick a device"); return; }
    postControl({ mrid, stop: true }, null);
  }

  // ---- control panel ----

  const controlChart = newChart($("control-chart"), $("control-summary"));
  let controlTimer = null;
  let controlToken = 0;

  function clockSec(s) {
    return typeof s === "number" ? new Date(s * 1000).toLocaleTimeString() : "-";
  }

  function describeCommand(st) {
    let what;
    if (st.stop || st.watts === 0) what = "0 W (stop)";
    else if (st.watts > 0) what = "Discharge " + st.watts + " W";
    else what = "Charge " + (-st.watts) + " W";
    return what + " for " + st.durationSeconds + " s, from " + clockSec(st.startedAt) + " to " + clockSec(st.endsAt);
  }

  function showControl(st, extraNote) {
    setText($("ctl-command"), describeCommand(st));
    setText($("ctl-state"), st.controlState || "-");
    const statuses = (st.responseStatuses || []).join(", ");
    setText($("ctl-received"), st.received ? "yes" + (statuses ? " (status " + statuses + ")" : "") : "not yet");
    const reports = st.reports || [];
    const last = reports.length > 0 ? reports[reports.length - 1] : null;
    setText($("ctl-soc"), last ? last.v + "% at " + clockSec(last.t) + " (" + (st.reportCount || reports.length) + " reports since the send)" : "no reports since the send");
    setText($("ctl-verdict"), st.verdict ? st.verdict + (VERDICT_TEXT[st.verdict] ? ": " + VERDICT_TEXT[st.verdict] : "") : "-");
    let watchText = "not set";
    if (typeof st.watchPercent === "number") {
      watchText = st.watchPercent + "%: " + (typeof st.watchReachedAt === "number" ? "reached at " + clockSec(st.watchReachedAt) : "not reached yet");
    }
    setText($("ctl-watch"), watchText);
    setText($("ctl-note"), extraNote || "-");
    const warn = $("ctl-warning");
    setText(warn, st.warning || "");
    warn.hidden = !st.warning;
    drawChart(controlChart, reports.length === 0 ? [] : [{ key: "control|" + st.id, label: "State of charge", scale: "percent", pts: reports }],
      "No state of charge reports since the send yet.");
  }

  // The newest send replaces the one being followed. Polling stops when the
  // control ends or is superseded, or a few minutes after its duration.
  function followControl(first, watch) {
    controlToken++;
    const token = controlToken;
    clearTimeout(controlTimer);
    showControl(first);
    const deadline = Date.now() + (first.durationSeconds + 300) * 1000;
    let shown = first;
    async function poll() {
      if (token !== controlToken) return;
      let problem = "";
      try {
        const url = API + "control/" + encodeURIComponent(first.id) + (watch !== null ? "?watch=" + encodeURIComponent(String(watch)) : "");
        const st = await timedFetch(url, { cache: "no-store" }, async (resp) => {
          if (resp.status === 404) return "gone";
          if (!resp.ok) throw new Error("HTTP " + resp.status);
          return resp.json();
        });
        if (token !== controlToken) return;
        if (st === "gone") {
          showControl({ ...shown, controlState: "gone" }, "The bridge no longer holds this control.");
          return;
        }
        shown = st;
        showControl(st);
        if (!LIVE_STATES.has(st.controlState) && st.controlState !== "unknown") return;
      } catch (err) {
        if (token !== controlToken) return;
        problem = err.message;
        showControl(shown, "Last check failed (" + problem + "), retrying.");
      }
      if (Date.now() > deadline) {
        showControl(shown, "Stopped polling after the duration plus 5 minutes" + (problem ? "; the last check failed (" + problem + ")" : "") + ".");
        return;
      }
      controlTimer = setTimeout(poll, CONTROL_POLL_MS);
    }
    poll();
  }

  // ---- start ----

  $("device-filter").addEventListener("input", (ev) => {
    deviceFilter = ev.target.value;
    renderDevicePicker();
  });
  $("devices-none").addEventListener("click", () => {
    selDevices.clear();
    renderDevicePicker();
    redrawAll();
  });
  $("soc-form").addEventListener("submit", onSend);
  $("soc-stop").addEventListener("click", onStop);
  window.addEventListener("resize", () => {
    resizeChart(mirrorChart);
    resizeChart(outputChart);
  });

  renderUomPicker();
  renderAttrPicker();
  mirrorChart.container.textContent = "Pick at least one device and one reading type.";
  outputChart.container.textContent = "Pick at least one device that the output topic has reported on.";
  loadDevices();
  setInterval(loadDevices, 30000);
  startPoller("mirror", MIRROR_SERIES_WANTED, $("mirror-status"), ingestMirror, describeSeries("mirror"));
  startPoller("output", OUTPUT_SERIES_WANTED, $("output-status"), ingestOutput, describeSeries("output"));
})();
