// ci-accelerator console: the conveniences on top of server-rendered pages.
// Every page works without it (every tab pane is in the page, every step
// and job has its own address). Nothing from build data is inserted as
// markup: hints clone templates the server rendered, panels and live
// updates insert HTML the server rendered, search results are built with
// text nodes.
"use strict";
(function () {
  const $ = (s, el) => (el || document).querySelector(s);
  const $$ = (s, el) => Array.from((el || document).querySelectorAll(s));
  const root = document.documentElement;
  const app = $("#app");
  const main = $("#main");
  const pop = $("#pop");
  const drawer = $("#drawer");
  const toastEl = $("#toast");
  const store = {
    get(k) { try { return localStorage.getItem("ci-accelerator-" + k); } catch (e) { return null; } },
    set(k, v) { try { localStorage.setItem("ci-accelerator-" + k, v); } catch (e) { /* storage blocked */ } },
    del(k) { try { localStorage.removeItem("ci-accelerator-" + k); } catch (e) { /* storage blocked */ } },
  };
  if (!main) return;

  let toastT = null;
  function toast(msg) {
    if (!toastEl) return;
    toastEl.textContent = msg;
    toastEl.hidden = false;
    clearTimeout(toastT);
    toastT = setTimeout(() => { toastEl.hidden = true; }, 1600);
  }

  // ---- theme, sidebar, project menu ----
  function toggleTheme() {
    const next = root.dataset.theme === "light" ? "dark" : "light";
    root.dataset.theme = next;
    // Back to the system's theme: follow the system again.
    // (the system's theme as theme.js picks it: light only when asked for)
    if (next === (matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark")) store.del("theme");
    else store.set("theme", next);
    toast(next === "dark" ? "Dark theme" : "Light theme");
  }
  function setCollapsed(on) {
    if (!app) return;
    app.dataset.collapsed = String(on);
    store.set("collapsed", String(on));
    const b = $("#collapseBtn");
    if (b) b.setAttribute("aria-expanded", String(!on));
  }
  if (root.dataset.collapsed === "true") setCollapsed(true);
  const themeBtn = $("#themeBtn"), mTheme = $("#mTheme");
  if (themeBtn) themeBtn.addEventListener("click", toggleTheme);
  if (mTheme) mTheme.addEventListener("click", toggleTheme);
  const collapseBtn = $("#collapseBtn");
  if (collapseBtn) collapseBtn.addEventListener("click", () => setCollapsed(app.dataset.collapsed !== "true"));
  const projBtn = $("#projBtn"), projMenu = $("#projMenu");
  if (projBtn && projMenu) {
    projBtn.addEventListener("click", () => {
      projMenu.hidden = !projMenu.hidden;
      projBtn.setAttribute("aria-expanded", String(!projMenu.hidden));
    });
  }

  // ---- copy buttons ----
  document.addEventListener("click", ev => {
    const c = ev.target.closest("[data-copy]");
    if (c) copy(c.dataset.copy);
  });
  function copy(text) {
    if (!text) return;
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(() => toast("Copied"), () => toast("Copy failed: select the text instead"));
    } else {
      toast("Copy is not available here");
    }
  }

  // ---- per-page setup (run again after a live update) ----
  function setup() {
    setupTabs();
    setupWaterfall();
    setupSteps();
    setupFolds();
    setupFilters();
    setupHowTo();
    setupLog();
  }

  // Tabs: the hash names the pane (#steps, #dockerfile, #timeline, ...).
  function showTab(name) {
    const tabs = $$("[data-tab]", main);
    if (!tabs.length) return;
    if (!tabs.some(t => t.dataset.tab === name)) name = tabs[0].dataset.tab;
    tabs.forEach(t => {
      t.setAttribute("aria-selected", String(t.dataset.tab === name));
      t.tabIndex = t.dataset.tab === name ? 0 : -1;
    });
    $$("[data-pane]", main).forEach(p => { p.hidden = p.dataset.pane !== name; });
    const dg = $(".dgrid", main);
    if (dg) dg.classList.toggle("wide", name !== "steps");
    refitWaterfalls();
  }
  function setupTabs() {
    $$("[data-tab]", main).forEach(t => t.addEventListener("click", ev => {
      ev.preventDefault();
      history.replaceState(null, "", "#" + t.dataset.tab);
      showTab(t.dataset.tab);
    }));
    showTab(location.hash.slice(1));
  }

  // Steps table: stage groups fold; runs of cached stages start folded.
  function fold(btn, open) {
    btn.setAttribute("aria-expanded", String(open));
    $$('tr[data-of="' + btn.dataset.blk + '"]', main).forEach(r => { r.hidden = !open; });
  }
  function setupFolds() {
    $$(".grpbtn[data-blk]", main).forEach(b => {
      if (b.dataset.fold === "true") fold(b, false);
      b.addEventListener("click", () => fold(b, b.getAttribute("aria-expanded") !== "true"));
    });
    const all = $('[data-act="toggleall"]', main);
    if (all) all.addEventListener("click", toggleAll);
  }
  // Expand every stage, or go back to the page's own folding (runs of
  // cached stages folded), which is what the button's label promises.
  function toggleAll() {
    const bs = $$(".grpbtn[data-blk]", main);
    const open = bs.some(b => b.getAttribute("aria-expanded") !== "true");
    bs.forEach(b => fold(b, open || b.dataset.fold !== "true"));
    const lbl = $('[data-act="toggleall"] span', main);
    if (lbl) lbl.textContent = open ? "Fold cached" : "Expand all";
  }

  // A job's log, read from GitLab when the reader asks, in parts of whole
  // lines: its end first, then earlier parts above it on request, and the
  // lines added since below it. The server says where each part sits
  // (X-Log-Start/End, bytes of the log as GitLab stores it).
  function setupLog() {
    const box = $(".logbox", main);
    if (!box) return;
    const b = $('[data-act="showlog"]', box), pre = $("[data-log]", box), note = $("[data-log-note]", box);
    const earlier = $('[data-act="logearlier"]', box), newer = $('[data-act="lognew"]', box);
    if (!b || !pre) return;
    let start = -1, end = -1;
    const say = t => { if (note) note.textContent = t; };
    async function part(query) {
      [b, earlier, newer].forEach(x => { if (x) x.disabled = true; });
      say("Reading from GitLab…");
      try {
        const res = await fetch(b.dataset.url + query, { cache: "no-store" });
        const text = await res.text();
        if (!res.ok) { say(text); return null; }
        const h = k => parseInt(res.headers.get(k) || "", 10);
        return { text, start: h("X-Log-Start"), end: h("X-Log-End"), gap: res.headers.get("X-Log-Gap") === "1" };
      } catch (e) {
        say("The console did not answer.");
        return null;
      } finally {
        [b, earlier, newer].forEach(x => { if (x) x.disabled = false; });
      }
    }
    function shown(extra) {
      const t = pre.textContent;
      const lines = t ? t.split("\n").length - (t.endsWith("\n") ? 1 : 0) : 0;
      const what = !lines ? "The log is empty so far" : start === 0 ? `The whole log, ${lines} lines` : `Its last ${lines} lines`;
      say(`${what}, as GitLab has them now; secrets masked.${extra ? " " + extra : ""}`);
      if (earlier) earlier.hidden = !(start > 0);
      if (newer) newer.hidden = false;
      pre.hidden = !lines;
    }
    b.addEventListener("click", async () => {
      const p = await part("");
      if (!p) return;
      pre.textContent = p.text;
      start = p.start;
      end = p.end;
      b.hidden = true;
      shown();
      pre.scrollTop = pre.scrollHeight;
    });
    if (earlier) earlier.addEventListener("click", async () => {
      if (!(start > 0)) return;
      const p = await part("?before=" + start);
      if (!p) return;
      // Above what is shown; what the reader was looking at stays put.
      const h0 = pre.scrollHeight;
      pre.textContent = p.text + pre.textContent;
      pre.scrollTop += pre.scrollHeight - h0;
      start = p.start;
      shown();
    });
    if (newer) newer.addEventListener("click", async () => {
      if (end < 0) return;
      const atEnd = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 4;
      const p = await part("?after=" + end);
      if (!p) return;
      if (p.gap) {
        // More was added than one part holds: its end, from where it starts.
        pre.textContent = p.text;
        start = p.start;
      } else {
        pre.textContent += p.text;
      }
      end = p.end;
      shown(p.gap ? "Many lines were added: the earlier ones are skipped." : p.text ? "" : "No new lines.");
      if (atEnd || p.gap) pre.scrollTop = pre.scrollHeight;
    });
  }

  // "From the GitLab job log" can be dismissed.
  function setupHowTo() {
    const how = $("#howto", main);
    if (!how) return;
    const x = $('[data-act="hidehow"]', how);
    if (x) x.addEventListener("click", () => { how.hidden = true; });
  }

  // ---- step hints and the step panel ----
  let popFor = null;
  let opened = null; // "build/step" whose panel is open
  function showPop(el) {
    const tpl = document.getElementById("pop-" + el.dataset.step);
    if (!tpl || !pop || matchMedia("(max-width: 760px)").matches) return;
    pop.replaceChildren(tpl.content.cloneNode(true));
    pop.hidden = false;
    const r = el.getBoundingClientRect();
    const w = pop.offsetWidth, h = pop.offsetHeight;
    const x = Math.min(Math.max(8, r.left + 80), window.innerWidth - w - 8);
    let y = r.bottom + 6;
    if (y + h > window.innerHeight - 8) y = Math.max(8, r.top - h - 6);
    pop.style.left = x + "px";
    pop.style.top = y + "px";
    popFor = el;
  }
  function hidePop() { if (pop) pop.hidden = true; popFor = null; }

  async function openPanel(build, step) {
    if (!drawer) return;
    hidePop();
    const res = await fetch("/builds/" + build + "/steps/" + step + "?part=panel");
    if (!res.ok) return;
    const doc = new DOMParser().parseFromString(await res.text(), "text/html");
    drawer.replaceChildren(...doc.body.childNodes);
    drawer.hidden = false;
    opened = build + "/" + step;
    markOpen();
    const h = $("h2", drawer);
    if (h) h.focus();
  }
  function markOpen() {
    $$("[data-step].open", main).forEach(o => o.classList.remove("open"));
    if (!opened) return;
    const ctx = $("#ctx", main);
    const [b, s] = opened.split("/");
    if (ctx && ctx.dataset.build === b) $$('[data-step="' + s + '"]', main).forEach(el => el.classList.add("open"));
  }
  function closePanel() {
    if (!drawer || drawer.hidden) return false;
    drawer.hidden = true;
    drawer.replaceChildren();
    const was = opened;
    opened = null;
    markOpen();
    if (was) {
      const el = $('[data-step="' + was.split("/")[1] + '"]', main);
      if (el && el.offsetParent !== null) el.focus();
    }
    return true;
  }
  if (drawer) {
    drawer.addEventListener("click", ev => {
      if (ev.target.closest("[data-close]")) { closePanel(); return; }
      const nav = ev.target.closest("[data-dstep], [data-hist]");
      if (nav && opened) {
        ev.preventDefault();
        if (nav.dataset.dstep !== undefined) {
          openPanel(opened.split("/")[0], nav.dataset.dstep);
        } else {
          const m = nav.getAttribute("href").match(/^\/builds\/(\d+)\/steps\/(\d+)/);
          if (m) openPanel(m[1], m[2]);
        }
        return;
      }
      const goto = ev.target.closest("[data-goto-tab]");
      const ctx = $("#ctx", main);
      if (goto && ctx && opened && opened.split("/")[0] === ctx.dataset.build) {
        ev.preventDefault();
        history.replaceState(null, "", "#" + goto.dataset.gotoTab);
        showTab(goto.dataset.gotoTab);
        closePanel();
      }
    });
  }

  function setupSteps() {
    const ctx = $("#ctx", main);
    const steps = $$("[data-step]", main);
    steps.forEach(el => {
      el.addEventListener("mouseenter", () => showPop(el));
      el.addEventListener("mouseleave", hidePop);
      el.addEventListener("focus", () => showPop(el));
      el.addEventListener("blur", hidePop);
      el.addEventListener("click", ev => {
        if (!ctx) return;
        ev.preventDefault();
        openPanel(ctx.dataset.build, el.dataset.step);
      });
      el.addEventListener("keydown", ev => {
        if (ev.key === "Enter" && ctx) { ev.preventDefault(); openPanel(ctx.dataset.build, el.dataset.step); }
        if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
          ev.preventDefault();
          const pane = el.closest("[data-pane]");
          const list = steps.filter(s => s.offsetParent !== null && s.closest("[data-pane]") === pane);
          const i = list.indexOf(el) + (ev.key === "ArrowDown" ? 1 : -1);
          if (list[i]) list[i].focus();
        }
      });
    });
    markOpen();
  }
  window.addEventListener("scroll", () => { if (popFor && popFor.isConnected) showPop(popFor); }, { passive: true });

  // ---- filters apply as they change; the Apply button is for no script ----
  // A search box applies on Enter only: "change" fires when it loses
  // focus, so clicking a result after typing would run the search instead.
  // The address keeps only the filters that are set (dates only for a
  // period of dates).
  function setupFilters() {
    $$("form[data-autosubmit]", main).forEach(f => {
      const btn = $('button[type="submit"]', f);
      if (btn) btn.hidden = true;
      $$('select, input:not([type="search"])', f).forEach(el => el.addEventListener("change", () => f.requestSubmit ? f.requestSubmit() : f.submit()));
      f.addEventListener("submit", ev => {
        ev.preventDefault();
        const period = f.elements.period ? f.elements.period.value : "";
        const q = new URLSearchParams();
        for (const [k, v] of new FormData(f)) {
          if (v === "" || ((k === "from" || k === "to") && period !== "custom")) continue;
          q.append(k, v);
        }
        location.assign((f.getAttribute("action") || location.pathname) + (q.toString() ? "?" + q : ""));
      });
    });
  }

  // ---- the waterfall ----
  // The names column's width (--tw) is the reader's: dragged, fitted to
  // the longest visible name, or set with the arrow keys, remembered in
  // this browser for every timeline. The bars keep at least wfBarsMin.
  const wfMinTW = 160, wfBarsMin = 460;
  function wfWidthLimit(wf) {
    const box = $(".wfbox", wf);
    if (!box || box.clientWidth === 0) return Infinity; // not laid out (a hidden tab)
    return Math.max(wfMinTW, box.clientWidth - wfBarsMin);
  }
  // Again once laid out: a timeline in a tab that was hidden.
  function refitWaterfalls() {
    $$("[data-wf]", main).forEach(wf => {
      const saved = parseInt(store.get("wf-tw") || "", 10);
      if (saved > 0) setTW(wf, saved, false); else fitLabels(wf);
    });
  }
  function setTW(wf, px, keep, noFit) {
    if (px === null) {
      wf.style.removeProperty("--tw");
      if (keep) store.del("wf-tw");
    } else {
      px = Math.round(Math.min(Math.max(px, wfMinTW), wfWidthLimit(wf)));
      wf.style.setProperty("--tw", px + "px");
      if (keep) store.set("wf-tw", String(px));
    }
    if (!noFit) fitLabels(wf);
  }
  function currentTW(wf) {
    const h = $(".wfax .h", wf);
    return h ? h.getBoundingClientRect().width : 250;
  }
  // The width that shows every visible name whole.
  function fittedTW(wf) {
    let need = 0;
    $$(".row:not([hidden]) .tc", wf).forEach(tc => {
      const nm = $(".nm", tc);
      if (!nm) return;
      need = Math.max(need, tc.getBoundingClientRect().width + (nm.scrollWidth - nm.clientWidth));
    });
    return Math.ceil(need) + 2;
  }
  function setupColumn(wf) {
    const grip = $(".wgrip", wf);
    const saved = parseInt(store.get("wf-tw") || "", 10);
    if (saved > 0) setTW(wf, saved, false);
    if (!grip) return;
    grip.addEventListener("pointerdown", ev => {
      if (ev.button !== 0) return;
      ev.preventDefault();
      const x0 = ev.clientX, w0 = currentTW(wf);
      grip.setPointerCapture(ev.pointerId);
      wf.classList.add("resizing");
      const move = e => setTW(wf, w0 + e.clientX - x0, false, true); // labels: once, on release
      const up = e => {
        grip.removeEventListener("pointermove", move);
        grip.removeEventListener("pointerup", up);
        grip.removeEventListener("pointercancel", up);
        wf.classList.remove("resizing");
        setTW(wf, w0 + e.clientX - x0, true);
      };
      grip.addEventListener("pointermove", move);
      grip.addEventListener("pointerup", up);
      grip.addEventListener("pointercancel", up);
    });
    // Fit the longest name; at that width already, back to the default.
    grip.addEventListener("dblclick", () => {
      const fit = Math.min(Math.max(fittedTW(wf), wfMinTW), wfWidthLimit(wf));
      if (wf.style.getPropertyValue("--tw") && Math.abs(currentTW(wf) - fit) < 2) setTW(wf, null, true);
      else setTW(wf, fit, true);
    });
    grip.addEventListener("keydown", ev => {
      const step = ev.shiftKey ? 64 : 16;
      if (ev.key === "ArrowRight") { ev.preventDefault(); setTW(wf, currentTW(wf) + step, true); }
      if (ev.key === "ArrowLeft") { ev.preventDefault(); setTW(wf, currentTW(wf) - step, true); }
      if (ev.key === "Home") { ev.preventDefault(); setTW(wf, null, true); }
      if (ev.key === "Enter") { ev.preventDefault(); grip.dispatchEvent(new MouseEvent("dblclick")); }
    });
  }
  // A label that would run off the track's right edge is put before its
  // bars instead (or over them at the right edge when they start early),
  // as the server does for the ones it can tell; widths are only known
  // here, and change with the column and the window.
  function fitLabels(wf) {
    $$(".row:not([hidden]) .trk", wf).forEach(trk => {
      const lab = $(".lab", trk);
      if (!lab) return;
      if (lab.dataset.l === undefined) {
        if (lab.classList.contains("end")) return; // the server placed it
        lab.dataset.l = lab.style.left;
      }
      lab.classList.remove("end", "over");
      lab.style.right = "";
      lab.style.left = lab.dataset.l;
      const t = trk.getBoundingClientRect(), r = lab.getBoundingClientRect();
      if (r.right <= t.right || t.width === 0) return;
      let lo = 100;
      $$(".bar", trk).forEach(b => { lo = Math.min(lo, parseFloat(b.style.left) || 0); });
      lab.style.left = "";
      lab.style.right = (lo < 40 ? 0 : 100 - lo + 0.6).toFixed(2) + "%";
      lab.classList.add("end", "over");
    });
  }
  let wfResizeT = null;
  window.addEventListener("resize", () => {
    clearTimeout(wfResizeT);
    wfResizeT = setTimeout(refitWaterfalls, 120);
  }, { passive: true });
  // What the reader set in each waterfall survives live updates of the page.
  const wfState = [];
  function setupWaterfall() {
    $$("[data-wf]", main).forEach((wf, wi) => {
      setupColumn(wf);
      const saved = wfState[wi] || (wfState[wi] = {});
      const rows = $$(".row", wf);
      const byId = new Map(rows.map(r => [r.dataset.id, r]));
      let filter = saved.filter || "all", q = saved.q || "";
      function remember() {
        saved.filter = filter;
        saved.q = q;
        saved.open = {};
        rows.forEach(r => { if (r.hasAttribute("aria-expanded")) saved.open[r.dataset.id] = r.getAttribute("aria-expanded"); });
      }
      const isOpen = r => r.getAttribute("aria-expanded") !== "false";
      const isStep = r => r.classList.contains("k-step") || r.classList.contains("k-group");
      function shown(r) {
        for (let p = byId.get(r.dataset.parent); p; p = byId.get(p.dataset.parent)) if (!isOpen(p)) return false;
        return true;
      }
      function render() {
        if (filter === "all" && !q) {
          rows.forEach(r => { r.hidden = !shown(r); });
          fitLabels(wf);
          return;
        }
        const keep = new Set();
        rows.forEach(r => {
          const byState = filter === "all" || (isStep(r) && r.dataset.state === filter);
          const byText = !q || r.dataset.text.includes(q);
          if (byState && byText && (isStep(r) || q)) {
            keep.add(r.dataset.id);
            for (let p = byId.get(r.dataset.parent); p; p = byId.get(p.dataset.parent)) keep.add(p.dataset.id);
          }
        });
        rows.forEach(r => { r.hidden = !keep.has(r.dataset.id); });
        fitLabels(wf);
      }
      function toggle(r, open) {
        if (!r.hasAttribute("aria-expanded")) return;
        r.setAttribute("aria-expanded", String(open === undefined ? !isOpen(r) : open));
        remember();
        render();
      }
      rows.forEach(r => {
        const chev = $(".wchev", r);
        if (chev) chev.addEventListener("click", ev => { ev.stopPropagation(); toggle(r); });
        r.addEventListener("click", ev => {
          if (ev.target.closest("a")) return;
          if (r.dataset.wstep) {
            const [b, s] = r.dataset.wstep.split("/");
            openPanel(b, s);
          } else if (r.hasAttribute("aria-expanded")) {
            toggle(r);
          }
        });
        r.addEventListener("keydown", ev => {
          const vis = rows.filter(x => !x.hidden);
          const i = vis.indexOf(r);
          if (ev.key === "ArrowDown" && vis[i + 1]) { ev.preventDefault(); vis[i + 1].focus(); }
          if (ev.key === "ArrowUp" && vis[i - 1]) { ev.preventDefault(); vis[i - 1].focus(); }
          if (ev.key === "ArrowRight") { ev.preventDefault(); toggle(r, true); }
          if (ev.key === "ArrowLeft") { ev.preventDefault(); toggle(r, false); }
          if (ev.key === "Enter") { ev.preventDefault(); r.click(); }
        });
      });
      if (rows[0]) rows[0].tabIndex = 0;
      $$("[data-wf-filter]", wf).forEach(b => b.addEventListener("click", () => {
        filter = b.dataset.wfFilter;
        $$("[data-wf-filter]", wf).forEach(x => x.setAttribute("aria-pressed", String(x === b)));
        remember();
        render();
      }));
      const input = $("[data-wf-q]", wf);
      if (input) input.addEventListener("input", () => { q = input.value.trim().toLowerCase(); saved.raw = input.value; remember(); render(); });
      const crit = $("[data-wf-crit]", wf);
      const setCrit = on => {
        crit.setAttribute("aria-pressed", String(on));
        wf.classList.toggle("crit-on", on);
        saved.crit = on;
      };
      if (crit) crit.addEventListener("click", () => setCrit(crit.getAttribute("aria-pressed") !== "true"));
      // Depth: jobs only, jobs with their stages (cached ones folded), or
      // every step.
      $$("[data-wf-depth]", wf).forEach(b => b.addEventListener("click", () => {
        const d = b.dataset.wfDepth;
        $$("[data-wf-depth]", wf).forEach(x => x.setAttribute("aria-pressed", String(x === b)));
        rows.forEach(r => {
          if (!r.hasAttribute("aria-expanded")) return;
          let open = true;
          if (r.classList.contains("k-job")) open = d !== "jobs";
          if (r.classList.contains("k-group")) open = d === "steps" || (d === "stages" && r.dataset.state !== "cached");
          r.setAttribute("aria-expanded", String(open));
        });
        saved.depth = d;
        remember();
        render();
      }));
      // Back to what the reader had before this update.
      if (saved.depth) $$("[data-wf-depth]", wf).forEach(x => x.setAttribute("aria-pressed", String(x.dataset.wfDepth === saved.depth)));
      if (saved.open) rows.forEach(r => { const o = saved.open[r.dataset.id]; if (o && r.hasAttribute("aria-expanded")) r.setAttribute("aria-expanded", o); });
      if (saved.filter) $$("[data-wf-filter]", wf).forEach(x => x.setAttribute("aria-pressed", String(x.dataset.wfFilter === filter)));
      if (input && saved.raw !== undefined) input.value = saved.raw;
      if (crit && saved.crit !== undefined) setCrit(saved.crit);
      render();
    });
  }


  // ---- dialogs: search and shortcuts ----
  const scrim = $("#scrim"), pal = $("#pal"), palQ = $("#palQ"), palL = $("#palL"), help = $("#help");
  let lastFocus = null, index = null, results = [], sel = 0;
  const commands = [
    { kind: "Command", label: "Toggle theme", sub: "light or dark · t", run: toggleTheme },
    { kind: "Command", label: "Keyboard shortcuts", sub: "?", run: () => openHelp() },
    { kind: "Command", label: "Collapse sidebar", sub: "[", run: () => setCollapsed(app.dataset.collapsed !== "true") },
  ];
  function fuzzy(q, s) {
    const t = s.toLowerCase();
    const sub = t.indexOf(q);
    if (sub >= 0) return { idx: Array.from({ length: q.length }, (_, k) => k + sub), score: 1000 - sub };
    const idx = [];
    let j = 0;
    for (let i = 0; i < t.length && j < q.length; i++) if (t[i] === q[j]) { idx.push(i); j++; }
    if (j < q.length) return null;
    return { idx, score: 500 - (idx[idx.length - 1] - idx[0]) };
  }
  function labelNode(label, idx) {
    const span = document.createElement("span");
    const set = new Set(idx);
    let buf = "";
    const flush = () => { if (buf) { span.append(buf); buf = ""; } };
    Array.from(label).forEach((c, i) => {
      if (set.has(i)) { flush(); const m = document.createElement("mark"); m.textContent = c; span.append(m); } else buf += c;
    });
    flush();
    return span;
  }
  function search() {
    const q = palQ.value.trim().toLowerCase();
    const items = (index || []).concat(commands);
    results = [];
    items.forEach(it => {
      if (!q) { if (it.kind !== "Job" || results.length < 10) results.push({ it, idx: [], score: 0 }); return; }
      const m = fuzzy(q, it.label);
      if (m) { results.push({ it, idx: m.idx, score: m.score }); return; }
      if (it.sub && fuzzy(q, it.sub)) results.push({ it, idx: [], score: 100 });
    });
    if (q) results.sort((a, b) => b.score - a.score);
    const order = ["Job", "Pipeline", "Node", "Page", "Command", "Search"];
    results = results.slice(0, 40).sort((a, b) => order.indexOf(a.it.kind) - order.indexOf(b.it.kind));
    // The index holds recent pipelines only: the pipelines page searches
    // them all.
    const typed = palQ.value.trim();
    if (typed) results.push({ it: { kind: "Search", label: "Search all pipelines for “" + typed + "”", url: "/pipelines?q=" + encodeURIComponent(typed) }, idx: [], score: 0 });
    sel = 0;
    palL.replaceChildren();
    if (!results.length) {
      const e = document.createElement("div");
      e.className = "pal-empty";
      e.textContent = "Nothing matches.";
      palL.append(e);
      return;
    }
    let group = "";
    results.forEach((r, i) => {
      if (r.it.kind !== group) {
        group = r.it.kind;
        const g = document.createElement("div");
        g.className = "pal-g";
        g.textContent = group === "Search" ? "Search" : group + "s";
        palL.append(g);
      }
      const o = document.createElement("div");
      o.className = "pal-o";
      o.id = "po-" + i;
      o.setAttribute("role", "option");
      o.dataset.i = String(i);
      const lb = document.createElement("span");
      lb.className = "lb3";
      lb.append(labelNode(r.it.label, r.idx));
      if (r.it.sub) { const sm = document.createElement("small"); sm.textContent = r.it.sub; lb.append(sm); }
      const ty = document.createElement("span");
      ty.className = "ty";
      ty.textContent = r.it.kind;
      o.append(lb, ty);
      o.addEventListener("mousemove", () => { if (sel !== i) { sel = i; mark(); } });
      o.addEventListener("click", () => run(i));
      palL.append(o);
    });
    mark();
  }
  function mark() {
    $$(".pal-o", palL).forEach(o => o.setAttribute("aria-selected", String(+o.dataset.i === sel)));
    palQ.setAttribute("aria-activedescendant", results.length ? "po-" + sel : "");
    const el = $("#po-" + sel);
    if (el) el.scrollIntoView({ block: "nearest" });
  }
  function run(i) {
    const r = results[i];
    if (!r) return;
    closePal(true);
    if (r.it.run) r.it.run(); else location.href = r.it.url;
  }
  async function openPal() {
    if (!pal) return;
    closeHelp(true);
    lastFocus = document.activeElement;
    pal.hidden = false;
    scrim.hidden = false;
    palQ.value = "";
    if (!index) {
      try {
        const res = await fetch("/search-index", { cache: "no-store" });
        index = res.ok ? (await res.json()).items || [] : [];
      } catch (e) { index = []; }
    }
    search();
    palQ.focus();
  }
  function closePal(keep) {
    if (!pal || pal.hidden) return false;
    pal.hidden = true;
    scrim.hidden = true;
    if (!keep && lastFocus && lastFocus.focus) lastFocus.focus({ preventScroll: true });
    return true;
  }
  function openHelp() {
    if (!help) return;
    closePal(true);
    lastFocus = document.activeElement;
    help.hidden = false;
    scrim.hidden = false;
    const x = $("[data-close-help]", help);
    if (x) x.focus();
  }
  function closeHelp(keep) {
    if (!help || help.hidden) return false;
    help.hidden = true;
    scrim.hidden = true;
    if (!keep && lastFocus && lastFocus.focus) lastFocus.focus({ preventScroll: true });
    return true;
  }
  if (palQ) {
    palQ.addEventListener("input", search);
    palQ.addEventListener("keydown", ev => {
      if (ev.key === "ArrowDown") { ev.preventDefault(); sel = Math.min(sel + 1, results.length - 1); mark(); }
      if (ev.key === "ArrowUp") { ev.preventDefault(); sel = Math.max(sel - 1, 0); mark(); }
      if (ev.key === "Enter") { ev.preventDefault(); run(sel); }
    });
  }
  if (scrim) scrim.addEventListener("click", () => { closePal(); closeHelp(); });
  const searchBtn = $("#searchBtn"), bSearch = $("#bSearch"), helpBtn = $("#helpBtn");
  if (searchBtn) searchBtn.addEventListener("click", openPal);
  if (bSearch) bSearch.addEventListener("click", openPal);
  if (helpBtn) helpBtn.addEventListener("click", openHelp);
  if (help) help.addEventListener("click", ev => { if (ev.target.closest("[data-close-help]")) closeHelp(); });
  const kbdK = $("#kbdK");
  if (kbdK && !/Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent)) kbdK.textContent = "Ctrl K";

  // ---- keyboard ----
  let gPressed = 0;
  document.addEventListener("keydown", ev => {
    const typing = ev.target.closest && ev.target.closest("input, textarea, select, [contenteditable]");
    if ((ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === "k") { ev.preventDefault(); openPal(); return; }
    if (ev.key === "Escape") {
      if (closePal() || closeHelp() || closePanel()) return;
      if (pop && !pop.hidden) { hidePop(); return; }
      if (typing) { ev.target.blur(); return; }
      const back = $("[data-back]", main);
      if (back) location.href = back.getAttribute("href");
      return;
    }
    if (typing || ev.metaKey || ev.ctrlKey || ev.altKey) return;
    if (gPressed && Date.now() - gPressed < 1200) {
      gPressed = 0;
      const to = { o: "/", p: "/pipelines", b: "/builds", i: "/images", n: "/nodes" }[ev.key];
      if (to) { location.href = to; return; }
    }
    switch (ev.key) {
      case "g": gPressed = Date.now(); return;
      case "/": ev.preventDefault(); openPal(); return;
      case "?": ev.preventDefault(); openHelp(); return;
      case "t": toggleTheme(); return;
      case "[": setCollapsed(app.dataset.collapsed !== "true"); return;
      case "e": if ($('[data-act="toggleall"]', main)) toggleAll(); return;
      case "c": { const ctx = $("#ctx", main); if (ctx && ctx.dataset.trace) copy(ctx.dataset.trace); return; }
    }
  });

  setup();

  // ---- live updates ----
  // The server says what changed; the page fetches itself again and swaps
  // its content, at most once a second.
  const live = main.dataset.live || "";
  const status = $("#live");
  if (!(live || main.dataset.liveNodes) || !window.EventSource) return;
  const params = new URLSearchParams();
  const notify = main.dataset.liveNotify === "true";
  if (live.startsWith("project:")) params.set("project", live.slice(8));
  else if (live && live !== "all") params.set("pipeline", live);
  if (main.dataset.liveNodes) params.set("nodes", "1");
  const filterQ = params.toString() ? "?" + params.toString() : "";
  let timer = null, busy = false;
  async function refresh() {
    timer = null;
    if (busy) { timer = setTimeout(refresh, 1000); return; }
    busy = true;
    try {
      const res = await fetch(location.pathname + location.search, { cache: "no-store" });
      if (!res.ok) return;
      const doc = new DOMParser().parseFromString(await res.text(), "text/html");
      const next = doc.getElementById("main");
      if (!next) return;
      const active = document.activeElement;
      const focused = active && main.contains(active) ? active.dataset.step : undefined;
      // Typing in the waterfall's search: keep the caret where it was.
      const typing = active && main.contains(active) && active.matches("[data-wf-q]") ? [active.selectionStart, active.selectionEnd] : null;
      const folds = new Map($$(".grpbtn[data-blk]", main).map(b => [b.dataset.blk, b.getAttribute("aria-expanded")]));
      const unfolded = $$("details[id][open]", main).map(d => d.id);
      hidePop();
      main.replaceChildren(...next.childNodes);
      setup();
      $$(".grpbtn[data-blk]", main).forEach(b => { if (folds.has(b.dataset.blk)) fold(b, folds.get(b.dataset.blk) === "true"); });
      unfolded.forEach(id => { const d = document.getElementById(id); if (d) d.open = true; });
      if (focused !== undefined) {
        const el = $('[data-step="' + focused + '"]', main);
        if (el) el.focus({ preventScroll: true });
      }
      if (typing) {
        const q = $("[data-wf-q]", main);
        if (q) { q.focus({ preventScroll: true }); q.setSelectionRange(typing[0], typing[1]); }
      }
      // In UTC, like every time the console shows.
      if (status) status.textContent = "Updated " + new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
    } finally {
      busy = false;
    }
  }
  // A browser opens at most six connections to a host over HTTP/1.1, and
  // every stream holds one. A hidden tab, or a page kept in the
  // back/forward cache, gives its stream up; otherwise a few open tabs or
  // a few clicks through the console leave no connection for the next page.
  let es = null;
  function connect() {
    if (es) return;
    es = new EventSource("/events" + filterQ);
    es.addEventListener("changed", () => {
      if (!notify) { if (!timer) timer = setTimeout(refresh, 1000); return; }
      // A page on a whole project only says something is new, at most
      // every 30 seconds: with hundreds of builds an hour it would
      // otherwise never hold still.
      if (!timer) timer = setTimeout(() => { timer = null; const nd = $("#newdata", main); if (nd) nd.hidden = false; }, 30000);
    });
    // The page arrives saying "Live". A stream that drops for a moment
    // (EventSource reconnects by itself, and leaving the page drops it too)
    // says nothing; only one gone for more than 3 seconds does.
    es.addEventListener("open", () => { clearTimeout(lost); lost = null; say("Live"); });
    es.addEventListener("error", () => { if (!lost) lost = setTimeout(() => say("Reconnecting…"), 3000); });
  }
  let lost = null;
  function say(text) { if (status) status.textContent = text; }
  function disconnect() {
    if (!es) return;
    es.close();
    es = null;
    clearTimeout(lost);
    lost = null;
  }
  // Back on the page: catch up on what changed meanwhile.
  function resume() {
    if (es) return;
    connect();
    if (!notify && !timer) timer = setTimeout(refresh, 0);
  }
  main.addEventListener("click", ev => {
    if (!ev.target.closest("#newdataBtn")) return;
    refresh();
  });
  document.addEventListener("visibilitychange", () => { if (document.hidden) disconnect(); else resume(); });
  window.addEventListener("pagehide", disconnect);
  window.addEventListener("pageshow", ev => { if (ev.persisted && !document.hidden) resume(); });
  if (!document.hidden) connect();
})();
