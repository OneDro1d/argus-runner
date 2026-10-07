package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// — `argus calm import`: the CLI over internal/calm.

func qconArch() string {
	return filepath.Join("..", "..", "testdata", "calm", "qcon-scenario2", "calm", "trades-api-and-mcp.architecture.json")
}

func qconArgs(out string) []string {
	return []string{"calm", "import", qconArch(),
		"--bind", "mcp-server=http://trades-mcp-server.calm-demo.svc.cluster.local/mcp,streamable-http",
		"--bind", "trades-api=http://trades.calm-demo.svc.cluster.local",
		"--control-arg", "mcp-guardrail.tool=PLACEHOLDER_TOOL",
		"--control-arg", "mcp-guardrail.arg=PLACEHOLDER_ARG",
		"--out", out}
}

// qconGoldenArgs are the REAL values of the live FINOS CALM QCon demo (review): the tool
// getTrades, an argument template, and a refusal that is text in an ordinary answer.
func qconGoldenArgs(out string) []string {
	return []string{"calm", "import", qconArch(),
		"--bind", "mcp-server=http://trades-mcp-server.calm-demo.svc.cluster.local/mcp,streamable-http",
		"--bind", "trades-api=http://trades.calm-demo.svc.cluster.local",
		"--control-arg", "mcp-guardrail.tool=getTrades",
		"--control-arg", `mcp-guardrail.args={"filter":"instrument eq '${symbol}'","nextLink":"","size":5}`,
		"--control-arg", "mcp-guardrail.refusal=text",
		"--control-arg", "mcp-guardrail.refused-contains=is restricted",
		"--control-arg", "mcp-guardrail.allowed=LSE:AAPL",
		"--out", out}
}

// refusalOf reads the {"error": …} line a refused command prints.
func refusalOf(t *testing.T, stdout string) string {
	t.Helper()
	var m struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatalf("a refusal must be one JSON object with an error field; got %q (%v)", stdout, err)
	}
	return m.Error
}

func TestCalm_HelpNamesTheCommandAndNeedsNoToken(t *testing.T) {
	for _, h := range []string{"--help", "-h"} {
		for _, argv := range [][]string{{"calm", h}, {"calm", "import", h}} {
			var rc int
			out := captureStdout(t, func() { captureStderr(t, func() { rc = dispatch(argv) }) })
			if rc != exitOK || !strings.Contains(out, "calm import") || !strings.Contains(out, "--bind") {
				t.Errorf("%v: exit %d, output %q", argv, rc, out)
			}
		}
	}
}
