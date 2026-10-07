package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// VR10-S3 (V28-012): named extra targets for all four kinds, strict keys under `targets`,
// the name rules, the walker over every named entry, and the validate-time refusals.
//
// These tests use only the loader's public surface (Load / ParseUnresolved / EnvRefs /
// Validate) so they compile on the tree BEFORE the fix and fail there for the real reason:
// today an unknown key under `targets` is accepted in silence (NEO-010's `graph_health:`).

const namedTargetsYAML = `project:
  name: memstore
targets:
  http:
    base_url: http://memstore-gateway:8090
  http_targets:
    graph:
      base_url: http://memstore-graph:8096
  mcp:
    base_url: http://memstore-gateway:8090/mcp
  mcp_targets:
    admin:
      base_url: http://memstore-admin:8091/mcp
      transport: streamable-http
      auth:
        type: bearer
        bearer_token: ${NT_ADMIN_TOKEN}
  database:
    type: postgres
    jdbc_url: jdbc:postgresql://memstore-pg:5432/memstore
    username: ro
    password: pg
  database_targets:
    neo4j:
      jdbc_url: jdbc:neo4j://memstore-neo4j:7687
      username: neo4j
      password: ${NT_NEO4J_PASSWORD}
  message_broker_targets:
    audit:
      url: amqp://${NT_AUDIT_USER}:${NT_AUDIT_PASSWORD}@audit-rabbit:5672/
      management_url: http://audit-rabbit:15672
      queues:
        incoming: audit.q
`

// VR10-S3-1 / acceptance 5: `targets: { graph_health: … }` — NEO-010's original mistake — is
// refused at load, by name, with the accepted key list. Today it parses fine and reaches nothing.
func TestNamedTargets_UnknownKeyUnderTargetsRefusedByName(t *testing.T) {
	body := `project:
  name: memstore
targets:
  http:
    base_url: http://memstore-gateway:8090
  graph_health:
    base_url: http://memstore-graph:8096
`
	_, err := Load(writeCfg(t, body))
	if err == nil {
		t.Fatal("an unknown key under targets (graph_health) was accepted in silence — that is the V28-012 failure shape")
	}
	msg := err.Error()
	for _, want := range []string{`"graph_health"`, "accepted", "http_targets", "database_targets", "mcp_targets", "message_broker_targets"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must name the key and list the accepted keys; missing %q in: %s", want, msg)
		}
	}
	if _, perr := ParseUnresolved(writeCfg(t, body)); perr == nil {
		t.Error("ParseUnresolved (the secrets-preflight loader) must refuse the same unknown key")
	}
}

// An unknown key INSIDE a named entry is refused too — strictness covers the whole targets subtree.
func TestNamedTargets_UnknownKeyInsideEntryRefused(t *testing.T) {
	body := `project:
  name: memstore
targets:
  http_targets:
    graph:
      base_url: http://memstore-graph:8096
      timeout: 5
`
	_, err := Load(writeCfg(t, body))
	if err == nil || !strings.Contains(err.Error(), `"timeout"`) || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("unknown key inside http_targets.graph must be refused naming the key and the accepted fields, got: %v", err)
	}
}

// VR10-S3-13: `rate_limiting` is REMOVED from the struct; under strict keys it is now an unknown
// key and refused — a dead-but-accepted key is the worst kind (it looks supported).
func TestNamedTargets_RateLimitingKeyIsRefused(t *testing.T) {
	body := `project:
  name: order-service
targets:
  http:
    base_url: http://order-api:8080
  rate_limiting:
    requests_per_second_threshold: 100
`
	_, err := Load(writeCfg(t, body))
	if err == nil || !strings.Contains(err.Error(), `"rate_limiting"`) {
		t.Fatalf("rate_limiting must be refused as an unknown key under targets, got: %v", err)
	}
}

// Strictness is on the `targets` node ONLY (SA §0.14 S3-a): other blocks stay lenient in this build.
func TestNamedTargets_OtherBlocksStayLenient(t *testing.T) {
	body := `project:
  name: t
targets:
  http:
    base_url: http://api:8080
observability:
  loki:
    url: http://loki:3100
    some_future_key: 1
deploy:
  unknown_here: yes
`
	if _, err := Load(writeCfg(t, body)); err != nil {
		t.Fatalf("unknown keys OUTSIDE targets must still parse in this build: %v", err)
	}
}

// VR10-S3-3: a name may not be `default` and may not collide with a plain-slot key.
func TestNamedTargets_NameRulesRefusedByName(t *testing.T) {
	for _, tc := range []struct{ name, kind string }{
		{"default", "http_targets"},
		{"http", "http_targets"},
		{"database", "http_targets"}, // a plain-slot key of ANOTHER kind is still a plain-slot key
		{"mcp", "mcp_targets"},
		{"message_broker", "message_broker_targets"},
		{"external", "database_targets"},
		{"auth", "database_targets"},
	} {
		body := "project:\n  name: t\ntargets:\n  " + tc.kind + ":\n    " + tc.name + ":\n      base_url: http://x:1\n      jdbc_url: jdbc:postgresql://x:5432/d\n      url: amqp://x:5672/\n"
		// only the field the kind knows is kept; strip the others so the name rule is the one thing under test
		switch tc.kind {
		case "http_targets", "mcp_targets":
			body = strings.ReplaceAll(body, "      jdbc_url: jdbc:postgresql://x:5432/d\n", "")
			body = strings.ReplaceAll(body, "      url: amqp://x:5672/\n", "")
		case "database_targets":
			body = strings.ReplaceAll(body, "      base_url: http://x:1\n", "")
			body = strings.ReplaceAll(body, "      url: amqp://x:5672/\n", "")
		case "message_broker_targets":
			body = strings.ReplaceAll(body, "      base_url: http://x:1\n", "")
			body = strings.ReplaceAll(body, "      jdbc_url: jdbc:postgresql://x:5432/d\n", "")
		}
		_, err := Load(writeCfg(t, body))
		if err == nil {
			t.Errorf("%s.%s: must be refused (a name may not be `default` or a plain-slot key)", tc.kind, tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.kind+"."+tc.name) {
			t.Errorf("%s.%s: refusal must name the entry, got: %v", tc.kind, tc.name, err)
		}
	}
}

// An entry that declares nothing (`graph:` with no fields) is refused rather than dereferenced later.
func TestNamedTargets_EmptyEntryRefused(t *testing.T) {
	body := "project:\n  name: t\ntargets:\n  http_targets:\n    graph:\n"
	_, err := Load(writeCfg(t, body))
	if err == nil || !strings.Contains(err.Error(), "http_targets.graph") {
		t.Fatalf("an empty named entry must be refused by name, got: %v", err)
	}
}

// VR10-S3-9 / acceptance 10: EnvRefs (the 2b masked preview's source) lists every named entry's
// variables with labels naming the ENTRY — `targets.database_targets.neo4j.password`.
func TestEnvRefs_NamedEntriesLabelledByEntry(t *testing.T) {
	for _, v := range []string{"NT_ADMIN_TOKEN", "NT_NEO4J_PASSWORD", "NT_AUDIT_USER", "NT_AUDIT_PASSWORD"} {
		os.Unsetenv(v)
	}
	c, err := ParseUnresolved(writeCfg(t, namedTargetsYAML))
	if err != nil {
		t.Fatalf("ParseUnresolved: %v", err)
	}
	got := map[string]string{}
	for _, r := range c.EnvRefs() {
		got[r.Name] = r.Field
	}
	want := map[string]string{
		"NT_ADMIN_TOKEN":    "targets.mcp_targets.admin.auth.bearer_token",
		"NT_NEO4J_PASSWORD": "targets.database_targets.neo4j.password",
		"NT_AUDIT_USER":     "targets.message_broker_targets.audit.url",
		"NT_AUDIT_PASSWORD": "targets.message_broker_targets.audit.url",
	}
	for name, field := range want {
		if got[name] != field {
			t.Errorf("EnvRefs[%s] = %q, want %q (the walker must iterate every named entry with a label naming it)", name, got[name], field)
		}
	}
}

// VR10-S3-9 / acceptance 9 (RED first): `database_targets.neo4j.password: ${NT_NEO4J_PASSWORD}` with
// the variable ABSENT — Load must refuse and NAME the variable and the entry, exactly as it does for
// `targets.database.password` today. On the unextended walker it passes and fails later at the SUT.
func TestExpandEnv_NamedEntryMissingVarIsNamed(t *testing.T) {
	os.Unsetenv("NT_NEO4J_PASSWORD")
	t.Setenv("NT_ADMIN_TOKEN", "tok")
	t.Setenv("NT_AUDIT_USER", "u")
	t.Setenv("NT_AUDIT_PASSWORD", "p")
	_, err := Load(writeCfg(t, namedTargetsYAML))
	if err == nil {
		t.Fatal("a missing ${VAR} inside a named entry was NOT refused — the walker does not cover the named maps")
	}
	if !strings.Contains(err.Error(), "NT_NEO4J_PASSWORD (referenced by targets.database_targets.neo4j.password)") {
		t.Errorf("the refusal must name the variable AND the entry: %v", err)
	}
}

// And with every variable set, the values resolve INSIDE the maps (via the same walker).
func TestExpandEnv_NamedEntriesResolve(t *testing.T) {
	t.Setenv("NT_ADMIN_TOKEN", "smcp_admin")
	t.Setenv("NT_NEO4J_PASSWORD", "neo-secret")
	t.Setenv("NT_AUDIT_USER", "audit")
	t.Setenv("NT_AUDIT_PASSWORD", "auditpw")
	c, err := Load(writeCfg(t, namedTargetsYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	refs := c.EnvRefs()
	if len(refs) != 0 {
		t.Errorf("after Load every placeholder must be substituted away, still referenced: %+v", refs)
	}
}

// VR10-S3-7 / VR10-S3-4 / VR10-S3-5: `validate-config --scenarios` reports a **Target** that names
// a missing entry (with the declared names + a nearest match), one of the wrong kind, and one on a
// layer that has no kind — by name, and never by falling back to the plain slot.
func TestValidate_ScenarioTargetRefusedByName(t *testing.T) {
	dir := t.TempDir()
	write := func(sub, id, layer, tags, target string) {
		d := filepath.Join(dir, sub)
		_ = os.MkdirAll(d, 0o755)
		md := "# Scenario: " + id + "\n\n## Metadata\n- **ID**: " + id + "\n- **Layer**: " + layer + "\n"
		if tags != "" {
			md += "- **Tags**: http, " + tags + "\n"
		}
		if target != "" {
			md += "- **Target**: " + target + "\n"
		}
		md += "\n## TRIGGER\nGET `http://x/health`\n\n## EXPECT\n- status=200\n"
		if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("http-ingestion", "NEO-010", "HTTP Ingestion", "", "graph") // declared → fine
	write("http-ingestion", "NEO-012", "HTTP Ingestion", "", "grap")  // typo → refused + nearest
	write("http-ingestion", "NEO-013", "HTTP Ingestion", "", "neo4j") // wrong kind (a database name on an HTTP scenario)
	write("database-state", "NEO-014", "Database State", "", "neo4j") // declared → fine, and no plain `database` slot needed
	write("web-ui", "UI-001", "Web UI", "ui", "graph")                // no kind on a Web UI scenario

	c, err := Load(writeCfg(t, `project:
  name: memstore
targets:
  http:
    base_url: http://memstore-gateway:8090
  http_targets:
    graph:
      base_url: http://memstore-graph:8096
  database_targets:
    neo4j:
      jdbc_url: jdbc:neo4j://memstore-neo4j:7687
      username: neo4j
      password: pw
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	errs, count, verr := c.Validate(dir)
	if verr != nil {
		t.Fatal(verr)
	}
	if count != 5 {
		t.Fatalf("scenarios_found = %d, want 5", count)
	}
	byID := map[string][]string{}
	for _, e := range errs {
		byID[e.Scenario] = append(byID[e.Scenario], e.Message)
	}
	if len(byID["NEO-010"]) != 0 {
		t.Errorf("NEO-010 selects a declared http target and must validate clean: %v", byID["NEO-010"])
	}
	if len(byID["NEO-014"]) != 0 {
		t.Errorf("NEO-014 selects a declared database target; the named entry covers the layer: %v", byID["NEO-014"])
	}
	joined := func(id string) string { return strings.Join(byID[id], " | ") }
	if m := joined("NEO-012"); !strings.Contains(m, `unknown http target "grap"`) || !strings.Contains(m, "declared: graph") || !strings.Contains(m, "did you mean graph") {
		t.Errorf("NEO-012: a typo must be refused with the declared names and a nearest match, got: %q", m)
	}
	if m := joined("NEO-013"); !strings.Contains(m, `unknown http target "neo4j"`) {
		t.Errorf("NEO-013: a database name on an HTTP scenario is the wrong kind and must be refused, got: %q", m)
	}
	if m := joined("UI-001"); !strings.Contains(m, "Target") || !strings.Contains(m, "Web UI") {
		t.Errorf("UI-001: **Target** on a Web UI scenario has no meaning and must be refused, got: %q", m)
	}
}

// VR10-S3-13 grep-test (SA §1.4.S3): no `rate_limiting:` stanza remains in any shipped config,
// template, or the published schema.
func TestNoRateLimitingStanzaRemains(t *testing.T) {
	root := repoRoot(t)
	var hits []string
	for _, sub := range []string{"examples", "onboarding", "schemas", "deploy"} {
		_ = filepath.WalkDir(filepath.Join(root, sub), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if ext := filepath.Ext(p); ext != ".yaml" && ext != ".yml" {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			for i, line := range strings.Split(string(b), "\n") {
				if strings.Contains(line, "rate_limiting") {
					rel, _ := filepath.Rel(root, p)
					hits = append(hits, rel+":"+itoa(i+1)+": "+strings.TrimSpace(line))
				}
			}
			return nil
		})
	}
	if len(hits) != 0 {
		t.Errorf("rate_limiting is removed from the struct (VR10-S3-13); these files still carry it:\n  %s", strings.Join(hits, "\n  "))
	}
}

// The published schema (the contract people read; nothing in the product loads it) stays accurate:
// it parses, documents the four maps beside the plain slots, and no longer lists rate_limiting.
func TestSchema_DocumentsNamedTargets(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "schemas", "argus-config.schema.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("schema is not valid YAML: %v", err)
	}
	props, _ := doc["properties"].(map[string]any)
	targets, _ := props["targets"].(map[string]any)
	tprops, _ := targets["properties"].(map[string]any)
	for _, want := range []string{"http", "http_targets", "mcp", "mcp_targets", "database", "database_targets", "message_broker", "message_broker_targets", "external", "auth"} {
		if _, ok := tprops[want]; !ok {
			t.Errorf("schema targets.properties must document %q", want)
		}
	}
	if _, ok := tprops["rate_limiting"]; ok {
		t.Error("schema still documents rate_limiting under targets")
	}
	if targets["additionalProperties"] != false {
		t.Errorf("schema must say keys under targets are strict (additionalProperties: false), got %v", targets["additionalProperties"])
	}
	defs, _ := doc["$defs"].(map[string]any)
	for _, want := range []string{"http_target", "mcp_target", "database_target", "message_broker_target"} {
		if _, ok := defs[want]; !ok {
			t.Errorf("schema $defs must carry the shared shape %q", want)
		}
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
