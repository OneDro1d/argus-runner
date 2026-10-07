package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// secretsmigrate_cmd_test.go — msgbus tester follow-up item 3 (2026-09-28): `argus secrets
// migrate` renames the exec-tokens Secret's ARGUS_AUTHOR_TOKEN key to ARGUS_EXECUTOR_SECRET in
// place, server-side, value never printed/written to a file/in argv. Uses the SAME fake-kubectl
// harness secrets_cmd_test.go's `secrets set`/`secrets list` tests use (writeFakeKubectl,
// callsFile, stdinFile, runSecretsWithStdin).

// secretObj mirrors just enough of a Secret's JSON to assert on the payload `secrets migrate`
// writes back via `kubectl replace -f -`.
type secretObj struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   map[string]any    `json:"metadata"`
	Type       string            `json:"type"`
	Data       map[string]string `json:"data"`
}

const fakeTokenValueB64 = "dG9rZW4tdmFsdWUtNDI=" // base64("token-value-42") — never a plaintext secret, but still must never be printed as-is either

func TestSecretsMigrate_RenamesKeyAndDropsLastApplied(t *testing.T) {
	dir := t.TempDir()
	getJSON := `{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": {
			"name": "exec-tokens", "namespace": "argus-inst-i9", "resourceVersion": "12345",
			"annotations": {
				"kubectl.kubernetes.io/last-applied-configuration": "{\"stale\":\"copy\"}",
				"keep-me": "yes"
			}
		},
		"data": {"ARGUS_RUNNER_TOKEN": "YWJj", "ARGUS_AUTHOR_TOKEN": "` + fakeTokenValueB64 + `"}
	}`
	writeFakeKubectl(t, dir, getJSON)

	out, code := runSecretsWithStdin(t, "", cmdSecretsMigrate, []string{"--namespace", "argus-inst-i9", "--drop-old"})
	if code != exitOK {
		t.Fatalf("cmdSecretsMigrate exit=%d output=%s", code, out)
	}
	if strings.Contains(out, fakeTokenValueB64) {
		t.Fatalf("secrets migrate printed the value in its own output: %s", out)
	}

	calls := callsFile(t, dir)
	if len(calls) != 2 {
		t.Fatalf("expected 2 kubectl calls (get + replace, no --restart), got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "get secret exec-tokens -n argus-inst-i9 -o json") {
		t.Fatalf("first kubectl call = %q, want a get -o json", calls[0])
	}
	if calls[1] != "replace -f -" {
		t.Fatalf("second kubectl call = %q, want exactly \"replace -f -\"", calls[1])
	}
	for _, c := range calls {
		if strings.Contains(c, "apply") {
			t.Fatalf("secrets migrate must NEVER run kubectl apply: %q", c)
		}
	}

	stdin1 := stdinFile(t, dir, 1)
	if strings.Contains(stdin1, fakeTokenValueB64) == false {
		t.Fatalf("the value never reached kubectl's replace stdin at all: %q", stdin1)
	}
	var sec secretObj
	if err := json.Unmarshal([]byte(stdin1), &sec); err != nil {
		t.Fatalf("replace stdin was not the expected Secret JSON: %v (%q)", err, stdin1)
	}
	if _, stillThere := sec.Data["ARGUS_AUTHOR_TOKEN"]; stillThere {
		t.Fatalf("old key ARGUS_AUTHOR_TOKEN is still present in the replacement object: %+v", sec.Data)
	}
	if sec.Data["ARGUS_EXECUTOR_SECRET"] != fakeTokenValueB64 {
		t.Fatalf("ARGUS_EXECUTOR_SECRET = %q, want the moved value %q", sec.Data["ARGUS_EXECUTOR_SECRET"], fakeTokenValueB64)
	}
	if sec.Data["ARGUS_RUNNER_TOKEN"] != "YWJj" {
		t.Fatalf("unrelated key ARGUS_RUNNER_TOKEN was disturbed: %+v", sec.Data)
	}
	anns, _ := sec.Metadata["annotations"].(map[string]any)
	if anns != nil {
		if _, present := anns["kubectl.kubernetes.io/last-applied-configuration"]; present {
			t.Fatalf("last-applied-configuration annotation survived into the replacement object: %+v", anns)
		}
		if anns["keep-me"] != "yes" {
			t.Fatalf("an unrelated annotation was dropped: %+v", anns)
		}
	}
	if sec.Metadata["resourceVersion"] != "12345" {
		t.Fatalf("resourceVersion was not preserved: %+v", sec.Metadata)
	}
	if sec.Metadata["name"] != "exec-tokens" || sec.Metadata["namespace"] != "argus-inst-i9" {
		t.Fatalf("name/namespace were not preserved: %+v", sec.Metadata)
	}

	if !strings.Contains(out, `"migrated": true`) {
		t.Fatalf("output does not report migrated:true: %s", out)
	}
	if !strings.Contains(out, "kubectl rollout restart deployment/executor -n argus-inst-i9") {
		t.Fatalf("secrets migrate did not report the restart command needed: %s", out)
	}
	if !strings.Contains(out, "v0.4.0") {
		t.Fatalf("secrets migrate did not say when the alias ends: %s", out)
	}
}

func TestSecretsMigrate_AlreadyMigrated_IsIdempotentAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	getJSON := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"exec-tokens","namespace":"argus-inst-i9"},
		"data":{"ARGUS_RUNNER_TOKEN":"YWJj","ARGUS_EXECUTOR_SECRET":"` + fakeTokenValueB64 + `"}}`
	writeFakeKubectl(t, dir, getJSON)

	out, code := runSecretsWithStdin(t, "", cmdSecretsMigrate, []string{"--namespace", "argus-inst-i9"})
	if code != exitOK {
		t.Fatalf("cmdSecretsMigrate exit=%d output=%s", code, out)
	}
	if !strings.Contains(out, `"already_migrated": true`) {
		t.Fatalf("output does not report already_migrated:true: %s", out)
	}
	calls := callsFile(t, dir)
	if len(calls) != 1 {
		t.Fatalf("already-migrated must write NOTHING (only the get call), got %d calls: %v", len(calls), calls)
	}
}

func TestSecretsMigrate_BothPresentSameValue_DropsOldKeyOnly(t *testing.T) {
	dir := t.TempDir()
	getJSON := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"exec-tokens","namespace":"argus-inst-i9"},
		"data":{"ARGUS_AUTHOR_TOKEN":"` + fakeTokenValueB64 + `","ARGUS_EXECUTOR_SECRET":"` + fakeTokenValueB64 + `"}}`
	writeFakeKubectl(t, dir, getJSON)

	out, code := runSecretsWithStdin(t, "", cmdSecretsMigrate, []string{"--namespace", "argus-inst-i9", "--drop-old"})
	if code != exitOK {
		t.Fatalf("cmdSecretsMigrate exit=%d output=%s", code, out)
	}
	calls := callsFile(t, dir)
	if len(calls) != 2 {
		t.Fatalf("expected get + replace, got %d: %v", len(calls), calls)
	}
	var sec secretObj
	if err := json.Unmarshal([]byte(stdinFile(t, dir, 1)), &sec); err != nil {
		t.Fatalf("replace stdin was not valid Secret JSON: %v", err)
	}
	if _, stillThere := sec.Data["ARGUS_AUTHOR_TOKEN"]; stillThere {
		t.Fatalf("old key was not dropped when both keys already matched: %+v", sec.Data)
	}
	if sec.Data["ARGUS_EXECUTOR_SECRET"] != fakeTokenValueB64 {
		t.Fatalf("new key's value changed: %q", sec.Data["ARGUS_EXECUTOR_SECRET"])
	}
	if !strings.Contains(out, `"migrated": true`) {
		t.Fatalf("output does not report migrated:true: %s", out)
	}
}

func TestSecretsMigrate_BothPresentDifferentValue_RefusesAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	getJSON := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"exec-tokens","namespace":"argus-inst-i9"},
		"data":{"ARGUS_AUTHOR_TOKEN":"` + fakeTokenValueB64 + `","ARGUS_EXECUTOR_SECRET":"ZGlmZmVyZW50LXZhbHVl"}}`
	writeFakeKubectl(t, dir, getJSON)

	out, code := runSecretsWithStdin(t, "", cmdSecretsMigrate, []string{"--namespace", "argus-inst-i9"})
	if code == exitOK {
		t.Fatalf("cmdSecretsMigrate exit=%d, want non-zero (conflicting values). output=%s", code, out)
	}
	calls := callsFile(t, dir)
	if len(calls) != 1 {
		t.Fatalf("a value conflict must write NOTHING (only the get call), got %d calls: %v", len(calls), calls)
	}
	for _, want := range []string{"ARGUS_AUTHOR_TOKEN", "ARGUS_EXECUTOR_SECRET", "DIFFERENT"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal does not mention %q: %s", want, out)
		}
	}
	if strings.Contains(out, fakeTokenValueB64) || strings.Contains(out, "ZGlmZmVyZW50LXZhbHVl") {
		t.Fatalf("refusal message leaked a value: %s", out)
	}
}

func TestSecretsMigrate_NeitherKeyPresent_ClearErrorAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	getJSON := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"exec-tokens","namespace":"argus-inst-i9"},
		"data":{"ARGUS_RUNNER_TOKEN":"YWJj"}}`
	writeFakeKubectl(t, dir, getJSON)

	out, code := runSecretsWithStdin(t, "", cmdSecretsMigrate, []string{"--namespace", "argus-inst-i9"})
	if code == exitOK {
		t.Fatalf("cmdSecretsMigrate exit=%d, want non-zero (nothing to migrate). output=%s", code, out)
	}
	calls := callsFile(t, dir)
	if len(calls) != 1 {
		t.Fatalf("nothing-to-migrate must write NOTHING (only the get call), got %d calls: %v", len(calls), calls)
	}
	for _, want := range []string{"ARGUS_AUTHOR_TOKEN", "ARGUS_EXECUTOR_SECRET", "nothing to migrate"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal does not mention %q: %s", want, out)
		}
	}
}

func TestSecretsMigrate_Restart_RunsRolloutRestart(t *testing.T) {
	dir := t.TempDir()
	getJSON := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"exec-tokens","namespace":"argus-inst-i9"},
		"data":{"ARGUS_AUTHOR_TOKEN":"` + fakeTokenValueB64 + `"}}`
	writeFakeKubectl(t, dir, getJSON)

	out, code := runSecretsWithStdin(t, "", cmdSecretsMigrate, []string{"--namespace", "argus-inst-i9", "--restart"})
	if code != exitOK {
		t.Fatalf("cmdSecretsMigrate exit=%d output=%s", code, out)
	}
	calls := callsFile(t, dir)
	if len(calls) != 3 {
		t.Fatalf("expected get + replace + rollout restart, got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[2], "rollout restart deployment/executor -n argus-inst-i9") {
		t.Fatalf("third kubectl call = %q, want a rollout restart", calls[2])
	}
	if !strings.Contains(out, `"restarted": true`) {
		t.Fatalf("output does not report restarted:true: %s", out)
	}
}

func TestSecretsMigrate_RefusesWithoutNamespace(t *testing.T) {
	dir := t.TempDir()
	writeFakeKubectl(t, dir, "")
	_, code := runSecretsWithStdin(t, "", cmdSecretsMigrate, nil)
	if code != exitUsage {
		t.Fatalf("exit = %d, want exitUsage", code)
	}
	if calls := callsFile(t, dir); len(calls) != 0 {
		t.Fatalf("kubectl was invoked despite missing --namespace: %v", calls)
	}
}

// TestSecretsMigrate_ListedInDispatchHelp is a light integration check that `secrets migrate` is
// reachable through the real `secrets` subcommand dispatcher, not just callable as a bare Go func.
func TestSecretsMigrate_ListedInDispatchHelp(t *testing.T) {
	rc := cmdSecrets([]string{"--help"})
	if rc != exitOK {
		t.Fatalf("argus secrets --help: exit %d", rc)
	}
}

// TestSecretsMigrate_DefaultKeepsOldKey: an executor older than 0.3.40 reads ONLY ARGUS_AUTHOR_TOKEN,
// so the default is a COPY — both keys carry the value afterwards, and the old one goes only with
// --drop-old. A 0.3.40+ executor reads the new key silently when both are set (envname.Lookup).
func TestSecretsMigrate_DefaultKeepsOldKey(t *testing.T) {
	dir := t.TempDir()
	getJSON := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"exec-tokens","namespace":"argus-inst-i9"},
		"data":{"ARGUS_AUTHOR_TOKEN":"` + fakeTokenValueB64 + `"}}`
	writeFakeKubectl(t, dir, getJSON)

	out, code := runSecretsWithStdin(t, "", cmdSecretsMigrate, []string{"--namespace", "argus-inst-i9"})
	if code != exitOK {
		t.Fatalf("cmdSecretsMigrate exit=%d output=%s", code, out)
	}
	if strings.Contains(out, fakeTokenValueB64) {
		t.Fatalf("secrets migrate printed the value in its own output: %s", out)
	}
	var sec secretObj
	if err := json.Unmarshal([]byte(stdinFile(t, dir, 1)), &sec); err != nil {
		t.Fatalf("replace stdin was not valid Secret JSON: %v", err)
	}
	if sec.Data["ARGUS_AUTHOR_TOKEN"] != fakeTokenValueB64 || sec.Data["ARGUS_EXECUTOR_SECRET"] != fakeTokenValueB64 {
		t.Fatalf("default migrate must COPY (both keys, same value), got %+v", sec.Data)
	}
	if !strings.Contains(out, `"old_key_kept": true`) {
		t.Fatalf("output does not say the old key was kept: %s", out)
	}
}

// TestSecretsMigrate_BothPresentSameValue_DefaultWritesNothing: already copied, no --drop-old → no write.
func TestSecretsMigrate_BothPresentSameValue_DefaultWritesNothing(t *testing.T) {
	dir := t.TempDir()
	getJSON := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"exec-tokens","namespace":"argus-inst-i9"},
		"data":{"ARGUS_AUTHOR_TOKEN":"` + fakeTokenValueB64 + `","ARGUS_EXECUTOR_SECRET":"` + fakeTokenValueB64 + `"}}`
	writeFakeKubectl(t, dir, getJSON)

	out, code := runSecretsWithStdin(t, "", cmdSecretsMigrate, []string{"--namespace", "argus-inst-i9"})
	if code != exitOK {
		t.Fatalf("cmdSecretsMigrate exit=%d output=%s", code, out)
	}
	if calls := callsFile(t, dir); len(calls) != 1 {
		t.Fatalf("already copied without --drop-old must write NOTHING, got %d calls: %v", len(calls), calls)
	}
	if !strings.Contains(out, `"already_migrated": true`) {
		t.Fatalf("output does not report already_migrated:true: %s", out)
	}
}
