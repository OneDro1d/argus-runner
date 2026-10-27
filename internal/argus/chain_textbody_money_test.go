package argus

// Point 3 — MONEY. A chain http step's text body must never get a spending request past the
// money_writes guard that it could not have got past before.
//
// WRITTEN SO IT COMPILES AND PASSES ON THE BASE COMMIT TOO (91b6833d): it names no symbol this branch added.
// The same cases run on both are the proof that "an allowlisted spending route sent a body the guard cannot
// read is REFUSED, never sent" is today's outcome and stays the outcome.
//
// What the guard does with a string body today: the body is the JSON text `"hello"`; EvaluateSpendAmount
// parses it, finds a string where it needs an object holding amount_field, and refuses (fail-closed). With
// the text form the bytes on the wire are `hello`, but the guard must still be shown the string, not the
// text: `{"pay_amount": 5}` as text would otherwise read as an object under the limit and be sent.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

func tbOrderAllow(spends bool) scenario.MoneyWriteAllowlist {
	a := scenario.MoneyWriteAllow{Method: "POST", Path: "/order", Spends: spends}
	if spends {
		a.AmountField, a.MaxAmount, a.MaxPerRun = "pay_amount", 25, 5
	}
	return scenario.MoneyWriteAllowlist{a}
}

// tbRun sends one chain http step at /order and reports whether the server was hit, what it received, the
// step's status and its observed text.
func tbRun(t *testing.T, allow scenario.MoneyWriteAllowlist, headers, body string) (hit bool, got, status, observed string) {
	t.Helper()
	var hits atomic.Int32
	var rcv atomic.Value
	rcv.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hits.Add(1)
		rcv.Store(string(b))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	step := `{"type":"http","name":"s","method":"POST","url":"${BASE}/order"`
	if headers != "" {
		step += `,"headers":` + headers
	}
	step += `,"body":` + body + `}`
	trig := `{"steps":[` + step + `]}`
	s := scenario.Parse(chainRunMD(srv.URL, trig, okExpect))
	res := runChainScenarioWithMoneyWrites(&config.Config{}, s, "tr-tbmoney", "testkit/ui", allow, &scenario.MoneySpendLedger{})
	if len(res.Steps) != 1 {
		t.Fatalf("want one step, got %+v", res)
	}
	return hits.Load() > 0, rcv.Load().(string), res.Steps[0].Status, res.Steps[0].Observed
}

func TestChainTextBody_MoneyGuard_StringBodiesOnASpendingRouteAreRefusedNeverSent(t *testing.T) {
	const textPlain = `{"Content-Type":"text/plain"}`
	cases := []struct {
		name, headers, body string
	}{
		{"JSON string, no header (today's quoted form)", "", `"hello"`},
		{"JSON string under text/plain (the new text form)", textPlain, `"hello"`},
		{"text that reads as a JSON object UNDER the limit", textPlain, `"{\"pay_amount\": 5}"`},
		{"text that reads as a JSON object OVER the limit", textPlain, `"{\"pay_amount\": 999}"`},
		{"form-encoded text", `{"Content-Type":"application/x-www-form-urlencoded"}`, `"pay_amount=5"`},
		{"JSON string under application/json", `{"Content-Type":"application/json"}`, `"hello"`},
		{"empty text", textPlain, `""`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hit, got, status, observed := tbRun(t, tbOrderAllow(true), c.headers, c.body)
			t.Logf("MONEY | %-52s | hit=%v status=%s observed=%.110s", c.name, hit, status, observed)
			if hit {
				t.Fatalf("the spending route was SENT a body the guard cannot read: %q", got)
			}
			// chain.Run reads a step that ended `error` as "the SUT is unreachable" and relabels it
			// not-measured (the refusal's own words are asserted at step level, in internal/chain): what
			// matters here is that the step did NOT pass and the server was NOT hit.
			if status == "passed" || (status != report.StatusError && status != report.StepNotMeasured) {
				t.Fatalf("want a refusal (error / not-measured), got %q / %q", status, observed)
			}
		})
	}
}

// The control the table above leans on: the guard still lets a readable object through, in the same harness,
// so "refused" is the guard's decision about the TEXT and not a harness that refuses everything.
func TestChainTextBody_MoneyGuard_ControlAnObjectUnderTheLimitIsSent(t *testing.T) {
	hit, got, status, observed := tbRun(t, tbOrderAllow(true), "", `{"pay_amount": 5}`)
	t.Logf("MONEY | control: object under the limit | hit=%v status=%s observed=%.80s got=%s", hit, status, observed, got)
	if !hit || status != "passed" {
		t.Fatalf("an object under max_amount must be sent: hit=%v %q / %q", hit, status, observed)
	}
}

// Routes the guard does not stop are not stopped by this rule either: a string body is sent on a non-spending
// allowlisted route and on a route that is not listed, as it always was.
func TestChainTextBody_MoneyGuard_NonSpendingRoutesSendAStringBody(t *testing.T) {
	for _, c := range []struct {
		name  string
		allow scenario.MoneyWriteAllowlist
	}{
		{"allowlisted, spends:false", tbOrderAllow(false)},
		{"no allowlist at all", nil},
	} {
		hit, _, status, observed := tbRun(t, c.allow, `{"Content-Type":"text/plain"}`, `"hello"`)
		t.Logf("MONEY | %-30s | hit=%v status=%s", c.name, hit, status)
		if !hit || status != "passed" {
			t.Errorf("%s: a string body must be sent: hit=%v %q / %q", c.name, hit, status, observed)
		}
	}
}

// The authoring door (validate-config) reads the same body text and refuses it the same way, before any run.
func TestChainTextBody_MoneyGuard_ValidateConfigRefusesAStringBodyOnASpendingRoute(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"s","method":"POST","url":"http://sut.invalid/order","headers":{"Content-Type":"text/plain"},"body":"hello"}]}`
	s := scenario.Parse(chainRunMD("", trig, okExpect))
	viol := scenario.MoneyWriteAmountViolations(s, tbOrderAllow(true))
	t.Logf("MONEY | validate-config: %v", viol)
	if len(viol) != 1 || !strings.Contains(viol[0], "refused (fail-closed)") {
		t.Fatalf("want one fail-closed violation, got %v", viol)
	}
}
