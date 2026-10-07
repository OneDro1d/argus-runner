package report

import (
	"encoding/json"
	"strings"
	"testing"
)

// An execution/harness error (status=="error") must make the run NON-green (UC-62/VR-L3):
// distinct in Summary.Errored, but Failed() is true so the run exits non-zero — never
// silent-green. A SUT "failed" likewise; a clean pass is green.
func TestFailed_IncludesErrored(t *testing.T) {
	cases := []struct {
		name string
		s    Summary
		want bool
	}{
		{"all-passed", Summary{Total: 3, Passed: 3}, false},
		{"one-failed", Summary{Total: 3, Passed: 2, Failed: 1}, true},
		{"one-errored", Summary{Total: 3, Passed: 2, Errored: 1}, true},
		{"skipped-only-is-green", Summary{Total: 1, Skipped: 1}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Report{Summary: c.s}
			if got := r.Failed(); got != c.want {
				t.Fatalf("Failed() = %v; want %v", got, c.want)
			}
		})
	}
}

// Errored is omitempty: a report with zero errors serializes WITHOUT the key (existing
// reports stay byte-identical).
func TestSummary_ErroredOmitempty(t *testing.T) {
	b, _ := json.Marshal(Summary{Total: 1, Passed: 1})
	if strings.Contains(string(b), "errored") {
		t.Fatalf("errored must be omitted when zero: %s", b)
	}
	b2, _ := json.Marshal(Summary{Total: 1, Errored: 1})
	if !strings.Contains(string(b2), "errored") {
		t.Fatalf("errored must appear when non-zero: %s", b2)
	}
}
