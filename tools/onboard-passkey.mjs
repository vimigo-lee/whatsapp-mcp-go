// Minimal, dependency-free passkey onboarding driver.
// Drives your installed Chrome over the DevTools Protocol (node built-in
// WebSocket/fetch) to run navigator.credentials.get() under the web.whatsapp.com
// origin, then submits the assertion + auto-confirms against the bridge.
//
// Leak-safety: ephemeral throwaway profile (deleted on exit), whole process-tree
// killed on exit (by PID + a backstop match on the unique profile name), a
// watchdog that force-quits if it hangs, and the process exits after one link.
//
// Usage:
//   MANAGER: PASSKEY_LINK=<code from manager "Link (passkey)"> node onboard-passkey.mjs
//   LOCAL:   node onboard-passkey.mjs   (talks to the bridge on localhost:8080;
//            override with BRIDGE_URL. API key from WHATSAPP_API_KEY env or
//            whatsapp-bridge/.env in this repo.)
// In both cases: start phone-code pairing for the number first, then run this.
import { spawn, execSync } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, basename } from "node:path";

const IS_WIN = process.platform === "win32";
const IS_MAC = process.platform === "darwin";

// Auto-detect a Chromium-based browser (Chrome/Edge/Brave/Chromium) across
// Windows/macOS/Linux. Override with BROWSER_PATH=... if yours lives elsewhere.
function detectBrowser() {
  if (process.env.BROWSER_PATH) return process.env.BROWSER_PATH;
  const candidates = IS_WIN ? [
    "C:/Program Files/Google/Chrome/Application/chrome.exe",
    "C:/Program Files (x86)/Google/Chrome/Application/chrome.exe",
    join(process.env.LOCALAPPDATA || "", "Google/Chrome/Application/chrome.exe"),
    "C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe",
    "C:/Program Files/Microsoft/Edge/Application/msedge.exe",
    "C:/Program Files/BraveSoftware/Brave-Browser/Application/brave.exe",
  ] : IS_MAC ? [
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
    "/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
    "/Applications/Chromium.app/Contents/MacOS/Chromium",
  ] : [
    "/usr/bin/google-chrome", "/usr/bin/google-chrome-stable",
    "/usr/bin/microsoft-edge", "/usr/bin/brave-browser",
    "/usr/bin/chromium", "/usr/bin/chromium-browser", "/snap/bin/chromium",
  ];
  for (const c of candidates) { if (c && existsSync(c)) return c; }
  if (!IS_WIN) {
    for (const name of ["google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"]) {
      try { const p = execSync(`command -v ${name}`, { stdio: ["ignore", "pipe", "ignore"] }).toString().trim(); if (p) return p; } catch {}
    }
  }
  throw new Error("No Chromium-based browser found. Set BROWSER_PATH to your Chrome/Edge/Brave/Chromium executable.");
}
const CHROME = detectBrowser();

// Two modes:
//  - MANAGER (production): set PASSKEY_LINK to the one-paste code from the
//    manager's "Link (passkey)" action. The helper talks to the hosted manager's
//    proxy routes with a scoped token — no bridge port is ever exposed.
//  - LOCAL (dev): no PASSKEY_LINK — talk straight to a bridge on localhost:8089
//    with the bridge API key (the original prototype path).
// Link code comes from PASSKEY_LINK env, OR from argv when launched as the
// wapasskey:// protocol handler (the manager's "Link via passkey" button). The
// OS passes the whole URL, e.g. "wapasskey://<code>/" — strip scheme + slashes.
function resolveLink() {
  if (process.env.PASSKEY_LINK) return process.env.PASSKEY_LINK;
  const arg = process.argv.slice(2).find((a) => a && a.trim());
  if (!arg) return undefined;
  return arg.trim().replace(/^wapasskey:\/\//i, "").replace(/\/+$/, "");
}
const LINK = resolveLink();
let BASE, PK, authHeader = null;
if (LINK) {
  const { u, n, t } = JSON.parse(Buffer.from(LINK, "base64url").toString("utf8"));
  BASE = `${String(u).replace(/\/+$/, "")}/api/admin/numbers/${n}/passkey`;
  PK = { request: "/request", response: "/response", code: "/code", confirm: "/confirm" };
  authHeader = `Bearer ${t}`;
} else {
  BASE = process.env.BRIDGE_URL || "http://localhost:8080";
  PK = { request: "/api/auth/passkey-request", response: "/api/auth/passkey-response", code: "/api/auth/passkey-code", confirm: "/api/auth/passkey-confirm" };
}
// Local mode auth: the bridge API key — from WHATSAPP_API_KEY env, else read
// from whatsapp-bridge/.env in this repo (the standard standalone layout).
function resolveKey() {
  if (process.env.WHATSAPP_API_KEY) return process.env.WHATSAPP_API_KEY;
  try {
    const envPath = join(import.meta.dirname, "..", "whatsapp-bridge", ".env");
    const line = readFileSync(envPath, "utf8").split(/\r?\n/).find((l) => l.startsWith("WHATSAPP_API_KEY="));
    if (line) return line.slice("WHATSAPP_API_KEY=".length).trim();
  } catch {}
  return null;
}
const KEY = LINK ? null : resolveKey();
if (!LINK && !KEY) {
  console.error("No bridge API key. Set WHATSAPP_API_KEY or ensure whatsapp-bridge/.env exists next to this repo's tools/ dir.");
  process.exit(1);
}
const PORT = 9300 + Math.floor(Math.random() * 500);
const PROFILE = mkdtempSync(join(tmpdir(), "wa-pk-"));
const PROFILE_TAG = basename(PROFILE);
const WATCHDOG_MS = 180000;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let chromeProc = null;
let cleaned = false;
function cleanup() {
  if (cleaned) return; cleaned = true;
  try {
    if (chromeProc?.pid) {
      if (IS_WIN) execSync(`taskkill /F /T /PID ${chromeProc.pid}`, { stdio: "ignore" });
      else { try { process.kill(-chromeProc.pid, "SIGKILL"); } catch { try { chromeProc.kill("SIGKILL"); } catch {} } }
    }
  } catch {}
  // backstop: kill any browser process launched with our unique temp profile
  try {
    if (IS_WIN) execSync(`powershell -NoProfile -Command "Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -like '*${PROFILE_TAG}*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }"`, { stdio: "ignore" });
    else execSync(`pkill -9 -f ${PROFILE_TAG}`, { stdio: "ignore" });
  } catch {}
  try { rmSync(PROFILE, { recursive: true, force: true }); } catch {}
}
process.on("exit", cleanup);
process.on("SIGINT", () => { cleanup(); process.exit(1); });
const wd = setTimeout(() => { console.error("watchdog: timed out, force-closing"); cleanup(); process.exit(2); }, WATCHDOG_MS);

// --- request helper (manager scoped-token OR local bridge JWT) -------------
let token = null;
async function jwt() {
  if (authHeader) return null;            // manager mode: static bearer, no login
  if (token) return token;
  const r = await fetch(`${BASE}/auth/login`, { method: "POST", headers: { Authorization: `Bearer ${KEY}` } });
  token = (await r.json()).token; return token;
}
async function pk(method, step, body) {
  const t = await jwt();
  const auth = authHeader ?? `Bearer ${t}`;
  const r = await fetch(`${BASE}${PK[step]}`, { method, headers: { Authorization: auth, "Content-Type": "application/json" }, body });
  if (r.status === 401 && !authHeader) token = null;
  return { status: r.status, text: r.status === 204 ? "" : await r.text() };
}

// --- CDP over built-in WebSocket -----------------------------------------
let ws = null, msgId = 0; const pending = new Map();
function cdp(method, params = {}) {
  const id = ++msgId; ws.send(JSON.stringify({ id, method, params }));
  return new Promise((res, rej) => pending.set(id, { res, rej }));
}
async function waitForTarget() {
  for (let i = 0; i < 80; i++) {
    try {
      const list = await (await fetch(`http://localhost:${PORT}/json`)).json();
      const t = list.find((x) => x.type === "page" && x.url.includes("web.whatsapp.com") && x.webSocketDebuggerUrl);
      if (t) return t;
    } catch {}
    await sleep(500);
  }
  throw new Error("web.whatsapp.com CDP target never appeared");
}
function buildGetExpr(o) {
  return `(async () => {
    const O = ${JSON.stringify(o)};
    const dec = (s) => { s = s.replaceAll('-','+').replaceAll('_','/'); while (s.length % 4) s += '='; const bin = atob(s); const u = new Uint8Array(bin.length); for (let i=0;i<bin.length;i++) u[i]=bin.charCodeAt(i); return u.buffer; };
    const enc = (buf) => { let x = btoa(String.fromCharCode.apply(null, new Uint8Array(buf))); x = x.replaceAll('+','-').replaceAll('/','_'); while (x.endsWith('=')) x = x.slice(0,-1); return x; };
    const publicKey = { challenge: dec(O.challenge), rpId: O.rpId, timeout: O.timeout, userVerification: O.userVerification, allowCredentials: [], extensions: O.extensions };
    const cred = await navigator.credentials.get({ publicKey });
    const r = cred.response;
    return JSON.stringify({ id: cred.id, rawId: enc(cred.rawId), type: cred.type, response: { clientDataJSON: enc(r.clientDataJSON), authenticatorData: enc(r.authenticatorData), signature: enc(r.signature), userHandle: r.userHandle ? enc(r.userHandle) : null } });
  })()`;
}

async function main() {
  // 1. wait for a pending passkey challenge (operator starts phone-code pairing)
  console.log(`Mode: ${LINK ? "MANAGER (" + BASE + ")" : "LOCAL (" + BASE + ")"}`);
  console.log("Waiting for a pending passkey request...");
  console.log("(Start phone-code pairing for the number now, if you haven't.)");
  let opts = null;
  for (let i = 0; i < 240; i++) {
    const r = await pk("GET", "request");
    if (r.status === 200) { opts = JSON.parse(r.text); break; }
    if (r.status === 409) { console.error("passkey error:", r.text); process.exit(3); }
    await sleep(500);
  }
  if (!opts) { console.error("No pending challenge appeared (did pairing start?)."); process.exit(3); }
  console.log("Got challenge:", opts.challenge);

  // 2. launch an isolated, ephemeral Chrome at web.whatsapp.com
  chromeProc = spawn(CHROME, [
    `--user-data-dir=${PROFILE}`,
    `--remote-debugging-port=${PORT}`,
    "--no-first-run", "--no-default-browser-check", "--disable-sync",
    "--app=https://web.whatsapp.com/",
  ], { stdio: "ignore", detached: !IS_WIN }); // POSIX: own process group so we can kill the whole tree

  // 3. attach CDP
  const target = await waitForTarget();
  ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = () => rej(new Error("CDP ws error")); });
  ws.onmessage = (e) => {
    const m = JSON.parse(e.data);
    if (m.id && pending.has(m.id)) { const { res, rej } = pending.get(m.id); pending.delete(m.id); m.error ? rej(new Error(JSON.stringify(m.error))) : res(m.result); }
  };
  await sleep(1200); // let the page settle on the whatsapp.com origin

  // 4. run get() with a synthetic user gesture -> passkey QR shows in the window
  console.log("\n>>> A WhatsApp window opened and a passkey prompt should appear.");
  console.log(">>> Scan the passkey QR with the phone's SYSTEM camera (not WhatsApp).\n");
  const ev = await cdp("Runtime.evaluate", { expression: buildGetExpr(opts), awaitPromise: true, userGesture: true, returnByValue: true });
  if (ev.exceptionDetails) throw new Error("get() failed: " + JSON.stringify(ev.exceptionDetails.exception?.description || ev.exceptionDetails));
  const assertion = ev.result.value;
  console.log("Assertion obtained. Submitting...");

  // 5. submit -> code -> confirm (all fast, within the socket window)
  const resp = await pk("POST", "response", assertion);
  console.log("  response ->", resp.status, resp.text);
  if (resp.status !== 200) throw new Error("server rejected the assertion");
  let code = null;
  for (let i = 0; i < 240; i++) {
    const c = await pk("GET", "code");
    if (c.status === 200) { code = JSON.parse(c.text); break; }
    if (c.status === 409) throw new Error("code error: " + c.text);
    await sleep(250);
  }
  if (!code) throw new Error("no confirmation code arrived");
  // Newer WhatsApp clients show the SAME code on the phone and expect the human
  // to confirm on the phone BEFORE the new device sends its confirmation. Firing
  // SendPasskeyConfirmation instantly races the phone -> "could not link device".
  // We can't detect the phone tap directly, so: in a terminal, wait for the
  // operator to press ENTER after they tap the phone (deterministic). Headless
  // (one-click) has no stdin, so fall back to a timed delay. The pairing socket
  // has ~3.3 min from challenge, so a human-paced wait is safe.
  const confirmDelayMs = Number(process.env.PASSKEY_CONFIRM_DELAY_MS ?? 12000);
  // Default: auto-confirm on a timer (no keypress needed, terminal or headless) —
  // you just tap the phone during the countdown. Set PASSKEY_WAIT_ENTER=1 for
  // manual control (wait for you to press ENTER after tapping) in a terminal.
  const waitEnter = process.env.PASSKEY_WAIT_ENTER === "1" && process.stdin.isTTY;
  console.log(`  code: ${code.code}  skipHandoffUX: ${code.skip_handoff_ux}  (verify it matches your phone)`);
  if (waitEnter) {
    console.log(`  >>> Tap "Link device"/Confirm on the PHONE, then press ENTER here to finish...`);
    await new Promise((res) => {
      process.stdin.resume();
      process.stdin.once("data", () => { process.stdin.pause(); res(); });
    });
  } else {
    console.log(`  >>> Tap "Link device"/Confirm on the PHONE now; confirming in ${Math.round(confirmDelayMs / 1000)}s...`);
    if (confirmDelayMs > 0) await sleep(confirmDelayMs);
  }
  const cf = await pk("POST", "confirm", "{}");
  console.log("  confirm ->", cf.status, cf.text);
  console.log("\nDONE. Watch the bridge for 'Successfully paired' / 'Connected to WhatsApp'.");
}

main()
  .then(() => { clearTimeout(wd); cleanup(); process.exit(0); })
  .catch((e) => { console.error("\nERROR:", e.message); clearTimeout(wd); cleanup(); process.exit(1); });
