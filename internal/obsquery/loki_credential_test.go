package obsquery

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// T3.1 (E3 export hosted-Loki target): obsquery.Loki sends HTTP Basic Auth when a Credential is
// configured, and NO Authorization header at all when it is not (bundled/adopt, every pre-T3.1
// config) — a proxy in front of a hosted Loki may treat an EMPTY Authorization differently from
// an ABSENT one, so "no credential" must mean the header never gets set.

func lokiFakeServer(t *testing.T, gotAuth *string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"result":[]}}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestLoki_QueryLines_SendsBasicAuthWhenCredentialSet(t *testing.T) {
	var gotAuth string
	ts := lokiFakeServer(t, &gotAuth)
	l := &Loki{BaseURL: ts.URL, Credential: "hosted-user:hosted-pass"}

	l.Logs("tr-20260902T134305716-FX-ABC-00000003", "1h", time.Time{})

	want := "Basic aG9zdGVkLXVzZXI6aG9zdGVkLXBhc3M=" // base64("hosted-user:hosted-pass")
	if gotAuth != want {
		t.Errorf("Authorization header = %q, want %q", gotAuth, want)
	}
}

func TestLoki_QueryLines_NoAuthorizationHeaderWithoutCredential(t *testing.T) {
	var gotAuth string
	ts := lokiFakeServer(t, &gotAuth)
	l := &Loki{BaseURL: ts.URL} // no Credential — the bundled/adopt/pre-T3.1 case

	l.Logs("tr-20260902T134305716-FX-ABC-00000003", "1h", time.Time{})

	if gotAuth != "" {
		t.Errorf("Authorization header = %q, want NO header at all (empty string) when no credential is configured", gotAuth)
	}
}
