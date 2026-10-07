package onboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mcpStub answers the streamable-HTTP handshake. tools is what tools/list returns; unauth makes every
// request a 401 with the WWW-Authenticate challenge the real control plane sends.
func mcpStub(t *testing.T, tools []string, unauth bool, gotBearer *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotBearer != nil {
			*gotBearer = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if unauth {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://cp.example/.well-known/oauth-protected-resource"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"unauthorized"}}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(b, &req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		case "tools/list":
			var ts []map[string]string
			for _, n := range tools {
				ts = append(ts, map[string]string{"name": n})
			}
			out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "result": map[string]any{"tools": ts}})
			_, _ = w.Write(out)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestToolsList_ReturnsNamesAndPresentsTheBearer(t *testing.T) {
	var bearer string
	srv := mcpStub(t, []string{"runner__run", "runner__get_report"}, false, &bearer)
	names, err := NewCloudClient(srv.URL).ToolsList(context.Background(), "tok-123")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "runner__run" {
		t.Fatalf("names = %v", names)
	}
	if bearer != "tok-123" {
		t.Errorf("the control plane was not presented the token (saw %q)", bearer)
	}
}

func TestToolsList_401WithChallengeIsASignInChallengeNotAnEmptyList(t *testing.T) {
	srv := mcpStub(t, nil, true, nil)
	names, err := NewCloudClient(srv.URL).ToolsList(context.Background(), "bad")
	if names != nil {
		t.Errorf("names = %v, want none", names)
	}
	var sc *SignInChallengeError
	if !errors.As(err, &sc) {
		t.Fatalf("err = %v, want *SignInChallengeError", err)
	}
	if sc.Status != http.StatusUnauthorized || !strings.Contains(sc.WWWAuthenticate, "resource_metadata") {
		t.Errorf("challenge = %+v", sc)
	}
}
