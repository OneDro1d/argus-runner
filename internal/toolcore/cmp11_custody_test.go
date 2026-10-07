package toolcore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
)

// ARGUS-CMP-11 custody: a tolerant number (a field of a response) is recorded on the executor and goes nowhere else from
// toolcore.Run: not to the Pushgateway or Loki (a compare run sends no per-check telemetry), and not to the product hat's
// report (verdict only for a compare run).

func TestCMP11_ATolerantCanaryNumberIsNeverPushedAndNeverInTheProductHatsReportOfACompareRun(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	const canary = "987654321.125"
	var bodies []string
	var hits int32
	record := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		bodies = append(bodies, r.URL.String()+string(buf[:n]))
		w.WriteHeader(http.StatusNoContent)
	}
	pg := httptest.NewServer(http.HandlerFunc(record))
	lk := httptest.NewServer(http.HandlerFunc(record))
	defer pg.Close()
	defer lk.Close()
	sut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"total":` + canary + `}`))
	}))
	defer sut.Close()
	md := strings.Join([]string{
		"# Scenario: c", "", "## Metadata", "- **ID**: CHN-TOL", "- **Layer**: Permissions", "- **Tags**: chain", "",
		"## TRIGGER", "POST `chain`", "", "```json",
		`{"steps":[{"type":"http","name":"read","method":"GET","url":"` + sut.URL + `/total"}]}`, "```", "",
		"## EXPECT", "### Runnable", "- step read: status=200", "", "## TIMEOUT", "60s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		"## COMPARE", "- **Reference**: measured", "- **Output**: status, body", "- **Tolerance**: $.total abs 0.01", "",
	}, "\n")
	e := rlEnv(t, "project:\n  name: t\n", map[string]string{"CHN-TOL": md})
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"
	e.Pushgateway, e.Loki = pg.URL, lk.URL
	e.RunMode = "compare"
	if _, _, err := Run(e, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("a compare run pushed %d request(s) to Pushgateway/Loki: %v", hits, bodies)
	}
	// the executor's own report has the number (the evidence hash commits to it): the positive control
	own, _, err := GetReport(e, role.Test, "")
	if err != nil {
		t.Fatal(err)
	}
	ob, _ := json.Marshal(own)
	if !strings.Contains(string(ob), canary) {
		t.Fatalf("control: the author's own copy of the report lacks the recorded number: %s", ob)
	}
	prod, _, err := GetReport(e, role.Product, "")
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := json.Marshal(prod)
	if strings.Contains(string(pb), canary) || strings.Contains(string(pb), "CHN-TOL") || strings.Contains(string(pb), `"values"`) {
		t.Errorf("the product hat's report of a compare run carries per-check data: %s", pb)
	}
}
