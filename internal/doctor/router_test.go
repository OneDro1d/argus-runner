package doctor

import (
	"strings"
	"testing"
)

// The router state on a tester operator's machine, 2026-10-09:
// the router onboarding last started holds 9766; six folders onboarded earlier still name 9765, where
// nothing listens (F16: the product hat wired to an orphan router); three of the four port-forwards are
// dead (one died when the executor scaled 1→3). Every argus call from those folders fails, and no check
// looked: doctor contacted only the control plane, which sees none of it.
func pilotRouterState() RouterInput {
	folder := func(path, hat, inst string, port int) RouterFolder {
		return RouterFolder{Path: path, Hat: hat, Instances: []string{inst}, MCPPort: port}
	}
	return RouterInput{
		StateDir: "/home/user/.local/state/argus/router",
		Port:     9766,
		Router:   Proc{PID: 2212238, State: ProcRunning},
		Folders: []RouterFolder{
			folder("/home/user/memstore-native-product", "product", "sut-demo", 9765),
			folder("/home/user/memstore-native-tests", "test", "sut-demo", 9765),
			folder("/home/user/hub-native-product", "product", "hub-tester", 9765),
			folder("/home/user/hub-native-tests", "test", "hub-tester", 9765),
			folder("/home/user/docapp-aks/product-agent", "product", "docapp", 9765),
			folder("/home/user/docapp-aks/test-agent/scenarios", "test", "docapp", 9765),
			folder("/home/user/tester/pilot/product-agent", "product", "pilot", 9766),
			folder("/home/user/tester/pilot/scenarios", "test", "pilot", 9766),
		},
		Tunnels: []RouterTunnel{
			{Instance: "docapp", Port: 50630, Proc: Proc{PID: 1815743, State: ProcGone}},
			{Instance: "sut-demo", Port: 52981, Proc: Proc{PID: 2833473, State: ProcGone}},
			{Instance: "pilot", Port: 58640, Proc: Proc{PID: 2221795, State: ProcRunning}},
			{Instance: "hub-tester", Port: 58692, Proc: Proc{PID: 3617261, State: ProcGone}},
		},
	}
}

func TestCheckRouterWiring_ReplayTesterMachine(t *testing.T) {
	c := CheckRouterWiring(pilotRouterState())
	if c.ID != "router-wiring" || c.Status != StatusWarn {
		t.Fatalf("got %s %q, want router-wiring warn: %s", c.ID, c.Status, c.Detail)
	}
	for _, want := range []string{
		"6 folders", "/home/user/memstore-native-product", "/home/user/docapp-aks/test-agent/scenarios", "9765", "9766",
		"sut-demo (pid 2833473)", "docapp (pid 1815743)", "hub-tester (pid 3617261)",
	} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail lacks %q: %s", want, c.Detail)
		}
	}
	if strings.Contains(c.Detail, "pilot/product-agent") || strings.Contains(c.Detail, "pilot (pid") {
		t.Errorf("detail names the healthy pilot folder or tunnel: %s", c.Detail)
	}
	for _, want := range []string{
		"http://127.0.0.1:9766/mcp",
		"kubectl -n argus-inst-sut-demo port-forward --address 127.0.0.1 svc/executor 52981:8080",
	} {
		if !strings.Contains(c.Fix, want) {
			t.Errorf("fix lacks %q: %s", want, c.Fix)
		}
	}
}

func TestCheckRouterWiring_InstanceNarrowsTheJudgment(t *testing.T) {
	in := pilotRouterState()
	in.Instance = "pilot"
	c := CheckRouterWiring(in)
	if c.Status != StatusOK {
		t.Fatalf("--instance pilot: got %q, want ok (its folders name 9766, its tunnel runs): %s", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "2 folders") {
		t.Errorf("detail should count the pilot folders it judged: %s", c.Detail)
	}
}

func TestCheckRouterWiring_RouterNotRunningFails(t *testing.T) {
	for _, st := range []ProcState{ProcGone, ProcOther} {
		in := pilotRouterState()
		in.Router.State = st
		c := CheckRouterWiring(in)
		if c.Status != StatusFail || !strings.Contains(c.Detail, "pid 2212238") || !strings.Contains(c.Detail, "not running") {
			t.Errorf("router %s: got %q %s", st, c.Status, c.Detail)
		}
	}
}

func TestCheckRouterWiring_NoStateSkips(t *testing.T) {
	c := CheckRouterWiring(RouterInput{StateDir: "/x", Absent: true})
	if c.Status != StatusSkip || !strings.Contains(c.Detail, "/x") {
		t.Errorf("got %q %s", c.Status, c.Detail)
	}
}

func TestCheckRouterWiring_UnreadableStateIsUnknown(t *testing.T) {
	c := CheckRouterWiring(RouterInput{StateDir: "/x", Err: "permission denied"})
	if c.Status != StatusUnknown || !strings.Contains(c.Detail, "permission denied") {
		t.Errorf("got %q %s", c.Status, c.Detail)
	}
}

func TestCheckRouterWiring_FolderWithoutAnArgusEntryWarns(t *testing.T) {
	in := RouterInput{StateDir: "/s", Port: 9765, Router: Proc{PID: 7, State: ProcRunning},
		Folders: []RouterFolder{{Path: "/w/p", Hat: "product", Instances: []string{"i"}, MCPErr: "no .mcp.json"}}}
	c := CheckRouterWiring(in)
	if c.Status != StatusWarn || !strings.Contains(c.Detail, "/w/p") || !strings.Contains(c.Detail, "no .mcp.json") {
		t.Errorf("got %q %s", c.Status, c.Detail)
	}
}

func TestCheckRouterWiring_GoneFolderIsNotedNotJudged(t *testing.T) {
	in := RouterInput{StateDir: "/s", Port: 9765, Router: Proc{PID: 7, State: ProcRunning},
		Folders: []RouterFolder{{Path: "/w/p", Hat: "product", Instances: []string{"i"}, Missing: true}}}
	c := CheckRouterWiring(in)
	if c.Status != StatusOK || !strings.Contains(c.Detail, "/w/p") || !strings.Contains(c.Detail, "no longer exists") {
		t.Errorf("got %q %s", c.Status, c.Detail)
	}
}

func TestCheckRouterWiring_UnreadableProcessIsUnknownNotOK(t *testing.T) {
	in := pilotRouterState()
	in.Instance = "pilot"
	in.Router.State = ProcUnknown
	c := CheckRouterWiring(in)
	if c.Status != StatusUnknown || !strings.Contains(c.Detail, "could not tell") {
		t.Errorf("got %q %s", c.Status, c.Detail)
	}
}
