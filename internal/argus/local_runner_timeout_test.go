package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSlowJMeter is a stand-in JMeter binary that ignores every flag and just sleeps — the
// process-level equivalent of "a TRIGGER that takes longer than its declared TIMEOUT": a hung SUT
// (accepted the connection, never answered) leaves the real jmeter subprocess blocked exactly like
// this. It only writes the .jtl AFTER the sleep, so a killed run leaves no .jtl behind — proof the
// process was actually stopped, not merely that Run() gave up on it.
const fakeSlowJMeter = `#!/bin/sh
jtl=""
while [ $# -gt 0 ]; do
  case "$1" in
    -l) jtl="$2"; shift;;
  esac
  shift
done
sleep 2
printf 'timeStamp,elapsed,label,responseCode,responseMessage,threadName,dataType,success,failureMessage,bytes,sentBytes,grpThreads,allThreads,URL,Latency,IdleTime,Connect\n' > "$jtl"
printf '1700000000000,12,SUT trigger,202,Accepted,T 1-1,text,true,,10,10,1,1,http://x,10,0,5\n' >> "$jtl"
`

func writeFakeSlowJMeter(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "jmeter")
	if err := os.WriteFile(p, []byte(fakeSlowJMeter), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLocalJMeterRunner_Run_EnforcesProcessBackstop is the RED/GREEN proof for P1 #12's PROCESS
// BACKSTOP on the JMeter path (http-ingestion, database-state, message-flow, external-delivery,
// error-path, rate-limiting, permissions, saga-presence — every layer that rides
// LocalJMeterRunner.Run) — exercising Run's `timeout` parameter directly at whatever value the
// caller passes it (jmeterProcessBackstop's job, argus.go, is tested separately: this fixture only
// proves Run() itself still enforces WHATEVER deadline it is given).
//
// BEFORE 3d91fd0, Run ran the subprocess under context.Background() — no deadline at all — so this
// exact fixture would block for the full 2s sleep and then return nil (a completed, if slow, run):
// "a scenario outliving its TIMEOUT" at the process level, not just unmeasured at the report level.
//
// AFTER the fix: the process is killed at the given deadline (200ms here), Run returns within well
// under the 2s sleep, the error names it as the PROCESS BACKSTOP (never as the scenario's own
// per-request TIMEOUT — that is now enforced separately, inside JMeter, per-request-timeout_test.go)
// and — the strongest proof the PROCESS itself was stopped, not just that Run gave up waiting on it
// — the .jtl is never written (the fake binary only writes it after the sleep completes).
func TestLocalJMeterRunner_Run_EnforcesProcessBackstop(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeSlowJMeter(t, dir)
	jtl := filepath.Join(dir, "x.jtl")
	r := &LocalJMeterRunner{TemplatesDir: "/templates", JMeterBin: bin}

	start := time.Now()
	err := r.Run("http-ingestion", jtl, map[string]string{"scenario.id": "S-1"}, 200*time.Millisecond)
	elapsed := time.Since(start)

	// budget (200ms) + LocalJMeterRunner's WaitDelay grace period (1s, for a lingering grandchild
	// holding the output pipe open) + scheduling slack — still far short of the fake SUT's 2s hang.
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("Run took %s — the process backstop was NOT enforced (it rode the fake "+
			"SUT's full 2s sleep, exactly the pre-fix defect)", elapsed)
	}
	if err == nil {
		t.Fatalf("Run returned nil after %s — a hung process must be reported, never silently absorbed", elapsed)
	}
	if !strings.Contains(err.Error(), "backstop") || !strings.Contains(err.Error(), "200ms") {
		t.Fatalf("error = %q, want it to name the BACKSTOP and the given 200ms deadline", err.Error())
	}
	if strings.Contains(err.Error(), "declared ## TIMEOUT") {
		t.Fatalf("error = %q must not claim to BE the scenario's own per-request ## TIMEOUT — this is "+
			"the process backstop, a distinct, more generous number (VR12-TO-RUN follow-up)", err.Error())
	}
	// jmeter exec failed" is one of report.executorFailureSignatures (RO-04, a dead-rig/harness
	// classification) — a timeout must NOT say it, or runOneScenario would misclassify a slow-but-
	// reachable SUT as `error` instead of the SUT-blamed `failed` the task requires.
	if strings.Contains(err.Error(), "jmeter exec failed") {
		t.Fatalf("error = %q must not contain the executor-failure signature %q (RO-04 misclassification)",
			err.Error(), "jmeter exec failed")
	}
	if _, statErr := os.Stat(jtl); !os.IsNotExist(statErr) {
		t.Fatalf(".jtl exists at %s — the subprocess was NOT actually killed (it ran to completion)", jtl)
	}
	t.Logf("GREEN: elapsed=%s err=%q", elapsed, err.Error())
}
