package toolcore

// / — report.Failure.FailedBodyCheck is TEST-HAT ONLY: the bullet is
// the author's words and carries the expected value. toolcore.GetReport is the one read every local
// builder surface goes through (the in-env runner__get_report tool, `argus get-report` under a runner
// credential, the relay's get_report before reduceReport), and RedactExpected is what the build record
// uses. The author's read of the same file is the positive control.

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
)

const (
	fbcBullet   = "body has fbcFieldSentinelK2 equals FBC-EXPECTED-SENTINEL-9Wd"
	fbcFieldTok = "fbcFieldSentinelK2"
	fbcExpected = "FBC-EXPECTED-SENTINEL-9Wd"
	fbcKey      = `"failed_body_check"`
)

func fbcToolcoreReport() *report.Report {
	exp := "status=200; " + fbcBullet
	return &report.Report{
		RunID: "run_1", Project: "p", Mode: "ci", Summary: report.Summary{Total: 1, Failed: 1},
		Layers: []report.Layer{{Layer: "HTTP Ingestion", Scenarios: []report.ScenarioResult{{
			ID: "BODY-030", Status: "failed",
			Failure: &report.Failure{
				Expected:        &exp,
				Observed:        "status matched but the response body did not satisfy the scenario's body assertion (the asserted value is held out; see failure.expected with the test hat)",
				FailedBodyCheck: &report.FailedBodyCheck{Index: 2, Bullet: fbcBullet},
			},
		}}}},
	}
}

func assertNoFailedBodyCheck(t *testing.T, path, out string) {
	t.Helper()
	for _, needle := range []string{fbcKey, fbcBullet, fbcFieldTok, fbcExpected} {
		if strings.Contains(out, needle) {
			t.Errorf("builder path %s carries %s: %s", path, needle, out)
		}
	}
}

func TestFailedBodyCheck_Builder_GetReport_BothReadPaths(t *testing.T) {
	e := fcEnv(t, fbcToolcoreReport())
	for name, runID := range map[string]string{"per-run file": "run_1", "latest report.json": ""} {
		builder := fcGet(t, e, role.Product, runID)
		// sanity: the builder DOES get this build-run report's scenarios (a withheld report would pass vacuously)
		if !strings.Contains(builder, "BODY-030") || !strings.Contains(builder, "status matched but") {
			t.Fatalf("sanity (%s): the builder must still read the scenario and its observed text: %s", name, builder)
		}
		assertNoFailedBodyCheck(t, "toolcore.GetReport(product, "+name+")", builder)
		author := fcGet(t, e, role.Test, runID)
		if !strings.Contains(author, fbcKey) || !strings.Contains(author, fbcFieldTok) || !strings.Contains(author, `"index":2`) {
			t.Errorf("positive control: the author's GetReport (%s) must carry failed_body_check: %s", name, author)
		}
	}
}

func TestFailedBodyCheck_Builder_RedactExpected(t *testing.T) {
	rep := fbcToolcoreReport()
	RedactExpected(rep, role.Product)
	if f := rep.Layers[0].Scenarios[0].Failure; f == nil || f.FailedBodyCheck != nil {
		t.Errorf("RedactExpected(product) must nil failed_body_check: %+v", f)
	}
	if got := rep.Layers[0].Scenarios[0].Failure.Observed; !strings.HasPrefix(got, "status matched but") {
		t.Errorf("observed must stay for the product hat, got %q", got)
	}
	author := fbcToolcoreReport()
	RedactExpected(author, role.Test)
	if author.Layers[0].Scenarios[0].Failure.FailedBodyCheck == nil {
		t.Errorf("RedactExpected(test) must not touch the author's record")
	}
}
