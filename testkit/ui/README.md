# OneDroid UI Test Kit (vendored)

The Playwright/Clerk UI testing subset, **vendored into this repo (D4)** so the suite has
no dependency on any private dev-kit. Name-neutral. The runner spawns these specs as an
OS process directly (AC-13: no JMeter/.jmx template involved) against a SUT's web SPA;
`internal/ui` maps the process outcome to a verdict (an execution/harness failure is
reported distinctly from a SUT failure — VR-L3).

## What's here
- `fixtures/diagnostics.ts` — the per-test fixture: console / pageerror / requestfailed /
  4xx-5xx-with-body capture + secret redaction + `requireFreshAuth()` + the binding
  `noBackendErrors()` assertion (no backend 4xx/5xx during the flow).
- `playwright.config.ts` — multi-env projects, `.env.test` loader, `trace:'on'`, serial live runs.
- `auth/capture-auth.mjs` — one-shot Clerk sign-in → `storageState` (DOM-signal wait).
- `auth/mint-clerk-session.mjs` — programmatic Clerk session mint (Admin API → synthetic
  `storageState`) for unattended runs.
- `tests/live/smoke.spec.ts` — authed load → rendered-DOM assert → no backend errors (UC-M25-35).

## Setup (live)
```bash
cd testkit/ui
npm install && npx playwright install chromium
# 1) auth: interactive capture …
APP_URL=https://<your-spa> node auth/capture-auth.mjs
#    … or programmatic (CI/agent):
CLERK_SECRET_KEY=… CLERK_SESSION_ID=… APP_DOMAIN=<host> node auth/mint-clerk-session.mjs
# 2) run
APP_URL=https://<your-spa> npm run test:live
```
`.env.test`, `playwright/.auth/*`, `test-results/`, `playwright-report/` are gitignored — never commit secrets.

## Commandments (load-bearing)
1. `trace:'on'` always · 2. role-based, region-scoped selectors · 3. wait on
`page.waitForResponse`, not fixed timeouts · 4. **always assert "no backend 4xx/5xx at the
end"** (`noBackendErrors`) · 5. skip-when-irrelevant for idempotent reruns · 6. never commit secrets.
