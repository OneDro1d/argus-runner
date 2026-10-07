package reporoute

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// openapiMethods are the HTTP verbs OpenAPI 3.x / Swagger 2.0 allow as path-item keys.
var openapiMethods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true, "delete": true, "head": true, "options": true,
}

// handleMaybeOpenAPI reads p (already known to be .yaml/.yml/.json) and, when it carries a
// top-level `openapi:` or `swagger:` key (spec item 2a), extracts its routes. Anything else with
// that extension is silently not an OpenAPI document and is skipped without comment — only a file
// that LOOKS like one and then fails to parse is reported as unparseable.
func handleMaybeOpenAPI(p, rel string, rep *Report) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var doc map[string]any
	if uerr := yaml.Unmarshal(raw, &doc); uerr != nil {
		// Only report as unparseable when the file at least LOOKS like it was meant to be an
		// OpenAPI/Swagger document (cheap textual pre-check) — otherwise every malformed JSON/YAML
		// fixture in a repo would show up as a false "could not parse" against this extractor.
		if looksLikeOpenAPIText(raw) {
			rep.UnparseableFiles = append(rep.UnparseableFiles, UnparseableFile{File: rel, Reason: "openapi/swagger: " + uerr.Error()})
		}
		return
	}
	if !isOpenAPIDoc(doc) {
		return
	}
	extractOpenAPIRoutes(doc, rel, rep)
}

func looksLikeOpenAPIText(raw []byte) bool {
	s := string(raw)
	return strings.Contains(s, "openapi:") || strings.Contains(s, "\"openapi\"") ||
		strings.Contains(s, "swagger:") || strings.Contains(s, "\"swagger\"")
}

func isOpenAPIDoc(doc map[string]any) bool {
	_, hasOpenAPI := doc["openapi"]
	_, hasSwagger := doc["swagger"]
	return hasOpenAPI || hasSwagger
}

func extractOpenAPIRoutes(doc map[string]any, rel string, rep *Report) {
	pathsAny, ok := doc["paths"]
	if !ok {
		return
	}
	paths, ok := pathsAny.(map[string]any)
	if !ok {
		return
	}
	// Every `paths` entry is relative to a prefix: Swagger 2.0's basePath, or the PATH of OpenAPI
	// 3.x's server URL ("the path is appended to the URL from the Server Object", OAS 3.0.3 §4.7.8).
	// Dropping the 3.x one proposed `GET /document` for an app that serves `/api/v2/document`.
	// A path item's or an operation's own `servers` replaces the root one (OAS 3.0.3 §4.7.9, §4.7.10).
	basePath := ""
	bp, swagger2 := doc["basePath"].(string)
	if swagger2 {
		basePath = strings.TrimSuffix(bp, "/")
	} else {
		basePath = openapiServerPath(doc["servers"])
	}
	// Deterministic order regardless of map iteration.
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, rawPath := range keys {
		itemAny := paths[rawPath]
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		itemBase := basePath
		if s, ok := item["servers"].([]any); ok && len(s) > 0 && !swagger2 {
			itemBase = openapiServerPath(s)
		}
		methodKeys := make([]string, 0, len(item))
		for k := range item {
			methodKeys = append(methodKeys, k)
		}
		sort.Strings(methodKeys)
		for _, mk := range methodKeys {
			lower := strings.ToLower(mk)
			if !openapiMethods[lower] {
				continue
			}
			path := itemBase + rawPath
			if op, ok := item[mk].(map[string]any); ok && !swagger2 {
				if s, ok := op["servers"].([]any); ok && len(s) > 0 {
					path = openapiServerPath(s) + rawPath
				}
			}
			rep.Routes = append(rep.Routes, Route{
				Method:     strings.ToUpper(lower),
				Path:       path,
				File:       rel,
				Line:       1, // OpenAPI documents are data, not line-addressable source in any useful sense
				Extractor:  "openapi",
				ParamNames: openapiPathParams(path),
			})
		}
	}
}

// openapiPathParams extracts `{name}` path parameters, OpenAPI/Swagger's own placeholder syntax.
func openapiPathParams(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") && len(seg) > 2 {
			out = append(out, seg[1:len(seg)-1])
		}
	}
	return out
}

// openapiServerPath is the path of the FIRST server URL (the default a client uses), with server
// variables replaced by their defaults and no trailing slash: "http://localhost:3000/api/v2" →
// "/api/v2", "/api/v1" → "/api/v1", a host-only URL → "". A variable with no default cannot be
// resolved, and then there is no prefix rather than a guessed one.
func openapiServerPath(serversAny any) string {
	servers, _ := serversAny.([]any)
	if len(servers) == 0 {
		return ""
	}
	server, _ := servers[0].(map[string]any)
	raw, _ := server["url"].(string)
	vars, _ := server["variables"].(map[string]any)
	for name, v := range vars {
		// A malformed variable (a scalar or null instead of a Server Variable Object) stays
		// unresolved and so gives no prefix below; it must not panic the whole walk.
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if def, ok := m["default"]; ok {
			raw = strings.ReplaceAll(raw, "{"+name+"}", fmt.Sprint(def))
		}
	}
	if strings.ContainsAny(raw, "{}") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	p := strings.TrimSuffix(u.Path, "/")
	// A relative URL like "v1" still names a path under the root, not a route without a leading slash.
	if p != "" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}
