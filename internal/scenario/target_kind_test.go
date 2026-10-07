package scenario

import (
	"strings"
	"testing"
)

// TargetKind is the pure rule "which kind does the **Target** word mean on THIS scenario" — the
// config-free half of VR10-S3-5, shared by the markdown validator and the config-side selector.
func TestTargetKind_OneMeaningPerLayer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		s       *Scenario
		kind    string
		wantErr string
	}{
		{"no word", &Scenario{Layers: []string{"HTTP Ingestion"}}, "", ""},
		{"http", &Scenario{Target: "g", Layers: []string{"HTTP Ingestion"}}, "http", ""},
		{"error path", &Scenario{Target: "g", Layers: []string{"Error Path"}}, "http", ""},
		{"rate limiting", &Scenario{Target: "g", Layers: []string{"Rate Limiting"}}, "http", ""},
		{"permissions", &Scenario{Target: "g", Layers: []string{"Permissions"}}, "http", ""},
		{"database", &Scenario{Target: "g", Layers: []string{"Database State"}}, "database", ""},
		{"terminal layer wins", &Scenario{Target: "g", Layers: []string{"HTTP Ingestion", "Database State"}}, "database", ""},
		{"message flow", &Scenario{Target: "g", Layers: []string{"Message Flow"}}, "message_broker", ""},
		{"mcp tag", &Scenario{Target: "g", Tags: []string{"mcp"}, Layers: []string{"HTTP Ingestion"}}, "mcp", ""},
		{"ui tag", &Scenario{Target: "g", Tags: []string{"ui"}, Layers: []string{"Web UI"}}, "", "Web UI"},
		{"web ui layer", &Scenario{Target: "g", Layers: []string{"Web UI"}}, "", "Web UI"},
		{"external delivery", &Scenario{Target: "g", Layers: []string{"External Delivery"}}, "", "External Delivery"},
		{"chain", &Scenario{Target: "g", Tags: []string{"chain"}}, "", `"target"`},
		{"no layer", &Scenario{Target: "g"}, "", "layer"},
	} {
		kind, err := TargetKind(tc.s)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: want error containing %q, got kind=%q err=%v", tc.name, tc.wantErr, kind, err)
			}
			continue
		}
		if err != nil || kind != tc.kind {
			t.Errorf("%s: want kind %q, got %q (err %v)", tc.name, tc.kind, kind, err)
		}
	}
}
