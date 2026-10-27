package chain

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// moneyallow_redirect_test.go — A REDIRECT IS A SECOND REQUEST. The step's request is
// judged before it is sent; the request a server's 30x then chooses was not. A real
// http.Client fires at real httptest servers, so "nothing refused is sent" is proven on the wire.

type hopServer struct {
	mu   sync.Mutex
	seen []string // "METHOD URI body"
	ts   *httptest.Server
}

// newHopServer answers a request for a path in redirects with the given status and Location, and
// everything else with 200; it records every request it receives, with the body.
func newHopServer(t *testing.T, redirects map[string][2]string) *hopServer {
	t.Helper()
	h := &hopServer{}
	h.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.seen = append(h.seen, r.Method+" "+r.RequestURI+" "+string(b))
		h.mu.Unlock()
		if rd, ok := redirects[r.URL.Path]; ok {
			w.Header().Set("Location", rd[1])
			switch rd[0] {
			case "301":
				w.WriteHeader(301)
			case "303":
				w.WriteHeader(303)
			case "307":
				w.WriteHeader(307)
			default:
				w.WriteHeader(302)
			}
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(h.ts.Close)
	return h
}

func (h *hopServer) got() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

func (h *hopServer) received(substr string) bool {
	for _, s := range h.got() {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

var quoteOnly = scenario.MoneyWriteAllowlist{{Method: "POST", Path: "/api/v1/pay/quote", Spends: false}}

func wantRedirectRefusal(t *testing.T, res report.StepResult) {
	t.Helper()
	if res.Status != report.StatusError || !strings.HasPrefix(res.Observed, MoneyResolvedRefusal) || !strings.Contains(res.Observed, "redirect") {
		t.Errorf("want an error carrying the refusal wording and naming a redirect, got %s / %s", res.Status, res.Observed)
	}
}

// (a) no list: a redirect to an order path.
func TestRedirect_NoList_ToAnOrderPathIsNotSent(t *testing.T) {
	srv := newHopServer(t, map[string][2]string{"/api/v1/items/1": {"302", "/api/v1/orders/1"}})
	step := HTTPStep("read", "GET", srv.ts.URL+"/api/v1/items/1", nil, "", 0, nil, nil, nil, false,
		nil, &scenario.MoneySpendLedger{}, WithMoneyHandling(true))
	res := step.Run("cid", map[string]string{})
	t.Logf("(a) status=%s observed=%q server received=%q", res.Status, res.Observed, srv.got())
	if srv.received("/api/v1/orders/1") {
		t.Errorf("(a) the server received a request to the order path it redirected to: %q", srv.got())
	}
	wantRedirectRefusal(t, res)
	if strings.Contains(res.Observed, "/api/v1/orders/1") {
		t.Errorf("the refusal names the resolved target: %s", res.Observed)
	}
}

// (b) a list allowing only POST quote: a 307 to POST order carries the method and the body.
func TestRedirect_307_ToANotAllowedPathIsNotSentWithItsBody(t *testing.T) {
	srv := newHopServer(t, map[string][2]string{"/api/v1/pay/quote": {"307", "/api/v1/pay/order"}})
	step := HTTPStep("quote", "POST", srv.ts.URL+"/api/v1/pay/quote", nil, `{"amount": 5}`, 0, nil, nil, nil, false,
		quoteOnly, &scenario.MoneySpendLedger{}, WithMoneyHandling(true))
	res := step.Run("cid", map[string]string{})
	t.Logf("(b) status=%s observed=%q server received=%q", res.Status, res.Observed, srv.got())
	if srv.received("/api/v1/pay/order") {
		t.Errorf("(b) the server received the redirected request: %q", srv.got())
	}
	wantRedirectRefusal(t, res)
}

// (c) a redirect to ANOTHER host, on a path that would pass on this one.
func TestRedirect_ToAnotherHostIsNotSent(t *testing.T) {
	other := newHopServer(t, nil)
	srv := newHopServer(t, map[string][2]string{"/api/v1/items/1": {"302", other.ts.URL + "/api/v1/items/2"}})
	step := HTTPStep("read", "GET", srv.ts.URL+"/api/v1/items/1", nil, "", 0, nil, nil, nil, false,
		nil, &scenario.MoneySpendLedger{}, WithMoneyHandling(true))
	res := step.Run("cid", map[string]string{})
	t.Logf("(c) status=%s observed=%q other host received=%q", res.Status, res.Observed, other.got())
	if len(other.got()) != 0 {
		t.Errorf("(c) another host received a request: %q", other.got())
	}
	wantRedirectRefusal(t, res)
	if strings.Contains(res.Observed, other.ts.URL) {
		t.Errorf("the refusal names the other host's address: %s", res.Observed)
	}
}

// A redirect to an allowed path of the same host is followed on a guarded run.
func TestRedirect_ToAnAllowedPathOnTheSameHostIsFollowed(t *testing.T) {
	srv := newHopServer(t, map[string][2]string{"/api/v1/items/1": {"302", "/api/v1/items/2"}})
	step := HTTPStep("read", "GET", srv.ts.URL+"/api/v1/items/1", nil, "", 200, nil, nil, nil, false,
		nil, &scenario.MoneySpendLedger{}, WithMoneyHandling(true))
	res := step.Run("cid", map[string]string{})
	if res.Status != "passed" || !srv.received("GET /api/v1/items/2") {
		t.Fatalf("an allowed same-host redirect was not followed: %s %s %q", res.Status, res.Observed, srv.got())
	}
}

// A 303 after an allowed POST turns into a GET; a GET path an entry allows is followed.
func TestRedirect_303AfterAnAllowedPostToAnAllowedGetIsFollowed(t *testing.T) {
	srv := newHopServer(t, map[string][2]string{"/api/v1/pay/quote": {"303", "/api/v1/pay/result"}})
	allow := scenario.MoneyWriteAllowlist{
		{Method: "POST", Path: "/api/v1/pay/quote", Spends: false},
		{Method: "GET", Path: "/api/v1/pay/result", Spends: false},
	}
	step := HTTPStep("quote", "POST", srv.ts.URL+"/api/v1/pay/quote", nil, `{"a":1}`, 200, nil, nil, nil, false,
		allow, &scenario.MoneySpendLedger{}, WithMoneyHandling(true))
	res := step.Run("cid", map[string]string{})
	if res.Status != "passed" || !srv.received("GET /api/v1/pay/result") {
		t.Fatalf("a 303 to an allowed GET was not followed: %s %s %q", res.Status, res.Observed, srv.got())
	}
}

// A hop whose matched entry spends is refused by name, whatever the method.
func TestRedirect_ToASpendingEntryIsNotSent(t *testing.T) {
	srv := newHopServer(t, map[string][2]string{"/api/v1/pay/quote": {"307", "/api/v1/pay/order"}})
	allow := scenario.MoneyWriteAllowlist{
		{Method: "POST", Path: "/api/v1/pay/quote", Spends: false},
		{Method: "POST", Path: "/api/v1/pay/order", Spends: true, AmountField: "amount", MaxAmount: 10, MaxPerRun: 5},
	}
	step := HTTPStep("quote", "POST", srv.ts.URL+"/api/v1/pay/quote", nil, `{"amount": 5}`, 0, nil, nil, nil, false,
		allow, &scenario.MoneySpendLedger{}, WithMoneyHandling(true))
	res := step.Run("cid", map[string]string{})
	wantRedirectRefusal(t, res)
	if !strings.Contains(res.Observed, "a spending request is never re-sent on a redirect") {
		t.Errorf("the refusal does not say a spend is never re-sent: %s", res.Observed)
	}
	if srv.received("/api/v1/pay/order") {
		t.Errorf("a spending request was re-sent on a redirect: %q", srv.got())
	}
}

// An unguarded run follows a redirect anywhere, as before.
func TestRedirect_UnguardedRunStillFollowsToAnywhere(t *testing.T) {
	other := newHopServer(t, nil)
	srv := newHopServer(t, map[string][2]string{"/api/v1/items/1": {"302", other.ts.URL + "/api/v1/orders/1"}})
	step := HTTPStep("read", "GET", srv.ts.URL+"/api/v1/items/1", nil, "", 200, nil, nil, nil, false,
		nil, &scenario.MoneySpendLedger{}, WithMoneyHandling(false))
	res := step.Run("cid", map[string]string{})
	if res.Status != "passed" || !other.received("GET /api/v1/orders/1") {
		t.Fatalf("an unguarded run did not follow the redirect: %s %s %q", res.Status, res.Observed, other.got())
	}
}

// A poll does not retry a redirect refusal: the first attempt's refusal ends the step.
func TestRedirect_PollDoesNotRetryARefusal(t *testing.T) {
	srv := newHopServer(t, map[string][2]string{"/api/v1/items/1": {"302", "/api/v1/orders/1"}})
	step := HTTPStep("read", "GET", srv.ts.URL+"/api/v1/items/1", nil, "", 200, nil, nil,
		&Poll{Timeout: 2 * time.Second, Interval: 20 * time.Millisecond}, false,
		nil, &scenario.MoneySpendLedger{}, WithMoneyHandling(true))
	res := step.Run("cid", map[string]string{})
	wantRedirectRefusal(t, res)
	if n := len(srv.got()); n != 1 {
		t.Errorf("the poll retried a refused redirect: the server saw %d requests: %q", n, srv.got())
	}
}
