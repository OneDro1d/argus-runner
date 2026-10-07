package toolcore

import "testing"

// D3: runScope classifies a run's selection for the cloud ledger, matching the cloud request_run scope
// vocabulary (single|tag|layer|full). Precedence: scenario id > tag > layer > full.
func TestRunScope(t *testing.T) {
	cases := []struct {
		layer, tag, scenarioID string
		want                   string
	}{
		{"", "", "", "full"},
		{"Database State", "", "", "layer"},
		{"", "smoke", "", "tag"},
		{"", "", "DB-004", "single"},
		{"Database State", "smoke", "DB-004", "single"}, // scenario id wins
		{"Database State", "smoke", "", "tag"},          // tag beats layer
	}
	for _, c := range cases {
		if got := runScope(c.layer, c.tag, c.scenarioID); got != c.want {
			t.Errorf("runScope(%q,%q,%q) = %q, want %q", c.layer, c.tag, c.scenarioID, got, c.want)
		}
	}
}
