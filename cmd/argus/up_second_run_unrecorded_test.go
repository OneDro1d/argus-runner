package main

import (
	"strings"
	"testing"
)

// AC-D58 (#392): `argus up`'s second run never reads an update that changed the machine and could not
// record itself as a success, whatever the block's own exit code was.
func TestSecondRunEvents_AnUnrecordedUpdateIsAFailWithAReason(t *testing.T) {
	for _, word := range []string{"unrecorded-updated", "unrecorded-rolled-back-to 0.3.31"} {
		out := "wrote /x/discover.sh\npreflight ok\n" + word + "\n"
		events, code := secondRunEvents(out, nil, t.TempDir(), "i1", "ghcr.io/x/y@sha256:aa")
		if code == exitOK {
			t.Errorf("%q: exit %d, want non-zero", word, code)
		}
		last := events[len(events)-1]
		if last.Step != "apply" || last.State != "fail" || !strings.Contains(last.Detail, word) || !strings.Contains(last.Detail, "not recorded") {
			t.Errorf("%q: last event = %+v, want apply/fail naming the word and that the update was not recorded", word, last)
		}
	}
}
