package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/auth"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// AC-D69 (design B7, "regression tests for the shared tool server"): the executor and the control plane leave
// SessionTools and UnknownTool nil, and must answer EXACTLY as before. These pin the answers byte for byte where a
// byte can change: the initialize result, the unknown-tool error, the closed-schema refusal, the static list.
// hooks_unset_ext_test.go does the same with the control plane's real tool set.

func executorServer(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer(auth.Config{RunnerToken: rtok, AuthorToken: atok}, DefaultTools(toolcore.Env{Instance: "local"})...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHooksUnset_InitializeAnswersExactlyAsBefore(t *testing.T) {
	s := executorServer(t)
	s.NewSession("s")
	raw, _ := s.Dispatch("s", rtok, reqBytes(1, "initialize", map[string]any{}))
	want := `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{}},"protocolVersion":"2024-11-05","serverInfo":{"name":"argus","version":"M2.5"}}}`
	if string(raw) != want {
		t.Fatalf("initialize changed with the hooks unset\n got: %s\nwant: %s", raw, want)
	}
}

func TestHooksUnset_ListUnknownAndClosedSchemaAnswerAsBefore(t *testing.T) {
	s := executorServer(t)
	handshake(t, s, "s", atok)
	raw, _ := s.Dispatch("s", atok, reqBytes(2, "tools/list", nil))
	var lr struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	_ = json.Unmarshal(raw, &lr)
	var names []string
	for _, tl := range lr.Result.Tools {
		names = append(names, tl.Name)
	}
	// AC-D70 renamed the executor's tools to one underscore on purpose (design #678 B5); the old names are hidden aliases
	// (runner_rename_test.go). Everything else pinned here is unchanged.
	if got := strings.Join(names, ","); got != "runner_validate_config,runner_run,runner_get_report,runner_get_sagas,runner_get_tail_logs,runner_get_dashboard_url" {
		t.Fatalf("the executor's static list changed: %s", got)
	}

	raw, _ = s.Dispatch("s", atok, reqBytes(3, "tools/call", map[string]any{"name": "author__write_scenario", "arguments": map[string]any{}}))
	if want := `{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"unknown tool author__write_scenario"}}`; string(raw) != want {
		t.Fatalf("unknown tool answer changed\n got: %s\nwant: %s", raw, want)
	}

	// The closed-schema check still runs for a STATIC tool (it is skipped only for session tools).
	raw, _ = s.Dispatch("s", atok, reqBytes(4, "tools/call", map[string]any{"name": "runner_get_dashboard_url",
		"arguments": map[string]any{"instance_id": "local", "not_an_argument": "x"}}))
	if !strings.Contains(string(raw), `unknown argument`) || !strings.Contains(string(raw), `not_an_argument`) || !strings.Contains(string(raw), `"isError":true`) {
		t.Fatalf("the closed-schema refusal is gone for a static tool: %s", raw)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if sess.sets != nil {
			t.Fatalf("session %q stored a tool set with the hooks unset", id)
		}
	}
	if s.building != nil {
		t.Fatal("a build was started with the hooks unset")
	}
}
