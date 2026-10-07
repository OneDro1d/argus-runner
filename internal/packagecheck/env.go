package packagecheck

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// clauseEnvSchema names the check the CLI and the report agree on.
const clauseEnvSchema = "env schema"

// getenvRe matches a literal os.Getenv("NAME") or atoiEnv("NAME") call — the two ways this repo's
// control plane and runner read a named environment variable by a fixed, literal key. A name
// reached only through a variable (one further level of indirection, e.g. a package constant) is
// a known limitation of a static text scan and is not found here.
var getenvRe = regexp.MustCompile(`\b(?:os\.Getenv|atoiEnv)\("([A-Za-z_][A-Za-z0-9_]*)"\)`)

// envSchemaClause requires the declared env schema to exist and parse, and every ${VAR}-style read
// this repo's control plane and runner do — scoped, precisely, to a literal os.Getenv("NAME")/
// atoiEnv("NAME") call in non-test Go source under cmd/ and internal/ — to be declared in it. An
// undeclared read is an offender named at its own file:line; a missing schema file is one finding
// naming the path (the convention, or the key the deployment declared it under).
func envSchemaClause(root string, d Declaration) Clause {
	if d.EnvSchema == "" {
		return fail(clauseEnvSchema, []Finding{{Location: "env_schema:0", Reason: "no env schema declared (package.env_schema)"}})
	}
	schema, err := loadEnvSchema(root, d.EnvSchema)
	if err != nil {
		return fail(clauseEnvSchema, []Finding{{Location: d.EnvSchema + ":0", Reason: err.Error()}})
	}
	if schema == nil {
		return fail(clauseEnvSchema, []Finding{{
			Location: d.EnvSchema + ":0",
			Reason:   missingEnvSchemaReason(d),
		}})
	}

	var findings []Finding
	for _, dir := range []string{"cmd", "internal"} {
		files, err := goSourceFilesUnder(filepath.Join(root, dir))
		if err != nil {
			continue
		}
		for _, f := range files {
			findings = append(findings, scanGetenvInFile(f, root, schema, d.EnvSchema)...)
		}
	}
	return fail(clauseEnvSchema, findings)
}

// missingEnvSchemaReason words the absent-schema finding for its origin: the conventional path is
// the one this package defines when none exists; a declared one is the deployment's own promise.
func missingEnvSchemaReason(d Declaration) string {
	if d.Origin == OriginDeclared {
		return "no env schema file at " + d.EnvSchema + d.where("env_schema")
	}
	return "no env schema file at " + d.EnvSchema + " (the convention this package defines when none exists)"
}

// goSourceFilesUnder lists every non-test .go file under dir, recursively, skipping testdata and
// this package's own tree — packagecheck is the checker, not part of the control plane/runner
// contract it verifies, and its doc comments legitimately spell out the literal call shape this
// scan looks for.
func goSourceFilesUnder(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "packagecheck" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// scanGetenvInFile finds every literal os.Getenv/atoiEnv call in file and reports the ones whose
// name is not a key of schema (read from schemaRel, named in the reason).
func scanGetenvInFile(file, root string, schema map[string]SchemaEntry, schemaRel string) []Finding {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	rel, err := filepath.Rel(root, file)
	if err != nil {
		rel = file
	}

	var findings []Finding
	lineNo := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lineNo++
		for _, m := range getenvRe.FindAllStringSubmatch(sc.Text(), -1) {
			name := m[1]
			if _, declared := schema[name]; !declared {
				findings = append(findings, Finding{
					Location: findingLocation(rel, lineNo),
					Reason:   fmt.Sprintf("environment variable %s is read but not declared in %s", name, schemaRel),
				})
			}
		}
	}
	return findings
}
