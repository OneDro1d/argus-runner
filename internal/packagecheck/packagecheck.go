// Package packagecheck is AC-9's static completeness check of a commit (the F4 idea: a commit
// that carries manifests, digests not tags, lockfiles, the env and secret *schemas*, and seed
// data, so a third party can rebuild what was tested). Check is pure: it reads the tree under
// root and returns a Report. No network, no Docker, no mutation of the tree.
//
// AC-36: the check certifies a DECLARED package — the manifests the deployment under test applies,
// the lockfiles its builds pin, and the three files the schema and seed clauses read — not the
// whole tree. CheckDeclared takes that Declaration; Check keeps the whole-root behaviour by
// building the conventional one (DefaultDeclaration) and running the same code path.
package packagecheck

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Status is one of the two clause verdicts. There is no WARN: a clause that would hide a
// failure behind a warning is exactly the gap this package exists to close.
type Status string

const (
	// StatusPass means the clause found no offender.
	StatusPass Status = "PASS"
	// StatusFail means the clause found at least one offender; Findings names every one.
	StatusFail Status = "FAIL"
)

// Finding is one named offender: where it is and why it failed.
type Finding struct {
	// Location is "file:line" relative to the checked root.
	Location string `json:"location"`
	// Reason is a one-line, generic explanation of the failure.
	Reason string `json:"reason"`
}

// findingLocation builds a Finding's "file:line" from a path the check read from disk, always with
// forward slashes. The report is JSON read on any platform, and filepath.Rel returns the OS separator —
// "k8s\base\control.yaml" on Windows — which no reader on another platform, nor a test comparing
// "k8s/base/control.yaml", can match (AC-32). Every Location built from a path goes through here.
func findingLocation(path string, line int) string {
	return fmt.Sprintf("%s:%d", filepath.ToSlash(path), line)
}

// Clause is one named check of the self-contained commit.
type Clause struct {
	Name     string    `json:"name"`
	Status   Status    `json:"status"`
	Findings []Finding `json:"findings,omitempty"`
}

// fail builds a FAIL clause from the offenders found; PASS when the slice is empty.
func fail(name string, findings []Finding) Clause {
	sort.Slice(findings, func(i, j int) bool { return findings[i].Location < findings[j].Location })
	if len(findings) == 0 {
		return Clause{Name: name, Status: StatusPass}
	}
	return Clause{Name: name, Status: StatusFail, Findings: findings}
}

// The two origins a Declaration can have. The report carries the origin so a reader of the reveal
// knows whether the certified package was declared by the deployment under test or is this
// package's whole-root convention.
const (
	// OriginConventional is Check's whole-root default: this repository's own conventional paths.
	OriginConventional = "conventional"
	// OriginDeclared is a package declared by the deployment under test (the package: block of its
	// argus-config.yaml).
	OriginDeclared = "declared"
)

// Declaration names the package the check certifies. Every path is relative to the checked root,
// forward-slashed. A declared path that does not exist at check time is a finding on the clause
// that needed it — never a silent skip.
type Declaration struct {
	// Origin is OriginConventional or OriginDeclared.
	Origin string `json:"origin"`
	// Manifests are the manifest paths the manifests clause scans: a directory is scanned
	// recursively for YAML (a kustomize overlay directory is the common case); an explicit file is
	// read as-is.
	Manifests []string `json:"manifests"`
	// Lockfiles are the lockfiles the deployment's builds pin. A go.sum is checked against the go.mod
	// beside it, a package-lock.json against the package.json beside it; any other lockfile must be
	// present and non-empty.
	Lockfiles []string `json:"lockfiles"`
	// EnvSchema is the env schema file: a JSON list of {name, required, secret, description}.
	EnvSchema string `json:"env_schema"`
	// SecretsExample is the Secret manifest documenting every secret key by name with a placeholder.
	SecretsExample string `json:"secrets_example"`
	// Seed is the document carrying the examples/ tree line the seed clause reads.
	Seed string `json:"seed"`
}

// where says which declaration named a path, for a finding on a path that is not there. A path
// the deployment declared is the operator's to fix, so the finding names its key; the conventional
// default is a location this package defines, and its clauses say so in their own words.
func (d Declaration) where(key string) string {
	if d.Origin == OriginDeclared {
		return " (declared as package." + key + ")"
	}
	return ""
}

// The conventional paths — this repository's own — that the whole-root check has always read.
const (
	conventionalK8sDir     = "k8s"
	conventionalComposeDir = "deploy/compose"
	conventionalGoSum      = "go.sum"
	conventionalUIDir      = "ui"
	conventionalUILockfile = "ui/package-lock.json"
	// envSchemaPath is the convention this package defines when none exists (AC-9): a single JSON
	// list, shared by the env-schema and secret-schema clauses, naming every variable the control
	// plane and runner read.
	envSchemaPath = "deploy/env.schema.json"
	// secretsExamplePath is the concrete artifact AC-9 names for the secret-schema clause: the
	// Secret manifest documenting every key the deployment needs, by name, with a placeholder.
	secretsExamplePath = "k8s/base/cp-secrets.example.yaml"
	// seedDocPath is the document whose examples/ tree line names the scenario sets seed data must
	// exist for.
	seedDocPath = "README.md"
)

// DefaultDeclaration is the package Check certifies when nothing is declared: this repository's
// own conventional paths. The two optional surfaces — a compose tree and a UI lockfile — are
// declared only when present, which is how the whole-root check has always treated them; the rest
// are declared unconditionally, so their absence stays the finding it has always been.
func DefaultDeclaration(root string) Declaration {
	d := Declaration{
		Origin:         OriginConventional,
		Manifests:      []string{}, // serialises as [] — a declaration always says what it scanned, even "nothing"
		Lockfiles:      []string{conventionalGoSum},
		EnvSchema:      envSchemaPath,
		SecretsExample: secretsExamplePath,
		Seed:           seedDocPath,
	}
	for _, dir := range []string{conventionalK8sDir, conventionalComposeDir} {
		if exists(root, dir) {
			d.Manifests = append(d.Manifests, dir)
		}
	}
	if exists(root, conventionalUIDir) {
		d.Lockfiles = append(d.Lockfiles, conventionalUILockfile)
	}
	return d
}

// exists reports whether the forward-slashed rel path is there under root.
func exists(root, rel string) bool {
	_, err := os.Stat(under(root, rel))
	return err == nil
}

// under joins a forward-slashed declared path onto root in the OS's own separators.
func under(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

// Report is the result of a check: the declaration it certified and one Clause per named check,
// in a fixed order.
type Report struct {
	Root        string      `json:"root"`
	Declaration Declaration `json:"declaration"`
	Clauses     []Clause    `json:"clauses"`
}

// Status is FAIL when any clause failed, else PASS.
func (r Report) Status() Status {
	for _, c := range r.Clauses {
		if c.Status == StatusFail {
			return StatusFail
		}
	}
	return StatusPass
}

// Check runs every named clause against the tree at root with the conventional declaration
// (DefaultDeclaration) — the whole-root check this repository certifies itself with. It only
// reads the tree.
func Check(root string) Report {
	return CheckDeclared(root, DefaultDeclaration(root))
}

// CheckDeclared runs every named clause against the package d declares under root and returns
// their reports in a fixed order (manifests, lockfiles, env schema, secret schema, seed data).
// Each clause takes its inputs from the declaration alone. It only reads the tree.
func CheckDeclared(root string, d Declaration) Report {
	return Report{
		Root:        root,
		Declaration: d,
		Clauses: []Clause{
			manifestsClause(root, d),
			lockfilesClause(root, d),
			envSchemaClause(root, d),
			secretSchemaClause(root, d),
			seedDataClause(root, d),
		},
	}
}

// The whole-root form of each clause: the conventional declaration, one clause at a time. These are
// the per-clause entry points the fixture tests exercise; Check itself goes through CheckDeclared.
func checkManifests(root string) Clause    { return manifestsClause(root, DefaultDeclaration(root)) }
func checkLockfiles(root string) Clause    { return lockfilesClause(root, DefaultDeclaration(root)) }
func checkEnvSchema(root string) Clause    { return envSchemaClause(root, DefaultDeclaration(root)) }
func checkSecretSchema(root string) Clause { return secretSchemaClause(root, DefaultDeclaration(root)) }
func checkSeedData(root string) Clause     { return seedDataClause(root, DefaultDeclaration(root)) }
