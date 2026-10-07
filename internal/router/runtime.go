package router

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/mcp"
)

// The router's runtime: how it reaches upstreams, which port it binds, and where its state lives.

// ── the real forwarder ───────────────────────────────────────────────────────────────────────────

// MCPForwarder forwards a routed call over the existing MCP client, which already speaks both
// transports (Streamable HTTP and legacy SSE) and is declared, never guessed.
type MCPForwarder struct {
	// Transport is how upstreams are addressed. Both planes the router talks to are ordinary MCP
	// servers: the in-env executor and the control plane both serve /mcp.
	Transport mcp.Transport
	// HostAlias reconciles the vantage the routing table was WRITTEN from (the host's) with the one
	// the router is READING it from. Empty for a host process; set to `host.docker.internal` for the
	// VR-R1 container, where an unrewritten `localhost` would loop every call back into the router.
	// See vantage.go for why this is declared rather than detected.
	HostAlias string
}

// Forward performs one tools/call against the resolved target.
//
// It maps the client's THREE distinct failure kinds onto errors that keep them distinct, because
// collapsing them is the defect this build exists to remove:
//   - Unreachable / TransportErr -> could not ASK (an error here; the proxy reports reached:false)
//   - JSONRPCError               -> the protocol refused the call (an error, with its message)
//   - IsError                    -> the TOOL answered, and its answer was a failure. NOT an error
//     here: the upstream was reached and gave a real answer, so it is
//     passed through as the payload for the caller to read.
func (f MCPForwarder) Forward(t Target, tool string, args json.RawMessage) (any, bool, error) {
	tr := f.Transport
	if tr == "" {
		tr = mcp.Streamable
	}
	// The rewrite happens HERE, at the last moment, and never in the stored state. The state file is
	// the host's record of where things are; a router that rewrote it on load would hand the next
	// reader — an operator running `router status`, or a host-process router started later against
	// the same state — a URL that is only true for a container.
	c := &mcp.Client{ServerURL: ResolveFromVantage(t.URL, f.HostAlias), Transport: tr, Token: t.Token}

	var parsed any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsed); err != nil {
			return nil, false, fmt.Errorf("arguments are not valid JSON: %w", err)
		}
	}
	parsed = localiseInstanceID(parsed, t.Plane)
	res := c.Call(mcp.CallInput{Tool: CloudToolName(tool, t.Plane), Args: parsed})

	if res.Unreachable {
		return nil, false, fmt.Errorf("endpoint not reachable")
	}
	if res.TransportErr != "" {
		return nil, false, fmt.Errorf("transport: %s", res.TransportErr)
	}
	if res.JSONRPCError != nil {
		// The upstream ANSWERED, and its answer was "no". That is not the same as being
		// unreachable, and the proxy must not report it as one — the same distinction
		// this codebase preserves everywhere, applied one layer up.
		//
		// It matters concretely now that runner__run is forwarded synchronously: a busy executor
		// replies -32002 "run already in progress on this instance", and rendering that as
		// "could not reach the runner plane" would send someone to check the network for a
		// perfectly healthy instance that simply said not yet.
		return nil, false, &UpstreamRefusal{Code: res.JSONRPCError.Code, Message: res.JSONRPCError.Message}
	}
	// VR2-13 (V15-007) — ONE ENVELOPE, and this REVERSES the decision recorded here before.
	//
	// It used to `return json.RawMessage(res.Raw)` — the upstream's COMPLETE JSON-RPC envelope — on
	// the reasoning that "the router is a router, and re-shaping an upstream's payload here would
	// create a second contract to drift from the first". That reasoning is sound about SHAPE and
	// wrong about NESTING: the proxy hands whatever it gets to mcpserver.Ok, which marshals it into
	// the text of a content block. Returning an envelope therefore produced an envelope INSIDE an
	// envelope, and every caller had to unwrap twice.
	//
	// Measured 2026-08-12: exactly 2 JSON-RPC envelopes deep on both compose and k3d — systemic, not
	// a one-off. Owner ruling the same day: "there should not be envelope inside envelope, it should
	// be wrapped once."
	//
	// So the router hands back the upstream's OWN payload and lets the proxy wrap it once. The
	// upstream's result is {content:[{type:"text",text:"<the real JSON>"}], isError:bool}; the useful
	// value is that inner text. It is returned PARSED when it is JSON, so the caller receives an
	// object rather than a string containing an object — one wrap, one decode.
	//
	// isError travels alongside it rather than being flattened away. Dropping it would be a real
	// regression: a tool that ANSWERED "failed" would arrive looking like a success, which is the
	// class of silent-wrong-answer this codebase exists to remove.
	return unwrapUpstream(res), res.IsError, nil
}

// unwrapUpstream turns an upstream CallResult into the value the proxy should wrap ONCE.
//
// It is deliberately total: every branch returns something a caller can read, because a router that
// answered nothing is indistinguishable from one that could not be reached, and those are the two
// states this codebase keeps apart everywhere else.
func unwrapUpstream(res mcp.CallResult) any {
	// The overwhelmingly common shape: a single text block carrying JSON.
	if len(res.Content) == 1 && res.Content[0].Type == "text" {
		var v any
		if err := json.Unmarshal([]byte(res.Content[0].Text), &v); err == nil {
			return v
		}
		// Not JSON — a tool is entitled to answer in prose. Hand back the text itself.
		return res.Content[0].Text
	}
	// Several blocks, or a non-text block: hand back the content array unchanged rather than
	// inventing a merge. Still ONE envelope; the caller sees exactly what the upstream sent.
	if len(res.Content) > 0 {
		return res.Content
	}
	// A result with no content at all. Returning the raw envelope here would reintroduce the very
	// nesting this change removes, so report the emptiness as itself.
	return map[string]any{"content": []any{}}
}

// UpstreamRefusal is an upstream that was REACHED and said no. It carries the upstream's own
// JSON-RPC code so the refusal survives the hop with its meaning intact rather than being flattened
// into "something went wrong" — a caller that sees -32002 knows to retry later, and one that sees a
// scope error knows not to bother.
type UpstreamRefusal struct {
	Code    int
	Message string
}

func (e *UpstreamRefusal) Error() string { return e.Message }

// CloudToolName maps an agent-facing tool name to the name the CLOUD plane publishes (VR-C5).
//
// The agent keeps seeing `author__request_run` — that is what its folder has always listed, and
// changing it would rewrite every skill and every doc for a cosmetic reason. The CLOUD publishes the
// BARE `author_request_run`, because a gateway builds its displayed name from `__`-joined segments:
//
//	mcp__acme-atlassian__onedroid_argus__author_request_run
//
// A tool carrying `__` inside its own name splits that display into segments that no longer parse,
// exactly where a user looks to tell which SUT they are addressing.
//
// Runner tools are untouched: they never reach a gateway.
func CloudToolName(tool, plane string) string {
	if plane != "author" {
		return tool
	}
	return strings.Replace(tool, "author__", "author_", 1)
}

// InEnvInstanceID is the instance id the IN-ENV executor answers to. It is `local` by contract
// (CLAUDE.md: "the in-env runner__* tools take `local`; the cloud author__* tools take your
// registered instance id"), and the executor enforces it:
//
//	{"error":"unknown instance_id \"orderservice-compose\" — this server serves instance \"local\" only"}
const InEnvInstanceID = "local"

// localiseInstanceID rewrites `instance_id` to `local` on the RUNNER plane, and leaves the author
// plane untouched.
//
// THE JOIN THIS FIXES, measured against the live estate: the router ROUTES by the registered
// instance id — that is how a folder reaches three SUTs through one endpoint — but the in-env
// executor it forwards to serves exactly one instance and calls it `local`. Passing the id through
// unchanged made every runner call fail with "unknown instance_id", while tools/list, the scope
// checks and the transport all looked perfect.
//
// It survived the unit tests because they forward through a STUB that echoes whatever arguments it
// is given: the assertion was "the right target was chosen", never "the payload the executor
// receives is one it can answer". Only a real executor could say otherwise, and it did.
//
// The author plane keeps the real id — the control plane resolves tenancy and the run ledger from
// it, so rewriting there would address the wrong instance entirely.
func localiseInstanceID(parsed any, plane string) any {
	if plane != "runner" {
		return parsed
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		return parsed
	}
	if _, present := m["instance_id"]; !present {
		return parsed
	}
	// Copy: the caller's map may be shared, and a router that mutated its input would make the same
	// call behave differently the second time.
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	out["instance_id"] = InEnvInstanceID
	return out
}

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
