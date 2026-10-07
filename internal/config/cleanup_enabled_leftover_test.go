package config

import "testing"

// V29-015 / round 12 — WHAT HAPPENS TO A LEFTOVER `scenarios.cleanup_enabled`.
//
// The key was DELETED from the template, the how-to and the schema. Every config an operator wrote
// before 0.3.31 still carries it, so the onboarding docs have to answer one question honestly: is
// the leftover key REFUSED, or IGNORED?
//
// It is IGNORED. Strictness (KnownFields) is scoped to `targets` (VR10-S3) and `rate_limit`
// (VR10-R1); everything else is absorbed into strictTargetsDoc.Rest and reaches nothing. The
// schema's own comment says as much of `rate_limit`: `additionalProperties: false` there documents
// a refusal that lives in CODE, and there is no such code for `scenarios`.
//
// This test exists because the Confluence page "How to write your argus-config.yaml" (691699714)
// and onboarding/HOW-TO-ARGUS-CONFIG.md tell operators what to do with the leftover key. A draft of
// that sentence claimed it was "refused by name when the file loads" — which would have been a
// second dead control documented as enforced, the exact shape of the finding V29-015 closed. The
// sentence now says the key is inert and should be deleted; this test is what keeps it true. If
// someone later widens strictness to the whole document, THIS TEST FAILS FIRST and the docs get
// corrected in the same commit.
func TestScenarios_LeftoverCleanupEnabledIsToleratedNotRefused(t *testing.T) {
	body := `
project:
  name: leftover-probe
targets:
  http:
    base_url: http://api:8080
scenarios:
  timeout_default: 20s
  cleanup_enabled: true
observability:
  loki:
    url: http://loki:3100
`
	c, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("a leftover scenarios.cleanup_enabled must still PARSE (the docs say it is inert, "+
			"not refused) — if this build now refuses it, fix onboarding/HOW-TO-ARGUS-CONFIG.md and "+
			"Confluence 691699714 in the same commit: %v", err)
	}
	if c == nil {
		t.Fatal("Load returned no config and no error")
	}
}
