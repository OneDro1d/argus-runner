package main

import (
	"strings"
	"testing"
)

// `argus up` pointed people at "one click in the dashboard" for a runner id, and the
// dashboard had no such control. The control now exists (Settings → Environments → Mint runner id);
// both messages must name the real place, and `argus runner-id mint`, and the behaviour must not move:
// the hint is a skip, and a control plane with no runner id is still a refusal before onboard.sh runs.
func TestResolveRunnerID_NamesTheRealPlaceToMint(t *testing.T) {
	_, hint, fail := resolveRunnerID(upArgs{ControlPlane: "https://argus.example"})
	if hint == nil || hint.State != "skip" || fail == nil || fail.State != "fail" {
		t.Fatalf("behaviour changed: hint=%+v fail=%+v", hint, fail)
	}
	for name, detail := range map[string]string{"hint": hint.Detail, "fail": fail.Detail} {
		if strings.Contains(detail, "in the dashboard") {
			t.Errorf("%s still says \"in the dashboard\" (no such place): %q", name, detail)
		}
		if !strings.Contains(detail, "Settings → Environments → Mint runner id") || !strings.Contains(detail, "argus runner-id mint") {
			t.Errorf("%s does not name Settings → Environments → Mint runner id and `argus runner-id mint`: %q", name, detail)
		}
	}
	if id, h, f := resolveRunnerID(upArgs{RunnerID: "rid_x"}); id != "rid_x" || h != nil || f != nil {
		t.Fatalf("a given --runner-id must pass through untouched: %q %+v %+v", id, h, f)
	}
}
