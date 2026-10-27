package onboard

// MintEnrollment names the workspace in the body when given one (a user-global author PAT
// has none of its own), sends the byte-identical `{}` when not, and carries the server's own error text.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func enrollServer(t *testing.T, status int, reply string, gotBody *string) *CloudClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return NewCloudClient(srv.URL)
}

const enrollOK = `{"enrollment_token":"enr_x","enrollment_id":"id1","workspace":"ws_b","expires_in":900}`

func TestMintEnrollment_BodyCarriesTheWorkspaceWhenGiven(t *testing.T) {
	var got string
	c := enrollServer(t, 200, enrollOK, &got)
	res, err := c.MintEnrollment(context.Background(), "odts_x", "ws_b")
	if err != nil || res.Workspace != "ws_b" {
		t.Fatalf("mint: %v %+v", err, res)
	}
	if got != `{"workspace":"ws_b"}` {
		t.Fatalf("request body = %q, want {\"workspace\":\"ws_b\"}", got)
	}
}

func TestMintEnrollment_BodyIsByteIdenticalToTodayWithoutAWorkspace(t *testing.T) {
	var got string
	c := enrollServer(t, 200, enrollOK, &got)
	if _, err := c.MintEnrollment(context.Background(), "odts_x", ""); err != nil {
		t.Fatal(err)
	}
	if got != "{}" {
		t.Fatalf("request body = %q, want the unchanged {}", got)
	}
}

func TestMintEnrollment_SurfacesTheServersErrorText(t *testing.T) {
	var got string
	c := enrollServer(t, 403, `{"error":"not your workspace"}`, &got)
	_, err := c.MintEnrollment(context.Background(), "odts_x", "ws_x")
	if err == nil || !strings.Contains(err.Error(), "not your workspace") {
		t.Fatalf("error = %v, want the server's text", err)
	}
	if StatusOf(err) != http.StatusForbidden {
		t.Fatalf("StatusOf = %d, want 403", StatusOf(err))
	}
}
