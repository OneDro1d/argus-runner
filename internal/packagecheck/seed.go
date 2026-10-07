package packagecheck

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// clauseSeedData names the check the CLI and the report agree on.
const clauseSeedData = "seed data"

// treeLineRe matches README.md's directory-tree line for examples/, e.g.:
//
//	├── examples/                # order-service (codebase/ + demo-packaging/), memstore, social
//
// the comment after "#" is a comma-separated list of the example names the README promises.
var treeLineRe = regexp.MustCompile(`examples/\s*#\s*(.+)$`)

// seedDataClause requires every example the declared seed document names to have seed data: at
// least one argus-config*.yaml somewhere under examples/<name>/. A name the document lists with no
// config anywhere under its directory is an offender at the document's own line. The conventional
// document is README.md; a deployment that keeps the line elsewhere declares it as package.seed.
func seedDataClause(root string, d Declaration) Clause {
	if d.Seed == "" {
		return fail(clauseSeedData, []Finding{{Location: "seed:0", Reason: "no seed data document declared (package.seed)"}})
	}
	names, line, err := namedExamples(root, d.Seed)
	if err != nil {
		return fail(clauseSeedData, []Finding{{Location: d.Seed + ":0", Reason: err.Error()}})
	}
	if names == nil {
		reason := "no examples/ tree line found to name the scenario sets seed data must exist for"
		if d.Origin == OriginDeclared && !exists(root, d.Seed) {
			reason = "no seed data document at " + d.Seed + d.where("seed")
		}
		return fail(clauseSeedData, []Finding{{Location: d.Seed + ":0", Reason: reason}})
	}

	var findings []Finding
	for _, name := range names {
		dir := filepath.Join(root, "examples", name)
		if !hasArgusConfig(dir) {
			findings = append(findings, Finding{
				Location: fmt.Sprintf("%s:%d", d.Seed, line),
				Reason:   fmt.Sprintf("examples/%s is named as a scenario set but has no argus-config*.yaml under it", name),
			})
		}
	}
	return fail(clauseSeedData, findings)
}

// namedExamples reads the seed document at root/rel and returns the example names the examples/
// tree-line comment lists, plus that line's number.
func namedExamples(root, rel string) ([]string, int, error) {
	f, err := os.Open(under(root, rel))
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	lineNo := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lineNo++
		m := treeLineRe.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		var names []string
		for _, part := range strings.Split(m[1], ",") {
			part = strings.TrimSpace(part)
			fields := strings.Fields(part)
			if len(fields) == 0 {
				continue
			}
			names = append(names, fields[0])
		}
		return names, lineNo, nil
	}
	return nil, 0, sc.Err()
}

// hasArgusConfig reports whether dir contains an argus-config*.yaml anywhere under it.
func hasArgusConfig(dir string) bool {
	found := false
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, "argus-config") && strings.HasSuffix(name, ".yaml") {
			found = true
		}
		return nil
	})
	return found
}
