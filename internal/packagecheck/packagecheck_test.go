package packagecheck

import (
	"strings"
	"testing"
)

// clauseOf returns the named clause from a Report, failing the test if absent.
func clauseOf(t *testing.T, r Report, name string) Clause {
	t.Helper()
	for _, c := range r.Clauses {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("report has no clause %q; clauses: %+v", name, r.Clauses)
	return Clause{}
}

func TestManifestsPass(t *testing.T) {
	c := clauseOf(t, Report{Clauses: []Clause{checkManifests("testdata/manifests-pass")}}, clauseManifests)
	if c.Status != StatusPass {
		t.Fatalf("want PASS, got %s findings=%+v", c.Status, c.Findings)
	}
}

func TestManifestsFail(t *testing.T) {
	c := checkManifests("testdata/manifests-fail")
	if c.Name != clauseManifests {
		t.Fatalf("want clause name %q, got %q", clauseManifests, c.Name)
	}
	if c.Status != StatusFail {
		t.Fatalf("want FAIL, got %s", c.Status)
	}
	wantLocs := []string{
		"k8s/base/control.yaml:10",
		"deploy/compose/docker-compose.yaml:3",
		"k8s/overlays/toy/kustomization.yaml:6",
	}
	for _, want := range wantLocs {
		found := false
		for _, f := range c.Findings {
			if f.Location == want {
				found = true
			}
		}
		if !found {
			t.Errorf("want a finding at %s, findings=%+v", want, c.Findings)
		}
	}
}

func TestLockfilesPass(t *testing.T) {
	c := checkLockfiles("testdata/lockfiles-pass")
	if c.Status != StatusPass {
		t.Fatalf("want PASS, got %s findings=%+v", c.Status, c.Findings)
	}
}

func TestLockfilesFail(t *testing.T) {
	c := checkLockfiles("testdata/lockfiles-fail")
	if c.Name != clauseLockfiles {
		t.Fatalf("want clause name %q, got %q", clauseLockfiles, c.Name)
	}
	if c.Status != StatusFail {
		t.Fatalf("want FAIL, got %s", c.Status)
	}
	if len(c.Findings) < 2 {
		t.Fatalf("want at least 2 findings (go.sum mismatch + ui version mismatch), got %+v", c.Findings)
	}
	var sawGoMod, sawUI bool
	for _, f := range c.Findings {
		if strings.HasPrefix(f.Location, "go.mod:") {
			sawGoMod = true
		}
		if strings.HasPrefix(f.Location, "ui/package-lock.json:") {
			sawUI = true
		}
	}
	if !sawGoMod || !sawUI {
		t.Errorf("want both a go.mod and a ui/package-lock.json finding, got %+v", c.Findings)
	}
}

func TestEnvSchemaPass(t *testing.T) {
	c := checkEnvSchema("testdata/env-pass")
	if c.Status != StatusPass {
		t.Fatalf("want PASS, got %s findings=%+v", c.Status, c.Findings)
	}
}

func TestEnvSchemaFail(t *testing.T) {
	c := checkEnvSchema("testdata/env-fail")
	if c.Name != clauseEnvSchema {
		t.Fatalf("want clause name %q, got %q", clauseEnvSchema, c.Name)
	}
	if c.Status != StatusFail {
		t.Fatalf("want FAIL, got %s", c.Status)
	}
	want := "cmd/toy/main.go:9"
	found := false
	for _, f := range c.Findings {
		if f.Location == want && strings.Contains(f.Reason, "UNDECLARED_VAR") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a finding at %s naming UNDECLARED_VAR, got %+v", want, c.Findings)
	}
}

func TestSecretSchemaPass(t *testing.T) {
	c := checkSecretSchema("testdata/secret-pass")
	if c.Status != StatusPass {
		t.Fatalf("want PASS, got %s findings=%+v", c.Status, c.Findings)
	}
}

func TestSecretSchemaFail(t *testing.T) {
	c := checkSecretSchema("testdata/secret-fail")
	if c.Name != clauseSecretSchema {
		t.Fatalf("want clause name %q, got %q", clauseSecretSchema, c.Name)
	}
	if c.Status != StatusFail {
		t.Fatalf("want FAIL, got %s", c.Status)
	}
	want := "k8s/base/cp-secrets.example.yaml:7"
	found := false
	for _, f := range c.Findings {
		if f.Location == want {
			found = true
		}
	}
	if !found {
		t.Errorf("want a finding at %s, got %+v", want, c.Findings)
	}
}

func TestSeedDataPass(t *testing.T) {
	c := checkSeedData("testdata/seed-pass")
	if c.Status != StatusPass {
		t.Fatalf("want PASS, got %s findings=%+v", c.Status, c.Findings)
	}
}

func TestSeedDataFail(t *testing.T) {
	c := checkSeedData("testdata/seed-fail")
	if c.Name != clauseSeedData {
		t.Fatalf("want clause name %q, got %q", clauseSeedData, c.Name)
	}
	if c.Status != StatusFail {
		t.Fatalf("want FAIL, got %s", c.Status)
	}
	found := false
	for _, f := range c.Findings {
		if strings.Contains(f.Reason, "missing-example") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a finding naming missing-example, got %+v", c.Findings)
	}
}

func TestCheckOrdersClauses(t *testing.T) {
	r := Check("testdata/manifests-pass")
	want := []string{clauseManifests, clauseLockfiles, clauseEnvSchema, clauseSecretSchema, clauseSeedData}
	if len(r.Clauses) != len(want) {
		t.Fatalf("want %d clauses, got %d: %+v", len(want), len(r.Clauses), r.Clauses)
	}
	for i, name := range want {
		if r.Clauses[i].Name != name {
			t.Errorf("clause %d: want %q, got %q", i, name, r.Clauses[i].Name)
		}
	}
}

func TestReportStatusFailsOnAnyClause(t *testing.T) {
	r := Report{Clauses: []Clause{
		{Name: "a", Status: StatusPass},
		{Name: "b", Status: StatusFail},
	}}
	if r.Status() != StatusFail {
		t.Fatalf("want FAIL, got %s", r.Status())
	}
}
