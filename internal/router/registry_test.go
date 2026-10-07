package router

import (
	"net"
	"runtime"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
)

func spec(path string, hat role.Role, instance string) FolderSpec {
	return FolderSpec{
		Path:       path,
		Hat:        hat,
		InstanceID: instance,
		Executor:   Upstream{URL: "http://localhost:8765/sse", Token: "exec-" + instance},
	}
}

func TestUpsertFolder_MintsARouterTokenWithItsOwnPrefix(t *testing.T) {
	st, f, created, err := UpsertFolder(State{}, spec("/w/product", role.Product, "suta"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !created {
		t.Fatal("a folder that did not exist must report created")
	}
	if !strings.HasPrefix(f.Token, RouterTokenPrefix) {
		t.Fatalf("router token %q must carry the router prefix %q", f.Token, RouterTokenPrefix)
	}
	// The prefix must not be the AUTHOR PAT prefix. These two strings sit next to each other in
	// .mcp.json files and in support conversations; if a router token were mistakable for a cloud
	// credential, "is this folder holding a real credential?" (VR-R6) stops being answerable by
	// looking.
	if strings.HasPrefix(f.Token, "odts_") {
		t.Fatalf("router token %q must not look like an author PAT", f.Token)
	}
	if len(st.Folders) != 1 {
		t.Fatalf("want 1 folder, got %d", len(st.Folders))
	}
}

func TestUpsertFolder_TokensAreUniquePerFolder(t *testing.T) {
	st, a, _, _ := UpsertFolder(State{}, spec("/w/product", role.Product, "suta"))
	_, b, _, err := UpsertFolder(st, spec("/w/test", role.Test, "suta"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if a.Token == b.Token {
		t.Fatal("two folders sharing a router token makes every scope check unanswerable")
	}
}

// THE REGRESSION THIS FUNCTION EXISTS FOR. Re-onboarding an instance must not change the folder's
// router token: the token is what the folder's .mcp.json already carries, and rotating it on every
// re-onboard would strand the agent whenever the shell's rewrite step did not run — silently, and
// only at the agent's next tool call.
func TestUpsertFolder_ReOnboardKeepsTheSameRouterToken(t *testing.T) {
	st, first, _, _ := UpsertFolder(State{}, spec("/w/test", role.Test, "suta"))
	st, second, created, err := UpsertFolder(st, spec("/w/test", role.Test, "suta"))
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if created {
		t.Fatal("a folder that already existed must not report created")
	}
	if second.Token != first.Token {
		t.Fatalf("router token changed on re-onboard: %q -> %q", first.Token, second.Token)
	}
	if len(st.Folders) != 1 {
		t.Fatalf("re-onboard duplicated the folder: %d records", len(st.Folders))
	}
}

// The router-side mirror of the .mcp.json defect that started block A: a second instance onboarded
// into a folder must ACCUMULATE, never replace.
func TestUpsertFolder_SecondInstanceJoinsTheFolder(t *testing.T) {
	st, _, _, _ := UpsertFolder(State{}, spec("/w/test", role.Test, "suta"))
	st, f, _, err := UpsertFolder(st, spec("/w/test", role.Test, "sutb"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if len(f.Upstreams) != 2 {
		t.Fatalf("want both instances routable from this folder, got %v", folderIDs(f))
	}
	if f.Upstreams["suta"].Token != "exec-suta" || f.Upstreams["sutb"].Token != "exec-sutb" {
		t.Fatal("each instance must keep its OWN per-hat upstream token")
	}
}

func TestUpsertFolder_ReOnboardReplacesThatInstancesUpstream(t *testing.T) {
	st, _, _, _ := UpsertFolder(State{}, spec("/w/test", role.Test, "suta"))
	s2 := spec("/w/test", role.Test, "suta")
	s2.Executor = Upstream{URL: "http://localhost:9999/sse", Token: "rotated"}
	st, f, _, _ := UpsertFolder(st, s2)
	if got := f.Upstreams["suta"]; got.URL != "http://localhost:9999/sse" || got.Token != "rotated" {
		t.Fatalf("re-onboard must replace the instance's upstream, got %+v", got)
	}
	if len(st.Folders[0].Upstreams) != 1 {
		t.Fatal("re-onboarding the same instance must not duplicate it")
	}
}

// A path that changes hat is refused rather than migrated. onboard.sh already refuses a product dir
// equal to or nested under the scenarios dir (onboard.sh:933-935), so reaching this state means
// something is wrong; silently re-hatting a folder would hand whichever agent is already running
// there a different tool set than the one it started with.
func TestUpsertFolder_ChangingAFoldersHatIsRefused(t *testing.T) {
	st, _, _, _ := UpsertFolder(State{}, spec("/w/f", role.Product, "suta"))
	_, _, _, err := UpsertFolder(st, spec("/w/f", role.Test, "suta"))
	if err == nil {
		t.Fatal("changing a folder's hat must be refused")
	}
	if !strings.Contains(err.Error(), "hat") {
		t.Fatalf("the refusal must say what is wrong, got %q", err)
	}
}

func TestUpsertFolder_ProductFolderWithACloudCredentialIsRefused(t *testing.T) {
	s := spec("/w/product", role.Product, "suta")
	s.Cloud = &CloudRef{URL: "https://cp/mcp", User: "u"}
	st := State{}
	st.PutRecord(CloudRecord{URL: "https://cp/mcp", User: "u", Token: "odts_deadbeef"})
	_, _, _, err := UpsertFolder(st, s)
	if err == nil {
		t.Fatal("VR-R4: a product folder must not be given an author-plane credential")
	}
	if !strings.Contains(err.Error(), "product") {
		t.Fatalf("the refusal must name the hat, got %q", err)
	}
}

// Every state this package produces must be loadable by the router. If UpsertFolder can build a
// state that TableFrom refuses, onboarding "succeeds" and the router then fails to start — the
// failure lands on the wrong side of the operation, hours later.
func TestUpsertFolder_ProducesStateTheRouterCanLoad(t *testing.T) {
	st, _, _, _ := UpsertFolder(State{}, spec("/w/product", role.Product, "suta"))
	st.PutRecord(CloudRecord{URL: "https://cp/mcp", User: "u", Token: "odts_deadbeef"})
	s := spec("/w/test", role.Test, "suta")
	s.Cloud = &CloudRef{URL: "https://cp/mcp", User: "u"}
	st, _, _, _ = UpsertFolder(st, s)
	st, _, _, _ = UpsertFolder(st, spec("/w/test", role.Test, "sutb"))
	if _, err := TableFrom(st); err != nil {
		t.Fatalf("TableFrom refused a state UpsertFolder built: %v", err)
	}
}

func TestUpsertFolder_EmptyInstanceIDIsRefused(t *testing.T) {
	if _, _, _, err := UpsertFolder(State{}, spec("/w/test", role.Test, "")); err == nil {
		t.Fatal("an upstream with no instance id is not routable and must be refused")
	}
}

func TestUpsertFolder_MissingExecutorURLIsRefused(t *testing.T) {
	s := spec("/w/test", role.Test, "suta")
	s.Executor.URL = ""
	if _, _, _, err := UpsertFolder(State{}, s); err == nil {
		t.Fatal("an upstream with no URL routes nowhere and must be refused")
	}
}

// The same folder named two ways must be ONE record. A trailing separator or a mixed separator is
// how the shell hands the same directory in twice — `/w/test` from one call site and `/w/test/`
// from another — and two records would mean two router tokens for one .mcp.json.
func TestUpsertFolder_PathIsNormalisedSoOneFolderIsOneRecord(t *testing.T) {
	st, first, _, _ := UpsertFolder(State{}, spec("/w/test", role.Test, "suta"))
	st, second, created, err := UpsertFolder(st, spec("/w/test/", role.Test, "sutb"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if created || second.Token != first.Token || len(st.Folders) != 1 {
		t.Fatalf("a trailing separator produced a second folder record (%d records)", len(st.Folders))
	}
}

func TestUpsertFolder_WindowsPathsAreCaseInsensitiveElsewhereTheyAreNot(t *testing.T) {
	st, first, _, _ := UpsertFolder(State{}, spec("/w/Test", role.Test, "suta"))
	st, _, created, err := UpsertFolder(st, spec("/w/test", role.Test, "sutb"))
	if err != nil && runtime.GOOS != "windows" {
		t.Fatalf("upsert: %v", err)
	}
	if runtime.GOOS == "windows" {
		if created || len(st.Folders) != 1 {
			t.Fatalf("windows paths differing only in case must be one folder (%d records)", len(st.Folders))
		}
		_ = first
		return
	}
	if !created || len(st.Folders) != 2 {
		t.Fatalf("POSIX paths differing in case are DIFFERENT directories (%d records)", len(st.Folders))
	}
}

func TestFindFolder_LocatesByTheNormalisedPath(t *testing.T) {
	st, want, _, _ := UpsertFolder(State{}, spec("/w/test", role.Test, "suta"))
	got, idx := FindFolder(st, "/w/test/")
	if got == nil || idx != 0 || got.Token != want.Token {
		t.Fatalf("FindFolder did not normalise: %+v idx=%d", got, idx)
	}
	if missing, _ := FindFolder(st, "/w/other"); missing != nil {
		t.Fatal("FindFolder must report an unknown path as absent")
	}
}

func TestRemoveInstance_TakesOnlyThatInstance(t *testing.T) {
	st, _, _, _ := UpsertFolder(State{}, spec("/w/test", role.Test, "suta"))
	st, _, _, _ = UpsertFolder(st, spec("/w/test", role.Test, "sutb"))
	st, res, err := RemoveInstance(st, "/w/test", "suta")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !res.Removed {
		t.Fatal("removing a present instance must report removed")
	}
	if res.FolderDropped {
		t.Fatal("a folder still routing another instance must survive")
	}
	f, _ := FindFolder(st, "/w/test")
	if _, still := f.Upstreams["suta"]; still {
		t.Fatal("the instance is still routable after removal")
	}
	if _, other := f.Upstreams["sutb"]; !other {
		t.Fatal("removing one instance took the other with it")
	}
}

// VR-A5: when the last instance leaves, the folder record goes too. Leaving an empty folder behind
// would keep its router token authenticating — an agent would connect successfully and then find
// that every call routes nowhere, which reads as a broken router rather than a torn-down instance.
func TestRemoveInstance_LastOneDropsTheFolder(t *testing.T) {
	st, _, _, _ := UpsertFolder(State{}, spec("/w/test", role.Test, "suta"))
	st, res, err := RemoveInstance(st, "/w/test", "suta")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !res.FolderDropped {
		t.Fatal("the last instance leaving must drop the folder record")
	}
	if len(st.Folders) != 0 {
		t.Fatalf("folder survived with no instances: %+v", st.Folders)
	}
}

// Teardown runs more than once, and it runs against machines where the instance was never there.
// Neither is an error — a teardown that fails because there was nothing to tear down is a teardown
// that cannot be re-run after a partial failure.
func TestRemoveInstance_UnknownFolderOrInstanceIsNotAnError(t *testing.T) {
	st, _, _, _ := UpsertFolder(State{}, spec("/w/test", role.Test, "suta"))
	st2, res, err := RemoveInstance(st, "/w/nowhere", "suta")
	if err != nil || res.Removed {
		t.Fatalf("unknown folder: err=%v removed=%v", err, res.Removed)
	}
	_, res, err = RemoveInstance(st2, "/w/test", "sutz")
	if err != nil || res.Removed {
		t.Fatalf("unknown instance: err=%v removed=%v", err, res.Removed)
	}
}

func TestRemoveFolder_TakesTheWholeRecord(t *testing.T) {
	st, _, _, _ := UpsertFolder(State{}, spec("/w/test", role.Test, "suta"))
	st, _, _, _ = UpsertFolder(st, spec("/w/test", role.Test, "sutb"))
	st, removed, err := RemoveFolder(st, "/w/test/")
	if err != nil || !removed || len(st.Folders) != 0 {
		t.Fatalf("RemoveFolder: removed=%v err=%v left=%d", removed, err, len(st.Folders))
	}
}

// The provenance VR-A5 needs. "Did onboarding create this .mcp.json?" cannot be inferred later —
// by then the file exists either way — so it is recorded when the answer is known.
func TestFolder_RecordsWhetherOnboardingCreatedTheMCPJSON(t *testing.T) {
	s := spec("/w/test", role.Test, "suta")
	s.MCPJSONCreated = true
	st, f, _, _ := UpsertFolder(State{}, s)
	if !f.MCPJSONCreated {
		t.Fatal("provenance was not recorded")
	}
	// A later re-onboard did NOT create the file, and must not erase the fact that a previous one did.
	st, f2, _, _ := UpsertFolder(st, spec("/w/test", role.Test, "sutb"))
	if !f2.MCPJSONCreated {
		t.Fatal("re-onboard erased the provenance — teardown would then refuse to delete a file it created")
	}
	_ = st
}

func TestMintRouterToken_IsUnpredictable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		tok, err := MintRouterToken()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if seen[tok] {
			t.Fatalf("duplicate router token on iteration %d", i)
		}
		if len(tok) < len(RouterTokenPrefix)+24 {
			t.Fatalf("router token %q is too short to be unguessable", tok)
		}
		seen[tok] = true
	}
}

func TestRedacted_StillCarriesNoCredentialsAfterUpsert(t *testing.T) {
	s := spec("/w/test", role.Test, "suta")
	s.Cloud = &CloudRef{URL: "https://cp/mcp", User: "u"}
	base := State{}
	base.PutRecord(CloudRecord{URL: "https://cp/mcp", User: "u", Token: "odts_supersecret"})
	st, f, _, _ := UpsertFolder(base, s)
	out := Redacted(st)
	for _, secret := range []string{f.Token, "exec-suta", "odts_supersecret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("Redacted leaked %q:\n%s", secret, out)
		}
	}
}

// The bug this guards is one an operator would read as "the router is broken": onboarding must never
// move the port out from under a router that is already serving on it.
func TestPortForWiring_ARunningRouterKeepsItsPort(t *testing.T) {
	ln, err := net.Listen("tcp", ListenAddr(PreferredPort))
	if err != nil {
		t.Skipf("cannot bind %d on this machine: %v", PreferredPort, err)
	}
	defer ln.Close()
	got, err := PortForWiring(PreferredPort)
	if err != nil {
		t.Fatalf("PortForWiring: %v", err)
	}
	if got != PreferredPort {
		t.Fatalf("onboarding moved the port off a RUNNING router: want %d, got %d", PreferredPort, got)
	}
}

func TestPortForWiring_FirstEverWireReservesThePreferredPort(t *testing.T) {
	got, err := PortForWiring(0)
	if err != nil {
		t.Fatalf("PortForWiring: %v", err)
	}
	if got < PreferredPort || got > FallbackHigh {
		t.Fatalf("first wire chose %d, outside the router range %d-%d", got, PreferredPort, FallbackHigh)
	}
}

// Onboarding runs this code INSIDE a Linux container while recording folders that live on a Windows
// host. filepath.Abs there would produce `/work/C:/agents/test` — a path the router could never
// match, written into the one file whose whole job is matching paths.
func TestNormalizeFolderPath_AWindowsHostPathSurvivesALinuxContainer(t *testing.T) {
	got, err := NormalizeFolderPath("C:/agents/test/")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got != `C:\agents\test` {
		t.Fatalf("want C:\agents\test, got %q", got)
	}
	if strings.Contains(got, "/work") || strings.HasPrefix(got, "/") {
		t.Fatalf("the host path was re-based against the container's cwd: %q", got)
	}
}

func TestUpsertFolder_TheSameWindowsFolderSpelledTwoWaysIsOneRecord(t *testing.T) {
	st, first, _, err := UpsertFolder(State{}, spec(`C:/agents/test`, role.Test, "suta"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	st, second, created, err := UpsertFolder(st, spec(`C:\Agents\Test\`, role.Test, "sutb"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if created || second.Token != first.Token || len(st.Folders) != 1 {
		t.Fatalf("one Windows folder became %d records", len(st.Folders))
	}
	// Case folds for COMPARISON only — the stored path keeps what the operator typed first.
	if st.Folders[0].Path != `C:\agents\test` {
		t.Fatalf("the stored path was rewritten: %q", st.Folders[0].Path)
	}
}
