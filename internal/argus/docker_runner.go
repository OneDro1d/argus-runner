package argus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// DockerRunner drives JMeter inside the long-running `jmeter` compose service
// (acme-jmeter:5.6.3a) via `docker compose exec`. The templates dir is mounted
// at /templates and the results dir at /results (compose handles host paths +
// the stack network so JMeter reaches the SUT by service name). This is the
// runner-core's sole JMeter invocation (NFR-4).
type DockerRunner struct {
	ComposeFile     string // abs path to deploy/compose/docker-compose.yaml
	Service         string // compose service name (default "jmeter")
	HostResultsRoot string // abs host dir bind-mounted at /results
}

// Run executes one template with -J props, writing jtlHostPath (which must live
// under HostResultsRoot). Returns an error only if JMeter could not be launched
// or exited non-zero in a way that prevented a result (assertion failures still
// produce a .jtl and are NOT errors here — they show up as success=false rows).
//
// ⛔ REVISED (P1 #12 follow-up): timeout is jmeterProcessBackstop's GENEROUS process-level value,
// not the scenario's raw declared `## TIMEOUT` — see LocalJMeterRunner.Run's comment for why. The
// per-request bound now lives inside JMeter itself (trigger.timeout_ms/trigger.timeout_s); this ctx
// only has to catch a `docker exec` that never returns (real cancellation, the client process
// killed), and the timeout path returns a distinct error rather than the "jmeter exec failed" wrap
// (that string is an executorFailureSignature and would misclassify a slow-but-reachable SUT as a
// dead rig).
func (d *DockerRunner) Run(templateBase, jtlHostPath string, props map[string]string, timeout time.Duration) error {
	svc := d.Service
	if svc == "" {
		svc = "jmeter"
	}
	rel, err := filepath.Rel(d.HostResultsRoot, jtlHostPath)
	if err != nil {
		return fmt.Errorf("jtl path %q not under results root %q: %w", jtlHostPath, d.HostResultsRoot, err)
	}
	containerJTL := "/results/" + filepath.ToSlash(rel)
	args, stdin := d.jmeterArgs(svc, templateBase, containerJTL, containerJTL+".log", props)
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	start := time.Now()
	cmd := exec.CommandContext(ctx, "docker", args...)
	// ⚠ WEAKER THAN LocalJMeterRunner's: `docker exec`'s client process is what ctx kills — that
	// stops OUR wait, but a `docker exec` client kill does not itself send a signal to the process
	// INSIDE the container (a known `docker exec` limitation, not a Go one), so a long-running
	// JMeter java process on this runner profile can keep running in the container after Run
	// returns. D2 (SERVICE-MAP) bundles JMeter into the SAME image as `serve` for the shipped
	// profile, where LocalJMeterRunner (same file's sibling type) gets the full, in-process kill;
	// this profile still gets a bounded, correctly-classified Run() return either way.
	cmd.WaitDelay = time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
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

// JMeterCapturePath is where JMeter, inside the compose service, sees a directory of the executor's
// results volume (ARGUS-CMP-3's output.capture.dir): the volume is bind-mounted at /results. A path
// outside the volume is returned unchanged (JMeter will not find it, and the capture then records
// nothing rather than something wrong).
func (d *DockerRunner) JMeterCapturePath(hostPath string) string {
	rel, err := filepath.Rel(d.HostResultsRoot, hostPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return hostPath
	}
	return "/results/" + filepath.ToSlash(rel)
}

// secretsWrapper runs INSIDE the executor container: it receives the private properties on stdin,
// writes them to a 0600 file in the container's own temp dir — never on the shared results volume
// and never on a command line — runs JMeter with `-q` on it and removes it whatever JMeter's exit
// code. `$0` is "jmeter" and `$@` the argv the runner built (see jmeterArgs).
const secretsWrapper = `umask 077
f=$(mktemp) || exit 1
cat > "$f" || exit 1
"$0" -q "$f" "$@"
rc=$?
rm -f "$f"
exit $rc`

// jmeterArgs is the pure, deterministic `docker compose exec` argv for one JMeter launch, plus the
// bytes to feed it on stdin (nil when the property set carries no credential, in which case the
// argv is the plain `jmeter …` exec it always was). logPath "" omits `-j`.
//
// ⛔ A credential-bearing property (isSecretProp) is NEVER emitted as `-J`: JMeter would echo it
// into the run log (see secret_props.go). It travels through secretsWrapper's stdin instead.
func (d *DockerRunner) jmeterArgs(svc, templateBase, containerJTL, logPath string, props map[string]string) (args []string, stdin []byte) {
	public, secret := splitSecretProps(props)
	jm := []string{"-n", "-t", "/templates/" + templateBase + ".jmx", "-l", containerJTL}
	if logPath != "" {
		jm = append(jm, "-j", logPath)
	}
	for _, k := range sortedKeys(public) {
		jm = append(jm, "-J"+k+"="+public[k])
	}
	args = []string{"compose", "-f", d.ComposeFile, "exec", "-T", svc}
	if len(secret) == 0 {
		return append(append(args, "jmeter"), jm...), nil
	}
	return append(append(args, "sh", "-c", secretsWrapper, "jmeter"), jm...), encodeProperties(secret)
}

// Healthcheck verifies the JMeter executor container is reachable (RO-04 preflight):
// exec a trivial command in it. A failure means the rig is down → the run is classified
// `error` (executor unavailable) instead of N SUT `failed`.
func (d *DockerRunner) Healthcheck() error {
	svc := d.Service
	if svc == "" {
		svc = "jmeter"
	}
	out, err := exec.Command("docker", "compose", "-f", d.ComposeFile, "exec", "-T", svc, "true").CombinedOutput()
	if err != nil {
		return fmt.Errorf("executor unreachable: %v: %s", err, truncate(string(out), 200))
	}
	return nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ExecCleanup runs one `## CLEANUP` block (VR12-C2). ⛔ SAME CONTAINER, SAME CREDENTIALS, SAME
// WORKING DIRECTORY as the run itself — rule 5 forbids privilege escalation, and the simplest way
// to guarantee it is to use the executor the run already uses rather than opening a second door.
//
//	bash → `docker compose exec` in the executor service, exactly as Run does
//	sql  → the cleanup-sql template against targets.database, the SAME connection a
//	       `Database State` VERIFY uses (no new configuration — a requirement of the row)
//
// ⛔ THE OUTPUT IS DISCARDED. It would otherwise reach report.json, which agents fetch and paste
// into chat, and a command line can carry a resolved ${VAR}. Only the error's EXISTENCE crosses
// this boundary; the message never does.
func (d *DockerRunner) ExecCleanup(form scenario.CleanupForm, body string, props map[string]string, timeout time.Duration) error {
	svc := d.Service
	if svc == "" {
		svc = "jmeter"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	switch form {
	case scenario.CleanupBash:
		cmd := exec.CommandContext(ctx, "docker", "compose", "-f", d.ComposeFile, "exec", "-T", svc, "sh", "-c", body)
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return errCleanupTimeout
			}
			return errCleanupFailed
		}
		return nil
	case scenario.CleanupSQL:
		jtl := cleanupJTL(d.HostResultsRoot, props["scenario.id"])
		rel, err := filepath.Rel(d.HostResultsRoot, jtl)
		if err != nil {
			return errCleanupFailed
		}
		_ = os.Remove(jtl) // JMeter -l APPENDS
		args, stdin := d.jmeterArgs(svc, "cleanup-sql", "/results/"+filepath.ToSlash(rel), "", props)
		cmd := exec.CommandContext(ctx, "docker", args...)
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return errCleanupTimeout
			}
			return errCleanupFailed
		}
		return nil
	}
	return errCleanupFailed
}

// errCleanupTimeout / errCleanupFailed carry NO detail on purpose — see ExecCleanup's note. The
// operator's detail lives in the executor's own log, which is not fetched by an agent.
var (
	errCleanupTimeout = errors.New("cleanup timed out")
	errCleanupFailed  = errors.New("cleanup did not complete")
)
