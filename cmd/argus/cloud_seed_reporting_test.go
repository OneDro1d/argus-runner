package main

// cloud_seed_reporting_test.go — VR10-S4-6/7/9 (V28-006): AN IMPORT NEVER DROPS A FILE IN SILENCE.
//
// Measured by Bartek on a real onboarding run against Memstore dev, 2026-09-01: 46 of 50 scenarios
// materialized. Four were refused and the operator was told NOTHING — not the names, not the reason,
// and not even that fifty had been offered. A missing test cannot fail, so the run went green: the
// absence of a test looked like the absence of a problem, at the exact moment people trust the tool
// most — their first onboard.
//
// Three things have to be true of the seed, and each is asserted on what an OPERATOR sees, not on the
// importer's return value:
//
//   - every reject is named, with its reason               (VR10-S4-6)
//   - the ARITHMETIC "imported N of M" always prints       (VR10-S4-7) — a bare "imported 46" cannot
//     be checked by someone who does not know 50 were offered, which is exactly the operator here
//   - the exit code is non-zero when anything was rejected (VR10-S4-9) — so a script caller that
//     reads nothing but $? still cannot miss it
//
// The valid files are still written: refusing the whole import for one bad file was considered and
// rejected — a colleague's first onboard should not fail on somebody else's typo.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedStubCP answers the MCP handshake and refuses exactly the writes whose content holds "BAD",
// with the control plane's own validation payload — the shape SeedScenarios has to render.
func seedStubCP(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					Content string `json:"content"`
				} `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if strings.Contains(req.Params.Arguments.Content, "BAD") {
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":`+
					`"{\"written\":false,\"valid\":false,\"errors\":[{\"line\":4,\"message\":\"ID \\\"my scenario\\\" is not allowed\"}]}"}],"isError":true}}`, req.ID)
				return
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"{\"written\":true}"}],"isError":false}}`, req.ID)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedRun writes `files` (name -> content) into a temp scenarios dir, runs the real
// cloud-seed-scenarios command against the stub, and returns its exit code, the JSON it emitted on
// stdout, and everything it wrote to stderr — the operator's console.
func seedRun(t *testing.T, files map[string]string) (int, map[string]any, string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := seedStubCP(t)

	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldErr := os.Stderr
	os.Stderr = errW
	errDone := make(chan string, 1)
	go func() { b, _ := io.ReadAll(errR); errDone <- string(b) }()

	rc := 999
	out := captureEmitRaw(t, func() {
		rc = dispatch([]string{"cloud-seed-scenarios", "--control-plane", srv.URL, "--token", "author-tok",
			"--instance-id", "inst-1", "--scenarios", dir})
	})
	_ = errW.Close()
	os.Stderr = oldErr
	stderr := <-errDone

	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("cloud-seed-scenarios did not emit JSON on stdout: %v\nout: %s", err, out)
	}
	return rc, doc, stderr
}

// ⭐ THE MEASURED CASE, scaled down: five files offered, one refused. The operator must be able to
// read the arithmetic, the name of the file that did not make it, and the reason — from the console,
// without opening a web page to find out what the command already knew.
func TestCloudSeedScenarios_NamesEveryRejectAndPrintsTheArithmetic(t *testing.T) {
	rc, doc, stderr := seedRun(t, map[string]string{
		"http-ingestion/A.md": "# Scenario A",
		"http-ingestion/B.md": "# Scenario B",
		"database/C.md":       "# Scenario C",
		"database/D.md":       "# Scenario D",
		"database/E.md":       "# Scenario E BAD",
	})

	if !strings.Contains(stderr, "imported 4 of 5") {
		t.Errorf("the arithmetic `imported 4 of 5` never printed. A bare count cannot be checked by\n"+
			"  someone who does not know how many files were offered — which is the operator on a\n"+
			"  first onboard.\n  stderr was:\n%s", stderr)
	}
	if !strings.Contains(stderr, "database/E.md") {
		t.Errorf("the rejected file was not NAMED. stderr was:\n%s", stderr)
	}
	if !strings.Contains(stderr, "is not allowed") {
		t.Errorf("the REASON the file was rejected never reached the operator, so they cannot fix it.\n"+
			"  stderr was:\n%s", stderr)
	}
	if rc == 0 {
		t.Error("VR10-S4-9: the seed exited 0 with a rejected file. A script caller that reads nothing " +
			"but $? — onboard.sh — cannot tell a clean import from a lossy one.")
	}
	// The valid files were still written: one typo must not fail a colleague's first onboard.
	if doc["seeded"] != float64(4) || doc["total"] != float64(5) {
		t.Errorf("emitted seeded/total = %v/%v, want 4/5", doc["seeded"], doc["total"])
	}
	failed, _ := doc["failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("emitted failed[] = %v, want the one refused file (the JSON is what onboard.sh parses)", doc["failed"])
	}
	entry, _ := failed[0].(map[string]any)
	if entry["path"] != "database/E.md" || !strings.Contains(fmt.Sprint(entry["error"]), "is not allowed") {
		t.Errorf("failed[0] = %v, want {path: database/E.md, error: the reason}", entry)
	}
}

// VR10-S4-7: the arithmetic prints on a CLEAN import too. "Always" is the whole point — an operator
// who only ever sees a number when something went wrong has no baseline to compare against.
func TestCloudSeedScenarios_TheArithmeticPrintsWhenNothingWasRejected(t *testing.T) {
	rc, _, stderr := seedRun(t, map[string]string{
		"http-ingestion/A.md": "# Scenario A",
		"database/B.md":       "# Scenario B",
	})
	if !strings.Contains(stderr, "imported 2 of 2") {
		t.Errorf("a clean import printed no arithmetic. stderr was:\n%s", stderr)
	}
	if rc != 0 {
		t.Errorf("a clean import exited %d, want 0", rc)
	}
}
