package onboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// UC123: the instance-name charset (lowercase alphanumeric + hyphens, no leading/trailing hyphen).
func TestValidInstanceName(t *testing.T) {
	valid := []string{"orders-compose", "memstore-k3d", "a1", "x-y-z", "social1"}
	for _, n := range valid {
		if ok, msg := ValidInstanceName(n); !ok {
			t.Errorf("ValidInstanceName(%q) = false (%s), want valid", n, msg)
		}
	}
	invalid := []string{"", "-x", "x-", "Orders", "a_b", "a.b", "orders compose", "café", "orders/compose"}
	for _, n := range invalid {
		if ok, _ := ValidInstanceName(n); ok {
			t.Errorf("ValidInstanceName(%q) = true, want invalid", n)
		}
	}
}

// ONE RULE, TWO READERS. Onboarding checks the name with messages; the update path checks the same rule
// wherever it joins the id into a host path (federation.ValidInstanceID). They must never disagree: an
// id onboarding accepts and the update path refuses is an instance that can never be updated, and the
// reverse is a path the update path builds from a name nobody could onboard.
func TestValidInstanceName_AgreesWithTheFederationRule(t *testing.T) {
	for _, n := range []string{"orders-compose", "memstore1-k3d", "a", "0", "x-y-z", "", "-x", "x-", "Orders",
		"a_b", "a.b", "a b", "café", "x/../../Documents", "..", "router", "a,b", "a'b", "a\nb"} {
		ok, _ := ValidInstanceName(n)
		if got := federation.ValidInstanceID(n); got != ok {
			t.Errorf("%q: onboarding says %v, the update path says %v", n, ok, got)
		}
	}
}

// D1 (M3 fix plan R9/R11): SeedScenarios uploads each scenario to the cloud catalog via the
// author__write_scenario MCP tool. It reuses ONE streamable-HTTP session, carries the instance_id +
// path + content, RECORDS a per-scenario validation/tool failure (never aborts the whole seed), and
// counts written vs failed. A transport/handshake failure is the only hard error.
func TestSeedScenarios(t *testing.T) {
	type call struct{ instance, path, content string }
	var got []call
	sawInit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer author-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					InstanceID string `json:"instance_id"`
					Path       string `json:"path"`
					Content    string `json:"content"`
				} `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "initialize":
			sawInit = true
			w.Header().Set("Mcp-Session-Id", "sess-1")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			// VR-C5: seeding calls the control plane DIRECTLY — no router in the path — so it must
			// name the BARE cloud tool. A doubled name here would be the pre-rename regression, and
			// the CP would answer "unknown tool" for every scenario in the kit.
			if req.Params.Name != "author_write_scenario" {
				t.Errorf("unexpected tool %q", req.Params.Name)
			}
			a := req.Params.Arguments
			got = append(got, call{a.InstanceID, a.Path, a.Content})
			if strings.Contains(a.Content, "BAD") {
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"{\"written\":false,\"valid\":false,\"errors\":[\"missing ID\"]}"}],"isError":true}}`, req.ID)
			} else {
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"{\"written\":true,\"scenario_id\":\"X\"}"}],"isError":false}}`, req.ID)
			}
		}
	}))
	defer srv.Close()

	scenarios := []ScenarioFile{
		{Path: "http-ingestion/A.md", Content: "# Scenario A"},
		{Path: "database/B.md", Content: "# Scenario B BAD"},
		{Path: "http-ingestion/C.md", Content: "# Scenario C"},
	}
	res, err := NewCloudClient(srv.URL).SeedScenarios(context.Background(), "author-tok", "orders-compose", scenarios)
	if err != nil {
		t.Fatalf("SeedScenarios error: %v", err)
	}
	if !sawInit {
		t.Error("SeedScenarios must run the MCP initialize handshake")
	}
	if res.Total != 3 || res.Written != 2 || len(res.Failed) != 1 {
		t.Fatalf("SeedResult = %+v, want total 3 / written 2 / 1 failed", res)
	}
	if res.Failed[0].Path != "database/B.md" || !strings.Contains(res.Failed[0].Error, "missing ID") {
		t.Errorf("failure = %+v, want database/B.md with the validation message", res.Failed[0])
	}
	if len(got) != 3 || got[0].instance != "orders-compose" || got[0].path != "http-ingestion/A.md" || got[0].content != "# Scenario A" {
		t.Fatalf("write calls = %+v, want each carrying instance_id + path + content", got)
	}
}

// SeedScenarios returns a hard error (not a per-scenario failure) when the handshake itself fails.
func TestSeedScenarios_HandshakeFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // initialize fails
	}))
	defer srv.Close()
	if _, err := NewCloudClient(srv.URL).SeedScenarios(context.Background(), "t", "i", []ScenarioFile{{Path: "a.md", Content: "x"}}); err == nil {
		t.Fatal("SeedScenarios must error when the MCP handshake fails")
	}
}

// LoadScenarioDir walks a dir for *.md scenarios, keying each by its POSIX-relative path (so the cloud
// catalog mirrors the on-disk layout), skipping non-.md files and top-level README.md.
func TestLoadScenarioDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "http-ingestion"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("http-ingestion/A.md", "# A")
	write("database/B.md", "# B")
	write("README.md", "readme")         // skipped
	write("notes.txt", "not a scenario") // skipped
	// R1b: an installed agent's skill markdown lives under a HIDDEN dir (.claude) — it is NOT a scenario
	// and must never be seeded (the seed mounts the whole agent folder, which carries .claude/skills).
	write(".claude/skills/scenario-author/SKILL.md", "# scenario-author skill")
	got, err := LoadScenarioDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("LoadScenarioDir found %d scenarios, want 2 (hidden .claude + README + .txt skipped): %+v", len(got), got)
	}
	// sorted, POSIX-relative keys
	if got[0].Path != "database/B.md" || got[1].Path != "http-ingestion/A.md" {
		t.Fatalf("paths = [%q, %q], want [database/B.md, http-ingestion/A.md]", got[0].Path, got[1].Path)
	}
	if got[1].Content != "# A" {
		t.Errorf("content = %q, want '# A'", got[1].Content)
	}
}
