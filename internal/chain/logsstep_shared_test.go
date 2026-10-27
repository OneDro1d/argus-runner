package chain

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// A logs step against an environment's SHARED Loki: the query it sends, and that "no stream"
// is never read as "zero lines".

func sharedLoki(f *fakeLoki) *obsquery.Loki {
	l := f.loki()
	l.Shared = &obsquery.SharedLogSource{Cluster: "cluster-a", Namespace: "sut-ns",
		Constants: map[string]string{"argus_instance": "i", "project": "p"},
		Format:    "json", LevelField: "level", CorrelationField: "correlation_id", SagaField: "event_type", SagaMarkers: []string{"saga"}}
	return l
}

func TestLogsStep_SharedLokiSendsTheClusterNamespaceSelector(t *testing.T) {
	excl := `container!~"` + obsquery.ObsInfraExclusion + `"`
	cases := []struct {
		name string
		spec LogsSpec
		want string
	}{
		{"correlation", LogsSpec{Contains: "order placed"}, `{cluster="cluster-a",namespace="sut-ns",` + excl + `} |= "` + testCID + `" |= "order placed"`},
		{"source: service becomes container, level is parsed", LogsSpec{Scope: scenario.LogsScopeSource,
			Labels: map[string]string{"service": "orders", "level": "error"}, Contains: "boom"},
			`{cluster="cluster-a",container="orders",namespace="sut-ns"} |= "boom" | json level="level" | level="error"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeLoki(t, nil, nLines(1))
			res := LogsStep("l", logsSpec(newClock(), c.spec), logsClaims(t, "body has count >= 1"), nil, nil, false, sharedLoki(f)).Run(testCID, map[string]string{})
			if res.Status != "passed" {
				t.Fatalf("%s: %q", res.Status, res.Observed)
			}
			reqs := f.requests()
			if len(reqs) != 1 || reqs[0].query != c.want {
				t.Fatalf("the query sent:\n got %v\nwant %s", reqs, c.want)
			}
			t.Logf("shared-mode %s query sent to Loki: %s", c.name, reqs[0].query)
		})
	}
}

// A step a shared Loki cannot honour is `failed` before anything is sent.
func TestLogsStep_SharedLokiRefusedLabelSendsNothing(t *testing.T) {
	f := newFakeLoki(t, nil, nLines(1))
	spec := logsSpec(newClock(), LogsSpec{Scope: scenario.LogsScopeSource, Labels: map[string]string{"cluster": "cluster-b"}, Contains: "x"})
	res := LogsStep("l", spec, logsClaims(t, "body has count == 0"), nil, nil, false, sharedLoki(f)).Run(testCID, map[string]string{})
	if res.Status != "failed" || !strings.Contains(res.Observed, "logs query could not be built") || len(f.requests()) != 0 {
		t.Fatalf("got %s %q after %d requests", res.Status, res.Observed, len(f.requests()))
	}
}

// (c) THE POINT: `count == 0` against a shared Loki must not hold because the selector matched no stream.
func TestLogsStep_SharedLokiNoStreamIsNotZeroLines(t *testing.T) {
	zero := logsClaims(t, "body has count == 0")

	t.Run("no stream at all: error, claim not judged, not passed", func(t *testing.T) {
		f := newFakeLoki(t, nil, nLines(0), nLines(0)) // the query, then the bare-selector probe
		res := LogsStep("l", logsSpec(newClock(), LogsSpec{Contains: "boom"}), zero, nil, nil, false, sharedLoki(f)).Run(testCID, map[string]string{})
		if res.Status != report.StatusError || !strings.Contains(res.Observed, "no stream at all") {
			t.Fatalf("want error / no stream at all, got %s %q", res.Status, res.Observed)
		}
		if len(res.FailedClaims) != 0 || res.Status == "passed" {
			t.Fatalf("a claim was judged on an answer that says nothing: %+v", res)
		}
	})

	t.Run("the same empty answer from an OWN Loki is a real zero and passes (unchanged)", func(t *testing.T) {
		f := newFakeLoki(t, nil, nLines(0))
		res := LogsStep("l", logsSpec(newClock(), LogsSpec{Contains: "boom"}), zero, nil, nil, false, f.loki()).Run(testCID, map[string]string{})
		if res.Status != "passed" || len(f.requests()) != 1 {
			t.Fatalf("own-Loki behaviour changed: %s %q after %d requests", res.Status, res.Observed, len(f.requests()))
		}
	})

	t.Run("the namespace is there but the line is not: a genuine zero passes", func(t *testing.T) {
		f := newFakeLoki(t, nil, nLines(0), nLines(1)) // the filtered query is empty, the probe finds a line
		res := LogsStep("l", logsSpec(newClock(), LogsSpec{Contains: "boom"}), zero, nil, nil, false, sharedLoki(f)).Run(testCID, map[string]string{})
		if res.Status != "passed" || res.Observed != "log query matched 0 line(s)" {
			t.Fatalf("want passed with 0 lines, got %s %q", res.Status, res.Observed)
		}
	})

	t.Run("a poll keeps asking while there is no stream, then errors; it never passes", func(t *testing.T) {
		f := newFakeLoki(t, nil, nLines(0))
		c := newClock()
		poll := &Poll{Timeout: 5 * 1e9, Interval: 1e9}
		res := LogsStep("l", logsSpec(c, LogsSpec{Contains: "boom"}), zero, nil, poll, false, sharedLoki(f)).Run(testCID, map[string]string{})
		if res.Status != report.StatusError || len(f.requests()) < 4 {
			t.Fatalf("want error after several polls, got %s %q after %d requests", res.Status, res.Observed, len(f.requests()))
		}
	})

	t.Run("a stream that appears while polling lets the claim be judged", func(t *testing.T) {
		// attempt 1: query empty + probe empty (no stream yet); attempt 2: query empty, probe finds a line
		f := newFakeLoki(t, nil, nLines(0), nLines(0), nLines(0), nLines(1))
		poll := &Poll{Timeout: 5 * 1e9, Interval: 1e9}
		res := LogsStep("l", logsSpec(newClock(), LogsSpec{Contains: "boom"}), zero, nil, poll, false, sharedLoki(f)).Run(testCID, map[string]string{})
		if res.Status != "passed" {
			t.Fatalf("got %s %q", res.Status, res.Observed)
		}
	})
}
