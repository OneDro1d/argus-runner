package doctor

import (
	"fmt"
	"sort"
	"strings"
)

// ProcState is what doctor could tell about a process named in a pidfile.
type ProcState string

const (
	ProcRunning ProcState = "running" // alive, and its command line is the one the pidfile is for
	ProcGone    ProcState = "gone"    // no such process
	ProcOther   ProcState = "other"   // alive, but another command: the pid was reused
	ProcUnknown ProcState = "unknown" // this platform shows neither /proc nor ps: not judged
)

// Proc is one pidfile's process. PID 0 = there is no pidfile.
type Proc struct {
	PID   int
	State ProcState
}

// RouterFolder is one agent folder the router's state routes for, and what its .mcp.json names.
type RouterFolder struct {
	Path      string
	Hat       string
	Instances []string // the upstreams onboarded into it
	MCPPort   int      // the port in .mcp.json's mcpServers.argus.url; 0 with MCPErr
	MCPErr    string   // why that port could not be read: no .mcp.json, no argus entry, not a local URL
	Missing   bool     // the folder itself is gone: the state still routes for it, nothing can call
}

// RouterTunnel is one instance's native port-forward (tunnels/native-pf-<id>.pid).
type RouterTunnel struct {
	Instance string
	Port     int // the local port the folders' upstream URL names
	Proc     Proc
}

// RouterInput is the local router's state as cmd/argus read it from disk — never through router.LoadState,
// which may rewrite the file. No field holds a token.
type RouterInput struct {
	StateDir string
	Absent   bool   // no state.json: nothing was onboarded natively here
	Err      string // state.json exists and could not be read
	Port     int    // the port state.json says the router holds
	Router   Proc   // native-router.pid
	Folders  []RouterFolder
	Tunnels  []RouterTunnel
	Instance string // --instance: judge only the folders and the tunnel for it
}

// executorNamespace mirrors updatecmd.InstanceNamespace: this package does no I/O and imports no command code.
func executorNamespace(instance string) string { return "argus-inst-" + instance }

// CheckRouterWiring reports whether the local agent folders can reach Argus through the router on this
// machine: each folder's .mcp.json names the port the router holds, that router is running, and the
// port-forward to each instance they use is running.
//
// WHY. None of it is visible from the control plane, and every part failed on a tester's machine
// (2026-10-07..09): a router started by a later onboarding took a new port while six folders kept the old
// one (F16: the product hat wired to an orphan router), and the port-forward stopped when the executor's
// pods changed. An agent there sees only "the argus server failed to connect". It reads local files and
// asks the OS whether a pid runs; it contacts nothing.
func CheckRouterWiring(in RouterInput) Check {
	c := Check{
		ID:      "router-wiring",
		What:    "the agent folders on this machine reach Argus: each .mcp.json names the router's port, the router runs, and so do the port-forwards they use",
		Subject: "router state in " + orNone(in.StateDir) + " and each folder's .mcp.json",
	}
	if in.Absent {
		c.Status = StatusSkip
		c.Detail = "no router state in " + orNone(in.StateDir) + ": nothing was onboarded natively on this machine (or ARGUS_ROUTER_STATE names another directory)."
		return c
	}
	if in.Err != "" {
		c.Status = StatusUnknown
		c.Detail = "could not read the router state in " + in.StateDir + ": " + in.Err + " — this is NOT a statement about the wiring."
		c.Fix = "check the file's owner and mode (it is written 0600 by the user who onboarded)."
		return c
	}

	judged := map[string]bool{}
	var folders []RouterFolder
	for _, f := range in.Folders {
		if in.Instance != "" && !contains(f.Instances, in.Instance) {
			continue
		}
		folders = append(folders, f)
		for _, i := range f.Instances {
			if in.Instance == "" || i == in.Instance {
				judged[i] = true
			}
		}
	}

	c.Status = StatusOK
	var details, fixes []string
	scope := ""
	if in.Instance != "" {
		scope = " for instance " + in.Instance
	}
	details = append(details, fmt.Sprintf("%d %s%s through the router on port %d.", len(folders), plural(len(folders), "folder routes", "folders route"), scope, in.Port))

	switch in.Router.State {
	case procNone:
	case ProcRunning:
		details = append(details, fmt.Sprintf("The router onboarding started (pid %d) is running.", in.Router.PID))
	case ProcGone, ProcOther:
		why := "no such process"
		if in.Router.State == ProcOther {
			why = "that pid is now another program"
		}
		c.Status = StatusFail
		details = append(details, fmt.Sprintf("The router onboarding started (pid %d) is not running (%s): every folder here reaches nothing.", in.Router.PID, why))
		fixes = append(fixes, "start the router again: re-run onboarding for one of these folders, then re-run argus doctor.")
	default:
		c.Status = worse(c.Status, StatusUnknown)
		details = append(details, fmt.Sprintf("Doctor could not tell whether the router (pid %d) is running: this system shows neither /proc nor ps.", in.Router.PID))
	}
	if in.Router.PID == 0 {
		details = append(details, "There is no native-router.pid: this router was not started by native onboarding (a container, or by hand), so whether it runs is not judged here.")
	}

	var miswired, noEntry, gone []string
	for _, f := range folders {
		switch {
		case f.Missing:
			gone = append(gone, f.Path)
		case f.MCPErr != "":
			noEntry = append(noEntry, fmt.Sprintf("%s (%s hat: %s)", f.Path, f.Hat, f.MCPErr))
		case f.MCPPort != in.Port:
			miswired = append(miswired, fmt.Sprintf("%s (%s hat, port %d)", f.Path, f.Hat, f.MCPPort))
		}
	}
	if len(miswired) > 0 {
		c.Status = worse(c.Status, StatusWarn)
		details = append(details, fmt.Sprintf("%d %s a port the router does not hold (it holds %d), so an agent there cannot reach Argus: %s.",
			len(miswired), plural(len(miswired), "folder's .mcp.json names", "folders' .mcp.json name"), in.Port, strings.Join(miswired, ", ")))
		fixes = append(fixes, fmt.Sprintf("in each, set mcpServers.argus.url in .mcp.json to http://127.0.0.1:%d/mcp (the router token there stays valid) "+
			"and start a new agent session in that folder; or re-run onboarding for it.", in.Port))
	}
	if len(noEntry) > 0 {
		c.Status = worse(c.Status, StatusWarn)
		details = append(details, "The router routes for these folders, but they name no router to call: "+strings.Join(noEntry, ", ")+".")
		fixes = append(fixes, "re-run onboarding for each, which writes the argus entry into its .mcp.json.")
	}
	if len(gone) > 0 {
		details = append(details, "Not judged, because the folder no longer exists (the state still routes for it, left by a removal without teardown; harmless): "+strings.Join(gone, ", ")+".")
	}

	tunnels := append([]RouterTunnel(nil), in.Tunnels...)
	sort.SliceStable(tunnels, func(i, j int) bool { return tunnels[i].Instance < tunnels[j].Instance })
	var dead, unsure []string
	for _, t := range tunnels {
		if !judged[t.Instance] || t.Proc.PID == 0 {
			continue
		}
		switch t.Proc.State {
		case ProcGone, ProcOther:
			dead = append(dead, fmt.Sprintf("%s (pid %d) on port %d", t.Instance, t.Proc.PID, t.Port))
			port := "<port>"
			if t.Port > 0 {
				port = fmt.Sprint(t.Port)
			}
			fixes = append(fixes, fmt.Sprintf("restart %s's forward: kubectl -n %s port-forward --address 127.0.0.1 svc/executor %s:8080 "+
				"(with the --kubeconfig/--context onboarding used), or re-run onboarding for it.", t.Instance, executorNamespace(t.Instance), port))
		case ProcUnknown:
			unsure = append(unsure, fmt.Sprintf("%s (pid %d)", t.Instance, t.Proc.PID))
		}
	}
	if len(dead) > 0 {
		c.Status = worse(c.Status, StatusWarn)
		details = append(details, fmt.Sprintf("The port-forward to %d %s is not running, so every call to it fails: %s. "+
			"kubectl port-forward holds one executor pod and stops when that pod goes (a tester's stopped while its executor scaled 1→3).",
			len(dead), plural(len(dead), "instance", "instances"), strings.Join(dead, ", ")))
	}
	if len(unsure) > 0 {
		c.Status = worse(c.Status, StatusUnknown)
		details = append(details, "Doctor could not tell whether these port-forwards run (no /proc and no ps here): "+strings.Join(unsure, ", ")+".")
	}

	c.Detail = strings.Join(details, " ")
	c.Fix = strings.Join(fixes, " ")
	return c
}

// procNone is a Proc with no pidfile: its zero State.
const procNone ProcState = ""

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
