package onboard

// teardown (POST /api/instances/delete) and a workspace switch (POST /api/workspaces/switch)
// accept only the owner's signed-in session. The CLI tells a personal access token of either kind to sign
// in, and never "or a token for all workspaces", which those routes refuse as well. BoundTokenMessage
// stays for the routes where it is true (enroll, register router, check instance...).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serverAnswering(t *testing.T, code int, body string) *CloudClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewCloudClient(srv.URL)
}

func TestSessionOnlyCalls_SayToSignIn_NeverToUseAnotherToken(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(c *CloudClient) error{
		"TeardownInstance": func(c *CloudClient) error { _, err := c.TeardownInstance(ctx, "odts_x", "i"); return err },
		"SwitchWorkspace":  func(c *CloudClient) error { _, err := c.SwitchWorkspace(ctx, "odts_x", "ws_b"); return err },
	}
	// the new control plane says session_required to both PAT kinds; an older one still says
	// workspace_bound_token on these two routes: the sentence must be the true one either way.
	for _, reason := range []string{"session_required", "workspace_bound_token"} {
		for name, call := range calls {
			t.Run(name+"/"+reason, func(t *testing.T) {
				c := serverAnswering(t, 403, `{"error":"server words","reason":"`+reason+`"}`)
				err := call(c)
				if err == nil {
					t.Fatal("no error")
				}
				msg := err.Error()
				for _, want := range []string{"owner's own sign-in", "argus cloud-login", "cannot do it"} {
					if !strings.Contains(msg, want) {
						t.Errorf("%q lacks %q", msg, want)
					}
				}
				for _, bad := range []string{"all workspaces", "status 403", "a token for"} {
					if strings.Contains(msg, bad) {
						t.Errorf("%q still carries %q", msg, bad)
					}
				}
				if StatusOf(err) != http.StatusForbidden {
					t.Errorf("StatusOf = %d, want 403 kept for status-based callers", StatusOf(err))
				}
			})
		}
	}
}

// BoundTokenMessage is still said where it is true.
func TestBoundTokenMessageStaysWhereItIsTrue(t *testing.T) {
	c := serverAnswering(t, 403, `{"error":"x","reason":"workspace_bound_token"}`)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"MintEnrollment": func() error { _, err := c.MintEnrollment(ctx, "odts_x", ""); return err },
		"CheckInstance":  func() error { _, err := c.CheckInstance(ctx, "odts_x", "i"); return err },
		"RegisterRouter": func() error { _, err := c.RegisterRouter(ctx, "odts_x", "r", "h", "AAAA"); return err },
	} {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "a token for all workspaces") || strings.Contains(err.Error(), "needs the owner's own sign-in") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Teardown carries the server's sentence instead of a bare status.
func TestTeardownCarriesTheServersError(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name, body string
		code       int
		want       string
	}{
		{"an error text", `{"error":"a workspace-bound token is required for the web"}`, 403, "teardown failed: status 403: a workspace-bound token is required for the web"},
		{"no body", ``, 403, "teardown failed: status 403"},
		{"a 401 keeps `status 401` (teardown.sh matches it)", `{"error":"invalid credential"}`, 401, "teardown failed: status 401: invalid credential"},
		{"a 500", `{"error":"boom"}`, 500, "teardown failed: status 500: boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := serverAnswering(t, c.code, c.body).TeardownInstance(ctx, "odts_x", "i")
			if err == nil || err.Error() != c.want {
				t.Fatalf("error = %v, want %q", err, c.want)
			}
		})
	}
}
