package argus

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// deadSUTRunner writes the .jtl JMeter produces when the SUT does not answer at all: responseCode 0,
// success=false, and a connection-level failureMessage. The RIG is fine — Run returns nil, exactly
// as JMeter exiting normally with failing samples — so the RO-04 dead-rig branch must NOT fire.
// This is the shape of run 20260809T172606532, where order-service had been down for 24 hours.
type deadSUTRunner struct{ msg string }

func (d *deadSUTRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	id := props["scenario.id"]
	row := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n" +
		"1781024939842,2774," + id + ",0,Non HTTP response code: java.net.UnknownHostException," +
		templateBase + " " + id + " 1-1,false," + d.msg + "\n"
	return os.WriteFile(jtlPath, []byte(row), 0o644)
}

// THE HEADLINE. An absent SUT must score ERRORED, not failed. Before this, the HTTP rollup could
// only say passed or failed, so a SUT that had been down for a day produced 33 "failed" scenarios —
// which reads as a product broken in 33 ways and sends a triager into the scenario files.
func TestRunAll_DeadSUTIsErroredNotFailed(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
	writeScenario(t, scDir, "http-ingestion", "ORD-002", "status=400")

	rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "20260101T000000",
		&deadSUTRunner{msg: "order-api: Name or service not known"})
	if err != nil {
		t.Fatal(err)
	}
	rep := rr.Report

	if rep.Summary.Errored != 2 || rep.Summary.Failed != 0 || rep.Summary.Passed != 0 {
		t.Fatalf("summary = passed:%d failed:%d errored:%d, want 0/0/2 — an absent SUT is unrun, not broken",
			rep.Summary.Passed, rep.Summary.Failed, rep.Summary.Errored)
	}
	for _, l := range rep.Layers {
		for _, s := range l.Scenarios {
			if s.Status != report.StatusError {
				t.Errorf("%s status = %q, want %q", s.ID, s.Status, report.StatusError)
			}
			if !s.NeverReachedSUT() {
				t.Errorf("%s counts contradict the status: ok=%d fail=%d err=%d",
					s.ID, s.ReqSuccess, s.ReqFailed, s.ReqError)
			}
			// The reality-only reason survives; the expectation does not, because nothing was
			// measured against it.
			if s.Failure == nil || s.Failure.Observed == "" {
				t.Errorf("%s lost its observed reason", s.ID)
			} else if s.Failure.Expected != nil {
				t.Errorf("%s carries Expected=%q on an errored scenario", s.ID, *s.Failure.Expected)
			}
		}
	}
	// And it is still RED. Errored is a different kind of non-green, never a lesser one.
	if !rep.Failed() {
		t.Fatal("a run against a dead SUT reported itself as not failed — the fix would have turned red into green")
	}
}

// A REACHABLE SUT that answers wrongly is untouched: it responded, the response was wrong, that is a
// product signal. This is the boundary that keeps the reclassification honest.
func TestRunAll_ReachableButWrongStaysFailed(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")

	rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "20260101T000000",
		&fakeRunner{pass: map[string]bool{}}) // fakeRunner emits a real 400 for a non-passing id
	if err != nil {
		t.Fatal(err)
	}
	rep := rr.Report
	if rep.Summary.Failed != 1 || rep.Summary.Errored != 0 {
		t.Fatalf("summary = failed:%d errored:%d, want 1/0 — a real 400 is a SUT response, not an infra error",
			rep.Summary.Failed, rep.Summary.Errored)
	}
	s := rep.Layers[0].Scenarios[0]
	if s.Failure == nil || s.Failure.Expected == nil {
		t.Fatalf("a genuine failure must keep its Expected for the test hat: %+v", s.Failure)
	}
}
