package toolcore

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
)

// VR12-E14 — TIER 3 IS HOLDOUT MATERIAL, AND A NEW REPORT FIELD IS NOT COVERED BY ACCIDENT.
//
// report.ScenarioResult.Unexecuted quotes the author's bullet VERBATIM — that is the point of tier 3
// (an author must be able to find the line in their own file) and it is also exactly why a product
// agent must never see it: "body has error containing \"missing or invalid bearer token\"" states
// what the test checks, in the test's own words.
//
// The dark-factory holdout is stated in the spec itself, specs/06-scenarios-orderservice.md:11 —
// "The agent that fixes a failing scenario never reads it … only the report is shared back." This
// test exists because the redaction is field-by-field: adding a field that carries expectations and
// forgetting this function is a silent leak, and a silent leak is what this whole round is about.
func TestVR12E14_UnexecutedIsRedactedForTheProductHat(t *testing.T) {
	mk := func() *report.Report {
		return &report.Report{Layers: []report.Layer{{
			Scenarios: []report.ScenarioResult{{
				ID: "T-001", Status: "passed",
				Unexecuted: []report.Unexecuted{{
					Bullet: `body has error containing "missing or invalid bearer token"`,
					Reason: "a body assertion is only evaluated by the http-ingestion template",
				}},
			}},
		}}}
	}

	t.Run("the TEST hat keeps it — it is the author's own signal", func(t *testing.T) {
		rep := mk()
		redactExpected(rep, role.Test)
		if len(rep.Layers[0].Scenarios[0].Unexecuted) != 1 {
			t.Fatal("the test hat must keep the unexecuted list; without it tier 3 tells nobody anything")
		}
	})

	t.Run("⛔ the PRODUCT hat must not see it", func(t *testing.T) {
		rep := mk()
		redactExpected(rep, role.Product)
		if got := rep.Layers[0].Scenarios[0].Unexecuted; got != nil {
			t.Fatalf("tier 3 quotes the test's expectation verbatim — it must be redacted: %+v", got)
		}
	})
}
