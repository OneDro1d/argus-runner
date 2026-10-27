package router

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The router's runtime: how it reaches upstreams, which port it binds, and where its state lives.

// ── the in-env instance id ────────────────────────────────────────────────────────────────────────

// InEnvInstanceID is the instance id the IN-ENV executor answers to. It is `local` by contract
// (the in-env runner tools take `local`; the cloud author tools take the registered instance id),
// and the executor enforces it:
//
//	{"error":"unknown instance_id \"orderservice-compose\" — this server serves instance \"local\" only"}
//
// THE JOIN THIS SERVES, measured against the live estate: the router ROUTES by the registered instance id
// — that is how a folder reaches three SUTs through one endpoint — but the in-env executor it forwards to
// serves exactly one instance and calls it `local`. So a runner call's instance_id is rewritten to `local`
// on the way out (localiseRaw, runner.go), and an author call keeps the real id: the control plane resolves
// tenancy and the run ledger from it. (Before AC-D70 the forwarder did this as localiseInstanceID.)
const InEnvInstanceID = "local"

// ── the sticky port (VR-R3) ──────────────────────────────────────────────────────────────────────

const (
	// PreferredPort is deliberately OUTSIDE the 8765-8790 range onboarding scans for per-instance
	// executor ports, so the router can never collide with the very things it routes to.
	PreferredPort = 9765
	// FallbackHigh bounds the small fallback range 9766..9775.
	FallbackHigh = 9775
)

// PickPort returns the port to bind, preferring the one recorded last time.
//
// The pattern is proven by connect.sh, whose comment states the reason exactly: "prefer the port this
// instance used last time, so a restart keeps every .mcp.json valid." For the router the stake is
// higher — every agent folder on the machine names this one port, so a port change on restart would
// strand all of them at once.
func PickPort(previous int, free func(int) bool) (int, error) {
	if free == nil {
		free = portIsFree
	}
	if previous > 0 && free(previous) {
		return previous, nil
	}
	if free(PreferredPort) {
		return PreferredPort, nil
	}
	for p := PreferredPort + 1; p <= FallbackHigh; p++ {
		if free(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("router: no free port in %d-%d — something else on this machine is holding the router's range", PreferredPort, FallbackHigh)
}

// PortForWiring returns the port ONBOARDING should write into an agent folder's .mcp.json.
//
// It must NOT probe a recorded port, and that is the whole reason it exists rather than onboarding
// calling PickPort. PickPort asks "can I bind this?", which is the right question for the process
// about to bind — and exactly the wrong one here: when the router is already RUNNING, its port is by
// definition unbindable, so PickPort would answer "9766" and onboarding would write a .mcp.json
// pointing at a port nothing serves. The bug would look like a broken router.
//
// A recorded port is therefore taken verbatim. Only a machine with no port yet chooses one, and it
// reserves the preferred port so the first `router serve` reclaims it.
func PortForWiring(previous int) (int, error) {
	if previous > 0 {
		return previous, nil
	}
	return PickPort(0, nil)
}

// portIsFree probes the LOOPBACK bind specifically (VR-R2). Binding 0.0.0.0 would answer a different
// question than the one the router asks.
func portIsFree(p int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// ListenAddr is the address the router binds. 127.0.0.1 ONLY (VR-R2) — never 0.0.0.0, because this
// endpoint holds every agent folder's route to every SUT on the machine.
//
// MEASURED 2026-08-08 on Rancher Desktop / WSL2 (engine 29.5.3-rd): a container's 127.0.0.1 publish
// IS reachable from the Windows host, contradicting the 2026-07-23 note that drove obs-shared to
// 0.0.0.0. Onboarding's preflight must CHECK this rather than assume it — the behaviour belongs to
// the tooling, not to us, and a machine on the older behaviour must be told rather than left with a
// silently unreachable router.
func ListenAddr(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// BindEnv lets the deployment declare a different bind address. It exists for exactly one case, and
// the case is not a relaxation of VR-R2 — it is how VR-R2 is achieved inside a container.
const BindEnv = "ARGUS_ROUTER_BIND"

// BindAddr is the address to LISTEN on, which is not always the address clients use.
//
// MEASURED 2026-08-09, and it cost a working-looking deployment: a container binding `127.0.0.1`
// listens on the CONTAINER's loopback. Docker publishes to the container's eth0 address, so nothing
// answers there and every request from the host is reset — while `docker ps` says `(healthy)`,
// because the health check ran inside the container and probed the one address that did work.
//
// So a containerised router binds `0.0.0.0` and the LOOPBACK RESTRICTION MOVES TO THE PUBLISH:
// `ports: ["127.0.0.1:9765:9765"]`. The property VR-R2 asks for — only this machine can reach the
// router — is preserved exactly, by the layer that can actually enforce it. Binding `0.0.0.0` on a
// HOST process would genuinely violate it, which is why the default here is loopback and the
// override has to be declared.
func BindAddr(port int, bind string) string {
	b := strings.TrimSpace(bind)
	if b == "" {
		return ListenAddr(port)
	}
	return net.JoinHostPort(b, fmt.Sprint(port))
}

// ── state (VR-R12, VR-F27) ───────────────────────────────────────────────────────────────────────

// State is what the router must survive a restart with: the port it holds, and the folders it routes
// for. It is written 0600 and never logged.
type State struct {
	Port    int      `json:"port"`
	Folders []Folder `json:"folders"`
	// Clouds (V27-009 redesign): ONE record per (control plane, user account) — the only place the author token
	// minted during onboarding lives on this machine. Folders refer to a record; they never carry a token.
	// Copied by every constructor in registry.go (the omission of the old machine-level field by those three
	// constructors was V27-009's established cause) and pinned by a test.
	Clouds []CloudRecord `json:"clouds,omitempty"`
}

// StateDir is where the router keeps its state. DURABLE, and deliberately NOT under C:\tmp: the owner
// wipes that directory intentionally between from-scratch cycles (VR-F27), and a router that lost its
// folder table there would strand every agent on the machine while looking healthy.
func StateDir() string {
	// ARGUS_ROUTER_STATE is the ONE knob both halves honour. Onboarding must name the same
	// directory the router will later read, and it computes that name in bash while the router
	// computes it in Go — two implementations of one rule, which is a drift waiting to happen. An
	// explicit override collapses them: set it once, and neither side is guessing.
	if dir := os.Getenv("ARGUS_ROUTER_STATE"); dir != "" {
		return dir
	}
	if runtime.GOOS == "windows" {
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return filepath.Join(base, "argus", "router")
		}
	}
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "argus", "router")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "argus", "router")
	}
	return filepath.Join(".", ".argus-router")
}

const stateMode = 0o600 // VR-R12: the file holds upstream bearer tokens

// SaveState writes the state atomically with owner-only permissions.
//
// On Windows the 0600 is a REQUEST, not a guarantee — there are no POSIX mode bits and os.Chmod only
// toggles read-only, so the directory ACL is what actually protects the tokens. Recorded here rather
// than assumed, for the same reason internal/agentcfg records it: a protection nobody verified is
// not a protection.
func SaveState(dir string, s State) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "state.json.tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, stateMode); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, "state.json"))
}

// LoadState reads the state. A MISSING file is not an error — a machine that has never onboarded has
// no router state — but a CORRUPT one is: silently starting with an empty folder table would present
// an agent with a router that authenticates nobody and explains nothing.
func LoadState(dir string) (State, error) {
	blob, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if os.IsNotExist(err) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	// V27-009 redesign: a 0.3.28 state carries the token on each test folder's "Cloud" object and (never
	// surviving) at the top level. Read those shapes once and lift them into Clouds; a 0.3.29 state has neither.
	var legacy struct {
		Cloud   *legacyCloud `json:"cloud"`
		Folders []struct {
			Cloud *struct {
				URL   string `json:"URL"`
				Token string `json:"Token"`
			} `json:"Cloud"`
		} `json:"folders"`
	}
	_ = json.Unmarshal(blob, &legacy)
	legacyByFolder := map[int]legacyCloud{}
	for i, f := range legacy.Folders {
		if f.Cloud != nil && f.Cloud.Token != "" {
			legacyByFolder[i] = legacyCloud{URL: f.Cloud.URL, Token: f.Cloud.Token}
		}
	}
	if err := json.Unmarshal(blob, &s); err != nil {
		return State{}, fmt.Errorf("router: state.json is unreadable (%w) — fix or delete it; starting with an empty routing table would silently strand every agent folder on this machine", err)
	}
	if len(s.Clouds) == 0 && len(legacyByFolder) > 0 {
		routerID := ""
		if ident, ierr := LoadIdentity(dir); ierr == nil {
			routerID = ident.RouterID
		}
		if legacy.Cloud != nil && legacy.Cloud.Token != "" {
			s.Clouds = append(s.Clouds, CloudRecord{URL: legacy.Cloud.URL, Token: legacy.Cloud.Token, RouterID: routerID})
		}
		if n := liftLegacyClouds(&s, legacyByFolder, routerID); n > 0 || legacy.Cloud != nil {
			// persisted so the next read is already the new shape; a failure to persist is not fatal to this read
			_ = SaveState(dir, s)
		}
	}
	return s, nil
}

// TableFrom rebuilds the routing table from saved state, applying the SAME construction rules — so a
// state file that somehow recorded a product folder holding a cloud credential is refused on load,
// not honoured. VR-R4 must hold across a restart, not only at onboarding.
func TableFrom(s State) (*Table, error) {
	t := New()
	for _, f := range s.Folders {
		if f.Cloud != nil {
			f.record = s.RecordFor(f.Cloud.URL, f.Cloud.User) // nil when the record is gone: Route says so
		}
		if err := t.AddFolder(f); err != nil {
			return nil, fmt.Errorf("router: refusing to load state: %w", err)
		}
	}
	return t, nil
}

// Redacted renders the state for logs and diagnostics with every credential removed (VR-R12).
// The router prints this; it never prints State.
func Redacted(s State) string {
	var b strings.Builder
	fmt.Fprintf(&b, "port=%d folders=%d", s.Port, len(s.Folders))
	for _, f := range s.Folders {
		fmt.Fprintf(&b, "\n  %-8s %s  instances=%v", f.Hat, f.Path, folderIDs(f))
		if f.Cloud != nil {
			fmt.Fprintf(&b, "  cloud=%s user=%s", f.Cloud.URL, f.Cloud.User)
		}
	}
	return b.String()
}

func folderIDs(f Folder) []string {
	out := make([]string, 0, len(f.Upstreams))
	for id := range f.Upstreams {
		out = append(out, id)
	}
	return out
}

// ReadCloudAuthorToken returns the account's author token as this MACHINE holds it, or "" if it holds
// none (VR3-08/VR3-11).
//
// This exists so onboarding stops keeping its own copy. Before V16 every kit had
// `deploy/compose/cp-author.token`: the account has ONE token and the machine kept N copies, each
// free to go stale alone. Measured 2026-08-12 on a seven-kit estate — five held August-7 tokens dead
// for days, and nothing noticed, because nothing ever read one to find out. The specification had
// already named the single holder and the implementation had not followed it: M3-FX PO VR-B4 ("the
// local router, its cloud entry in router state"), M3-FX SA:463 ("which is its only holder").
//
// "" IS NOT AN ERROR. A machine that has never onboarded, or holds only product folders, genuinely
// has no author credential — that is STATE NONE and the caller mints. Distinguishing it from a
// FAILURE to read matters: an error swallowed into "" would mint a second token for an account that
// already has one, which is the VR-B3 conflict that started this round.
//
// THE HAT CHECK IS NOT DECORATION. Folder.Cloud is nil for a product folder ALWAYS, enforced at
// AddFolder — VR-R4's "structurally incapable". This does not lean on that alone: state.json is a
// file a person can edit, and a hand-written cloud entry on a product folder is REFUSED rather than
// read. It is reported rather than skipped, because skipping reads as "this machine has no token"
// and quietly mints another. The holdout (VR-C8/VR-P7) is the one boundary where a second,
// independent check earns its keep.
//
// The token is returned to the caller in-process and never logged (VR-R12).
// (V27-009 redesign) HolderCount, TestFolderCount, WriteCloudAuthorToken, ReadCloudAuthorToken, ApplyRotatedAuthorToken
// and ApplyReplacementAuthorToken are gone: the token has one place (records.go — RecordFor, PutRecord,
// RemoveRecord, FoldersUsing, Holds, SetRecordToken, ReadRecordToken).
