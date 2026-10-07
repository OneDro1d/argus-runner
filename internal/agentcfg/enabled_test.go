package agentcfg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func settingsPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "settings.local.json")
}

func writeSettings(t *testing.T, body string) string {
	t.Helper()
	p := settingsPath(t)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnableServer_CreatesTheFileWhenThereIsNone(t *testing.T) {
	p := settingsPath(t)
	created, err := EnableServer(p, "argus")
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !created {
		t.Fatal("a file that did not exist must report created — teardown's permission to delete it depends on this")
	}
	got, err := EnabledServers(p)
	if err != nil || len(got) != 1 || got[0] != "argus" {
		t.Fatalf("EnabledServers = %v (%v)", got, err)
	}
}

// THE DEFECT THIS FILE EXISTS FOR. The pre-M3-FX shell wrote this file with a truncating printf, so
// a user's permissions, hooks and env — none of which onboarding put there — were deleted every time
// an instance was onboarded.
func TestEnableServer_LeavesTheUsersOwnSettingsAlone(t *testing.T) {
	original := `{
  "permissions": {
    "allow": ["Bash(git status)"],
    "deny": ["Bash(rm -rf /)"]
  },
  "hooks": { "PreToolUse": [{ "matcher": "Bash", "hooks": [] }] },
  "enabledMcpjsonServers": ["someone-elses"],
  "env": { "FOO": "bar" }
}
`
	p := writeSettings(t, original)
	created, err := EnableServer(p, "argus")
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if created {
		t.Fatal("the file already existed — reporting it as created would let teardown DELETE a user's settings")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(read(t, p)), &doc); err != nil {
		t.Fatalf("we produced invalid JSON: %v\n%s", err, read(t, p))
	}
	if doc["permissions"] == nil || doc["hooks"] == nil || doc["env"] == nil {
		t.Fatalf("the user's own settings were destroyed:\n%s", read(t, p))
	}
	got, _ := EnabledServers(p)
	if len(got) != 2 || got[0] != "someone-elses" || got[1] != "argus" {
		t.Fatalf("want [someone-elses argus], got %v", got)
	}
	// The permissions block must be byte-identical, not merely equivalent.
	if !strings.Contains(read(t, p), `"deny": ["Bash(rm -rf /)"]`) {
		t.Fatalf("the permissions block was reformatted:\n%s", read(t, p))
	}
}

func TestEnableServer_AddsTheMemberWhenTheFileHasOtherKeysButNotOurs(t *testing.T) {
	p := writeSettings(t, "{\n  \"permissions\": { \"allow\": [] }\n}\n")
	if _, err := EnableServer(p, "argus"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, err := EnabledServers(p)
	if err != nil || len(got) != 1 || got[0] != "argus" {
		t.Fatalf("EnabledServers = %v (%v)\n%s", got, err, read(t, p))
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(read(t, p)), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, read(t, p))
	}
	if doc["permissions"] == nil {
		t.Fatalf("permissions lost:\n%s", read(t, p))
	}
}

// Re-running onboarding must not touch a file it has nothing to change in. A rewrite that produces
// identical bytes still moves the mtime, and an mtime is a signal somebody may be watching.
func TestEnableServer_AlreadyPresentTouchesNothing(t *testing.T) {
	p := writeSettings(t, `{"enabledMcpjsonServers":["argus"]}`)
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	body := read(t, p)
	if _, err := EnableServer(p, "argus"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, p) != body {
		t.Fatalf("the file changed:\n%s", read(t, p))
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("a no-op rewrote the file")
	}
}

func TestEnableServer_MalformedIsRefusedAndUntouched(t *testing.T) {
	p := writeSettings(t, `{ "enabledMcpjsonServers": [ `)
	body := read(t, p)
	_, err := EnableServer(p, "argus")
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("want ErrMalformed, got %v", err)
	}
	if read(t, p) != body {
		t.Fatal("a malformed file was rewritten — that destroys the hand edit the user is trying to fix")
	}
}

// A value we cannot represent is a reason to stop, not to overwrite. Both of these would silently
// lose data if we re-serialised.
func TestEnableServer_ARefusedShapeIsNeverRewritten(t *testing.T) {
	for _, body := range []string{
		`{"enabledMcpjsonServers": "argus"}`,
		`{"enabledMcpjsonServers": {"argus": true}}`,
		`{"enabledMcpjsonServers": ["argus", 42]}`,
	} {
		p := writeSettings(t, body)
		_, err := EnableServer(p, "other")
		if !errors.Is(err, ErrNotAnArray) {
			t.Fatalf("%s: want ErrNotAnArray, got %v", body, err)
		}
		if read(t, p) != body {
			t.Fatalf("%s: was rewritten to %s", body, read(t, p))
		}
	}
}

func TestDisableServer_TakesOnlyOurEntry(t *testing.T) {
	p := writeSettings(t, `{"enabledMcpjsonServers":["someone-elses","argus","third"]}`)
	removed, deleted, err := DisableServer(p, "argus", true)
	if err != nil || !removed || deleted {
		t.Fatalf("removed=%v deleted=%v err=%v", removed, deleted, err)
	}
	got, _ := EnabledServers(p)
	if len(got) != 2 || got[0] != "someone-elses" || got[1] != "third" {
		t.Fatalf("want the other two intact, got %v", got)
	}
}

func TestDisableServer_LastEntryInAFileWeCreatedDeletesTheFile(t *testing.T) {
	p := settingsPath(t)
	if _, err := EnableServer(p, "argus"); err != nil {
		t.Fatal(err)
	}
	removed, deleted, err := DisableServer(p, "argus", true)
	if err != nil || !removed || !deleted {
		t.Fatalf("removed=%v deleted=%v err=%v", removed, deleted, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("an empty settings file was left behind")
	}
}

// The protective case: even with deleteIfEmpty, a file holding anything of the user's is KEPT.
func TestDisableServer_AFileHoldingUserSettingsIsAlwaysKept(t *testing.T) {
	p := writeSettings(t, `{"permissions":{"allow":["Bash(ls)"]},"enabledMcpjsonServers":["argus"]}`)
	removed, deleted, err := DisableServer(p, "argus", true)
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if deleted {
		t.Fatal("teardown deleted a file holding the user's permissions")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(read(t, p)), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, read(t, p))
	}
	if doc["permissions"] == nil {
		t.Fatalf("permissions lost:\n%s", read(t, p))
	}
}

func TestDisableServer_MissingFileOrAbsentEntryIsNotAnError(t *testing.T) {
	removed, deleted, err := DisableServer(settingsPath(t), "argus", true)
	if err != nil || removed || deleted {
		t.Fatalf("missing file: removed=%v deleted=%v err=%v", removed, deleted, err)
	}
	p := writeSettings(t, `{"enabledMcpjsonServers":["other"]}`)
	removed, _, err = DisableServer(p, "argus", true)
	if err != nil || removed {
		t.Fatalf("absent entry: removed=%v err=%v", removed, err)
	}
}

func TestEnableServer_PreservesCRLF(t *testing.T) {
	p := writeSettings(t, "{\r\n  \"permissions\": {}\r\n}\r\n")
	if _, err := EnableServer(p, "argus"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	body := read(t, p)
	if strings.Contains(body, "\n") && !strings.Contains(body, "\r\n") {
		t.Fatalf("CRLF was converted to LF:\n%q", body)
	}
	if got, _ := EnabledServers(p); len(got) != 1 {
		t.Fatalf("entry not added: %v", got)
	}
}
