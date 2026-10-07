package obsquery

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// under --obs=export with a hosted Loki the executor's --loki is the hosted QUERY url,
// so the request-event push went to <query-url>/loki/api/v1/push with NO credential — a hosted Loki
// refuses that, and Panel A stayed empty. The push must go to the declared push_url, VERBATIM (it
// is already the full endpoint), with the same Basic Auth the query side sends.

type pushRecorder struct {
	mu        sync.Mutex
	path      string
	user, pwd string
	hasAuth   bool
	calls     int
}

func newPushRecorder(t *testing.T) (*pushRecorder, *httptest.Server) {
	t.Helper()
	rec := &pushRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.calls++
		rec.path = r.URL.Path
		rec.user, rec.pwd, rec.hasAuth = r.BasicAuth()
		rec.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func TestPushRequestEvents_UsesPushURLVerbatimWithBasicAuth(t *testing.T) {
	rec, srv := newPushRecorder(t)
	// BaseURL deliberately unreachable: the push must not go anywhere near the query side.
	l := &Loki{BaseURL: "http://query-side.invalid", PushURL: srv.URL + "/hosted/push", Credential: "hosted-user:hosted-pass"}
	if err := PushRequestEvents(l, "local", "p", "aks", oneRequestReport()); err != nil {
		t.Fatal(err)
	}
	if rec.path != "/hosted/push" {
		t.Errorf("the push must POST to push_url verbatim, got path %q", rec.path)
	}
	if !rec.hasAuth || rec.user != "hosted-user" || rec.pwd != "hosted-pass" {
		t.Errorf("the push must carry the resolved Basic-Auth credential, got user=%q ok=%v", rec.user, rec.hasAuth)
	}
}

func TestPushRequestEvents_NoAuthorizationWithoutCredential(t *testing.T) {
	rec, srv := newPushRecorder(t)
	if err := PushRequestEvents(&Loki{BaseURL: srv.URL}, "local", "p", "k3d", oneRequestReport()); err != nil {
		t.Fatal(err)
	}
	if rec.calls == 0 {
		t.Fatal("the fake Loki was never called — the test proves nothing")
	}
	if rec.path != "/loki/api/v1/push" {
		t.Errorf("with no push_url the push keeps the BaseURL endpoint, got path %q", rec.path)
	}
	if rec.hasAuth {
		t.Error("with no credential the push must send NO Authorization header")
	}
}

func TestPushRequestEvents_NilLokiIsANoOp(t *testing.T) {
	if err := PushRequestEvents(nil, "local", "p", "k3d", oneRequestReport()); err != nil {
		t.Fatalf("a nil Loki must be a no-op, got %v", err)
	}
}
