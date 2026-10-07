// One-shot Clerk auth capture. Run from testkit/ui/:  node auth/capture-auth.mjs
// Opens a headed Chromium → sign in → the session storageState is saved (~1h TTL).
// Uses a DOM-signal wait (the React app rendering an authed state) — NOT a hash-change
// wait, which fails for apps whose signed-in dashboard stays at #/.
import { chromium } from 'playwright';
import { mkdirSync, existsSync } from 'node:fs';

const OUTPUT = 'playwright/.auth/clerk-session.json';
const URL = process.env.APP_URL || 'https://app.example.com';

if (!existsSync('playwright/.auth')) mkdirSync('playwright/.auth', { recursive: true });

console.log(`Opening ${URL} in a headed browser — sign in; the session saves automatically.`);
const browser = await chromium.launch({ headless: false });
const context = await browser.newContext();
const page = await context.newPage();
await page.goto(URL);

await page.waitForFunction(
  () => {
    const sidebar = document.querySelector('nav');
    const onboarding = Array.from(document.querySelectorAll('h1, h2, h3'))
      .some(el => /welcome|get started|dashboard|onboard/i.test(el.textContent || ''));
    return sidebar !== null || onboarding;
  },
  undefined,             // arg — required positional (a {timeout} here would be treated as arg!)
  { timeout: 300_000 },  // 5-min sign-in window
);
await page.waitForTimeout(3000); // let Clerk hydrate all tokens
await context.storageState({ path: OUTPUT });
console.log(`Auth state saved to ${OUTPUT}`);
await browser.close();
