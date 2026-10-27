package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/doctor"
	"github.com/OneDro1d/argus-runner/internal/router"
)

// gatherRouter reads the local router's state for router-wiring: state.json, the pidfiles native onboarding
// writes (native-router.pid, tunnels/native-pf-<id>.pid), and each folder's .mcp.json.
//
// It parses state.json itself and never calls router.LoadState: that one lifts a legacy file and SAVES the
// result, and doctor writes nothing. Of the state it keeps ports, paths, hats and instance ids — never a token.
func gatherRouter(instance string) doctor.RouterInput {
	dir := router.StateDir()
	in := doctor.RouterInput{StateDir: dir, Instance: instance}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if errors.Is(err, fs.ErrNotExist) {
		in.Absent = true
		return in
	}
	if err != nil {
		in.Err = err.Error()
		return in
	}
	var st struct {
		Port    int `json:"port"`
		Folders []struct {
			Path      string
			Hat       string
			Upstreams map[string]struct{ URL string }
		} `json:"folders"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		in.Err = "state.json does not parse: " + err.Error()
		return in
	}
	in.Port = st.Port
	in.Router = procOf(readPID(filepath.Join(dir, "native-router.pid")), "router", "serve")

	tunnelPort := map[string]int{}
	for _, f := range st.Folders {
		rf := doctor.RouterFolder{Path: f.Path, Hat: f.Hat}
		for id, up := range f.Upstreams {
			rf.Instances = append(rf.Instances, id)
			if _, ok := tunnelPort[id]; !ok {
				tunnelPort[id] = urlPort(up.URL)
			}
		}
		sort.Strings(rf.Instances)
		if _, err := os.Stat(f.Path); errors.Is(err, fs.ErrNotExist) {
			rf.Missing = true
		} else {
			rf.MCPPort, rf.MCPErr = folderRouterPort(f.Path)
		}
		in.Folders = append(in.Folders, rf)
	}
	for id, port := range tunnelPort {
		pid := readPID(filepath.Join(dir, "tunnels", "native-pf-"+id+".pid"))
		in.Tunnels = append(in.Tunnels, doctor.RouterTunnel{Instance: id, Port: port, Proc: procOf(pid, "port-forward")})
	}
	return in
}

// folderRouterPort is the port in the folder's .mcp.json mcpServers.argus.url, or why there is none.
func folderRouterPort(folder string) (int, string) {
	b, err := os.ReadFile(filepath.Join(folder, mcpJSONName))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, "no " + mcpJSONName
	}
	if err != nil {
		return 0, mcpJSONName + " unreadable: " + err.Error()
	}
	var m struct {
		MCPServers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return 0, mcpJSONName + " does not parse"
	}
	e, ok := m.MCPServers[routerServerKey]
	if !ok || e.URL == "" {
		return 0, "no " + routerServerKey + " entry in " + mcpJSONName
	}
	if p := urlPort(e.URL); p > 0 {
		return p, ""
	}
	return 0, routerServerKey + " entry names no local port"
}

func urlPort(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(u.Port())
	return p
}

func readPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// doctorProcArgs is a process's command line, space-joined, and whether this system could say. It is the
// rule onboarding's native_proc_args applies (onboarding/lib/native.sh): /proc on Linux, `ps -o args=`
// elsewhere. A seam for tests.
var doctorProcArgs = func(pid int) (args string, known bool) {
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil {
		return strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " ")), true
	}
	if _, err := os.Stat("/proc/self"); err == nil {
		return "", true // a /proc system with no such pid: gone
	}
	if runtime.GOOS == "windows" {
		return "", false
	}
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return strings.TrimSpace(string(out)), true
	case errors.As(err, &ee):
		return "", true // ps ran and found no such pid (it exits 1)
	default:
		return "", false
	}
}

// procOf judges a pidfile's process: running only when its command line holds every word the pidfile is for,
// so a pid the OS has since given to another program is not taken for the router.
func procOf(pid int, words ...string) doctor.Proc {
	if pid == 0 {
		return doctor.Proc{}
	}
	args, known := doctorProcArgs(pid)
	switch {
	case !known:
		return doctor.Proc{PID: pid, State: doctor.ProcUnknown}
	case args == "":
		return doctor.Proc{PID: pid, State: doctor.ProcGone}
	}
	for _, w := range words {
		if !strings.Contains(args, w) {
			return doctor.Proc{PID: pid, State: doctor.ProcOther}
		}
	}
	return doctor.Proc{PID: pid, State: doctor.ProcRunning}
}
