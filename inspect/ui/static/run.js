// Run-detail live-tail driver. Opens an EventSource against
// /run/{id}/events/stream when the run is non-terminal at page load,
// appends each arriving row's pre-rendered HTML to #timeline, and
// reloads the page on terminal so the server re-runs
// eventlog.Validate and paints a fresh badge.
//
// Wire format (mirrors inspect/live.go):
//   event: event     data: {"seq":N,"kind":"...","terminal":bool,"row_html":"..."}
//   event: end       data: "<reason>"
//   event: error     data: "<msg>"
//
// Zero-dependency vanilla JS — same constraint as replay.js.

(function () {
  "use strict";

  const head = document.querySelector(".run-head");
  if (!head) return;

  const runID       = head.dataset.runId || "";
  const terminalKnd = head.dataset.runTerminal || "";
  const lastSeqRaw  = head.dataset.lastSeq || "0";
  const timeline    = document.getElementById("timeline");
  const pill        = document.getElementById("live-status");
  const countEl     = document.getElementById("event-count");

  if (!runID || !timeline) return;
  if (terminalKnd !== "") return; // already terminal — no stream

  const lastSeq = parseInt(lastSeqRaw, 10) || 0;

  function setPill(label, state) {
    if (!pill) return;
    pill.hidden = false;
    pill.textContent = label;
    if (state) pill.dataset.state = state;
    else pill.removeAttribute("data-state");
  }

  function bumpCount() {
    if (!countEl) return;
    const n = parseInt(countEl.textContent, 10) || 0;
    countEl.textContent = String(n + 1);
  }

  // appendRow inserts server-rendered HTML. We trust the server (it
  // renders via html/template); payloads arrive over a same-origin SSE.
  function appendRow(html, seq) {
    // Skip if a row with this seq is already present — defends against
    // any future at-least-once quirk in the emitter.
    if (timeline.querySelector('[data-seq="' + seq + '"]')) return false;
    const tpl = document.createElement("template");
    tpl.innerHTML = html.trim();
    const node = tpl.content.firstElementChild;
    if (!node) return false;
    timeline.appendChild(node);
    return true;
  }

  const url = "/run/" + runID + "/events/stream?since=" + encodeURIComponent(String(lastSeq));
  const es  = new EventSource(url);
  setPill("● live", "ok");

  es.addEventListener("event", (ev) => {
    let frame;
    try { frame = JSON.parse(ev.data); }
    catch (err) { console.error("live frame parse:", err); return; }

    if (frame.row_html && appendRow(frame.row_html, frame.seq)) {
      bumpCount();
    }
    if (frame.terminal) {
      es.close();
      // Server-rendered validation badge needs a fresh Validate() pass.
      // Brief reload is cheaper than mirroring the check in JS.
      window.location.reload();
    }
  });

  es.addEventListener("end", () => {
    setPill("● disconnected", "disconnected");
    es.close();
  });

  es.addEventListener("error", () => {
    // EventSource will attempt auto-reconnect on transient drops;
    // close defensively so a dead run (session gone) doesn't retry
    // forever.
    setPill("● disconnected", "disconnected");
    es.close();
  });
})();
