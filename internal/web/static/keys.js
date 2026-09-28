// Keyboard shortcuts; ? lists them. Loaded after app.js, whose close() it borrows.
(function () {
  "use strict";

  var close = window.stalker.close;

  // The cursor is real focus: a follow's title link (its arrow when it has no site) or a post in its open box,
  // so Enter opens it, Cmd+Enter opens it in the background, and focusin tunes the dial.

  // tags (, . or 1-9) and the importance knob ([ ]) are clicked, which keeps the latch and the boost
  function step(selector, by, wrap) {
    var items = Array.prototype.slice.call(document.querySelectorAll(selector));
    var i = items.findIndex(function (li) {
      return li.classList.contains("active");
    });
    var next = i < 0 ? (by > 0 ? 0 : items.length - 1) : i + by;
    if (wrap) {
      next = (next + items.length) % items.length;
    }
    return items[next];
  }

  function anchor(row) {
    return row.querySelector(".head a.title") || row.querySelector(".head .toggle");
  }

  function boxOf(row) {
    return row.querySelector(".posts");
  }

  // every place j and k stop: each follow, then the posts of its box when that is open
  function stops() {
    var out = [];
    document.querySelectorAll(".follows .follow").forEach(function (row) {
      out.push(anchor(row));
      var box = boxOf(row);
      if (box.dataset.open === "posts") {
        box.querySelectorAll(".recent .post a").forEach(function (a) {
          out.push(a);
        });
      }
    });
    return out;
  }

  function cursorRow() {
    var el = document.activeElement;
    return (el && el.closest && el.closest(".follows .follow")) || null;
  }

  // the stop the cursor is on; anything else focused in a follow (its edit key, a form) counts as the follow
  function cursor(list) {
    var el = document.activeElement;
    var i = list.indexOf(el);
    if (i >= 0) {
      return i;
    }
    var row = cursorRow();
    return row ? list.indexOf(anchor(row)) : -1;
  }

  function onScreen(el) {
    var r = el.getBoundingClientRect();
    return r.bottom > 0 && r.top < window.innerHeight;
  }

  function put(el) {
    if (!el) {
      return;
    }
    el.focus({ preventScroll: true });
    (el.closest(".post, .head") || el).scrollIntoView({ block: "nearest" });
  }

  // j/k and the arrows: one stop; J/K: one follow, skipping an open box. With no cursor yet, start on screen.
  function move(by, rows) {
    var list = rows
      ? Array.prototype.map.call(document.querySelectorAll(".follows .follow"), anchor)
      : stops();
    if (!list.length) {
      return;
    }
    var i = rows ? list.indexOf(cursorRow() && anchor(cursorRow())) : cursor(list);
    if (i < 0) {
      var visible = list.filter(onScreen);
      put(by > 0 ? visible[0] || list[0] : visible[visible.length - 1] || list[list.length - 1]);
      return;
    }
    put(list[Math.max(0, Math.min(list.length - 1, i + by))]);
  }

  // what to focus once a box the keyboard opened has loaded
  var pending = null;

  function load(row, trigger, open, focus) {
    var box = boxOf(row);
    if (box.dataset.open === open && box.childElementCount > 0) {
      put(box.querySelector(focus));
      return;
    }
    pending = { box: box, focus: focus };
    trigger.click();
  }

  document.addEventListener("htmx:afterSwap", function (e) {
    if (pending && e.detail && e.detail.target === pending.box) {
      put(pending.box.querySelector(pending.focus));
      pending = null;
    }
  });

  // SSE and refresh replace a row's summary, focused link and all: put the cursor back where it was
  var last = null;

  function keyOf(el) {
    var post = el.closest(".latest .post");
    if (post) {
      return ".latest .post:nth-child(" + (Array.prototype.indexOf.call(post.parentElement.children, post) + 1) + ") a";
    }
    return [".toggle", ".title", ".error", ".edit", ".refresh button"].find(function (sel) {
      return el.matches(sel);
    });
  }

  document.addEventListener("focusin", function (e) {
    var row = e.target.closest && e.target.closest(".follows .follow .summary") && e.target.closest(".follow");
    last = row ? { el: e.target, row: row.id, key: keyOf(e.target) } : null;
  });

  // a click on the page drops focus on purpose; a later swap must not bring it back
  document.addEventListener("pointerdown", function () {
    last = null;
  });

  document.addEventListener("htmx:afterSwap", function (e) {
    // SSE swaps carry no detail.target; they fire on the summary itself
    var summary = (e.detail && e.detail.target) || e.target;
    var row = summary && summary.closest && summary.closest(".follow");
    if (!last || last.el.isConnected || !row || row.id !== last.row) {
      return;
    }
    if (document.activeElement && document.activeElement !== document.body) {
      return;
    }
    var el = (last.key && row.querySelector(last.key)) || anchor(row);
    if (el) {
      el.focus({ preventScroll: true });
    }
  });

  function help() {
    var dialog = document.getElementById("keys");
    if (!dialog) {
      return;
    }
    if (dialog.open) {
      dialog.close();
    } else {
      dialog.showModal();
    }
  }

  document.addEventListener("click", function (e) {
    if (e.target.closest && e.target.closest("[data-keys]")) {
      help();
    }
  });

  function keydown(e) {
    var row = cursorRow();
    var box = row && boxOf(row);
    switch (e.key) {
      case "Escape":
        // leave the box (posts or the edit form) for its follow
        if (box && box.childElementCount > 0) {
          close(box);
          put(anchor(row));
          return true;
        }
        return false;
      case "j":
        move(1, false);
        return true;
      case "k":
        move(-1, false);
        return true;
      case "ArrowDown":
      case "ArrowUp":
        // they scroll pages without follows
        if (!document.querySelector(".follows .follow")) {
          return false;
        }
        move(e.key === "ArrowDown" ? 1 : -1, false);
        return true;
      case "J":
        move(1, true);
        return true;
      case "K":
        move(-1, true);
        return true;
      case "[":
      case "]":
        return latch(step(".tiers .tier", e.key === "]" ? 1 : -1, false));
      case ",":
      case ".":
        return latch(step(".tags .tag", e.key === "." ? 1 : -1, true));
      case "?":
        help();
        return true;
    }
    if (/^[1-9]$/.test(e.key)) {
      return latch(document.querySelectorAll(".tags .tag")[Number(e.key) - 1]);
    }
    if (!row) {
      return false;
    }
    var onPost = !!document.activeElement.closest(".posts");
    switch (e.key) {
      case "l":
      case "ArrowRight":
        if (!onPost) {
          load(row, row.querySelector(".head .toggle"), "posts", ".recent .post a");
        }
        return true;
      case "h":
      case "ArrowLeft":
        if (onPost) {
          put(anchor(row));
        } else if (box.childElementCount > 0) {
          close(box);
        }
        return true;
      case "o":
        var newest = row.querySelector(".latest .post a");
        if (newest) {
          newest.click();
        }
        return true;
      case "e":
        load(row, row.querySelector(".controls .edit"), "edit", "input");
        return true;
      case "r":
        var refresh = row.querySelector(".refresh button:not(:disabled)");
        if (refresh) {
          refresh.click();
        }
        return true;
    }
    return false;
  }

  function latch(item) {
    var link = item && !item.classList.contains("active") && item.querySelector("a");
    if (link) {
      link.click();
    }
    return !!item;
  }

  document.addEventListener("keydown", function (e) {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || e.isComposing) {
      return;
    }
    // the shortcut list is modal: only ? (and the dialog's own Escape) reach it
    var dialog = document.getElementById("keys");
    if (dialog && dialog.open) {
      if (e.key === "?") {
        e.preventDefault();
        help();
      }
      return;
    }
    // while typing, only Escape counts, and only to leave a follow's edit form
    var t = e.target;
    if (t.closest && t.closest("input, textarea, select, [contenteditable]") && (e.key !== "Escape" || !cursorRow())) {
      return;
    }
    if (keydown(e)) {
      e.preventDefault();
    }
  });
})();
