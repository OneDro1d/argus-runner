package scenario

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
)

// VR12-T3 (V29-018) — EVERY AUTHOR-DECLARED HEADER IS SENT.
//
// ⭐ THE PARSE SIDE ALREADY EXISTED. `Trigger.Headers` has always been populated for every
// `Name: value` line before the payload fence — and exactly ONE key was consumed downstream:
//
//	if v, ok := s.Trigger.Headers["Authorization"]; ok { props["auth.header"] = v }
//
// The map was fully populated and then all but one key was dropped between the parser and the
// template. SYN-MCP-002 and SYN-MCP-004 each write `Accept: application/json, text/event-stream`
// into their TRIGGER and neither has ever sent it; corroborated on the wire by byte accounting —
// two scenarios posting an identical 155-byte body differ by exactly the 43 bytes of
// `"Bearer " + token`, leaving ZERO bytes for a 45-byte Accept header.

// reservedHeaders are the names an author may NOT set, with the reason. ⛔ They are REFUSED at
// validate time, naming the header — never silently ignored, which is the defect this row is about
// pointed the other way.
var reservedHeaders = map[string]string{
	http.CanonicalHeaderKey("X-Correlation-Id"): "the correlation id is the runner's alone — the report, the logs, " +
		"the dashboard and the SUT's own records are stitched together by it, and a scenario that sets its own " +
		"would make its run untraceable",
	http.CanonicalHeaderKey("Mcp-Session-Id"): "the MCP session id is minted by the protocol handshake, not by a " +
		"scenario; setting it by hand would break the session the runner just opened",
}

// amqpPropertyHeader is AC-D16's `User-Id` TRIGGER line: it declares the AMQP `user_id` message
// property a direct broker publish is made with (internal/argus.DeriveProps reads it into
// mq.user_id) — not an HTTP header at all, so it is excluded from AuthorHeaders exactly the way
// Authorization already is (its own case below), rather than reserved/refused like the two names
// above: an author may legally write it, it is simply never sent as a literal HTTP header.
var amqpPropertyHeader = http.CanonicalHeaderKey("User-Id")

// ReservedHeader returns the reason a header name is refused, or "" when the author may set it.
func ReservedHeader(name string) string { return reservedHeaders[http.CanonicalHeaderKey(name)] }

// AuthorHeaders returns the scenario's declared headers in a STABLE order (sorted by canonical
// name), so the numbered property set a template reads is deterministic across runs — a set that
// renumbered itself would make two identical runs differ on the wire.
//
// ⚠ `Authorization` is EXCLUDED: it already has its own path (`props["auth.header"]`, the 4.8
// override, including present-but-empty = send no token) and permission scenarios rely on it.
// Emitting it twice would give the template two answers to one question.
//
// ⚠ `User-Id` is EXCLUDED the same way (AC-D16): it already has its own path
// (`props["mq.user_id"]`, internal/argus.DeriveProps) into the AMQP publish sampler, and it is not
// an HTTP header at all — sending it to the SUT would be a stray header nobody asked for.
func AuthorHeaders(s *Scenario) [][2]string {
	if s == nil || len(s.Trigger.Headers) == 0 {
		return nil
	}
	names := make([]string, 0, len(s.Trigger.Headers))
	for k := range s.Trigger.Headers {
		ck := http.CanonicalHeaderKey(k)
		if ck == "Authorization" || ck == amqpPropertyHeader || ReservedHeader(k) != "" {
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([][2]string, 0, len(names))
	for _, k := range names {
		out = append(out, [2]string{k, s.Trigger.Headers[k]})
	}
	return out
}

// HeaderNames lists the declared header NAMES — for logs, and for the author tools' reply.
// ⛔ NAMES ONLY. A header value is the likeliest place in a scenario to hold a credential.
func HeaderNames(s *Scenario) []string {
	var out []string
	for _, h := range AuthorHeaders(s) {
		out = append(out, h[0])
	}
	if s != nil {
		if _, ok := s.Trigger.Headers["Authorization"]; ok {
			out = append(out, "Authorization")
		}
		if _, ok := s.Trigger.Headers[amqpPropertyHeader]; ok {
			out = append(out, amqpPropertyHeader)
		}
	}
	sort.Strings(out)
	return out
}

// headerProps renders the numbered property set a JMeter template reads. It mirrors VR12-E8's
// numbered body set on purpose: ONE convention, so a reader who has met one has met both.
//
// Absent, or `trigger.header_n = 0`, means byte-identical to today.
func headerProps(hs [][2]string) map[string]string {
	if len(hs) == 0 {
		return nil
	}
	p := map[string]string{"trigger.header_n": fmt.Sprint(len(hs))}
	for i, h := range hs {
		n := fmt.Sprint(i + 1)
		p["trigger.header_"+n+"_name"] = h[0]
		p["trigger.header_"+n+"_value"] = h[1]
	}
	return p
}

// HeaderProps is headerProps for a scenario — the form callers use.
func HeaderProps(s *Scenario) map[string]string { return headerProps(AuthorHeaders(s)) }

// ── item 25 — THE BASIC-AUTH HELPER ─────────────────────────────────────────────────────────────
//
// The gap: an author who needed HTTP Basic auth had to pre-encode `base64("user:pass")` themselves
// and paste the RESULT into `Authorization: Basic <encoded>` — the SUT's password sat, base64'd but
// not secret, in the scenario file, in every diff and in git history forever.
//
// The fix keeps the existing `Authorization: <value>` header line (both in a plain TRIGGER and in a
// chain `http` step's own `headers`), and gives it ONE new value FORM:
//
//	Authorization: Basic ${basic_auth:<username>:<PASSWORD_ENV_VAR>}
//
// chosen over a dedicated header key (`X-Basic-Auth-User` + `X-Basic-Auth-Pass-Env`) because the
// header a reader already expects to carry credentials keeps carrying them — a second header pair
// would be two places to look for the same thing — and it reuses the `${...}` placeholder shape
// every other value in this grammar already uses (${cid}, ${saved.<var>}), rather than inventing a
// third. The username is written in the clear (usernames are not the secret here — the same
// assumption `mgmt.auth.header`'s URL-userinfo split already makes, argus.go:391); the password NEVER
// is — only the NAME of the env var that holds it is. It is a helper, not a placeholder resolveVars
// expands: resolveVars's ${VAR} grammar cannot match it (the colons are not identifier characters),
// so it passes through untouched to whichever call site actually builds the request, and IS resolved
// there, at run time — chain.HTTPStep's buildRequest (per attempt) and argus.DeriveProps (per run) —
// never earlier, and never into anything printed (the value never reaches a report field; the
// existing secret-prop split, internal/argus/secret_props.go, already keeps `auth.header` out of
// JMeter's own log for the JMeter path).
var (
	// basicAuthAttemptRe matches ANYTHING claiming to be this form — `Basic ${basic_auth:...}` —
	// whether or not the inside parses, so a malformed attempt is refused BY NAME (ValidateBasicAuthHeader)
	// rather than silently sent as a literal, useless Authorization header nobody meant to send.
	basicAuthAttemptRe = regexp.MustCompile(`(?i)^\s*Basic\s+\$\{basic_auth:(.*)\}\s*$`)
	// envVarNameRe is an ordinary environment variable name — the same shape `ValidURLEnv` polices
	// for `url_env` (amqpstep.go), applied here to the password's env var name.
	envVarNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// IsBasicAuthAttempt reports whether v CLAIMS to be the basic-auth helper form, whether or not it
// parses — the same "claims to be mine" split ClaimsToBeBodyAssert draws for the body grammar.
func IsBasicAuthAttempt(v string) bool { return basicAuthAttemptRe.MatchString(v) }

// BasicAuthMarker parses a well-formed `Basic ${basic_auth:<user>:<PASSWORD_ENV_VAR>}` header
// value. ok=false means v is not this form AT ALL (not even a malformed attempt) — the ordinary
// caller's cue to fall through to plain header resolution, exactly as it does today.
func BasicAuthMarker(v string) (user, passEnvVar string, ok bool) {
	m := basicAuthAttemptRe.FindStringSubmatch(v)
	if m == nil {
		return "", "", false
	}
	idx := strings.Index(m[1], ":")
	if idx < 0 {
		return "", "", false
	}
	user, passEnvVar = m[1][:idx], m[1][idx+1:]
	if user == "" || !envVarNameRe.MatchString(passEnvVar) {
		return "", "", false
	}
	return user, passEnvVar, true
}

// ValidateBasicAuthHeader returns the reason an Authorization header value using the basic-auth
// helper form is malformed (a bad operator is not this grammar's concern — there is none — but a
// missing `:` separator, an empty username, or an env var name that is not a valid identifier all
// are), or "" when v either parses cleanly or is not attempting this form at all.
func ValidateBasicAuthHeader(v string) string {
	if !IsBasicAuthAttempt(v) {
		return ""
	}
	if _, _, ok := BasicAuthMarker(v); ok {
		return ""
	}
	inner := basicAuthAttemptRe.FindStringSubmatch(v)[1]
	idx := strings.Index(inner, ":")
	if idx < 0 {
		return "declares the basic-auth helper form but names no env var for the password — write " +
			"`Basic ${basic_auth:<username>:<PASSWORD_ENV_VAR>}`"
	}
	user, passEnvVar := inner[:idx], inner[idx+1:]
	if user == "" {
		return "declares the basic-auth helper form with an empty username"
	}
	return fmt.Sprintf("declares the basic-auth helper form with %q as the password env var name, "+
		"which is not a valid environment variable name (letters, digits, underscore; must not start "+
		"with a digit)", passEnvVar)
}

// ResolveBasicAuthHeader builds the `Basic <base64>` value AT RUN TIME: user is whatever the
// scenario wrote (never secret), and the password comes from the NAMED env var — never from the
// scenario file, so it can never appear in a diff, in git history, or in an author tool's reply.
// Refuses BY NAME when the env var is not set — a clear reason, never an empty credential silently
// encoded as `user:` that would surface as a confusing 401 several layers downstream.
func ResolveBasicAuthHeader(user, passEnvVar string) (string, error) {
	pw, ok := os.LookupEnv(passEnvVar)
	if !ok {
		return "", fmt.Errorf("basic-auth helper: environment variable %q (declared as the password "+
			"for user %q) is not set", passEnvVar, user)
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pw)), nil
}
