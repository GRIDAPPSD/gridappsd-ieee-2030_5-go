"use strict";

// The page talks only to its own origin and writes every server-supplied
// string with textContent: the CSP allows no inline script and the routes
// take no credential, so a name from the registry must never become markup.
(function () {
  const API = "/apps/soc/api/";
  const POLL_MS = 5000;
  const CONTROL_POLL_MS = 3000;
  const MIN_SECONDS = 60;
  // The select value that sends to every device; no device mRID is this.
  const ALL_DEVICES = "*all*";
  const MAX_SECONDS = 3600;
  // A control still being served; the page polls while it is one of these.
  const LIVE_STATES = new Set(["scheduled", "active"]);
  const MAX_POINTS = 2000;
  const FETCH_TIMEOUT_MS = 10000;
  // Points per series shrink as the series count grows (the body is
  // bounded): the 343 output series of a 49-device fleet fit in 500 with 100
  // points each.
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

  function clock(iso) {
    if (!iso) return "-";
    const d = new Date(iso);
    return isNaN(d.getTime()) ? String(iso) : d.toLocaleTimeString();
  }

  // ---- selection state ----

  const knownDevices = new Map(); // key -> {key, name}
  const knownAttrs = new Set();
  const selDevices = new Set();
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

  function noteDevice(key, name) {
    if (knownDevices.has(key)) return false;
    knownDevices.set(key, { key, name });
    return true;
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

  // The count is the series the page holds, because a poll since the cursor
  // carries only the series with new points. The server's cut is reported
  // only when it says it cut, as how many series are missing.
  function describeSeries(what, heldCount) {
    return (body) => {
      const held = heldCount();
      let t = what + ": " + held + " series, updated " + new Date().toLocaleTimeString();
      if (body.seriesTruncated) {
        t += ". The server cut the list of series, so " + Math.max(0, (body.totalSeries || 0) - (body.series || []).length) + " are missing";
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
  // Kept across polls until the operator picks a device, so a fallback is not
  // wiped by the next poll that finds the list unchanged.
  let deviceNote = "";

  async function loadDevices() {
    try {
      const list = await timedFetch(API + "devices", { cache: "no-store" }, async (resp) => {
        if (!resp.ok) throw new Error("HTTP " + resp.status);
        return resp.json();
      });
      showDevicesError(deviceNote);
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
      // The all-devices choice sits after the devices, so the form still
      // opens on the first device.
      if (list.length > 0) {
        const all = el("option", "All devices");
        all.value = ALL_DEVICES;
        opts.push(all);
      }
      select.replaceChildren(...opts);
      if (keep === ALL_DEVICES && list.length > 0) {
        select.value = keep;
      } else if (keep && list.some((d) => d.mrid === keep)) {
        select.value = keep;
      } else if (keep && list.length > 0) {
        deviceNote = "The device you had selected is no longer in the list; the form now shows " + (list[0].name || list[0].mrid) + ".";
      }
      showDevicesError(deviceNote);
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
  async function postControl(body, watch, deviceName) {
    sending = true;
    syncButtons();
    showControlError("");
    let accepted = false;
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
        accepted = true;
        followControl(payload, watch, deviceName);
      });
    } catch (err) {
      if (accepted) {
        showControlError("The bridge accepted the control, but the page could not show it: " + err.message);
      } else {
        // The panel and Stop stay on the last control known to be issued: a
        // missing answer must never remove the operator's way to stop it.
        if (body.stop) {
          showControlError("No answer to the Stop (" + err.message + "). Whether it was applied is unknown; Stop is still enabled, press it again to retry.");
        } else {
          const kept = followed ? " The panel and Stop still follow the earlier control on " + followed.name + "." : "";
          showControlError("No answer to the send (" + err.message + "). Whether the bridge issued this control is unknown: check the device's control state in the output chart before sending again." + kept);
        }
      }
    } finally {
      sending = false;
      syncButtons();
    }
  }

  function showAllResult(text, warn) {
    const e = $("soc-all-result");
    e.textContent = text;
    e.className = warn ? "warn" : "muted";
    e.hidden = text === "";
  }

  // Sends one control to every device in a single request. The panel below
  // follows one device, so it is left alone; the answer lists each device.
  async function postAll(body) {
    sending = true;
    syncButtons();
    showControlError("");
    showAllResult("", false);
    try {
      await timedFetch(API + "control/all", {
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
        if (!payload || !Array.isArray(payload.results)) {
          showControlError("unexpected answer from the server");
          return;
        }
        const what = payload.stop ? "Stop (0 W)" : payload.watts + " W for " + payload.durationSeconds + " s";
        let msg = what + " sent to " + payload.succeeded + " of " + payload.total + " devices.";
        const failed = payload.results.filter((r) => !r.ok);
        if (failed.length > 0) {
          msg += " Failed: " + failed.map((r) => (r.name || r.mrid) + " (" + r.error + ")").join(", ") + ".";
        }
        const warned = payload.results.filter((r) => r.ok && r.warning);
        if (warned.length > 0) {
          msg += " Warning: " + warned.map((r) => (r.name || r.mrid) + " (" + r.warning + ")").join(", ") + ".";
        }
        showAllResult(msg, failed.length > 0 || warned.length > 0);
      });
    } catch (err) {
      showControlError("No answer to the all-devices send (" + err.message + "). Whether the bridge issued these controls is unknown: check the devices in the output chart before sending again.");
    } finally {
      sending = false;
      syncButtons();
    }
  }

  function parseWhole(raw, lo, hi) {
    if (!/^[0-9]+$/.test(raw.trim())) return null;
    const n = Number(raw);
    return Number.isInteger(n) && n >= lo && n <= hi ? n : null;
  }

  // Positive discharges and negative charges, as on the wire. Zero is refused:
  // Stop is the way to end a control.
  function parseSigned(raw) {
    if (!/^-?[0-9]+$/.test(raw.trim())) return null;
    const n = Number(raw);
    return Number.isSafeInteger(n) && n !== 0 ? n : null;
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
    const watts = parseSigned($("soc-watts").value);
    if (watts === null) { showControlError("watts must be a whole number, positive to discharge or negative to charge; use Stop for 0 W"); return; }
    const seconds = parseWhole($("soc-seconds").value, MIN_SECONDS, MAX_SECONDS);
    if (seconds === null) { showControlError("duration must be a whole number of seconds from " + MIN_SECONDS + " to " + MAX_SECONDS); return; }
    const watch = parseWatch($("soc-watch").value);
    if (!watch.ok) { showControlError("watch percent must be a number from 0 to 100, or empty"); return; }
    if (mrid === ALL_DEVICES) {
      postAll({ watts, durationSeconds: seconds });
      return;
    }
    const opt = $("soc-device").selectedOptions[0];
    postControl({ mrid, watts, durationSeconds: seconds }, watch.value, opt ? opt.textContent : mrid);
  }

  // Stop goes to the device of the control being followed, never to whatever
  // the dropdown shows now.
  function onStop() {
    if (!followed) { showControlError("no control is being followed"); return; }
    postControl({ mrid: followed.mrid, stop: true }, null, followed.name);
  }

  function onStopAll() {
    postAll({ stop: true });
  }

  function syncButtons() {
    $("soc-send").disabled = sending;
    $("soc-stop-all").disabled = sending;
    $("soc-stop").disabled = sending || followed === null;
  }

  // ---- control panel ----

  const controlChart = newChart($("control-chart"), $("control-summary"));
  let controlTimer = null;
  let controlToken = 0;
  let followed = null;
  let sending = false;
  // Devices whose earlier control the page no longer follows; that control may
  // still be running.
  const unfollowed = new Map();

  function showUnfollowed() {
    const e = $("ctl-earlier");
    setText(e, unfollowed.size === 0 ? "" : "No longer followed here (the control may still be running): " + [...unfollowed.values()].join(", ") + ".");
    e.hidden = unfollowed.size === 0;
  }

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
    setText($("ctl-device"), followed ? followed.name + " (" + followed.mrid + ")" : "-");
    setText($("ctl-command"), describeCommand(st));
    setText($("ctl-state"), st.controlState || "-");
    const statuses = (st.responseStatuses || []).join(", ");
    setText($("ctl-received"), st.received ? "yes" + (statuses ? " (status " + statuses + ")" : "") : "not yet");
    const reports = st.reports || [];
    const last = reports.length > 0 ? reports[reports.length - 1] : null;
    setText($("ctl-soc"), last ? last.v + "% at " + clockSec(last.t) + " (" + (st.reportCount || reports.length) + " reports since the send)" : "no reports since the send");
    setText($("ctl-verdict"), st.verdict || "-");
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
  function followControl(first, watch, deviceName) {
    if (followed && followed.mrid !== first.mrid) unfollowed.set(followed.mrid, followed.name);
    followed = { mrid: first.mrid, name: deviceName || first.mrid };
    unfollowed.delete(followed.mrid);
    showUnfollowed();
    syncButtons();
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
          if (!resp.ok) {
            let detail = "";
            try {
              const body = JSON.parse(await resp.text());
              if (body && typeof body.error === "string") detail = body.error;
            } catch (e) {
              detail = "";
            }
            throw new Error("HTTP " + resp.status + (detail ? ": " + detail : ""));
          }
          return resp.json();
        });
        if (token !== controlToken) return;
        if (st === "gone") {
          showControl({ ...shown, controlState: "gone" }, "The bridge no longer holds this control.");
          return;
        }
        shown = st;
        const ended = !LIVE_STATES.has(st.controlState) && st.controlState !== "unknown";
        showControl(st, ended ? "Stopped checking: the control is " + st.controlState + "." : undefined);
        if (ended) return;
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

  // ---- device cards ----

  const CARDS_POLL_MS = 5000;
  const STRIPES = new Set(["grey", "green", "blue", "yellow", "pink"]);
  const STATUS_FIELDS = [
    ["State of charge", (st) => (typeof st.stateOfChargePercent === "number" ? st.stateOfChargePercent + "%" : "-")],
    ["Inverter", (st) => st.inverterStatus || "-"],
    ["Operational mode", (st) => st.operationalModeStatus || "-"],
    ["Storage mode", (st) => st.storageModeStatus || "-"],
    ["Generator connection", (st) => st.genConnectStatus || "-"],
    ["Storage connection", (st) => st.storConnectStatus || "-"],
    ["Alarm", (st) => st.alarmStatus || "-"],
    ["Reading time", (st) => utcText(st.readingTime)],
  ];
  const cardRows = new Map(); // mrid -> card parts
  let cardOrder = "";

  // The device's own report time, in UTC so it reads the same on every screen.
  function utcText(sec) {
    if (typeof sec !== "number" || !Number.isFinite(sec) || sec <= 0) return "-";
    const d = new Date(sec * 1000);
    if (isNaN(d.getTime())) return "-";
    return d.toISOString().slice(0, 19).replace("T", " ") + " UTC";
  }

  function describeWatts(c) {
    if (c.direction === "discharge") return "Discharge " + c.watts + " W";
    if (c.direction === "charge") return "Charge " + (-c.watts) + " W";
    return "0 W (hold)";
  }

  function showCardsError(text) {
    const e = $("cards-error");
    setText(e, text);
    e.hidden = text === "";
  }

  // A value the page does not recognise falls back to grey rather than into a
  // class name taken from the server.
  function stripeClass(stripe) {
    return "card c-" + (STRIPES.has(stripe) ? stripe : "grey");
  }

  function pair(dl, label) {
    const dt = el("dt", label);
    const dd = el("dd", "-");
    dl.append(dt, dd);
    return dd;
  }

  function makeCard(mrid) {
    const root = el("div", undefined, "card c-grey");
    root.setAttribute("role", "group");
    const head = el("div", undefined, "card-head");
    const name = el("strong");
    const kind = el("span", undefined, "muted");
    const cim = el("span", undefined, "muted");
    const tag = el("span", "unlabeled", "muted");
    head.append(name, " ", cim, " ", kind, " ", tag);
    const label = el("div", undefined, "stripe-label");
    const noReport = el("p", "no report yet", "muted");
    const statusList = el("dl", undefined, "ctl");
    const statusCells = STATUS_FIELDS.map(([l]) => pair(statusList, l));
    const control = el("dl", undefined, "ctl");
    const watts = pair(control, "Control");
    const source = pair(control, "Source");
    const received = pair(control, "Received by the device");
    const stop = el("button", "Stop");
    stop.type = "button";
    stop.addEventListener("click", () => stopRow(mrid, stop));
    root.append(head, label, noReport, statusList, control, stop);
    return { root, name, cim, tag, kind, label, noReport, statusList, statusCells, control, watts, source, received, stop };
  }

  function renderCard(row, c) {
    row.root.className = stripeClass(c.stripe);
    // A labelled card is titled by its EPRI label with the CIM name beside it;
    // an unmapped one shows the CIM name and an unlabeled tag. A battery's CIM
    // name starts with pv_, so its card shows the label alone.
    const labelled = typeof c.label === "string" && c.label !== "";
    const labelOnly = labelled && c.label.startsWith("battery");
    setText(row.name, labelled ? c.label : c.name || c.mrid);
    setText(row.cim, labelled && !labelOnly ? c.name || c.mrid : "");
    row.cim.hidden = !labelled || labelOnly;
    row.tag.hidden = labelled;
    setText(row.kind, labelOnly ? "" : c.kind || "");
    row.kind.hidden = labelOnly;
    setText(row.label, c.stripeLabel || "");
    const st = c.status;
    row.noReport.hidden = st !== null && st !== undefined;
    row.statusList.hidden = !row.noReport.hidden;
    if (st) STATUS_FIELDS.forEach(([, f], i) => setText(row.statusCells[i], f(st)));
    const ctl = c.control;
    row.control.hidden = !ctl;
    row.stop.hidden = !ctl;
    row.root.setAttribute("aria-label", (labelled ? c.label : c.name || c.mrid) + ", " + (c.stripeLabel || ""));
    if (!ctl) return;
    setText(row.watts, describeWatts(ctl));
    setText(row.source, ctl.source || "-");
    setText(row.received, ctl.received ? "yes" : "not yet");
    row.stop.setAttribute("aria-label", "Stop " + (labelled ? c.label : c.name || c.mrid));
  }

  // Cards are reused across polls so a focused Stop button survives a refresh.
  function renderCards(cards) {
    $("cards-empty").hidden = cards.length > 0;
    const kept = new Set();
    const roots = [];
    for (const c of cards) {
      kept.add(c.mrid);
      let row = cardRows.get(c.mrid);
      if (!row) {
        row = makeCard(c.mrid);
        cardRows.set(c.mrid, row);
      }
      renderCard(row, c);
      roots.push(row.root);
    }
    for (const k of [...cardRows.keys()]) {
      if (!kept.has(k)) cardRows.delete(k);
    }
    const order = cards.map((c) => c.mrid).join("\n");
    if (order !== cardOrder) {
      $("cards").replaceChildren(...roots);
      cardOrder = order;
    }
  }

  function cardsSummary(body) {
    let t = "devices: " + (body.cards || []).length + ", updated " + new Date().toLocaleTimeString();
    if (body.truncated) t += ". Showing " + (body.cards || []).length + " of " + body.total;
    if (body.skippedDevices > 0) t += ". " + body.skippedDevices + " devices could not be read";
    if (body.statusUnavailable) t += ". DERStatus could not be read";
    return t;
  }

  async function pollCards() {
    const status = $("cards-status");
    try {
      await timedFetch(API + "cards", { cache: "no-store" }, async (resp) => {
        if (resp.status === 503) {
          setStatus(status, "cards: server busy, retrying", true);
        } else if (!resp.ok) {
          setStatus(status, "cards: HTTP " + resp.status, true);
        } else {
          const body = await resp.json();
          renderCards(body.cards || []);
          setStatus(status, cardsSummary(body), body.truncated === true || body.skippedDevices > 0 || body.statusUnavailable === true);
        }
      });
    } catch (err) {
      setStatus(status, "cards: " + err.message, true);
    }
    setTimeout(pollCards, CARDS_POLL_MS);
  }

  // Stop targets the card's own device and leaves the form's followed control
  // alone.
  async function stopRow(mrid, button) {
    button.disabled = true;
    showCardsError("");
    try {
      await timedFetch(API + "control", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ mrid, stop: true }),
      }, async (resp) => {
        if (resp.ok) return;
        let payload = null;
        let text = "";
        try {
          text = await resp.text();
          payload = JSON.parse(text);
        } catch (err) {
          payload = null;
        }
        let msg = "HTTP " + resp.status + ": " + ((payload && payload.error) || text.trim() || resp.statusText);
        const retry = resp.headers.get("Retry-After");
        if (retry) msg += " (retry after " + retry + " s)";
        showCardsError("Stop failed: " + msg);
      });
    } catch (err) {
      showCardsError("No answer to the Stop (" + err.message + "). Whether it was applied is unknown; press Stop again to retry.");
    } finally {
      button.disabled = false;
    }
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
  $("soc-stop-all").addEventListener("click", onStopAll);
  $("soc-device").addEventListener("change", () => {
    deviceNote = "";
    showDevicesError("");
  });
  syncButtons();
  window.addEventListener("resize", () => {
    resizeChart(outputChart);
    resizeChart(controlChart);
  });

  renderAttrPicker();
  outputChart.container.textContent = "Pick at least one device that the output topic has reported on.";
  loadDevices();
  setInterval(loadDevices, 30000);
  pollCards();
  startPoller("output", OUTPUT_SERIES_WANTED, $("output-status"), ingestOutput, describeSeries("output", () => outputStore.size));
})();
