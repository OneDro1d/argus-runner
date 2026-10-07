package argus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// multiSampleRunner writes a JMeter .jtl with SEVERAL rows (AC-11: a load scenario's percentiles
// need more than one sample to be meaningful) — elapsedMs[i] / codes[i] are index-aligned.
type multiSampleRunner struct {
	elapsedMs []int
	codes     []int
}

func (f *multiSampleRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	id := props["scenario.id"]
	var b strings.Builder
	b.WriteString("timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n")
	for i, e := range f.elapsedMs {
		code := f.codes[i]
		succ := "true"
		msg := "OK"
		if code >= 400 {
			succ, msg = "false", "Error"
		}
		fmt.Fprintf(&b, "%d,%d,%s,%d,%s,%s %s 1-%d,%s,\n",
			1781024939842+int64(i), e, id, code, msg, templateBase, id, i+1, succ)
	}
	return os.WriteFile(jtlPath, []byte(b.String()), 0o644)
}

func loadScenarioMD(id, loadBody string) string {
	return strings.Join([]string{
		"# Scenario: " + id, "", "## Metadata", "- **ID**: " + id,
		"- **Layer**: HTTP Ingestion", "", "## TRIGGER",
		"POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
		"## EXPECT", "### Runnable", "- status=202", "",
		"## LOAD", loadBody, "",
	}, "\n")
}

const loadBodyGenerous = "" +
	"- **Users**: 20\n- **Ramp Seconds**: 5\n- **Duration Seconds**: 30\n" +
	"- **Target P95 Ms**: 1000\n- **Max Error Rate**: 0.5"

const loadBodyStrict = "" +
	"- **Users**: 20\n- **Ramp Seconds**: 5\n- **Duration Seconds**: 30\n" +
	"- **Target P95 Ms**: 50\n- **Max Error Rate**: 0.01"

// AC-11: percentiles + error rate are computed from the fixture JTL and attached to the scenario's
// report row, quoted with exact nearest-rank values over a known sample set.
func TestRunAll_LoadProfileReportsPercentilesFromFixtureJTL(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenarioMD(t, scDir, "http-ingestion", "LOAD-001", loadScenarioMD("LOAD-001", loadBodyGenerous))

	// 10 samples, elapsed 10..100ms in steps of 10, all 2xx: p50=50 (rank 5), p95=100 (rank 10),
	// p99=100 (rank 10), error_rate=0.
	elapsed := []int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	codes := make([]int, 10)
	for i := range codes {
		codes[i] = 202
	}
	fr := &multiSampleRunner{elapsedMs: elapsed, codes: codes}
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}

	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", fr)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("LOAD-001")
	if res == nil {
		t.Fatal("LOAD-001 not found in report")
	}
	if res.Status != "passed" {
		t.Fatalf("status = %q, want passed (target is generous): %+v", res.Status, res)
	}
	if res.Load == nil {
		t.Fatal("a load-profile scenario must carry Load stats")
	}
	if res.Load.Samples != 10 {
		t.Errorf("samples = %d, want 10", res.Load.Samples)
	}
	if res.Load.P50Ms != 50 {
		t.Errorf("p50 = %d, want 50", res.Load.P50Ms)
	}
	if res.Load.P95Ms != 100 {
		t.Errorf("p95 = %d, want 100", res.Load.P95Ms)
	}
	if res.Load.P99Ms != 100 {
		t.Errorf("p99 = %d, want 100", res.Load.P99Ms)
	}
	if res.Load.ErrorRate != 0 {
		t.Errorf("error_rate = %v, want 0", res.Load.ErrorRate)
	}
	if res.Load.Breached {
		t.Errorf("a generous target must not be breached: %+v", res.Load)
	}
	if b, err := json.MarshalIndent(res, "", "  "); err == nil {
		t.Logf("LOAD-001 report row:\n%s", b)
	}
}

// AC-11: a measured p95 or error rate past the scenario's OWN declared targets surfaces as `failed`
// — the load SLA is the scenario's own claim, distinct from DEGRADED (a survival-plane signal).
func TestRunAll_LoadProfileBreachSurfacesAsFailed(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenarioMD(t, scDir, "http-ingestion", "LOAD-002", loadScenarioMD("LOAD-002", loadBodyStrict))

	elapsed := []int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100} // p95=100 > target 50
	codes := make([]int, 10)
	for i := range codes {
		codes[i] = 202
	}
	fr := &multiSampleRunner{elapsedMs: elapsed, codes: codes}
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}

	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", fr)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("LOAD-002")
	if res == nil {
		t.Fatal("LOAD-002 not found in report")
	}
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed (p95 100 > target 50): %+v", res.Status, res)
	}
	if res.Load == nil || !res.Load.Breached {
		t.Fatalf("Load.Breached must be true: %+v", res.Load)
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "load target breached") {
		t.Errorf("Failure.Observed must name the breach, got %+v", res.Failure)
	}
	if rr.Report.Summary.Failed != 1 {
		t.Errorf("Summary.Failed = %d, want 1", rr.Report.Summary.Failed)
	}
}

// A scenario with NO `## LOAD` never carries Load stats — the percentile machinery is inert
// without a declared profile, exactly like every other AC-11 addition.
func TestRunAll_NoLoadProfileNoLoadStats(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")

	fr := &fakeRunner{pass: map[string]bool{"ORD-001": true}}
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", fr)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("ORD-001")
	if res == nil {
		t.Fatal("ORD-001 not found")
	}
	if res.Load != nil {
		t.Errorf("a scenario with no ## LOAD must carry nil Load, got %+v", res.Load)
	}
}
