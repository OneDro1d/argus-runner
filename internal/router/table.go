// Package router is the LOCAL MCP router (M3-FX block A, VR-R1..R13): one endpoint per machine that
// routes by instance_id, so an agent folder can drive many SUTs while holding no real credential.
//
// WHY IT EXISTS. A `.mcp.json` key maps to ONE url, but every instance's runner is a SEPARATE
// endpoint on its own port (measured: compose 8765/8767/8768; k3d+aks 50765/64617/53905). N SUTs in
// one folder therefore need N entries — unless something routes by `instance_id`. A second, unplanned
// benefit: the k3d/aks ports are port-forwards that pick a DIFFERENT port when they re-bind, silently
// stranding every `.mcp.json` naming the old one. A router with a sticky port makes agent config
// immune to that.
//
// ── THE PART THAT MATTERS ────────────────────────────────────────────────────────────────────────
//
// Measured on disk, the SAME instance from two agent folders:
//
//	TEST agent -> localhost:8765/sse   Bearer author-425e158c…
//	PROD agent -> localhost:8765/sse   Bearer runner-fd9e7bdc…
//
// Same endpoint, DIFFERENT tokens. That difference IS the dark-factory holdout: the in-env server
// resolves the presented token to a hat (internal/auth) and redacts `EXPECT` values for the product
// hat. A router that stored "the instance's token" and injected it would silently start feeding
// expected values to the product agent — VR-P7, the highest-risk rule in the build.
//
// So the table maps `router token -> (folder, hat, permitted instances)` and injects the matching
// per-instance, PER-HAT upstream token. And the defusing structure is not a rule the routing code
// follows, it is what the data can express: a PRODUCT folder cannot hold an author-plane credential
// at all (AddFolder refuses one). A routing BUG then produces a failed lookup, not a leak.
package router

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/OneDro1d/argus-runner/internal/role"
)

// RunnerTools are the 6 in-env tools, served by the instance's own executor. Both hats see all six;
// they differ in what comes BACK (the in-env server redacts for the product hat).
var RunnerTools = []string{
	"runner__validate_config",
	"runner__run",
	"runner__get_report",
	"runner__get_sagas",
	"runner__get_tail_logs",
	"runner__get_dashboard_url",
}

// AuthorTools are the cloud tools, served by the control plane. TEST HAT ONLY (VR-R7), and they
// never reach a SUT — which is why they are not in the runner set (VR-C4).
var AuthorTools = []string{
	"author__propose_scenario",
	"author__validate_scenario",
	"author__write_scenario",
	"author__delete_scenario",
	"author__list_scenarios",
	"author__read_scenario",
	"author__request_run",
	"author__get_run_status",
	"author__get_executor_status",
	"author__list_runs",
}

// Upstream is one instance's in-env executor, with the token for THIS folder's hat.
type Upstream struct {
	URL   string
	Token string
}

// (V27-009 redesign) The author plane is a CloudRecord on the STATE, one per (control plane, user); a folder
// carries only a CloudRef to it — see records.go. The per-folder token copy is gone.

// Folder is one agent folder's routing record: which hat it wears, which instances were onboarded
// into it, and the credentials to reach them.
type Folder struct {
	Path      string
	Hat       role.Role
	Token     string // the router token handed to this folder's .mcp.json
	Upstreams map[string]Upstream
	// Cloud is nil for a product folder, ALWAYS, and AddFolder enforces it. This is VR-R4's
	// "structurally incapable": the product hat cannot be handed an author credential by a routing
	// mistake, because its record does not contain one to hand.
	Cloud *CloudRef
	// record is the resolved CloudRecord for Cloud, set by TableFrom at load time (never serialised).
	record *CloudRecord
	// MCPJSONCreated records whether ONBOARDING created this folder's .mcp.json, as opposed to
	// merging its entry into a file the user already had. VR-A5 lets teardown delete the file only
	// in the first case, and that question cannot be answered later by looking — by then the file
	// exists either way. So it is recorded at the moment it is known, in the state, rather than
	// inferred. It is routing-irrelevant: nothing in Route or AddFolder reads it.
	MCPJSONCreated bool
}

// Target is where a call goes and what credential it carries.
type Target struct {
	URL   string
	Token string
	Plane string // "runner" | "author" — for logging; never a routing input
}

// Table is the machine's routing table: router token -> folder.
//
// It is read on every request and REPLACED whenever onboarding rewrites the state file, so the map
// is guarded. The lock is not decoration: without it, an onboarding that adds a folder while agents
// are working is a data race on the one structure that decides which hat sees which tools.
type Table struct {
	mu      sync.RWMutex
	byToken map[string]*Folder
}

func New() *Table { return &Table{byToken: map[string]*Folder{}} }

// Replace swaps in another table's folders, atomically from a caller's point of view: a request
// either resolves against the old set or the new one, never a half-applied mix.
//
// This exists because a folder wired while the router is SERVING was invisible until a restart, and
// the symptom was a fresh onboarding's agent being told "unrecognized router token" — an answer that
// points at the token, which is the one thing that was correct. Measured, not theorised: a folder
// wired against a running router failed initialize with -32001 until the process was restarted.
func (t *Table) Replace(other *Table) {
	other.mu.RLock()
	next := make(map[string]*Folder, len(other.byToken))
	for k, v := range other.byToken {
		next[k] = v
	}
	other.mu.RUnlock()

	t.mu.Lock()
	t.byToken = next
	t.mu.Unlock()
}

// Len reports how many folders are routable. Used by the reload to report what changed without
// printing anything the state file holds.
func (t *Table) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.byToken)
}

// AddFolder registers an agent folder.
//
// It REFUSES a product folder that carries a cloud credential. That refusal is the whole security
// model in one line: everything else here is ordinary routing code, and ordinary routing code
// acquires bugs.
// ⚠ THE WRITE LOCK COVERS THE CHECK AND THE INSERT TOGETHER, and that is the point rather than an
// implementation detail. Unlocked, the duplicate-token guard below could not do its job under
// concurrency at all: every caller observes "not present" before any of them inserts, so two folders
// end up sharing one token — the state this function's own error text calls "unanswerable", because
// a token IS a folder's identity and the scope check keys on it. A racy guard against a collision is
// not a weaker guard; it is no guard.
func (t *Table) AddFolder(f Folder) error {
	if strings.TrimSpace(f.Token) == "" {
		return fmt.Errorf("router: a folder needs a router token")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, dup := t.byToken[f.Token]; dup {
		return fmt.Errorf("router: that router token is already bound to another folder — a token IS a folder's identity, and a collision makes every scope check unanswerable")
	}
	switch f.Hat {
	case role.Product:
		if f.Cloud != nil {
			return fmt.Errorf("router: a product folder must not hold an author-plane credential (VR-R4/VR-P7) — the product hat must be structurally incapable of reaching the author plane, not merely declined")
		}
	case role.Test:
		// a test folder MAY have no cloud credential (a machine onboarded without a control plane);
		// author tools then simply resolve to nothing, which Route reports honestly.
	default:
		return fmt.Errorf("router: unknown hat %q", f.Hat)
	}
	cp := f
	if cp.Upstreams == nil {
		cp.Upstreams = map[string]Upstream{}
	}
	t.byToken[f.Token] = &cp
	return nil
}

// Resolve maps a presented router token to its folder. An unknown token is refused — the router
// serves only folders it was told about.
// ⚠ THE READ LOCK IS LOAD-BEARING, not tidiness. This is the SERVING HOT PATH — proxy.go calls it
// for every request — and Replace writes the same map on every state reload, which is what happens
// when onboarding wires a folder into a RUNNING router. Unlocked, the two raced in the ORDINARY
// flow, and a concurrent map read during a map write does not return stale data in Go: the runtime
// throws "concurrent map read and map write", which is unrecoverable and takes down the machine-wide
// component every agent folder on that host routes through. Reproduced as a hard crash, not merely
// as a detector warning. Replace and Len locked correctly all along; this and AddFolder did not.
func (t *Table) Resolve(token string) (*Folder, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("router: a router token is required")
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	f, ok := t.byToken[token]
	if !ok {
		return nil, fmt.Errorf("router: unrecognized router token")
	}
	return f, nil
}

// VisibleTools is what this folder's `tools/list` returns.
//
// VR-R7: the product agent's list does NOT contain author__* AT ALL. Before the router both hats hit
// one endpoint and differed only in what came back, so a redaction bug leaked expected values. After
// this, the tools are never offered to that caller — the holdout becomes structural rather than
// behavioural.
func (f *Folder) VisibleTools() []string {
	out := append([]string{}, RunnerTools...)
	if f.Hat.CanAccessScenarios() {
		out = append(out, AuthorTools...)
	}
	return out
}

// Instances lists the instances onboarded into THIS folder, sorted. An out-of-folder instance is not
// discoverable here — a refusal that still told the caller what exists would just be a map.
func (f *Folder) Instances() []string {
	out := make([]string, 0, len(f.Upstreams))
	for id := range f.Upstreams {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// CloudCredential is the author-plane token, or "" when this folder has none. Product folders always
// return "".
func (f *Folder) CloudCredential() string {
	if f.Cloud == nil || f.record == nil {
		return ""
	}
	return f.record.Token
}

func known(tool string) bool {
	for _, t := range RunnerTools {
		if t == tool {
			return true
		}
	}
	for _, t := range AuthorTools {
		if t == tool {
			return true
		}
	}
	return false
}

// Route decides where a call goes, in a deliberate order: is the tool one we serve, is the caller
// allowed it, does the instance_id name exactly one instance, is that instance in THIS folder.
//
// Every check happens BEFORE any network call, so an out-of-folder or out-of-scope call never reaches
// the control plane or a SUT at all.
func (f *Folder) Route(tool, instanceID string) (Target, error) {
	if !known(tool) {
		return Target{}, fmt.Errorf("router: unknown tool %q — the router forwards a fixed set, never an arbitrary name", tool)
	}

	// scope BEFORE identity: a product folder asking for an author tool is told about the scope, not
	// about which instances exist.
	isAuthor := strings.HasPrefix(tool, "author__")
	if isAuthor && !f.Hat.CanAccessScenarios() {
		return Target{}, fmt.Errorf("router: %q is not available to this folder — it is registered as a product-agent folder, which carries the runner tools only", tool)
	}

	id := strings.TrimSpace(instanceID)
	if id == "" {
		return Target{}, fmt.Errorf("router: instance_id is required")
	}
	// VR-R13. The plausible agent error is a joined list, and the plausible FAILURE is a half-run
	// reported as success — so this is loud, and it names the fix.
	if strings.ContainsAny(id, ", ;\t\n") {
		return Target{}, fmt.Errorf("router: instance_id takes exactly one instance (got %q); call once per instance", instanceID)
	}

	up, ok := f.Upstreams[id]
	if !ok {
		return Target{}, fmt.Errorf("router: instance %q is not onboarded into this folder (%s)", id, f.Path)
	}

	if isAuthor {
		if f.Cloud == nil {
			return Target{}, fmt.Errorf("router: no control plane is configured for this folder, so the author tools have nowhere to go")
		}
		if f.record == nil {
			return Target{}, fmt.Errorf("router: this folder refers to a control-plane record (%s, user %q) this machine no longer holds — onboard again", f.Cloud.URL, f.Cloud.User)
		}
		if strings.TrimSpace(f.record.Token) == "" {
			return Target{}, fmt.Errorf("router: this machine holds no author token for %s yet — onboarding mints it at step 8b", f.record.URL)
		}
		return Target{URL: f.record.URL, Token: f.record.Token, Plane: "author"}, nil
	}
	// runner__*: the instance's own executor, with THIS FOLDER'S HAT's token. The token is what makes
	// the in-env server redact (or not) — see internal/auth.
	return Target{URL: up.URL, Token: up.Token, Plane: "runner"}, nil
}
