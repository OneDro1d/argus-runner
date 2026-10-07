package argus

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// LocalJMeterRunner drives JMeter as a LOCAL subprocess (JMeter-as-a-service, D2): JMeter is
// bundled into the same image as `serve`, so the runner invokes it directly — no
// `docker compose exec`, no docker socket handed to the server. The serve container joins the
// SUT's network so the jmeter subprocess reaches the SUT by service name. Templates are
// mounted/baked at TemplatesDir; results at the jtl path the orchestration passes.
type LocalJMeterRunner struct {
	TemplatesDir string // dir holding <base>.jmx (default /templates)
	JMeterBin    string // jmeter executable (default "jmeter")
}

func (r *LocalJMeterRunner) templates() string {
	if r.TemplatesDir != "" {
		return r.TemplatesDir
	}
	return "/templates"
}

func (r *LocalJMeterRunner) bin() string {
	if r.JMeterBin != "" {
		return r.JMeterBin
	}
	return "jmeter"
}

// buildArgs is the pure, deterministic JMeter argv (sorted -J props) — unit-tested.
//
// ⛔ A credential-bearing property (isSecretProp) is NEVER emitted as `-J`, whatever the caller
// passes: JMeter would echo it into the run log (see secret_props.go). Those travel in the private
// properties file named by secretsFile, passed with `-q`; "" when there is none.
func (r *LocalJMeterRunner) buildArgs(templateBase, jtlPath, secretsFile string, props map[string]string) []string {
	args := []string{"-n",
		"-t", filepath.ToSlash(filepath.Join(r.templates(), templateBase+".jmx")),
		"-l", jtlPath,
		"-j", jtlPath + ".log",
	}
	if secretsFile != "" {
		args = append(args, "-q", secretsFile)
	}
	public, _ := splitSecretProps(props)
	for _, k := range sortedKeys(public) {
		args = append(args, "-J"+k+"="+public[k])
	}
	return args
}

// runJMeter launches one template: the credentials go through a private properties file that
// exists only for the duration of the process, the rest of the properties as -J. The run log
// is created 0600 BEFORE JMeter opens it (log4j truncates or appends in place, keeping the mode),
// so whatever JMeter does write there is readable by the executor's user only.
func (r *LocalJMeterRunner) runJMeter(ctx context.Context, templateBase, jtlPath string, props map[string]string) ([]byte, error) {
	_, secret := splitSecretProps(props)
	secretsFile, remove, err := writeSecretPropsFile(secret)
	if err != nil {
		return nil, err
	}
	defer remove()
	if err := createPrivate(jtlPath + ".log"); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, r.bin(), r.buildArgs(templateBase, jtlPath, secretsFile, props)...)
	// VR12-TO-RUN: exec.CommandContext SIGKILLs the direct child on ctx's deadline, but
	// CombinedOutput() still blocks reading its output pipe until every process holding the
	// pipe's write end has exited — including a lingering grandchild the killed process forked
	// (measured: a `sh`-wrapped fake JMeter whose own child outlived it rode the child's exit, not
	// the kill, and returned only after the child's own sleep completed). WaitDelay bounds that:
	// once ctx is done, Go force-closes the pipes after this grace period even if something is
	// still holding them open, so Run reliably returns within its declared budget.
	cmd.WaitDelay = time.Second
	// the deadline must take the JVM too, not just the `jmeter` wrapper.
	killWholeGroup(cmd)
	return cmd.CombinedOutput()
}

// createPrivate creates (or truncates) p with mode 0600.
func createPrivate(p string) error {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// Run executes one template with its props, writing the .jtl. As with DockerRunner, an
// assertion failure is NOT an error here (it surfaces as a success=false row); only a failure
// to LAUNCH JMeter is an error (→ RO-04 errored classification).
//
// ⛔ REVISED (P1 #12 follow-up): timeout is jmeterProcessBackstop's GENEROUS process-level value —
// NOT the scenario's raw declared `## TIMEOUT` anymore. The first cut of VR12-TO-RUN passed the raw
// TimeoutDuration here directly, which made it a hard deadline on the WHOLE JMeter process
// (JVM start-up + every sampler a content template fires); a 10-15s TIMEOUT killed a healthy run on
// a slow pod, and a `## LOAD` scenario's ramp+duration (up to 86400s) would always exceed a 30-120s
// cap. TIMEOUT itself now bounds EACH REQUEST inside JMeter (DeriveProps' trigger.timeout_ms/
// trigger.timeout_s + templates/*.jmx's HTTPSampler.connect_timeout/response_timeout and
// JDBCSampler.queryTimeout); this ctx only has to catch a jmeter PROCESS that never returns at all
// (a genuinely hung/broken executor) — Run returns a distinct, named backstop error rather than the
// generic "jmeter exec failed" wrap: that string is one of report.executorFailureSignatures (an
// EXECUTOR/HARNESS failure, RO-04), and a scenario whose SUT was simply too slow is a SUT-blamed
// `failed`, never an `error` — see runOneScenario's default branch, which is exactly where an error
// NOT matching those signatures lands.
func (r *LocalJMeterRunner) Run(templateBase, jtlPath string, props map[string]string, timeout time.Duration) error {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	start := time.Now()
	out, err := r.runJMeter(ctx, templateBase, jtlPath, props)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("jmeter process exceeded its backstop of %s (stopped after %s) — this is "+
				"the EXECUTOR's own process-level backstop against a hung jmeter, not the scenario's "+
				"per-request ## TIMEOUT (each request is already bounded by TIMEOUT inside JMeter)",
				timeout, time.Since(start).Round(time.Millisecond))
		}
		return fmt.Errorf("jmeter exec failed: %v: %s", err, truncate(string(out), 400))
	}
	return nil
}

// Healthcheck verifies the bundled JMeter is runnable (RO-04 preflight) — `jmeter --version`.
func (r *LocalJMeterRunner) Healthcheck() error {
	out, err := exec.Command(r.bin(), "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("executor unreachable: bundled jmeter not runnable: %v: %s", err, truncate(strings.TrimSpace(string(out)), 200))
	}
	return nil
}

// ExecCleanup runs one `## CLEANUP` block (VR12-C2) in the SAME process space the run uses — the
// serve container, which already holds the SUT network reach and the resolved credentials. Rule 5:
// never more privilege than the scenario itself has.
//
//	bash → a `sh -c` subprocess, same working directory and environment as the run
//	sql  → the cleanup-sql template against targets.database, the SAME connection a
//	       `Database State` VERIFY uses
//
// ⛔ THE OUTPUT IS DISCARDED — see DockerRunner.ExecCleanup. Only the error's existence crosses
// this boundary; the message never does.
func (r *LocalJMeterRunner) ExecCleanup(form scenario.CleanupForm, body string, props map[string]string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	switch form {
	case scenario.CleanupBash:
		cmd := exec.CommandContext(ctx, "sh", "-c", body)
		cmd.WaitDelay = time.Second
		killWholeGroup(cmd) // a timed-out cleanup must not leave what it forked running
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return errCleanupTimeout
			}
			return errCleanupFailed
		}
		return nil
	case scenario.CleanupSQL:
		jtl := cleanupJTL(os.TempDir(), props["scenario.id"])
		_ = os.Remove(jtl) // JMeter -l APPENDS
		if _, err := r.runJMeter(ctx, "cleanup-sql", jtl, props); err != nil {
			if ctx.Err() != nil {
				return errCleanupTimeout
			}
			return errCleanupFailed
		}
		return nil
	}
	return errCleanupFailed
}
