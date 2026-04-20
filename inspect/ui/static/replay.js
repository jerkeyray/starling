// Replay UI: drives the side-by-side timeline, the divergence
// dialog, and the pause/step/resume/restart controls. Pure vanilla
// JS so the inspector keeps zero npm/CDN dependencies. EventSource
// (built-in to every modern browser) handles the SSE channel; HTMX
// is not used here because the interaction is push-driven from the
// server, not click-driven from the user.
//
// Lifecycle:
//   1. POST /run/{id}/replay        → get session_id
//   2. EventSource(/.../stream)     → receive step + end events
//   3. Buttons POST /.../control    → pause/step/resume/restart
//
// Restart spins up a brand-new session id (and clears the lists);
// the server-side restart command also exists but the simpler
// "tear-down + new session" model fits the page-refresh-anyway UX.

(function () {
  "use strict";

  const cfg = window.STARLING_REPLAY;
  if (!cfg || !cfg.runID) {
    console.error("STARLING_REPLAY config missing");
    return;
  }

  const recordedEl = document.getElementById("recorded-list");
  const producedEl = document.getElementById("produced-list");
  const progressEl = document.getElementById("replay-progress");
  const statusEl   = document.getElementById("replay-status");
  const pauseBtn   = document.getElementById("ctl-pause");
  const stepBtn    = document.getElementById("ctl-step");
  const resumeBtn  = document.getElementById("ctl-resume");
  const restartBtn = document.getElementById("ctl-restart");
  const dlg        = document.getElementById("diverge-dialog");
  const ddIndex    = document.getElementById("dd-index");
  const ddReason   = document.getElementById("dd-reason");

  let sessionID = null;
  let es        = null;
  let stepCount = 0;
  let paused    = false;

  function setStatus(label, cls) {
    statusEl.textContent = label;
    statusEl.className   = "status status-" + cls;
  }

  function setControlsRunning() {
    pauseBtn.disabled   = false;
    stepBtn.disabled    = !paused;
    resumeBtn.disabled  = !paused;
    restartBtn.disabled = false;
  }

  function setControlsEnded() {
    pauseBtn.disabled   = true;
    stepBtn.disabled    = true;
    resumeBtn.disabled  = true;
    restartBtn.disabled = false;
  }

  function clearLists() {
    recordedEl.replaceChildren();
    producedEl.replaceChildren();
    progressEl.textContent = "0";
    stepCount = 0;
  }

  // renderStep appends one row to each list. Diverged steps get a
  // red border and a click handler that opens the divergence dialog.
  function renderStep(step) {
    const idx = step.Index;

    const recRow = document.createElement("li");
    recRow.className = "replay-row";
    recRow.dataset.index = idx;
    if (step.Recorded && step.Recorded.Kind != null) {
      recRow.append(rowSpans("#" + (idx + 1), step.Recorded));
    } else {
      recRow.classList.add("missing");
      recRow.textContent = "(no recorded event at this index)";
    }

    const prodRow = document.createElement("li");
    prodRow.className = "replay-row";
    prodRow.dataset.index = idx;
    if (step.Diverged) {
      prodRow.classList.add("diverged");
      recRow.classList.add("diverged");
      prodRow.append(divergenceRow(step));
      const open = () => openDivergence(step);
      prodRow.addEventListener("click", open);
      recRow.addEventListener("click", open);
    } else if (step.Produced && step.Produced.Kind != null) {
      prodRow.append(rowSpans("#" + (idx + 1), step.Produced));
    } else {
      prodRow.classList.add("missing");
      prodRow.textContent = "(no produced event)";
    }

    recordedEl.appendChild(recRow);
    producedEl.appendChild(prodRow);
    stepCount++;
    progressEl.textContent = String(stepCount);
  }

  function rowSpans(seq, ev) {
    const frag = document.createDocumentFragment();
    const seqSpan  = document.createElement("span");
    seqSpan.className = "replay-seq";
    seqSpan.textContent = seq;
    const kindSpan = document.createElement("span");
    kindSpan.className = "replay-kind";
    kindSpan.textContent = kindName(ev.Kind);
    frag.append(seqSpan, kindSpan);
    return frag;
  }

  function divergenceRow(step) {
    const frag = document.createDocumentFragment();
    const tag = document.createElement("span");
    tag.className = "replay-tag-diverged";
    tag.textContent = "✗ diverged";
    const reason = document.createElement("span");
    reason.className = "replay-reason";
    reason.textContent = oneLine(step.DivergenceReason || "(no reason)");
    frag.append(tag, reason);
    return frag;
  }

  function oneLine(s) {
    return s.length > 80 ? s.slice(0, 77) + "…" : s;
  }

  // kindName mirrors event.Kind.String() in Go. Keeping it client-side
  // means the SSE payload doesn't need to ship the human label per
  // step — kinds are dense small ints, the list rarely changes.
  function kindName(k) {
    return [
      "?", "RunStarted", "UserMessageAppended", "TurnStarted",
      "ReasoningEmitted", "AssistantMessageCompleted",
      "ToolCallScheduled", "ToolCallCompleted", "ToolCallFailed",
      "SideEffectRecorded", "BudgetExceeded", "ContextTruncated",
      "RunCompleted", "RunFailed", "RunCancelled",
    ][k] || ("Kind(" + k + ")");
  }

  function openDivergence(step) {
    ddIndex.textContent  = String(step.Index);
    ddReason.textContent = step.DivergenceReason || "(no reason)";
    if (typeof dlg.showModal === "function") {
      dlg.showModal();
    } else {
      // dialog polyfill missing — fall back to alert.
      alert("Divergence at " + step.Index + ":\n\n" + step.DivergenceReason);
    }
  }

  // ----------------------------------------------------------------
  // session lifecycle
  // ----------------------------------------------------------------

  async function startSession() {
    setStatus("starting…", "init");
    const resp = await fetch("/run/" + encodeURIComponent(cfg.runID) + "/replay", {
      method:  "POST",
      headers: { "Content-Type": "application/json" },
    });
    if (!resp.ok) {
      const text = await resp.text().catch(() => "");
      setStatus("start failed (" + resp.status + ")", "err");
      console.error("replay start failed:", resp.status, text);
      return;
    }
    const body = await resp.json();
    sessionID = body.session_id;
    paused = false;
    setControlsRunning();
    setStatus("running", "ok");
    openStream();
  }

  function openStream() {
    if (es) { es.close(); }
    es = new EventSource(streamURL());
    es.addEventListener("step", (ev) => {
      try {
        const step = JSON.parse(ev.data);
        renderStep(step);
      } catch (err) {
        console.error("step parse:", err);
      }
    });
    es.addEventListener("end", (ev) => {
      const reason = safeJSONString(ev.data);
      setStatus(reason || "ended", "muted");
      setControlsEnded();
      es.close();
      es = null;
    });
    es.addEventListener("error", (ev) => {
      // EventSource auto-reconnects on transient drops, but a 404
      // (session GC'd) will re-fire forever. Close defensively;
      // restart is one click away.
      console.warn("SSE error:", ev);
      setStatus("disconnected", "err");
      setControlsEnded();
      if (es) { es.close(); es = null; }
    });
  }

  function streamURL() {
    return "/run/" + encodeURIComponent(cfg.runID) +
           "/replay/" + encodeURIComponent(sessionID) + "/stream";
  }

  function controlURL() {
    return "/run/" + encodeURIComponent(cfg.runID) +
           "/replay/" + encodeURIComponent(sessionID) + "/control";
  }

  function safeJSONString(s) {
    try { return JSON.parse(s); } catch (_) { return s; }
  }

  async function sendControl(action) {
    if (!sessionID) return;
    const resp = await fetch(controlURL(), {
      method:  "POST",
      headers: { "Content-Type": "application/json" },
      body:    JSON.stringify({ action: action }),
    });
    if (!resp.ok) {
      console.error("control " + action + " failed:", resp.status);
      return false;
    }
    return true;
  }

  // ----------------------------------------------------------------
  // wiring
  // ----------------------------------------------------------------

  pauseBtn.addEventListener("click", async () => {
    if (await sendControl("pause")) {
      paused = true;
      setStatus("paused", "warn");
      setControlsRunning();
    }
  });

  stepBtn.addEventListener("click", async () => {
    if (paused) {
      await sendControl("step");
    }
  });

  resumeBtn.addEventListener("click", async () => {
    if (await sendControl("resume")) {
      paused = false;
      setStatus("running", "ok");
      setControlsRunning();
    }
  });

  restartBtn.addEventListener("click", async () => {
    // Brand-new session — simpler than the server-side restart command
    // because the lists need to clear anyway and a fresh session_id
    // means no stale events leak through.
    if (es) { es.close(); es = null; }
    sessionID = null;
    clearLists();
    await startSession();
  });

  // Kick off on load.
  startSession();
})();
