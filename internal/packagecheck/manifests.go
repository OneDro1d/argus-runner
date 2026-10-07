package packagecheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// clauseManifests names the check the CLI and the report agree on.
const clauseManifests = "manifests"

// manifestsClause reads every manifest the declaration names — a directory recursively for its
// YAML files (the conventional k8s/ and deploy/compose/, or a declared kustomize overlay), an
// explicit file as-is — and requires each container image to be pinned by digest (@sha256:...),
// never by tag — the F4 rule that a third party can rebuild exactly what was tested. It covers two
// surfaces:
//   - a plain "image:" scalar field (a container, initContainer, or compose service);
//   - a kustomize "images:" transformer entry (name/newName + either newTag or digest).
//
// A tag-pinned entry is an offender named at its own file:line; an entry pinned by digest is not.
// A declared path that is not there is an offender too: the declaration promised manifests here.
func manifestsClause(root string, d Declaration) Clause {
	var findings []Finding

	for _, rel := range d.Manifests {
		full := under(root, rel)
		info, err := os.Stat(full)
		if err != nil {
			findings = append(findings, Finding{Location: rel + ":0", Reason: "declared manifest path does not exist" + d.where("manifests")})
			continue
		}
		if !info.IsDir() {
			findings = append(findings, scanImagesInFile(full, root)...)
			continue
		}
		files, err := yamlFilesUnder(full)
		if err != nil {
			findings = append(findings, Finding{Location: rel + ":0", Reason: fmt.Sprintf("cannot read manifest path: %v", err)})
			continue
		}
		for _, f := range files {
			findings = append(findings, scanImagesInFile(f, root)...)
		}
	}

	return fail(clauseManifests, findings)
}

// yamlFilesUnder lists every .yaml/.yml file under dir, recursively.
func yamlFilesUnder(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(path)); ext == ".yaml" || ext == ".yml" {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// scanImagesInFile decodes every YAML document in file and walks it for image references.
// A file that does not parse as YAML (e.g. a template with unresolved Go text/template syntax)
// is skipped rather than reported — this clause checks image pinning, not YAML validity.
func scanImagesInFile(file, root string) []Finding {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	rel, err := filepath.Rel(root, file)
	if err != nil {
		rel = file
	}

	var findings []Finding
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			break // EOF or a document this decoder cannot parse; nothing further to walk
		}
		walkImages(&node, rel, &findings)
	}
	return findings
}

// walkImages recurses through a YAML node tree looking for two shapes: a scalar "image:" field,
// and a kustomize "images:" transformer list.
func walkImages(n *yaml.Node, rel string, findings *[]Finding) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			walkImages(c, rel, findings)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			switch {
			case key.Value == "image" && val.Kind == yaml.ScalarNode:
				checkImageRef(val.Value, rel, val.Line, findings)
			case key.Value == "images" && val.Kind == yaml.SequenceNode:
				for _, item := range val.Content {
					checkKustomizeImageEntry(item, rel, findings)
				}
			default:
				walkImages(val, rel, findings)
			}
		}
	}
}

// checkImageRef records an offender when ref is not pinned by digest.
func checkImageRef(ref, rel string, line int, findings *[]Finding) {
	if strings.Contains(ref, "@sha256:") {
		return
	}
	*findings = append(*findings, Finding{
		Location: findingLocation(rel, line),
		Reason:   fmt.Sprintf("image pinned by tag, not digest: %s", ref),
	})
}

// checkKustomizeImageEntry handles one item of a kustomize "images:" transformer list: a map with
// "name", optionally "newName", and either "newTag" (a tag, an offender) or "digest" (fine).
func checkKustomizeImageEntry(item *yaml.Node, rel string, findings *[]Finding) {
	if item.Kind != yaml.MappingNode {
		return
	}
	var name, newName, newTag string
	var digestSeen bool
	var tagLine, nameLine int
	for i := 0; i+1 < len(item.Content); i += 2 {
		key, val := item.Content[i], item.Content[i+1]
		switch key.Value {
		case "name":
			name = val.Value
			nameLine = key.Line
		case "newName":
			newName = val.Value
		case "newTag":
			newTag = val.Value
			tagLine = key.Line
		case "digest":
			if strings.TrimSpace(val.Value) != "" {
				digestSeen = true
			}
		}
	}
	if digestSeen {
		return
	}
	ref := name
	if newName != "" {
		ref = newName
	}
	line := tagLine
	if line == 0 {
		line = nameLine
	}
	*findings = append(*findings, Finding{
		Location: findingLocation(rel, line),
		Reason:   fmt.Sprintf("image pinned by tag, not digest: %s newTag=%s", ref, newTag),
	})
}
