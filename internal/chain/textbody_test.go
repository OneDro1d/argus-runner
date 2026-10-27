package chain

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// at step level (HTTPStep called directly, as moneywrites_test.go does, so the refusal's own
// words are visible: chain.Run would relabel a refusal as "SUT unreachable").

// The money guard is shown a text body as a JSON STRING, so a spending route is refused text it cannot read
// as an object, INCLUDING text that happens to be a JSON object under the limit.
func TestHTTPStep_TextBodyOnASpendingRoute_IsRefusedWithTheGuardsOwnWords(t *testing.T) {
	for _, text := range []string{`hello`, `{"source_amount": 5}`, `source_amount=5`, ``} {
		var hit atomic.Bool
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hit.Store(true)
			w.WriteHeader(200)
		}))
		step := HTTPStep("spend", "POST", ts.URL+"/order", map[string]string{"Content-Type": "text/plain"}, text, 200, nil, nil, nil, false,
			orderAllow(), &scenario.MoneySpendLedger{}, WithTextBody(true))
		res := step.Run("cid", map[string]string{})
		ts.Close()
		if hit.Load() {
			t.Errorf("text %q was SENT to a spending route", text)
		}
		if res.Status != report.StatusError || !strings.HasPrefix(res.Observed, MoneyWriteRefusal) || !strings.Contains(res.Observed, "refused (fail-closed)") {
			t.Errorf("text %q: want the guard's fail-closed refusal, got %q / %q", text, res.Status, res.Observed)
		}
	}
}

// MUTATION CONTROL for the above: had the guard been shown the raw text, `{"source_amount": 5}` would have
// been judged an object under the limit. This pins that the two forms differ, so the test above can fail.
func TestGuardViewOfText_IsAJSONStringNeverTheText(t *testing.T) {
	text := `{"source_amount": 5}`
	if msg := scenario.EvaluateSpendAmount(&orderAllow()[0], text); msg != "" {
		t.Fatalf("setup: the raw text should read as an object under the limit, got %q", msg)
	}
	if msg := scenario.EvaluateSpendAmount(&orderAllow()[0], guardViewOfText(text)); msg == "" {
		t.Fatal("the guard's view of a text body must be refused (a JSON string holds no amount_field)")
	}
}

// The non-text form of the same step is unchanged: an object under the limit is sent.
func TestHTTPStep_WithoutTextBody_AnObjectUnderTheLimitIsStillSent(t *testing.T) {
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(200)
	}))
	defer ts.Close()
	step := HTTPStep("spend", "POST", ts.URL+"/order", nil, `{"source_amount": 5}`, 200, nil, nil, nil, false,
		orderAllow(), &scenario.MoneySpendLedger{}, WithTextBody(false))
	if res := step.Run("cid", nil); res.Status != "passed" || got != `{"source_amount": 5}` {
		t.Fatalf("%q / %q, received %q", res.Status, res.Observed, got)
	}
}

func TestBindSavedText_RawValuesAndByNameRefusal(t *testing.T) {
	vars := map[string]string{"s": `x"y\z`, "o": `{"k":"v\"w"}`, jsonKindPrefix + "o": "1"}
	got, err := bindSavedText(`s=${saved.s} o=${saved.o} again=${saved.s}`, vars)
	if err != nil || got != `s=x"y\z o={"k":"v\"w"} again=x"y\z` {
		t.Fatalf("%q, %v", got, err)
	}
	if _, err := bindSavedText(`a=${saved.nope} b=${saved.s}`, vars); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("an unsaved variable must be refused by name, got %v", err)
	}
	// untouched when there is nothing to bind
	if got, _ := bindSavedText(`plain "quoted" \ text`, vars); got != `plain "quoted" \ text` {
		t.Fatalf("%q", got)
	}
}

// Needs() holds a text step whose variable is not saved yet, like the JSON form does.
func TestHTTPStep_TextBody_NeedsTheVariableItNames(t *testing.T) {
	step := HTTPStep("w", "POST", "http://127.0.0.1:1/w", map[string]string{"Content-Type": "text/plain"}, `id=${saved.id}`, 200, nil, nil, nil, false,
		nil, nil, WithTextBody(true))
	if err := step.Needs(map[string]string{}); err == nil || !strings.Contains(err.Error(), "id") {
		t.Fatalf("Needs = %v", err)
	}
	if err := step.Needs(map[string]string{"id": "7"}); err != nil {
		t.Fatalf("Needs with the variable saved = %v", err)
	}
}
