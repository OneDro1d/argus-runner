package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// ⛔ ENV-GATED, and meant to run in an OLD tree. scripts/amqp-load-old-executor-check.sh copies this file
// into a git worktree of the pinned release (v0.3.50, an executor that predates the `AMQP Load` layer)
// and runs it THERE (, design §3.4): an old executor handed a new AMQP Load scenario file
// must fire NOTHING.
//
// It uses nothing this change added (RunAll, config.Load and a recording runner), so it compiles in the
// old tree. In THIS tree it behaves differently on purpose (the layer is known and the ramp runs), which
// is why it SKIPS unless ARGUS_OLD_EXECUTOR=1 and is not part of `go test ./...`. With
// ARGUS_REQUIRE_OLDEXEC=1 a missing ARGUS_OLD_EXECUTOR is a FAILURE, never a skip ("ok" must not be the
// word for a test that did not run).

type oldexecRunner struct{ calls int }

func (r *oldexecRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	r.calls++
	return os.WriteFile(jtlPath, []byte("timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n"), 0o600)
}

const oldexecConfig = "project:\n  name: p\ntargets:\n  message_broker_targets:\n    load-lab:\n      type: amqp\n      url: amqp://u:x@load-lab.invalid:5672/\n" +
	"load_allowed_targets:\n  load-lab:\n    max_sessions: 1000\n"

const oldexecScenarioTmpl = "# Scenario: t\n\n## Metadata\n- **ID**: AL-OLD\n- **Layer**: AMQP Load\n- **Tags**: http, load\n%s\n## TRIGGER\nPOST `/amqp-load`\n\n" +
	"## EXPECT\n### Runnable\n- broker is not blocked\n\n## LOAD\n- **Steps**: 2\n- **Step Duration Seconds**: 20\n- **Target P95 Ms**: 250\n- **Max Error Rate**: 0.01\n\n" +
	"## TIMEOUT\n30s\n\n## CLEANUP\nN/A — fixture.\n"

func TestOldExecutorRefusesAMQPLoad(t *testing.T) {
	if os.Getenv("ARGUS_OLD_EXECUTOR") != "1" {
		if os.Getenv("ARGUS_REQUIRE_OLDEXEC") == "1" {
			t.Fatal("ARGUS_REQUIRE_OLDEXEC=1 but ARGUS_OLD_EXECUTOR is not 1: this test must run in the OLD tree, not be skipped")
		}
		t.Skip("env-gated: runs only in the old-executor worktree (scripts/amqp-load-old-executor-check.sh)")
	}
	for _, tc := range []struct {
		name, meta, want string
	}{
		{"with a Target: refused as a word with no meaning on the layer", "- **Target**: load-lab\n", "has no meaning on layer"},
		{"hand-written with no Target: refused for declaring no status", "", "status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "argus-config.yaml")
			if err := os.WriteFile(cfgPath, []byte(oldexecConfig), 0o644); err != nil {
				t.Fatal(err)
			}
			c, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("the OLD executor could not load a config carrying the new top-level key: %v", err)
			}
			sc := filepath.Join(dir, "scenarios", "amqp-load")
			_ = os.MkdirAll(sc, 0o755)
			body := strings.Replace(oldexecScenarioTmpl, "%s", tc.meta, 1)
			if err := os.WriteFile(filepath.Join(sc, "AL-OLD.md"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			rec := &oldexecRunner{}
			rr, err := RunAll(c, filepath.Join(dir, "scenarios"), filepath.Join(dir, "results"), "p", "", "", "", "", rec)
			if err != nil {
				t.Fatal(err)
			}
			if rec.calls != 0 {
				t.Fatalf("the old executor fired %d JMeter run(s) for an AMQP Load scenario", rec.calls)
			}
			var status, observed string
			for _, l := range rr.Report.Layers {
				for _, s := range l.Scenarios {
					status = s.Status
					if s.Failure != nil {
						observed = s.Failure.Observed
					}
				}
			}
			t.Logf("OLD EXECUTOR: status=%s observed=%s", status, observed)
			if status != "failed" && status != "error" {
				t.Errorf("status = %q, want failed or error", status)
			}
			if !strings.Contains(observed, tc.want) {
				t.Errorf("observed %q does not say %q", observed, tc.want)
			}
		})
	}
}
