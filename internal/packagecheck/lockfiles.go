package packagecheck

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
)

// clauseLockfiles names the check the CLI and the report agree on.
const clauseLockfiles = "lockfiles"

// lockfilesClause checks every lockfile the declaration names, by what it is:
//   - a go.sum must be consistent with the go.mod beside it (offline: no `go list -m all`, so this
//     compares text, as AC-9 specifies);
//   - a package-lock.json must be present with a top-level version matching the package.json beside it;
//   - any other lockfile must be present and non-empty.
//
// The conventional declaration names go.sum and, when a ui/ tree exists, ui/package-lock.json.
func lockfilesClause(root string, d Declaration) Clause {
	var findings []Finding
	for _, rel := range d.Lockfiles {
		switch path.Base(rel) {
		case "go.sum":
			findings = append(findings, checkGoLockfile(root, rel, d)...)
		case "package-lock.json":
			findings = append(findings, checkNPMLockfile(root, rel, d)...)
		default:
			findings = append(findings, checkLockfilePresent(root, rel, d)...)
		}
	}
	return fail(clauseLockfiles, findings)
}

// checkGoLockfile checks the declared go.sum (sumRel) against the go.mod beside it: every require
// must have an entry. A module with no third-party requires needs no go.sum at all (`go mod tidy`
// removes an empty one), so the go.mod is the authority on whether the sum must exist.
func checkGoLockfile(root, sumRel string, d Declaration) []Finding {
	modRel := path.Join(path.Dir(sumRel), "go.mod")

	requires, err := goModRequires(under(root, modRel))
	if err != nil {
		return []Finding{{Location: modRel + ":0", Reason: fmt.Sprintf("cannot read go.mod: %v", err) + d.where("lockfiles")}}
	}
	if len(requires) == 0 {
		return nil // no third-party requires, nothing for go.sum to cover
	}

	sumSet, err := goSumEntries(under(root, sumRel))
	if err != nil {
		return []Finding{{Location: sumRel + ":0", Reason: fmt.Sprintf("%s missing or unreadable: %v", sumRel, err) + d.where("lockfiles")}}
	}

	var findings []Finding
	for _, r := range requires {
		if !sumSet[r.module+"@"+r.version] {
			findings = append(findings, Finding{
				Location: fmt.Sprintf("%s:%d", modRel, r.line),
				Reason:   fmt.Sprintf("%s has no entry for %s require %s %s", sumRel, modRel, r.module, r.version),
			})
		}
	}
	return findings
}

type goRequire struct {
	module, version string
	line            int
}

// goModRequires collects every "module version" pair named by a require directive, single-line
// or block form, with the source line each was found on.
func goModRequires(path string) ([]goRequire, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []goRequire
	inBlock := false
	lineNo := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		switch {
		case inBlock:
			if trimmed == ")" {
				inBlock = false
				continue
			}
			if mod, ver, ok := parseModVersion(trimmed); ok {
				out = append(out, goRequire{module: mod, version: ver, line: lineNo})
			}
		case strings.HasPrefix(trimmed, "require ("):
			inBlock = true
		case strings.HasPrefix(trimmed, "require "):
			if mod, ver, ok := parseModVersion(strings.TrimPrefix(trimmed, "require ")); ok {
				out = append(out, goRequire{module: mod, version: ver, line: lineNo})
			}
		}
	}
	return out, sc.Err()
}

// parseModVersion reads "module version [// indirect]" and returns module, version.
func parseModVersion(s string) (string, string, bool) {
	if i := strings.Index(s, "//"); i >= 0 {
		s = s[:i]
	}
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return "", "", false
	}
	return fields[0], fields[1], true
}

// goSumEntries returns the set of "module@version" pairs go.sum attests to, from either the
// h1: line or the /go.mod h1: line — either is enough to call a require "covered".
func goSumEntries(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		mod, ver := fields[0], fields[1]
		ver = strings.TrimSuffix(ver, "/go.mod")
		out[mod+"@"+ver] = true
	}
	return out, sc.Err()
}

// checkNPMLockfile requires the declared package-lock.json (lockRel) to exist with a top-level
// "version" equal to the "version" of the package.json beside it — the two drifting is exactly
// what a stale lockfile looks like.
func checkNPMLockfile(root, lockRel string, d Declaration) []Finding {
	pkgRel := path.Join(path.Dir(lockRel), "package.json")

	pkgVersion, err := jsonStringField(under(root, pkgRel), "version")
	if err != nil {
		return []Finding{{Location: pkgRel + ":0", Reason: fmt.Sprintf("cannot read version: %v", err) + d.where("lockfiles")}}
	}

	lockVersion, err := jsonStringField(under(root, lockRel), "version")
	if err != nil {
		return []Finding{{Location: lockRel + ":0", Reason: fmt.Sprintf("package-lock.json missing or unreadable: %v", err) + d.where("lockfiles")}}
	}

	if pkgVersion != lockVersion {
		return []Finding{{
			Location: lockRel + ":0",
			Reason:   fmt.Sprintf("top-level version %q does not match %s version %q", lockVersion, pkgRel, pkgVersion),
		}}
	}
	return nil
}

// checkLockfilePresent is the check for a lockfile this package does not read the inside of
// (yarn.lock, pnpm-lock.yaml, poetry.lock, Cargo.lock, …): it must be there and carry something.
func checkLockfilePresent(root, rel string, d Declaration) []Finding {
	info, err := os.Stat(under(root, rel))
	if err != nil {
		return []Finding{{Location: rel + ":0", Reason: fmt.Sprintf("declared lockfile %s does not exist", rel) + d.where("lockfiles")}}
	}
	if info.IsDir() || info.Size() == 0 {
		return []Finding{{Location: rel + ":0", Reason: fmt.Sprintf("declared lockfile %s is empty", rel) + d.where("lockfiles")}}
	}
	return nil
}

func jsonStringField(path, field string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	v, ok := doc[field].(string)
	if !ok {
		return "", fmt.Errorf("field %q not a string", field)
	}
	return v, nil
}
