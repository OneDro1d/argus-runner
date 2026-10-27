package argus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// moneyguard_runner_kind_test.go —: THE GUARD JUDGES THE RUNNER THE SCENARIO WILL GET.
//
// The money guard used to pick what a scenario is by tag (mcp, ui, chain) and ignore the layer; the run
// dispatch picks the AMQP Load layer, the HTTP Load layer, then chain, mcp, ui. A file carrying two engine
// tags, or an engine tag on a load layer, was judged as one runner and RUN as another. These tests run the
// real door (runOneScenario) with a money-handling config and an httptest server and assert ZERO requests.

type mixedEnv struct {
	mu    sync.Mutex
	hits  []string
	srv   *httptest.Server
	cfg   *config.Config
	loadR *loadFake
}

func newMixedEnv(t *testing.T, allow ...config.MoneyWriteAllowEntry) *mixedEnv {
	t.Helper()
	newHTTPLoadEnv(t) // stubs the HTTP load sleeps / env reads
	newLoadEnv(t)     // stubs the AMQP load sleeps / env reads
	e := &mixedEnv{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.hits = append(e.hits, r.Method+" "+r.URL.Path)
		e.mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(e.srv.Close)
	yaml := "project:\n  name: lab\ntargets:\n  http:\n    base_url: " + e.srv.URL + "\n  http_targets:\n    api-lab:\n      base_url: " + e.srv.URL + "\n" +
		"  message_broker_targets:\n    load-lab:\n      type: amqp\n      url: amqp://loaduser:pw@load-lab.invalid:5672/\n      management_url: http://load-lab.invalid:15672\n      exchanges:\n        load: argus.load\n" +
		"load_allowed_targets:\n  api-lab:\n    max_sessions: 100\n  load-lab:\n    max_sessions: 1000\n"
	e.cfg = loadCfg(t, yaml)
	e.cfg.MoneyHandling = true
	if len(allow) > 0 {
		e.cfg.MoneyWrites = &config.MoneyWritesConfig{Allow: allow}
	}
	e.loadR = &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("MX-001", p) }}
	return e
}

func (e *mixedEnv) hitList() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.hits...)
}

// mixedMD builds one scenario file. payload is the JSON after the TRIGGER line ("" = none); load is the ## LOAD body.
func mixedMD(layer, tags, target, trigger, payload, expect, load string) string {
	l := []string{"# Scenario: m", "", "## Metadata", "- **ID**: MX-001", "- **Layer**: " + layer, "- **Tags**: " + tags}
	if target != "" {
		l = append(l, "- **Target**: "+target)
	}
	l = append(l, "", "## TRIGGER", trigger)
	if payload != "" {
		l = append(l, "", "```json", payload, "```")
	}
	l = append(l, "", "## EXPECT", expect, "")
	if load != "" {
		l = append(l, "## LOAD", load, "")
	}
	l = append(l, "## TIMEOUT", "30s", "", "## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "")
	return strings.Join(l, "\n")
}

const (
	mixedHTTPLoad = "- **Steps**: 2\n- **Step Duration Seconds**: 20\n- **Ramp Seconds**: 0\n- **Target P95 Ms**: 250\n- **Max Error Rate**: 0.05\n"
	mixedAMQPLoad = "- **Steps**: 2\n- **Step Duration Seconds**: 20\n- **Ramp Seconds**: 10\n- **Target P95 Ms**: 250\n- **Max Error Rate**: 0.01\n"
)

func chainPostPayload(srvURL string) string {
	return `{"steps":[{"type":"http","name":"s","method":"POST","url":"` + srvURL + `/api/v1/orders/transfer"}]}`
}

func chainGetPayload(srvURL string) string {
	return `{"steps":[{"type":"http","name":"s","method":"GET","url":"` + srvURL + `/health"}]}`
}

func (e *mixedEnv) run(t *testing.T, md string) (res struct{ Status, Observed string }) {
	t.Helper()
	s := scenario.Parse(md)
	r := runOneScenario(e.cfg, scenarioFile{s: s}, "tr-mixed", t.TempDir(), e.loadR,
		e.cfg.MoneyWriteAllowlist(), &scenario.MoneySpendLedger{}, "", nil)
	res.Status = string(r.Status)
	if r.Failure != nil {
		res.Observed = r.Failure.Observed
	}
	return res
}

// THE REVIEWER'S PROBE: `ui, chain`, the list holds only the ui entry for the spec the TRIGGER names, the chain
// holds a POST to a money-word path. The guard used to judge a ui entry and the dispatch ran the chain: the POST was sent.
func TestMoneyGuard_UIChainWithOnlyAUIEntry_PostStepRefusedNothingSent(t *testing.T) {
	specs := fakeUIRun(t)
	e := newMixedEnv(t, config.MoneyWriteAllowEntry{Engine: "ui", Spec: runDoorSpec})
	md := mixedMD("Permissions", "ui, chain", "", "POST `"+runDoorSpec+"`", chainPostPayload(e.srv.URL),
		"### Runnable\n- step s: status=200\n", "")
	res := e.run(t, md)
	if res.Status != "error" || !strings.HasPrefix(res.Observed, MoneyGuardRefusal) || !strings.Contains(res.Observed, "only GET") {
		t.Errorf("want a money refusal naming the POST step: %+v", res)
	}
	if h := e.hitList(); len(h) != 0 {
		t.Errorf("the refused chain sent %v", h)
	}
	if len(*specs) != 0 {
		t.Errorf("the ui driver was reached: %v", *specs)
	}
}

// Every mixed case, at the run door: ZERO hits at the httptest server and ZERO calls to the load runner.
func TestMoneyGuard_MixedRunnerScenarios_RefusedAtTheRunDoorNothingSent(t *testing.T) {
	mcpE := func(target string) config.MoneyWriteAllowEntry {
		return config.MoneyWriteAllowEntry{Engine: "mcp", Target: target, Tool: "t"}
	}
	uiE := func(spec string) config.MoneyWriteAllowEntry {
		return config.MoneyWriteAllowEntry{Engine: "ui", Spec: spec}
	}
	for _, tc := range []struct {
		name  string
		allow func() []config.MoneyWriteAllowEntry
		md    func(srv string) string
	}{
		{"mcp+chain, POST to a money path, only an mcp entry",
			func() []config.MoneyWriteAllowEntry { return []config.MoneyWriteAllowEntry{{Engine: "mcp", Tool: "t"}} },
			func(srv string) string {
				return mixedMD("Permissions", "mcp, chain", "", "", `{"tool":"t","steps":[{"type":"http","name":"s","method":"POST","url":"`+srv+`/api/v1/orders/transfer"}]}`,
					"### Runnable\n- step s: status=200\n", "")
			}},
		{"ui+HTTP Load, only a ui entry for the trigger path",
			func() []config.MoneyWriteAllowEntry { return []config.MoneyWriteAllowEntry{uiE("/api/v1/orders/x")} },
			func(string) string {
				return mixedMD("HTTP Load", "ui", "api-lab", "POST `/api/v1/orders/x`", "", "### Runnable\n- every step is measured\n", mixedHTTPLoad)
			}},
		{"ui+AMQP Load, only a ui entry for the trigger path",
			func() []config.MoneyWriteAllowEntry { return []config.MoneyWriteAllowEntry{uiE("/amqp-load")} },
			func(string) string {
				return mixedMD("AMQP Load", "ui", "load-lab", "POST `/amqp-load`", "", "### Runnable\n- every step is measured\n", mixedAMQPLoad)
			}},
		{"mcp+HTTP Load, only an mcp entry for the target",
			func() []config.MoneyWriteAllowEntry { return []config.MoneyWriteAllowEntry{mcpE("api-lab")} },
			func(string) string {
				return mixedMD("HTTP Load", "mcp", "api-lab", "POST `/api/v1/orders/x`", `{"tool":"t"}`, "### Runnable\n- every step is measured\n", mixedHTTPLoad)
			}},
		{"mcp+AMQP Load, only an mcp entry for the target",
			func() []config.MoneyWriteAllowEntry { return []config.MoneyWriteAllowEntry{mcpE("load-lab")} },
			func(string) string {
				return mixedMD("AMQP Load", "mcp", "load-lab", "POST `/amqp-load`", `{"tool":"t"}`, "### Runnable\n- every step is measured\n", mixedAMQPLoad)
			}},
		{"chain+HTTP Load, a chain of GETs on a POST ramp, no list at all",
			func() []config.MoneyWriteAllowEntry { return nil },
			func(srv string) string {
				return mixedMD("HTTP Load", "chain", "api-lab", "POST `/api/v1/orders/x`", chainGetPayload(srv), "### Runnable\n- every step is measured\n", mixedHTTPLoad)
			}},
		{"chain+AMQP Load, a chain of GETs on a publishing ramp, no list at all",
			func() []config.MoneyWriteAllowEntry { return nil },
			func(srv string) string {
				return mixedMD("AMQP Load", "chain", "load-lab", "POST `/amqp-load`", chainGetPayload(srv), "### Runnable\n- every step is measured\n", mixedAMQPLoad)
			}},
	} {
		specs := fakeUIRun(t)
		e := newMixedEnv(t, tc.allow()...)
		res := e.run(t, tc.md(e.srv.URL))
		if res.Status != "error" || !strings.HasPrefix(res.Observed, MoneyGuardRefusal) {
			t.Errorf("%s: want a money refusal, got %+v", tc.name, res)
		}
		if h := e.hitList(); len(h) != 0 {
			t.Errorf("%s: the server was hit: %v", tc.name, h)
		}
		if len(e.loadR.calls) != 0 {
			t.Errorf("%s: the load runner was started %d time(s)", tc.name, len(e.loadR.calls))
		}
		if len(*specs) != 0 {
			t.Errorf("%s: the ui driver was reached: %v", tc.name, *specs)
		}
	}
}
