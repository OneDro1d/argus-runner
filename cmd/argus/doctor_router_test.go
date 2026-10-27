package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/doctor"
	"github.com/OneDro1d/argus-runner/internal/onboard"
)

// noRouterState points router-wiring at an empty state directory, so a doctor test judges what it plants
// and never the router state of the machine running the tests.
func noRouterState(t *testing.T) {
	t.Helper()
	t.Setenv("ARGUS_ROUTER_STATE", filepath.Join(t.TempDir(), "no-router-state"))
}

// A tester machine's shape, planted (2026-10-09): the router onboarding last started holds 9766, one folder
// still names 9765 (F16), and its port-forward's pid is gone. A second folder is wired right. Process
// command lines come from a stub; the state and .mcp.json files are real files read the real way.
func TestDoctor_RouterWiringReadsTheLocalState(t *testing.T) {
	noHats(t)
	t.Setenv(cpTokenEnv, "odts_a-long-lived-author-pat")
	t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
	stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)

	state := t.TempDir()
	stale, good := t.TempDir(), t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const planted = "PLANTED-router-token-5e1f"
	write(filepath.Join(state, "state.json"), `{"port":9766,"folders":[`+
		`{"Path":"`+stale+`","Hat":"product","Token":"`+planted+`","Upstreams":{"sut-demo":{"URL":"http://localhost:52981/mcp","Token":"`+planted+`"}}},`+
		`{"Path":"`+good+`","Hat":"test","Token":"`+planted+`","Upstreams":{"pilot":{"URL":"http://localhost:58640/mcp","Token":"`+planted+`"}}}]}`)
	write(filepath.Join(state, "native-router.pid"), "2212238\n")
	write(filepath.Join(state, "tunnels", "native-pf-sut-demo.pid"), "2833473")
	write(filepath.Join(state, "tunnels", "native-pf-demo.pid"), "2221795")
	write(filepath.Join(stale, ".mcp.json"), `{"mcpServers":{"argus":{"type":"http","url":"http://127.0.0.1:9765/mcp"}}}`)
	write(filepath.Join(good, ".mcp.json"), `{"mcpServers":{"argus":{"type":"http","url":"http://127.0.0.1:9766/mcp"}}}`)
	t.Setenv("ARGUS_ROUTER_STATE", state)

	running := map[int]string{
		2212238: "/home/user/argus-kit-0.3.68/bin/argus router serve --state " + state,
		2221795: "kubectl -n argus-inst-demo port-forward --address 127.0.0.1 svc/executor 58640:8080",
	}
	prev := doctorProcArgs
	doctorProcArgs = func(pid int) (string, bool) { return running[pid], true }
	t.Cleanup(func() { doctorProcArgs = prev })

	rep, _, out := runDoctorKeepingState(t, "--control-plane", "https://argus-dev.onedroid.ai", "--scenarios", scenariosDir(t))
	c := checkByID(t, rep, "router-wiring")
	if c.Status != doctor.StatusWarn {
		t.Fatalf("router-wiring = %q, want warn: %s", c.Status, c.Detail)
	}
	for _, want := range []string{stale + " (product hat, port 9765)", "sut-demo (pid 2833473) on port 52981", "pid 2212238) is running"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail lacks %q: %s", want, c.Detail)
		}
	}
	if strings.Contains(c.Detail, good) || strings.Contains(c.Detail, "pilot (pid") {
		t.Errorf("detail names the healthy folder or tunnel: %s", c.Detail)
	}
	if !strings.Contains(c.Fix, "http://127.0.0.1:9766/mcp") || !strings.Contains(c.Fix, "svc/executor 52981:8080") {
		t.Errorf("fix = %s", c.Fix)
	}
	if strings.Contains(out, planted) {
		t.Errorf("LEAK: a token from state.json is in doctor's output")
	}

	// The router's pid now belongs to another program: the router is not running.
	running[2212238] = "/usr/bin/sleep 600"
	rep, _, _ = runDoctorKeepingState(t, "--control-plane", "https://argus-dev.onedroid.ai", "--scenarios", scenariosDir(t))
	if c := checkByID(t, rep, "router-wiring"); c.Status != doctor.StatusFail || !strings.Contains(c.Detail, "not running") {
		t.Errorf("reused router pid: router-wiring = %q %s", c.Status, c.Detail)
	}

	// --instance pilot judges only pilot's folder and tunnel, both fine.
	running[2212238] = "argus router serve"
	rep, _, _ = runDoctorKeepingState(t, "--control-plane", "https://argus-dev.onedroid.ai", "--scenarios", scenariosDir(t), "--instance", "pilot")
	if c := checkByID(t, rep, "router-wiring"); c.Status != doctor.StatusOK {
		t.Errorf("--instance pilot: router-wiring = %q %s", c.Status, c.Detail)
	}
}

func TestDoctor_RouterWiringSkipsWithNoState(t *testing.T) {
	noHats(t)
	t.Setenv(cpTokenEnv, "odts_a-long-lived-author-pat")
	stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)
	rep, _, _ := runDoctor(t, "--control-plane", "https://argus-dev.onedroid.ai", "--scenarios", scenariosDir(t))
	if c := checkByID(t, rep, "router-wiring"); c.Status != doctor.StatusSkip {
		t.Errorf("router-wiring = %q %s", c.Status, c.Detail)
	}
}

func TestDoctorProcArgs_ThisProcessAndAGoneOne(t *testing.T) {
	args, known := doctorProcArgs(os.Getpid())
	if !known || args == "" {
		t.Skipf("this system shows no command lines (known=%v)", known)
	}
	if p := procOf(os.Getpid(), "no-such-word-in-argv"); p.State != doctor.ProcOther {
		t.Errorf("own pid with a word it lacks = %+v, want other", p)
	}
	// A pid above any default pid_max is no process.
	if p := procOf(1<<22+12345, "x"); p.State != doctor.ProcGone {
		t.Errorf("pid %s = %+v, want gone", strconv.Itoa(1<<22+12345), p)
	}
}

// runDoctorKeepingState is runDoctor without noRouterState: for the tests that plant the state themselves.
func runDoctorKeepingState(t *testing.T, args ...string) (doctor.Report, int, string) {
	t.Helper()
	var rc int
	out := captureStdout(t, func() { rc = cmdDoctor(args) })
	rep := doctor.Report{}
	if err := jsonUnmarshalReport(out, &rep); err != nil {
		t.Fatalf("doctor did not print one JSON report: %v\n%s", err, out)
	}
	return rep, rc, out
}

func jsonUnmarshalReport(out string, rep *doctor.Report) error {
	return json.Unmarshal([]byte(out), rep)
}
