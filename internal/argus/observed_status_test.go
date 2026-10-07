package argus

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// — a passing HTTP/JMeter scenario's report never showed the HTTP status it got, and
// `assertions_enforced_count: 0` (the status line is enforced but excluded from that count BY DESIGN,
// enforced.go) read as "nothing was checked". A test agent concluded live that status=200 was never
// checked. The report now carries the OBSERVED code — from the run's own result (the .jtl
// responseCode), never from the expected value — on a pass AND on a fail.

// pass: the declared status is met; the observed code is on the row and the count keeps its meaning.
func TestObservedStatus_OnAPassingHTTPScenario(t *testing.T) {
	res := runOneHTTP(t, "http-ingestion", "HTTP Ingestion", "OBS-001", []string{"status=201"},
		&statusBodyRunner{code: "201", bodyOK: true})
	if res.Status != "passed" {
		t.Fatalf("setup: want passed, got %s %+v", res.Status, res.Failure)
	}
	if res.ObservedStatus != 201 {
		t.Errorf("ObservedStatus = %d, want 201 (the code the SUT answered)", res.ObservedStatus)
	}
	// the count's meaning is unchanged: a status line is not a content check
	if res.AssertionsEnforcedCount != 0 {
		t.Errorf("assertions_enforced_count = %d, want 0 — the status line stays out of the count", res.AssertionsEnforcedCount)
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"observed_status":201`) {
		t.Errorf("report JSON must carry \"observed_status\":201, got %s", b)
	}
}

// fail: the SUT answered 500 against a declared 200. The code on the row is the 500 that came back,
// not the 200 that was expected — it is read from the run, never from the scenario.
func TestObservedStatus_OnAFailingHTTPScenario_IsTheObservedNotTheExpected(t *testing.T) {
	res := runOneHTTP(t, "http-ingestion", "HTTP Ingestion", "OBS-002", []string{"status=200"},
		&statusBodyRunner{code: "500", bodyOK: true})
	if res.Status != "failed" {
		t.Fatalf("setup: want failed, got %s %+v", res.Status, res.Failure)
	}
	if res.ObservedStatus != 500 {
		t.Errorf("ObservedStatus = %d, want 500 (observed), not the expected 200", res.ObservedStatus)
	}
}

// a body miss on a code that matched: the verdict is the content's, the observed code is still the SUT's.
func TestObservedStatus_OnABodyMiss(t *testing.T) {
	res := runOneHTTP(t, "http-ingestion", "HTTP Ingestion", "OBS-003",
		[]string{"status=200", "body has note containing x"}, &statusBodyRunner{code: "200", bodyOK: false})
	if res.Status != "failed" {
		t.Fatalf("setup: want failed, got %s", res.Status)
	}
	if res.ObservedStatus != 200 {
		t.Errorf("ObservedStatus = %d, want 200", res.ObservedStatus)
	}
}

// a transport failure has no HTTP status: code 0 is not a status and is not reported as one.
func TestObservedStatus_AbsentWhenTheSUTNeverAnswered(t *testing.T) {
	res := runOneHTTP(t, "http-ingestion", "HTTP Ingestion", "OBS-004", []string{"status=200"},
		&statusBodyRunner{code: "Non HTTP response code: java.net.ConnectException", bodyOK: true})
	if res.ObservedStatus != 0 {
		t.Errorf("ObservedStatus = %d, want 0 (absent) — a connection failure has no status", res.ObservedStatus)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "observed_status") {
		t.Errorf("no observed_status key may be emitted when nothing answered: %s", b)
	}
}

// an idempotency pair fires two SUT-trigger samples: one number would hide the other, so the list rides along.
func TestObservedStatusFrom(t *testing.T) {
	for _, c := range []struct {
		name    string
		in      []int
		first   int
		wantAll []int
	}{
		{"one sample", []int{204}, 204, nil},
		{"a pair keeps both", []int{202, 409}, 202, []int{202, 409}},
		{"transport failure only", []int{0}, 0, nil},
		{"none", nil, 0, nil},
		{"leading transport failure then an answer", []int{0, 503}, 503, []int{0, 503}},
		{"a repeated code is one number", []int{200, 200}, 200, nil},
		{"a load run keeps distinct codes once, first-seen order", loadCodes(), 200, []int{200, 503, 429}},
	} {
		first, all := observedStatusFrom(c.in)
		if first != c.first || fmt.Sprint(all) != fmt.Sprint(c.wantAll) {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", c.name, first, all, c.first, c.wantAll)
		}
	}
	// the list is bounded however many distinct codes arrive
	var many []int
	for c := 100; c < 140; c++ {
		many = append(many, c)
	}
	if _, all := observedStatusFrom(many); len(all) != maxObservedStatusCodes {
		t.Errorf("distinct codes not capped: got %d, want %d", len(all), maxObservedStatusCodes)
	}
}

// loadCodes is a 10,000-sample load run: mostly 200, some 503, a few 429.
func loadCodes() []int {
	out := make([]int, 0, 10000)
	for i := 0; i < 10000; i++ {
		switch {
		case i%97 == 5:
			out = append(out, 503)
		case i%1000 == 999:
			out = append(out, 429)
		default:
			out = append(out, 200)
		}
	}
	return out
}

var _ = report.ScenarioResult{}
