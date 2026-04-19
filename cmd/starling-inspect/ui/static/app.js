// starling-inspect — page-local JS. Tiny helpers; no framework.

// Runs list: filter table rows by a substring of the run id.
// The runs.html template renders <tr data-runid="..."> rows inside
// #runs-body and an <input id="filter"> in the page header. If either
// is missing (e.g. on /run/{id}), this is a no-op.
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
})();

// Run detail: click a CallID link in the right pane → jump to the
// next event in the timeline that shares the same CallID (wrapping
// from the bottom back to the top). Skip the currently-shown event.
window.starlingJumpToCall = function (callID, fromSeq) {
  if (!callID) return;
  var rows = document.querySelectorAll("#timeline .ev[data-callid]");
  if (!rows.length) return;

  // Build the wrap-around order starting just after fromSeq.
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
