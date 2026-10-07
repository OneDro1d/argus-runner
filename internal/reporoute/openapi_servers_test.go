package reporoute

import (
	"os"
	"path/filepath"
	"testing"
)

// OpenAPI 3.x: every `paths` entry is relative to the server URL, PATH INCLUDED ("The path is appended
// to the URL from the Server Object in order to construct the full URL", OAS 3.0.3 §4.7.8). Swagger
// 2.0's basePath was already applied (TestSwagger2BasePath); the 3.x equivalent was dropped, so a
// draft for Documenso v2.19.0's runtime spec (servers: [{url: http://localhost:3000/api/v2}]) read
// `GET ${INGESTION_URL}/document`, a path that does not exist on the app
// (tester verify/2026-10-01-documenso-native/propose-from-repo.out).
func TestOpenAPI3ServersPathPrefixesRoutes(t *testing.T) {
	cases := []struct {
		name, servers, want string
	}{
		{"absolute url with a path (Documenso's runtime spec)", `
servers:
  - url: http://localhost:3000/api/v2`, "/api/v2/document"},
		{"relative url", `
servers:
  - url: /api/v1`, "/api/v1/document"},
		{"trailing slash is not doubled", `
servers:
  - url: https://api.example.com/api/v2/`, "/api/v2/document"},
		{"server variables take their defaults", `
servers:
  - url: "{scheme}://api.example.com/{base}"
    variables:
      scheme: {default: https}
      base: {default: v3}`, "/v3/document"},
		{"host only: no prefix", `
servers:
  - url: https://api.example.com`, "/document"},
		{"no servers: no prefix", ``, "/document"},
		// A variable with no default cannot be resolved: no guess, the route stays unprefixed as before.
		{"unresolvable variable: no prefix", `
servers:
  - url: "https://api.example.com/{base}"`, "/document"},
		{"the first server decides", `
servers:
  - url: https://prod.example.com/api/v2
  - url: http://localhost:3000/other`, "/api/v2/document"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			doc := "openapi: 3.0.3\ninfo: {title: t, version: \"1\"}" + tc.servers + "\npaths:\n  /document:\n    get: {summary: list}\n"
			if err := os.WriteFile(filepath.Join(repo, "openapi.yaml"), []byte(doc), 0o644); err != nil {
				t.Fatal(err)
			}
			rep, err := Run(Options{Repo: repo, Out: t.TempDir()})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(rep.Routes) != 1 || rep.Routes[0].Method != "GET" || rep.Routes[0].Path != tc.want {
				t.Fatalf("routes = %+v, want exactly GET %s", rep.Routes, tc.want)
			}
		})
	}
}
