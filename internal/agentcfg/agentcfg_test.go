package agentcfg

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The rules under test are VR-A1..A3 and VR-A5 of the M3-FX build. They exist because the
// pre-M3-FX writer (onboard.sh:1335) was a truncating heredoc under a FIXED key: it destroyed any
// pre-existing .mcp.json, and onboarding a SECOND instance into one folder silently deleted the
// first. Every test below is a behaviour that heredoc got wrong.

const srvA = `{"type":"sse","url":"http://127.0.0.1:9765/sse","headers":{"Authorization":"Bearer odtr_aaa"}}`
const srvB = `{"type":"sse","url":"http://127.0.0.1:9765/sse","headers":{"Authorization":"Bearer odtr_bbb"}}`

func write(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// VR-A1 (create half): no file yet -> onboarding creates one, and reports that it created it.
// The provenance matters: VR-A5 may only DELETE a file that onboarding itself created.
func TestMerge_CreatesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".mcp.json")
	created, err := Merge(p, "argus_suta", []byte(srvA))
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !created {
		t.Error("created = false; a missing file must report created = true")
	}
	got := read(t, p)
	if !strings.Contains(got, `"argus_suta"`) || !strings.Contains(got, "odtr_aaa") {
		t.Errorf("entry not written:\n%s", got)
	}
	if names, err := Servers(p); err != nil || len(names) != 1 || names[0] != "argus_suta" {
		t.Errorf("Servers = %v, %v; want [argus_suta]", names, err)
	}
}

// VR-A1 (the load-bearing half): a foreign MCP server must survive BYTE-IDENTICALLY, and so must
// the formatting around it. This is the rule the heredoc violated most destructively — someone's
// hand-configured server simply vanished.
func TestMerge_ForeignServerSurvivesByteIdentical(t *testing.T) {
	dir := t.TempDir()
	const before = `{
  "mcpServers": {
    "some-other-tool": {
      "type":    "stdio",
      "command": "/usr/local/bin/other",
      "args":    ["--flag", "value"]
    }
  }
}
`
	p := write(t, dir, before)
	if _, err := Merge(p, "argus_suta", []byte(srvA)); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got := read(t, p)

	// the foreign block, including its idiosyncratic alignment, must appear verbatim
	const foreign = `"some-other-tool": {
      "type":    "stdio",
      "command": "/usr/local/bin/other",
      "args":    ["--flag", "value"]
    }`
	if !strings.Contains(got, foreign) {
		t.Errorf("foreign server was not preserved byte-identically.\n--- got ---\n%s", got)
	}
	names, err := Servers(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Errorf("Servers = %v; want both the foreign server and ours", names)
	}
}

// VR-A2: a re-onboard of the SAME instance replaces that entry in place (new token, new port) and
// never leaves a stale duplicate. Before M3-FX this "worked" only because the whole file was thrown
// away.
func TestMerge_ReonboardReplacesInPlace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".mcp.json")
	if _, err := Merge(p, "argus_suta", []byte(srvA)); err != nil {
		t.Fatal(err)
	}
	if _, err := Merge(p, "argus_suta", []byte(srvB)); err != nil {
		t.Fatal(err)
	}
	got := read(t, p)
	if strings.Contains(got, "odtr_aaa") {
		t.Errorf("the OLD token survived a re-onboard:\n%s", got)
	}
	if !strings.Contains(got, "odtr_bbb") {
		t.Errorf("the NEW token was not written:\n%s", got)
	}
	if n := strings.Count(got, `"argus_suta"`); n != 1 {
		t.Errorf("key appears %d times; a re-onboard must not duplicate it:\n%s", n, got)
	}
}

// VR-A1/A2 together: the capability the whole of block A exists for — one folder accumulating
// several SUTs. The old fixed key `onedroid_argus_runner` made this impossible by construction.
func TestMerge_ThreeInstancesCoexist(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".mcp.json")
	for _, k := range []string{"argus_suta", "argus_sutb", "argus_sutc"} {
		if _, err := Merge(p, k, []byte(srvA)); err != nil {
			t.Fatalf("Merge(%s): %v", k, err)
		}
	}
	names, err := Servers(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Errorf("Servers = %v; want 3 coexisting instances", names)
	}
}

// VR-A3: a malformed file REFUSES and is left untouched. Rewriting it would destroy hand edits,
// which is precisely what the user is trying to protect.
func TestMerge_MalformedRefusesAndDoesNotTouchTheFile(t *testing.T) {
	dir := t.TempDir()
	const broken = `{ "mcpServers": { "oops": { "url": }  ` // truncated + invalid
	p := write(t, dir, broken)
	if _, err := Merge(p, "argus_suta", []byte(srvA)); err == nil {
		t.Fatal("Merge accepted a malformed .mcp.json; it must refuse")
	}
	if got := read(t, p); got != broken {
		t.Errorf("the malformed file was MODIFIED.\nwant: %q\ngot:  %q", broken, got)
	}
}

// A JSON document whose root is not an object is equally unusable, and equally must not be rewritten.
func TestMerge_NonObjectRootRefuses(t *testing.T) {
	dir := t.TempDir()
	const arr = `["not", "an", "object"]`
	p := write(t, dir, arr)
	if _, err := Merge(p, "argus_suta", []byte(srvA)); err == nil {
		t.Fatal("Merge accepted a non-object root; it must refuse")
	}
	if got := read(t, p); got != arr {
		t.Errorf("file was modified: %q", got)
	}
}

// This machine writes CRLF. A line-ending-blind implementation is exactly the class of bug that
// destroyed 30 of 34 findings in FINDINGS-INTAKE.md, so it is pinned here.
func TestMerge_CRLFFileIsHandled(t *testing.T) {
	dir := t.TempDir()
	before := "{\r\n  \"mcpServers\": {\r\n    \"keepme\": {\"type\":\"stdio\",\"command\":\"x\"}\r\n  }\r\n}\r\n"
	p := write(t, dir, before)
	if _, err := Merge(p, "argus_suta", []byte(srvA)); err != nil {
		t.Fatalf("Merge on a CRLF file: %v", err)
	}
	got := read(t, p)
	if !strings.Contains(got, `"keepme"`) {
		t.Errorf("CRLF file lost its existing server:\n%q", got)
	}
	names, _ := Servers(p)
	if len(names) != 2 {
		t.Errorf("Servers = %v; want 2", names)
	}
}

// VR-A5: teardown removes the instance's entry from every file that references it, and the other
// instances keep working.
func TestRemove_LeavesSiblingsIntact(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".mcp.json")
	Merge(p, "argus_suta", []byte(srvA))
	Merge(p, "argus_sutb", []byte(srvB))

	removed, deleted, err := Remove(p, "argus_suta", true)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Error("removed = false; the key was present")
	}
	if deleted {
		t.Error("the FILE was deleted while another instance still referenced it")
	}
	names, err := Servers(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "argus_sutb" {
		t.Errorf("Servers = %v; want only argus_sutb", names)
	}
}

// VR-A5 (the trace rule): when onboarding CREATED the file and this was the last instance in it,
// the FILE is deleted. An empty {"mcpServers":{}} is itself a trace.
func TestRemove_DeletesFileWhenWeCreatedItAndItIsNowEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".mcp.json")
	created, _ := Merge(p, "argus_suta", []byte(srvA))
	if !created {
		t.Fatal("precondition: we must have created the file")
	}
	_, deleted, err := Remove(p, "argus_suta", created)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !deleted {
		t.Error("deleted = false; an onboarding-created file with no servers left must be removed")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("the file still exists: %v", err)
	}
}

// The mirror of the rule above, and the one that protects the user: a file WE did not create is
// never deleted, even when it ends up with no servers.
func TestRemove_KeepsAUserOwnedFileEvenWhenEmptied(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "{\n  \"mcpServers\": {}\n}\n")
	Merge(p, "argus_suta", []byte(srvA))

	_, deleted, err := Remove(p, "argus_suta", false /* we did not create it */)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if deleted {
		t.Fatal("a user-owned .mcp.json was DELETED; only onboarding-created files may be removed")
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("the user's file is gone: %v", err)
	}
}

// Teardown must be idempotent: removing an entry that is not there is not an error, and must not
// report a removal that did not happen.
func TestRemove_AbsentKeyIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".mcp.json")
	Merge(p, "argus_suta", []byte(srvA))
	removed, deleted, err := Remove(p, "argus_nope", true)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removed {
		t.Error("removed = true for a key that was never present")
	}
	if deleted {
		t.Error("deleted = true; the file still holds another server")
	}
}

// A missing file is the normal teardown case on a folder that was never onboarded into.
func TestRemove_MissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	removed, deleted, err := Remove(filepath.Join(dir, ".mcp.json"), "argus_suta", true)
	if err != nil {
		t.Fatalf("Remove on a missing file: %v", err)
	}
	if removed || deleted {
		t.Errorf("removed=%v deleted=%v; nothing existed to remove", removed, deleted)
	}
}

// VR-A3 applies to teardown too: a malformed file is refused, not rewritten and not deleted.
func TestRemove_MalformedRefuses(t *testing.T) {
	dir := t.TempDir()
	const broken = `{ "mcpServers": { `
	p := write(t, dir, broken)
	if _, _, err := Remove(p, "argus_suta", true); err == nil {
		t.Fatal("Remove accepted a malformed .mcp.json; it must refuse")
	}
	if got := read(t, p); got != broken {
		t.Errorf("the malformed file was modified: %q", got)
	}
}

// VR-R12: the file carries bearer tokens, so it must not be world-readable. The pre-M3-FX files
// were measured at -rw-r--r-- in known locations.
//
// PLATFORM TRUTH, found by this test failing on the owner's machine: Windows has no POSIX mode bits,
// and Go's os.Chmod there only toggles the read-only attribute — a file created with 0600 reports
// 0666. So on Windows the 0600 request is NOT what protects the token; the directory ACL is. The
// test skips rather than pretending, because a green assertion here would claim a protection the OS
// is not providing. See the note on fileMode in agentcfg.go.
func TestMerge_FileModeIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows: POSIX mode bits are not enforced; os.Chmod only toggles read-only (see agentcfg.go fileMode)")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, ".mcp.json")
	if _, err := Merge(p, "argus_suta", []byte(srvA)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("mode = %o; a file holding bearer tokens must not be group/world readable", mode)
	}
}

// The server value we are handed must itself be valid JSON — a caller splicing a broken value in
// would corrupt the file for everyone else in it.
func TestMerge_RejectsInvalidServerValue(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".mcp.json")
	if _, err := Merge(p, "argus_suta", []byte(`{"url": }`)); err == nil {
		t.Fatal("Merge accepted an invalid server value")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("a file was created despite the invalid value")
	}
}
