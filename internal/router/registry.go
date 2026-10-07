package router

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/role"
)

// RouterTokenPrefix marks a LOCAL router token — the only credential an agent folder is allowed to
// hold after M3-FX (VR-R6). It is deliberately distinguishable from `odts_` (the cloud author PAT,
// store.AuthorTokenPrefix) at a glance, because "is this folder holding a real credential?" has to
// be answerable by LOOKING at the file. A router token is worthless off this machine: it
// authenticates to 127.0.0.1 and nothing else.
const RouterTokenPrefix = "odtr_"

// routerTokenBytes is 24 bytes of entropy. The token is loopback-only, but it is also the ONLY
// thing separating a product folder from a test folder's tool set, so it is sized as a real secret.
const routerTokenBytes = 24

// MintRouterToken generates a folder's router token.
func MintRouterToken() (string, error) {
	var b [routerTokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("router: could not mint a router token: %w", err)
	}
	return RouterTokenPrefix + hex.EncodeToString(b[:]), nil
}

// FolderSpec is what onboarding knows at the moment it wires one instance into one agent folder:
// where the folder is, which hat it wears, and how to reach that instance's executor AS THAT HAT.
//
// One spec = one (folder, instance) pair. A folder accumulates instances by being upserted again.
type FolderSpec struct {
	Path       string
	Hat        role.Role
	InstanceID string
	Executor   Upstream
	// Cloud (V27-009 redesign) REFERS to a CloudRecord the machine already holds — never a token. Supplying it
	// for a product folder is refused, not ignored.
	Cloud *CloudRef
	// MCPJSONCreated records that onboarding CREATED the folder's .mcp.json (rather than merging
	// into one the user already had). Only meaningful on the call that creates the folder; a later
	// upsert never clears it.
	MCPJSONCreated bool
}

// NormalizeFolderPath returns the cleaned path used to identify and display a folder. A folder is
// identified by this path, so `/w/test` and `/w/test/` must not become two records holding two
// router tokens for one .mcp.json.
//
// A path that is ALREADY ABSOLUTE is never re-based, and that is not a nicety — onboarding runs the
// CLI inside a Linux container while the folders it records live on a Windows host. `filepath.Abs`
// there would turn `C:/agents/test` into `/work/C:/agents/test`: a path no router could ever match,
// written into the one file whose whole job is to match paths. Only a genuinely RELATIVE path is
// resolved, against the caller's own working directory, which is the only case where that is what
// the caller meant.
func NormalizeFolderPath(p string) (string, error) {
	t := strings.TrimSpace(p)
	if t == "" {
		return "", fmt.Errorf("router: a folder needs a path")
	}
	if isWindowsStyle(t) {
		// Clean in slash form (path.Clean is platform-independent), then restore the backslashes a
		// Windows operator will compare against by eye.
		c := path.Clean(strings.ReplaceAll(t, `\`, "/"))
		return strings.ReplaceAll(c, "/", `\`), nil
	}
	if strings.HasPrefix(t, "/") {
		// Absolute POSIX. On Windows filepath.Abs would prepend the current drive, which is what turns
		// a bare `/w/test` into `C:\w\test` — correct for a host-side invocation, so keep doing it
		// there; on POSIX the path already IS absolute.
		if runtime.GOOS == "windows" {
			abs, err := filepath.Abs(t)
			if err != nil {
				return "", fmt.Errorf("router: cannot resolve folder path %q: %w", p, err)
			}
			return filepath.Clean(abs), nil
		}
		return path.Clean(t), nil
	}
	abs, err := filepath.Abs(t)
	if err != nil {
		return "", fmt.Errorf("router: cannot resolve folder path %q: %w", p, err)
	}
	return filepath.Clean(abs), nil
}

// isWindowsStyle reports whether a path names a Windows location — a drive letter, or backslash
// separators. It is deliberately a property of the PATH, not of runtime.GOOS: the process doing the
// normalising is often a Linux container reasoning about a Windows host's folders.
func isWindowsStyle(p string) bool {
	if len(p) >= 2 && p[1] == ':' &&
		((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) {
		return true
	}
	return strings.Contains(p, `\`)
}

// folderKey is the COMPARISON form of a folder path. Case folds for WINDOWS-STYLE paths — again a
// property of the path rather than of the running OS, so a Linux container comparing two spellings
// of `C:\Agents\Test` reaches the same answer the Windows host would.
//
// On POSIX paths it does NOT fold: `/w/Test` and `/w/test` are genuinely different directories, and
// merging them would collapse two agent folders into one routing record.
//
// The STORED path keeps its original casing; only the comparison folds. A lowercased path would be
// shown back to the operator by `router status` and by the cutover runbook's checks, where it would
// no longer match what they typed.
func folderKey(normalized string) string {
	if isWindowsStyle(normalized) || runtime.GOOS == "windows" {
		return strings.ToLower(normalized)
	}
	return normalized
}

// FindFolder locates a folder by path and returns a pointer INTO the state slice plus its index, or
// (nil, -1). The path is normalised first, so callers may pass whatever the operator typed.
func FindFolder(s State, path string) (*Folder, int) {
	norm, err := NormalizeFolderPath(path)
	if err != nil {
		return nil, -1
	}
	want := folderKey(norm)
	for i := range s.Folders {
		n, err := NormalizeFolderPath(s.Folders[i].Path)
		if err != nil {
			continue
		}
		if folderKey(n) == want {
			return &s.Folders[i], i
		}
	}
	return nil, -1
}

// UpsertFolder wires one instance into one agent folder and returns the NEW state.
//
// It is the only way onboarding writes the routing table, and it is idempotent by design: onboarding
// runs repeatedly against the same folders, and the operations it performs — add an instance, replace
// an instance's upstream, re-run unchanged — must all converge rather than accumulate.
//
// THE INVARIANT THAT MATTERS MOST: an existing folder KEEPS its router token. That token is already
// written into the folder's .mcp.json; minting a fresh one on every re-onboard would strand the agent
// on any run where the config-rewrite step did not also complete, and the strand would only surface
// at the agent's next tool call.
func UpsertFolder(s State, spec FolderSpec) (State, Folder, bool, error) {
	norm, err := NormalizeFolderPath(spec.Path)
	if err != nil {
		return s, Folder{}, false, err
	}
	if _, err := role.Parse(string(spec.Hat)); err != nil {
		return s, Folder{}, false, fmt.Errorf("router: %w", err)
	}
	id := strings.TrimSpace(spec.InstanceID)
	if id == "" {
		return s, Folder{}, false, fmt.Errorf("router: a folder entry needs an instance id — an upstream with no id is not addressable by any tool call")
	}
	// VR-R13 is enforced at call time by whole-string equality, but a stored id containing a
	// separator would be permanently unreachable: nothing would ever match it. Refuse it here, where
	// the operator is still watching, rather than at the agent's first tool call.
	if strings.ContainsAny(id, ", \t") {
		return s, Folder{}, false, fmt.Errorf("router: instance id %q contains a separator — one entry names exactly one instance", id)
	}
	if strings.TrimSpace(spec.Executor.URL) == "" {
		return s, Folder{}, false, fmt.Errorf("router: instance %q needs an executor URL — a folder entry that routes nowhere is worse than a missing one, because tools/list still succeeds", id)
	}
	if strings.TrimSpace(spec.Executor.Token) == "" {
		return s, Folder{}, false, fmt.Errorf("router: instance %q needs the per-hat executor token — the router injects it on every forwarded call (VR-R4), and an empty one would reach the executor as an unauthenticated request", id)
	}
	// The same refusal AddFolder makes, made EARLIER: at onboarding, where the operator can act on
	// it, rather than at router start, where the whole machine is already wired.
	if spec.Hat == role.Product && spec.Cloud != nil {
		return s, Folder{}, false, fmt.Errorf("router: refusing to give the product folder %s an author-plane credential (VR-R4/VR-P7) — the product hat must be structurally incapable of reaching the author plane", norm)
	}

	out := State{Port: s.Port, Folders: make([]Folder, len(s.Folders)), Clouds: append([]CloudRecord(nil), s.Clouds...)}
	for i, f := range s.Folders {
		out.Folders[i] = cloneFolder(f)
	}

	existing, idx := FindFolder(out, norm)
	created := existing == nil

	var f Folder
	if created {
		tok, err := MintRouterToken()
		if err != nil {
			return s, Folder{}, false, err
		}
		f = Folder{
			Path:           norm,
			Hat:            spec.Hat,
			Token:          tok,
			Upstreams:      map[string]Upstream{},
			MCPJSONCreated: spec.MCPJSONCreated,
		}
	} else {
		f = *existing
		if f.Hat != spec.Hat {
			return s, Folder{}, false, fmt.Errorf(
				"router: %s is already registered as the %s folder and this call declares it %s — a folder does not change hat. Tear the instance down (or remove the folder) before re-onboarding it under a different hat",
				norm, f.Hat, spec.Hat)
		}
		// Provenance is sticky: a later merge did not create the file, but a previous onboarding may
		// have, and that is what decides whether teardown may DELETE it (VR-A5).
		f.MCPJSONCreated = f.MCPJSONCreated || spec.MCPJSONCreated
	}

	f.Upstreams[id] = spec.Executor
	if spec.Cloud != nil {
		// V27-009 redesign: a folder REFERS to a record the machine already holds (`router register`, step 8a,
		// creates it); a ref to nothing would route the author plane nowhere and say so only at call time.
		if s.RecordFor(spec.Cloud.URL, spec.Cloud.User) == nil {
			return s, Folder{}, false, fmt.Errorf("router: no control-plane record for %s (user %q) on this machine — run `router register` (onboarding step 8a) before wiring the cloud", spec.Cloud.URL, spec.Cloud.User)
		}
		c := *spec.Cloud
		f.Cloud = &c
	}

	if created {
		out.Folders = append(out.Folders, f)
	} else {
		out.Folders[idx] = f
	}

	// Build the table the router would build. This turns "onboarding wrote a state the router cannot
	// load" from a class of bug into an impossibility: the refusal lands on the onboarding run that
	// caused it, not on the next router start.
	if _, err := TableFrom(out); err != nil {
		return s, Folder{}, false, err
	}
	return out, f, created, nil
}

// RemoveResult reports what a removal actually did, so the caller can decide what to do with the
// folder's .mcp.json — and can tell "removed" from "was not there", which teardown must not conflate.
type RemoveResult struct {
	Removed        bool
	FolderDropped  bool
	Path           string
	MCPJSONCreated bool
	// Hat is the hat the removed folder wore, carried out because the caller cannot look it up
	// afterwards — by then the folder record is gone.
	//
	// ⚠ IT HAS NO READER TODAY. Its one consumer was VR8-S1's name-scoped skill purge, which chose
	// what to delete from the hat; the owner deferred that whole topic on 2026-08-21 and teardown
	// now removes no skills at all. The field is kept because the deferred analysis needs exactly
	// this fact and it cannot be recovered later, but it is WRITE-ONLY until then — said plainly
	// rather than left looking load-bearing.
	Hat role.Role
	// Token is the router token this folder's .mcp.json was written with (VR8-S1 / gate F1).
	//
	// It is carried out so the caller can PROVE a file belongs to this folder before deleting
	// anything beside it. The token is a per-folder secret; a caller who can produce it already
	// has router state, so it is a fact about the world rather than a claim about it.
	Token string
}

// RemoveInstance takes one instance out of one folder. When it was the folder's LAST instance the
// folder record goes too: an empty folder would keep authenticating its router token while routing
// nothing, which presents to the agent as a broken router rather than a torn-down instance.
//
// An unknown folder and an unknown instance are both NORMAL — teardown runs more than once, and it
// runs on machines where the instance was never wired. Neither is an error.
func RemoveInstance(s State, path, instanceID string) (State, RemoveResult, error) {
	norm, err := NormalizeFolderPath(path)
	if err != nil {
		return s, RemoveResult{}, err
	}
	out := State{Port: s.Port, Folders: make([]Folder, len(s.Folders)), Clouds: append([]CloudRecord(nil), s.Clouds...)}
	for i, f := range s.Folders {
		out.Folders[i] = cloneFolder(f)
	}
	f, idx := FindFolder(out, norm)
	if f == nil {
		return out, RemoveResult{Path: norm}, nil
	}
	id := strings.TrimSpace(instanceID)
	if _, ok := f.Upstreams[id]; !ok {
		return out, RemoveResult{Path: f.Path, MCPJSONCreated: f.MCPJSONCreated, Hat: f.Hat, Token: f.Token}, nil
	}
	res := RemoveResult{Removed: true, Path: f.Path, MCPJSONCreated: f.MCPJSONCreated, Hat: f.Hat, Token: f.Token}
	delete(f.Upstreams, id)
	if len(f.Upstreams) == 0 {
		out.Folders = append(out.Folders[:idx], out.Folders[idx+1:]...)
		res.FolderDropped = true
	}
	return out, res, nil
}

// RemoveFolder drops a whole folder record — every instance it routed and its router token.
func RemoveFolder(s State, path string) (State, bool, error) {
	norm, err := NormalizeFolderPath(path)
	if err != nil {
		return s, false, err
	}
	out := State{Port: s.Port, Folders: make([]Folder, len(s.Folders)), Clouds: append([]CloudRecord(nil), s.Clouds...)}
	for i, f := range s.Folders {
		out.Folders[i] = cloneFolder(f)
	}
	if _, idx := FindFolder(out, norm); idx >= 0 {
		out.Folders = append(out.Folders[:idx], out.Folders[idx+1:]...)
		return out, true, nil
	}
	return out, false, nil
}

// cloneFolder deep-copies the parts a caller could otherwise mutate through the returned state —
// the upstream map and the cloud pointer. Without this, UpsertFolder's "returns a new state" promise
// would be false for exactly the fields that carry credentials.
func cloneFolder(f Folder) Folder {
	cp := f
	cp.Upstreams = make(map[string]Upstream, len(f.Upstreams))
	for k, v := range f.Upstreams {
		cp.Upstreams[k] = v
	}
	if f.Cloud != nil {
		c := *f.Cloud
		cp.Cloud = &c
	}
	cp.record = nil // resolved again by TableFrom; a clone never carries a pointer into the old state
	return cp
}
