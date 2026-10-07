package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC-36: the optional top-level `package:` block declares the package `argus package-check --config`
// certifies. Every path is relative to the config file's directory, must stay under it, and must
// exist at load time — validate-config loads the file, so a missing declared path is refused there,
// by key and path, before anything runs.

// writePackageTree writes a config with a package: block and every file it declares, returning the
// config path. A caller removes files to test the refusals.
func writePackageTree(t *testing.T, block string) string {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range []string{
		"k8s/overlays/prod/app.yaml",
		"go.sum",
		"ui/package-lock.json",
		"deploy/env.schema.json",
		"k8s/base/secrets.example.yaml",
		"README.md",
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	body := "project:\n  name: t\ntargets:\n  http:\n    base_url: http://api:8080\n" + block
	p := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const packageBlock = `package:
  manifests:
    - k8s/overlays/prod
  lockfiles:
    - go.sum
    - ui/package-lock.json
  env_schema: deploy/env.schema.json
  secrets_example: k8s/base/secrets.example.yaml
  seed: README.md
`

func TestPackage_ValidBlockRoundTrips(t *testing.T) {
	c, err := Load(writePackageTree(t, packageBlock))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Package == nil {
		t.Fatal("package: block was declared but Package is nil")
	}
	p := c.Package
	if strings.Join(p.Manifests, ",") != "k8s/overlays/prod" || strings.Join(p.Lockfiles, ",") != "go.sum,ui/package-lock.json" ||
		p.EnvSchema != "deploy/env.schema.json" || p.SecretsExample != "k8s/base/secrets.example.yaml" || p.Seed != "README.md" {
		t.Errorf("package block did not round-trip: %+v", *p)
	}
}

func TestPackage_AbsentBlockIsNil(t *testing.T) {
	c, err := Load(writePackageTree(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Package != nil {
		t.Errorf("no package: block was declared, want nil, got %+v", *c.Package)
	}
}

// A declared path that is not there is refused at load — by BOTH loaders — naming the key and the path.
func TestPackage_MissingDeclaredPathRefusedByKeyAndPath(t *testing.T) {
	cfg := writePackageTree(t, packageBlock)
	if err := os.Remove(filepath.Join(filepath.Dir(cfg), "ui", "package-lock.json")); err != nil {
		t.Fatal(err)
	}
	_, err := Load(cfg)
	if err == nil {
		t.Fatal("a declared lockfile that does not exist was accepted in silence")
	}
	for _, want := range []string{"package.lockfiles[1]", "ui/package-lock.json", "does not exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name the key and the path; missing %q in: %s", want, err)
		}
	}
	if _, perr := ParseUnresolved(cfg); perr == nil {
		t.Error("ParseUnresolved (the secrets-preflight loader) must refuse the same missing path")
	}
}

func TestPackage_EveryKeyIsCheckedForExistence(t *testing.T) {
	for _, tc := range []struct{ remove, key string }{
		{"k8s/overlays/prod/app.yaml", "package.manifests[0]"}, // removing the only file leaves the directory: remove the directory below
		{"deploy/env.schema.json", "package.env_schema"},
		{"k8s/base/secrets.example.yaml", "package.secrets_example"},
		{"README.md", "package.seed"},
	} {
		cfg := writePackageTree(t, packageBlock)
		target := filepath.Join(filepath.Dir(cfg), filepath.FromSlash(tc.remove))
		if tc.key == "package.manifests[0]" {
			target = filepath.Join(filepath.Dir(cfg), "k8s", "overlays", "prod")
		}
		if err := os.RemoveAll(target); err != nil {
			t.Fatal(err)
		}
		_, err := Load(cfg)
		if err == nil || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s absent: want a refusal naming %s, got: %v", tc.remove, tc.key, err)
		}
	}
}

// A path may not leave the config file's directory and may not be absolute.
func TestPackage_PathEscapingTheRootRefused(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"../outside/env.schema.json", "escapes"},
		{"/etc/env.schema.json", "absolute"},
	} {
		block := strings.Replace(packageBlock, "env_schema: deploy/env.schema.json", "env_schema: "+tc.value, 1)
		_, err := Load(writePackageTree(t, block))
		if err == nil || !strings.Contains(err.Error(), "package.env_schema") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("env_schema %q: want a refusal naming package.env_schema and %q, got: %v", tc.value, tc.want, err)
		}
	}
}

// A declared package is complete or refused: every key is required, and the two lists need at
// least one entry — a package with no manifests certifies nothing.
func TestPackage_IncompleteBlockRefusedByKey(t *testing.T) {
	for _, tc := range []struct{ drop, key string }{
		{"  seed: README.md\n", "package.seed"},
		{"  env_schema: deploy/env.schema.json\n", "package.env_schema"},
		{"  secrets_example: k8s/base/secrets.example.yaml\n", "package.secrets_example"},
		{"  manifests:\n    - k8s/overlays/prod\n", "package.manifests"},
		{"  lockfiles:\n    - go.sum\n    - ui/package-lock.json\n", "package.lockfiles"},
	} {
		block := strings.Replace(packageBlock, tc.drop, "", 1)
		if block == packageBlock {
			t.Fatalf("test bug: %q not found in the block", tc.drop)
		}
		_, err := Load(writePackageTree(t, block))
		if err == nil || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("without %s: want a refusal naming it, got: %v", tc.key, err)
		}
	}
}

// Unknown keys under package: are refused by name with the accepted keys — the rule targets: and
// rate_limit: already follow; a key that parses and reaches nothing looks supported.
func TestPackage_UnknownKeyRefusedByName(t *testing.T) {
	block := strings.Replace(packageBlock, "  manifests:", "  manifest:", 1)
	_, err := Load(writePackageTree(t, block))
	if err == nil {
		t.Fatal("an unknown key under package: (manifest) was accepted in silence")
	}
	for _, want := range []string{`"manifest"`, "package", "manifests", "lockfiles", "env_schema", "secrets_example", "seed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name the key and list the accepted keys; missing %q in: %s", want, err)
		}
	}
}

// A path written with a leading ./ is stored clean, so the report and the findings name it the way
// the checker reads it.
func TestPackage_PathsAreStoredClean(t *testing.T) {
	block := strings.Replace(packageBlock, "seed: README.md", "seed: ./README.md", 1)
	c, err := Load(writePackageTree(t, block))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Package.Seed != "README.md" {
		t.Errorf("seed = %q, want the clean %q", c.Package.Seed, "README.md")
	}
}
