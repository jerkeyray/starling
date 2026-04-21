// Run-detail live-tail driver. Opens an EventSource on non-terminal runs,
// appends each arriving row's pre-rendered HTML to #timeline, reloads on
// terminal so the server re-runs eventlog.Validate.
//
// Wire format (mirrors inspect/live.go):
//   event: event     data: {"seq":N,"kind":"...","terminal":bool,"row_html":"..."}
//   event: end       data: "<reason>"
//   event: error     data: "<msg>"

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
  const reconnectEl = document.getElementById("reconnect");

  if (!runID || !timeline) return;
  if (terminalKnd !== "") return;

  let lastSeq = parseInt(lastSeqRaw, 10) || 0;
  let es = null;

  function setPill(label, state) {
    if (!pill) return;
    pill.hidden = false;
    pill.textContent = label;
    if (state) pill.dataset.state = state;
    else pill.removeAttribute("data-state");
  }

  function showReconnect(show) {
    if (!reconnectEl) return;
    reconnectEl.hidden = !show;
    reconnectEl.style.display = show ? "inline-block" : "none";
  }

  function bumpCount() {
    if (!countEl) return;
    const n = parseInt(countEl.textContent, 10) || 0;
    countEl.textContent = String(n + 1);
  }

  function appendRow(html, seq) {
    if (timeline.querySelector('[data-seq="' + seq + '"]')) return false;
    const tpl = document.createElement("template");
    tpl.innerHTML = html.trim();
    const node = tpl.content.firstElementChild;
    if (!node) return false;
    timeline.appendChild(node);
    return true;
  }

  function connect() {
    showReconnect(false);
    const url = "/run/" + runID + "/events/stream?since=" + encodeURIComponent(String(lastSeq));
    es = new EventSource(url);
    setPill("live", "ok");

    es.addEventListener("event", (ev) => {
      let frame;
      try { frame = JSON.parse(ev.data); }
      catch (err) { console.error("live frame parse:", err); return; }

      if (frame.row_html && appendRow(frame.row_html, frame.seq)) {
        bumpCount();
      }
      if (typeof frame.seq === "number" && frame.seq > lastSeq) {
        lastSeq = frame.seq;
      }
      if (frame.terminal) {
        es.close();
        window.location.reload();
      }
    });

    es.addEventListener("end", () => {
      setPill("disconnected", "disconnected");
      showReconnect(true);
      es.close();
    });

    es.addEventListener("error", () => {
      setPill("disconnected", "disconnected");
      showReconnect(true);
      if (es) es.close();
    });
  }

  if (reconnectEl) {
    reconnectEl.addEventListener("click", connect);
  }

  connect();
})();
