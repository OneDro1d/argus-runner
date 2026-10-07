/**
 * VR-L3 regression (blind-gate finding 2026-06-21): a spec where every test is skipped
 * exits 0 and produces NO per-test results artifact — the SUT is never exercised. Per
 * VR-L3 / UC-62 ("absent results != pass, fail closed") this MUST be reported as a
 * DISTINCT execution failure, NOT a silent green. Drives the exit-0-no-artifact path.
 */
import { test } from '@playwright/test';

test.skip('skipped: the SUT is never exercised — must NOT be reported green', async () => {
  // intentionally never runs
});
