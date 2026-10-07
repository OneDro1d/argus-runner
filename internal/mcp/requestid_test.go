package mcp

import (
	"regexp"
	"testing"
)

// PerCallRequestID: the correlation id, a dot, 8 hex — distinct on every call.
func TestPerCallRequestID_UniqueAndPrefixed(t *testing.T) {
	const corr = "tr-run1-SOC-001-0badf00d"
	shape := regexp.MustCompile(`^` + regexp.QuoteMeta(corr) + `\.[0-9a-f]{8}$`)
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := PerCallRequestID(corr)
		if !shape.MatchString(id) {
			t.Fatalf("id %q, want %s.<8 hex>", id, corr)
		}
		if seen[id] {
			t.Fatalf("id %q repeated within 1000 calls", id)
		}
		seen[id] = true
	}
}
