package argus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// Fix for the cid-in-chain-claims defect (tester-reported, confirmed live): a chain step's `##
// EXPECT` / `### Runnable` claim (`- step <name>: <assertion>`) was parsed RAW — only the TRIGGER
// payload went through resolveVars — so ${cid}/${cid8}/${correlation_id} inside a claim reached the
// mcp/http/amqp step (and the report's assertions_enforced / Failure.Expected) as the placeholder
// LITERAL. Live symptom: an amqp consume claim `content contains "argus-lab NLB-001 ${cid8}"` was
// enforced against the literal text, so it never matched a real delivered message.
//
// What was already correct at this baseline (V31-003): the mcp step's claims (chainStepExpect,
// chain_scenario.go) already went through fillCorrelationIDInChecks right after being parsed.
// TestMCPChain_ClaimResolvesCorrelationID below is therefore a REGRESSION GUARD for that path, not a
// new fix — it is included because the dispatcher's PROMISE asks for coverage of all three step
// types, and it is reported honestly below as already-green at baseline (see the RED-output
// evidence in the builder's report). The http and amqp cases WERE broken and are RED at baseline.

// ── amqp: a consume body claim with ${cid8} ─────────────────────────────────────────────────────

func TestAMQPChain_ConsumeBodyClaimResolvesCid8(t *testing.T) {
	trig := `{"steps":[{"type":"amqp","name":"consume","op":"consume","url_env":"MSGBUS_ARGUS_TEST_URL","queue":"q","wait":"1s"}]}`
	expect := "### Runnable\n- step consume: body contains \"argus-lab NLB-001 ${cid8}\"\n"

	t.Run("passes when the delivered message carries THIS run's cid8", func(t *testing.T) {
		const corr = "tr-amqp-cid8-a"
		f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true},
			consumeBody: []byte("argus-lab NLB-001 " + cid8(corr))}
		withFakeBroker(t, f)
		s := scenario.Parse(chainRunMD("", trig, expect))
		res := runChainScenario(&config.Config{}, s, corr, "testkit/ui")
		if res.Status != "passed" {
			t.Fatalf("${cid8} must resolve to THIS run's cid8 and match the delivered message: %q %+v %+v",
				res.Status, res.Failure, res.Steps)
		}
		if len(res.Steps) != 1 || len(res.Steps[0].AssertionsEnforced) != 2 ||
			!strings.Contains(res.Steps[0].AssertionsEnforced[1], cid8(corr)) {
			t.Fatalf("assertions_enforced must carry the BOUND cid8, not the placeholder: %+v", res.Steps)
		}
	})

	t.Run("fails when the delivered message carries a DIFFERENT run's cid8", func(t *testing.T) {
		const corrA, corrB = "tr-amqp-cid8-a", "tr-amqp-cid8-b"
		if cid8(corrA) == cid8(corrB) {
			t.Fatalf("test fixture bug: corrA and corrB must derive different cid8 values")
		}
		f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true},
			consumeBody: []byte("argus-lab NLB-001 " + cid8(corrA))}
		withFakeBroker(t, f)
		s := scenario.Parse(chainRunMD("", trig, expect))
		res := runChainScenario(&config.Config{}, s, corrB, "testkit/ui")
		if res.Status != "passed" {
			// Expected: it fails. This inverse catches a regression that makes EVERY cid8 match
			// (e.g. resolving against the wrong run).
			return
		}
		t.Fatalf("a claim bound to run B's cid8 must NOT pass against a message carrying run A's cid8: %+v", res.Steps)
	})
}

// ── http: a step claim with ${cid} ──────────────────────────────────────────────────────────────

func TestHTTPChain_ClaimResolvesCid(t *testing.T) {
	const corr = "tr-http-cid-1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"created-` + corr + `"}`))
	}))
	defer srv.Close()

	trig := `{"steps":[{"type":"http","name":"create","method":"POST","url":"${BASE}/workspaces"}]}`
	expect := "### Runnable\n- step create: status=201\n- step create: body contains created-${cid}\n"
	s := scenario.Parse(chainRunMD(srv.URL, trig, expect))
	res := runChainScenario(&config.Config{}, s, corr, "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("${cid} must resolve to THIS run's correlation id and match the response body: %q %+v %+v",
			res.Status, res.Failure, res.Steps)
	}
	if len(res.Steps) != 1 || len(res.Steps[0].AssertionsEnforced) != 2 ||
		!strings.Contains(res.Steps[0].AssertionsEnforced[1], corr) {
		t.Fatalf("assertions_enforced must carry the BOUND correlation id, not the placeholder: %+v", res.Steps)
	}

	// The inverse: a DIFFERENT run's id must not satisfy this claim (proves the value is bound, not
	// coincidentally matched — e.g. the server ignoring the id and always answering the same text).
	s2 := scenario.Parse(chainRunMD(srv.URL, trig, expect))
	res2 := runChainScenario(&config.Config{}, s2, "tr-http-cid-2", "testkit/ui")
	if res2.Status != "failed" {
		t.Fatalf("a claim bound to a DIFFERENT run's id must fail against this server's fixed response: %+v", res2.Steps)
	}
}

// ── mcp: a step claim with ${correlation_id} (already correct at baseline — regression guard) ────

func TestMCPChain_ClaimResolvesCorrelationID(t *testing.T) {
	const corr = "tr-mcp-corr-1"
	ts := httptest.NewServer(fakeMCPTextServer(`{"id":"created-` + corr + `"}`))
	defer ts.Close()
	c := chainCfg(t, ts.URL)
	trig := `{"steps":[{"type":"mcp","name":"create","tool":"t","args":{}}]}`
	expect := "### Runnable\n- step create: body has id containing created-${correlation_id}\n"

	s := scenario.Parse(chainRunMD(ts.URL, trig, expect))
	res := runChainScenario(c, s, corr, "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("${correlation_id} must resolve to THIS run's id and match the response body: %q %+v %+v",
			res.Status, res.Failure, res.Steps)
	}
	if len(res.Steps) != 1 || len(res.Steps[0].AssertionsEnforced) != 1 ||
		!strings.Contains(res.Steps[0].AssertionsEnforced[0], corr) {
		t.Fatalf("assertions_enforced must carry the BOUND correlation id, not the placeholder: %+v", res.Steps)
	}
}

// ── any OTHER ${…} in a claim (an env var) is refused, never substituted ──────────────────────────

// TestChainClaim_EnvPlaceholderRefusedAtRunTime is the SECOND door (run.go): a file that reaches a
// run WITHOUT going through scenario.Validate (e.g. hand-edited, or a validator bypassed by a bug)
// must still be refused, by name, and the env var's value must never be read at all — not merely
// "not printed": os.Getenv is never called on the placeholder's name on this path.
func TestChainClaim_EnvPlaceholderRefusedAtRunTime(t *testing.T) {
	const secretEnv, secretValue = "ARGUS_TEST_CLAIM_SECRET", "sh0uld-never-leak-93f7"
	t.Setenv(secretEnv, secretValue)

	// An http step (not mcp): it needs no targets.mcp.base_url, so the ONLY thing standing between
	// this scenario and a run is the claim itself — isolating the refusal from an unrelated preflight
	// requirement.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	trig := `{"steps":[{"type":"http","name":"create","method":"GET","url":"${BASE}/x"}]}`
	expect := "### Runnable\n- step create: status=200\n- step create: body contains ${" + secretEnv + "}\n"
	s := scenario.Parse(chainRunMD(srv.URL, trig, expect)) // scenario.Parse ONLY — Validate is deliberately skipped
	res := runChainScenario(&config.Config{}, s, "tr-env-refuse", "testkit/ui")

	if res.Status != "failed" {
		t.Fatalf("a claim naming an environment variable must be REFUSED, never resolved: %q %+v", res.Status, res.Steps)
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "${"+secretEnv+"}") {
		t.Fatalf("the refusal must name the placeholder %q: %+v", "${"+secretEnv+"}", res.Failure)
	}
	if res.Failure != nil && strings.Contains(res.Failure.Observed, secretValue) {
		t.Fatalf("the env var's VALUE must never appear in the report: %+v", res.Failure)
	}
}

// TestChainClaim_EnvPlaceholderRefusedAtAuthoringTime is the FIRST door: scenario.Validate
// (checkPlaceholderErrors, V31-003) already refuses this for every scenario, chain included — this
// pins that a chain's `### Runnable` claim is covered too, so it cannot regress silently.
func TestChainClaim_EnvPlaceholderRefusedAtAuthoringTime(t *testing.T) {
	md := chainRunMD("", `{"steps":[{"type":"mcp","name":"create","tool":"t","args":{}}]}`,
		"### Runnable\n- step create: body contains ${ARGUS_TEST_CLAIM_SECRET}\n")
	_, errs := scenario.Validate(md)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "${ARGUS_TEST_CLAIM_SECRET}") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Validate must refuse an env-var placeholder in a chain claim by name; errors: %+v", errs)
	}
}
