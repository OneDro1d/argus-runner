package runner

// ownwords_marker_test.go — AC-F1 (#679): the executor sends the control plane only text Argus writes itself in a
// run's failure reason and in a relayed command's error text, never the app's own words.
//
// A fake app answers every request with a unique marker — in its body, in a header, in a JSON value, in a 500's
// error text. The REAL run function and the REAL relayed-command function talk to it (or read what it answered)
// under the executor's own loop, against a fake control plane that records exactly what arrives. A run ends with a
// failure reason after the app was contacted, and one relayed command of every type is made to fail where it can.
// The marker must reach neither field — and Argus's own words must still say which step failed and why.
//
// Nothing here leaks on the code this ticket started from: the test pins the rule rather than fixing a leak. It is
// shown failing on a build that copies the app's error into either field.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/argus"
	"github.com/OneDro1d/argus-runner/internal/federation"
)

// appMarker is text only the fake app ever writes. It is no word Argus uses.
const appMarker = "kestrel-umber-7f3c-app-says"

// newMarkerApp is an app whose every answer and every error carries appMarker.
func newMarkerApp(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-App-Note", appMarker)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"` + appMarker + ` the app failed","detail":{"note":"` + appMarker + `","code":503}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// markerScenario is one chain check that calls the app and expects a 200, so the app's 500 and its body are what the
// run records about it.
func markerScenario(appURL string) string {
	return strings.Join([]string{
		"# Scenario: the app answers with its own words", "", "## Metadata", "- **ID**: CHN-ACF1", "- **Layer**: Permissions",
		"- **Tags**: chain", "",
		"## TRIGGER", "POST `chain`", "", "```json",
		`{"steps":[{"type":"http","name":"call","method":"POST","url":"` + appURL + `/things"}]}`, "```", "",
		"## EXPECT", "### Runnable", "- step call: status=200", "", "## TIMEOUT", "60s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
}

// markerRunRoot lays out a results root where a real run talks to the app, writes report.json, and then fails at an
// Argus step of its own: `runs` is a regular file, so the per-run report cannot be written (toolcore.Run's
// "write run report"). It returns the root and a config path that loads.
func markerRunRoot(t *testing.T) (root, cfgPath string) {
	t.Helper()
	root = t.TempDir()
	cfgPath = filepath.Join(root, "argus-config.yaml")
	if err := os.WriteFile(cfgPath, []byte("project:\n  name: acf1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "local"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "local", "runs"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, cfgPath
}

// assertTheAppReachedTheExecutor is the positive control: the app's text really is on this executor's disk, so a
// clean field means the executor kept it there — not that the app was never heard.
func assertTheAppReachedTheExecutor(t *testing.T, root string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "local", "report.json"))
	if err != nil {
		t.Fatalf("the run wrote no report.json, so it never ran its check against the app: %v", err)
	}
	if !strings.Contains(string(b), appMarker) {
		t.Fatalf("positive control: report.json does not hold the app's answer, so this test would prove nothing: %.600s", b)
	}
}

// relayLog collects what the relayed command function logs. It never calls t.Logf: the relayed `run` logs from a
// goroutine that may finish after the test.
type relayLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *relayLog) log(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
}

func (l *relayLog) has(part string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}

func TestExecutor_TheAppsWordsNeverReachTheFailureReasonOrACommandsError(t *testing.T) {
	t.Setenv("ARGUS_SUT_NAMESPACE", "") // the real core, but a clean env: no telemetry endpoints
	app := newMarkerApp(t)
	root, cfgPath := markerRunRoot(t)

	// The relayed commands read the same results directory the run wrote. Their config does not load, so
	// validate_config fails with Argus's words, and the relayed `run` stops before it touches anything.
	badCfg := filepath.Join(root, "relay-config.yaml")
	if err := os.WriteFile(badCfg, []byte("not: [valid yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A stored output that holds the app's answer, for get_output (sample 1), and one over the size a relayed answer
	// may have (sample 3, as an older executor could have left it).
	base := argus.OutputsBase(filepath.Join(root, "local"))
	if err := argus.WriteOutputFile(base, "run-acf1", "CHN-ACF1", "call", 1, []byte(`{"status":500,"body":"`+appMarker+`"}`)); err != nil {
		t.Fatal(err)
	}
	if err := argus.WriteOutputFile(base, "run-acf1", "CHN-ACF1", "call", 3, []byte(`"`+strings.Repeat(appMarker, (1<<20)/len(appMarker)+1)+`"`)); err != nil {
		t.Fatal(err)
	}

	type verbCase struct {
		id, verb, args string
		mustFail       bool
		exact          string // the error text, byte for byte, when it must be exactly Argus's words
		contains       string // a part of Argus's words the error must still carry
		status         string // the status a status-map answer must carry; its result must hold nothing of the app
	}
	cases := []verbCase{
		{id: "c-validate", verb: "validate_config", args: `{}`, mustFail: true, contains: "load config"},
		{id: "c-dash", verb: "get_dashboard_url", args: `{}`},
		{id: "c-report", verb: "get_report", args: `{}`, mustFail: true, contains: "read report"},
		{id: "c-full", verb: "get_full_report", args: `{}`, mustFail: true, contains: "read report"},
		{id: "c-out-missing", verb: "get_output", args: `{"run_id":"run-acf1","scenario_id":"CHN-ACF1","step":"call","sample":2}`,
			mustFail: true, exact: argus.ErrOutputNotFound.Error()},
		{id: "c-out-large", verb: "get_output", args: `{"run_id":"run-acf1","scenario_id":"CHN-ACF1","step":"call","sample":3}`,
			mustFail: true, exact: argus.ErrOutputTooLarge.Error()},
		// AC-F2: get_limited_report reads the same damaged report. With a run id it cannot fail: no readable run file
		// is a not_found status map; without one it is refused as a status map before anything is read.
		{id: "c-limited", verb: "get_limited_report", args: `{"run_id":"run-acf1"}`, status: "not_found"},
		{id: "c-limited-noid", verb: "get_limited_report", args: `{}`, status: "refused"},
		{id: "c-unknown", verb: "frobnicate", args: `{}`, mustFail: true, exact: `unknown relayed verb "frobnicate"`},
		// last: the relayed run is answered at once and runs on in a goroutine; with the relay's config not loading
		// it stops before it touches the results directory, and the test waits for its log line before it ends
		{id: "c-run", verb: "run", args: `{}`},
	}
	cmds := make([]federation.CommandEnvelope, 0, len(cases))
	for _, c := range cases {
		cmds = append(cmds, federation.CommandEnvelope{CommandID: c.id, Verb: c.verb, Args: json.RawMessage(c.args)})
	}

	var (
		mu       sync.Mutex
		phase    int // 0: hand out the run; 1: hand out the commands once; 2: nothing more
		failed   []byte
		answers  = map[string]federation.CommandResult{}
		damaged  bool
		damageEr error
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/fed/poll", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		p := phase
		if p == 1 {
			phase = 2
		}
		mu.Unlock()
		switch p {
		case 0:
			_ = json.NewEncoder(w).Encode(federation.PollResponse{HasRun: true, Run: &federation.RunAssignment{
				Mode: "build", RunID: "run-acf1", RunRequestID: "rr-acf1", Scope: "full", SetHash: "h-acf1",
				Scenarios: []federation.ScenarioPayload{{Path: "permissions/CHN-ACF1.md", Body: markerScenario(app.URL)}}}})
		case 1:
			_ = json.NewEncoder(w).Encode(federation.PollResponse{Commands: cmds})
		default:
			_ = json.NewEncoder(w).Encode(federation.PollResponse{})
		}
	})
	mux.HandleFunc("/fed/results", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p federation.ResultsPush
		_ = json.Unmarshal(b, &p)
		if p.Status == "failed" {
			mu.Lock()
			if failed == nil {
				failed = b
				// The run is over and its report.json holds the app's answer. Damage it the way a half-written file
				// would be — cut just after the app's words — so get_report and get_full_report must fail over a
				// file that still holds them. Then hand out the commands.
				rep, err := os.ReadFile(filepath.Join(root, "local", "report.json"))
				if i := strings.Index(string(rep), appMarker); err == nil && i >= 0 {
					err = os.WriteFile(filepath.Join(root, "local", "report.json"), rep[:i+len(appMarker)+3], 0o644)
					damaged = err == nil
				}
				damageEr = err
				phase = 1
			}
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/fed/command-result", func(w http.ResponseWriter, r *http.Request) {
		var res federation.CommandResult
		_ = json.NewDecoder(r.Body).Decode(&res)
		mu.Lock()
		answers[res.CommandID] = res
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	cp := httptest.NewServer(mux)
	defer cp.Close()

	rl := &relayLog{}
	ex := &Executor{
		Client: tallyTestClient(t, cp.URL), RunnerVersion: "1.0.0",
		PollGap: 10 * time.Millisecond, Backoff: 10 * time.Millisecond, HeartbeatEvery: time.Hour, PushRetries: 1,
		Run:      NewRunFunc(ExecConfig{InstanceID: "inst-acf1", ConfigPath: cfgPath, ResultsRoot: root}),
		Commands: NewCommandFunc(ExecConfig{InstanceID: "inst-acf1", ConfigPath: badCfg, ResultsRoot: root, Log: rl.log}),
		Log:      func(f string, a ...any) { t.Logf(f, a...) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ex.Loop(ctx) }()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(answers)
		mu.Unlock()
		if n == len(cases) && rl.has("relayed run") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if !rl.has("relayed run") {
		t.Error("the relayed run never reported back in this executor's log: it may still be running")
	}

	mu.Lock()
	defer mu.Unlock()

	// ── the run's failure reason ────────────────────────────────────────────────────────────────────────
	if failed == nil {
		t.Fatal("no terminal failed push arrived: the run did not end with a failure reason, so this test proves nothing")
	}
	if damageEr != nil || !damaged {
		t.Fatalf("positive control: report.json did not hold the app's answer when the run ended (%v)", damageEr)
	}
	var fp federation.ResultsPush
	_ = json.Unmarshal(failed, &fp)
	if fp.RunID != "run-acf1" || strings.TrimSpace(fp.FailureReason) == "" {
		t.Fatalf("the failed push names no run or carries no reason: %s", failed)
	}
	if strings.Contains(string(failed), appMarker) {
		t.Errorf("the app's words reached the control plane in the failed push (failure_reason %q)", fp.FailureReason)
	}
	if !strings.Contains(fp.FailureReason, "write run report") {
		t.Errorf("the failure reason lost Argus's own words — the author can no longer tell which step failed: %q", fp.FailureReason)
	}

	// ── every relayed request type ──────────────────────────────────────────────────────────────────────
	for _, c := range cases {
		res, ok := answers[c.id]
		if !ok {
			t.Errorf("%s (%s): never answered", c.id, c.verb)
			continue
		}
		if strings.Contains(res.Error, appMarker) {
			t.Errorf("%s (%s): the app's words reached the command's error text: %q", c.id, c.verb, res.Error)
		}
		switch {
		case c.mustFail && res.Error == "":
			t.Errorf("%s (%s): was meant to fail and did not — this case proves nothing (result %.200s)", c.id, c.verb, res.Result)
		case !c.mustFail && res.Error != "":
			t.Errorf("%s (%s): failed unexpectedly: %q", c.id, c.verb, res.Error)
		}
		if c.exact != "" && res.Error != c.exact {
			t.Errorf("%s (%s): the error must stay exactly Argus's words %q (the control plane matches it), got %q", c.id, c.verb, c.exact, res.Error)
		}
		if c.contains != "" && !strings.Contains(res.Error, c.contains) {
			t.Errorf("%s (%s): the error lost Argus's own words %q: %q", c.id, c.verb, c.contains, res.Error)
		}
		if c.status != "" {
			var st struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(res.Result, &st); err != nil || st.Status != c.status {
				t.Errorf("%s (%s): want a %q status answer, got %.200s (%v)", c.id, c.verb, c.status, res.Result, err)
			}
			if strings.Contains(string(res.Result), appMarker) {
				t.Errorf("%s (%s): the app's words reached the command's result: %.200s", c.id, c.verb, res.Result)
			}
		}
		t.Logf("%-14s %-17s error=%q", c.id, c.verb, res.Error)
	}
	t.Logf("failure_reason=%q", fp.FailureReason)
}

// The direct path pushes its own abort with a failure reason (direct.go) when a fenced run fails. Same app, same rule.
func TestDirectRun_TheAppsWordsNeverReachTheAbortReason(t *testing.T) {
	t.Setenv("ARGUS_SUT_NAMESPACE", "")
	app := newMarkerApp(t)
	root, cfgPath := markerRunRoot(t)
	rec := &pushRecorder{set: federation.MaterializeResponse{Scope: "full", SetHash: "h-direct",
		Scenarios: []federation.ScenarioPayload{{Path: "permissions/CHN-ACF1.md", Body: markerScenario(app.URL)}}}}
	cp := httptest.NewServer(rec.handler())
	defer cp.Close()
	e := &Executor{Client: tallyTestClient(t, cp.URL), HeartbeatEvery: time.Hour,
		Run: NewRunFunc(ExecConfig{InstanceID: "inst-acf1", ConfigPath: cfgPath, ResultsRoot: root})}
	_, _, err := e.DirectRun(context.Background(), filepath.Join(t.TempDir(), "cache.json"), "full", federation.Selection{}, 5*time.Second)
	if err == nil {
		t.Fatal("the direct run did not fail, so no abort reason was pushed and this test proves nothing")
	}
	assertTheAppReachedTheExecutor(t, root)
	pushes, raws := rec.pushes(t)
	var abort *federation.ResultsPush
	for i := range pushes {
		if pushes[i].Status == "failed" {
			abort = &pushes[i]
			if strings.Contains(string(raws[i]), appMarker) {
				t.Errorf("the app's words reached the control plane in the direct abort push (failure_reason %q)", pushes[i].FailureReason)
			}
		}
	}
	if abort == nil {
		t.Fatalf("no abort push arrived (the run was not fenced?); %d pushes, last: %s", len(pushes), lastOf(raws))
	}
	if !strings.Contains(abort.FailureReason, "write run report") {
		t.Errorf("the abort reason lost Argus's own words: %q", abort.FailureReason)
	}
	t.Logf("direct failure_reason=%q (the local error: %v)", abort.FailureReason, err)
}
