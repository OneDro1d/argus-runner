package packagecheck

import (
	"encoding/json"
	"strings"
	"testing"
)

// AC-36: package-check certifies the DECLARED package of the deployment under test, not the whole
// tree. The fixture under testdata/declared-package carries a production overlay whose images are
// pinned by digest beside a legacy overlay the deployment never applies (images by tag), and keeps
// its env schema, secrets example and seed-data document at its own paths — not this repository's.

// declaredFixture is the declaration the fixture's argus-config.yaml carries (mirrored here so the
// package test does not depend on the config loader).
func declaredFixture() Declaration {
	return Declaration{
		Origin:         OriginDeclared,
		Manifests:      []string{"k8s/overlays/prod"},
		Lockfiles:      []string{"go.sum"},
		EnvSchema:      "config/env.schema.json",
		SecretsExample: "config/secrets.example.yaml",
		Seed:           "docs/SEED.md",
	}
}

const declaredFixtureRoot = "testdata/declared-package"

func TestCheckDeclared_CertifiesTheDeclaredOverlayNotTheTree(t *testing.T) {
	rep := CheckDeclared(declaredFixtureRoot, declaredFixture())
	if rep.Status() != StatusPass {
		t.Fatalf("the declared package must PASS, got %s: %+v", rep.Status(), rep.Clauses)
	}
	for _, c := range rep.Clauses {
		if c.Status != StatusPass {
			t.Errorf("clause %s: want PASS, got %s findings=%+v", c.Name, c.Status, c.Findings)
		}
	}
	if rep.Root != declaredFixtureRoot {
		t.Errorf("report root = %q, want %q", rep.Root, declaredFixtureRoot)
	}
}

func TestCheck_WholeRootFailsOnTheSameTree(t *testing.T) {
	rep := Check(declaredFixtureRoot)
	if rep.Status() != StatusFail {
		t.Fatalf("the whole-root check must FAIL on the legacy overlay and this repository's absent paths, got %s", rep.Status())
	}
	m := clauseOf(t, rep, clauseManifests)
	if m.Status != StatusFail {
		t.Fatalf("manifests: want FAIL on the legacy overlay, got %s", m.Status)
	}
	found := false
	for _, f := range m.Findings {
		if f.Location == "k8s/overlays/legacy/app.yaml:10" {
			found = true
		}
	}
	if !found {
		t.Errorf("want the legacy overlay's tag at k8s/overlays/legacy/app.yaml:10, findings=%+v", m.Findings)
	}
	for _, name := range []string{clauseEnvSchema, clauseSecretSchema, clauseSeedData} {
		if c := clauseOf(t, rep, name); c.Status != StatusFail {
			t.Errorf("%s: the whole-root check reads this repository's own paths, which the fixture does not have; want FAIL, got %s", name, c.Status)
		}
	}
}

func TestCheckDeclared_MissingLockfileIsAFindingOnTheLockfilesClause(t *testing.T) {
	d := declaredFixture()
	d.Lockfiles = append(d.Lockfiles, "ui/pnpm-lock.yaml")
	rep := CheckDeclared(declaredFixtureRoot, d)
	c := clauseOf(t, rep, clauseLockfiles)
	if c.Status != StatusFail {
		t.Fatalf("a declared lockfile that is not there must FAIL the lockfiles clause, got %s", c.Status)
	}
	found := false
	for _, f := range c.Findings {
		if f.Location == "ui/pnpm-lock.yaml:0" && strings.Contains(f.Reason, "ui/pnpm-lock.yaml") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a finding at ui/pnpm-lock.yaml:0 naming the path, got %+v", c.Findings)
	}
	// The other clauses are untouched by the extra lockfile.
	for _, name := range []string{clauseManifests, clauseEnvSchema, clauseSecretSchema, clauseSeedData} {
		if c := clauseOf(t, rep, name); c.Status != StatusPass {
			t.Errorf("%s: want PASS, got %s findings=%+v", name, c.Status, c.Findings)
		}
	}
}

// A declared path that does not exist is a finding on the clause that needed it — for every
// clause, never a silent skip.
func TestCheckDeclared_EveryMissingDeclaredPathIsAFindingOnItsClause(t *testing.T) {
	d := Declaration{
		Origin:         OriginDeclared,
		Manifests:      []string{"k8s/overlays/absent"},
		Lockfiles:      []string{"absent/go.sum"},
		EnvSchema:      "config/absent.schema.json",
		SecretsExample: "config/absent-secrets.yaml",
		Seed:           "docs/ABSENT.md",
	}
	rep := CheckDeclared(declaredFixtureRoot, d)
	want := map[string]string{
		clauseManifests:    "k8s/overlays/absent:0",
		clauseLockfiles:    "absent/go.mod:0",
		clauseEnvSchema:    "config/absent.schema.json:0",
		clauseSecretSchema: "config/absent-secrets.yaml:0",
		clauseSeedData:     "docs/ABSENT.md:0",
	}
	for name, loc := range want {
		c := clauseOf(t, rep, name)
		if c.Status != StatusFail {
			t.Errorf("%s: want FAIL for the absent declared path, got %s", name, c.Status)
			continue
		}
		found := false
		for _, f := range c.Findings {
			if f.Location == loc {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want a finding at %s, got %+v", name, loc, c.Findings)
		}
	}
}

// An explicit file in manifests: is read as-is (a directory is scanned recursively).
func TestCheckDeclared_AnExplicitManifestFileIsReadAsIs(t *testing.T) {
	d := declaredFixture()
	d.Manifests = []string{"k8s/overlays/legacy/app.yaml"}
	c := clauseOf(t, CheckDeclared(declaredFixtureRoot, d), clauseManifests)
	if c.Status != StatusFail {
		t.Fatalf("the legacy file names an image by tag; want FAIL, got %s", c.Status)
	}
	if len(c.Findings) != 1 || c.Findings[0].Location != "k8s/overlays/legacy/app.yaml:10" {
		t.Errorf("want exactly the one tag at k8s/overlays/legacy/app.yaml:10, got %+v", c.Findings)
	}
}

func TestCheckDeclared_ReportCarriesTheDeclaration(t *testing.T) {
	rep := CheckDeclared(declaredFixtureRoot, declaredFixture())
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Declaration struct {
			Origin         string   `json:"origin"`
			Manifests      []string `json:"manifests"`
			Lockfiles      []string `json:"lockfiles"`
			EnvSchema      string   `json:"env_schema"`
			SecretsExample string   `json:"secrets_example"`
			Seed           string   `json:"seed"`
		} `json:"declaration"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	d := back.Declaration
	if d.Origin != OriginDeclared || len(d.Manifests) != 1 || d.Manifests[0] != "k8s/overlays/prod" ||
		len(d.Lockfiles) != 1 || d.Lockfiles[0] != "go.sum" || d.EnvSchema != "config/env.schema.json" ||
		d.SecretsExample != "config/secrets.example.yaml" || d.Seed != "docs/SEED.md" {
		t.Errorf("the JSON report must carry the declaration it certified, got %s", raw)
	}
}

// Check(root) is CheckDeclared with the conventional declaration: this repository's own paths, the
// two optional surfaces (a compose tree, a UI lockfile) declared only when present — exactly how the
// whole-root check has always treated them.
func TestDefaultDeclaration_NamesTheConventionalPaths(t *testing.T) {
	d := DefaultDeclaration("testdata/manifests-fail") // has k8s/ and deploy/compose/, no ui/
	if d.Origin != OriginConventional {
		t.Errorf("origin = %q, want %q", d.Origin, OriginConventional)
	}
	if strings.Join(d.Manifests, ",") != "k8s,deploy/compose" {
		t.Errorf("manifests = %v, want [k8s deploy/compose]", d.Manifests)
	}
	if strings.Join(d.Lockfiles, ",") != "go.sum" {
		t.Errorf("lockfiles = %v, want [go.sum] (no ui/ tree)", d.Lockfiles)
	}
	if d.EnvSchema != "deploy/env.schema.json" || d.SecretsExample != "k8s/base/cp-secrets.example.yaml" || d.Seed != "README.md" {
		t.Errorf("schema/seed paths = %q %q %q, want the conventional three", d.EnvSchema, d.SecretsExample, d.Seed)
	}

	d = DefaultDeclaration("testdata/lockfiles-pass") // has ui/, no k8s/ and no deploy/compose/
	if len(d.Manifests) != 0 {
		t.Errorf("manifests = %v, want none (no k8s/ or deploy/compose/)", d.Manifests)
	}
	if strings.Join(d.Lockfiles, ",") != "go.sum,ui/package-lock.json" {
		t.Errorf("lockfiles = %v, want [go.sum ui/package-lock.json]", d.Lockfiles)
	}
	if rep := Check("testdata/lockfiles-pass"); rep.Declaration.Origin != OriginConventional {
		t.Errorf("Check's report must carry the conventional declaration, got %+v", rep.Declaration)
	}
}

// No regression: Check(root) on every fixture that existed before the declaration gives exactly the
// findings it gave before — clause by clause, location by location. A reason that carries an OS error
// text is matched on its stable prefix.
func TestCheck_WholeRootFindingsOnTheExistingFixturesAreUnchanged(t *testing.T) {
	type finding struct{ loc, reasonPrefix string }
	type clause struct {
		status   Status
		findings []finding
	}
	noGoMod := finding{"go.mod:0", "cannot read go.mod: "}
	noEnvSchema := finding{"deploy/env.schema.json:0", "no env schema file at deploy/env.schema.json (the convention this package defines when none exists)"}
	noSecrets := finding{"k8s/base/cp-secrets.example.yaml:0", "no secrets manifest at k8s/base/cp-secrets.example.yaml to check against the schema"}
	noSeed := finding{"README.md:0", "no examples/ tree line found to name the scenario sets seed data must exist for"}
	pass := clause{status: StatusPass}
	failWith := func(f ...finding) clause { return clause{status: StatusFail, findings: f} }

	cases := map[string][]clause{ // in clause order: manifests, lockfiles, env schema, secret schema, seed data
		"env-fail": {pass, failWith(noGoMod),
			failWith(finding{"cmd/toy/main.go:9", "environment variable UNDECLARED_VAR is read but not declared in deploy/env.schema.json"}),
			failWith(noSecrets), failWith(noSeed)},
		"env-pass": {pass, failWith(noGoMod), pass, failWith(noSecrets), failWith(noSeed)},
		"lockfiles-fail": {pass,
			failWith(finding{"go.mod:6", "go.sum has no entry for go.mod require example.com/dep v1.2.3"},
				finding{"ui/package-lock.json:0", `top-level version "0.1.0" does not match ui/package.json version "0.2.0"`}),
			failWith(noEnvSchema), failWith(noSecrets), failWith(noSeed)},
		"lockfiles-pass": {pass, pass, failWith(noEnvSchema), failWith(noSecrets), failWith(noSeed)},
		"manifests-fail": {
			failWith(finding{"deploy/compose/docker-compose.yaml:3", "image pinned by tag, not digest: postgres:16-alpine"},
				finding{"k8s/base/control.yaml:10", "image pinned by tag, not digest: ghcr.io/example/toy-control:latest"},
				finding{"k8s/overlays/toy/kustomization.yaml:6", "image pinned by tag, not digest: registry.example/toy-control newTag=dev-latest"}),
			failWith(noGoMod), failWith(noEnvSchema), failWith(noSecrets), failWith(noSeed)},
		"manifests-pass": {pass, failWith(noGoMod), failWith(noEnvSchema), failWith(noSecrets), failWith(noSeed)},
		"secret-fail": {pass, failWith(noGoMod), pass,
			failWith(finding{"k8s/base/cp-secrets.example.yaml:7", "secret TOY_SECRET is not declared in deploy/env.schema.json"},
				finding{"k8s/base/cp-secrets.example.yaml:7", "secret TOY_SECRET has a value-looking string in a tracked file, not the REPLACE_ME placeholder"}),
			failWith(noSeed)},
		"secret-pass": {pass, failWith(noGoMod), pass, pass, failWith(noSeed)},
		"seed-fail": {pass, failWith(noGoMod), failWith(noEnvSchema), failWith(noSecrets),
			failWith(finding{"README.md:4", "examples/missing-example is named as a scenario set but has no argus-config*.yaml under it"})},
		"seed-pass": {pass, failWith(noGoMod), failWith(noEnvSchema), failWith(noSecrets), pass},
	}
	names := []string{clauseManifests, clauseLockfiles, clauseEnvSchema, clauseSecretSchema, clauseSeedData}
	for fixture, want := range cases {
		rep := Check("testdata/" + fixture)
		if len(rep.Clauses) != len(want) {
			t.Errorf("%s: %d clauses, want %d", fixture, len(rep.Clauses), len(want))
			continue
		}
		for i, w := range want {
			got := rep.Clauses[i]
			if got.Name != names[i] || got.Status != w.status {
				t.Errorf("%s: clause %d = %s %s, want %s %s (findings %+v)", fixture, i, got.Name, got.Status, names[i], w.status, got.Findings)
				continue
			}
			if len(got.Findings) != len(w.findings) {
				t.Errorf("%s/%s: %d findings, want %d: %+v", fixture, got.Name, len(got.Findings), len(w.findings), got.Findings)
				continue
			}
			for j, f := range w.findings {
				if got.Findings[j].Location != f.loc || !strings.HasPrefix(got.Findings[j].Reason, f.reasonPrefix) {
					t.Errorf("%s/%s finding %d = %+v, want %s %q…", fixture, got.Name, j, got.Findings[j], f.loc, f.reasonPrefix)
				}
			}
		}
	}
}
