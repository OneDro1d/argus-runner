package updatecmd

import (
	"strings"
	"testing"
)

// Orchestrator holdout for AC-D49 (#296): the pairs the issue and its neighbours name.
func TestAC_D49_Holdout(t *testing.T) {
	img := "ghcr.io/x/y@sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name, prev, cur string
		offered         bool
		reasonHas       []string
	}{
		{"dev build to release", "0.3.37-dev+9816842", "0.3.40", true, nil},
		{"dev build to its own release", "0.3.40-dev+b7a3d98", "0.3.40", true, nil},
		{"release to release", "0.3.39", "0.3.40", true, nil},
		{"unparseable previous", "banana", "0.3.40", false, []string{"banana", "0.3.40"}},
		{"release back to its dev build", "0.3.40", "0.3.40-dev+b7a3d98", false, []string{"0.3.40", "0.3.40-dev+b7a3d98"}},
		{"sentinel previous", "0.0.0-dev", "0.3.40", false, []string{"0.0.0-dev", "0.3.40"}},
	}
	for _, c := range cases {
		m := Manifest{Version: c.cur, Previous: &Previous{Version: c.prev, Image: img}}
		if got := RollbackOffered(m); got != c.offered {
			t.Errorf("%s: RollbackOffered = %v, want %v", c.name, got, c.offered)
		}
		reason := RollbackReason(m)
		if c.offered && reason != "" {
			t.Errorf("%s: offered, but a withheld-reason was given: %q", c.name, reason)
		}
		if !c.offered && reason == "" {
			t.Errorf("%s: withheld with NO reason", c.name)
		}
		for _, w := range c.reasonHas {
			if !strings.Contains(reason, w) {
				t.Errorf("%s: reason %q does not name %q", c.name, reason, w)
			}
		}
	}
}
