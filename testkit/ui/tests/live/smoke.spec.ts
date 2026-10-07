/**
 * Live UI smoke — authenticated load → DOM assert → no backend 4xx/5xx (UC-M25-35).
 *
 * Assumptions: a captured/minted Clerk session (storageState), the SPA reachable at
 * APP_URL. On ANY failure the diagnostics fixture attaches diagnostics.log (console +
 * failed requests + 4xx/5xx bodies); trace/screenshot/video are captured by config.
 */
import { test, expect, requireFreshAuth, noBackendErrors } from '../../fixtures/diagnostics';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const AUTH_FILE = path.resolve(__dirname, '../../playwright/.auth/clerk-session.json');

test.beforeAll(() => requireFreshAuth(AUTH_FILE));

test('smoke: authenticated user sees the dashboard, no backend errors', async ({ page, diagnostics }) => {
  const statusPromise = page.waitForResponse(r => /\/api\//.test(r.url()), { timeout: 30_000 }).catch(() => null);
  await page.goto('/');
  const statusResp = await statusPromise;
  if (statusResp && statusResp.status() === 401) {
    throw new Error('A backend /api/* call returned 401 — re-run auth/capture-auth.mjs.');
  }
  // authed-signal assertion (role-based; selector discipline). The Clerk user-menu
  // button renders only when signed in; a sidebar <nav> appears once onboarded — either
  // proves the authenticated app rendered (the app's onboarding view has the user-menu, no nav).
  await expect(
    page.locator('nav').or(page.getByRole('button', { name: /user menu|account/i })).first(),
  ).toBeVisible({ timeout: 15_000 });
  // the load-bearing final assertion
  noBackendErrors(diagnostics);
});
