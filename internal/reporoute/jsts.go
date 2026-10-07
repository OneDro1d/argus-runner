package reporoute

import (
	"os"
	"regexp"
	"strings"
)

// jsMethodCallRe matches the common Fastify/Express verb-call shapes (spec item 2b):
//
//	app.get('/p', ...)      fastify.post("/p", ...)     router.put(`/p`, ...)
//
// The receiver is captured (group 1) so the caller can decide whether it names a server/router
// object or an OUTBOUND HTTP CLIENT (spec item 2: `client.get(...)`, `this.api.get(...)` — a call TO
// a third party is not a route of the SUT) — what makes the call a CANDIDATE route registration is
// the verb method name plus a string-literal first argument, but the receiver decides whether it is
// actually kept.
var jsMethodCallRe = regexp.MustCompile(
	`\b([A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)*)\.(get|post|put|patch|delete|head|options)\s*\(\s*['"` + "`" + `]([^'"` + "`" + `]+)['"` + "`" + `]`)

// jsRouteObjectRe matches Fastify's object form: fastify.route({ method: 'GET', url: '/p', ... }).
// Group 1 is the receiver, checked the same way as jsMethodCallRe's.
var jsRouteObjectRe = regexp.MustCompile(`\b([A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)*)\.route\s*\(\s*\{`)
var jsRouteMethodFieldRe = regexp.MustCompile(`(?i)method\s*:\s*['"` + "`" + `](GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)['"` + "`" + `]`)
var jsRouteURLFieldRe = regexp.MustCompile(`url\s*:\s*['"` + "`" + `]([^'"` + "`" + `]+)['"` + "`" + `]`)

// jsRegisterRe finds a `.register(<plugin>, { ... prefix: '/x' ... })` call (spec item 2b).
var jsRegisterRe = regexp.MustCompile(`\.register\s*\(`)
var jsPrefixFieldRe = regexp.MustCompile(`prefix\s*:\s*['"` + "`" + `]([^'"` + "`" + `]+)['"` + "`" + `]`)

// regSpan is a register(...) call's bracket-balanced argument span plus the prefix it declared
// (spec item 2b: "when it is statically visible in the same file").
type regSpan struct {
	start, end int // byte offsets of the call's argument list, start = the '(' after register
	prefix     string
	line       int
}

func extractJSTS(p, rel string, rep *Report) {
	raw, err := os.ReadFile(p)
	if err != nil {
		rep.UnparseableFiles = append(rep.UnparseableFiles, UnparseableFile{File: rel, Reason: err.Error()})
		return
	}
	src := string(raw)

	// File-level signals, computed once (spec items 2 and 6): which identifiers in THIS file are
	// HTTP-client aliases (e.g. `this.api = axios.create(...)`), and which router library the
	// file's imports/types name — a colon in the PATH is not evidence of which library wrote it
	// (Fastify accepts `:param` paths too), the import/require/type is.
	clientAliases := clientAliasesIn(src)
	hint := frameworkHint(src)

	// Pass 1: every register(...) call and its bracket-balanced span, so a route found INSIDE that
	// span can inherit its prefix (spec item 2b: "when it is statically visible in the same file").
	var regs []regSpan
	for _, loc := range jsRegisterRe.FindAllStringIndex(src, -1) {
		openParen := loc[1] - 1
		end := matchingParen(src, openParen)
		if end < 0 {
			continue // unbalanced — best effort, skip rather than misattribute
		}
		body := src[openParen:end]
		m := jsPrefixFieldRe.FindStringSubmatch(body)
		line := 1 + strings.Count(src[:loc[0]], "\n")
		if m == nil {
			// A register() call with no prefix option is not this extractor's concern (routes
			// inside it simply have no prefix, same as a route outside any register() call).
			continue
		}
		regs = append(regs, regSpan{start: openParen, end: end, prefix: m[1], line: line})
	}

	skipAmbiguous := func(method, path string, offset int, reason string) {
		line := 1 + strings.Count(src[:offset], "\n")
		rep.AmbiguousCalls = append(rep.AmbiguousCalls, AmbiguousCall{
			File: rel, Line: line, Method: strings.ToUpper(method), Path: path, Reason: reason,
		})
	}

	addRoute := func(method, path string, offset int, extractor string) {
		line := 1 + strings.Count(src[:offset], "\n")
		prefix, unresolved := prefixFor(offset, regs)
		full := prefix + path
		rep.Routes = append(rep.Routes, Route{
			Method: strings.ToUpper(method), Path: full, File: rel, Line: line,
			Extractor: extractor, ParamNames: jsPathParams(full), PrefixUnresolved: unresolved,
		})
	}

	for _, m := range jsMethodCallRe.FindAllStringSubmatchIndex(src, -1) {
		receiver := src[m[2]:m[3]]
		method := src[m[4]:m[5]]
		path := src[m[6]:m[7]]
		// Fastify/Express both REQUIRE a route path to start with "/" — a bare word here is not a
		// route registration at all, it is some OTHER `.get(...)`/`.post(...)` call this generic
		// receiver-agnostic pattern also matches: `Map.get(key)`, `URLSearchParams.get(name)`,
		// `formData.get(field)`, an object literal's own `.get` accessor, and so on. Measured
		// against a real repo (shop-services): without this guard, `action`/`amount`/
		// `source`/`target`/`token` were proposed as "routes" — every one a Map/URLSearchParams
		// read, not an HTTP registration.
		if !strings.HasPrefix(path, "/") {
			continue
		}
		// spec item 2: a template expression in the path (`${...}`) is never a static route literal
		// — e.g. `this.api.get(\`/applicant/${applicantId}/status\`)`, an outbound client call.
		if hasTemplateExpr(path) {
			skipAmbiguous(method, path, m[0], "path contains a template expression (`${`), not a static route literal")
			continue
		}
		isRoute, reason := classifyJSReceiver(receiver, clientAliases)
		if !isRoute {
			skipAmbiguous(method, path, m[0], reason)
			continue
		}
		addRoute(method, path, m[0], resolveExtractor(hint))
	}

	for _, m := range jsRouteObjectRe.FindAllStringSubmatchIndex(src, -1) {
		receiver := src[m[2]:m[3]]
		openBrace := m[1] - 1
		end := matchingBrace(src, openBrace)
		if end < 0 {
			continue
		}
		body := src[openBrace:end]
		mm := jsRouteMethodFieldRe.FindStringSubmatch(body)
		uu := jsRouteURLFieldRe.FindStringSubmatch(body)
		if mm == nil || uu == nil {
			continue
		}
		if hasTemplateExpr(uu[1]) {
			skipAmbiguous(mm[1], uu[1], m[0], "path contains a template expression (`${`), not a static route literal")
			continue
		}
		isRoute, reason := classifyJSReceiver(receiver, clientAliases)
		if !isRoute {
			skipAmbiguous(mm[1], uu[1], m[0], reason)
			continue
		}
		// The object-route-registration form (`.route({ method, url })`) is Fastify-specific syntax
		// — no other extractor here matches it, so it is never ambiguous (spec item 6).
		addRoute(mm[1], uu[1], m[0], "fastify")
	}

	// register() calls whose prefix could not be attached to any route in this file at all are
	// still worth recording — spec item 2b: "record when a prefix could not be resolved".
	recordUnresolvedRegisters(rel, regs, rep)
}

// prefixFor returns the register() prefix whose span contains offset, and whether more than one
// such span exists (an ambiguous, hence unresolved, nesting — best effort stops there rather than
// guessing which one wins).
func prefixFor(offset int, regs []regSpan) (prefix string, unresolved bool) {
	best := -1
	count := 0
	for i, r := range regs {
		if offset > r.start && offset < r.end {
			count++
			if best == -1 || (r.end-r.start) < (regs[best].end-regs[best].start) {
				best = i
			}
		}
	}
	if count == 0 {
		return "", false
	}
	if count > 1 {
		return regs[best].prefix, true // nested register()s: take the innermost, flag as best-effort
	}
	return regs[best].prefix, false
}

func recordUnresolvedRegisters(rel string, regs []regSpan, rep *Report) {
	for _, r := range regs {
		hasRouteInside := false
		for _, rt := range rep.Routes {
			if rt.File != rel {
				continue
			}
			// A route recorded with this literal prefix string is presumed to have come from
			// inside this span; good enough for "did this prefix ever apply to anything".
			if strings.HasPrefix(rt.Path, r.prefix) {
				hasRouteInside = true
				break
			}
		}
		if !hasRouteInside {
			rep.UnresolvedPrefixes = append(rep.UnresolvedPrefixes, UnresolvedPrefix{
				File: rel, Line: r.line, Prefix: r.prefix,
				Reason: "a register() call declared this prefix, but no route registration was found inside its argument list in this file",
			})
		}
	}
}

// resolveExtractor turns a file-level frameworkHint into the label written on a Draft/Route (spec
// item 6): "js-router" when the file gave no unambiguous signal, rather than guessing from the
// path's punctuation (a colon in the path is not proof of which library wrote it).
func resolveExtractor(hint string) string {
	if hint == "" {
		return "js-router"
	}
	return hint
}

// jsPathParams extracts Express/Fastify `:name` path params.
func jsPathParams(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, ":") && len(seg) > 1 {
			out = append(out, seg[1:])
		}
	}
	return out
}

// matchingParen returns the offset just past the '(' at open's matching ')', or -1.
func matchingParen(s string, open int) int { return matchingBracket(s, open, '(', ')') }

// matchingBrace returns the offset just past the '{' at open's matching '}', or -1.
func matchingBrace(s string, open int) int { return matchingBracket(s, open, '{', '}') }

func matchingBracket(s string, open int, o, c byte) int {
	if open >= len(s) || s[open] != o {
		return -1
	}
	depth := 0
	inStr := byte(0)
	for i := open; i < len(s); i++ {
		ch := s[i]
		if inStr != 0 {
			if ch == '\\' {
				i++
				continue
			}
			if ch == inStr {
				inStr = 0
			}
			continue
		}
		switch ch {
		case '\'', '"', '`':
			inStr = ch
		case o:
			depth++
		case c:
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}
