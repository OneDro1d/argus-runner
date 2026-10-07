package reporoute

import (
	"regexp"
	"sort"
	"strings"
)

// routeGroup is one logical, deduplicated route (spec item 3): every raw Route occurrence that
// normalises to the SAME (method, path, service) is folded into one group whose References list
// every source file:line. A group whose (method, normalised path) recurs under a DIFFERENT service
// is kept as its OWN group (ambiguousSvc=true) — that is six services each with their own /health,
// not one route found six times.
type routeGroup struct {
	method       string
	path         string // representative literal path (first source, by File:Line)
	paramNames   []string
	serviceKey   string
	sources      []Route // every raw occurrence, sorted by File then Line
	ambiguousSvc bool    // this (method,normPath) also occurs under >=1 OTHER serviceKey
}

// serviceDirRe pulls the service/package directory out of a conventional monorepo layout
// (packages/<name>/..., services/<name>/..., apps/<name>/...).
var serviceDirRe = regexp.MustCompile(`(?:^|/)(?:packages|services|apps)/([^/]+)/`)

func normalizePath(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if isParamSegment(s) {
			segs[i] = "{param}"
		}
	}
	return strings.Join(segs, "/")
}

func isParamSegment(s string) bool {
	switch {
	case strings.HasPrefix(s, ":") && len(s) > 1:
		return true
	case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") && len(s) > 2:
		return true
	case strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">") && len(s) > 2:
		return true
	}
	return false
}

// serviceKey names the service/package a route's source file belongs to, so that the SAME route
// declared in two DIFFERENT services (six GET /health) is told apart from the SAME route declared
// twice in the SAME service by two different extractors (an OpenAPI doc and its Fastify
// implementation). Falls back to the top-level directory, then "" for a repo-root file.
func serviceKey(file string) string {
	if m := serviceDirRe.FindStringSubmatch(file); m != nil {
		return m[1]
	}
	parts := strings.SplitN(file, "/", 2)
	if len(parts) > 0 && parts[0] != "" {
		return parts[0]
	}
	return ""
}

type groupKey struct{ method, norm, svc string }

// groupRoutes folds routes into deduplicated routeGroups (spec item 3), sorted deterministically.
func groupRoutes(routes []Route) []routeGroup {
	idx := map[groupKey]int{}
	var groups []routeGroup
	for _, r := range routes {
		k := groupKey{r.Method, normalizePath(r.Path), serviceKey(r.File)}
		if i, ok := idx[k]; ok {
			groups[i].sources = append(groups[i].sources, r)
			continue
		}
		idx[k] = len(groups)
		groups = append(groups, routeGroup{
			method: r.Method, path: r.Path, paramNames: r.ParamNames,
			serviceKey: k.svc, sources: []Route{r},
		})
	}
	for i := range groups {
		sort.Slice(groups[i].sources, func(a, b int) bool { return routeLess(groups[i].sources[a], groups[i].sources[b]) })
		// The representative path/params come from the FIRST source in deterministic (File,Line)
		// order, so re-runs of Run() on the same repo pick the same representative every time.
		groups[i].path = groups[i].sources[0].Path
		groups[i].paramNames = groups[i].sources[0].ParamNames
	}
	svcCount := map[string]map[string]bool{}
	for _, g := range groups {
		nk := g.method + " " + normalizePath(g.path)
		if svcCount[nk] == nil {
			svcCount[nk] = map[string]bool{}
		}
		svcCount[nk][g.serviceKey] = true
	}
	for i := range groups {
		nk := groups[i].method + " " + normalizePath(groups[i].path)
		groups[i].ambiguousSvc = len(svcCount[nk]) > 1
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].method != groups[j].method {
			return groups[i].method < groups[j].method
		}
		if groups[i].path != groups[j].path {
			return groups[i].path < groups[j].path
		}
		return groups[i].serviceKey < groups[j].serviceKey
	})
	return groups
}
