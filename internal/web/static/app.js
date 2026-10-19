// Keeps the page live without a full reload: swaps in a fresh board every
// 30 seconds and keeps the "Updated ... ago" label honest in between. Also
// opens a day breakdown when a bar cell is clicked (database history only).
(function () {
  "use strict";

  var POLL_MS = 30000;
  var SLOT_MIN = 15; // minutes per cell in the day breakdown
  var board = document.getElementById("board");
  var updated = document.getElementById("updated");
  var favicon = document.querySelector('link[rel="icon"]');
  var siteTitle = (document.title.split(" · ").pop() || "").trim();
  var generatedAt = updated ? new Date(updated.getAttribute("datetime")) : new Date();
  var open = null; // {key, date} of the open day panel, so it survives refreshes
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

  function tick() {
    if (updated) updated.textContent = ago(generatedAt);
  }

  function refresh() {
    if (document.hidden) return;
    fetch("/partials/board", { headers: { Accept: "text/html" }, cache: "no-store" })
      .then(function (res) {
        if (!res.ok) throw new Error("status " + res.status);
        return res.text().then(function (html) {
          board.innerHTML = html;
          var status = res.headers.get("X-Status") || "unknown";
          var headline = res.headers.get("X-Headline") || "";
          var gen = res.headers.get("X-Generated-At");
          document.body.setAttribute("data-status", status);
          if (headline) document.title = headline + (siteTitle ? " · " + siteTitle : "");
          if (gen) {
            generatedAt = new Date(gen);
            if (updated) updated.setAttribute("datetime", gen);
          }
          if (favicon) favicon.href = "/favicon.svg?s=" + status + "&t=" + Date.now();
          tick();
          applyToggles();
          if (open) reopen(open.key, open.date);
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

  function closePanel() {
    var p = board.querySelector(".daypanel");
    if (p) p.remove();
    var sel = board.querySelector(".bar__day--selected");
    if (sel) sel.classList.remove("bar__day--selected");
    open = null;
  }

  function findCell(key, date) {
    var bars = board.querySelectorAll('.bar[data-key]');
    for (var i = 0; i < bars.length; i++) {
      if (bars[i].getAttribute("data-key") !== key) continue;
      return bars[i].querySelector('.bar__day[data-date="' + date + '"]');
    }
    return null;
  }

  function reopen(key, date) {
    var cell = findCell(key, date);
    if (cell) openPanel(cell, key, date, true);
    else open = null;
  }

  function openPanel(cell, key, date, silent) {
    closePanel();
    open = { key: key, date: date };
    cell.classList.add("bar__day--selected");
    var bar = cell.parentNode;
    var panel = el("div", "daypanel");
    panel.setAttribute("aria-live", "polite");
    var head = el("div", "daypanel__head");
    head.appendChild(el("span", "daypanel__date", niceDate(date)));
    var summary = el("span", "daypanel__summary", silent ? "" : "Loading…");
    head.appendChild(summary);
    var close = el("button", "daypanel__close", "Close");
    close.type = "button";
    close.addEventListener("click", closePanel);
    head.appendChild(close);
    panel.appendChild(head);
    bar.insertAdjacentElement("afterend", panel);

    var url = "/api/v1/series?key=" + encodeURIComponent(key) + "&from=" + date + "&bucket=" + SLOT_MIN + "m";
    fetch(url, { cache: "no-store" })
      .then(function (res) {
        if (!res.ok) return res.text().then(function (t) { throw new Error(t || ("status " + res.status)); });
        return res.json();
      })
      .then(function (data) {
        if (!open || open.key !== key || open.date !== date) return; // closed or replaced meanwhile
        render(panel, summary, date, data.points || [], key.indexOf("::") > 0 && !/::Pods$/.test(key));
      })
      .catch(function (err) {
        summary.textContent = "Could not load this day: " + err.message;
      });
  }

  function render(panel, summary, date, points, isEndpoint) {
    var slots = (24 * 60) / SLOT_MIN;
    var start = Date.parse(date + "T00:00:00Z");
    var cells = new Array(slots);
    var worst = "unknown", observed = 0, downtime = 0, latSum = 0, latN = 0, latMax = 0;
    points.forEach(function (p) {
      var i = Math.floor((Date.parse(p.time) - start) / (SLOT_MIN * 60000));
      if (i < 0 || i >= slots) return;
      cells[i] = p;
      if (RANK[p.status] > RANK[worst]) worst = p.status;
      observed += p.observed_seconds || 0;
      downtime += p.downtime_seconds || 0;
      if (p.latency_ms != null) { latSum += p.latency_ms; latN++; if (p.latency_ms > latMax) latMax = p.latency_ms; }
    });

    if (!points.length) {
      summary.textContent = "No samples recorded on this day.";
      return;
    }
    summary.innerHTML = "";
    var b = el("b", null, LABEL[worst] || worst);
    summary.appendChild(b);
    var parts = [];
    if (downtime > 0) parts.push(dur(downtime) + " degraded or down");
    if (observed > 0) parts.push((100 * (1 - downtime / observed)).toFixed(2) + "% uptime");
    parts.push(dur(observed) + " observed");
    summary.appendChild(document.createTextNode(" · " + parts.join(" · ")));

    var bar = el("div", "bar");
    bar.setAttribute("role", "img");
    bar.setAttribute("aria-label", "Breakdown of " + niceDate(date) + " in " + SLOT_MIN + " minute steps");
    for (var i = 0; i < slots; i++) {
      var p = cells[i];
      var status = p ? p.status : "unknown";
      var c = el("i", "bar__day bar__day--" + status);
      var from = new Date(start + i * SLOT_MIN * 60000), to = new Date(start + (i + 1) * SLOT_MIN * 60000);
      var title = hhmm(from) + "–" + hhmm(to) + " UTC: " + (LABEL[status] || status);
      if (p && p.downtime_seconds > 0) title += ", " + dur(p.downtime_seconds) + " down";
      if (p && p.latency_ms != null) title += ", " + ms(p.latency_ms);
      c.title = title;
      bar.appendChild(c);
    }
    panel.appendChild(bar);
    var legend = el("div", "legend");
    legend.appendChild(el("span", null, "00:00 UTC"));
    legend.appendChild(el("span", null, "12:00"));
    legend.appendChild(el("span", null, "24:00"));
    panel.appendChild(legend);
    if (isEndpoint && latN > 0) {
      panel.appendChild(el("p", "daypanel__latency", "Latency: average " + ms(latSum / latN) + ", peak " + ms(latMax)));
    }
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
    if (!cell) return;
    var bar = cell.closest(".bar[data-key]");
    if (!bar) return;
    var key = bar.getAttribute("data-key"), date = cell.getAttribute("data-date");
    if (open && open.key === key && open.date === date) { closePanel(); return; }
    openPanel(cell, key, date, false);
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape" && open) closePanel();
  });

  setInterval(tick, 1000);
  setInterval(refresh, POLL_MS);
  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) refresh();
  });
})();
