// Keeps the page live without a full reload: swaps in a fresh board every
// 30 seconds and keeps the "Updated ... ago" label honest in between. Also
// opens a day breakdown when a bar cell is clicked (database history only).
(function () {
  "use strict";

  var POLL_MS = 30000;
  // Minutes per cell in the day breakdown: quarter hours, or hours on a
  // phone, where 96 cells would be slivers (the width matches style.css).
  var phone = window.matchMedia ? window.matchMedia("(max-width: 639px)") : { matches: false };
  function slotMinutes() { return phone.matches ? 60 : 15; }
  var board = document.getElementById("board");
  var favicon = document.querySelector('link[rel="icon"]');
  var siteTitle = (document.title.split(" · ").pop() || "").trim();
  var updated = document.getElementById("updated"); // in the Now header
  var generatedAt = updated ? new Date(updated.getAttribute("datetime")) : new Date();
  var open = null; // {comp, date} of the open day view, so it survives refreshes
  var toggled = {}; // subs element id -> true/false, the user's own expand/collapse choices

  var LABEL = { operational: "Operational", maintenance: "Under maintenance", degraded: "Degraded", outage: "Outage", unknown: "No data" };
  var RANK = { unknown: 0, operational: 1, maintenance: 2, degraded: 3, outage: 4 };

  function ago(date) {
    var s = Math.max(0, Math.round((Date.now() - date.getTime()) / 1000));
    if (s < 10) return "just now";
    if (s < 60) return s + " s ago";
    var m = Math.round(s / 60);
    if (m < 60) return m + " min ago";
    var h = Math.round(m / 60);
    if (h < 48) return h + " h ago";
    return Math.round(h / 24) + " days ago";
  }

  // The board is replaced on refresh, so the label is looked up each time.
  function tick() {
    var label = board.querySelector(".now__ago");
    if (label) label.textContent = ago(generatedAt);
  }

  function refresh() {
    if (document.hidden) return;
    fetch("/partials/board", { headers: { Accept: "text/html" }, cache: "no-store" })
      .then(function (res) {
        if (!res.ok) throw new Error("status " + res.status);
        return res.text().then(function (html) {
          board.innerHTML = html;
          column = -1; // the outlined cells were replaced
          hideTip();
          cancelHide();
          var status = res.headers.get("X-Status") || "unknown";
          var headline = res.headers.get("X-Headline") || "";
          var gen = res.headers.get("X-Generated-At");
          document.body.setAttribute("data-status", status);
          if (headline) document.title = headline + (siteTitle ? " · " + siteTitle : "");
          if (gen) generatedAt = new Date(gen);
          if (favicon) favicon.href = "/favicon.svg?s=" + status + "&t=" + Date.now();
          tick();
          applyToggles();
          if (open) reopen(open.comp, open.date);
        });
      })
      .catch(function (err) {
        // Leave the last good board in place; the "updated" label will age.
        console.warn("sutats: refresh failed", err);
      });
  }

  // ---- expand / collapse a component's signal rows ----

  function setExpanded(id, expanded) {
    var subs = document.getElementById(id);
    var btn = board.querySelector('.row__toggle[data-target="' + id + '"]');
    if (!subs || !btn) return;
    subs.hidden = !expanded;
    btn.setAttribute("aria-expanded", expanded ? "true" : "false");
  }

  // The server decides the default (open when something needs attention);
  // the user's own choices win over it, including across refreshes.
  function applyToggles() {
    Object.keys(toggled).forEach(function (id) { setExpanded(id, toggled[id]); });
  }

  // ---- day breakdown ----

  function pad(n) { return (n < 10 ? "0" : "") + n; }
  function hhmm(d) { return pad(d.getUTCHours()) + ":" + pad(d.getUTCMinutes()); }
  function dur(sec) {
    sec = Math.round(sec);
    if (sec < 60) return sec + "s";
    var m = Math.round(sec / 60);
    if (m < 60) return m + "m";
    return Math.floor(m / 60) + "h " + (m % 60) + "m";
  }
  function ms(v) { return v >= 1000 ? (v / 1000).toFixed(1) + " s" : Math.round(v) + " ms"; }
  function niceDate(iso) {
    var d = new Date(iso + "T00:00:00Z");
    return d.toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric", timeZone: "UTC" });
  }
  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  // The component a clicked cell belongs to: its own row, or the row its
  // checks hang under (subs-<id>).
  function componentOf(cell) {
    var article = cell.closest("article.row--component");
    if (article) return article;
    var subs = cell.closest(".subs");
    return subs ? document.getElementById(subs.id.replace(/^subs-/, "")) : null;
  }

  function fetchDay(key, date, slotMin) {
    var url = "/api/v1/series?key=" + encodeURIComponent(key) + "&from=" + date + "&bucket=" + slotMin + "m";
    return fetch(url, { cache: "no-store" }).then(function (res) {
      if (!res.ok) return res.text().then(function (t) { throw new Error(t || ("status " + res.status)); });
      return res.json();
    }).then(function (data) { return data.points || []; });
  }

  // What a day's points add up to.
  function summarise(points) {
    var s = { worst: "unknown", observed: 0, downtime: 0 };
    points.forEach(function (p) {
      if (RANK[p.status] > RANK[s.worst]) s.worst = p.status;
      s.observed += p.observed_seconds || 0;
      s.downtime += p.downtime_seconds || 0;
    });
    return s;
  }

  function uptime(s) { return s.observed > 0 ? (100 * (1 - s.downtime / s.observed)).toFixed(2) + "%" : "No data"; }

  function dayBar(date, points, label, slotMin) {
    var slots = (24 * 60) / slotMin;
    var start = Date.parse(date + "T00:00:00Z");
    var cells = new Array(slots);
    points.forEach(function (p) {
      var i = Math.floor((Date.parse(p.time) - start) / (slotMin * 60000));
      if (i >= 0 && i < slots) cells[i] = p;
    });
    var bar = el("div", "bar bar--day");
    bar.setAttribute("role", "img");
    bar.setAttribute("aria-label", label + ", " + niceDate(date) + " in " + slotMin + " minute steps");
    for (var i = 0; i < slots; i++) {
      var p = cells[i];
      var status = p ? p.status : "unknown";
      var c = el("i", "bar__day bar__day--" + status);
      var from = new Date(start + i * slotMin * 60000), to = new Date(start + (i + 1) * slotMin * 60000);
      var tipText = hhmm(from) + "–" + hhmm(to) + " · " + (LABEL[status] || status);
      if (p && p.downtime_seconds > 0) tipText += " · " + dur(p.downtime_seconds) + " down";
      if (p && p.latency_ms != null) tipText += " · " + ms(p.latency_ms);
      c.setAttribute("data-tip", tipText); // shown at once by the hover label
      bar.appendChild(c);
    }
    return bar;
  }

  // Same axis as the page's: a slot per cell, a label every six hours.
  function dayAxis(date, slotMin) {
    var slots = (24 * 60) / slotMin;
    var every = 6 * 60 / slotMin; // a time label every six hours
    var start = Date.parse(date + "T00:00:00Z");
    var axis = el("div", "axis");
    axis.setAttribute("aria-hidden", "true");
    for (var j = 0; j < slots; j++) {
      var mark = j > 0 && j % every === 0;
      var slot = el("span", "axis__slot" + (mark ? " axis__slot--mark" : "") + (j === slots - 1 ? " axis__slot--end" : ""));
      // In hours (on a phone) there is no room for 24:00 after 18:00.
      var end = j === slots - 1 && slotMin < 60;
      var text = j === 0 ? "00:00 UTC" : mark ? hhmm(new Date(start + j * slotMin * 60000)) : end ? "24:00" : "";
      if (text) slot.appendChild(el("span", "axis__label", text));
      axis.appendChild(slot);
    }
    return axis;
  }

  function shiftDate(date, days) {
    return new Date(Date.parse(date + "T00:00:00Z") + days * 864e5).toISOString().slice(0, 10);
  }

  function textButton(label, onClick, disabled) {
    var b = el("button", "dayview__nav", label);
    b.type = "button";
    if (disabled) b.disabled = true;
    else b.addEventListener("click", onClick);
    return b;
  }

  // ---- day view: the component's section slides out, the day's slides in ----
  //
  // Clicking a day on any of a component's bars slides the whole section
  // (its row and its checks) out of the column to the left while a section
  // for that day slides in from the right: a copy of the same rows, each bar
  // showing the day in 15-minute cells (hours on a phone) with the day's
  // uptime, the way back and the neighbouring days where the status was
  // (see dayPane). Previous/next slide the way time runs; Back slides back.
  // Both sections sit in one frame the width of the column, which hides
  // whatever leaves it. One day is open at a time.

  var busy = false; // during a slide, clicks wait
  var lessMotion = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  // Builds the section for one day of a component as a copy of its 90-day
  // section, so every row and divider stays where it was: the same rows in
  // the same order and size (descriptions, notes and configuration rows
  // included, collapsed sections still collapsed), with each bar showing
  // the day and each number the day's uptime. The component's Now column
  // carries the way back and the neighbouring days, the date sits by the
  // name, and the hour axis sits under the component's own bar, in space it
  // already has (where rows stack, it takes a line of its own).
  // height: the 90-day row's height, which the day's row keeps.
  function dayPane(article, date, height) {
    var pane = el("div", "slide__pane slide__pane--day");
    var slotMin = slotMinutes();
    var subs = document.getElementById("subs-" + article.id);
    var ownBar = article.querySelector(".bar[data-key]");
    var first = ownBar && ownBar.querySelector(".bar__day[data-date]");
    var oldest = first ? first.getAttribute("data-date") : date;
    var today = new Date().toISOString().slice(0, 10);
    var daySubsId = subs ? "day-" + subs.id : null;

    var top = article.cloneNode(true);
    var subsCopy = subs ? subs.cloneNode(true) : null;
    [top, subsCopy].forEach(function (root) {
      if (!root) return;
      root.removeAttribute("id");
      var withIds = root.querySelectorAll("[id]");
      for (var i = 0; i < withIds.length; i++) withIds[i].removeAttribute("id");
    });
    if (subsCopy) subsCopy.id = daySubsId;
    // The name's toggle opens and closes this copy's checks, not the page's.
    var toggle = top.querySelector(".row__toggle");
    if (toggle && daySubsId) { toggle.setAttribute("data-target", daySubsId); toggle.setAttribute("aria-controls", daySubsId); }
    if (height) top.style.minHeight = height + "px";

    // The date by the name, the day's buttons in the Now column: the way
    // back where the state was, the neighbouring days under it.
    var nameEl = top.querySelector(".row__name");
    if (nameEl) nameEl.appendChild(el("span", "dayview__date", niceDate(date)));
    var nowCell = top.querySelector(".row__now");
    var nav = el("div", "row__now dayview__actions");
    var back = textButton("← Back", function () { if (!busy) closeDay(); }, false);
    back.classList.add("dayview__back");
    back.setAttribute("aria-label", "Back to 90 days");
    nav.appendChild(back);
    var steps = el("div", "dayview__steps");
    var prev = textButton("‹ " + shortDate(shiftDate(date, -1)), function () { if (!busy) moveDay(-1); }, date <= oldest);
    prev.setAttribute("aria-label", "Previous day");
    steps.appendChild(prev);
    var next = textButton(shortDate(shiftDate(date, 1)) + " ›", function () { if (!busy) moveDay(1); }, date >= today);
    next.setAttribute("aria-label", "Next day");
    steps.appendChild(next);
    nav.appendChild(steps);
    if (nowCell) nowCell.replaceWith(nav); else top.appendChild(nav);

    pane.appendChild(top);
    if (subsCopy) pane.appendChild(subsCopy);

    // Each bar in the copy becomes the day, filled in as its data arrives.
    function swap(row, isOwn) {
      var bar = row.querySelector(".bar[data-key]");
      if (!bar) return;
      var key = bar.getAttribute("data-key");
      var nameNode = row.querySelector(".sub__name") || row.querySelector(".row__name");
      var label = nameNode ? nameNode.textContent.trim() : "";
      var holder = dayBar(date, [], label, slotMin);
      bar.replaceWith(holder);
      // Now belongs to today only; on other days the column stays empty.
      var nowEl = row.querySelector(".row__now:not(.dayview__actions)");
      if (nowEl && date !== today) nowEl.classList.add("row__now--past");
      if (isOwn) holder.insertAdjacentElement("afterend", dayAxis(date, slotMin)); // the hours
      var num = row.querySelector(".row__num");
      if (num) num.textContent = "";
      var detail = row.querySelector(".sub__detail");
      if (detail) { detail.textContent = ""; detail.classList.remove("sub__detail--bad"); }
      var nm = row.querySelector(".sub__name");
      if (nm) nm.classList.remove("sub__name--bad");
      var note = row.querySelector(".sub__note"); // keeps its line, says nothing about this day
      if (note) note.style.visibility = "hidden";
      fetchDay(key, date, slotMin).then(function (points) {
        if (!holder.isConnected) return;
        holder.replaceWith(dayBar(date, points, label, slotMin));
        var s = summarise(points);
        if (num) num.textContent = uptime(s);
        if (detail && s.downtime > 0) detail.textContent = dur(s.downtime) + " down";
      }, function () {
        if (num) num.textContent = "Error";
      });
    }
    swap(top, true);
    if (subsCopy) {
      var rows = subsCopy.querySelectorAll(".row");
      for (var r = 0; r < rows.length; r++) swap(rows[r], false);
    }
    return pane;
  }

  // Slides one pane out and another in, across the frame. dir 1 moves
  // left (forward), -1 right (back). Resolves when done.
  function slide(outPane, inPane, dir) {
    var from = outPane.getBoundingClientRect().height;
    inPane.hidden = false;
    if (lessMotion || !inPane.animate) { outPane.hidden = true; return Promise.resolve(); }
    var to = inPane.getBoundingClientRect().height;
    var ms = 320, ease = "cubic-bezier(.4, 0, .2, 1)";
    var a = outPane.animate([
      { transform: "translateX(0)", opacity: 1 },
      { transform: "translateX(" + (-100 * dir) + "%)", opacity: 0.3 }
    ], { duration: ms, easing: ease });
    var b = inPane.animate([
      { transform: "translateX(" + (100 * dir) + "%)", opacity: 0.3 },
      { transform: "translateX(0)", opacity: 1 }
    ], { duration: ms, easing: ease });
    var done = [a.finished, b.finished];
    // Where the two differ in height (a stacked row gains a line for its
    // hours), the frame eases between them, so what follows moves with the
    // slide instead of jumping.
    if (Math.abs(to - from) > 0.5) {
      done.push(inPane.parentNode.animate([{ height: from + "px" }, { height: to + "px" }], { duration: ms, easing: ease }).finished);
    }
    return Promise.all(done).catch(function () {}).then(function () { outPane.hidden = true; });
  }

  // Wraps a component's section in a frame with its 90-day pane, once.
  function frameFor(article) {
    var subs = document.getElementById("subs-" + article.id);
    var frame = el("div", "slide");
    var main = el("div", "slide__pane slide__pane--main");
    article.insertAdjacentElement("beforebegin", frame);
    main.appendChild(article);
    if (subs) main.appendChild(subs);
    frame.appendChild(main);
    return { frame: frame, main: main };
  }

  function unframe(frame, main) {
    while (main.firstChild) frame.insertAdjacentElement("beforebegin", main.firstChild);
    frame.remove();
  }

  // Opens a day. still: no motion (after a refresh).
  function openDay(article, date, still) {
    hideColumn();
    if (open) closeDay(true);
    var height = article.getBoundingClientRect().height; // exact, measured while still shown
    var f = frameFor(article);
    var pane = dayPane(article, date, height);
    pane.hidden = true;
    f.frame.appendChild(pane);
    open = { comp: article.id, date: date, frame: f.frame, main: f.main, pane: pane, height: height };
    if (still) { pane.hidden = false; f.main.hidden = true; return; }
    busy = true;
    slide(f.main, pane, 1).then(function () { busy = false; });
  }

  function moveDay(step) {
    var article = document.getElementById(open.comp);
    var next = dayPane(article, shiftDate(open.date, step), open.height);
    next.hidden = true;
    open.frame.appendChild(next);
    var old = open.pane;
    open.pane = next;
    open.date = shiftDate(open.date, step);
    busy = true;
    slide(old, next, step).then(function () { old.remove(); busy = false; });
  }

  function closeDay(instant) {
    if (!open) return;
    hideColumn();
    var o = open;
    open = null;
    if (instant === true) { unframe(o.frame, o.main); return; }
    busy = true;
    slide(o.pane, o.main, -1).then(function () { unframe(o.frame, o.main); busy = false; });
  }

  // After a refresh replaces the board, open the same day again, still.
  function reopen(comp, date) {
    var article = document.getElementById(comp);
    open = null; // the board it described is gone
    busy = false;
    if (article) openDay(article, date, true);
  }

  board.addEventListener("click", function (ev) {
    var toggle = ev.target.closest && ev.target.closest("button.row__toggle[data-target]");
    if (toggle) {
      var id = toggle.getAttribute("data-target");
      var expanded = toggle.getAttribute("aria-expanded") !== "true";
      toggled[id] = expanded;
      setExpanded(id, expanded);
      return;
    }
    var cell = ev.target.closest && ev.target.closest("button.bar__day[data-date]");
    if (!cell || busy) return;
    var article = componentOf(cell);
    if (!article) return;
    var date = cell.getAttribute("data-date");
    if (open && open.comp === article.id && open.date === date) { closeDay(); return; }
    openDay(article, date, false);
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape" && open && !busy) closeDay();
  });

  // Hovering (or focusing) a day outlines that day in every 90-day bar and
  // shows its date just above the bar under the pointer, so one day can be
  // followed down the page.
  // The day view's bars are hours, not days, and are left out.
  var column = -1;
  function shortDate(iso) {
    // Same locale and order as niceDate, without the year: "Oct 14".
    return new Date(iso + "T00:00:00Z").toLocaleDateString(undefined, { day: "numeric", month: "short", timeZone: "UTC" });
  }
  // What the pointer is over decides which bars take part: the 90-day bars
  // of the page, or the day bars of an open day's section (time slots).
  function hoverContext(target) {
    if (!target.closest) return null;
    var pane = target.closest(".slide__pane--day");
    if (pane) return { scope: pane, ref: pane.querySelector(".bar--day"), day: true };
    if (target.closest(".ledger")) return { scope: board, ref: board.querySelector(".ledger--system .bar"), day: false };
    return null;
  }
  function barsIn(ctx) {
    var all = ctx.scope.querySelectorAll(".bar"), out = [];
    for (var i = 0; i < all.length; i++) {
      var isDay = all[i].classList.contains("bar--day");
      if (ctx.day ? isDay : !isDay && !all[i].closest(".slide__pane--day")) out.push(all[i]);
    }
    return out;
  }

  // The hovered cell's description floats just above the bar under the
  // pointer, at once, in place of the browser's delayed tooltip.
  var tip = null, tipBar = null, hoverCtx = null;
  function hideTip() { tipBar = null; if (tip) tip.hidden = true; }
  function hideColumn() {
    hideTip();
    if (column < 0) return;
    column = -1;
    hoverCtx = null;
    var cells = board.querySelectorAll(".bar__day--column");
    for (var i = 0; i < cells.length; i++) cells[i].classList.remove("bar__day--column");
    hideTip();
  }
  // The bar of the row under the pointer that belongs to the context.
  function barAt(target, ctx) {
    var row = target.closest && target.closest(".row");
    if (!row) return null;
    var bar = row.querySelector(ctx.day ? ".bar--day" : ".bar:not(.bar--day)");
    return bar && barsIn(ctx).indexOf(bar) >= 0 ? bar : null;
  }
  // Puts text just above target, centred on it and kept within area.
  function tipAt(target, area, text) {
    if (!tip) {
      tip = el("div", "daytip");
      tip.setAttribute("aria-hidden", "true");
      document.body.appendChild(tip);
    }
    tip.textContent = text;
    tip.hidden = false;
    var c = target.getBoundingClientRect(), b = area.getBoundingClientRect();
    var w = tip.offsetWidth, h = tip.offsetHeight;
    var x = Math.max(b.left, Math.min(c.left + c.width / 2 - w / 2, b.right - w));
    tip.style.left = (x + window.scrollX) + "px";
    tip.style.top = (b.top + window.scrollY - h - 4) + "px";
  }
  function placeTip(bar, i) {
    var cell = bar.children[i];
    var text = cell && cell.getAttribute("data-tip");
    if (!text) { hideTip(); return; }
    tipAt(cell, bar, text);
    tipBar = bar;
  }
  // Outlines cell i in every bar of the context and describes it above bar
  // (or, between rows, above the bar it was last on, else the first bar).
  function showColumn(i, bar, ctx) {
    var sameCtx = hoverCtx && hoverCtx.scope === ctx.scope && hoverCtx.day === ctx.day;
    if (!sameCtx) { hideColumn(); hoverCtx = ctx; }
    bar = bar || tipBar || ctx.ref;
    if (i === column && bar === tipBar && bar && bar.isConnected) return;
    var old = board.querySelectorAll(".bar__day--column");
    for (var o = 0; o < old.length; o++) old[o].classList.remove("bar__day--column");
    var bars = barsIn(ctx);
    for (var k = 0; k < bars.length; k++) {
      var c = bars[k].children[i];
      if (c) c.classList.add("bar__day--column");
    }
    column = i;
    if (bar) placeTip(bar, i);
  }
  function dayCell(target) {
    var cell = target.closest && target.closest(".bar__day");
    return cell && !cell.closest(".bar--day") ? cell : null;
  }
  // The cell under the pointer comes from its horizontal position in the bar
  // column, not from the element under it, so crossing the 2px gaps between
  // cells or the space between rows does not clear it. It clears after a
  // short pause once the pointer leaves the bar column.
  var hideTimer = null;
  function cancelHide() { if (hideTimer) { clearTimeout(hideTimer); hideTimer = null; } }
  function hideSoon() { if ((column >= 0 || (tip && !tip.hidden)) && !hideTimer) hideTimer = setTimeout(function () { hideTimer = null; hideColumn(); }, 150); }
  function columnAt(ev, ctx) {
    var ref = ctx && ctx.ref;
    if (!ref) return -1;
    var r = ref.getBoundingClientRect(), n = ref.children.length;
    // On a phone the oldest days are hidden: count from the first shown.
    var skip = 0;
    while (skip < n && ref.children[skip].offsetWidth === 0) skip++;
    var shown = n - skip;
    if (!shown || ev.clientX < r.left || ev.clientX >= r.right) return -1;
    var w = (r.width - 2 * (shown - 1)) / shown; // cell width; cells are 2px apart
    return skip + Math.min(shown - 1, Math.floor((ev.clientX - r.left + 1) / (w + 2)));
  }
  var pending = null;
  // Touch has no hover: a tap opens the day, which shows its date.
  board.addEventListener("pointermove", function (ev) {
    if (ev.pointerType !== "mouse") return;
    if (pending) { pending = ev; return; }
    pending = ev;
    requestAnimationFrame(function () {
      var e = pending;
      pending = null;
      var ctx = hoverContext(e.target);
      var i = columnAt(e, ctx);
      if (i >= 0) { cancelHide(); showColumn(i, barAt(e.target, ctx), ctx); } else hideSoon();
    });
  });
  // Keyboard focus only: a tap focuses the cell too, and opens it anyway.
  board.addEventListener("focusin", function (ev) {
    var cell = dayCell(ev.target);
    if (cell && cell.matches(":focus-visible")) { cancelHide(); showColumn(Array.prototype.indexOf.call(cell.parentNode.children, cell), cell.parentNode, hoverContext(cell)); }
  });
  board.addEventListener("focusout", hideSoon);
  board.addEventListener("mouseleave", function () { cancelHide(); hideColumn(); });

  // Crossing the phone width with a day open: open it again in the cells
  // that fit (hours or quarter hours).
  if (phone.addEventListener) phone.addEventListener("change", function () {
    if (!open || busy) return;
    var o = open;
    closeDay(true);
    var article = document.getElementById(o.comp);
    if (article) openDay(article, o.date, true);
  });

  setInterval(tick, 1000);
  setInterval(refresh, POLL_MS);
  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) refresh();
  });
})();
