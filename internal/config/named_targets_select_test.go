package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// VR10-S3 (V28-012): the selection API — the named structs the plain slots and the maps share,
// the by-name lookups with their refusal shape, and SelectTarget, the ONE function that turns a
// scenario's word into an entry of the matching kind and never falls back to the plain slot.

// VR10-S3-2 / VR10-S3-13: one named struct per kind, reused by the plain slot AND its map; no
// RateLimiting field remains.
func TestTargets_NamedStructsReusedAndRateLimitingGone(t *testing.T) {
	var c Config
	var _ *HTTPTarget = c.Targets.HTTP
	var _ map[string]*HTTPTarget = c.Targets.HTTPTargets
	var _ *MCPTarget = c.Targets.MCP
	var _ map[string]*MCPTarget = c.Targets.MCPTargets
	var _ *DBTarget = c.Targets.Database
	var _ map[string]*DBTarget = c.Targets.DatabaseTargets
	var _ *MQTarget = c.Targets.MessageBroker
	var _ map[string]*MQTarget = c.Targets.MessageBrokerTargets
	if _, has := reflect.TypeOf(c.Targets).FieldByName("RateLimiting"); has {
		t.Fatal("Targets still carries RateLimiting — VR10-S3-13 removes it (V28-019 owns the concept)")
	}
}

func TestHTTPTargetNamed_UnknownRefusedWithDeclaredAndNearest(t *testing.T) {
	c := &Config{}
	c.Targets.HTTPTargets = map[string]*HTTPTarget{"graph": {BaseURL: "http://memstore-graph:8096"}, "admin": {BaseURL: "http://adm:1"}}
	if got, err := c.HTTPTargetNamed("graph"); err != nil || got == nil || got.BaseURL != "http://memstore-graph:8096" {
		t.Fatalf("declared name must resolve: %v %v", got, err)
	}
	_, err := c.HTTPTargetNamed("grap")
	if err == nil {
		t.Fatal("an unknown name must be refused")
	}
	for _, want := range []string{`unknown http target "grap"`, "declared: admin, graph", "did you mean graph"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must carry %q, got: %v", want, err)
		}
	}
	// Levenshtein > 2 → no suggestion (a wrong hint is worse than none — S1-a).
	_, err = c.HTTPTargetNamed("gateway")
	if err == nil || strings.Contains(err.Error(), "did you mean") {
		t.Errorf("no nearest match beyond distance 2, got: %v", err)
	}
	// No map at all: the refusal says so and points at the key to add.
	_, err = (&Config{}).DBTargetNamed("neo4j")
	if err == nil || !strings.Contains(err.Error(), `unknown database target "neo4j"`) || !strings.Contains(err.Error(), "database_targets") {
		t.Errorf("with no database_targets declared the refusal must say so, got: %v", err)
	}
}

// VR10-S3-4 / VR10-S3-5: the word selects the kind of the scenario's terminal layer (mcp/chain/ui by
// tag); a missing entry, a wrong-kind name, and a layer with no kind are refused by name; a scenario
// without the word selects nothing (the plain slots stay the default).
func TestSelectTarget_KindPerLayer_NeverFallsBack(t *testing.T) {
	c := &Config{}
	c.Targets.HTTP = &HTTPTarget{BaseURL: "http://gw:8090"}
	c.Targets.HTTPTargets = map[string]*HTTPTarget{"graph": {BaseURL: "http://graph:8096"}}
	c.Targets.MCPTargets = map[string]*MCPTarget{"admin": {BaseURL: "http://adm:1/mcp"}}
	c.Targets.DatabaseTargets = map[string]*DBTarget{"neo4j": {JDBCURL: "jdbc:neo4j://neo:7687"}}
	c.Targets.MessageBrokerTargets = map[string]*MQTarget{"audit": {URL: "amqp://a:5672/"}}

	sc := func(target string, tags []string, layers ...string) *scenario.Scenario {
		return &scenario.Scenario{ID: "X", Target: target, Tags: tags, Layers: layers}
	}
	for _, tc := range []struct {
		name    string
		s       *scenario.Scenario
		kind    string
		wantErr string
	}{
		{"no word → nil selection", sc("", nil, "HTTP Ingestion"), "", ""},
		{"http layer", sc("graph", nil, "HTTP Ingestion"), "http", ""},
		{"error path is an http layer", sc("graph", nil, "Error Path"), "http", ""},
		{"rate limiting is an http layer", sc("graph", nil, "Rate Limiting"), "http", ""},
		{"permissions is an http layer", sc("graph", nil, "Permissions"), "http", ""},
		{"terminal layer decides", sc("neo4j", nil, "HTTP Ingestion", "Database State"), "database", ""},
		{"message flow", sc("audit", nil, "Message Flow"), "message_broker", ""},
		{"mcp by tag", sc("admin", []string{"mcp"}, "HTTP Ingestion"), "mcp", ""},
		{"wrong kind: a db name on an http scenario", sc("neo4j", nil, "HTTP Ingestion"), "", `unknown http target "neo4j"`},
		{"missing entry", sc("grap", nil, "HTTP Ingestion"), "", `unknown http target "grap"`},
		{"web ui has no kind", sc("graph", []string{"ui"}, "Web UI"), "", "Web UI"},
		{"external delivery has no named form", sc("graph", nil, "External Delivery"), "", "External Delivery"},
		{"chain selects per step", sc("admin", []string{"chain"}, "HTTP Ingestion"), "", `"target"`},
	} {
		sel, err := c.SelectTarget(tc.s)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: want refusal containing %q, got sel=%+v err=%v", tc.name, tc.wantErr, sel, err)
			}
			if sel != nil {
				t.Errorf("%s: a refusal must select NOTHING (never the plain slot), got %+v", tc.name, sel)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected refusal: %v", tc.name, err)
			continue
		}
		if tc.kind == "" {
			if sel != nil {
				t.Errorf("%s: no word must select nothing, got %+v", tc.name, sel)
			}
			continue
		}
		if sel == nil || sel.Kind != tc.kind || sel.Name != tc.s.Target {
			t.Errorf("%s: want kind %s name %s, got %+v", tc.name, tc.kind, tc.s.Target, sel)
			continue
		}
		switch tc.kind {
		case "http":
			if sel.HTTP == nil || sel.HTTP.BaseURL != "http://graph:8096" {
				t.Errorf("%s: http entry not carried: %+v", tc.name, sel)
			}
		case "mcp":
			if sel.MCP == nil || sel.MCP.BaseURL != "http://adm:1/mcp" {
				t.Errorf("%s: mcp entry not carried: %+v", tc.name, sel)
			}
		case "database":
			if sel.DB == nil || sel.DB.JDBCURL != "jdbc:neo4j://neo:7687" {
				t.Errorf("%s: database entry not carried: %+v", tc.name, sel)
			}
		case "message_broker":
			if sel.MQ == nil || sel.MQ.URL != "amqp://a:5672/" {
				t.Errorf("%s: broker entry not carried: %+v", tc.name, sel)
			}
		}
	}
}

// The MCP accessors read from an entry, so a named entry answers exactly like the plain slot
// (transport/timeout defaults included).
func TestMCPTarget_AccessorsDefault(t *testing.T) {
	var nilT *MCPTarget
	if nilT.TransportOrDefault() != "streamable-http" || nilT.TimeoutOrDefault() != defaultMCPTimeout || nilT.Token() != "" {
		t.Error("a nil entry answers with the defaults")
	}
	full := &MCPTarget{BaseURL: " http://x/mcp ", Transport: "http-sse", TimeoutSeconds: 5, Auth: &MCPAuth{Type: "bearer", BearerToken: " tok "}}
	if full.TransportOrDefault() != "http-sse" || full.TimeoutOrDefault().Seconds() != 5 || full.Token() != "tok" || full.URL() != "http://x/mcp" {
		t.Errorf("entry accessors: %+v", full)
	}
}
