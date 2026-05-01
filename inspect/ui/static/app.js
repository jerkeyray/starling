// starling-inspect — page-local JS. Tiny helpers; no framework.

// Runs list: filter table rows by a substring of the run id.
(function initRunsFilter() {
  var input = document.getElementById("filter");
  var body = document.getElementById("runs-body");
  if (!input || !body) return;

  function apply() {
    var q = input.value.trim().toLowerCase();
    var rows = body.querySelectorAll("tr[data-runid]");
    for (var i = 0; i < rows.length; i++) {
      var id = (rows[i].getAttribute("data-runid") || "").toLowerCase();
      rows[i].style.display = q === "" || id.indexOf(q) !== -1 ? "" : "none";
    }
  }

  input.addEventListener("input", apply);

  // "/" to focus from anywhere on the runs list page.
  document.addEventListener("keydown", function (e) {
    if (e.key === "/" && document.activeElement !== input) {
      e.preventDefault();
      input.focus();
      input.select();
    } else if (e.key === "Escape" && document.activeElement === input) {
      input.value = "";
      apply();
      input.blur();
    }
  });
})();

// Run detail: timeline filter. Matches against kind, call id, and summary.
(function initTimelineFilter() {
  var input = document.getElementById("timeline-filter");
  var timeline = document.getElementById("timeline");
  var count = document.getElementById("timeline-visible");
  if (!input || !timeline) return;

  function apply() {
    var q = input.value.trim().toLowerCase();
    var rows = timeline.querySelectorAll(".ev");
    var shown = 0;
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      if (q === "") {
        r.style.display = "";
        shown++;
        continue;
      }
      var hay = [
        r.dataset.kind || "",
        r.dataset.callid || "",
        r.dataset.summary || "",
        r.dataset.seq || "",
      ].join(" ").toLowerCase();
      var match = hay.indexOf(q) !== -1;
      r.style.display = match ? "" : "none";
      if (match) shown++;
    }
    if (count) {
      count.textContent = q === "" ? "" : shown + " of " + rows.length + " shown";
    }
  }

  input.addEventListener("input", apply);

  // Re-apply when new rows arrive via SSE so live-appended rows respect the filter.
  var obs = new MutationObserver(function () {
    if (input.value.trim() !== "") apply();
  });
  obs.observe(timeline, { childList: true });
})();

// Run detail: keyboard shortcuts.
//   j / ArrowDown — next event, k / ArrowUp — previous event
//   /             — focus filter
//   Esc           — clear filter, blur focus
(function initTimelineKeys() {
  var timeline = document.getElementById("timeline");
  if (!timeline) return;
  var filter = document.getElementById("timeline-filter");

  function visibleRows() {
    return Array.prototype.filter.call(
      timeline.querySelectorAll(".ev"),
      function (r) { return r.style.display !== "none"; }
    );
  }

  function activate(row) {
    if (!row) return;
    var link = row.querySelector("a");
    if (link && typeof link.click === "function") link.click();
    row.scrollIntoView({ block: "nearest" });
  }

  document.addEventListener("keydown", function (e) {
    var tag = (e.target && e.target.tagName) || "";
    var typing = tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";

    if (e.key === "/" && !typing && filter) {
      e.preventDefault();
      filter.focus();
      filter.select();
      return;
    }

    if (e.key === "Escape") {
      if (filter && document.activeElement === filter) {
        filter.value = "";
        filter.dispatchEvent(new Event("input"));
        filter.blur();
      }
      return;
    }

    if (typing) return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;

    var rows = visibleRows();
    if (!rows.length) return;
    var active = timeline.querySelector(".ev.active");
    var idx = active ? rows.indexOf(active) : -1;

    if (e.key === "j" || e.key === "ArrowDown") {
      e.preventDefault();
      activate(rows[Math.min(rows.length - 1, idx + 1)] || rows[0]);
    } else if (e.key === "k" || e.key === "ArrowUp") {
      e.preventDefault();
      activate(rows[Math.max(0, idx - 1)] || rows[0]);
    } else if (e.key === "g") {
      e.preventDefault();
      activate(rows[0]);
    } else if (e.key === "G") {
      e.preventDefault();
      activate(rows[rows.length - 1]);
    } else if (e.key === "y") {
      // 'y' (yank): copy the active event's hash. Helps when comparing
      // chains across two runs in the diff page or pasting into a bug
      // report — saves a click on the detail-pane copy button.
      e.preventDefault();
      var hashCode = document.querySelector(".detail-hashes dd:first-of-type code[data-copy]");
      if (hashCode && navigator.clipboard) {
        var v = hashCode.getAttribute("data-copy");
        navigator.clipboard.writeText(v || "");
        if (window.starlingToast) window.starlingToast("copied hash");
      }
    } else if (e.key === "w") {
      // 'w' (wrap): toggle line-wrap on the payload-json pane.
      e.preventDefault();
      window.starlingToggleWrap && window.starlingToggleWrap();
    } else if (e.key === "c") {
      // 'c' (copy): copy the run id from the page header.
      e.preventDefault();
      var rid = document.querySelector(".runid-mono[data-copy]");
      if (rid && navigator.clipboard) {
        var v = rid.getAttribute("data-copy");
        navigator.clipboard.writeText(v || "");
        if (window.starlingToast) window.starlingToast("copied run id");
      }
    }
  });
})();

// Copy-to-clipboard: any element with data-copy="..." acts as a copy
// trigger. Works across HTMX swaps via event delegation on document.
//
// For real <button>s with text we swap the label briefly to "copied"
// (matches user expectation for an explicit copy button). For
// inline elements (like a <code> hash readout or the runid-mono
// span) we leave their text alone, just toggle a `.copied` class
// and surface a transient toast so the user gets feedback without
// the displayed hash bytes getting clobbered.
(function initCopyButtons() {
  document.addEventListener("click", function (e) {
    var btn = e.target.closest && e.target.closest("[data-copy]");
    if (!btn) return;
    e.preventDefault();
    var text = btn.getAttribute("data-copy") || "";
    var isButton = btn.tagName === "BUTTON";
    var iconOnly = !!btn.querySelector("svg");
    var swapLabel = isButton && !iconOnly;
    var done = function () {
      var prev = btn.textContent;
      btn.classList.add("copied");
      if (swapLabel) btn.textContent = "copied";
      else showToast("copied " + summarize(text));
      setTimeout(function () {
        if (swapLabel) btn.textContent = prev;
        btn.classList.remove("copied");
      }, 1200);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done).catch(function () {
        fallbackCopy(text); done();
      });
    } else {
      fallbackCopy(text); done();
    }
  });

  function summarize(s) {
    if (!s) return "";
    if (s.length <= 22) return s;
    return s.slice(0, 12) + "…";
  }

  function fallbackCopy(text) {
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand("copy"); } catch (_) {}
    document.body.removeChild(ta);
  }
})();

// Generic transient toast — used by the copy handler for inline
// triggers, and available to other UI bits that want a quick
// confirmation without committing to a dialog.
var _toastEl = null, _toastTimer = null;
function showToast(msg) {
  if (!_toastEl) {
    _toastEl = document.createElement("div");
    _toastEl.className = "toast";
    document.body.appendChild(_toastEl);
  }
  _toastEl.textContent = msg;
  _toastEl.classList.add("visible");
  if (_toastTimer) clearTimeout(_toastTimer);
  _toastTimer = setTimeout(function () {
    if (_toastEl) _toastEl.classList.remove("visible");
  }, 1400);
}
window.starlingToast = showToast;

// Theme toggle — flips the data-theme attribute on <html> and persists
// the user's choice in localStorage. The preload snippet in
// layout.html applies the persisted value before paint to avoid a
// flash of the wrong theme. With nothing stored, the page follows
// prefers-color-scheme.
(function initThemeToggle() {
  var btn = document.getElementById("theme-toggle");
  if (!btn) return;
  btn.addEventListener("click", function () {
    var root = document.documentElement;
    var current = root.getAttribute("data-theme");
    var next;
    if (current === "dark") next = "light";
    else if (current === "light") next = "dark";
    else {
      // No explicit choice yet — flip away from whatever the OS
      // gave us. matchMedia tells us the current effective theme.
      var prefersLight = window.matchMedia &&
        window.matchMedia("(prefers-color-scheme: light)").matches;
      next = prefersLight ? "dark" : "light";
    }
    root.setAttribute("data-theme", next);
    try { localStorage.setItem("starling.theme", next); } catch (e) {}
  });
})();

// Run detail: click a CallID link → jump to next event in the timeline
// sharing the same CallID (wrapping from bottom to top).
window.starlingJumpToCall = function (callID, fromSeq) {
  if (!callID) return;
  var rows = document.querySelectorAll("#timeline .ev[data-callid]");
  if (!rows.length) return;

  var ordered = [];
  for (var i = 0; i < rows.length; i++) {
    if (parseInt(rows[i].dataset.seq, 10) > fromSeq) ordered.push(rows[i]);
  }
  for (var j = 0; j < rows.length; j++) {
    if (parseInt(rows[j].dataset.seq, 10) <= fromSeq) ordered.push(rows[j]);
  }

  for (var k = 0; k < ordered.length; k++) {
    if (ordered[k].dataset.callid === callID) {
      var link = ordered[k].querySelector("a");
      if (link && typeof link.click === "function") link.click();
      ordered[k].scrollIntoView({ block: "nearest" });
      return;
    }
  }
};

// Payload-pane wrap toggle. The user choice persists across pages
// and HTMX detail-pane swaps via localStorage. Default is wrapped
// (long string values reflow); 'no-wrap' falls back to strict-pre
// for diff-by-eye work. Wired to the toolbar button (data-wrap-toggle)
// and the 'w' keyboard shortcut.
(function initWrapToggle() {
  var KEY = 'starling.payload.nowrap';
  function isNoWrap() {
    try { return localStorage.getItem(KEY) === '1'; } catch (e) { return false; }
  }
  function apply() {
    var pane = document.querySelector('.payload-json');
    if (!pane) return;
    var btn = document.querySelector('[data-wrap-toggle]');
    if (isNoWrap()) {
      pane.classList.add('no-wrap');
      if (btn) btn.classList.add('active');
    } else {
      pane.classList.remove('no-wrap');
      if (btn) btn.classList.remove('active');
    }
  }
  window.starlingToggleWrap = function () {
    var v = isNoWrap();
    try { localStorage.setItem(KEY, v ? '0' : '1'); } catch (e) {}
    apply();
    if (window.starlingToast) {
      window.starlingToast(v ? 'wrap on' : 'wrap off');
    }
  };
  document.addEventListener('click', function (e) {
    var t = e.target && e.target.closest && e.target.closest('[data-wrap-toggle]');
    if (!t) return;
    e.preventDefault();
    window.starlingToggleWrap();
  });
  // Re-apply after every HTMX swap of the detail pane.
  document.body && document.body.addEventListener && document.body.addEventListener('htmx:afterSwap', apply);
  apply();
})();

