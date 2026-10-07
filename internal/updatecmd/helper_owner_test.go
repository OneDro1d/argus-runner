package updatecmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// AC-D60 (#394) — THE UPDATE'S HELPER CONTAINERS MUST NOT LEAVE ROOT-OWNED 0700 FILES THE OPERATOR HAS TO READ.
//
// On Linux with the standard root Docker daemon a file root writes into a bind mount is root-owned on the
// host, and discover.sh / preflight.sh / apply.sh are 0700. The block then runs `bash "$STAGE_U/discover.sh"`
// as the operator: Permission denied, exit 126, nothing changed, every retry the same.
//
// THE RULE UNDER TEST: on a LINUX host whose operator is not root, EVERY helper `docker run` the block and the
// generated scripts make runs as the operator (`--user <uid>:<gid>`). Git Bash (uname MINGW*/MSYS*), macOS and a
// root operator keep today's behaviour exactly — no --user at all.
//
// uname and id are driven through stub binaries on PATH, so every platform is exercised on this one machine.

type platform struct {
	name, uname, uid, gid string
	wantUser              string // "" = no --user at all
	dockerInfo            string // what `docker info --format {{.SecurityOptions}}` answers ("" = a plain root daemon)
	dockerVersion         string // what `docker --version` answers ("" = Docker)
}

var helperPlatforms = []platform{
	{"linux non-root operator", "Linux", "1000", "1001", "1000:1001", "[name=apparmor name=seccomp,profile=builtin name=cgroupns]", "Docker version 27.3.1, build ce12230"},
	{"linux root operator", "Linux", "0", "0", "", "", ""},
	{"macOS", "Darwin", "501", "20", "", "", ""},
	{"Git Bash (MINGW)", "MINGW64_NT-10.0-22631", "197609", "197609", "", "", ""},
	{"Git Bash (MSYS)", "MSYS_NT-10.0-22631", "197609", "197609", "", "", ""},
}

// ⛔ reviewer (AC-D60): a daemon that already maps the container's root to the operator must NOT get --user — there it
// would land on a subordinate uid and the operator could not read what the helpers wrote. Driven through the block.
var remappingDaemons = []platform{
	{"linux rootless Docker", "Linux", "1000", "1001", "", "[name=seccomp,profile=builtin name=rootless name=cgroupns]", "Docker version 27.3.1, build ce12230"},
	{"linux Docker with userns-remap", "Linux", "1000", "1001", "", "[name=apparmor name=seccomp,profile=builtin name=userns]", "Docker version 27.3.1, build ce12230"},
	{"linux Podman behind a docker shim", "Linux", "1000", "1001", "", "", "podman version 4.9.3"},
}

// seamDir writes stub `uname` and `id`; anything else they are asked for is passed to the real program.
func seamDir(t *testing.T, p platform) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("uname", "if [ \"${1:-}\" = -s ]; then echo '"+p.uname+"'; else exec /usr/bin/env -i PATH=/usr/bin:/bin uname \"$@\"; fi\n")
	write("id", "case \"${1:-}\" in -u) echo "+p.uid+" ;; -g) echo "+p.gid+" ;; *) exec /usr/bin/env -i PATH=/usr/bin:/bin id \"$@\" ;; esac\n")
	return dir
}

// dockerRuns is every logged `docker run` line.
func dockerRuns(calls string) []string {
	var runs []string
	for _, l := range strings.Split(calls, "\n") {
		if strings.HasPrefix(l, "docker run ") {
			runs = append(runs, l)
		}
	}
	return runs
}

func checkUser(t *testing.T, what string, runs []string, p platform) {
	t.Helper()
	if len(runs) == 0 {
		t.Fatalf("%s: no helper `docker run` was made — the assertion would be vacuous", what)
	}
	for _, r := range runs {
		has := strings.Contains(r, " --user ")
		switch {
		case p.wantUser != "" && !strings.Contains(r, " --user "+p.wantUser+" "):
			t.Errorf("⛔ %s on %s: a helper container runs as root and will leave root-owned 0700 files — want `--user %s` in:\n  %s", what, p.name, p.wantUser, r)
		case p.wantUser == "" && has:
			t.Errorf("%s on %s must keep today's behaviour (no --user), got:\n  %s", what, p.name, r)
		}
	}
}

// ⭐ THE BLOCK THE PAGE PRINTS: dr() is where discover.sh, the staged kit and undo/ are created.
func TestRenderBlock_HelpersRunAsTheOperatorOnLinux(t *testing.T) {
	requireBash(t)
	for _, p := range append(append([]platform{}, helperPlatforms...), remappingDaemons...) {
		t.Run(p.name, func(t *testing.T) {
			root := t.TempDir()
			calls := filepath.Join(root, "calls.log")
			bin := filepath.Join(root, "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			docker := "#!/usr/bin/env bash\nprintf 'docker %s\\n' \"$*\" >> \"$CALLS\"\n" +
				"case \"$*\" in\n  run\\ *) exit 1 ;;\n  *RepoDigests*) echo 'ghcr.io/x/exec@sha256:new' ;;\n" +
				"  info\\ *SecurityOptions*) echo '" + p.dockerInfo + "' ;;\n  --version) echo '" + p.dockerVersion + "' ;;\nesac\nexit 0\n"
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0o755); err != nil {
				t.Fatal(err)
			}
			block, _ := RenderBlock(Instance{Tier: "compose", InstanceID: "i1", Image: "ghcr.io/x/exec:0.3.32", KitDir: filepath.Join(root, "kits", "i1")})
			if block == "" {
				t.Fatal("no block was rendered")
			}
			cmd := exec.Command("bash", "-c", block)
			cmd.Dir = root
			cmd.Env = []string{
				"PATH=" + seamDir(t, p) + string(os.PathListSeparator) + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
				"HOME=" + root, "CALLS=" + calls, "ARGUS_ROUTER_STATE=" + filepath.Join(root, "state"),
			}
			_ = os.MkdirAll(filepath.Join(root, "state"), 0o755)
			_ = os.MkdirAll(filepath.Join(root, "kits"), 0o755)
			// the first helper fails (exit 1) so the block stops right after recording it
			_, _ = cmd.CombinedOutput()
			checkUser(t, "the update block's first helper (update discover)", dockerRuns(readFile(t, calls)), p)
		})
	}
}

// ⭐ MEASURED WITH REAL UIDS, WHERE THE ENVIRONMENT ALLOWS (the d48ProbeAsNobody pattern: setpriv, and UNPROBED —
// recorded as a skip — when it cannot). The test process must be root to play the root daemon's helper.
//
//	root helper (no --user)        -> discover.sh is root:root 0700 -> the operator's bash exits 126  (the defect)
//	helper run with the block's own flag -> discover.sh is the operator's 0700 -> the operator's bash runs it
//
// The flag is NOT typed here: it is read from the `docker run` the real rendered block makes when it is run as
// the unprivileged user.
func TestHelperOwner_ANonRootOperatorCanRunWhatTheHelperWrote(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("UNPROBED: this ownership rule is a Linux root-daemon behaviour")
	}
	if os.Geteuid() != 0 {
		t.Skip("UNPROBED: not root, so this process cannot play a root Docker daemon's helper or switch to an unprivileged operator; " +
			"run it as root with setpriv (as the Go build container does) to measure the ownership rule")
	}
	setpriv, err := exec.LookPath("setpriv")
	if err != nil {
		t.Skip("UNPROBED: no setpriv to drop to an unprivileged operator")
	}
	const nobody = "65534"
	asNobody := []string{setpriv, "--reuid=" + nobody, "--regid=" + nobody, "--clear-groups"}

	dir, err := os.MkdirTemp("", "acd60-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	for _, d := range []string{dir, filepath.Join(dir, "kits"), filepath.Join(dir, "state"), filepath.Join(dir, "bin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(d, 65534, 65534); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(dir, "calls.log")
	// os.WriteFile's mode passes through the umask: under 022 calls.log is 0644 and the unprivileged operator
	// cannot append to it; under 077 the stub is 0700 and it cannot run it, so the real docker answers instead.
	// Either way the test fails for a reason that is not the rule it measures — set both modes explicitly.
	if err := os.WriteFile(calls, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(calls, 0o666); err != nil {
		t.Fatal(err)
	}
	docker := "#!/usr/bin/env bash\nprintf 'docker %s\\n' \"$*\" >> \"" + calls + "\"\n" +
		"case \"$*\" in\n  run\\ *) exit 1 ;;\n  *RepoDigests*) echo 'ghcr.io/x/exec@sha256:new' ;;\nesac\nexit 0\n"
	stub := filepath.Join(dir, "bin", "docker")
	if err := os.WriteFile(stub, []byte(docker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	block, _ := RenderBlock(Instance{Tier: "compose", InstanceID: "i1", Image: "ghcr.io/x/exec:0.3.32", KitDir: filepath.Join(dir, "kits", "i1")})
	cmd := exec.Command(asNobody[0], append(asNobody[1:], "bash", "-c", block)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + filepath.Join(dir, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + dir,
		"ARGUS_ROUTER_STATE=" + filepath.Join(dir, "state")}
	_, _ = cmd.CombinedOutput()
	runs := dockerRuns(readFile(t, calls))
	if len(runs) == 0 {
		t.Fatal("the block made no helper docker run as the unprivileged operator")
	}
	user := ""
	f := strings.Fields(runs[0])
	for i, w := range f {
		if w == "--user" && i+1 < len(f) {
			user = f[i+1]
		}
	}

	stage := filepath.Join(dir, "stage")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(stage, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	// the helper, as the ENGINE runs it: a process with the container's uid, writing a 0700 script into the mount
	helper := func(asUser string) {
		script := "printf '#!/usr/bin/env bash\\necho discovered\\n' > " + stage + "/discover.sh && chmod 0700 " + stage + "/discover.sh"
		argv := []string{"bash", "-c", script}
		if asUser != "" {
			parts := strings.SplitN(asUser, ":", 2)
			argv = append([]string{setpriv, "--reuid=" + parts[0], "--regid=" + parts[1], "--clear-groups"}, argv...)
		}
		if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("the modelled helper failed: %v\n%s", err, out)
		}
	}
	operatorRuns := func() (int, string) {
		out, err := exec.Command(asNobody[0], append(asNobody[1:], "bash", stage+"/discover.sh")...).CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		}
		return 0, string(out)
	}

	helper("") // today's behaviour: no --user, the container is root
	if code, out := operatorRuns(); code != 126 {
		t.Fatalf("premise: a root-owned 0700 discover.sh should stop the operator at exit 126, got %d\n%s", code, out)
	}
	_ = os.Remove(stage + "/discover.sh")

	if user != nobody+":"+nobody {
		t.Fatalf("⛔ the block's helper ran with --user %q; the operator is uid %s, so it must run as %s:%s\n  %s", user, nobody, nobody, nobody, runs[0])
	}
	helper(user)
	if code, out := operatorRuns(); code != 0 || !strings.Contains(out, "discovered") {
		t.Errorf("⛔ with the block's own --user the operator's bash still exits %d:\n%s", code, out)
	}
	if fi, err := os.Stat(stage + "/discover.sh"); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("discover.sh must stay mode 0700 (only its owner changes), got %v (%v)", fi, err)
	}
}
