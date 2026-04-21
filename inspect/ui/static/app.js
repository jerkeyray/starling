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
    }
  });
})();

// Copy-to-clipboard: any element with data-copy="..." acts as a copy button.
// Works across HTMX swaps via event delegation on document.
(function initCopyButtons() {
  document.addEventListener("click", function (e) {
    var btn = e.target.closest && e.target.closest("[data-copy]");
    if (!btn) return;
    e.preventDefault();
    var text = btn.getAttribute("data-copy") || "";
    var done = function () {
      var prev = btn.textContent;
      btn.textContent = "copied";
      btn.classList.add("copied");
      setTimeout(function () {
        btn.textContent = prev;
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
