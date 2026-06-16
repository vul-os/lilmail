/**
 * lilmail Playwright screenshotter
 *
 * Captures screenshots of all major lilmail pages and writes them to
 * docs/screenshots/. Run via:
 *
 *   make screenshots          (from repo root — builds binary + installs deps)
 *   node scripts/screenshots.mjs  (if lilmail is already running)
 *
 * Environment variables:
 *
 *   BASE_URL            Base URL of the running lilmail instance.
 *                       Default: http://localhost:3099
 *
 *   LILMAIL_EXTERNAL    Set to "1" to skip starting/stopping a lilmail server.
 *                       The script will connect to BASE_URL directly.
 *
 *   LILMAIL_USERNAME    Email address for login (inbox/message/compose screenshots).
 *   LILMAIL_PASSWORD    Password for login.
 *   LILMAIL_IMAP_SERVER IMAP hostname  (used when writing a temp config).
 *   LILMAIL_IMAP_PORT   IMAP port      (default: 993).
 *   LILMAIL_SMTP_SERVER SMTP hostname  (used when writing a temp config).
 *   LILMAIL_SMTP_PORT   SMTP port      (default: 587).
 *   LILMAIL_JWT_SECRET  JWT secret     (generated if omitted).
 *   LILMAIL_ENC_KEY     32-byte AES key (generated if omitted).
 */

import { chromium } from 'playwright';
import { execFileSync, spawn } from 'child_process';
import { writeFileSync, existsSync, mkdirSync, rmSync } from 'fs';
import { resolve, dirname } from 'path';
import { fileURLToPath } from 'url';
import { randomBytes } from 'crypto';

// ---------------------------------------------------------------------------
// Paths
// ---------------------------------------------------------------------------
const __dirname = dirname(fileURLToPath(import.meta.url));
const ROOT      = resolve(__dirname, '..');
const OUT_DIR   = resolve(ROOT, 'docs', 'screenshots');
const BINARY    = resolve(ROOT, 'lilmail');
// lilmail reads config.toml from its working directory; we run it from a
// temporary directory so we don't overwrite the real config.toml.
const TMP_DIR   = resolve(ROOT, '.screenshots-tmp');
const TMP_CFG   = resolve(TMP_DIR, 'config.toml');

mkdirSync(OUT_DIR, { recursive: true });
mkdirSync(TMP_DIR, { recursive: true });

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------
const BASE_URL  = process.env.BASE_URL || 'http://localhost:3099';
const PORT      = new URL(BASE_URL).port || '3099';
const EXTERNAL  = process.env.LILMAIL_EXTERNAL === '1';

const USERNAME  = process.env.LILMAIL_USERNAME || '';
const PASSWORD  = process.env.LILMAIL_PASSWORD || '';
const IMAP_HOST = process.env.LILMAIL_IMAP_SERVER || 'imap.example.com';
const IMAP_PORT = process.env.LILMAIL_IMAP_PORT   || '993';
const SMTP_HOST = process.env.LILMAIL_SMTP_SERVER || 'smtp.example.com';
const SMTP_PORT = process.env.LILMAIL_SMTP_PORT   || '587';
const JWT_SECRET  = process.env.LILMAIL_JWT_SECRET  || randomBytes(32).toString('hex');
// AES key must be exactly 32 printable bytes (no control chars in TOML)
const ENC_KEY     = process.env.LILMAIL_ENC_KEY     || randomBytes(16).toString('hex'); // 32 hex chars = 32 bytes

const HAS_CREDS = Boolean(USERNAME && PASSWORD && process.env.LILMAIL_IMAP_SERVER);

// ---------------------------------------------------------------------------
// Temp config
// ---------------------------------------------------------------------------
function writeTempConfig() {
  // lilmail reads config.toml from its cwd; we write into TMP_DIR and run
  // the binary with cwd=TMP_DIR so the real config.toml is never touched.
  const toml = `
[server]
port = ${PORT}
username_is_email = true

[imap]
server = "${IMAP_HOST}"
port = ${IMAP_PORT}
tls = true

[smtp]
server = "${SMTP_HOST}"
port = ${SMTP_PORT}
use_starttls = true

[cache]
folder = "${TMP_DIR}/cache"

[jwt]
secret = "${JWT_SECRET}"

[encryption]
key = "${ENC_KEY.substring(0, 32)}"
`.trim();
  writeFileSync(TMP_CFG, toml, 'utf8');
}

// ---------------------------------------------------------------------------
// Server lifecycle
// ---------------------------------------------------------------------------
let serverProc = null;

function startServer() {
  if (!existsSync(BINARY)) {
    console.error(`Binary not found at ${BINARY}. Run: go build -o lilmail`);
    process.exit(1);
  }
  writeTempConfig();
  console.log(`Starting lilmail on port ${PORT}...`);
  serverProc = spawn(BINARY, [], {
    cwd: TMP_DIR,   // binary reads config.toml from its cwd
    env: process.env,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  serverProc.stdout.on('data', (d) => process.stdout.write(`[server] ${d}`));
  serverProc.stderr.on('data', (d) => process.stderr.write(`[server] ${d}`));
  serverProc.on('exit', (code) => {
    if (code !== null && code !== 0) {
      console.error(`Server exited with code ${code}`);
    }
  });
}

function stopServer() {
  if (serverProc) {
    serverProc.kill('SIGTERM');
    serverProc = null;
  }
  // Clean up temp directory (config + cache)
  if (existsSync(TMP_DIR)) rmSync(TMP_DIR, { recursive: true, force: true });
}

async function waitForServer(url, timeoutMs = 10000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`${url}/health`);
      if (res.ok) return;
    } catch (_) { /* not up yet */ }
    await new Promise((r) => setTimeout(r, 300));
  }
  throw new Error(`Server at ${url} did not become ready within ${timeoutMs}ms`);
}

// ---------------------------------------------------------------------------
// Screenshot helpers
// ---------------------------------------------------------------------------
async function shot(page, name, description) {
  const path = resolve(OUT_DIR, `${name}.png`);
  await page.screenshot({ path, fullPage: false });
  console.log(`  [ok] ${name}.png — ${description}`);
  return path;
}

function skip(name, reason) {
  console.warn(`  [skip] ${name}.png — ${reason}`);
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------
async function main() {
  if (!EXTERNAL) {
    startServer();
    await waitForServer(BASE_URL);
  } else {
    console.log(`Using external server at ${BASE_URL}`);
  }

  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({
    viewport: { width: 1280, height: 800 },
    colorScheme: 'dark',
  });
  const page = await context.newPage();

  try {
    // ------------------------------------------------------------------
    // 1. Login page — always capturable (no credentials needed)
    // ------------------------------------------------------------------
    console.log('\nCapturing: login page');
    await page.goto(`${BASE_URL}/login`, { waitUntil: 'networkidle' });
    await shot(page, 'login', 'Login page');

    // Use login screenshot as hero too (or copy after we have more)
    await page.screenshot({ path: resolve(OUT_DIR, 'hero.png'), fullPage: false });
    console.log('  [ok] hero.png — Login page (placeholder hero; regenerate with live account)');

    // ------------------------------------------------------------------
    // 2. Authenticated views — require a live IMAP account
    // ------------------------------------------------------------------
    if (!HAS_CREDS) {
      console.warn('\nNo IMAP credentials set (LILMAIL_USERNAME / LILMAIL_PASSWORD / LILMAIL_IMAP_SERVER).');
      console.warn('Skipping authenticated screenshots. See docs/SCREENSHOTS.md for instructions.\n');

      for (const name of ['inbox', 'message', 'compose', 'calendar', 'settings', 'search']) {
        skip(name, 'requires live IMAP account — set LILMAIL_USERNAME, LILMAIL_PASSWORD, LILMAIL_IMAP_SERVER');
      }
    } else {
      console.log('\nLogging in...');
      await page.goto(`${BASE_URL}/login`, { waitUntil: 'networkidle' });
      await page.fill('input[name="username"], input[type="email"]', USERNAME);
      await page.fill('input[name="password"], input[type="password"]', PASSWORD);
      await page.click('button[type="submit"]');
      await page.waitForURL((url) => !url.toString().includes('/login'), { timeout: 10000 })
        .catch(() => { throw new Error('Login failed — check credentials'); });

      // Inbox
      console.log('\nCapturing: inbox');
      await page.goto(`${BASE_URL}/inbox`, { waitUntil: 'networkidle' });
      await shot(page, 'inbox', 'Inbox / email list');

      // Update hero to use inbox view
      await page.screenshot({ path: resolve(OUT_DIR, 'hero.png'), fullPage: false });
      console.log('  [ok] hero.png updated to inbox view');

      // Message view — click first message
      console.log('\nCapturing: message view');
      const firstMsg = await page.$('[data-email-id], .email-row, tr[data-id]');
      if (firstMsg) {
        await firstMsg.click();
        await page.waitForTimeout(800);
        await shot(page, 'message', 'Message viewer');
      } else {
        skip('message', 'inbox is empty');
      }

      // Compose modal
      console.log('\nCapturing: compose modal');
      await page.goto(`${BASE_URL}/inbox`, { waitUntil: 'networkidle' });
      const composeBtn = await page.$('button[data-compose], [data-action="compose"], .compose-btn, button:has-text("Compose")');
      if (composeBtn) {
        await composeBtn.click();
        await page.waitForTimeout(600);
        await shot(page, 'compose', 'Compose modal');
      } else {
        skip('compose', 'compose button not found');
      }

      // Calendar
      console.log('\nCapturing: calendar');
      const calResp = await page.goto(`${BASE_URL}/calendar`, { waitUntil: 'networkidle' });
      if (calResp && calResp.status() < 400) {
        await shot(page, 'calendar', 'Calendar month view');
      } else {
        skip('calendar', 'caldav not enabled or route returned error');
      }

      // Settings
      console.log('\nCapturing: settings');
      await page.goto(`${BASE_URL}/settings`, { waitUntil: 'networkidle' });
      await shot(page, 'settings', 'Settings page');

      // Search
      console.log('\nCapturing: search results');
      await page.goto(`${BASE_URL}/api/search?q=test`, { waitUntil: 'networkidle' });
      await shot(page, 'search', 'Search results');
    }

  } finally {
    await browser.close();
    if (!EXTERNAL) stopServer();
  }

  console.log(`\nDone. Screenshots written to ${OUT_DIR}`);
  console.log('See docs/screenshots/README.md for status of each file.');
}

main().catch((err) => {
  console.error('\nScreenshotter failed:', err.message);
  if (!EXTERNAL && serverProc) stopServer();
  process.exit(1);
});
