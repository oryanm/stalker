// Small behaviours htmx does not cover. The CSP forbids inline handlers, so
// everything is delegated from the document.
(function () {
  "use strict";

  function close(box) {
    box.replaceChildren();
    delete box.dataset.open;
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
