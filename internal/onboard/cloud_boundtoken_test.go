package onboard

// a workspace-bound token is refused (403, reason workspace_bound_token) on the routes
// onboarding and teardown use. The operator must be told so at the FIRST refused call, in plain words —
// not "status 403". The recognition lives in CloudClient.do, so every caller gets it.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func boundTokenServer(t *testing.T, body string) *CloudClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewCloudClient(srv.URL)
}

func TestBoundTokenRefusalIsSaidPlainlyByEveryOnboardingCall(t *testing.T) {
	c := boundTokenServer(t, `{"error":"this token is bound to one workspace and acts only inside it","reason":"workspace_bound_token"}`)
	ctx := context.Background()
	calls := map[string]func() error{
		"MintEnrollment":   func() error { _, err := c.MintEnrollment(ctx, "odts_x"); return err },
		"RevokeEnrollment": func() error { _, err := c.RevokeEnrollment(ctx, "odts_x", "enr1"); return err },
		"CheckInstance":    func() error { _, err := c.CheckInstance(ctx, "odts_x", "i"); return err },
		"InstanceAvailable": func() error {
			_, err := c.InstanceAvailable(ctx, "odts_x", "i")
			return err
		},
		"RegisterRouter":   func() error { _, err := c.RegisterRouter(ctx, "odts_x", "r", "h", "AAAA"); return err },
		"TeardownInstance": func() error { _, err := c.TeardownInstance(ctx, "odts_x", "i"); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil || !strings.Contains(err.Error(), "bound to one workspace") ||
				!strings.Contains(err.Error(), "cloud-login") || strings.Contains(err.Error(), "status 403") {
				t.Fatalf("%s: operator sees %q, want the plain bound-token message", name, err)
			}
			if StatusOf(err) != http.StatusForbidden {
				t.Fatalf("%s: StatusOf = %d, want 403 kept for status-based callers", name, StatusOf(err))
			}
		})
	}
}

// Control: any OTHER 403 keeps its old, bare text — the recognition is keyed on the reason, nothing else.
func TestOtherForbiddenAnswersAreUnchanged(t *testing.T) {
	for _, body := range []string{`{"error":"author-scope only"}`, `{"error":"x","reason":"something_else"}`, `not json`} {
		c := boundTokenServer(t, body)
		_, err := c.MintEnrollment(context.Background(), "odts_x")
		if err == nil || err.Error() != "mint enrollment failed: status 403" {
			t.Fatalf("body %q: error = %v, want the unchanged %q", body, err, "mint enrollment failed: status 403")
		}
	}
}
