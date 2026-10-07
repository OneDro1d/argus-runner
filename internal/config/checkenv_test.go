package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// / `check_env` declares the environment variable NAMES a check uses.

const checkEnvBase = `project:
  name: p
targets:
  http:
    base_url: http://sut.invalid:8081
`

const fakeSecretValue = "fake-secret-value-0451"

func parseUnresolvedFrom(t *testing.T, body string) *Config {
	t.Helper()
	p := t.TempDir() + "/argus-config.yaml"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := ParseUnresolved(p)
	if err != nil {
		t.Fatalf("ParseUnresolved: %v", err)
	}
	return c
}

func TestCheckEnv_DeclaredNamesAppearInEnvRefs(t *testing.T) {
	c := parseUnresolvedFrom(t, checkEnvBase+"check_env:\n  - SOME_PASSWORD\n  - OTHER_TOKEN\n")
	got := map[string]string{}
	for _, r := range c.EnvRefs() {
		got[r.Name] = r.Field
	}
	for _, n := range []string{"SOME_PASSWORD", "OTHER_TOKEN"} {
		if got[n] != "check_env" {
			t.Errorf("EnvRefs missed declared name %s (got %v)", n, got)
		}
	}
}

func TestCheckEnv_DuplicateAndImpliedNamesAreListedOnce(t *testing.T) {
	body := checkEnvBase + "  auth:\n    type: bearer\n    bearer_token: ${SHARED_TOKEN}\n" +
		"check_env:\n  - SOME_PASSWORD\n  - SOME_PASSWORD\n  - SHARED_TOKEN\n"
	c := parseUnresolvedFrom(t, body)
	count := map[string]int{}
	field := map[string]string{}
	for _, r := range c.EnvRefs() {
		count[r.Name]++
		field[r.Name] = r.Field
	}
	if count["SOME_PASSWORD"] != 1 {
		t.Errorf("a name declared twice must be listed once, got %d", count["SOME_PASSWORD"])
	}
	if count["SHARED_TOKEN"] != 1 || field["SHARED_TOKEN"] != "targets.auth.bearer_token" {
		t.Errorf("a name a credential field already references must stay listed once, under that field; got count=%d field=%q", count["SHARED_TOKEN"], field["SHARED_TOKEN"])
	}
}

func TestCheckEnv_UnsetDeclaredNameFailsLoadByName(t *testing.T) {
	t.Setenv("OTHER_TOKEN", fakeSecretValue)
	_, err := loadFrom(t, checkEnvBase+"check_env:\n  - SOME_PASSWORD\n  - OTHER_TOKEN\n")
	if err == nil {
		t.Fatal("a declared name that is not in the environment must fail Load, like a credential reference")
	}
	if !strings.Contains(err.Error(), "SOME_PASSWORD (referenced by check_env)") {
		t.Errorf("the error must name the missing variable and where it was declared; got: %v", err)
	}
	if strings.Contains(err.Error(), "OTHER_TOKEN") || strings.Contains(err.Error(), fakeSecretValue) {
		t.Errorf("only the MISSING name belongs in the error, never a set one or a value; got: %v", err)
	}
}

func TestCheckEnv_SetDeclaredNameLoads(t *testing.T) {
	t.Setenv("SOME_PASSWORD", fakeSecretValue)
	c, err := loadFrom(t, checkEnvBase+"check_env:\n  - SOME_PASSWORD\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.EnvRefs(); len(got) != 1 || got[0].Name != "SOME_PASSWORD" {
		t.Errorf("EnvRefs = %v", got)
	}
}

func TestCheckEnv_RefusesEntriesThatAreNotNamesWithoutEchoingThem(t *testing.T) {
	for _, entry := range []string{"SOME_PASSWORD=" + fakeSecretValue, "has space", "lower_case", "1STARTS_WITH_DIGIT", "ARGUS_RUNNER_TOKEN", "${SOME_PASSWORD}"} {
		_, err := loadFrom(t, checkEnvBase+"check_env:\n  - \""+entry+"\"\n")
		if err == nil {
			t.Errorf("entry %q was accepted", entry)
			continue
		}
		if !strings.Contains(err.Error(), "check_env[0]") {
			t.Errorf("entry %q: the refusal must say which entry; got: %v", entry, err)
		}
		if strings.Contains(err.Error(), fakeSecretValue) || strings.Contains(err.Error(), entry) {
			t.Errorf("entry %q: the refusal echoed the entry (it may be a pasted value): %v", entry, err)
		}
	}
}

func TestCheckEnv_RefusesMoreThanTheMaximum(t *testing.T) {
	var b strings.Builder
	b.WriteString(checkEnvBase + "check_env:\n")
	const max = 64 // the documented bound
	for i := 0; i <= max; i++ {
		b.WriteString("  - NAME_" + strings.Repeat("A", 1) + string(rune('A'+i%26)) + string(rune('A'+i/26)) + "\n")
	}
	if _, err := loadFrom(t, b.String()); err == nil || !strings.Contains(err.Error(), "check_env") {
		t.Errorf("more than %d names must be refused by key; got %v", max, err)
	}
}

// An executor built before this key decodes the top level leniently and ignores it — it does NOT refuse
// it. The consequence is silent: the name is never scanned or delivered. The docs say to upgrade the
// executor first; this pins the fact the "Upgrade notes" line rests on.
func TestCheckEnv_OldExecutorDecodesIgnoring(t *testing.T) {
	dec := yaml.NewDecoder(bytes.NewReader([]byte(checkEnvBase + "check_env:\n  - SOME_PASSWORD\n")))
	dec.KnownFields(true)
	var d oldStrictTargetsDoc
	if err := dec.Decode(&d); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("an old executor's strict decode refused the new top-level key: %v", err)
	}
	if _, ok := d.Rest["check_env"]; !ok {
		t.Error("the key did not land in the old executor's lenient inline map")
	}
}

// Names only: the scan's JSON and every error carry no value, even with the value set in the environment.
func TestCheckEnv_ScanReportCarriesNamesOnly(t *testing.T) {
	t.Setenv("SOME_PASSWORD", fakeSecretValue)
	c := parseUnresolvedFrom(t, checkEnvBase+"check_env:\n  - SOME_PASSWORD\n")
	b, err := json.Marshal(map[string]any{"count": len(c.EnvRefs()), "referenced": c.EnvRefs()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), fakeSecretValue) {
		t.Fatalf("the scan report carries a value: %s", b)
	}
	if !strings.Contains(string(b), `"name":"SOME_PASSWORD"`) {
		t.Fatalf("the scan report does not carry the name: %s", b)
	}
}
