package federation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// TestResultsPush_NoEvidenceFields is the locality-by-schema guard (D-FED.4 / D-CP-WEB.1): a fully
// populated results push, serialized, must contain NO key that could carry evidence — the upload
// format structurally has nowhere to put logs, sagas, DB rows, or verdict classes.
func TestResultsPush_NoEvidenceFields(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p := ResultsPush{
		RunID: "run_1", RunRequestID: "rr_1", Scope: "full", Status: "completed",
		Tallies:     Tallies{Passed: 2, Failed: 1, Errored: 0, Total: 3},
		Scenarios:   []ScenarioResult{{ID: "S1", Outcome: "passed", DurationMs: 12, Summary: "ok"}, {ID: "S2", Outcome: "failed", Summary: "assertion mismatch"}},
		Annotations: Annotations{Commit: "abc", PR: "7", Label: "nightly"},
		SetHash:     "deadbeef", DeepLink: "https://g/x", StartedAt: &now, FinishedAt: &now, DurationMs: 50,
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var m any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{
		"logs": true, "log": true, "tail_logs": true, "sagas": true, "saga": true,
		"verdict": true, "verdicts": true, "triage": true, "expect": true, "expected": true,
		"db_rows": true, "dbrows": true, "evidence": true, "content": true,
	}
	var walk func(v any)
	walk = func(v any) {
		switch t2 := v.(type) {
		case map[string]any:
			for k, vv := range t2 {
				if forbidden[k] {
					t.Errorf("results push carries a forbidden evidence key %q", k)
				}
				walk(vv)
			}
		case []any:
			for _, vv := range t2 {
				walk(vv)
			}
		}
	}
	walk(m)
}

// PR-E: a FULLY populated load_ramp (every optional LoadStep field set) adds no evidence-shaped key, and
// the field is absent from a push that has none (an older control plane never sees it).
func TestResultsPush_LoadRampCarriesNoEvidenceKeys(t *testing.T) {
	zero := 3
	ramp, _ := json.Marshal([]report.LoadRampEntry{{ScenarioID: "AMQL-001", Target: "lab", Driver: "amqp", Steps: []report.LoadStep{{
		Step: 1, Sessions: 10, Status: "blocked", WindowSeconds: 30, OfferedPerS: 1, SentPerS: 1, ConfirmedPerS: 1, DeliveredPerS: 1, DeliveredRatio: 1,
		PublishConfirmUs: &report.Quantiles{Min: 1, P50: 2, P75: 3, P95: 4, P99: 5, Max: 6},
		PublishDeliverUs: &report.Quantiles{Min: 1, P50: 2, P75: 3, P95: 4, P99: 5, Max: 6},
		Errors:           map[string]int{"blocked": 1}, Blocked: []report.BlockedPeriod{{SinceMs: 1, UntilMs: 2, Reason: "memory"}},
		BlockedSeconds: 1, GeneratorLimited: true, RestartsDelta: &zero, NotReady: &zero, Comfortable: true,
	}}}})
	p := ResultsPush{RunID: "r", Scope: "full", Status: "completed", Scenarios: []ScenarioResult{{ID: "S1", Outcome: "passed"}}, LoadRamp: ramp}
	b, _ := json.Marshal(p)
	var m any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{
		"logs": true, "log": true, "sagas": true, "verdict": true, "expect": true, "expected": true, "observed": true,
		"db_rows": true, "evidence": true, "content": true, "body": true, "url": true, "host": true, "password": true,
		"user": true, "message": true, "messages": true, "threshold": true,
	}
	var walk func(v any)
	walk = func(v any) {
		switch t2 := v.(type) {
		case map[string]any:
			for k, vv := range t2 {
				if forbidden[k] {
					t.Errorf("the push carries a forbidden key %q", k)
				}
				walk(vv)
			}
		case []any:
			for _, vv := range t2 {
				walk(vv)
			}
		}
	}
	walk(m)
	if !strings.Contains(string(b), `"load_ramp":[`) {
		t.Fatalf("a push with a ramp lacks load_ramp: %s", b)
	}
	none, _ := json.Marshal(ResultsPush{RunID: "r"})
	if strings.Contains(string(none), "load_ramp") {
		t.Fatalf("a push with no ramp carries the key: %s", none)
	}
}

func TestWire_JSONRoundTrips(t *testing.T) {
	assign := RunAssignment{
		RunRequestID: "rr_1", Scope: "layer", Selection: Selection{Layer: "HTTP Ingestion"},
		SetHash: "h1", Scenarios: []ScenarioPayload{{Path: "a/b.md", Body: "# s"}},
	}
	resp := PollResponse{HasRun: true, Run: &assign, Versions: VersionInfo{CurrentVersion: "1.2.0", MinSupportedRunnerVersion: "1.0.0"}}
	b, _ := json.Marshal(resp)
	var back PollResponse
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.HasRun || back.Run == nil || back.Run.RunRequestID != "rr_1" || back.Run.Selection.Layer != "HTTP Ingestion" {
		t.Fatalf("poll response did not round-trip: %+v", back)
	}
	if back.Versions.CurrentVersion != "1.2.0" {
		t.Errorf("versions lost: %+v", back.Versions)
	}
}

func TestReject_CarriesReasonAndServerTime(t *testing.T) {
	rr := RejectResponse{Reason: ReasonUnknownInstance, ServerTime: time.Unix(1_700_000_000, 0)}
	b, _ := json.Marshal(rr)
	var back RejectResponse
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Reason != ReasonUnknownInstance || back.ServerTime.Unix() != 1_700_000_000 {
		t.Fatalf("reject did not round-trip: %+v", back)
	}
}

// WireTier is the boundary between the CONCRETE tier an operator onboards with (`aks` today; `eks`
// or `gke` later) and the three CANONICAL tiers the control plane accepts. Without it, `--tier aks`
// brings the whole execution plane up and is then REJECTED at RegisterInstance ("invalid tier"),
// which surfaces ~60s later as "the executor did not REGISTER" and leaves a half-built instance —
// a namespace, a running executor and a copied pull secret, with no teardown.
//
// It must normalize on the EXECUTOR side, not by widening ValidTier: the control plane is already
// deployed and validates with the three-tier vocabulary, so a new concrete tier must not require a
// CP redeploy to be onboardable.
func TestWireTier_concreteTiersNormalizeToACanonicalTier(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"aks", "managed"},
		{"AKS", "managed"}, // case-insensitive: the flag is operator-typed
		{"eks", "managed"},
		{"gke", "managed"},
		{"k8s-dev", "managed"},
		{"managed", "managed"},
		{"k3d", "k3d"},         // local tiers pass through untouched
		{"compose", "compose"}, // the default must never be rewritten
		{"", ""},               // empty stays empty; the caller's own default applies
	} {
		if got := WireTier(tc.in); got != tc.want {
			t.Errorf("WireTier(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The whole point: whatever WireTier emits for a concrete tier must SATISFY ValidTier, or the
// register is rejected. This is the assertion that would have caught the aks defect.
func TestWireTier_outputIsAlwaysAcceptedByValidTier(t *testing.T) {
	for _, concrete := range []string{"aks", "eks", "gke", "k8s-dev", "managed", "k3d", "compose"} {
		w := WireTier(concrete)
		if !ValidTier(w) {
			t.Errorf("WireTier(%q) = %q, which ValidTier REJECTS — an onboard on this tier would fail at registration", concrete, w)
		}
	}
}

// TestRegistrableTier pins the guard preflight and render-k8s apply before anything is created. The
// list above is only the tiers someone thought of; this is the check for the ones nobody did — k3s
// passed straight through WireTier and was refused at registration (2026-09-23), and kind/minikube,
// which the renderer supports as local tiers, fail the same way.
func TestRegistrableTier(t *testing.T) {
	for _, ok := range []string{"aks", "AKS", "eks", "gke", "k8s-dev", "managed", "k3d", "compose"} {
		if !RegistrableTier(ok) {
			t.Errorf("RegistrableTier(%q) = false; WireTier folds it to %q, which ValidTier accepts", ok, WireTier(ok))
		}
	}
	for _, bad := range []string{"k3s", "kind", "minikube", "typo", ""} {
		if RegistrableTier(bad) {
			t.Errorf("RegistrableTier(%q) = true; the control plane would refuse %q at registration", bad, WireTier(bad))
		}
	}
}

func TestValidTier(t *testing.T) {
	for _, ok := range []string{"compose", "k3d", "managed"} {
		if !ValidTier(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "prod", "aks", "docker"} {
		if ValidTier(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
