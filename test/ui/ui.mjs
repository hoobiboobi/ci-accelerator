// Browser checks of the console's pages: what a tester would do by hand.
//
//   node test/ui/ui.mjs --base http://localhost:8080 [--chrome PATH]
//        [--live /pipelines/<instance>/<id>] [--cookie NAME=VALUE]
//
// It starts a headless Chrome of its own and talks to it over the DevTools
// protocol (Node >= 22: WebSocket is built in; no packages). It visits every
// page it can reach at a desktop and a phone width, clicks through the
// interactive parts, and fails on script errors, content security policy
// reports, failed requests, broken text and pages wider than the screen.
// --live names a pipeline page whose pipeline changes while the checks run
// (the Go test announces it): what the reader unfolded must survive it.
// --cookie is a session cookie of a console with sign-in: the pages are
// checked as that person, then the sign-in page, then signing out.
// --shots DIR saves a full-page screenshot of the first two pages of each
// kind (ids aside) at both widths, with an index.html to look through.
// Exit status 1 when a check fails; each check prints one line.
"use strict";
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const args = Object.fromEntries(process.argv.slice(2).reduce((acc, a, i, all) => {
  if (a.startsWith("--")) acc.push([a.slice(2), all[i + 1] && !all[i + 1].startsWith("--") ? all[i + 1] : "true"]);
  return acc;
}, []));
const base = (args.base || "").replace(/\/$/, "");
if (!base) { console.error("usage: ui.mjs --base URL [--chrome PATH] [--live PATH] [--cookie NAME=VALUE]"); process.exit(2); }
if (typeof WebSocket === "undefined") { console.error("Node >= 22 needed (built-in WebSocket)"); process.exit(2); }
const sleep = ms => new Promise(r => setTimeout(r, ms));

// ---- results ----
let failed = 0, passed = 0;
function check(name, ok, detail = "") {
  if (ok) passed++; else failed++;
  console.log(`${ok ? "ok  " : "FAIL"} ${name}${detail ? " — " + detail : ""}`);
}

// ---- Chrome ----
function findChrome() {
  if (args.chrome) return args.chrome;
  if (process.env.CHROME) return process.env.CHROME;
  const mac = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
  if (fs.existsSync(mac)) return mac;
  for (const name of ["google-chrome", "google-chrome-stable", "chromium", "chromium-browser"]) {
    for (const dir of (process.env.PATH || "").split(path.delimiter)) {
      const p = path.join(dir, name);
      if (fs.existsSync(p)) return p;
    }
  }
  throw new Error("no Chrome found: pass --chrome or set CHROME");
}

async function launch() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "ci-accelerator-ui-"));
  const flags = ["--headless=new", "--remote-debugging-port=0", `--user-data-dir=${dir}`, "--no-first-run",
    "--no-default-browser-check", "--disable-background-timer-throttling", "--disable-renderer-backgrounding",
    "--disable-backgrounding-occluded-windows", "--lang=en-GB", "about:blank"];
  // CI runners restrict the user namespaces Chrome's sandbox needs; the
  // browser only visits the console under test.
  if (process.env.CI) flags.unshift("--no-sandbox");
  const proc = spawn(findChrome(), flags, { stdio: ["ignore", "ignore", "pipe"] });
  const port = await new Promise((resolve, reject) => {
    let buf = "";
    // Two checks start their Chromes at once; a busy CI runner has needed
    // more than 20 s. A Chrome that never answers is killed, not orphaned.
    const t = setTimeout(() => { proc.kill("SIGKILL"); reject(new Error("Chrome did not start in 60 s: " + buf.slice(-400))); }, 60000);
    proc.stderr.on("data", d => {
      buf += d;
      const m = buf.match(/DevTools listening on ws:\/\/[^:]+:(\d+)\//);
      if (m) { clearTimeout(t); resolve(+m[1]); }
    });
    proc.on("exit", c => reject(new Error("Chrome exited " + c + ": " + buf.slice(-400))));
  });
  const version = (await (await fetch(`http://127.0.0.1:${port}/json/version`)).json()).Browser;
  return { proc, port, dir, version };
}

// ---- a page over the DevTools protocol ----
async function page(port, { width = 1400, height = 900 } = {}) {
  const t = await (await fetch(`http://127.0.0.1:${port}/json/new?about:blank`, { method: "PUT" })).json();
  const ws = new WebSocket(t.webSocketDebuggerUrl);
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
  let id = 0;
  const pending = new Map(), handlers = [];
  const s = { problems: [], status: new Map(), main: null };
  ws.onmessage = m => {
    const d = JSON.parse(m.data);
    if (d.id && pending.has(d.id)) { pending.get(d.id)(d); pending.delete(d.id); return; }
    handlers.forEach(h => h(d));
    const p = d.params || {};
    switch (d.method) {
      case "Runtime.exceptionThrown":
        s.problems.push("exception: " + ((p.exceptionDetails.exception || {}).description || p.exceptionDetails.text));
        break;
      case "Runtime.consoleAPICalled":
        if (p.type === "error" || p.type === "warning") s.problems.push("console." + p.type + ": " + p.args.map(a => a.value ?? a.description).join(" "));
        break;
      case "Log.entryAdded":
        // Expected 404s are checked by status, not reported twice.
        if (p.entry.level === "error" && !/status of 404/.test(p.entry.text)) s.problems.push(p.entry.source + ": " + p.entry.text);
        break;
      case "Network.responseReceived":
        if (p.type === "Document" && p.frameId) s.main = p.response.status;
        if (p.response.status >= 400 && p.type !== "Document") s.problems.push("http " + p.response.status + " " + p.response.url);
        break;
      case "Network.loadingFailed":
        if (!p.canceled) s.problems.push("failed request: " + p.errorText);
        break;
    }
  };
  s.send = (method, params = {}) => new Promise((resolve, reject) => {
    const i = ++id;
    const timer = setTimeout(() => { pending.delete(i); reject(new Error("timeout: " + method)); }, 15000);
    pending.set(i, d => { clearTimeout(timer); d.error ? reject(new Error(method + ": " + d.error.message)) : resolve(d.result); });
    ws.send(JSON.stringify({ id: i, method, params }));
  });
  for (const domain of ["Page", "Runtime", "Log", "Network"]) await s.send(domain + ".enable");
  s.size = (w, h) => s.send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile: w < 600 });
  await s.size(width, height);
  // Loads a page; resolves with the time to its load event (or Infinity).
  s.go = async (url, settle = 300) => {
    s.main = null;
    const t0 = Date.now();
    const loaded = new Promise(r => { const h = d => { if (d.method === "Page.loadEventFired") { handlers.splice(handlers.indexOf(h), 1); r(true); } }; handlers.push(h); });
    let ok = true;
    try {
      await s.send("Page.navigate", { url: url.startsWith("http") ? url : base + url });
    } catch (e) {
      ok = false;
    }
    ok = ok && await Promise.race([loaded, sleep(8000).then(() => false)]);
    const ms = ok ? Date.now() - t0 : Infinity;
    await sleep(settle);
    return ms;
  };
  s.eval = async expr => {
    const r = await s.send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
    if (r.exceptionDetails) throw new Error("eval: " + ((r.exceptionDetails.exception || {}).description || r.exceptionDetails.text));
    return r.result.value;
  };
  s.at = sel => s.eval(`(() => { const e = document.querySelector(${JSON.stringify(sel)}); if (!e) return null;
    e.scrollIntoView({ block: "center", inline: "center" }); const b = e.getBoundingClientRect();
    return b.width || b.height ? [b.left + Math.min(b.width / 2, 120), b.top + b.height / 2] : null; })()`);
  s.click = async (sel, wait = 500) => {
    const p = await s.at(sel);
    if (!p) return false;
    for (const type of ["mouseMoved", "mousePressed", "mouseReleased"])
      await s.send("Input.dispatchMouseEvent", { type, x: p[0], y: p[1], button: "left", clickCount: 1 });
    await sleep(wait);
    return true;
  };
  s.hover = async sel => {
    const p = await s.at(sel);
    if (!p) return false;
    await s.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: p[0], y: p[1] });
    await sleep(300);
    return true;
  };
  s.key = async (key, { meta = false, ctrl = false, wait = 300 } = {}) => {
    const modifiers = (meta ? 4 : 0) | (ctrl ? 2 : 0);
    const vk = { Escape: 27, Enter: 13, ArrowDown: 40, ArrowUp: 38 }[key] || key.toUpperCase().charCodeAt(0);
    const text = key.length === 1 && !meta && !ctrl ? key : undefined;
    const code = key.length === 1 ? "Key" + key.toUpperCase() : key;
    await s.send("Input.dispatchKeyEvent", { type: text ? "keyDown" : "rawKeyDown", key, code, windowsVirtualKeyCode: vk, modifiers, text });
    await s.send("Input.dispatchKeyEvent", { type: "keyUp", key, code, windowsVirtualKeyCode: vk, modifiers });
    await sleep(wait);
  };
  s.type = async text => { await s.send("Input.insertText", { text }); await sleep(200); };
  s.visible = sel => s.eval(`(() => { const e = document.querySelector(${JSON.stringify(sel)});
    if (!e || e.closest("[hidden]")) return false; const cs = getComputedStyle(e);
    return e.getClientRects().length > 0 && cs.visibility !== "hidden" && cs.display !== "none"; })()`);
  s.take = () => { const p = s.problems; s.problems = []; return p; };
  s.close = async () => { await s.send("Page.close").catch(() => {}); ws.close(); };
  return s;
}

// What a tester flags by eye on a rendered page.
const audit = `(() => {
  const out = [];
  const se = document.scrollingElement;
  if (se.scrollWidth > se.clientWidth + 1) out.push("wider than the screen: " + se.scrollWidth + " > " + se.clientWidth);
  const text = document.body.innerText;
  for (const re of [/\\bNaN\\b/, /\\bundefined\\b/, /<no value>/, /%!/, /0001-01-01/, /\\bInfinity\\b/, /\\[object /, /ZgotmplZ/,
    /(^|\\s)-\\d+(\\.\\d+)? ?(ms|s|min|h)\\b/]) {
    const m = text.match(re);
    if (m) out.push("text " + re + ": …" + text.slice(Math.max(0, m.index - 40), m.index + 30).replace(/\\s+/g, " ") + "…");
  }
  if (document.documentElement.innerHTML.includes("ZgotmplZ")) out.push("ZgotmplZ in markup");
  document.querySelectorAll("[style]").forEach(e => { if (/NaN|undefined|Infinity/.test(e.getAttribute("style"))) out.push("style " + e.getAttribute("style")); });
  document.querySelectorAll("a[href]").forEach(a => { const h = a.getAttribute("href"); if (!h || /undefined|NaN/.test(h)) out.push("link " + JSON.stringify(h)); });
  const ids = {};
  document.querySelectorAll("[id]").forEach(e => { ids[e.id] = (ids[e.id] || 0) + 1; });
  Object.entries(ids).filter(([, n]) => n > 1).forEach(([k, n]) => out.push("id " + k + " x" + n));
  document.querySelectorAll("[aria-labelledby],[aria-controls],[aria-describedby]").forEach(e => {
    for (const at of ["aria-labelledby", "aria-controls", "aria-describedby"])
      (e.getAttribute(at) || "").split(/\\s+/).filter(Boolean).forEach(id => { if (!document.getElementById(id)) out.push(at + " -> missing #" + id); });
  });
  return out;
})()`;

// ---- screenshots (--shots) ----
const shotsTaken = [], shotKinds = new Map();
async function shot(s, width, p) {
  const kind = width + " " + p.split("?")[0].replace(/\d+/g, "N");
  const n = (shotKinds.get(kind) || 0) + 1;
  shotKinds.set(kind, n);
  if (n > 2) return;
  const r = await s.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: true });
  const name = `${width}${p.replace(/[^A-Za-z0-9]+/g, "-").replace(/-$/, "") || "-home"}.png`;
  fs.mkdirSync(args.shots, { recursive: true });
  fs.writeFileSync(path.join(args.shots, name), Buffer.from(r.data, "base64"));
  shotsTaken.push({ width, page: p, name });
}
function shotIndex() {
  if (!args.shots || !shotsTaken.length) return;
  const esc = t => t.replace(/[&<>"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
  const rows = shotsTaken.map(x => `<figure><figcaption>${x.width}px ${esc(x.page)}</figcaption><img src="${esc(x.name)}" loading="lazy" style="max-width:${x.width}px;width:100%;border:1px solid #ccc"></figure>`);
  fs.writeFileSync(path.join(args.shots, "index.html"), `<!doctype html><meta charset=utf-8><title>Console screenshots</title><body style="font:14px system-ui;margin:16px">${rows.join("\n")}`);
  console.log(`# ${shotsTaken.length} screenshots in ${args.shots}/index.html`);
}

// ---- the checks ----
async function crawl(s, width) {
  await s.size(width, width < 600 ? 844 : 900);
  const seen = new Set(["/"]), queue = ["/"], pages = [];
  let bad = 0, slow = 0, stuck = 0;
  while (queue.length && pages.length < 400) {
    const p = queue.shift();
    const ms = await s.go(p, 150);
    pages.push(p);
    stuck = ms === Infinity ? stuck + 1 : 0;
    if (stuck === 3) {
      check(`${width}px: pages keep loading`, false,
        `${pages.length - 3} loaded, then three in a row never did (no free connection? live streams of left pages)`);
      return pages;
    }
    if (ms === Infinity) continue;
    const found = (await s.eval(audit)).concat(s.take());
    if (s.main !== 200) found.push("status " + s.main);
    if (ms > 3000) { slow++; found.push("loaded in " + (ms === Infinity ? "never" : ms + " ms")); }
    if (found.length) { bad++; check(`${width}px ${p}`, false, found.join(" | ").slice(0, 400)); }
    if (args.shots) await shot(s, width, p);
    for (const h of await s.eval(`[...document.querySelectorAll("a[href]")].map(a => a.getAttribute("href"))`)) {
      if (!h.startsWith("/") || h.startsWith("//") || h.startsWith("/static/") || h.startsWith("/api/")) continue;
      const q = h.split("#")[0];
      if (!seen.has(q)) { seen.add(q); queue.push(q); }
    }
  }
  check(`${width}px: every page renders cleanly`, bad === 0, `${pages.length} pages, ${bad} with problems, ${slow} slow`);
  return pages;
}

// A browser opens six HTTP/1.1 connections per host; live pages kept one
// each, and pages left in the back/forward cache kept theirs.
async function navigationBurst(s, pages) {
  await s.size(1400, 900);
  const live = pages.filter(p => /^\/(builds|pipelines)\/[^/]+(\/\d+)?$/.test(p)).slice(0, 12);
  const times = [];
  for (const p of live) times.push(await s.go(p, 50));
  const worst = Math.max(...times);
  check("12 pages in a row load without waiting for a connection", worst < 3000, `slowest ${worst === Infinity ? "never loaded" : worst + " ms"}`);
  await s.go(live[0]);
  await s.go(live[1]);
  await s.eval("history.back()");
  await sleep(1200);
  const status = await s.eval(`(document.getElementById("live") || {}).textContent || ""`);
  check("back to a page from the back/forward cache: live again", /^(Live|Updated)/.test(status), JSON.stringify(status));
  s.take();
}

async function buildPage(s, pages) {
  const builds = pages.filter(p => /^\/builds\/\d+$/.test(p));
  let b = null;
  for (const p of builds) {
    await s.go(p);
    if (await s.eval(`[...document.querySelectorAll('[data-pane="steps"] [data-step]')].some(e => !e.closest("[hidden]"))`)) { b = p; break; }
  }
  if (!b) return check("a build with visible steps", false, "none found");
  for (const tab of await s.eval(`[...document.querySelectorAll("[data-tab]")].map(t => t.dataset.tab)`)) {
    await s.click(`[data-tab="${tab}"]`);
    const ok = await s.eval(`!document.querySelector('[data-pane="${tab}"]').hidden && location.hash === "#${tab}"`);
    check(`${b} tab ${tab}`, ok);
  }
  await s.click('[data-tab="steps"]');
  const step = await s.eval(`[...document.querySelectorAll('[data-pane="steps"] [data-step]')].find(e => !e.closest("[hidden]")).dataset.step`);
  const sel = `[data-pane="steps"] [data-step="${step}"]`;
  await s.hover(sel);
  check(`${b} hint on a step`, await s.visible("#pop"));
  await s.click(sel, 900);
  check(`${b} step panel opens`, (await s.visible("#drawer")) && (await s.eval(`document.querySelector("#drawer h2").textContent.trim().length > 0`)));
  if (await s.eval(`!!document.querySelector("#drawer [data-dstep]")`)) {
    await s.click("#drawer [data-dstep]", 900);
    check(`${b} panel moves to another step`, await s.visible("#drawer"));
  }
  await s.key("Escape");
  check(`${b} Esc closes the panel`, !(await s.visible("#drawer")));
  if (await s.eval(`!!document.querySelector('[data-act="toggleall"]')`)) {
    const rows = () => s.eval(`document.querySelectorAll("tr[data-of]:not([hidden])").length`);
    const a = await rows(); await s.key("e"); const c = await rows(); await s.key("e"); const d = await rows();
    check(`${b} e expands every stage and back`, c >= a && d === a, `${a} → ${c} → ${d}`);
  }
  check(`${b} no errors`, s.take().length === 0);
}

async function pipelinePage(s, pages) {
  let p = null;
  for (const x of pages.filter(x => /^\/pipelines\/[^/]+\/\d+$/.test(x))) {
    await s.go(x);
    if (await s.eval(`!!document.querySelector("[data-wf] .row.k-job")`)) { p = x; if (await s.eval(`!!document.querySelector("#cachedFold")`)) break; }
  }
  if (!p) return check("a pipeline with a timeline", false, "none found");
  await s.go(p);
  if (await s.eval(`!!document.querySelector("#cachedFold")`)) {
    await s.click("#cachedFold > summary");
    check(`${p} the cached images unfold`, await s.eval(`document.querySelector("#cachedFold").open && document.querySelectorAll("#cachedFold .brow").length > 0`));
  }
  const visibleRows = () => s.eval(`[...document.querySelectorAll("[data-wf] .row")].filter(r => !r.hidden).length`);
  // Several jobs: it opens on one row per job; one job: on its stages.
  const first = await visibleRows();
  const pressed = await s.eval(`document.querySelector('[data-wf-depth][aria-pressed="true"]').dataset.wfDepth`);
  for (const d of ["stages", "steps", "jobs"]) {
    if (await s.click(`[data-wf-depth="${d}"]`)) check(`${p} timeline depth ${d}`, (await visibleRows()) > 0);
  }
  await s.click(`[data-wf-depth="${pressed}"]`);
  check(`${p} the timeline opens as its depth button says (${pressed})`, (await visibleRows()) === first, `${first}`);
  await s.click("[data-wf-q]");
  await s.type("zzzqqq");
  check(`${p} timeline search without a match hides every row`, (await visibleRows()) === 0);
  await s.eval(`(() => { const i = document.querySelector("[data-wf-q]"); i.value = ""; i.dispatchEvent(new Event("input")); i.blur(); })()`);
  if (await s.click("[data-wf-crit]")) {
    // The toggle must change what one sees: rows off the path fade, the path does not.
    const fade = await s.eval(`(() => { const rows = [...document.querySelectorAll("[data-wf] .row.k-job")].filter(r => !r.hidden);
      const op = r => parseFloat(getComputedStyle(r).opacity); const on = rows.filter(r => r.classList.contains("oncrit")), off = rows.filter(r => !r.classList.contains("oncrit"));
      return { on: on.length, off: off.length, onFull: on.every(r => op(r) === 1), offFaded: off.every(r => op(r) < 1) }; })()`);
    check(`${p} critical path fades the jobs off the path`, fade.on > 0 && (fade.off === 0 || fade.offFaded) && fade.onFull, JSON.stringify(fade));
    await s.click("[data-wf-crit]");
  }
  await s.click('[data-wf-depth="steps"]');
  const row = await s.eval(`(() => { const r = [...document.querySelectorAll("[data-wf] .row[data-wstep]")].find(r => !r.hidden); return r && r.dataset.id; })()`);
  if (row) {
    await s.click(`[data-wf] .row[data-id="${row}"]`, 900);
    check(`${p} a timeline step opens the panel`, await s.visible("#drawer"));
    await s.key("Escape");
  }
  const href = await s.eval(`(() => { const a = document.querySelector('[data-wf] .row.k-job a.nm[href^="/builds/"]'); return a && a.getAttribute("href"); })()`);
  if (href) {
    await s.click(`[data-wf] .row.k-job a.nm[href="${href}"]`, 1200);
    check(`${p} a job's build opens from the timeline`, (await s.eval("location.pathname")) === href, href);
    if (await s.click('[data-tab="timeline"]')) {
      check(`${href} the build's steps on a timeline`, await s.eval(`[...document.querySelectorAll(".sgantt .srow2")].some(r => r.offsetParent)`));
    }
  }
  check(`${p} no errors`, s.take().length === 0);
}

// The pipelines: one card each with its jobs by stage; a card opens its
// pipeline.
async function pipelinesPage(s) {
  await s.go("/pipelines");
  const st = await s.eval(`({ cards: document.querySelectorAll(".pcard").length, jobs: document.querySelectorAll(".pg-tr a.j").length,
    href: (document.querySelector(".pcard .pc-head") || {}).getAttribute?.call(document.querySelector(".pcard .pc-head"), "href") })`);
  check("pipelines: cards with their jobs on a time axis", st.cards > 0 && st.jobs > 0, JSON.stringify(st));
  if (st.href) {
    await s.click(".pcard .pc-head", 1200);
    check("pipelines: a card opens its pipeline", (await s.eval("location.pathname")) === st.href, st.href);
  }
  check("pipelines: no errors", s.take().length === 0);
}

// The build list: a filter applies as it changes and lands in the address.
async function buildList(s) {
  await s.go("/builds");
  const all = await s.eval(`document.querySelectorAll(".brow").length`);
  await s.eval(`(() => { const sel = document.getElementById("fOutcome"); sel.value = "rebuilt"; sel.dispatchEvent(new Event("change")); })()`);
  await sleep(1200);
  const st = await s.eval(`({ url: location.search, rows: document.querySelectorAll(".brow").length, quiet: document.querySelectorAll(".brow.quiet").length })`);
  check("builds: a filter applies at once and is in the address", /outcome=rebuilt/.test(st.url) && st.rows > 0 && st.rows <= all && st.quiet === 0, JSON.stringify(st));
  await s.go("/builds?group=pipeline");
  check("builds: grouped by pipeline", await s.eval(`document.querySelectorAll("a.pgh").length > 0`));
  check("builds: no errors", s.take().length === 0);
}

// A job that built no image: its page reads the end of its log from
// GitLab when asked, as text.
async function jobPage(s, pages) {
  let p = null;
  for (const x of pages.filter(x => /^\/jobs\/[^/]+\/\d+$/.test(x))) {
    await s.go(x);
    if (await s.eval(`!!document.querySelector('[data-act="showlog"]')`)) { p = x; break; }
  }
  if (!p) return check("a job page offers its log", true, "none here (no GitLab token)");
  await s.click('[data-act="showlog"]', 300);
  let text = "";
  for (let i = 0; i < 30 && !text; i++) {
    await sleep(200);
    text = await s.eval(`(() => { const pre = document.querySelector("[data-log]"); return pre && !pre.hidden ? pre.textContent : ""; })()`);
  }
  check(`${p} shows the end of its log`, text.length > 0 && !/\x1b/.test(text), JSON.stringify(text.slice(0, 60)));
  check(`${p} log: secrets masked, markup shown as text`, !/glpat-/.test(text) && !(await s.eval(`!!document.querySelector("[data-log] script, [data-log] img")`)));
  check(`${p} no errors`, s.take().length === 0);
}

async function keys(s, pages) {
  const b = pages.find(p => /^\/builds\/\d+$/.test(p)) || "/";
  await s.go(b);
  for (const [name, opt] of [["Cmd+K", { meta: true }], ["Ctrl+K", { ctrl: true }]]) {
    await s.key("k", opt);
    const open = await s.visible("#pal");
    await s.type("a");
    const n = await s.eval(`document.querySelectorAll(".pal-o").length`);
    await s.key("Escape");
    check(`${name} opens search with results`, open && n > 0 && !(await s.visible("#pal")), `${n} results`);
  }
  await s.key("k", { ctrl: true });
  await s.type("build");
  await s.key("ArrowDown");
  await s.key("Enter", { wait: 1200 });
  check("search: Enter opens the chosen item", !(await s.visible("#pal")));
  await s.go(b);
  await s.key("?");
  check("? opens the shortcuts", await s.visible("#help"));
  await s.key("Escape");
  const th = await s.eval("document.documentElement.dataset.theme");
  await s.key("t");
  check("t switches the theme", (await s.eval("document.documentElement.dataset.theme")) !== th);
  await s.key("t");
  await s.key("[");
  check("[ collapses the sidebar", (await s.eval(`document.getElementById("app").dataset.collapsed`)) === "true");
  await s.key("[");
  await s.key("g", { wait: 100 });
  await s.key("n", { wait: 1200 });
  check("g n opens the build nodes", (await s.eval("location.pathname")) === "/nodes");
  check("keys: no errors", s.take().length === 0);
}

async function notFound(s) {
  for (const p of ["/nope", "/builds/999999999", "/pipelines/abcdef/1"]) {
    await s.go(p);
    const title = await s.eval("document.title");
    check(`${p} is the console's not-found page`, s.main === 404 && /^Not found/.test(title), `${s.main} ${title}`);
  }
  s.take();
}

// The page's pipeline changes while the reader has unfolded part of it:
// the update keeps it unfolded.
async function liveUpdate(s, p) {
  await s.go(p);
  const fold = await s.eval(`!!document.querySelector("#cachedFold")`);
  if (fold) await s.click("#cachedFold > summary");
  let status = "";
  for (let i = 0; i < 40 && !/^Updated/.test(status); i++) {
    await sleep(250);
    status = await s.eval(`(document.getElementById("live") || {}).textContent || ""`);
  }
  const open = fold ? await s.eval(`document.querySelector("#cachedFold").open`) : true;
  check("a live update keeps what the reader unfolded", /^Updated/.test(status) && open, `${status} fold ${fold} open ${open}`);
  check("live update: no errors", s.take().length === 0);
}

// Signed in: who it is shows on every page; the sign-in page passes the
// same checks as the others; signing out ends the session.
async function signedIn(s) {
  await s.go("/");
  check("signed in: the sidebar names the person and offers to sign out",
    await s.eval(`!!document.querySelector(".who small") && /^@/.test(document.querySelector(".who small").textContent) && !!document.querySelector("form.signout button")`));
  for (const w of [1400, 390]) {
    await s.size(w, w < 600 ? 844 : 900);
    await s.go("/auth/sign-in?e=denied&to=%2F");
    const issues = await s.eval(audit);
    check(`${w}px: the sign-in page`, s.main === 200 && issues.length === 0 && await s.visible(".signin-btn"), issues.join("; "));
  }
  await s.size(1400, 900);
  await s.go("/");
  await s.click("form.signout button", 1200);
  check("signing out lands on the sign-in page and says so", (await s.eval("location.pathname + location.search")) === "/auth/sign-in?e=out" &&
    /You are signed out/.test(await s.eval("document.body.innerText")));
  const before = s.take();
  const status = await s.eval(`fetch("/search-index").then(r => r.status)`);
  check("after signing out, data is refused", status === 401, String(status));
  // That refusal is the point; anything else is a problem.
  const left = before.concat(s.take().filter(p => !/search-index|status of 401/.test(p)));
  check("sign-in: no errors", left.length === 0, left.join("; "));
}

// ---- run ----
const chrome = await launch();
console.log(`# ${chrome.version} against ${base}`);
try {
  const s = await page(chrome.port);
  if (args.cookie) {
    const [name, ...v] = args.cookie.split("=");
    const ok = (await s.send("Network.setCookie", { name, value: v.join("="), url: base + "/", path: "/", secure: true, httpOnly: true, sameSite: "Lax" })).success;
    check("the session cookie is set", ok !== false);
  }
  const pages = await crawl(s, 1400);
  await crawl(s, 390);
  await navigationBurst(s, pages);
  await buildPage(s, pages);
  await pipelinePage(s, pages);
  await pipelinesPage(s);
  await buildList(s);
  await jobPage(s, pages);
  await keys(s, pages);
  await notFound(s);
  if (args.live) await liveUpdate(s, args.live);
  if (args.cookie) await signedIn(s);
  await s.close();
} catch (e) {
  check("the checks ran to the end", false, e.stack || String(e));
} finally {
  chrome.proc.kill();
  await sleep(300);
  fs.rmSync(chrome.dir, { recursive: true, force: true });
}
shotIndex();
console.log(`# ${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
