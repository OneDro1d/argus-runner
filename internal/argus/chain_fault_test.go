package argus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/fault"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The fault chain step as the RUN sees it: the allowlist door, the end-of-chain restore on
// every path, and a restore that never touches the verdict. internal/fault pins the manager itself.

type runKube struct {
	mu      sync.Mutex
	replica map[string]int
	calls   []string
	failSet error
}

func (k *runKube) GetScale(_ context.Context, ns, n string) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.replica[ns+"/"+n], nil
}
func (k *runKube) SetScale(_ context.Context, ns, n string, r int) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.failSet != nil {
		return k.failSet
	}
	k.calls = append(k.calls, "scale "+n)
	k.replica[ns+"/"+n] = r
	return nil
}
func (k *runKube) Ready(_ context.Context, ns, n string) (int, int, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.replica[ns+"/"+n], k.replica[ns+"/"+n], true, nil
}
func (k *runKube) up() int { k.mu.Lock(); defer k.mu.Unlock(); return k.replica["calm-demo/trades"] }

type runToxi struct {
	mu     sync.Mutex
	toxics []string
	calls  []string
}

func (x *runToxi) ProxyEnabled(context.Context, string) (bool, error) { return true, nil }
func (x *runToxi) SetEnabled(context.Context, string, bool) error     { return nil }
func (x *runToxi) AddToxic(_ context.Context, _ string, t fault.Toxic) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.calls = append(x.calls, "add")
	x.toxics = append(x.toxics, t.Name)
	return nil
}
func (x *runToxi) DeleteToxic(_ context.Context, _, n string) (bool, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.calls = append(x.calls, "del")
	for i, t := range x.toxics {
		if t == n {
			x.toxics = append(x.toxics[:i], x.toxics[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func faultCfg() *config.Config {
	return &config.Config{FaultAllowedTargets: &config.FaultAllow{
		Deployments: map[string]config.FaultDeployment{"trades": {Namespace: "calm-demo"}},
		Links:       map[string]config.FaultLink{"mcp-to-trades": {ToxiproxyAPI: "http://toxiproxy.calm-demo:8474", Proxy: "trades"}},
	}}
}

func withFakes(t *testing.T) (*runKube, *runToxi) {
	t.Helper()
	k := &runKube{replica: map[string]int{"calm-demo/trades": 2}}
	x := &runToxi{}
	oldK, oldX := newFaultKube, newFaultToxi
	newFaultKube = func() (fault.Kube, error) { return k, nil }
	newFaultToxi = func(string) fault.Toxi { return x }
	t.Cleanup(func() { newFaultKube, newFaultToxi = oldK, oldX })
	return k, x
}

// a SUT whose /ok answers 200 while the "deployment" has replicas and 503 when it has none.
func sutFor(k *runKube) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k.up() == 0 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
}

func faultRun(t *testing.T, c *config.Config, base, trig, expect string, journal string) report.ScenarioResult {
	t.Helper()
	s := scenario.Parse(chainRunMD(base, trig, expect))
	return runChainScenarioWithFaults(c, s, "tr-fault-1", "testkit/ui", nil, nil, nil, faultEnv{JournalDir: journal})
}

func journalFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	return out
}

const stopReadTrig = `{"steps":[
 {"type":"fault","name":"cut","op":"stop","deployment":"trades"},
 {"type":"http","name":"while-down","method":"GET","url":"${BASE}/ok"}
]}`

func TestChainFault_RestoresAtTheEndOfAPassingChain(t *testing.T) {
	k, _ := withFakes(t)
	srv := sutFor(k)
	defer srv.Close()
	j := filepath.Join(t.TempDir(), "j")
	res := faultRun(t, faultCfg(), srv.URL, stopReadTrig, "### Runnable\n- step while-down: status=503\n", j)
	if res.Status != "passed" {
		t.Fatalf("status %q: %+v %+v", res.Status, res.Failure, res.Steps)
	}
	if k.up() != 2 {
		t.Fatalf("the deployment was left at %d replicas after a PASSING chain", k.up())
	}
	if len(res.Faults) != 1 || res.Faults[0].Op != "stop" || res.Faults[0].Target != "trades" || res.Faults[0].RestoreOutcome != report.FaultRestored {
		t.Fatalf("faults on the row: %+v", res.Faults)
	}
	if len(journalFiles(t, j)) != 0 {
		t.Fatal("undo journal not emptied")
	}
}

func TestChainFault_RestoresAtTheEndOfAFailedChain_AndTheVerdictIsTheChains(t *testing.T) {
	k, _ := withFakes(t)
	srv := sutFor(k)
	defer srv.Close()
	j := filepath.Join(t.TempDir(), "j")
	// the claim is wrong (the SUT answers 503 while down), so the chain FAILS
	res := faultRun(t, faultCfg(), srv.URL, stopReadTrig, "### Runnable\n- step while-down: status=200\n", j)
	if res.Status != "failed" {
		t.Fatalf("want failed, got %q", res.Status)
	}
	if k.up() != 2 {
		t.Fatal("a FAILED chain left the deployment stopped")
	}
}

func TestChainFault_RestoreNeverChangesTheVerdict_AndAFailedRestoreIsLoud(t *testing.T) {
	k, _ := withFakes(t)
	srv := sutFor(k)
	defer srv.Close()
	fault.RestoreBackoff = time.Millisecond
	j := filepath.Join(t.TempDir(), "j")
	// let the stop through, then make every later scale (the restore) fail
	k.mu.Lock()
	k.failSet = nil
	k.mu.Unlock()
	oldK := newFaultKube
	newFaultKube = func() (fault.Kube, error) { return &flipKube{runKube: k}, nil }
	defer func() { newFaultKube = oldK }()
	res := faultRun(t, faultCfg(), srv.URL, stopReadTrig, "### Runnable\n- step while-down: status=503\n", j)
	if res.Status != "passed" {
		t.Fatalf("a red restore changed a passing verdict: %q", res.Status)
	}
	if len(res.Faults) != 1 || res.Faults[0].RestoreOutcome != report.FaultRestoreFailed {
		t.Fatalf("a failed restore must be recorded on the run: %+v", res.Faults)
	}
	if !strings.Contains(res.Residue, "FAULT NOT RESTORED") || !strings.Contains(res.Residue, "trades") {
		t.Fatalf("a failed restore must be loud on the row: %q", res.Residue)
	}
	if len(journalFiles(t, j)) != 1 {
		t.Fatal("the undo of a failed restore must stay journaled for the next start")
	}
}

// flipKube lets the first SetScale through and fails every later one.
type flipKube struct {
	*runKube
	n int
}

func (f *flipKube) SetScale(ctx context.Context, ns, name string, r int) error {
	f.n++
	if f.n > 1 {
		return errors.New("forbidden")
	}
	return f.runKube.SetScale(ctx, ns, name, r)
}

func TestChainFault_NoAllowlistRefusesTheWholeChainBeforeAnythingIsApplied(t *testing.T) {
	k, _ := withFakes(t)
	srv := sutFor(k)
	defer srv.Close()
	j := filepath.Join(t.TempDir(), "j")
	res := faultRun(t, &config.Config{}, srv.URL, stopReadTrig, "### Runnable\n- step while-down: status=503\n", j)
	if res.Status == "passed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "fault_allowed_targets") || !strings.Contains(res.Failure.Observed, "preflight") {
		t.Fatalf("want a preflight refusal naming fault_allowed_targets: %+v", res)
	}
	if len(k.calls) != 0 || k.up() != 2 || len(res.Steps) != 0 {
		t.Fatal("something ran although the allowlist is absent")
	}
}

func TestChainFault_AnUnlistedTargetLaterInTheChainRefusesTheChainBeforeTheFirstFault(t *testing.T) {
	k, _ := withFakes(t)
	srv := sutFor(k)
	defer srv.Close()
	trig := `{"steps":[
 {"type":"fault","name":"cut","op":"stop","deployment":"trades"},
 {"type":"fault","name":"cut2","op":"stop","deployment":"payments"}
]}`
	j := filepath.Join(t.TempDir(), "j")
	res := faultRun(t, faultCfg(), srv.URL, trig, "### Runnable\n", j)
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, `"payments"`) {
		t.Fatalf("want a refusal naming the unlisted entry: %+v", res.Failure)
	}
	if len(k.calls) != 0 || k.up() != 2 {
		t.Fatalf("the listed fault was applied before the later refusal: %v", k.calls)
	}
}

func TestChainFault_RefusalNeverEchoesTheToxiproxyAddress(t *testing.T) {
	withFakes(t)
	c := faultCfg()
	c.FaultAllowedTargets.Links["mcp-to-trades"] = config.FaultLink{ToxiproxyAPI: "http://toxiproxy.calm-demo:8474", Proxy: "trades"}
	trig := `{"steps":[{"type":"fault","name":"x","op":"delay","link":"nope","latency_ms":10}]}`
	res := faultRun(t, c, "http://unused", trig, "### Runnable\n", filepath.Join(t.TempDir(), "j"))
	if res.Failure == nil || strings.Contains(res.Failure.Observed, "toxiproxy.calm-demo") || strings.Contains(res.Failure.Observed, "8474") {
		t.Fatalf("%+v", res.Failure)
	}
}

func TestChainFault_WithoutAJournalDirectoryNothingIsApplied(t *testing.T) {
	k, _ := withFakes(t)
	s := scenario.Parse(chainRunMD("http://unused", stopReadTrig, "### Runnable\n- step while-down: status=503\n"))
	res := runChainScenario(faultCfg(), s, "tr-nojournal", "testkit/ui")
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "journal") || len(k.calls) != 0 {
		t.Fatalf("a fault with nowhere to keep its undo must be refused: %+v calls=%v", res.Failure, k.calls)
	}
}

func TestChainFault_ALinkDelayIsAddedAndRemovedAndRecordedWithToxiproxy(t *testing.T) {
	_, x := withFakes(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	trig := `{"steps":[
 {"type":"fault","name":"slow","op":"delay","link":"mcp-to-trades","latency_ms":5000},
 {"type":"http","name":"read","method":"GET","url":"${BASE}/x"}
]}`
	res := faultRun(t, faultCfg(), srv.URL, trig, "### Runnable\n- step read: status=200\n", filepath.Join(t.TempDir(), "j"))
	if res.Status != "passed" || strings.Join(x.calls, ",") != "add,del" {
		t.Fatalf("status %q toxi calls %v", res.Status, x.calls)
	}
	if len(res.Faults) != 1 || !res.Faults[0].Toxiproxy || res.Faults[0].Params["latency_ms"] != 5000 {
		t.Fatalf("%+v", res.Faults)
	}
}

func TestChainFault_AClaimOnAFaultStepIsRefusedAtRunTimeToo(t *testing.T) {
	k, _ := withFakes(t)
	trig := `{"steps":[{"type":"fault","name":"cut","op":"stop","deployment":"trades"}]}`
	res := faultRun(t, faultCfg(), "http://unused", trig, "### Runnable\n- step cut: status=200\n", filepath.Join(t.TempDir(), "j"))
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "claim") || len(k.calls) != 0 {
		t.Fatalf("%+v", res.Failure)
	}
}

func TestSummarize_CollectsEveryScenariosFaultsOntoTheRunRecord_AndOmitsThemWhenThereAreNone(t *testing.T) {
	f := report.Fault{Step: "cut", Op: "stop", Target: "trades", AppliedAt: "2026-10-09T10:00:00Z", RestoreOutcome: report.FaultRestored}
	rep := summarize("p", map[string][]report.ScenarioResult{"a": {{ID: "A", Status: "passed", Faults: []report.Fault{f}}, {ID: "B", Status: "passed"}}})
	if len(rep.Faults) != 1 || rep.Faults[0].Target != "trades" {
		t.Fatalf("run record faults: %+v", rep.Faults)
	}
	plain := summarize("p", map[string][]report.ScenarioResult{"a": {{ID: "A", Status: "passed"}}})
	b, _ := json.Marshal(plain)
	if strings.Contains(string(b), "faults") {
		t.Fatalf("a run with no fault must not grow a faults key: %s", b)
	}
}

func TestChainFault_ARestoreStepEmptiesTheSetMidChain(t *testing.T) {
	k, _ := withFakes(t)
	srv := sutFor(k)
	defer srv.Close()
	trig := `{"steps":[
 {"type":"fault","name":"cut","op":"stop","deployment":"trades"},
 {"type":"http","name":"down","method":"GET","url":"${BASE}/ok"},
 {"type":"fault","name":"heal","op":"restore"},
 {"type":"http","name":"up","method":"GET","url":"${BASE}/ok"}
]}`
	res := faultRun(t, faultCfg(), srv.URL, trig, "### Runnable\n- step down: status=503\n- step up: status=200\n", filepath.Join(t.TempDir(), "j"))
	if res.Status != "passed" {
		t.Fatalf("%q %+v %+v", res.Status, res.Failure, res.Steps)
	}
}
