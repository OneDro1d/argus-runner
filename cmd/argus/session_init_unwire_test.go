package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Review finding 5: the usage text says an "argus" entry left by an earlier run is removed if init fails.
// Only the author-tool and sign-in refusals did it; the empty-token, unreadable-token and could-not-verify
// paths left a stale entry pointing a session at a token that is gone or bad.
func TestBuilderInit_EveryFailurePathPullsBackAStaleEntry(t *testing.T) {
	const stale = `{"mcpServers":{"argus":{"type":"http","url":"x","headersHelper":"y"},"keep":{"type":"http","url":"z"}}}`
	for _, tc := range []struct {
		name   string
		prep   func(t *testing.T, dir string, cp *testerCP)
		wantRC int
	}{
		// First half of the two-step flow: nothing is wrong yet, so the exit code stays 0 - but a stale
		// entry must not survive, because an empty token sends the MCP client to a sign-in prompt.
		{"empty token", func(t *testing.T, dir string, cp *testerCP) {}, exitOK},
		{"token that only bash could evaluate", func(t *testing.T, dir string, cp *testerCP) {
			setBuilderToken(t, dir, "$(id)")
		}, exitErr},
		{"control plane unreachable", func(t *testing.T, dir string, cp *testerCP) {
			setBuilderToken(t, dir, builderTok)
			cp.srv.Close()
		}, exitErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, cp := builderSetup(t, builderTok, []string{"runner__run"}, false)
			if rc, _ := runInit(t, "builder", "init", "msgbus", "--dir", dir, "--control-plane", cp.srv.URL); rc != exitOK {
				t.Fatal("first run failed")
			}
			tc.prep(t, dir, cp)
			mcpPath := filepath.Join(dir, ".mcp.json")
			if err := os.WriteFile(mcpPath, []byte(stale), 0o600); err != nil {
				t.Fatal(err)
			}
			rc, out := runInit(t, "builder", "init", "msgbus", "--dir", dir, "--control-plane", cp.srv.URL)
			if rc != tc.wantRC {
				t.Fatalf("rc=%d, want %d\n%s", rc, tc.wantRC, out)
			}
			mcp, _ := os.ReadFile(mcpPath)
			if strings.Contains(string(mcp), `"argus"`) || !strings.Contains(string(mcp), `"keep"`) {
				t.Errorf("the argus entry must be removed and the foreign one kept:\n%s", mcp)
			}
			if !strings.Contains(out, "was removed") {
				t.Errorf("the output does not say the stale entry was removed:\n%s", out)
			}
			if strings.Contains(out, builderTok) || strings.Contains(out, "$(id)") {
				t.Errorf("LEAK: a token value reached stdout")
			}
		})
	}
}
