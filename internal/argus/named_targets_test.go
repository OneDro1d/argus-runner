package argus

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// VR10-S3 (V28-012): a scenario's `**Target**` word (a chain step's "target") selects a NAMED entry
// of the matching kind; a missing or wrong-kind name is refused by name at preflight and NEVER falls
// back to the plain slot — that silent fallback is how NEO-010 quietly hit the gateway.

// mustProps derives the -J props for a scenario that selects no (or a valid) target; a refusal is
// a test failure. The older DeriveProps tests read through it.
func mustProps(t *testing.T, c *config.Config, s *scenario.Scenario, corr string) map[string]string {
	t.Helper()
	p, err := DeriveProps(c, s, corr)
	if err != nil {
		t.Fatalf("DeriveProps refused %s: %v", s.ID, err)
	}
	return p
}

// recordingRunner records the -J props of every JMeter run it is asked for (and passes them all).
type recordingRunner struct {
	fakeRunner
	props []map[string]string
}

func (r *recordingRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	r.props = append(r.props, props)
	if r.pass == nil {
		r.pass = map[string]bool{}
	}
	r.pass[props["scenario.id"]] = true
	return r.fakeRunner.Run(templateBase, jtlPath, props, 0)
}

func writeMD(t *testing.T, dir, sub, id, layer, extraMeta, trigger string) {
	t.Helper()
	d := filepath.Join(dir, sub)
	_ = os.MkdirAll(d, 0o755)
	md := "# Scenario: " + id + "\n\n## Metadata\n- **ID**: " + id + "\n- **Layer**: " + layer + "\n" + extraMeta +
		"\n## TRIGGER\n" + trigger + "\n\n## EXPECT\n### Runnable\n- status=200\n"
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadCfg(t *testing.T, body string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return c
}

const memstoreTwoSurfaces = `project:
  name: memstore
targets:
  http:
    base_url: http://memstore-gateway:8090
  http_targets:
    graph:
      base_url: http://memstore-graph:8096
  database:
    jdbc_url: jdbc:postgresql://memstore-pg:5432/memstore
    username: ro
    password: pg
  database_targets:
    neo4j:
      jdbc_url: jdbc:neo4j://memstore-neo4j:7687
      username: neo4j
      password: neo
  message_broker:
    url: amqp://u:p@main-rabbit:5672/
    management_url: http://main-rabbit:15672
    queues:
      incoming: main.q
  message_broker_targets:
    audit:
      url: amqp://a:b@audit-rabbit:5672/
      management_url: http://audit-rabbit:15672
      queues:
        incoming: audit.q
`

// Acceptance 1 (positive control, RED first): NEO-010 with `**Target**: graph` runs against
// memstore-graph:8096 — on today's tree the same file hits the gateway. A scenario WITHOUT the word
// keeps the plain slot (VR10-S3-12). A typo is refused by name with the declared names + nearest,
// and the runner is NEVER asked to fire it against the plain slot.
func TestRunAll_HTTPTarget_SelectsNamedSurface_RefusesUnknownByName(t *testing.T) {
	dir := t.TempDir()
	sc := filepath.Join(dir, "scenarios")
	writeMD(t, sc, "http-ingestion", "NEO-010", "HTTP Ingestion", "- **Target**: graph\n", "GET `http://memstore-graph:8096/health`")
	writeMD(t, sc, "http-ingestion", "NEO-012", "HTTP Ingestion", "- **Target**: grap\n", "GET `http://memstore-graph:8096/health`")
	writeMD(t, sc, "http-ingestion", "GW-001", "HTTP Ingestion", "", "GET `${INGESTION_URL}/health`")
	c := loadCfg(t, memstoreTwoSurfaces)

	rec := &recordingRunner{}
	rr, err := RunAll(c, sc, filepath.Join(dir, "results"), "memstore", "", "", "", "", rec)
	if err != nil {
		t.Fatal(err)
	}
	hostOf := func(id string) string {
		for _, p := range rec.props {
			if p["scenario.id"] == id {
				return p["http.host"] + ":" + p["http.port"]
			}
		}
		return "<not fired>"
	}
	if got := hostOf("NEO-010"); got != "memstore-graph:8096" {
		t.Errorf("NEO-010 with **Target**: graph must be fired at memstore-graph:8096, was fired at %s", got)
	}
	if got := hostOf("GW-001"); got != "memstore-gateway:8090" {
		t.Errorf("a scenario without the word keeps the plain slot, was fired at %s", got)
	}
	if got := hostOf("NEO-012"); got != "<not fired>" {
		t.Errorf("NEO-012 names a missing target and must NOT be fired anywhere (no fallback), was fired at %s", got)
	}
	res, _ := rr.Report.Find("NEO-012")
	if res == nil || res.Status != "failed" || res.Failure == nil {
		t.Fatalf("NEO-012 must be refused at preflight as failed: %+v", res)
	}
	for _, want := range []string{`unknown http target "grap"`, "declared: graph", "did you mean graph", "preflight"} {
		if !strings.Contains(res.Failure.Observed, want) {
			t.Errorf("refusal must carry %q, got: %q", want, res.Failure.Observed)
		}
	}
}

// Acceptance 2 / 4: a Database State scenario with `**Target**: neo4j` gets the named entry's JDBC
// props; a Message Flow scenario with `**Target**: audit` gets the named broker + ITS queue. The
// default scenarios keep Postgres / the main broker.
func TestRunAll_DBAndMQTargets_SelectNamedEntries(t *testing.T) {
	dir := t.TempDir()
	sc := filepath.Join(dir, "scenarios")
	writeMD(t, sc, "database-state", "DB-NEO", "Database State", "- **Target**: neo4j\n", "GET `${INGESTION_URL}/x`")
	writeMD(t, sc, "database-state", "DB-PG", "Database State", "", "GET `${INGESTION_URL}/x`")
	writeMD(t, sc, "message-flow", "MQ-AUD", "Message Flow", "- **Target**: audit\n", "GET `${INGESTION_URL}/x`")
	writeMD(t, sc, "message-flow", "MQ-MAIN", "Message Flow", "", "GET `${INGESTION_URL}/x`")
	writeMD(t, sc, "database-state", "DB-BAD", "Database State", "- **Target**: audit\n", "GET `${INGESTION_URL}/x`") // a broker name on a DB scenario
	c := loadCfg(t, memstoreTwoSurfaces)

	rec := &recordingRunner{}
	rr, err := RunAll(c, sc, filepath.Join(dir, "results"), "memstore", "", "", "", "", rec)
	if err != nil {
		t.Fatal(err)
	}
	props := func(id string) map[string]string {
		for _, p := range rec.props {
			if p["scenario.id"] == id {
				return p
			}
		}
		return nil
	}
	if p := props("DB-NEO"); p == nil || p["db.url"] != "jdbc:neo4j://memstore-neo4j:7687" || p["db.user"] != "neo4j" || p["db.password"] != "neo" {
		t.Errorf("DB-NEO must carry the named database entry's props, got %v", p)
	}
	if p := props("DB-PG"); p == nil || p["db.url"] != "jdbc:postgresql://memstore-pg:5432/memstore" {
		t.Errorf("DB-PG (no word) must keep the plain database slot, got %v", p)
	}
	if p := props("MQ-AUD"); p == nil || p["mgmt.host"] != "audit-rabbit" || p["mq.queue"] != "audit.q" {
		t.Errorf("MQ-AUD must carry the named broker's management host and ITS queue, got %v", p)
	}
	if p := props("MQ-MAIN"); p == nil || p["mgmt.host"] != "main-rabbit" || p["mq.queue"] != "main.q" {
		t.Errorf("MQ-MAIN (no word) must keep the plain broker, got %v", p)
	}
	if p := props("DB-BAD"); p != nil {
		t.Errorf("DB-BAD names a broker on a Database State scenario (wrong kind) and must not be fired, got %v", p)
	}
	res, _ := rr.Report.Find("DB-BAD")
	if res == nil || res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, `unknown database target "audit"`) {
		t.Fatalf("DB-BAD must be refused as an unknown DATABASE target (the word means one thing per layer): %+v", res)
	}
}

// VR10-S3-5 at run time: `**Target**` on a Web UI scenario is refused before anything runs.
func TestRunAll_TargetOnUIScenarioRefused(t *testing.T) {
	dir := t.TempDir()
	sc := filepath.Join(dir, "scenarios")
	writeMD(t, sc, "web-ui", "UI-001", "Web UI", "- **Tags**: http, ui\n- **Target**: graph\n", "POST `${APP_URL}`\n\n```json\n{\"spec\":\"tests/x.spec.ts\"}\n```")
	c := loadCfg(t, memstoreTwoSurfaces)
	rr, err := RunAll(c, sc, filepath.Join(dir, "results"), "memstore", "", "", "", "", &recordingRunner{})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("UI-001")
	if res == nil || res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "Web UI") || !strings.Contains(res.Failure.Observed, "preflight") {
		t.Fatalf("**Target** on a Web UI scenario must be refused at preflight: %+v", res)
	}
}

// countingMCP wraps the fake MCP server, counting tools/call hits and recording the bearer it saw.
func countingMCP(hits *int32, auth *string) http.HandlerFunc {
	inner := fakeMCPServer()
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1) // every request counts — a refused scenario must produce ZERO on the plain slot
		if a := r.Header.Get("Authorization"); a != "" && auth != nil {
			*auth = a
		}
		inner(w, r)
	}
}

func mcpMDWithTarget(name string) string {
	return strings.Replace(mcpScenarioMD(), "- **Tags**: mcp, critical", "- **Tags**: mcp, critical\n- **Target**: "+name, 1)
}

// Acceptance 3 (the single-mcp half): `**Target**: admin` on an mcp scenario reaches the SECOND MCP
// endpoint with ITS token; the plain slot is never called. A typo is refused by name; a scenario
// that also sets server_url in its payload is refused (two answers to one question).
func TestRunMCPScenario_NamedTarget(t *testing.T) {
	os.Unsetenv("MCP_URL")
	var hitsA, hitsB int32
	var authB string
	A := httptest.NewServer(countingMCP(&hitsA, nil))
	defer A.Close()
	B := httptest.NewServer(countingMCP(&hitsB, &authB))
	defer B.Close()
	c := loadCfg(t, "project:\n  name: memstore\ntargets:\n  mcp:\n    base_url: "+A.URL+"\n  mcp_targets:\n    admin:\n      base_url: "+B.URL+"\n      auth:\n        type: bearer\n        bearer_token: smcp_admin\n")

	res := runMCPScenario(c, scenario.Parse(mcpMDWithTarget("admin")), "tr-named")
	if res.Status != "passed" {
		t.Fatalf("the named mcp target must resolve and pass: %+v", res)
	}
	if atomic.LoadInt32(&hitsB) == 0 || atomic.LoadInt32(&hitsA) != 0 {
		t.Errorf("the call must go to the NAMED endpoint only: A=%d B=%d", hitsA, hitsB)
	}
	if authB != "Bearer smcp_admin" {
		t.Errorf("the named entry's own token must be sent, got %q", authB)
	}

	res = runMCPScenario(c, scenario.Parse(mcpMDWithTarget("admn")), "tr-typo")
	if res.Status != "failed" || res.Failure == nil {
		t.Fatalf("a typo must be refused at preflight: %+v", res)
	}
	for _, want := range []string{`unknown mcp target "admn"`, "declared: admin", "did you mean admin", "preflight"} {
		if !strings.Contains(res.Failure.Observed, want) {
			t.Errorf("refusal must carry %q, got: %q", want, res.Failure.Observed)
		}
	}
	if atomic.LoadInt32(&hitsA) != 0 {
		t.Errorf("a refused scenario must never fall back to the plain slot (A=%d)", hitsA)
	}

	both := strings.Replace(mcpMDWithTarget("admin"), `"tool":"ok_tool"`, `"server_url":"`+A.URL+`","tool":"ok_tool"`, 1)
	res = runMCPScenario(c, scenario.Parse(both), "tr-both")
	if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "server_url") || !strings.Contains(res.Failure.Observed, "Target") {
		t.Fatalf("**Target** plus a payload server_url is two answers to one question and must be refused: %+v", res)
	}
}

// Acceptance 3 (the chain half): a chain step with "target": "admin" reaches the second MCP endpoint;
// a step setting both target and server_url is refused; an unknown step target is refused by name.
func TestRunChainScenario_StepTarget(t *testing.T) {
	os.Unsetenv("MCP_URL")
	var hitsA, hitsB int32
	A := httptest.NewServer(countingMCP(&hitsA, nil))
	defer A.Close()
	B := httptest.NewServer(countingMCP(&hitsB, nil))
	defer B.Close()
	c := loadCfg(t, "project:\n  name: memstore\ntargets:\n  mcp:\n    base_url: "+A.URL+"\n  mcp_targets:\n    admin:\n      base_url: "+B.URL+"\n")

	s := &scenario.Scenario{ID: "CHAIN-T", Tags: []string{"chain"}}
	s.Trigger.Payload = `{"steps":[{"type":"mcp","name":"admin-step","target":"admin","tool":"ok_tool","args":{},"expect":"result.isError == false"}]}`
	res := runChainScenario(c, s, "tr-chain-t", "testkit/ui")
	if res.Status != "passed" || atomic.LoadInt32(&hitsB) == 0 || atomic.LoadInt32(&hitsA) != 0 {
		t.Fatalf("a step with target admin must reach the named endpoint only (A=%d B=%d): %+v", hitsA, hitsB, res)
	}

	s.Trigger.Payload = `{"steps":[{"type":"mcp","name":"both","target":"admin","server_url":"` + A.URL + `","tool":"ok_tool","args":{}}]}`
	res = runChainScenario(c, s, "tr-chain-both", "testkit/ui")
	if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "both") || !strings.Contains(res.Failure.Observed, "server_url") || !strings.Contains(res.Failure.Observed, "target") {
		t.Fatalf("a step with both target and server_url must be refused naming the step and both fields: %+v", res)
	}

	s.Trigger.Payload = `{"steps":[{"type":"mcp","name":"typo","target":"admn","tool":"ok_tool","args":{}}]}`
	res = runChainScenario(c, s, "tr-chain-typo", "testkit/ui")
	if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, `unknown mcp target "admn"`) || !strings.Contains(res.Failure.Observed, "declared: admin") {
		t.Fatalf("an unknown step target must be refused by name with the declared names: %+v", res)
	}
	if atomic.LoadInt32(&hitsA) != 0 {
		t.Errorf("a refused step must never fall back to the plain slot (A=%d)", hitsA)
	}
}

// `**Target**` in the Metadata of a chain scenario is refused (a chain selects per step).
func TestRunChainScenario_MetadataTargetRefused(t *testing.T) {
	os.Unsetenv("MCP_URL")
	c := loadCfg(t, "project:\n  name: memstore\ntargets:\n  mcp:\n    base_url: http://gw:8090/mcp\n  mcp_targets:\n    admin:\n      base_url: http://adm:8091/mcp\n")
	s := scenario.Parse("# Scenario: c\n\n## Metadata\n- **ID**: CHAIN-M\n- **Layer**: HTTP Ingestion\n- **Tags**: chain\n- **Target**: admin\n\n## TRIGGER\nPOST `${MCP_URL}`\n\n```json\n{\"steps\":[{\"type\":\"mcp\",\"name\":\"s\",\"tool\":\"ok_tool\",\"args\":{}}]}\n```\n\n## EXPECT\n### Runnable\n- result.isError == false\n")
	res := runChainScenario(c, s, "tr-chain-meta", "testkit/ui")
	if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, `"target"`) || !strings.Contains(res.Failure.Observed, "preflight") {
		t.Fatalf("a Metadata **Target** on a chain must be refused and point at the step field: %+v", res)
	}
}
