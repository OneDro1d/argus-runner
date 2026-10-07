package obsquery

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// / PushMetrics deletes THIS instance's old per-run groups.

// fakeGroup is one group the fake Pushgateway lists.
type fakeGroup struct {
	job, instance, runID string
	age                  time.Duration // how long ago it was last pushed
}

type fakePGW struct {
	mu         sync.Mutex
	groups     []fakeGroup
	listStatus int // 0 = 200
	delStatus  int // 0 = 202
	deletes    []string
	puts       []string
	srv        *httptest.Server
}

func newFakePGW(t *testing.T, groups []fakeGroup) *fakePGW {
	t.Helper()
	f := &fakePGW{groups: groups}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/metrics":
			if f.listStatus != 0 {
				w.WriteHeader(f.listStatus)
				return
			}
			var items []string
			for _, g := range f.groups {
				labels := fmt.Sprintf(`"instance":%q,"job":%q`, g.instance, g.job)
				if g.runID != "" {
					labels += fmt.Sprintf(`,"run_id":%q`, g.runID)
				}
				ts := float64(time.Now().Add(-g.age).UnixNano()) / 1e9
				items = append(items, fmt.Sprintf(`{"labels":{%s},"push_time_seconds":{"time_stamp":"x","type":"GAUGE","metrics":[{"labels":{},"value":"%.3f"}]}}`, labels, ts))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":[` + strings.Join(items, ",") + `]}`))
		case r.Method == http.MethodDelete:
			f.deletes = append(f.deletes, r.URL.Path)
			if f.delStatus != 0 {
				w.WriteHeader(f.delStatus)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut:
			f.puts = append(f.puts, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func pruneRep() *report.Report {
	return &report.Report{RunID: "run-new", Layers: []report.Layer{{Layer: "permissions", Scenarios: []report.ScenarioResult{
		{ID: "PERM-001", Status: "passed", DurationMs: 10},
	}}}}
}

func pruneFixture() []fakeGroup {
	return []fakeGroup{
		{"argus", "lab", "run-old", 2 * time.Hour},     // old, this instance: DELETE
		{"argus", "lab", "run-old2", 40 * time.Minute}, // old, this instance: DELETE
		{"argus", "lab", "run-new", 2 * time.Hour},     // the current run (clock oddity): KEEP
		{"argus", "lab", "run-young", 2 * time.Minute}, // inside the window: KEEP
		{"argus", "lab", "", 2 * time.Hour},            // the instance-only marker group: KEEP
		{"argus", "other", "run-old", 2 * time.Hour},   // another instance: KEEP
		{"othrjob", "lab", "run-old", 2 * time.Hour},   // another job: KEEP
	}
}

func TestPushMetrics_PrunesOldGroupsOfThisInstanceOnly(t *testing.T) {
	f := newFakePGW(t, pruneFixture())
	if err := PushMetrics(f.srv.URL, "lab", "p", "c", pruneRep(), 15*time.Minute); err != nil {
		t.Fatalf("push: %v", err)
	}
	want := []string{
		"/metrics/job/argus/instance/lab/run_id/run-old",
		"/metrics/job/argus/instance/lab/run_id/run-old2",
	}
	got := map[string]bool{}
	for _, d := range f.deletes {
		got[d] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("old group not deleted: %s (deletes: %v)", w, f.deletes)
		}
	}
	if len(f.deletes) != len(want) {
		t.Errorf("exactly the old groups of this instance must be deleted; deletes = %v", f.deletes)
	}
}

func TestPushMetrics_KeepsCurrentRunAndYoungGroups(t *testing.T) {
	f := newFakePGW(t, pruneFixture())
	_ = PushMetrics(f.srv.URL, "lab", "p", "c", pruneRep(), 15*time.Minute)
	for _, d := range f.deletes {
		if strings.HasSuffix(d, "/run_id/run-new") {
			t.Errorf("the current run's group was deleted: %s", d)
		}
		if strings.HasSuffix(d, "/run_id/run-young") {
			t.Errorf("a group inside the retention window was deleted: %s", d)
		}
		if d == "/metrics/job/argus/instance/lab" {
			t.Errorf("the instance-only marker group (argus_last_run_timestamp) was deleted: %s", d)
		}
	}
}

func TestPushMetrics_NeverTouchesAnotherInstanceOrJob(t *testing.T) {
	f := newFakePGW(t, pruneFixture())
	_ = PushMetrics(f.srv.URL, "lab", "p", "c", pruneRep(), 15*time.Minute)
	for _, d := range f.deletes {
		if !strings.HasPrefix(d, "/metrics/job/argus/instance/lab/run_id/") {
			t.Errorf("a delete outside this job+instance: %s", d)
		}
	}
}

func TestPushMetrics_RetentionZeroDeletesNothingAndDoesNotList(t *testing.T) {
	f := newFakePGW(t, pruneFixture())
	if err := PushMetrics(f.srv.URL, "lab", "p", "c", pruneRep(), 0); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.deletes) != 0 {
		t.Errorf("retention 0 keeps groups for ever, but deleted %v", f.deletes)
	}
	if len(f.puts) != 2 {
		t.Errorf("the pushes themselves must be unchanged (2 PUTs), got %v", f.puts)
	}
}

func TestPushMetrics_ListFailureDoesNotFailThePush(t *testing.T) {
	f := newFakePGW(t, pruneFixture())
	f.listStatus = http.StatusInternalServerError
	if err := PushMetrics(f.srv.URL, "lab", "p", "c", pruneRep(), 15*time.Minute); err != nil {
		t.Fatalf("a failed list must not fail the push: %v", err)
	}
	if len(f.deletes) != 0 {
		t.Errorf("no list, no deletes; got %v", f.deletes)
	}
}

func TestPushMetrics_DeleteFailureDoesNotFailThePush(t *testing.T) {
	f := newFakePGW(t, pruneFixture())
	f.delStatus = http.StatusInternalServerError
	if err := PushMetrics(f.srv.URL, "lab", "p", "c", pruneRep(), 15*time.Minute); err != nil {
		t.Fatalf("a failed delete must not fail the push: %v", err)
	}
	if len(f.deletes) == 0 {
		t.Error("the deletes were never attempted")
	}
}
