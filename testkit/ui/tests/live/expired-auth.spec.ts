/**
 * Expired/stale Clerk session handling (UC-M25-38): requireFreshAuth must FAIL LOUD with
 * re-capture instructions when the captured session is not signed in (__client_uat=0 /
 * missing) or the file is absent — never silently proceed into a 401. (The live-401 path
 * is asserted in smoke.spec.ts: a backend /api/* 401 throws "re-run auth/capture-auth.mjs".)
 */
import { test, expect, requireFreshAuth } from '../../fixtures/diagnostics';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';

test('expired/missing session fails loud with re-capture instructions', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'authchk-'));

  // (a) __client_uat=0 → signed out: must throw "not signed in" + re-capture.
  const uat0 = path.join(dir, 'uat0.json');
  fs.writeFileSync(uat0, JSON.stringify({ cookies: [{ name: '__client_uat', value: '0' }] }));
  expect(() => requireFreshAuth(uat0)).toThrow(/not signed in|re-?run|capture-auth/i);

  // (b) __client_uat missing entirely → must throw.
  const noUat = path.join(dir, 'nouat.json');
  fs.writeFileSync(noUat, JSON.stringify({ cookies: [{ name: 'other', value: 'x' }] }));
  expect(() => requireFreshAuth(noUat)).toThrow(/MISSING|not signed in|capture-auth/i);

  // (c) file absent → must throw with the capture command.
  expect(() => requireFreshAuth(path.join(dir, 'does-not-exist.json'))).toThrow(/Auth state not found|capture-auth/i);

  // (d) a signed-in session (uat != 0) must NOT throw.
  const good = path.join(dir, 'good.json');
  fs.writeFileSync(good, JSON.stringify({ cookies: [{ name: '__client_uat', value: '1781968679' }] }));
  expect(() => requireFreshAuth(good)).not.toThrow();
});
