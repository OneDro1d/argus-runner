package reporoute

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The edge cases of the OpenAPI 3 server prefix (openapiServerPath), from an adversarial review of #404
// (2026-10-03). A panic is caught and reported as a failure, so one malformed spec yields an observed
// result instead of stopping the walk.
func TestOpenAPI3ServerPrefixEdgeCases(t *testing.T) {
	const oas3 = "openapi: 3.0.3\ninfo: {title: t, version: \"1\"}\n"
	cases := []struct {
		name, file, doc string
		want            []string // "METHOD path", sorted
	}{
		{"R1 absolute url with path", "openapi.yaml", oas3 + `
servers:
  - url: https://api.x.com/api/v2
paths:
  /document: {get: {summary: x}}
`, []string{"GET /api/v2/document"}},
		{"R2 trailing slash", "openapi.yaml", oas3 + `
servers:
  - url: https://api.x.com/api/v2/
paths:
  /document: {get: {summary: x}}
`, []string{"GET /api/v2/document"}},
		{"R3 relative url", "openapi.yaml", oas3 + `
servers:
  - url: /api/v2
paths:
  /document: {get: {summary: x}}
`, []string{"GET /api/v2/document"}},
		{"R4a variables host+version with defaults", "openapi.yaml", oas3 + `
servers:
  - url: "https://{host}/v{version}"
    variables:
      host: {default: api.x.com}
      version: {default: "2", enum: ["1", "2"]}
paths:
  /document: {get: {summary: x}}
`, []string{"GET /v2/document"}},
		{"R4b variable default is a YAML int", "openapi.yaml", oas3 + `
servers:
  - url: "https://{host}/v{version}"
    variables:
      host: {default: api.x.com}
      version: {default: 2}
paths:
  /document: {get: {summary: x}}
`, []string{"GET /v2/document"}},
		// Invalid spec (a Server Variable's default is REQUIRED, OAS 3.0.3 §4.7.6): the PR's documented
		// "no guess" fallback applies. Accepted, not a defect.
		{"R4c host variable without default, literal path", "openapi.yaml", oas3 + `
servers:
  - url: "https://{host}/api/v2"
paths:
  /document: {get: {summary: x}}
`, []string{"GET /document"}},
		{"R5 multiple servers: first wins", "openapi.yaml", oas3 + `
servers:
  - url: https://prod.x.com/api/v2
  - url: https://staging.x.com/api/v9
  - url: http://localhost:3000
paths:
  /document: {get: {summary: x}}
`, []string{"GET /api/v2/document"}},
		{"R6a path-level servers override", "openapi.yaml", oas3 + `
servers:
  - url: https://api.x.com/api/v2
paths:
  /document: {get: {summary: x}}
  /upload:
    servers:
      - url: https://files.x.com/files
    post: {summary: x}
`, []string{"GET /api/v2/document", "POST /files/upload"}},
		{"R6b operation-level servers override", "openapi.yaml", oas3 + `
servers:
  - url: https://api.x.com/api/v2
paths:
  /document:
    get:
      summary: x
      servers:
        - url: https://legacy.x.com/v1
`, []string{"GET /v1/document"}},
		{"R7a empty servers list", "openapi.yaml", oas3 + `
servers: []
paths:
  /document: {get: {summary: x}}
`, []string{"GET /document"}},
		{"R7b server without url", "openapi.yaml", oas3 + `
servers:
  - description: no url
paths:
  /document: {get: {summary: x}}
`, []string{"GET /document"}},
		{"R7c servers null", "openapi.yaml", oas3 + `
servers:
paths:
  /document: {get: {summary: x}}
`, []string{"GET /document"}},
		{"R8a swagger2 basePath unaffected", "swagger.yaml", `swagger: "2.0"
info: {title: t, version: "1"}
host: api.x.com
basePath: /v1/
paths:
  /widgets: {get: {summary: x}}
`, []string{"GET /v1/widgets"}},
		{"R8b swagger2 no basePath unaffected", "swagger.yaml", `swagger: "2.0"
info: {title: t, version: "1"}
host: api.x.com
paths:
  /widgets: {get: {summary: x}}
`, []string{"GET /widgets"}},
		// The spec says the path is APPENDED to the server URL (OAS 3.0.3 §4.7.8), so a spec that also writes
		// the prefix into every path really does describe /api/v2/api/v2/... Pinned as written, no heuristic:
		// it is almost always an authoring mistake, and the doubled route in the proposal makes it visible.
		{"R9 route already carries the prefix: appended as the spec says", "openapi.yaml", oas3 + `
servers:
  - url: http://localhost:3000/api/v2
paths:
  /api/v2/document: {get: {summary: x}}
  /api/v2/document/{id}: {get: {summary: x}}
`, []string{"GET /api/v2/api/v2/document", "GET /api/v2/api/v2/document/{id}"}},
		{"X1 scalar server variable (HoP B1) must not panic", "openapi.yaml", oas3 + `
servers:
  - url: "https://api.x.com/{base}"
    variables:
      base: v3
paths:
  /document: {get: {summary: x}}
`, []string{"GET /document"}},
		{"X2 null server variable must not panic", "openapi.yaml", oas3 + `
servers:
  - url: "https://api.x.com/{base}"
    variables:
      base:
paths:
  /document: {get: {summary: x}}
`, []string{"GET /document"}},
		{"X3 relative url without leading slash (HoP N1)", "openapi.yaml", oas3 + `
servers:
  - url: v1
paths:
  /document: {get: {summary: x}}
`, []string{"GET /v1/document"}},
		{"X4 JSON spec (Documenso shape)", "openapi.json", `{"openapi":"3.0.3","info":{"title":"t","version":"1"},
"servers":[{"url":"http://localhost:3000/api/v2"}],
"paths":{"/document":{"get":{"summary":"x"}},"/document/{documentId}":{"get":{"summary":"x"}}}}`,
			[]string{"GET /api/v2/document", "GET /api/v2/document/{documentId}"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.WriteFile(filepath.Join(repo, tc.file), []byte(tc.doc), 0o644); err != nil {
				t.Fatal(err)
			}
			var got []string
			func() {
				defer func() {
					if r := recover(); r != nil {
						got = []string{fmt.Sprintf("PANIC: %v", r)}
					}
				}()
				rep, err := Run(Options{Repo: repo, Out: t.TempDir()})
				if err != nil {
					got = []string{"ERR: " + err.Error()}
					return
				}
				for _, r := range rep.Routes {
					got = append(got, r.Method+" "+r.Path)
				}
			}()
			sort.Strings(got)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("OBSERVED %q, WANT %q", got, tc.want)
			} else {
				t.Logf("OBSERVED %q", got)
			}
		})
	}
}
