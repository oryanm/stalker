// Small behaviours htmx does not cover. The CSP forbids inline handlers, so
// everything is delegated from the document.
(function () {
  "use strict";

  // the arrow reflects whether its row's box shows the posts
  function sync(box) {
    var open = box.dataset.open === "posts" ? "true" : "false";
    document.querySelectorAll('[aria-controls="' + box.id + '"]').forEach(function (t) {
      t.setAttribute("aria-expanded", open);
    });
  }

  function close(box) {
    box.replaceChildren();
    delete box.dataset.open;
    sync(box);
  }

  // a row's posts/edit links toggle its box: a second click closes it without a request
  document.addEventListener("htmx:beforeRequest", function (e) {
    var el = e.detail && e.detail.elt;
    if (!el || !el.hasAttribute || !el.hasAttribute("data-toggle")) {
      return;
    }
    var box = document.querySelector(el.getAttribute("hx-target"));
    if (!box) {
      return;
    }
    if (box.dataset.open === el.dataset.toggle && box.childElementCount > 0) {
      e.preventDefault();
      close(box);
      return;
    }
    box.dataset.open = el.dataset.toggle;
    sync(box);
  });

  // SSE and refresh re-render a row's summary, arrow included
  document.addEventListener("htmx:afterSwap", function (e) {
    var summary = e.detail && e.detail.target;
    var row = summary && summary.closest && summary.closest(".follow");
    var box = row && row.querySelector(".posts");
    if (box) {
      sync(box);
    }
  });

  document.addEventListener("click", function (e) {
    var el = e.target.closest && e.target.closest("[data-collapse], [data-reload]");
    if (!el) {
      return;
    }
    if (el.hasAttribute("data-reload")) {
      location.reload();
      return;
    }
    var box = el.closest(".posts");
    if (box) {
      e.preventDefault();
      close(box);
    }
  });
})();
