"use strict";

// The page talks only to its own origin and writes every server-supplied
// string with textContent: the CSP allows no inline script and the routes
// take no credential, so a name from the registry must never become markup.
(function () {
  const API = "/apps/soc/api/";
  const POLL_MS = 5000;
  const TRIP_POLL_MS = 2000;
  const MAX_POINTS = 2000;
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

  function newChart(container) {
    return { container, u: null, sig: "" };
  }

  // lines is [{key, label, scale, pts: [{t, v}]}]. All lines share one
  // timestamp axis, with null where a line has no point, and spanGaps joins them.
  function drawChart(chart, lines, emptyText) {
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
  const mirrorChart = newChart($("mirror-chart"));
  let mirrorTruncated = false;

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
    mirrorTruncated = Boolean(body.seriesTruncated);
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
  const outputChart = newChart($("output-chart"));

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
  function startPoller(route, statusEl, ingest, describe) {
    let since = 0;
    async function tick() {
      try {
        const resp = await fetch(API + route + "?since=" + since, { cache: "no-store" });
        if (resp.status === 503) {
          // busy: try again next tick
        } else if (!resp.ok) {
          statusEl.textContent = route + ": HTTP " + resp.status;
        } else {
          const body = await resp.json();
          ingest(body);
          if (typeof body.now === "number") since = body.now;
          statusEl.textContent = describe(body);
        }
      } catch (err) {
        statusEl.textContent = route + ": " + err.message;
      }
      setTimeout(tick, POLL_MS);
    }
    tick();
  }

  function describeSeries(what) {
    return (body) => {
      let t = what + ": " + (body.totalSeries || 0) + " series, updated " + new Date().toLocaleTimeString();
      if (body.seriesTruncated) t += ". The server cut the list of series, so some are missing.";
      return t;
    };
  }

  // ---- devices and the SoC form ----

  async function loadDevices() {
    try {
      const resp = await fetch(API + "devices", { cache: "no-store" });
      if (!resp.ok) return;
      const list = await resp.json();
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
      showSoCError("devices: " + err.message);
    }
  }

  function showSoCError(text) {
    const e = $("soc-error");
    e.textContent = text;
    e.hidden = text === "";
  }

  function updateBand() {
    const raw = $("soc-percent").value;
    const n = Number(raw);
    $("soc-band").hidden = !(raw !== "" && Number.isFinite(n) && n >= 60 && n <= 70);
  }

  async function postSoC(body) {
    const buttons = [$("soc-send"), $("soc-clear")];
    buttons.forEach((b) => { b.disabled = true; });
    showSoCError("");
    try {
      const resp = await fetch(API + "soc", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
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
        showSoCError(msg);
        return;
      }
      if (!payload || !payload.id) {
        showSoCError("unexpected answer from the server");
        return;
      }
      followTrip(payload);
    } catch (err) {
      showSoCError("send failed: " + err.message);
    } finally {
      buttons.forEach((b) => { b.disabled = false; });
    }
  }

  function parseWhole(raw, lo, hi) {
    if (raw.trim() === "") return null;
    const n = Number(raw);
    return Number.isInteger(n) && n >= lo && n <= hi ? n : null;
  }

  function onSend(ev) {
    ev.preventDefault();
    const mrid = $("soc-device").value;
    if (!mrid) { showSoCError("pick a device"); return; }
    const percent = parseWhole($("soc-percent").value, 0, 100);
    if (percent === null) { showSoCError("percent must be a whole number from 0 to 100"); return; }
    const hold = parseWhole($("soc-hold").value, 1, 3600);
    if (hold === null) { showSoCError("hold must be a whole number of seconds from 1 to 3600"); return; }
    postSoC({ mrid, percent, holdSeconds: hold });
  }

  function onClear() {
    const mrid = $("soc-device").value;
    if (!mrid) { showSoCError("pick a device"); return; }
    postSoC({ mrid, clear: true });
  }

  // ---- round trip panel ----

  let tripTimer = null;
  let tripToken = 0;

  function showTrip(st, extraNote) {
    $("trip-sent").textContent = clock(st.sentAt) + " (" + st.kind + (st.kind === "send" ? ", " + st.percent + "% for " + st.holdSeconds + " s" : "") + ")";
    $("trip-posted").textContent = st.postedAt ? clock(st.postedAt) + (st.deviceReportedPercent !== undefined ? ", device reported " + st.deviceReportedPercent + "%" : "") : "waiting";
    $("trip-seen").textContent = st.seenAt ? clock(st.seenAt) + (st.outputReportedPercent !== undefined ? ", topic carried " + st.outputReportedPercent + "%" : "") : "waiting";
    $("trip-released").textContent = st.releasedAt ? clock(st.releasedAt) : "waiting";
    $("trip-verdict").textContent = st.verdict || "-";
    $("trip-note").textContent = [st.note, extraNote].filter(Boolean).join(" ") || "-";
  }

  function tripDone(st) {
    return st.verdict !== "pending" && (st.kind === "clear" || Boolean(st.releasedAt) || st.verdict !== "matched");
  }

  // The newest send replaces the one being followed. Polling stops at a final
  // state, or a few minutes after the hold ends.
  function followTrip(first) {
    tripToken++;
    const token = tripToken;
    clearTimeout(tripTimer);
    showTrip(first);
    if (tripDone(first)) return;
    const deadline = Date.now() + (first.holdSeconds + 300) * 1000;
    async function poll() {
      if (token !== tripToken) return;
      let st = null;
      try {
        const resp = await fetch(API + "soc/" + encodeURIComponent(first.id), { cache: "no-store" });
        if (token !== tripToken) return;
        if (resp.status === 404) {
          showTrip(first, "The bridge no longer holds this send.");
          return;
        }
        if (resp.ok) st = await resp.json();
      } catch (err) {
        st = null;
      }
      if (token !== tripToken) return;
      if (st) {
        showTrip(st);
        if (tripDone(st)) return;
      }
      if (Date.now() > deadline) {
        $("trip-note").textContent += " Stopped polling.";
        return;
      }
      tripTimer = setTimeout(poll, TRIP_POLL_MS);
    }
    tripTimer = setTimeout(poll, TRIP_POLL_MS);
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
  $("soc-clear").addEventListener("click", onClear);
  $("soc-percent").addEventListener("input", updateBand);
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
  startPoller("mirror", $("mirror-status"), ingestMirror, describeSeries("mirror"));
  startPoller("output", $("output-status"), ingestOutput, describeSeries("output"));
})();
