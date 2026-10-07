import { defineConfig, devices } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// Load .env.test (gitignored) so specs can use TEST_* / CLERK_* secrets.
const __dirname = path.dirname(fileURLToPath(import.meta.url));
const envTestPath = path.resolve(__dirname, '.env.test');
if (fs.existsSync(envTestPath)) {
  for (const line of fs.readFileSync(envTestPath, 'utf8').split(/\r?\n/)) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith('#')) continue;
    const eq = trimmed.indexOf('=');
    if (eq < 0) continue;
    const key = trimmed.slice(0, eq).trim();
    const val = trimmed.slice(eq + 1).trim();
    if (!(key in process.env)) process.env[key] = val;
  }
}

const LIVE_URL = process.env.APP_URL ?? 'https://app.example.com';
const LOCAL_URL = process.env.LOCAL_APP_URL ?? 'http://localhost:5173';
// The captured Clerk session — used when it exists. UC168: an AUTH_MODE=none SUT (e.g. a SUT on k3d)
// has no Clerk session, and hard-requiring this file made the `live` project fail at harness launch
// for such SUTs. Load it only when present; auth-free SUTs run with no stored state, auth-gated SUTs
// still use it. A spec that NEEDS auth guards with requireFreshAuth (it fails loudly if absent).
const AUTH_STATE = path.resolve(__dirname, 'playwright/.auth/clerk-session.json');
// ⛔ V29-021 U-D: SKIP_WEBSERVER=1 is how the declared-assertions contract runs. Without the guard
// the config starts `npm run dev` — a script this repo does not have — so the run would hang or die
// before a single test executed. The contract drives this spec against a STATIC fixture page and
// needs no server at all.
const onlyLive = process.argv.some(a => a.includes('live')) || process.env.SKIP_WEBSERVER === '1';

export default defineConfig({
  testDir: './tests',
  timeout: 60_000,
  expect: { timeout: 15_000 },
  fullyParallel: false, // live tests mutate real state; keep serial (bounded blast radius)
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: 1,
  // V29-021: the json reporter writes beside the HTML one, into argus-out/ — ⛔ NEVER
  // test-results/, which is the VR-L3 discriminator the runner reads to tell a SUT failure from a
  // harness failure. A file of ours in there would make an execution failure look like a SUT one.
  reporter: [
    ['list'],
    ['html', { open: 'never', outputFolder: 'playwright-report' }],
    ['json', { outputFile: 'argus-out/results.json' }],
  ],
  use: {
    trace: 'on', // evidence on every run (cheap; gold for post-mortems)
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
  projects: [
    {
      name: 'chromium-dev', // AUTH_MODE=none dev bypass, no Clerk
      use: { ...devices['Desktop Chrome'], baseURL: LOCAL_URL },
      testMatch: /dev-mode\.spec\.ts/,
    },
    {
      name: 'live', // deployed URL + captured/minted Clerk session
      testMatch: /live\/.*\.spec\.ts/,
      use: {
        ...devices['Desktop Chrome'],
        baseURL: LIVE_URL,
        storageState: fs.existsSync(AUTH_STATE) ? AUTH_STATE : undefined,
      },
    },
  ],
  webServer: onlyLive
    ? undefined
    : { command: 'npm run dev', cwd: '.', url: LOCAL_URL, reuseExistingServer: !process.env.CI, timeout: 60_000 },
});
