/**
 * Deliberate-RED UI flow (UC-M25-61): a backend 4xx/5xx during the flow MUST fire the
 * "no backend 4xx/5xx" assertion → the scenario goes RED, with the 4xx/5xx captured in
 * diagnostics.log + trace/video. This proves the load-bearing assertion actually bites
 * (a silent-green here would be VR-L2 failure).
 *
 * Mechanism (operator-authorized): drive a page-context request that the backend rejects.
 * The Vite dev server proxies /api/* → the SUT gateway; a method-not-allowed request
 * (DELETE /api/health) returns a real backend 405 — observed via page.on('response') in
 * the diagnostics fixture (a genuine backend 4xx during the flow, not a synthetic stub).
 */
import { test, expect, noBackendErrors } from '../../fixtures/diagnostics';

test('RED: a backend 4xx/5xx during the flow fires the assertion', async ({ page, diagnostics }) => {
  await page.goto('/', { waitUntil: 'networkidle' });
  // [onedroid-test] deliberate fault: a page-context request the backend rejects (405).
  await page.evaluate(async () => {
    try { await fetch('/api/health', { method: 'DELETE' }); } catch { /* network error is also a finding */ }
  });
  await page.waitForResponse(r => /\/api\//.test(r.url()), { timeout: 10_000 }).catch(() => null);
  await page.waitForTimeout(500); // let the response event land in diagnostics
  // MUST throw: a backend 4xx/5xx was observed during the flow.
  noBackendErrors(diagnostics);
});
