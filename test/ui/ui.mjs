// Browser checks of the console's pages: what a tester would do by hand.
//
//   node test/ui/ui.mjs --base http://localhost:8080 [--chrome PATH]
//        [--live /pipelines/<instance>/<id>]
//
// It starts a headless Chrome of its own and talks to it over the DevTools
// protocol (Node >= 22: WebSocket is built in; no packages). It visits every
// page it can reach at a desktop and a phone width, clicks through the
// interactive parts, and fails on script errors, content security policy
// reports, failed requests, broken text and pages wider than the screen.
// --live names a pipeline page whose pipeline changes while the checks run
// (the Go test announces it): its waterfall search must survive the update.
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
if (!base) { console.error("usage: ui.mjs --base URL [--chrome PATH] [--live PATH]"); process.exit(2); }
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
    const t = setTimeout(() => reject(new Error("Chrome did not start: " + buf.slice(-400))), 20000);
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
  const pipes = pages.filter(p => /^\/pipelines\/[^/]+\/\d+$/.test(p));
  let p = null;
  for (const x of pipes) { await s.go(x); if (await s.eval(`!!document.querySelector("[data-wf] .row")`)) { p = x; break; } }
  if (!p) return check("a pipeline with a timeline", false, "none found");
  await s.click('[data-tab="timeline"]');
  const visibleRows = () => s.eval(`[...document.querySelectorAll("[data-wf] .row")].filter(r => !r.hidden).length`);
  for (const d of ["jobs", "stages", "steps"]) {
    if (await s.click(`[data-wf-depth="${d}"]`)) check(`${p} timeline depth ${d}`, (await visibleRows()) > 0);
  }
  await s.click("[data-wf-q]");
  await s.type("zzzqqq");
  check(`${p} timeline search without a match hides every row`, (await visibleRows()) === 0);
  await s.eval(`(() => { const i = document.querySelector("[data-wf-q]"); i.value = ""; i.dispatchEvent(new Event("input")); i.blur(); })()`);
  if (await s.click("[data-wf-crit]")) check(`${p} critical path`, await s.eval(`document.querySelector("[data-wf]").classList.contains("crit-on")`));
  const row = await s.eval(`(() => { const r = [...document.querySelectorAll("[data-wf] .row[data-wstep]")].find(r => !r.hidden); return r && r.dataset.id; })()`);
  if (row) {
    await s.click(`[data-wf] .row[data-id="${row}"]`, 900);
    check(`${p} a timeline step opens the panel`, await s.visible("#drawer"));
    await s.key("Escape");
  }
  if (await s.click('[data-tab="dockerfile"]') && await s.eval(`!!document.querySelector('[data-pane="dockerfile"] a.jc')`)) {
    const before = await s.eval("location.pathname");
    await s.click('[data-pane="dockerfile"] a.jc', 900);
    check(`${p} a Dockerfile chip opens the panel in place`, (await s.visible("#drawer")) && (await s.eval("location.pathname")) === before);
    await s.key("Escape");
  }
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

// The page's pipeline changes while the reader types in its waterfall.
async function liveUpdate(s, p) {
  await s.go(p + "#timeline");
  await s.click("[data-wf-q]");
  await s.type("zzzqqq");
  let status = "";
  for (let i = 0; i < 40 && !/^Updated/.test(status); i++) {
    await sleep(250);
    status = await s.eval(`(document.getElementById("live") || {}).textContent || ""`);
  }
  const st = await s.eval(`({ q: document.querySelector("[data-wf-q]").value, rows: [...document.querySelectorAll("[data-wf] .row")].filter(r => !r.hidden).length,
    focus: document.activeElement && document.activeElement.matches("[data-wf-q]") })`);
  check("a live update keeps the waterfall search", /^Updated/.test(status) && st.q === "zzzqqq" && st.rows === 0 && st.focus,
    `${status} ${JSON.stringify(st)}`);
  check("live update: no errors", s.take().length === 0);
}

// ---- run ----
const chrome = await launch();
console.log(`# ${chrome.version} against ${base}`);
try {
  const s = await page(chrome.port);
  const pages = await crawl(s, 1400);
  await crawl(s, 390);
  await navigationBurst(s, pages);
  await buildPage(s, pages);
  await pipelinePage(s, pages);
  await keys(s, pages);
  await notFound(s);
  if (args.live) await liveUpdate(s, args.live);
  await s.close();
} catch (e) {
  check("the checks ran to the end", false, e.stack || String(e));
} finally {
  chrome.proc.kill();
  await sleep(300);
  fs.rmSync(chrome.dir, { recursive: true, force: true });
}
console.log(`# ${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
