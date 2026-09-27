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

  // an icon that fails to load (gone, or served over http only) gives way to the title's initial
  function brokenIcon(img) {
    var initial = document.createElement("span");
    initial.className = "icon none";
    initial.setAttribute("aria-hidden", "true");
    initial.textContent = img.dataset.initial || "";
    img.replaceWith(initial);
  }

  document.addEventListener(
    "error",
    function (e) {
      if (e.target.matches && e.target.matches("img.icon")) {
        brokenIcon(e.target);
      }
    },
    true
  );

  // this deferred script may run after some icons already failed
  document.querySelectorAll("img.icon").forEach(function (img) {
    if (img.complete && img.naturalWidth === 0) {
      brokenIcon(img);
    }
  });

  // the tuning dial's needle follows the channel being pointed at or focused
  var tuned = null;

  function tune(row) {
    var dial = document.querySelector("[data-dial]");
    if (!dial || !row || row === tuned) {
      return;
    }
    if (tuned) {
      tuned.classList.remove("tuned");
    }
    tuned = row;
    row.classList.add("tuned");
    dial.querySelectorAll(".station.tuned").forEach(function (s) {
      s.classList.remove("tuned");
    });
    var tick = dial.querySelector('.station[data-follow="' + row.id.replace("follow-", "") + '"]');
    if (tick) {
      tick.classList.add("tuned");
      // CSSOM writes are not inline style attributes, so the CSP allows them
      dial.style.setProperty("--needle", String(parseFloat(tick.getAttribute("x1")) / 10));
    }
    var readout = dial.querySelector("[data-tuned]");
    var title = row.querySelector(".head .title");
    if (readout && title) {
      var name = document.createElement("span");
      name.textContent = title.textContent;
      name.dir = "auto";
      readout.replaceChildren(name);
      var age = row.querySelector(".head .age");
      if (age) {
        var when = document.createElement("span");
        when.className = "when";
        when.textContent = age.textContent;
        readout.appendChild(when);
      }
      readout.className = "readout " + (row.querySelector(".head").className.match(/age-\w/) || [""])[0];
    }
  }

  // on load the needle sweeps from the left end to the newest station
  function powerOn() {
    var dial = document.querySelector("[data-dial]");
    if (!dial || dial.dataset.on) {
      return;
    }
    dial.dataset.on = "1";
    tuned = null;
    dial.classList.add("sweep");
    requestAnimationFrame(function () {
      requestAnimationFrame(function () {
        tune(document.querySelector(".follows .follow"));
        setTimeout(function () {
          dial.classList.remove("sweep");
        }, 1300);
      });
    });
  }

  document.addEventListener("mouseover", function (e) {
    var row = e.target.closest && e.target.closest(".follows .follow");
    if (row) {
      tune(row);
    }
  });

  document.addEventListener("focusin", function (e) {
    var row = e.target.closest && e.target.closest(".follows .follow");
    if (row) {
      tune(row);
    }
  });

  // band keys and selector positions latch the moment they are pressed, before the page arrives
  document.addEventListener("click", function (e) {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) {
      return;
    }
    var link = e.target.closest && e.target.closest(".tag a, .tier a");
    if (!link) {
      return;
    }
    var item = link.parentElement;
    Array.prototype.forEach.call(item.parentElement.children, function (li) {
      li.classList.toggle("active", li === item);
    });
    var nav = item.closest(".tiers");
    if (nav) {
      nav.style.setProperty("--i", String(Array.prototype.indexOf.call(item.parentElement.children, item) + 1));
    }
  });

  // the "live" window lights while the event stream is connected; the root survives boosted swaps
  document.addEventListener("htmx:sseOpen", function () {
    document.documentElement.classList.add("on-air");
  });
  ["htmx:sseError", "htmx:sseClose"].forEach(function (name) {
    document.addEventListener(name, function () {
      document.documentElement.classList.remove("on-air");
    });
  });

  document.addEventListener("DOMContentLoaded", powerOn);
  document.addEventListener("htmx:afterSettle", powerOn);
  if (document.readyState !== "loading") {
    powerOn();
  }
})();
