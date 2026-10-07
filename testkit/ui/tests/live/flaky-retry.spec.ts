/**
 * Flaky/retry containment (UC-M25-40): a non-deterministic step fails the first attempt
 * and passes on retry. With retries configured the flake is CONTAINED (overall PASS),
 * and an artifact (trace) is ALWAYS captured (trace:'on') — even for the failed attempt.
 * Demonstrates the kit's retry + always-on evidence; run with --retries>=1.
 */
import { test, expect, noBackendErrors } from '../../fixtures/diagnostics';

test('flaky: fails attempt 0, passes on retry — flake contained', async ({ page, diagnostics }, testInfo) => {
  await page.goto('/', { waitUntil: 'networkidle' });
  await expect(
    page.getByRole('heading', { name: /Welcome/i }).or(page.locator('body')).first(),
  ).toBeVisible({ timeout: 15_000 });
  // Inject a deterministic flake: the FIRST attempt throws; retries pass.
  if (testInfo.retry === 0) {
    throw new Error('injected non-deterministic flake (attempt 0) — should pass on retry');
  }
  noBackendErrors(diagnostics);
});
