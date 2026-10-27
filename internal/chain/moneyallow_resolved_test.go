package chain

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// moneyallow_resolved_test.go — THE RESOLVED PATH IS WHAT IS ALLOWED (point 2c).
// A real net/http.Client fires at a real httptest server, so "nothing is sent" is
// proven on the wire, not on a mocked judgement.

type recordingServer struct {
	mu    sync.Mutex
	paths []string
	ts    *httptest.Server
}

func newRecordingServer(t *testing.T) *recordingServer {
	t.Helper()
	r := &recordingServer{}
	r.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.paths = append(r.paths, req.RequestURI)
		r.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(r.ts.Close)
	return r
}

func (r *recordingServer) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

var getOrders = scenario.MoneyWriteAllowlist{{Method: "GET", Path: "/api/v1/orders/{id}", Spends: false}}

func TestResolved_AnEntryThatNoLongerMatchesTheResolvedPathIsNotSent(t *testing.T) {
	srv := newRecordingServer(t)
	for _, tc := range []struct {
		saved string
		sent  bool
	}{
		{"42", true},
		{"a-b_c", true},
		{"42/cancel", false}, // would address another route after the file's guard passed
		{"42%2Fcancel", false},
		{"..", false},
		{"%2e%2e", false},
		{"", false},
	} {
		before := len(srv.seen())
		step := HTTPStep("read", "GET", srv.ts.URL+"/api/v1/orders/${saved.id}", nil, "", 0, nil, nil, nil, false,
			getOrders, &scenario.MoneySpendLedger{}, WithMoneyHandling(true))
		res := step.Run("cid", map[string]string{"id": tc.saved})
		sent := len(srv.seen()) > before
		if sent != tc.sent {
			t.Errorf("saved %q: sent = %v, want %v (status %s, %s)", tc.saved, sent, tc.sent, res.Status, res.Observed)
		}
		if !tc.sent {
			if res.Status != report.StatusError || !strings.HasPrefix(res.Observed, MoneyResolvedRefusal) {
				t.Errorf("saved %q: want an error carrying the refusal wording, got %s / %s", tc.saved, res.Status, res.Observed)
			}
			if strings.Contains(res.Observed, tc.saved) && tc.saved != "" && tc.saved != ".." {
				t.Errorf("saved %q: the refusal echoes the saved value: %s", tc.saved, res.Observed)
			}
		}
	}
}

// at the chain door, with NO entry at all: a money-handling run refuses a GET whose
// resolved path names a money verb; a plain read that needs no entry is still sent.
func TestResolved_NoEntryNeededIsTellable_FromAnEntryThatStoppedMatching(t *testing.T) {
	srv := newRecordingServer(t)
	run := func(saved string, guarded bool) report.StepResult {
		step := HTTPStep("read", "GET", srv.ts.URL+"/api/v1/items/${saved.id}", nil, "", 0, nil, nil, nil, false,
			nil, &scenario.MoneySpendLedger{}, WithMoneyHandling(guarded))
		return step.Run("cid", map[string]string{"id": saved})
	}
	if r := run("42", true); r.Status != "passed" {
		t.Fatalf("a plain read of a non-money path needs no entry and must be sent: %s %s", r.Status, r.Observed)
	}
	for _, saved := range []string{"orders", "../orders/1", "quote", "x?kind=swap"} {
		before := len(srv.seen())
		r := run(saved, true)
		if r.Status != report.StatusError || len(srv.seen()) != before {
			t.Errorf("saved %q: a money-handling run sent a request that resolved to a money path: %s %s", saved, r.Status, r.Observed)
		}
	}
	// a run that is NOT money-handling is untouched: the same values are sent
	before := len(srv.seen())
	if r := run("orders", false); r.Status != "passed" || len(srv.seen()) != before+1 {
		t.Errorf("a non-money run must be byte-identical to before: %s %s", r.Status, r.Observed)
	}
}

func TestResolved_UnreachableStepIsJudgedToo(t *testing.T) {
	srv := newRecordingServer(t)
	step := UnreachableHTTPStep("u", "GET", srv.ts.URL+"/api/v1/items/${saved.id}", nil, "", nil, &scenario.MoneySpendLedger{},
		WithMoneyHandling(true))
	res := step.Run("cid", map[string]string{"id": "orders"})
	if res.Status != report.StatusError || !strings.HasPrefix(res.Observed, MoneyResolvedRefusal) || len(srv.seen()) != 0 {
		t.Fatalf("an unreachable step was sent to a resolved order path: %s %s seen=%v", res.Status, res.Observed, srv.seen())
	}
	// and an entry that no longer matches
	step = UnreachableHTTPStep("u", "GET", srv.ts.URL+"/api/v1/orders/${saved.id}", nil, "", getOrders, &scenario.MoneySpendLedger{},
		WithMoneyHandling(true))
	res = step.Run("cid", map[string]string{"id": "1/cancel"})
	if res.Status != report.StatusError || len(srv.seen()) != 0 {
		t.Fatalf("an unreachable step whose entry stopped matching was sent: %s %s seen=%v", res.Status, res.Observed, srv.seen())
	}
}

func TestResolved_PlaceholderSpendEntryIsBoundedOnTheResolvedPathInOneBucket(t *testing.T) {
	srv := newRecordingServer(t)
	allow := scenario.MoneyWriteAllowlist{{Method: "POST", Path: "/api/v1/orders/{id}/fill", Spends: true,
		AmountField: "amount", MaxAmount: 10, MaxPerRun: 2}}
	ledger := &scenario.MoneySpendLedger{}
	mk := func(amt string) Step {
		return HTTPStep("fill", "POST", srv.ts.URL+"/api/v1/orders/${saved.id}/fill", nil, `{"amount": `+amt+`}`, 0, nil, nil, nil, false,
			allow, ledger, WithMoneyHandling(true))
	}
	// two different ids share ONE max_per_run bucket (the ledger key is the ENTRY's method + path)
	if r := mk("5").Run("cid", map[string]string{"id": "a1"}); r.Status != "passed" {
		t.Fatalf("first spend: %s %s", r.Status, r.Observed)
	}
	if r := mk("5").Run("cid", map[string]string{"id": "b2"}); r.Status != "passed" {
		t.Fatalf("second spend (another id): %s %s", r.Status, r.Observed)
	}
	r := mk("5").Run("cid", map[string]string{"id": "c3"})
	if r.Status != report.StatusError || !strings.Contains(r.Observed, "max_per_run (2)") || !strings.Contains(r.Observed, "/api/v1/orders/{id}/fill") {
		t.Fatalf("third spend on a third id must hit the ONE shared bucket and name the entry: %s %s", r.Status, r.Observed)
	}
	if got := srv.seen(); len(got) != 2 || got[0] != "/api/v1/orders/a1/fill" || got[1] != "/api/v1/orders/b2/fill" {
		t.Fatalf("exactly the two first spends reach the wire, on their RESOLVED paths: %v", got)
	}
	// over max_amount on the resolved body is refused before sending, with a fresh ledger
	over := HTTPStep("fill", "POST", srv.ts.URL+"/api/v1/orders/${saved.id}/fill", nil, `{"amount": 11}`, 0, nil, nil, nil, false,
		allow, &scenario.MoneySpendLedger{}, WithMoneyHandling(true))
	before := len(srv.seen())
	if r := over.Run("cid", map[string]string{"id": "d4"}); r.Status != report.StatusError || len(srv.seen()) != before {
		t.Fatalf("an over-max_amount spend on a placeholder entry was sent: %s %s", r.Status, r.Observed)
	}
	// and a saved id that leaves the entry's route is refused WITHOUT spending a reservation
	ledger2 := &scenario.MoneySpendLedger{}
	esc := HTTPStep("fill", "POST", srv.ts.URL+"/api/v1/orders/${saved.id}/fill", nil, `{"amount": 1}`, 0, nil, nil, nil, false,
		allow, ledger2, WithMoneyHandling(true))
	before = len(srv.seen())
	if r := esc.Run("cid", map[string]string{"id": "a1/fill/../../x"}); r.Status != report.StatusError || len(srv.seen()) != before {
		t.Fatalf("a saved id with slashes was sent: %s %s", r.Status, r.Observed)
	}
	if ok, n := ledger2.Reserve(&allow[0]); !ok || n != 1 {
		t.Errorf("a refused request consumed a reservation: ok=%v n=%d", ok, n)
	}
}

// A step that does NOT hold a `${saved.` and is not covered by an entry is the very text the file's guard
// judged: it is not re-judged on a url carrying the run's correlation id (whose scenario id the author
// chooses, and may contain a money word).
func TestResolved_AFileTextTheGuardAlreadyJudgedIsNotRejudged(t *testing.T) {
	srv := newRecordingServer(t)
	step := HTTPStep("read", "GET", srv.ts.URL+"/api/v1/items/tr-run-ORDER-1-ab12", nil, "", 0, nil, nil, nil, false,
		nil, &scenario.MoneySpendLedger{}, WithMoneyHandling(true))
	if r := step.Run("cid", map[string]string{}); r.Status != "passed" {
		t.Fatalf("a literal url with no saved value was re-judged and refused: %s %s", r.Status, r.Observed)
	}
}
