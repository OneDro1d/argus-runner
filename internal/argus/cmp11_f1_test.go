package argus

// cmp11_f1_test.go -- ARGUS-CMP-11 fix F1: a comparison member's target that a check cannot take is a refusal before
// firing, never a silent default (a chain sent both requests to its own urls, and two members would have read identical).

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
)

func blueCfg() *config.Config {
	c := httpCfg()
	c.Targets.HTTPTargets = map[string]*config.HTTPTarget{"blue": {BaseURL: "http://blue.invalid:9000"}, "green": {BaseURL: "http://green.invalid:9100"}}
	return c
}

const twoStepChainExpect = "### Runnable\n- step one: status=200\n- step two: status=200\n"

func twoStepChain(a, b string) string {
	return `{"steps":[{"type":"http","name":"one","method":"POST","url":"` + a + `/things"},{"type":"http","name":"two","method":"POST","url":"` + b + `/things"}]}`
}

func TestCMP11_AChainCheckUnderAMemberTargetIsRefusedBeforeFiringAndRecordsNothing(t *testing.T) {
	var hitsA, hitsB int32
	a := chainServer(t, `{"id":"a","n":1}`, &hitsA)
	b := chainServer(t, `{"id":"b","n":1}`, &hitsB)
	md := cmpChainMD("CHN-F1", twoStepChain(a.URL, b.URL), twoStepChainExpect, cmpSection)
	run := runCmpTarget(t, blueCfg(), map[string]string{"CHN-F1": md}, "compare", "blue", nil)
	row := run.row(t, "CHN-F1")
	want := `refused before firing: this comparison member names the connection target "blue", and a chain check cannot be pointed at a named target; give the check its own target or compare this system as its own instance`
	if row.Status != report.StatusError || row.Failure == nil || row.Failure.Observed != want {
		t.Fatalf("status %q failure %+v, want error %q", row.Status, row.Failure, want)
	}
	if row.Outputs != nil {
		t.Errorf("a refused check records no output: %+v", row.Outputs)
	}
	if atomic.LoadInt32(&hitsA) != 0 || atomic.LoadInt32(&hitsB) != 0 {
		t.Errorf("requests reached the systems: %d and %d, want 0 and 0", hitsA, hitsB)
	}
}

func TestCMP11_AChainCheckIsUntouchedByATargetInEveryModeButCompareAndWithNoTarget(t *testing.T) {
	for _, tc := range []struct{ mode, target string }{{"build", "blue"}, {"final", "blue"}, {"scheduled", "blue"}, {"", "blue"}, {"compare", ""}} {
		var hitsA, hitsB int32
		a := chainServer(t, `{"id":"a","n":1}`, &hitsA)
		b := chainServer(t, `{"id":"b","n":1}`, &hitsB)
		md := cmpChainMD("CHN-F1", twoStepChain(a.URL, b.URL), twoStepChainExpect, cmpSection)
		row := runCmpTarget(t, blueCfg(), map[string]string{"CHN-F1": md}, tc.mode, tc.target, nil).row(t, "CHN-F1")
		if row.Status != "passed" || atomic.LoadInt32(&hitsA) != 1 || atomic.LoadInt32(&hitsB) != 1 {
			t.Errorf("mode %q target %q: status %q (%+v), hits %d and %d, want passed and 1 and 1", tc.mode, tc.target, row.Status, row.Failure, hitsA, hitsB)
		}
	}
}

func TestCMP11_ACheckOfALayerWithNoNamedFormIsRefusedUnderAMemberTargetAndACheckWithItsOwnTargetIsNot(t *testing.T) {
	webui := strings.Replace(cmpHTTPMD, "- **Layer**: HTTP Ingestion\n", "- **Layer**: Web UI\n", 1)
	ext := strings.Replace(cmpHTTPMD, "- **Layer**: HTTP Ingestion\n", "- **Layer**: External Delivery\n", 1)
	for name, md := range map[string]string{"Web UI": webui, "External Delivery": ext} {
		r := &capRunner{t: t}
		row := runCmpTarget(t, blueCfg(), map[string]string{"CMP-H1": md}, "compare", "blue", r).row(t, "CMP-H1")
		prefix := `refused before firing: this comparison member names the connection target "blue", and a ` + name + ` check cannot be pointed at a named target`
		if row.Status != report.StatusError || row.Failure == nil || !strings.HasPrefix(row.Failure.Observed, prefix) {
			t.Errorf("%s: status %q failure %+v", name, row.Status, row.Failure)
		}
		if len(r.props) != 0 {
			t.Errorf("%s: the runner fired: %v", name, r.props)
		}
	}
	// a check with its own target (on a layer that has a named form) is unchanged
	own := strings.Replace(cmpHTTPMD, "- **Layer**: HTTP Ingestion\n", "- **Layer**: HTTP Ingestion\n- **Target**: green\n", 1)
	r := &capRunner{t: t}
	row := runCmpTarget(t, blueCfg(), map[string]string{"CMP-H1": own}, "compare", "blue", r).row(t, "CMP-H1")
	if row.Status == report.StatusError || len(r.props) != 1 || hostOf(r.props[0]) != "green.invalid:9100" {
		t.Errorf("own target: status %q props %v", row.Status, r.props)
	}
}
