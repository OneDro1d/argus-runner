package chain

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// moneywrites_test.go — "money writes" (T5.4 follow-up, 2026-09-26), item 3, at the EXECUTION door
// that actually sees the resolved value: an http chain step. HTTPStep is called directly (its Step's
// Run closure), exactly like chain_test.go's other step-level tests, so a real net/http.Client fires
// at a real httptest server — proving the check happens BEFORE the request reaches the wire, not
// just against a mocked judgement.

func orderAllow() scenario.MoneyWriteAllowlist {
	return scenario.MoneyWriteAllowlist{{
		Method: "POST", Path: "/order", Spends: true,
		AmountField: "source_amount", MaxAmount: 25, MaxPerRun: 2,
	}}
}

func TestHTTPStep_SpendOverMaxAmountNotSent(t *testing.T) {
	var hit atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	step := HTTPStep("spend", "POST", ts.URL+"/order", nil, `{"source_amount": 999}`, 200, nil, nil, nil, false,
		orderAllow(), &scenario.MoneySpendLedger{})
	res := step.Run("cid", map[string]string{})

	if hit.Load() {
		t.Fatal("the SUT was hit despite an over-max_amount spend")
	}
	if res.Status != report.StatusError {
		t.Fatalf("status = %q, want %q", res.Status, report.StatusError)
	}
	if !strings.Contains(res.Observed, "999") || !strings.Contains(res.Observed, "25") {
		t.Errorf("observed does not name the entry and the value: %q", res.Observed)
	}
}

func TestHTTPStep_MissingAmountFieldNotSent(t *testing.T) {
	var hit atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	step := HTTPStep("spend", "POST", ts.URL+"/order", nil, `{"other_field": 1}`, 200, nil, nil, nil, false,
		orderAllow(), &scenario.MoneySpendLedger{})
	res := step.Run("cid", map[string]string{})

	if hit.Load() {
		t.Fatal("the SUT was hit despite a missing amount_field")
	}
	if res.Status != report.StatusError {
		t.Fatalf("status = %q, want %q", res.Status, report.StatusError)
	}
}

func TestHTTPStep_NonNumericAmountFieldNotSent(t *testing.T) {
	var hit atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	step := HTTPStep("spend", "POST", ts.URL+"/order", nil, `{"source_amount": "10"}`, 200, nil, nil, nil, false,
		orderAllow(), &scenario.MoneySpendLedger{})
	res := step.Run("cid", map[string]string{})

	if hit.Load() {
		t.Fatal("the SUT was hit despite a non-numeric (quoted) amount_field")
	}
	if res.Status != report.StatusError {
		t.Fatalf("status = %q, want %q", res.Status, report.StatusError)
	}
}

func TestHTTPStep_UnderCapIsSent(t *testing.T) {
	var body []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	step := HTTPStep("spend", "POST", ts.URL+"/order", nil, `{"source_amount": 10}`, 200, nil, nil, nil, false,
		orderAllow(), &scenario.MoneySpendLedger{})
	res := step.Run("cid", map[string]string{})

	if res.Status != "passed" {
		t.Fatalf("status = %q, observed %q, want passed", res.Status, res.Observed)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil || got["source_amount"] != float64(10) {
		t.Fatalf("the SUT did not receive the request under cap: body=%q err=%v", body, err)
	}
}

// max_per_run enforced ACROSS TWO STEPS sharing one ledger — the chain-level twin of item 3's own
// "across all scenarios" acceptance case.
func TestHTTPStep_MaxPerRunEnforcedAcrossTwoSteps(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	ledger := &scenario.MoneySpendLedger{} // orderAllow's max_per_run is 2
	allow := orderAllow()
	step1 := HTTPStep("s1", "POST", ts.URL+"/order", nil, `{"source_amount": 5}`, 200, nil, nil, nil, false, allow, ledger)
	step2 := HTTPStep("s2", "POST", ts.URL+"/order", nil, `{"source_amount": 5}`, 200, nil, nil, nil, false, allow, ledger)
	step3 := HTTPStep("s3", "POST", ts.URL+"/order", nil, `{"source_amount": 5}`, 200, nil, nil, nil, false, allow, ledger)

	if r := step1.Run("cid", nil); r.Status != "passed" {
		t.Fatalf("step1: %q %q", r.Status, r.Observed)
	}
	if r := step2.Run("cid", nil); r.Status != "passed" {
		t.Fatalf("step2: %q %q", r.Status, r.Observed)
	}
	r3 := step3.Run("cid", nil)
	if r3.Status != report.StatusError {
		t.Fatalf("step3 (the 3rd spend against max_per_run=2) status = %q, want error", r3.Status)
	}
	if hits.Load() != 2 {
		t.Fatalf("the SUT was hit %d times, want exactly 2 (the 3rd must never be sent)", hits.Load())
	}
}

// An unlisted request (no matching allow entry) is UNCHANGED by this file's whole mechanism — the
// spend check never even engages, so a normal http step keeps working exactly as before.
func TestHTTPStep_UnmatchedRequestUnaffectedByAllowlist(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	step := HTTPStep("plain", "GET", ts.URL+"/health", nil, "", 200, nil, nil, nil, false,
		orderAllow(), &scenario.MoneySpendLedger{})
	res := step.Run("cid", map[string]string{})
	if res.Status != "passed" {
		t.Fatalf("an unrelated GET was affected by an unrelated allowlist: %q %q", res.Status, res.Observed)
	}
}

// nil allowlist / nil ledger (today's behaviour, no money_writes block declared) must not panic or
// change anything — this is what every OTHER chain http test in this package relies on implicitly.
func TestHTTPStep_NilAllowlistAndLedgerBehaveAsBefore(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	step := HTTPStep("plain", "POST", ts.URL+"/order", nil, `{"source_amount": 999999}`, 200, nil, nil, nil, false,
		nil, nil)
	res := step.Run("cid", map[string]string{})
	if res.Status != "passed" {
		t.Fatalf("nil allow/ledger changed behaviour: %q %q", res.Status, res.Observed)
	}
}
