package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ── package (AC-36) ──────────────────────────────────────────────────────────
//
// The top-level `package:` block declares the package `argus package-check --config` certifies:
// the manifests the deployment under test applies, the lockfiles its builds pin, and the three
// files the schema and seed clauses read. Without it the check reads the whole repository at
// Argus's own conventional paths — which fails every app on overlays it never deploys and on files
// it has no reason to keep where Argus keeps them.
//
// Every path is relative to the config file's directory, must stay under it, and must EXIST at
// load time: validate-config loads the file, so a missing declared path is refused there — by key
// and path — before anything runs. Unknown keys under `package:` are refused by name at load, the
// rule `targets:` and `rate_limit:` follow: a key that parses and reaches nothing looks supported.

// Package is the top-level `package:` block of an argus-config.yaml. Optional; when present it is
// complete or refused.
type Package struct {
	// Manifests are directories (scanned recursively for YAML — a kustomize overlay directory is
	// the common case) or explicit manifest files. At least one.
	Manifests []string `yaml:"manifests"`
	// Lockfiles are the lockfiles the deployment's builds pin (go.sum, ui/package-lock.json, …). At least one.
	Lockfiles []string `yaml:"lockfiles"`
	// EnvSchema is the env schema file: a JSON list of {name, required, secret, description}.
	EnvSchema string `yaml:"env_schema"`
	// SecretsExample is the Secret manifest documenting every secret key by name with a placeholder.
	SecretsExample string `yaml:"secrets_example"`
	// Seed is the document (a README-style file) carrying the examples/ tree line that names the
	// scenario sets seed data must exist for.
	Seed string `yaml:"seed"`
}

// validate applies the path rules to every declared path, relative to root (the config file's
// directory), and stores each path clean and forward-slashed so the checker and its report name it
// the way it was read. A nil block (none declared) is fine.
func (p *Package) validate(root string) error {
	if p == nil {
		return nil
	}
	var errs []string
	list := func(key string, paths []string) {
		if len(paths) == 0 {
			errs = append(errs, fmt.Sprintf("package.%s: declare at least one path", key))
			return
		}
		for i := range paths {
			clean, e := declaredPath(root, fmt.Sprintf("%s[%d]", key, i), paths[i])
			if e != "" {
				errs = append(errs, e)
				continue
			}
			paths[i] = clean
		}
	}
	one := func(key string, v *string) {
		if strings.TrimSpace(*v) == "" {
			errs = append(errs, fmt.Sprintf("package.%s is required", key))
			return
		}
		clean, e := declaredPath(root, key, *v)
		if e != "" {
			errs = append(errs, e)
			return
		}
		*v = clean
	}
	list("manifests", p.Manifests)
	list("lockfiles", p.Lockfiles)
	one("env_schema", &p.EnvSchema)
	one("secrets_example", &p.SecretsExample)
	one("seed", &p.Seed)
	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}

// declaredPath checks one declared path under key: relative, never escaping root, and present. It
// returns the clean forward-slashed path, or the refusal naming the key and the path.
func declaredPath(root, key, v string) (string, string) {
	label := "package." + key
	if strings.TrimSpace(v) == "" {
		return "", fmt.Sprintf("%s: empty path", label)
	}
	if filepath.IsAbs(v) || strings.HasPrefix(v, "/") || strings.HasPrefix(v, `\`) {
		return "", fmt.Sprintf("%s: %q is absolute — declare it relative to the config file's directory", label, v)
	}
	clean := path.Clean(filepath.ToSlash(v))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Sprintf("%s: %q escapes the config file's directory", label, v)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(clean))); err != nil {
		return "", fmt.Sprintf("%s: %q does not exist under %s", label, v, root)
	}
	return clean, ""
}
