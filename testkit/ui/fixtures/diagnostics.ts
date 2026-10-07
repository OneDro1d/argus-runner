/**
 * Diagnostics fixture for Playwright specs against a live deployment.
 *
 * Vendored into the OneDroid Testing Suite (D4) — name-neutral, no dependency on any
 * private dev-kit. Every test gets, for free:
 *   - console error + warning capture
 *   - pageerror capture (JS exceptions)
 *   - requestfailed capture (network failures)
 *   - response-error capture (4xx/5xx) WITH body (truncated)
 *   - auto-attached diagnostics.log on the HTML report
 *   - automatic secret redaction (key/password/secret/token env values + JWTs)
 *
 * The load-bearing assertion is "no backend 4xx/5xx during the flow" — see
 * noBackendErrors(). A UI scenario that elides it is a silent regression (VR-L2).
 */
import { test as base, expect, type TestInfo } from '@playwright/test';
import fs from 'node:fs';

/** Build a redactor that masks known secrets before they land in any artifact. */
function buildRedactor() {
  const secrets: string[] = [];
  for (const k of Object.keys(process.env)) {
    if (/(KEY|PASSWORD|SECRET|TOKEN)$/.test(k)) {
      const v = process.env[k];
      if (v && v.length > 8) secrets.push(v);
    }
  }
  const patterns: RegExp[] = [
    /sk-[A-Za-z0-9_-]{20,}/g,
    /sk-proj-[A-Za-z0-9_-]{20,}/g,
    /eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/g, // JWTs
  ];
  return (s: string): string => {
    let out = s;
    for (const secret of secrets) out = out.split(secret).join('***REDACTED***');
    for (const pattern of patterns) out = out.replace(pattern, '***REDACTED***');
    return out;
  };
}
export const redact = buildRedactor();

export type DiagnosticEntry = {
  kind: 'console' | 'pageerror' | 'requestfailed' | 'response-error';
  time: string;
  status?: number;
  detail: string;
};

export type Diagnostics = {
  entries: DiagnosticEntry[];
  flush: (info: TestInfo) => Promise<void>;
};

export const test = base.extend<{ diagnostics: Diagnostics }>({
  diagnostics: async ({ page }, use, info) => {
    const entries: DiagnosticEntry[] = [];
    const stamp = () => new Date().toISOString();

    page.on('console', msg => {
      if (msg.type() === 'error' || msg.type() === 'warning') {
        entries.push({ kind: 'console', time: stamp(), detail: `[${msg.type()}] ${msg.text()}` });
      }
    });
    page.on('pageerror', err => {
      entries.push({ kind: 'pageerror', time: stamp(), detail: err.stack || err.message });
    });
    page.on('requestfailed', req => {
      entries.push({
        kind: 'requestfailed', time: stamp(),
        detail: `${req.method()} ${req.url()} — ${req.failure()?.errorText ?? 'unknown failure'}`,
      });
    });
    page.on('response', async resp => {
      const status = resp.status();
      if (status >= 400) {
        let body = '';
        try { body = (await resp.text()).slice(0, 2000); } catch { /* body may be consumed */ }
        entries.push({
          kind: 'response-error', time: stamp(), status,
          detail: `${resp.request().method()} ${resp.url()} → ${status}${body ? '\n' + body : ''}`,
        });
      }
    });

    const diagnostics: Diagnostics = {
      entries,
      flush: async (testInfo: TestInfo) => {
        if (!entries.length) return;
        const body = redact(entries.map(e => `[${e.time}] ${e.kind.toUpperCase()}\n${e.detail}`).join('\n\n---\n\n'));
        await testInfo.attach('diagnostics.log', { body, contentType: 'text/plain' });
      },
    };
    await use(diagnostics);
    await diagnostics.flush(info);
  },
});

export { expect };

/**
 * The binding backend-error predicate (DF-DEC-M25-08 / VR-L2): NO backend 4xx OR 5xx
 * (status 400–599) was observed on a backend (/api/*) response during the flow. Call
 * this at the END of every UI spec; if it fails, the scenario goes RED.
 */
export function noBackendErrors(diagnostics: Diagnostics) {
  const backendErrors = diagnostics.entries.filter(
    e => e.kind === 'response-error' && typeof e.status === 'number' && e.status >= 400 && e.status <= 599 && /\/api\//.test(e.detail),
  );
  expect(
    backendErrors,
    `Backend 4xx/5xx during the flow:\n${backendErrors.map(e => e.detail).join('\n\n')}`,
  ).toEqual([]);
}

/**
 * Assert a storageState file exists and has a non-zero __client_uat. Fails loud with
 * re-capture instructions if auth is missing/stale; warns at >55 minutes.
 */
export function requireFreshAuth(authFile: string) {
  if (!fs.existsSync(authFile)) {
    throw new Error(`Auth state not found at ${authFile}.\nRun:  node auth/capture-auth.mjs  (then sign in in the headed browser).`);
  }
  const ageMs = Date.now() - fs.statSync(authFile).mtimeMs;
  if (ageMs > 55 * 60 * 1000) {
    console.warn(`[auth] Captured session is ${Math.floor(ageMs / 60000)}m old — may be stale. Re-capture if a 401 appears.`);
  }
  const raw = JSON.parse(fs.readFileSync(authFile, 'utf8'));
  const uat = raw.cookies?.find((c: any) => c.name === '__client_uat')?.value;
  if (!uat || uat === '0') {
    throw new Error(`Captured session has __client_uat=${uat ?? 'MISSING'} — not signed in.\nRe-run: node auth/capture-auth.mjs`);
  }
}

/**
 * V29-021 (VR13-UI) — the SUMMARY twin of noBackendErrors.
 *
 * ⛔ WHY BOTH EXIST. noBackendErrors *throws*: it is the binding predicate a hand-written spec calls
 * at the end of a flow, and a throw is exactly right there. But a declared `- no backend 4xx/5xx`
 * bullet has to fill an `observed` field either way — the outcomes file records what was ACTUALLY
 * seen, for a pass and for a failure alike — and a function that throws can never return that.
 *
 * Returns null when clean, else the redacted `GET <url> → <status>` list.
 */
export function backendErrorSummary(diagnostics: Diagnostics): string | null {
  const backendErrors = diagnostics.entries.filter(
    e => e.kind === 'response-error' && typeof e.status === 'number' && e.status >= 400 && e.status <= 599 && /\/api\//.test(e.detail),
  );
  if (backendErrors.length === 0) return null;
  return backendErrors.map(e => redact(e.detail)).join('; ');
}

/**
 * V29-021 — the same, for `- no console errors`. Console output is the SUT's own text, so it goes
 * through redact() before it can reach a report.
 */
export function consoleErrorSummary(diagnostics: Diagnostics): string | null {
  const consoleErrors = diagnostics.entries.filter(e => e.kind === 'console' || e.kind === 'pageerror');
  if (consoleErrors.length === 0) return null;
  return consoleErrors.map(e => redact(e.detail)).join('; ');
}
