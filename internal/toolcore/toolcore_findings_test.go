package toolcore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validScenario(id, layer string) string {
	return strings.Join([]string{
		"# Scenario: " + id, "", "## Metadata",
		"- **ID**: " + id, "- **Layer**: " + layer, "- **Tags**: http, x", "",
		"## TRIGGER", "POST `${INGESTION_URL}/api/v1/orders`", "",
		"```json", "{}", "```", "",
		"## EXPECT", "### Runnable", "- status=202", "",
		"## TIMEOUT", "15s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
}

// DF-05: write_scenario resolves the destination against ScenariosDir (symmetry with
// read/list/run) and rejects `..`/absolute escapes; returns the resolved absolute path.
func TestWriteScenario_ResolvesAgainstScenariosDir(t *testing.T) {
	dir := t.TempDir()
	body := []byte(validScenario("WR-001", "HTTP Ingestion"))

	p, _, err := WriteScenario(dir, body, "http-ingestion/WR-001.md")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, e := os.Stat(filepath.Join(dir, "http-ingestion", "WR-001.md")); e != nil {
		t.Fatalf("scenario must land UNDER ScenariosDir: %v", e)
	}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), "WR-001.md") || !strings.Contains(string(b), `"written":true`) {
		t.Fatalf("result must report written:true + the resolved path: %s", b)
	}
	// `..` escape rejected
	if _, _, err := WriteScenario(dir, body, "../escape.md"); err == nil {
		t.Fatal("a `..` escape must be rejected")
	}
	// absolute path rejected
	if _, _, err := WriteScenario(dir, body, "/abs/x.md"); err == nil {
		t.Fatal("an absolute path must be rejected")
	}
}

// DF-02: list_scenarios shows the TERMINAL (run) layer + the full chain, so the listed
// layer matches `run layer:X` (the runner keys on the terminal layer).
func TestListScenarios_TerminalLayerAndChain(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CHN-001.md"), []byte(validScenario("CHN-001", "HTTP Ingestion -> Database State")), 0o644); err != nil {
		t.Fatal(err)
	}
	p, _ := ListScenarios(Env{ScenariosDir: dir})
	b, _ := json.Marshal(p)
	s := string(b)
	if !strings.Contains(s, "Database State") {
		t.Fatalf("listed layer must be the terminal (run) layer 'Database State': %s", s)
	}
	if !strings.Contains(s, "layer_chain") || !strings.Contains(s, "HTTP Ingestion") {
		t.Fatalf("must surface the full layer_chain: %s", s)
	}
}

// DF-16: propose_scenario is LAYER-AWARE — a Database State proposal scaffolds a VERIFY
// (SQL/row) skeleton, not the fixed HTTP stub. DF-03: the title is clean (no mid-word
// truncation). The draft stays argus-valid + flags its guessed fields.
func TestProposeScenario_LayerAwareCleanTitle(t *testing.T) {
	longIntent := "Verify that POST /api/v1/orders rejects an order whose item quantity exceeds the maximum allowed (1000) with HTTP 400 and a clear error"
	p, err := ProposeScenario(longIntent, "order-api", "Database State")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	var got struct {
		Drafts []struct {
			Scenario        string   `json:"scenario"`
			NeedsUserReview []string `json:"needs_user_review"`
		} `json:"drafts"`
	}
	json.Unmarshal(b, &got)
	if len(got.Drafts) == 0 {
		t.Fatal("no draft")
	}
	sc := got.Drafts[0].Scenario
	if !strings.Contains(sc, "Database State") {
		t.Fatalf("layer-aware: a Database State proposal must declare that layer:\n%s", sc)
	}
	if !strings.Contains(sc, "VERIFY") || !(strings.Contains(sc, "row") || strings.Contains(strings.ToLower(sc), "sql") || strings.Contains(sc, "SELECT")) {
		t.Fatalf("Database State proposal must scaffold a row/SQL VERIFY, not the HTTP stub:\n%s", sc)
	}
	// DF-03: clean title — the H1 line must not be a mid-word truncation of the intent.
	title := strings.SplitN(sc, "\n", 2)[0]
	if strings.HasSuffix(title, "exceeds the") || len(title) > 75 {
		t.Fatalf("title must be clean (no mid-word chop): %q", title)
	}
	if len(got.Drafts[0].NeedsUserReview) == 0 {
		t.Fatal("must still flag guessed fields")
	}
	if _, invalid, _ := ValidateScenario(sc); invalid {
		t.Fatalf("layer-aware draft must be argus-valid:\n%s", sc)
	}
}

// Unit 5 (M25-FX3 / 4.6): get_dashboard_url always offers the GENERAL (overview) dashboard
// — so a caller who wants the general one doesn't have to know to omit correlation_id — and
// carries an honesty note that a built URL is not proof the run/correlation exists.
func TestGetDashboardURL_GeneralAffordanceAndHonesty(t *testing.T) {
	e := Env{Instance: "local", Grafana: "http://localhost:3000"}
	b, _ := json.Marshal(GetDashboardURL(e, "tr-abc"))
	s := string(b)
	if !strings.Contains(s, "var-correlation_id=tr-abc") {
		t.Errorf("dashboard_url should deep-link the correlation_id: %s", s)
	}
	if !strings.Contains(s, "overview_url") {
		t.Errorf("must always offer a general overview_url affordance: %s", s)
	}
	if !strings.Contains(s, "note") {
		t.Errorf("must carry an honesty note (a URL is not proof the run exists): %s", s)
	}
	// the overview_url itself must NOT carry a correlation filter
	var m map[string]any
	json.Unmarshal(b, &m)
	if ov, _ := m["overview_url"].(string); strings.Contains(ov, "correlation_id") {
		t.Errorf("overview_url must be the GENERAL dashboard (no correlation filter): %s", ov)
	}
	// with no correlation_id, the deep link == the general dashboard
	b2, _ := json.Marshal(GetDashboardURL(e, ""))
	if strings.Contains(string(b2), "var-correlation_id") {
		t.Errorf("no correlation_id -> general dashboard, got: %s", b2)
	}
}

// E1 (R12): GetDashboardURL scopes the deep-link to the RUN. It derives the run id embedded in the
// correlation id (tr-<run_id>-…) and sets var-current_run so the dashboard's run-scoped panels — whose
// "Run ID" variable has no default — actually focus (the "nothing usable" fix).
func TestGetDashboardURL_ScopesToRun(t *testing.T) {
	e := Env{Instance: "local", Grafana: "http://g:3000", ResultsRoot: t.TempDir()}
	s := string(mustJSON(t, GetDashboardURL(e, "tr-20260715T120000-ORD-006-abcd")))
	if !strings.Contains(s, "var-current_run=20260715T120000") {
		t.Errorf("dashboard_url must scope to the run (var-current_run derived from the correlation id): %s", s)
	}
	if !strings.Contains(s, "var-correlation_id=tr-20260715T120000-ORD-006-abcd") {
		t.Errorf("dashboard_url must keep the correlation scope: %s", s)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// FX-3 (M25): a skill/example doc under a dot-dir (e.g. .claude/skills/scenario-author/
// SKILL.md, ID <PREFIX>-<NNN>) must NOT be listed or counted as a scenario — it was the
// phantom that ran + 404'd on the colleague rig and inflated scenarios_found.
func TestListAndCount_ExcludePhantomUnderDotDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ORDE-001.md"), []byte(validScenario("ORDE-001", "HTTP Ingestion")), 0o644)
	phantom := filepath.Join(dir, ".claude", "skills", "scenario-author")
	os.MkdirAll(phantom, 0o755)
	os.WriteFile(filepath.Join(phantom, "SKILL.md"), []byte(validScenario("<PREFIX>-<NNN>", "<Layer>...")), 0o644)

	p, _ := ListScenarios(Env{ScenariosDir: dir})
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "PREFIX") {
		t.Fatalf("phantom under .claude must be excluded from list_scenarios: %s", b)
	}
	if !strings.Contains(string(b), "ORDE-001") {
		t.Fatalf("the real scenario must still be listed: %s", b)
	}
	if got := scenarioLayers(dir); len(got) != 1 || got[0] != "HTTP Ingestion" {
		t.Fatalf("scenario_layers must exclude the placeholder <Layer>, got %v", got)
	}
}

// FX-2 (M25): author__validate_scenario's `scenario` path arg must resolve RELATIVE to
// the scenarios dir (symmetry with read/list/write/delete) — not the container cwd, which
// made the colleague's path-based validate fail "no such file or directory".
func TestValidateScenarioAt_ResolvesUnderScenariosDir(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := WriteScenario(dir, []byte(validScenario("VAL-001", "HTTP Ingestion")), "http/VAL-001.md"); err != nil {
		t.Fatal(err)
	}
	// a path RELATIVE to the scenarios dir validates (this is what the MCP arg carries)
	p, invalid, err := ValidateScenarioAt(dir, "http/VAL-001.md")
	if err != nil {
		t.Fatalf("validate-by-path under scenarios dir must work: %v", err)
	}
	if invalid {
		t.Fatalf("a valid scenario must validate, got %v", p)
	}
	// a missing path errors clearly (mentions the scenarios dir we looked under)
	_, _, err = ValidateScenarioAt(dir, "http/missing.md")
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("missing path must error and name the scenarios dir, got %v", err)
	}
}

// DF-12: validate_config must distinguish config TARGETS from scenario LAYERS.
func TestScenarioLayers_DistinctTerminal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "CHN-001.md"), []byte(validScenario("CHN-001", "HTTP Ingestion -> Database State")), 0o644)
	os.WriteFile(filepath.Join(dir, "ORD-001.md"), []byte(validScenario("ORD-001", "HTTP Ingestion")), 0o644)
	got := scenarioLayers(dir)
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "Database State") || !strings.Contains(joined, "HTTP Ingestion") {
		t.Fatalf("scenario_layers must list distinct terminal layers, got %v", got)
	}
}
