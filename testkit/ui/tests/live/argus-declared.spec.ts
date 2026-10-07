import * as fs from 'fs';
import * as path from 'path';
import type { Page } from '@playwright/test';
import { test, expect, backendErrorSummary, consoleErrorSummary } from '../../fixtures/diagnostics';

/**
 * V29-021 (VR13-UI) — THE SPEC THAT EVALUATES A SCENARIO'S DECLARED ASSERTIONS.
 *
 * Until 0.3.32 a Web UI scenario's `## EXPECT` was read by nothing: the assertions lived inside a
 * hand-written spec and the verdict was this process's exit code. An author could write three
 * careful bullets and the product would compare none of them.
 *
 * The runner parses those bullets, hands them here as ARGUS_UI_ASSERTS, and reads back one
 * `{bullet, ok, observed}` per bullet from ARGUS_UI_OUT. This file is generic: it knows the
 * grammar, never a particular SUT.
 *
 * ⛔⛔ EVERY DECLARED BULLET ALWAYS PRODUCES AN ENTRY. This is the rule the whole file is shaped
 * around. A literal `await expect(...)` throws on the FIRST failure, so with one test() the
 * remaining bullets would produce no entry at all — and the runner reads a missing entry as "not
 * evaluated" and reports an EXECUTION ERROR. A thrown assertion would therefore turn a SUT failure
 * into "we did not look", which is the exact misattribution this round exists to remove. So every
 * check is wrapped, records its outcome, and CONTINUES; the test still ends red through the tally.
 *
 * ⚠ `observed` is REALITY-ONLY (VR-C8). It says what was in the DOM, never what the scenario asked
 * for: the asserted value is holdout material and `observed` reaches the product hat.
 */

type Assert = {
  bullet: string;
  kind: 'dom' | 'no_backend_errors' | 'no_console_errors';
  selector?: string;
  op?: 'contains' | 'matches';
  value?: string;
};

type Outcome = { bullet: string; ok: boolean; observed: string };

const rawAsserts = process.env.ARGUS_UI_ASSERTS ?? '[]';
const outPath = process.env.ARGUS_UI_OUT ?? '';
const appURL = process.env.APP_URL ?? '';

// ⛔ A `dom has` CHECK WAITS, BOUNDED. It used to count the selector the instant page.goto returned,
// so an SPA that renders after a script loads (Clerk's sign-in card appears 1-3s after the first
// paint) was judged on an empty document: a WEBUI-001 run, body text "".
// The deadline is ONE budget for the whole run, measured from navigation, so several failing bullets
// cost 15s in total, not 15s each. ARGUS_UI_DOM_WAIT_MS overrides it (the contract tests use a short one).
const domWaitMs = (() => {
  const n = Number(process.env.ARGUS_UI_DOM_WAIT_MS);
  return process.env.ARGUS_UI_DOM_WAIT_MS && Number.isFinite(n) && n >= 0 ? n : 15_000;
})();
const domPollMs = 250;

// An auth provider that never loads is the usual reason such a page stays blank, so a check that
// times out names the auth requests that failed. Clerk is served from *.clerk.accounts.dev (dev
// instances) or clerk.<the app's domain> (production). Host + path only: a query string can carry a
// session or a key.
function isClerkHost(host: string): boolean {
  return host === 'clerk.accounts.dev' || host.endsWith('.clerk.accounts.dev') || host.startsWith('clerk.');
}

function hostAndPath(raw: string): string {
  try {
    const u = new URL(raw);
    return u.host + u.pathname;
  } catch {
    return '(unparseable url)';
  }
}

// One evaluation of a `dom has` bullet. observed names what was there, never what was asked for.
async function evalDom(page: Page, a: Assert): Promise<[boolean, string]> {
  const loc = page.locator(a.selector!);
  const count = await loc.count();
  if (count === 0) {
    return [false, `no element matches ${a.selector}`];
  }
  if (!a.op) {
    return [true, `${count} element(s) match ${a.selector}`];
  }
  const text = (await loc.first().innerText()).trim();
  const ok = a.op === 'contains' ? text.includes(a.value!) : new RegExp(a.value!).test(text);
  return [ok, `${a.selector} text is ${JSON.stringify(text)}`];
}

let asserts: Assert[] = [];
try {
  asserts = JSON.parse(rawAsserts) as Assert[];
} catch {
  asserts = [];
}

function writeOutcomes(outcomes: Outcome[]) {
  if (!outPath) return;
  fs.mkdirSync(path.dirname(outPath), { recursive: true });
  fs.writeFileSync(outPath, JSON.stringify(outcomes, null, 2), 'utf8');
}

// A scenario that declares nothing is not this spec's business — the runner refuses it (V31-002's
// R10) long before a browser starts. Skipping keeps this file silent rather than inventing a pass.
test.skip(asserts.length === 0, 'no declared ui assertions for this run');

test('argus: the scenario\'s declared assertions', async ({ page, diagnostics }) => {
  const outcomes: Outcome[] = [];

  const clerkFailures: string[] = [];
  const noteClerk = (url: string, what: string) => {
    try {
      if (isClerkHost(new URL(url).hostname)) clerkFailures.push(`${hostAndPath(url)} ${what}`);
    } catch {
      // not a URL we can classify, so not one we report
    }
  };
  page.on('requestfailed', req => noteClerk(req.url(), `failed: ${req.failure()?.errorText ?? 'unknown'}`));
  page.on('response', res => {
    if (res.status() >= 400) noteClerk(res.url(), `answered ${res.status()}`);
  });

  await page.goto(appURL || '/');
  const domDeadline = Date.now() + domWaitMs;

  for (const a of asserts) {
    // ⛔ ONE ENTRY PER BULLET, WHATEVER HAPPENS INSIDE. try/catch rather than a bare await, so a
    // locator that throws (a bad selector, a detached frame) is recorded as a FAILED check with the
    // reason — not as an absent entry, which the runner would read as "never evaluated".
    let ok = false;
    let observed = '';
    try {
      switch (a.kind) {
        case 'dom': {
          // Re-evaluated until it holds or the run's deadline passes. The LAST evaluation is what is
          // recorded, so a pass and a timeout both report what was actually on the page.
          for (;;) {
            [ok, observed] = await evalDom(page, a);
            if (ok || Date.now() >= domDeadline) break;
            await page.waitForTimeout(domPollMs);
          }
          if (!ok) {
            observed += ` (after waiting up to ${domWaitMs}ms from navigation)`;
            if (clerkFailures.length > 0) {
              observed += `; failed Clerk requests: ${[...new Set(clerkFailures)].slice(0, 5).join('; ')}`;
            }
          }
          break;
        }
        case 'no_backend_errors': {
          const summary = backendErrorSummary(diagnostics);
          ok = summary === null;
          observed = summary === null ? 'no backend 4xx/5xx during the flow' : `backend errors: ${summary}`;
          break;
        }
        case 'no_console_errors': {
          const summary = consoleErrorSummary(diagnostics);
          ok = summary === null;
          observed = summary === null ? 'no console errors during the flow' : `console errors: ${summary}`;
          break;
        }
        default:
          ok = false;
          observed = `unknown assertion kind ${JSON.stringify((a as Assert).kind)}`;
      }
    } catch (err) {
      ok = false;
      observed = `the check could not be evaluated: ${String((err as Error)?.message ?? err)}`;
    }
    outcomes.push({ bullet: a.bullet, ok, observed });
  }

  // ⛔ WRITTEN BEFORE THE TALLY. The file is the evidence, and it must exist even when the tally
  // below fails the test — the runner reads it either way, and a failing run with no evidence is
  // indistinguishable from a run that never looked.
  writeOutcomes(outcomes);

  const failed = outcomes.filter(o => !o.ok);
  expect(
    failed.map(o => `${o.bullet} -> ${o.observed}`),
    'declared ui assertions that did not hold',
  ).toEqual([]);
});
