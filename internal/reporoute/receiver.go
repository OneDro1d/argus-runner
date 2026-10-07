package reporoute

import (
	"regexp"
	"strings"
)

// jsReceiverWhitelist names the identifiers a JS/TS method-call receiver must match (on its LAST
// dotted segment) to be trusted as a server/router object by default — spec item 2 of the review:
// "app, fastify, server, instance, router, r, api, routes". This is a DEFAULT, not an override: a
// file that assigns one of these names from an HTTP client constructor (see clientAliasesIn) loses
// the benefit of the doubt for that specific identifier in that file.
var jsReceiverWhitelist = map[string]bool{
	"app": true, "fastify": true, "server": true, "instance": true,
	"router": true, "r": true, "api": true, "routes": true,
}

// jsReceiverClientSubstrings: a receiver chain containing any of these (case-insensitive, anywhere
// in the dotted chain) is an HTTP CLIENT, never a route registration — spec item 2: "Reject
// receivers that are HTTP clients (axios, http, https, got, ky, fetch, request, superagent, client,
// this.client, *Client, *.http)". "http" as a substring also catches "https" and ".http".
var jsReceiverClientSubstrings = []string{
	"axios", "http", "got", "ky", "fetch", "request", "superagent", "client",
}

// axiosCreateAssignRe finds `<name> = axios.create(...)` (with or without a `this.` prefix), the
// exact shape found in packages/kyc-allpass/src/clients/allpass.ts (`this.api = axios.create(...)`)
// — a case the plain name-based whitelist gets WRONG, because "api" is also a legitimate server
// receiver name. Content evidence in the file overrides the name-based default for that identifier.
var axiosCreateAssignRe = regexp.MustCompile(`(?:this\.)?([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*axios\s*\.\s*create\s*\(`)

// otherClientCtorAssignRe covers the same shape for got()/superagent()-style client constructors
// assigned to a local alias, e.g. `this.http = got.extend(...)`.
var otherClientCtorAssignRe = regexp.MustCompile(`(?:this\.)?([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*(?:got|superagent)\s*(?:\.\s*(?:extend|create)\s*)?\(`)

// clientAliasesIn scans src for identifiers assigned from an HTTP-client constructor and returns
// their last-segment names, lower-cased — e.g. {"api"} for `this.api = axios.create(...)`. These
// override jsReceiverWhitelist for this file: a name being in the generic whitelist is not evidence
// against direct proof, in the same file, that THIS alias is a client.
func clientAliasesIn(src string) map[string]bool {
	out := map[string]bool{}
	for _, m := range axiosCreateAssignRe.FindAllStringSubmatch(src, -1) {
		out[strings.ToLower(m[1])] = true
	}
	for _, m := range otherClientCtorAssignRe.FindAllStringSubmatch(src, -1) {
		out[strings.ToLower(m[1])] = true
	}
	return out
}

// classifyJSReceiver decides whether receiver (e.g. "app", "this.api", "client") is trustworthy as
// a server/router object. isRoute=false, isAmbiguous=true means "best-effort conservative skip" —
// spec item 2: "when in doubt, do not extract, and count it as 'ambiguous call skipped'".
func classifyJSReceiver(receiver string, clientAliases map[string]bool) (isRoute bool, reason string) {
	low := strings.ToLower(receiver)
	last := low
	if i := strings.LastIndex(low, "."); i >= 0 {
		last = low[i+1:]
	}
	if clientAliases[last] {
		return false, "receiver " + receiver + " was assigned from an HTTP client constructor in this file"
	}
	for _, b := range jsReceiverClientSubstrings {
		if strings.Contains(low, b) {
			return false, "outbound HTTP client receiver (" + receiver + ")"
		}
	}
	if jsReceiverWhitelist[last] {
		return true, ""
	}
	return false, "unrecognized receiver, best-effort conservative skip (" + receiver + ")"
}

// hasTemplateExpr reports whether path contains a JS/TS template expression (`${...}`) — spec item
// 2: "Reject ... any path containing a template expression `${`" — such a path is never a static
// route literal, whichever receiver it came from.
func hasTemplateExpr(path string) bool {
	return strings.Contains(path, "${")
}

// frameworkHint scans a whole JS/TS file's source for a strong, file-level signal of which router
// library it uses (spec item 6: "Label by what was matched" — a colon in the PATH is not evidence
// of which library wrote it, since Fastify accepts `:param` paths too; the import/require/type is).
func frameworkHint(src string) string {
	fastify := fastifyHintRe.MatchString(src)
	express := expressHintRe.MatchString(src)
	switch {
	case fastify && !express:
		return "fastify"
	case express && !fastify:
		return "express"
	default:
		return "" // neither, or both: ambiguous
	}
}

var fastifyHintRe = regexp.MustCompile(`(?i)from\s+["']fastify["']|require\(\s*["']fastify["']\s*\)|\bFastifyInstance\b|\bFastify\s*\(`)
var expressHintRe = regexp.MustCompile(`(?i)from\s+["']express["']|require\(\s*["']express["']\s*\)|\bexpress\s*\(\s*\)|\bexpress\.Router\s*\(`)
