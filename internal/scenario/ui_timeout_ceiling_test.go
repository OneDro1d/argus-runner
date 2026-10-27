package scenario

import (
	"strings"
	"testing"
)

// a `ui` scenario may declare `## TIMEOUT` up to 300s (a browser flow is many round
// trips); every other shape keeps the ceiling it had.
func TestUITimeoutCeiling_300sAccepted_301sRefusedByName(t *testing.T) {
	cases := []struct {
		name, tags, layer, timeout string
		refused                    bool
	}{
		{"ui at 30s (the shipped value)", "ui", "HTTP Ingestion", "30s", false},
		{"ui at 10s", "ui", "HTTP Ingestion", "10s", false},
		{"ui at 300s", "ui", "HTTP Ingestion", "300s", false},
		{"ui at 300000ms", "ui", "HTTP Ingestion", "300000ms", false},
		{"ui at 301s", "ui", "HTTP Ingestion", "301s", true},
		{"ui at 600s", "ui", "HTTP Ingestion", "600s", true},
		// unchanged shapes
		{"non-ui at 31s", "http", "HTTP Ingestion", "31s", true},
		{"non-ui at 300s", "http", "HTTP Ingestion", "300s", true},
		{"chained layer at 61s", "http", "HTTP Ingestion -> Database State", "61s", true},
		{"rate limiting at 61s", "http", "Rate Limiting", "61s", true},
		{"chain at 121s", "chain", "Permissions", "121s", true},
		{"chain at 300s", "chain", "Permissions", "300s", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := fullMD(func(m string) string {
				m = strings.Replace(m, "- **Tags**: http, order", "- **Tags**: "+c.tags+", order", 1)
				m = strings.Replace(m, "- **Layer**: HTTP Ingestion", "- **Layer**: "+c.layer, 1)
				return strings.Replace(m, "## TIMEOUT\n15s", "## TIMEOUT\n"+c.timeout, 1)
			})
			_, errs := Validate(md)
			if got := find(errs, "exceeds the ceiling"); got != c.refused {
				t.Fatalf("timeout %s on %q: refused=%v, want %v (%v)", c.timeout, c.tags, got, c.refused, errs)
			}
			if c.refused && c.tags == "ui" {
				// refused BY NAME: the sentence carries the value, the ceiling and the reason.
				var msg string
				for _, e := range errs {
					if strings.Contains(e.Message, "exceeds the ceiling") {
						msg = e.Message
					}
				}
				for _, want := range []string{"## TIMEOUT " + c.timeout, "300s", "a browser flow is many round trips"} {
					if !strings.Contains(msg, want) {
						t.Errorf("refusal %q must contain %q", msg, want)
					}
				}
			}
		})
	}
}

// timeoutCeiling itself, for each shape (the table the validator path above goes through).
func TestTimeoutCeiling_UIIs300s_OthersUnchanged(t *testing.T) {
	cases := []struct {
		s    Scenario
		want int
	}{
		{Scenario{Tags: []string{"ui"}}, 300000},
		{Scenario{Tags: []string{"ui", "web-ui"}, Layers: []string{"Rate Limiting"}}, 300000},
		{Scenario{Tags: []string{"chain", "ui"}}, 120000}, // a chain stays a chain
		{Scenario{Tags: []string{"http"}}, 30000},
		{Scenario{Tags: []string{"http"}, Layers: []string{"Rate Limiting"}}, 60000},
		{Scenario{Tags: []string{"http"}, Layers: []string{"A", "B"}}, 60000},
		{Scenario{Tags: []string{"chain"}}, 120000},
	}
	for _, c := range cases {
		if got, _ := timeoutCeiling(&c.s); got != c.want {
			t.Errorf("tags %v layers %v: ceiling %d, want %d", c.s.Tags, c.s.Layers, got, c.want)
		}
	}
}
