package argus

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// · THE BACKSTOP KILLS THE WHOLE PROCESS GROUP.
//
// `jmeter` is a shell script that starts the JVM as its CHILD. exec.CommandContext signals only the
// direct child (the wrapper), so on the deadline the java grandchild kept running — observed publishing
// 30+ s after "stopped after 1m51s". The fake below is the same shape: a wrapper that forks a long-lived
// child and waits for it.
//
// ⛔ SAFETY: this test kills processes. The only thing it signals is the process group it created itself;
// all of its files live in a directory made UNDER THE WORKTREE (never /tmp, never a path it did not make),
// removed by name at the end. It never calls a real remover on anything else.

// pgTestDir makes a private scratch directory inside the package directory (the worktree).
func pgTestDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	d, err := os.MkdirTemp(wd, ".pgkill-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) }) // d is the directory MkdirTemp just made, nothing else
	return d
}

// procAlive: a process that exists and is not a zombie (a killed orphan nobody has reaped is still dead).
func procAlive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return false
	}
	return s[i+2] != 'Z'
}

func waitGone(pid int, within time.Duration) bool {
	end := time.Now().Add(within)
	for time.Now().Before(end) {
		if !procAlive(pid) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return !procAlive(pid)
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	var b []byte
	for i := 0; i < 100; i++ {
		var err error
		b, err = os.ReadFile(path)
		if err == nil && len(strings.TrimSpace(string(b))) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		t.Fatalf("no grandchild pid recorded at %s: %q (%v)", path, b, err)
	}
	return pid
}

// forkingFakeJMeter writes a `jmeter` wrapper that forks a 300 s child, records its pid and waits.
func forkingFakeJMeter(t *testing.T, dir string) (bin, pidFile string) {
	t.Helper()
	pidFile = filepath.Join(dir, "child.pid")
	bin = filepath.Join(dir, "jmeter")
	script := "#!/bin/sh\nsleep 300 &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, pidFile
}

func TestLocalRunner_BackstopKillsTheWholeProcessGroup(t *testing.T) {
	dir := pgTestDir(t)
	bin, pidFile := forkingFakeJMeter(t, dir)
	r := &LocalJMeterRunner{TemplatesDir: dir, JMeterBin: bin}
	start := time.Now()
	err := r.Run("amqp-load", filepath.Join(dir, "x.jtl"), map[string]string{"scenario.id": "S-1"}, 700*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "exceeded its backstop") {
		t.Fatalf("Run err = %v, want the named backstop error", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Run took %v: the WaitDelay bound no longer holds", d)
	}
	pid := readPid(t, pidFile)
	if !waitGone(pid, 3*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL) // do not leave the leak behind: it is the one process this test made
		t.Fatalf("the wrapper's child (pid %d) is still running after the backstop fired: the JVM would keep publishing", pid)
	}
}

func TestLocalRunner_CleanupTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	dir := pgTestDir(t)
	pidFile := filepath.Join(dir, "cleanup.pid")
	body := "sleep 300 &\necho $! > " + pidFile + "\nwait\n"
	r := &LocalJMeterRunner{TemplatesDir: dir}
	start := time.Now()
	err := r.ExecCleanup(scenario.CleanupBash, body, map[string]string{"scenario.id": "S-1"}, 700*time.Millisecond)
	if err == nil {
		t.Fatal("a cleanup that outlives its timeout must be an error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("ExecCleanup took %v", d)
	}
	pid := readPid(t, pidFile)
	if !waitGone(pid, 3*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the cleanup's child (pid %d) is still running after its timeout", pid)
	}
}
