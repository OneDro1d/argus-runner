package runner

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"
)

// AC-D61 — THE TEARDOWN READS BACK ITS OWN DE-REGISTER.
//
// RegistrationPresent asks `GET /fed/state` with the executor's own machine JWT. The control plane
// authenticates before it reads state, so a deleted row answers 401 unknown_instance and a live one
// answers 200 — the control plane's own answer, with a credential the teardown already holds.
// ⛔ Every OTHER outcome is an error, never "gone": "I could not look" is not "it is gone".
func TestClient_RegistrationPresent(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	ask := func(t *testing.T, status int, body string) (bool, error, string) {
		t.Helper()
		var gotPath, gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotAuth = r.Method+" "+r.URL.Path, r.Header.Get("Authorization")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		present, err := NewClient(srv.URL, "inst-1", priv).RegistrationPresent(context.Background())
		if gotAuth == "" || len(gotAuth) < 8 {
			t.Fatalf("the read-back presented no machine JWT (Authorization=%q)", gotAuth)
		}
		return present, err, gotPath
	}

	t.Run("a deleted row answers unknown_instance: GONE", func(t *testing.T) {
		present, err, path := ask(t, 401, `{"reason":"unknown_instance"}`)
		if err != nil || present {
			t.Fatalf("present=%v err=%v, want present=false err=nil", present, err)
		}
		if path != "GET /fed/state" {
			t.Fatalf("asked %q, want the read-only GET /fed/state (never an effect verb)", path)
		}
	})

	t.Run("a live row answers the state: STILL REGISTERED", func(t *testing.T) {
		present, err, _ := ask(t, 200, `{"running_run_id":""}`)
		if err != nil || !present {
			t.Fatalf("present=%v err=%v, want present=true err=nil", present, err)
		}
	})

	t.Run("a 200 that is not a state answer is not proof of anything", func(t *testing.T) {
		if _, err, _ := ask(t, 200, `<html>sign in</html>`); err == nil {
			t.Fatal("an HTML 200 was read as an answer")
		}
	})

	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"a bad signature is not gone", 401, `{"reason":"bad_signature"}`},
		{"a 401 that is not a reject body is not gone", 401, `nope`},
		{"a 500 is not gone", 500, `boom`},
		{"a 404 from a proxy is not gone", 404, `not found`},
	} {
		t.Run(c.name, func(t *testing.T) {
			present, err, _ := ask(t, c.status, c.body)
			if err == nil {
				t.Fatalf("present=%v with NO error: an unanswered question must never read as an answer", present)
			}
		})
	}

	t.Run("an unreachable control plane is not gone", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		if _, err := NewClient(url, "inst-1", priv).RegistrationPresent(context.Background()); err == nil {
			t.Fatal("a refused connection was read as an answer")
		}
	})
}
