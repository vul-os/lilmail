/**
 * lilmail demo-mode Playwright screenshotter
 *
 * Captures screenshots using the in-memory demo account (no real IMAP needed).
 * Assumes the server is already running (started by scripts/seed-demo.sh or
 * manually with [demo] enabled = true in config.toml).
 *
 * Run via:
 *   scripts/seed-demo.sh --screenshots    (recommended — handles everything)
 *   LILMAIL_EXTERNAL=1 BASE_URL=http://localhost:3099 node scripts/demo-screenshots.mjs
 *
 * Environment variables:
 *   BASE_URL            Running lilmail instance. Default: http://localhost:3099
 *   LILMAIL_DEMO_EMAIL  Demo account email.    Default: demo@lilmail.dev
 *   LILMAIL_DEMO_PASS   Demo account password. Default: demo
 */

import { chromium } from 'playwright';
import { resolve, dirname } from 'path';
import { fileURLToPath } from 'url';
import { mkdirSync } from 'fs';

const __dirname = dirname(fileURLToPath(import.meta.url));
const ROOT    = resolve(__dirname, '..');
const OUT_DIR = resolve(ROOT, 'docs', 'screenshots');
mkdirSync(OUT_DIR, { recursive: true });

const BASE_URL    = process.env.BASE_URL || 'http://localhost:3099';
const DEMO_EMAIL  = process.env.LILMAIL_DEMO_EMAIL || process.env.LILMAIL_USERNAME || 'demo@lilmail.dev';
const DEMO_PASS   = process.env.LILMAIL_DEMO_PASS  || process.env.LILMAIL_PASSWORD || 'demo';

async function shot(page, name, description) {
  const path = resolve(OUT_DIR, `${name}.png`);
  await page.screenshot({ path, fullPage: false });
  console.log(`  [ok] ${name}.png — ${description}`);
  return path;
}

async function main() {
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({
    viewport: { width: 1280, height: 800 },
    colorScheme: 'light',
  });
  const page = await context.newPage();

  try {
    // ------------------------------------------------------------------
    // 1. Login page
    // ------------------------------------------------------------------
    console.log('\nCapturing: login page');
    await page.goto(`${BASE_URL}/login`, { waitUntil: 'networkidle' });
    await shot(page, 'login', 'Login page');

    // ------------------------------------------------------------------
    // 2. Demo login — GET /demo-login creates session + redirects to /inbox
    // ------------------------------------------------------------------
    console.log('\nLogging in via demo-login...');
    // /demo-login (GET) immediately creates a demo session and redirects to /inbox.
    await page.goto(`${BASE_URL}/demo-login`, { waitUntil: 'networkidle' });

    // Verify we're actually authenticated.
    if (page.url().includes('/login')) {
      throw new Error('Demo login failed — check that [demo] enabled = true in config.toml');
    }
    console.log('  Logged in. Current URL:', page.url());

    // Helper: wait for Alpine to finish initialising (removes [x-cloak] attrs).
    async function waitForAlpine(p) {
      try {
        await p.waitForFunction(
          () => document.querySelectorAll('[x-cloak]').length === 0,
          { timeout: 4000 }
        );
      } catch (_) {
        await p.waitForTimeout(600);
      }
    }

    // ------------------------------------------------------------------
    // 3. Inbox
    // ------------------------------------------------------------------
    console.log('\nCapturing: inbox');
    await page.goto(`${BASE_URL}/inbox`, { waitUntil: 'networkidle' });
    await waitForAlpine(page);
    await shot(page, 'inbox', 'Inbox with seeded demo messages');

    // ------------------------------------------------------------------
    // HERO: inbox with a message OPEN in the reading pane (3-pane view).
    // Open a single-message row so the reading pane shows the email content.
    // ------------------------------------------------------------------
    console.log('\nCapturing: hero (inbox + open message)');
    try {
        const heroRows = await page.$$('.email-row');
        const heroTarget = heroRows.length > 1 ? heroRows[1] : heroRows[0];
        if (heroTarget) {
            await heroTarget.click();
            await page.waitForFunction(
                () => {
                    const pane = document.querySelector('#email-viewer-pane');
                    return pane && pane.children.length > 0 && pane.textContent.trim().length > 10
                        && !document.querySelector('#email-viewer-pane .viewer-placeholder');
                },
                { timeout: 5000 }
            ).catch(() => {});
            await page.waitForTimeout(500);
        }
    } catch (_) {}
    await page.screenshot({ path: resolve(OUT_DIR, 'hero.png'), fullPage: false });
    console.log('  [ok] hero.png — Inbox with message open in reading pane (demo data)');

    // ------------------------------------------------------------------
    // 4. Message view — click the first email row, wait for HTMX pane
    // ------------------------------------------------------------------
    console.log('\nCapturing: message view');
    // Use a fresh page for the message view so no residual modal state.
    const msgPage = await context.newPage();
    await msgPage.goto(`${BASE_URL}/inbox`, { waitUntil: 'networkidle' });
    await waitForAlpine(msgPage);

    // Ensure no modals are open.
    await msgPage.evaluate(() => {
      // Force close compose modal via Alpine if available.
      try {
        const root = document.querySelector('[x-data]');
        if (root && root._x_dataStack) {
          const data = root._x_dataStack[0];
          if (data) { data.showComposeModal = false; data.showEmailViewer = false; }
        }
      } catch (_) {}
    });
    await msgPage.waitForTimeout(300);

    // Click a single-message email row to open the viewer (not a thread which
    // would just expand). Pick the second visible row (GitHub, single message).
    const allRows = await msgPage.$$('.email-row');
    const targetRow = allRows.length > 1 ? allRows[1] : allRows[0];
    if (targetRow) {
      await targetRow.click();
      // Wait for the viewer pane to load — the HTMX response populates
      // #email-viewer-pane; wait until it has child content.
      try {
        await msgPage.waitForFunction(
          () => {
            const pane = document.querySelector('#email-viewer-pane');
            return pane && pane.children.length > 0 && pane.textContent.trim().length > 10;
          },
          { timeout: 5000 }
        );
      } catch (_) {
        await msgPage.waitForTimeout(1500);
      }
      const msgPath = resolve(OUT_DIR, 'message.png');
      await msgPage.screenshot({ path: msgPath, fullPage: false });
      console.log('  [ok] message.png — Message viewer — seeded email open');
    } else {
      console.warn('  [skip] message — no .email-row found; inbox may still be loading');
    }
    await msgPage.close();

    // ------------------------------------------------------------------
    // 5. Compose modal
    // ------------------------------------------------------------------
    console.log('\nCapturing: compose modal');
    await page.goto(`${BASE_URL}/inbox`, { waitUntil: 'networkidle' });
    await waitForAlpine(page);
    await page.waitForTimeout(200);

    // Try common selectors for the compose button.
    const composeBtn = await page.$([
      'button[data-compose]',
      '[data-action="compose"]',
      '.compose-btn',
      '.btn-compose',
      'button:has-text("Compose")',
    ].join(', '));

    if (composeBtn) {
      await composeBtn.click();
      await page.waitForTimeout(700);
      // Fill in some demo compose data so it looks realistic.
      const toField = await page.$('input[name="to"], input[placeholder*="To"], input[aria-label*="To"]');
      if (toField) await toField.fill('alice@example.com');
      const subjectField = await page.$('input[name="subject"], input[placeholder*="Subject"]');
      if (subjectField) await subjectField.fill('Re: Product roadmap Q3');
      await shot(page, 'compose', 'Compose modal with CC/BCC and attachment UI');
    } else {
      console.warn('  [skip] compose — compose button not found');
    }

    // ------------------------------------------------------------------
    // 6. Search results
    // ------------------------------------------------------------------
    console.log('\nCapturing: search results');
    await page.goto(`${BASE_URL}/inbox`, { waitUntil: 'networkidle' });
    await waitForAlpine(page);
    await page.waitForTimeout(200);

    const searchInput = await page.$('input[type="search"], input[name="q"]');
    if (searchInput) {
      await searchInput.click();
      // Use type() to generate real keydown/keyup events for HTMX to fire on.
      await page.type('input[type="search"], input[name="q"]', 'roadmap', { delay: 80 });
      // Wait for the debounced HTMX search (500ms delay in hx-trigger) + network.
      await page.waitForTimeout(1400);
      await shot(page, 'search', 'Search results for "roadmap"');
    } else {
      // Fallback: direct API endpoint.
      await page.goto(`${BASE_URL}/api/search?q=roadmap`, { waitUntil: 'networkidle' });
      await shot(page, 'search', 'Search results (fragment)');
    }

    // ------------------------------------------------------------------
    // 7. Calendar (rendered even without CalDAV — shows empty month)
    // ------------------------------------------------------------------
    console.log('\nCapturing: calendar');
    const calResp = await page.goto(`${BASE_URL}/calendar`, { waitUntil: 'networkidle' });
    if (calResp && calResp.status() < 400) {
      await shot(page, 'calendar', 'Calendar month view');
    } else {
      console.warn('  [skip] calendar — caldav not enabled (status ' + calResp?.status() + ')');
      // Capture settings as a fallback so we at least have 6 screenshots.
    }

    // ------------------------------------------------------------------
    // 8. Settings
    // ------------------------------------------------------------------
    console.log('\nCapturing: settings');
    await page.goto(`${BASE_URL}/settings`, { waitUntil: 'networkidle' });
    await shot(page, 'settings', 'Settings page');

  } finally {
    await browser.close();
  }

  console.log(`\nDone. Screenshots written to ${OUT_DIR}`);
}

main().catch((err) => {
  console.error('\nDemo screenshotter failed:', err.message);
  process.exit(1);
});
