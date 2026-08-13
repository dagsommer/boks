// Three small things: the theme toggle, the menu on a narrow screen, and search.
//
// Search is a fetch of one static JSON file this site generated at build time, scored in
// the reader's browser. There is no search service, and nothing about a query leaves the
// page — the same reason the project ships no telemetry.

(function () {
  "use strict";

  // --- theme -------------------------------------------------------------

  var toggle = document.getElementById("theme-toggle");
  if (toggle) {
    var label = toggle.querySelector(".theme-toggle-label");
    var systemDark = window.matchMedia("(prefers-color-scheme: dark)");

    function current() {
      var t = document.documentElement.dataset.theme;
      if (t === "light" || t === "dark") return t;
      return systemDark.matches ? "dark" : "light";
    }
    function paint() {
      var next = current() === "dark" ? "light" : "dark";
      toggle.setAttribute("aria-label", "Switch to the " + next + " theme");
      if (label) label.textContent = current() === "dark" ? "Light" : "Dark";
    }
    toggle.addEventListener("click", function () {
      var next = current() === "dark" ? "light" : "dark";
      document.documentElement.dataset.theme = next;
      try { localStorage.setItem("boks-theme", next); } catch (e) {}
      paint();
    });
    systemDark.addEventListener("change", paint);
    paint();
  }

  // --- menu --------------------------------------------------------------

  var menu = document.getElementById("menu-toggle");
  var sidebar = document.getElementById("sidebar");
  if (menu && sidebar) {
    menu.addEventListener("click", function () {
      var open = sidebar.classList.toggle("open");
      menu.setAttribute("aria-expanded", open ? "true" : "false");
    });
    sidebar.addEventListener("click", function (e) {
      if (e.target.tagName === "A" && window.innerWidth <= 960) {
        sidebar.classList.remove("open");
        menu.setAttribute("aria-expanded", "false");
      }
    });
  }

  // --- where am I --------------------------------------------------------

  // The sidebar lists every section of the current document — windows.md alone has 69 of
  // them — so it marks the one being read rather than leaving the reader to count.
  var tocLinks = Array.prototype.slice.call(document.querySelectorAll(".toc a"));
  if (tocLinks.length && "IntersectionObserver" in window) {
    var byId = {};
    tocLinks.forEach(function (a) { byId[a.getAttribute("href").slice(1)] = a; });

    var seen = {};
    var observer = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) { seen[e.target.id] = e.isIntersecting; });
      var active = null;
      Object.keys(byId).forEach(function (id) {
        if (seen[id] && !active) active = byId[id];
      });
      tocLinks.forEach(function (a) { a.classList.remove("active"); });
      if (active) active.classList.add("active");
    }, { rootMargin: "-4rem 0px -70% 0px" });

    Object.keys(byId).forEach(function (id) {
      var el = document.getElementById(id);
      if (el) observer.observe(el);
    });
  }

  // --- search ------------------------------------------------------------

  var input = document.getElementById("search-input");
  var results = document.getElementById("search-results");
  var nav = document.getElementById("nav");
  if (!input || !results || !nav) return;

  var index = null;
  var loading = false;

  function load() {
    if (index || loading) return Promise.resolve();
    loading = true;
    return fetch(window.BOKS_SEARCH_INDEX)
      .then(function (r) { return r.json(); })
      .then(function (data) {
        index = data.map(function (e) {
          return {
            title: e.title,
            url: e.url,
            section: e.section,
            anchor: e.anchor,
            text: e.text,
            haystack: (e.title + " " + (e.section || "") + " " + e.text).toLowerCase(),
            head: (e.title + " " + (e.section || "")).toLowerCase(),
          };
        });
      })
      .catch(function () { index = []; })
      .then(function () { loading = false; });
  }

  function escape(s) {
    return s.replace(/[&<>"]/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
    });
  }

  function snippet(entry, terms) {
    var lower = entry.text.toLowerCase();
    var at = -1;
    for (var i = 0; i < terms.length && at < 0; i++) at = lower.indexOf(terms[i]);
    if (at < 0) at = 0;
    var start = Math.max(0, at - 60);
    var text = entry.text.slice(start, start + 190);
    if (start > 0) text = "…" + text;
    if (start + 190 < entry.text.length) text = text + "…";

    var html = escape(text);
    terms.forEach(function (t) {
      if (!t) return;
      var re = new RegExp("(" + t.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + ")", "gi");
      html = html.replace(re, "<mark>$1</mark>");
    });
    return html;
  }

  function score(entry, terms) {
    var total = 0;
    for (var i = 0; i < terms.length; i++) {
      var t = terms[i];
      if (entry.haystack.indexOf(t) < 0) return 0; // every term must appear
      var hits = entry.text.toLowerCase().split(t).length - 1;
      total += Math.min(hits, 8);
      if (entry.head.indexOf(t) >= 0) total += 12;
    }
    return total;
  }

  function render(query) {
    var terms = query.toLowerCase().split(/\s+/).filter(Boolean);
    if (!terms.length || !index) {
      results.hidden = true;
      results.innerHTML = "";
      nav.hidden = false;
      return;
    }

    var hits = [];
    for (var i = 0; i < index.length; i++) {
      var s = score(index[i], terms);
      if (s > 0) hits.push({ entry: index[i], score: s });
    }
    hits.sort(function (a, b) { return b.score - a.score; });
    hits = hits.slice(0, 12);

    nav.hidden = true;
    results.hidden = false;

    if (!hits.length) {
      results.innerHTML = '<p class="count">No section contains all of those words.</p>';
      return;
    }

    var html = '<p class="count">' + hits.length + " section" +
      (hits.length === 1 ? "" : "s") + "</p><ol>";
    hits.forEach(function (hit) {
      var e = hit.entry;
      var href = e.url + (e.anchor ? "#" + e.anchor : "");
      html += '<li><a href="' + escape(href) + '">' +
        '<span class="where">' + escape(e.title) + "</span>" +
        '<span class="what">' + escape(e.section || e.title) + "</span>" +
        '<span class="snippet">' + snippet(e, terms) + "</span>" +
        "</a></li>";
    });
    results.innerHTML = html + "</ol>";
  }

  var pending;
  input.addEventListener("input", function () {
    var query = input.value.trim();
    clearTimeout(pending);
    pending = setTimeout(function () {
      load().then(function () { render(query); });
    }, 80);
  });
  input.addEventListener("focus", load);
  input.addEventListener("keydown", function (e) {
    if (e.key === "Escape") {
      input.value = "";
      render("");
    }
  });

  document.addEventListener("keydown", function (e) {
    if (e.key === "/" && document.activeElement !== input &&
        !/^(INPUT|TEXTAREA)$/.test(document.activeElement.tagName)) {
      e.preventDefault();
      if (sidebar && window.innerWidth <= 960 && !sidebar.classList.contains("open")) {
        sidebar.classList.add("open");
        if (menu) menu.setAttribute("aria-expanded", "true");
      }
      input.focus();
    }
  });
})();
