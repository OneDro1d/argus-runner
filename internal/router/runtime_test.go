package router

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
)

func isWindows() bool { return runtime.GOOS == "windows" }

// ── VR-R3: the sticky port ──────────────────────────────────────────────────────────────────────
//
// Every agent folder on the machine names ONE port. A port that changes on restart strands all of
// them at once — which is exactly the failure the k3d/aks port-forwards produce today, and one of the
// reasons the router exists.

func TestPickPort_PrefersThePortItHeldLastTime(t *testing.T) {
	free := func(p int) bool { return true } // everything free
	got, err := PickPort(9771, free)
	if err != nil {
		t.Fatal(err)
	}
	if got != 9771 {
		t.Errorf("PickPort(9771) = %d; a restart must reclaim the port every .mcp.json already names", got)
	}
}

func TestPickPort_FallsBackWhenThePreviousPortIsTaken(t *testing.T) {
	free := func(p int) bool { return p != 9771 && p != PreferredPort }
	got, err := PickPort(9771, free)
	if err != nil {
		t.Fatal(err)
	}
	if got == 9771 || got == PreferredPort {
		t.Fatalf("PickPort returned an unavailable port: %d", got)
	}
	if got < PreferredPort || got > FallbackHigh {
		t.Errorf("PickPort = %d, outside the fallback range", got)
	}
}

func TestPickPort_FirstRunTakesThePreferredPort(t *testing.T) {
	got, err := PickPort(0, func(int) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if got != PreferredPort {
		t.Errorf("first run = %d, want the preferred %d", got, PreferredPort)
	}
}

// The preferred port must not sit inside the range onboarding scans for per-instance executors, or
// the router could take a port one of its own upstreams needs.
func TestPreferredPort_IsOutsideTheExecutorRange(t *testing.T) {
	if PreferredPort >= 8765 && PreferredPort <= 8790 {
		t.Errorf("PreferredPort %d is inside the 8765-8790 executor scan range", PreferredPort)
	}
}

func TestPickPort_RefusesWhenTheWholeRangeIsTaken(t *testing.T) {
	if _, err := PickPort(0, func(int) bool { return false }); err == nil {
		t.Fatal("PickPort must refuse rather than return a port it could not bind")
	}
}

// VR-R2: loopback only. The listener address is asserted because "bind 127.0.0.1" is the single
// sentence standing between this endpoint — which holds every folder's route to every SUT — and the
// rest of the network.
func TestListenAddr_IsLoopbackOnly(t *testing.T) {
	got := ListenAddr(9765)
	if !strings.HasPrefix(got, "127.0.0.1:") {
		t.Errorf("ListenAddr = %q, want a 127.0.0.1 bind", got)
	}
	if strings.Contains(got, "0.0.0.0") || strings.HasPrefix(got, ":") {
		t.Errorf("ListenAddr = %q — that binds every interface", got)
	}
}

// ── state ───────────────────────────────────────────────────────────────────────────────────────

func TestState_RoundTripsThroughDisk(t *testing.T) {
	dir := t.TempDir()
	in := State{Port: 9765, Folders: []Folder{{
		Path: "C:/work/test-agent", Hat: role.Test, Token: testTok,
		Upstreams: map[string]Upstream{"suta": {URL: "http://127.0.0.1:8765", Token: "author-suta"}},
		Cloud:     &CloudRef{URL: "https://argus-dev.onedroid.ai/mcp", User: "u"},
	}}, Clouds: []CloudRecord{{URL: "https://argus-dev.onedroid.ai/mcp", User: "u", Token: "odts_x"}}}
	if err := SaveState(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out.Port != 9765 || len(out.Folders) != 1 {
		t.Fatalf("round-trip lost data: %+v", out)
	}
	tbl, err := TableFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	f, err := tbl.Resolve(testTok)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := f.Route("runner__run", "suta"); got.Token != "author-suta" {
		t.Errorf("upstream token did not survive the round trip: %+v", got)
	}
}

// A machine that has never onboarded has no state, and that is not an error.
func TestLoadState_MissingIsNotAnError(t *testing.T) {
	s, err := LoadState(t.TempDir())
	if err != nil {
		t.Fatalf("a missing state file must not be an error: %v", err)
	}
	if s.Port != 0 || len(s.Folders) != 0 {
		t.Errorf("want a zero state, got %+v", s)
	}
}

// A CORRUPT state file IS an error. Starting with an empty routing table would present agents with a
// router that authenticates nobody and explains nothing — absence reported as health.
func TestLoadState_CorruptIsAnErrorNotAnEmptyTable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadState(dir)
	if err == nil {
		t.Fatal("a corrupt state file was silently treated as an empty routing table")
	}
	if !strings.Contains(err.Error(), "strand") {
		t.Errorf("the error should say what silently continuing would cost; got %v", err)
	}
}

// VR-R4 must hold across a RESTART, not only at onboarding: a state file that somehow records a
// product folder holding a cloud credential is refused on load, not honoured.
func TestTableFrom_RefusesAProductFolderWithACloudCredential(t *testing.T) {
	bad := State{Folders: []Folder{{
		Path: "C:/work/product-agent", Hat: role.Product, Token: prodTok,
		Upstreams: map[string]Upstream{"suta": {URL: "http://127.0.0.1:8765", Token: "runner-suta"}},
		Cloud:     &CloudRef{URL: "https://argus-dev.onedroid.ai/mcp", User: "u"},
	}}}
	if _, err := TableFrom(bad); err == nil {
		t.Fatal("a product folder with a cloud credential was loaded from state — VR-R4 does not survive a restart")
	}
}

// VR-F27: durable state must NOT live under C:\tmp — the owner wipes that deliberately between
// from-scratch cycles, and a router that lost its folder table there would strand every agent while
// still looking healthy.
func TestStateDir_IsNotUnderTmp(t *testing.T) {
	d := strings.ToLower(filepath.ToSlash(StateDir()))
	for _, bad := range []string{"/tmp/", "c:/tmp", "/c/tmp"} {
		if strings.Contains(d, bad) {
			t.Errorf("StateDir() = %q — that is a reset surface, not durable storage", StateDir())
		}
	}
	if d == "" {
		t.Error("StateDir() is empty")
	}
}

// VR-R12: the state file holds upstream bearer tokens, so what the router PRINTS must not.
func TestRedacted_CarriesNoCredentials(t *testing.T) {
	s := State{Port: 9765, Folders: []Folder{{
		Path: "C:/work/test-agent", Hat: role.Test, Token: "odtr_super_secret_router_token",
		Upstreams: map[string]Upstream{"suta": {URL: "http://127.0.0.1:8765", Token: "author-suta-secret"}},
		Cloud:     &CloudRef{URL: "https://argus-dev.onedroid.ai/mcp", User: "u"},
	}}, Clouds: []CloudRecord{{URL: "https://argus-dev.onedroid.ai/mcp", User: "u", Token: "odts_user_global_secret"}}}
	out := Redacted(s)
	for _, secret := range []string{"odtr_super_secret_router_token", "author-suta-secret", "odts_user_global_secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("Redacted() leaked %q:\n%s", secret, out)
		}
	}
	// …while still being useful: it must say enough to diagnose a routing problem.
	for _, want := range []string{"port=9765", "test-agent", "suta"} {
		if !strings.Contains(out, want) {
			t.Errorf("Redacted() omitted %q, which makes it useless for diagnosis:\n%s", want, out)
		}
	}
}

func TestSaveState_FileModeIsOwnerOnly(t *testing.T) {
	if isWindows() {
		t.Skip("windows: POSIX mode bits are not enforced; the directory ACL protects the tokens (see SaveState)")
	}
	dir := t.TempDir()
	if err := SaveState(dir, State{Port: 9765}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("state.json mode = %o; it holds bearer tokens", mode)
	}
}
