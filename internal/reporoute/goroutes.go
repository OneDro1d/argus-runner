package reporoute

import (
	"os"
	"regexp"
	"strings"
)

// netHTTPRe matches `http.HandleFunc("/p", h)` / `http.HandleFunc("GET /p", h)` (Go 1.22+ pattern
// mux) and the `mux.HandleFunc`/`mux.Handle` method-set on a *http.ServeMux (spec item 2c).
var netHTTPRe = regexp.MustCompile(`\b(?:http|mux|router|r)\.(?:HandleFunc|Handle)\s*\(\s*"([^"]+)"`)

// gorillaMethodsRe finds a trailing `.Methods("GET", "POST")` chained onto a mux.Handle(Func) call
// — gorilla/mux's own way of declaring the verb (spec item 2c, "mux.Handle").
var gorillaMethodsRe = regexp.MustCompile(`\.Methods\(([^)]*)\)`)
var quotedRe = regexp.MustCompile(`"([A-Za-z]+)"`)

// chiRe matches chi's `r.Get("/p", h)` / `r.Post(...)` — capitalised verb, any receiver name.
var chiRe = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\.(Get|Post|Put|Patch|Delete|Head|Options)\s*\(\s*"([^"]+)"`)

// ginRe matches gin's `r.GET("/p", h)` — all-caps verb, any receiver name.
var ginRe = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\.(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s*\(\s*"([^"]+)"`)

func extractGo(p, rel string, rep *Report) {
	raw, err := os.ReadFile(p)
	if err != nil {
		rep.UnparseableFiles = append(rep.UnparseableFiles, UnparseableFile{File: rel, Reason: err.Error()})
		return
	}
	src := string(raw)
	lines := strings.Split(src, "\n")

	for i, line := range lines {
		if m := netHTTPRe.FindStringSubmatch(line); m != nil {
			method, path := splitGoMuxPattern(m[1])
			extractor := "go-net-http"
			if strings.Contains(line, "mux.") {
				extractor = "go-mux"
			}
			if method == "" {
				// No verb in the pattern and no chained .Methods() on this same line: net/http's
				// ServeMux with no method prefix answers every verb, which this tool records as GET
				// (the read-only, always-safe verb to propose against) rather than inventing a write.
				if mm := gorillaMethodsRe.FindStringSubmatch(line); mm != nil {
					method = firstQuoted(mm[1])
					extractor = "go-mux"
				} else {
					method = "GET"
				}
			}
			addGoRoute(rep, rel, i+1, method, path, extractor)
			continue
		}
		if m := chiRe.FindStringSubmatch(line); m != nil {
			addGoRoute(rep, rel, i+1, m[1], m[2], "go-chi")
			continue
		}
		if m := ginRe.FindStringSubmatch(line); m != nil {
			addGoRoute(rep, rel, i+1, m[1], m[2], "go-gin")
			continue
		}
	}
}

// addGoRoute records a route, but only when path looks like an HTTP path (starts with "/") — chi's
// `r.Get`/gin's `r.GET` share their exact call shape with any other `.Get`/`.GET` method a struct
// might define (a cache, a config accessor, …), and only the leading "/" tells them apart.
func addGoRoute(rep *Report, rel string, line int, method, path, extractor string) {
	if !strings.HasPrefix(path, "/") {
		return
	}
	rep.Routes = append(rep.Routes, Route{
		Method: strings.ToUpper(method), Path: path, File: rel, Line: line,
		Extractor: extractor, ParamNames: goPathParams(path),
	})
}

// splitGoMuxPattern splits a Go 1.22+ ServeMux pattern ("GET /p", "/p") into (method, path).
func splitGoMuxPattern(pattern string) (method, path string) {
	fields := strings.Fields(pattern)
	if len(fields) == 2 && isHTTPMethod(fields[0]) {
		return fields[0], fields[1]
	}
	return "", pattern
}

func isHTTPMethod(s string) bool {
	switch strings.ToUpper(s) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return true
	}
	return false
}

func firstQuoted(s string) string {
	m := quotedRe.FindStringSubmatch(s)
	if m == nil {
		return "GET"
	}
	return m[1]
}

// goPathParams extracts chi/gin/Go-1.22-mux `{name}`/`:name` path params.
func goPathParams(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		switch {
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") && len(seg) > 2:
			out = append(out, strings.TrimSuffix(seg[1:len(seg)-1], "..."))
		case strings.HasPrefix(seg, ":") && len(seg) > 1:
			out = append(out, seg[1:])
		}
	}
	return out
}
