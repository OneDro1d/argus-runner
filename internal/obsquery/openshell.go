package obsquery

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// ── Spec 26, A1 (observe only): what the SUT agent's OpenShell sandbox denied ─────────────────────
//
// OpenShell writes one OCSF v1.8.0 JSON object per line (docs/observability/ocsf-json-export.mdx:69
// at v0.1.2): network and HTTP allow/deny decisions, process lifecycle events, findings and config
// changes. Filesystem (Landlock) and seccomp refusals are never logged (spec 26 C2, C3), so they can
// never appear in a block. No event carries a correlation id, so an
// event can be tied to a scenario only by time window plus sandbox id (container.uid, :67). This file
// is the read (SandboxLines) and the pure builder that turns one window's lines into the report's
// sandbox_policy block (SandboxPolicyFor).
//
// ⛔ Three rules the builder exists to keep:
//   - `unavailable` is never an empty "clean": a failed read, a broken selector (Loki answers it with
//     HTTP 200 and nothing), a downgraded schema and a window with no line from the sandbox all give
//     `unavailable` with a named reason, `"denied_count":null` and `"events":[]`.
//   - No payloads, and credentials removed by SHAPE, which is not a guarantee (#448). Only the fields
//     of ocsfLine are decoded, so nothing else can be copied. From those fields an event carries: an
//     HTTP target's method, and its scheme, host and port as logged with its first 8 path segments
//     (then "…"), userinfo, query string and fragment dropped (a URL that does not parse and still
//     holds an "@" is dropped whole, and dst_endpoint's host and port stand in); a finding's title (cut
//     to 128 runes) and a reason, each with query strings and `scheme://user:pw@` userinfo dropped (in
//     free text, userinfo holding a "/" or white space is not recognised); a NET target's host and
//     port; and the class (with activity_name), the action or disposition, and the process and rule
//     names, as logged. A path segment, or a run of a title or a reason, that looks like a token is
//     replaced by "[redacted]" (tokenShaped: a known prefix such as ghp_, github_pat_, sk-, syn_,
//     xoxb-, eyJ, then 16+ characters; 24+ hex digits; 24+ letters and digits mixed; 24+ base64
//     characters with a piece of 8+ mixing upper case, lower case and digits; the value after Bearer
//     or Basic unless it is a plain word). A secret that does not look like one (a short password in a
//     path segment, a value of letters only) is carried as it was logged, and so is a host name; a git
//     SHA or a digest in a path looks like one and is redacted. Every event field is cut to 256 runes,
//     and coverage_reason names a downgraded metadata.version redacted and cut to 32 runes.
//   - Deterministic: the same Loki answer gives the same bytes (the evidence hash rests on it).

// SandboxEvidence is the A1 read. *Loki implements it; tests use a fake, the way survival.go uses
// PromQuery. Backend is NOT widened, because *BetterStack would then have to implement it (and
// config refuses observability.openshell beside betterstack).
type SandboxEvidence interface {
	SandboxLines(selector, sandbox string, from, to time.Time) SandboxRead
}

var _ SandboxEvidence = (*Loki)(nil)

// SandboxLine is one Loki entry as Loki returned it. Exported (unlike lokiEntry) so a fake reader in
// another package's test can hand the builder preset lines.
type SandboxLine struct {
	TS   string // Loki entry timestamp, unix nanoseconds (string)
	Line string // the raw line; never copied into a report — only ocsfLine's fields are decoded
}

// SandboxRead is the outcome of one read: the lines, the limit the query used, or why it failed.
type SandboxRead struct {
	Lines []SandboxLine
	Limit int   // the query's line limit; a read that returns this many is `lossy`
	Err   error // why the read failed (no URL, unsafe id, transport, HTTP status, decode); nil when Loki answered
}

// SandboxLineLimit is the per-window line limit, the same 2000 queryLines uses.
const SandboxLineLimit = 2000

// sandboxIDRe is what a sandbox id may look like before it is put into a LogQL line filter. It is
// quoted as well, but an id is config input, and nothing outside this charset is ever sent.
var sandboxIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// SafeSandboxID is the id as a report block may carry it: the id itself when it passes the charset
// rule, "" otherwise. A refused id is config input that is never echoed (not in the read error,
// not in the block); the block's reason says the id was refused.
func SafeSandboxID(id string) string {
	if !sandboxIDRe.MatchString(id) {
		return ""
	}
	return id
}

// SandboxLines reads every line of the sandbox in [from, to] with `<selector> |= "<sandbox>"`: lines of
// any action, because allowed lines are what prove the pipeline is alive (the liveness rule).
func (l *Loki) SandboxLines(selector, sandbox string, from, to time.Time) SandboxRead {
	read := SandboxRead{Limit: SandboxLineLimit}
	if strings.TrimSpace(l.BaseURL) == "" {
		read.Err = errors.New("no Loki URL for this executor (--loki is empty)")
		return read
	}
	if !sandboxIDRe.MatchString(sandbox) {
		// The value itself is not echoed: it is config input that failed the charset rule.
		read.Err = fmt.Errorf("the sandbox id is not a safe identifier (accepted pattern %s), so nothing was queried", sandboxIDRe.String())
		return read
	}
	q := strings.TrimSpace(selector) + " |= " + strconv.Quote(sandbox)
	entries, err := l.queryRange(q, from, to, SandboxLineLimit)
	if err != nil {
		read.Err = err
		return read
	}
	for _, e := range entries {
		read.Lines = append(read.Lines, SandboxLine{TS: e.TS, Line: e.Line})
	}
	return read
}

// ScenarioWindow is one scenario's padded window: From = start - pad, To = end + pad.
type ScenarioWindow struct {
	ID       string
	From, To time.Time
}

func (w ScenarioWindow) holds(t time.Time) bool { return !t.Before(w.From) && !t.After(w.To) }

const (
	maxSandboxPolicyEvents       = 50  // events listed per block; the rest are counted in events_omitted
	maxSandboxPolicyText         = 256 // runes per event field
	maxSandboxPolicyPathSegments = 8   // path segments an HTTP target keeps; the rest become one "…" (#448)
	maxSandboxPolicyTitle        = 128 // runes of a finding title (#448)
	maxSandboxPolicyVersion      = 32  // runes of a downgraded metadata.version named in coverage_reason (#448)
)

// ocsfLine is the ONLY shape a line is decoded into. Any other field — message, http_request bodies
// and headers, query strings, unmapped (except downgraded_from), evidences — is never decoded, so it
// can never be copied into a report (invariant 5).
type ocsfLine struct {
	Time         *int64 `json:"time"` // ms since the epoch
	ClassUID     *int   `json:"class_uid"`
	ActivityName string `json:"activity_name"`
	ActionID     *int   `json:"action_id"`
	Action       string `json:"action"`
	Disposition  string `json:"disposition"`
	StatusDetail string `json:"status_detail"`
	DstEndpoint  struct {
		Domain string `json:"domain"`
		IP     string `json:"ip"`
		Port   *int   `json:"port"`
	} `json:"dst_endpoint"`
	HTTPRequest struct {
		HTTPMethod string `json:"http_method"`
		// URL is a string, or an OCSF URL object (url_string | scheme, hostname, port, path). The real
		// shape is pinned by spec 26 G1; either way only scheme, host, port and path survive.
		URL json.RawMessage `json:"url"`
	} `json:"http_request"`
	Actor struct {
		Process struct {
			Name string `json:"name"`
		} `json:"process"`
	} `json:"actor"`
	FirewallRule struct {
		Name string `json:"name"`
	} `json:"firewall_rule"`
	FindingInfo struct {
		Title string `json:"title"`
	} `json:"finding_info"`
	Container struct {
		UID string `json:"uid"`
	} `json:"container"`
	Metadata struct {
		Version string `json:"version"`
	} `json:"metadata"`
	Unmapped struct {
		DowngradedFrom string `json:"downgraded_from"`
	} `json:"unmapped"`
}

// downgraded: ocsf_schema_version 1.1 or 1.3 strips `container` and rewrites metadata.version, and
// marks the record unmapped.downgraded_from (ocsf-json-export.mdx:203-207, :224-231; spec 26 C4).
func (o *ocsfLine) downgraded() bool {
	if o.Unmapped.DowngradedFrom != "" {
		return true
	}
	v := strings.TrimSpace(o.Metadata.Version)
	for _, old := range []string{"1.1", "1.3"} {
		if v == old || strings.HasPrefix(v, old+".") {
			return true
		}
	}
	return false
}

// kept: Denied (action_id 2 or action "Denied"), Blocked (disposition), or a finding (class 2004) —
// spec 26 §4 as corrected by C7; class 0 (EVENT) is kept when it carries a denial. DNS-failure
// denials are kept in P1, which observes; P2 decides whether they count (, C8).
func (o *ocsfLine) kept() bool {
	return (o.ActionID != nil && *o.ActionID == 2) ||
		strings.EqualFold(o.Action, "Denied") ||
		strings.EqualFold(o.Disposition, "Blocked") ||
		(o.ClassUID != nil && *o.ClassUID == 2004)
}

// classPrefix maps class_uid to the shorthand prefix (docs/observability/logging.mdx:55-64).
var classPrefix = map[int]string{
	0: "EVENT", 4001: "NET", 4002: "HTTP", 4007: "SSH", 1007: "PROC", 2004: "FINDING", 5019: "CONFIG", 6002: "LIFECYCLE",
}

func (o *ocsfLine) class() string {
	uid := *o.ClassUID
	p, ok := classPrefix[uid]
	if !ok {
		return "CLASS:" + strconv.Itoa(uid)
	}
	if a := strings.TrimSpace(o.ActivityName); a != "" {
		return p + ":" + strings.ToUpper(strings.ReplaceAll(a, " ", "_"))
	}
	return p
}

func (o *ocsfLine) target() string {
	hostPort := func() string {
		h := o.DstEndpoint.Domain
		if h == "" {
			h = o.DstEndpoint.IP
		}
		if h != "" && o.DstEndpoint.Port != nil {
			h += ":" + strconv.Itoa(*o.DstEndpoint.Port)
		}
		return h
	}
	switch *o.ClassUID {
	case 4001:
		return hostPort()
	case 4002:
		u := safeURL(o.HTTPRequest.URL)
		if u == "" {
			u = hostPort()
		}
		return strings.TrimSpace(o.HTTPRequest.HTTPMethod + " " + u)
	case 2004:
		return safeTitle(o.FindingInfo.Title)
	}
	return ""
}

var (
	userinfoRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@]*@`)
	urlTailRe  = regexp.MustCompile(`[?#][^\s\]]*`) // a query string or fragment, up to a space or `]`
	reasonQRe  = regexp.MustCompile(`\?[^\s\]]*`)   // a query string inside reason text

	// #448: the token-shape check. tokenRunRe is a run of the characters tokens are written in
	// (base64, base64url, hex and their separators). A run is redacted whole when any part of it is
	// token-shaped, so a standard-base64 value cut by "/" goes as one piece.
	tokenRunRe = regexp.MustCompile(`[A-Za-z0-9_+=/-]+`)
	// tokenPrefixRe: a known token prefix at the start of a run or after a separator, with 16 or more
	// token characters after it.
	tokenPrefixRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9])(?:syn_|sk-|sk_|ghp_|gho_|ghu_|ghs_|ghr_|github_pat_|glpat-|xox[a-z]-|npm_|AKIA|eyJ)[A-Za-z0-9_-]{16,}`)
	longChunkRe   = regexp.MustCompile(`[A-Za-z0-9]{24,}`)
	// b64ChunkRe: 24+ characters of the base64 and base64url alphabets. A random secret written in
	// them is cut by "-", "_", "+" or "/" into pieces that can each stay under 24, so such a chunk
	// counts when one of its pieces of 8+ mixes upper case, lower case and digits; words, numbers,
	// UUIDs and hyphenated names (CAT-6610-Add-Change-Notice-Bot) have no such piece.
	b64ChunkRe     = regexp.MustCompile(`[A-Za-z0-9+/_-]{24,}`)
	b64SeparatorRe = regexp.MustCompile(`[+/_-]`)
	// authSchemeRe: the value after Bearer or Basic, which is redacted unless it is a plain word
	// ("Basic auth", "Bearer token missing").
	authSchemeRe = regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)(\S+)`)
	plainWordRe  = regexp.MustCompile(`^[A-Za-z][a-z]*[[:punct:]]*$`)
)

const redactedMark = "[redacted]"

// tokenShaped reports whether a run looks like a credential: a known prefix; 24+ letters and digits
// in a row that are all hex or mix letters with digits; or 24+ base64 characters with a piece of 8+
// that mixes upper case, lower case and digits. A shape check, not a guarantee: a short password, or
// a long value of letters only, passes.
func tokenShaped(run string) bool {
	if tokenPrefixRe.MatchString(run) {
		return true
	}
	for _, c := range longChunkRe.FindAllString(run, -1) {
		hex, letter, digit := true, false, false
		for _, r := range c {
			switch {
			case r >= '0' && r <= '9':
				digit = true
			case (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'):
				letter = true
			default:
				letter, hex = true, false
			}
		}
		if hex || (letter && digit) {
			return true
		}
	}
	for _, c := range b64ChunkRe.FindAllString(run, -1) {
		for _, piece := range b64SeparatorRe.Split(c, -1) {
			if len(piece) >= 8 && mixesCaseAndDigits(piece) {
				return true
			}
		}
	}
	return false
}

// mixesCaseAndDigits: s holds an upper-case letter, a lower-case letter and a digit.
func mixesCaseAndDigits(s string) bool {
	var upper, lower, digit bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	return upper && lower && digit
}

// RedactTokens replaces the value after Bearer/Basic (unless it is a plain word), and every
// token-shaped run, with "[redacted]".
func RedactTokens(s string) string {
	s = authSchemeRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := authSchemeRe.FindStringSubmatch(m)
		if plainWordRe.MatchString(sub[3]) {
			return m
		}
		return sub[1] + sub[2] + redactedMark
	})
	return tokenRunRe.ReplaceAllStringFunc(s, func(run string) string {
		if tokenShaped(run) {
			return redactedMark
		}
		return run
	})
}

// safePath keeps the first maxSandboxPolicyPathSegments segments of a path and replaces a segment
// that holds a token (as written, or percent-decoded) with "[redacted]"; more segments become one "…".
func safePath(p string) string {
	lead := strings.HasPrefix(p, "/")
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(segs) > maxSandboxPolicyPathSegments {
		segs = append(segs[:maxSandboxPolicyPathSegments:maxSandboxPolicyPathSegments], "…")
	}
	for i, seg := range segs {
		dec, err := url.PathUnescape(seg)
		if err != nil {
			dec = seg
		}
		if RedactTokens(seg) != seg || RedactTokens(dec) != dec {
			segs[i] = redactedMark
		}
	}
	out := strings.Join(segs, "/")
	if lead {
		out = "/" + out
	}
	return out
}

// safeTitle is a finding title treated as a reason is (userinfo and query strings dropped, token-shaped
// runs redacted), then cut to maxSandboxPolicyTitle runes. Redaction comes first, so the cut can never
// shorten a token below the shape check.
func safeTitle(s string) string {
	return cut(safeReason(strings.TrimSpace(s)), maxSandboxPolicyTitle)
}

// cut keeps the first n runes of s.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// safeURL keeps scheme, host, port and path of an http_request.url and drops userinfo, query string
// and fragment, whatever shape the field has (C9: HTTP is matched on method, host, port and path).
// The path goes through safePath (#448): at most 8 segments, token-shaped ones redacted. On the text
// branch an "@" left after the userinfo rule drops the URL, and the target falls back to dst_endpoint.
func safeURL(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		var obj struct {
			URLString string `json:"url_string"`
			Scheme    string `json:"scheme"`
			Hostname  string `json:"hostname"`
			Port      *int   `json:"port"`
			Path      string `json:"path"`
		}
		if json.Unmarshal(raw, &obj) != nil {
			return ""
		}
		if obj.URLString != "" {
			s = obj.URLString
		} else if obj.Hostname != "" {
			s = obj.Hostname
			if obj.Port != nil {
				s += ":" + strconv.Itoa(*obj.Port)
			}
			if obj.Scheme != "" {
				s = obj.Scheme + "://" + s
			}
			s += obj.Path
		}
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		path := u.EscapedPath()
		u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
		u.Path, u.RawPath = "", ""
		return u.String() + safePath(path)
	}
	// Not a parseable absolute URL: strip by text, never pass it through as it came.
	s = urlTailRe.ReplaceAllString(userinfoRe.ReplaceAllString(s, "$1"), "")
	if strings.Contains(s, "@") {
		// Userinfo the text rule cannot bound (a "/" or white space in it, or no scheme before it), or
		// an "@" in the path: the two cannot be told apart here, so the URL is dropped and target()
		// falls back to dst_endpoint.
		return ""
	}
	// Scheme and host are not path segments: only what follows the first "/" after them is.
	head := ""
	if i := strings.Index(s, "://"); i >= 0 {
		head, s = s[:i+3], s[i+3:]
	}
	if i := strings.Index(s, "/"); i >= 0 {
		return head + s[:i] + safePath(s[i:])
	}
	return head + s
}

// safeReason is status_detail with every userinfo and query string removed and token-shaped runs
// redacted (#448).
func safeReason(s string) string {
	return RedactTokens(reasonQRe.ReplaceAllString(userinfoRe.ReplaceAllString(s, "$1"), ""))
}

func clip(s string) string { return cut(strings.TrimSpace(s), maxSandboxPolicyText) }

// RFC3339Milli is the time format of a sandbox_policy block: RFC 3339 with milliseconds, UTC.
const RFC3339Milli = "2006-01-02T15:04:05.000Z07:00"

func stamp(t time.Time) string { return t.UTC().Format(RFC3339Milli) }

func nanosTime(ts string) (time.Time, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, n), true
}

// SandboxPolicyFor turns one window's read into its report block. neighbours are the OTHER windows of
// the same run, used only to mark shared events. Pure: no I/O, no clock.
func SandboxPolicyFor(read SandboxRead, sandbox string, w ScenarioWindow, neighbours []ScenarioWindow) *report.SandboxPolicy {
	p := &report.SandboxPolicy{
		Source:  "loki",
		Sandbox: SafeSandboxID(sandbox),
		Window:  &report.SandboxPolicyWindow{From: stamp(w.From), To: stamp(w.To)},
		Events:  []report.SandboxPolicyEvent{},
	}
	if read.Err != nil {
		p.Coverage = report.CoverageUnavailable
		p.CoverageReason = "the sandbox evidence could not be read from Loki: " + read.Err.Error()
		return p
	}
	if strings.TrimSpace(sandbox) == "" {
		p.Coverage = report.CoverageUnavailable
		p.CoverageReason = "no sandbox id is configured (observability.openshell.sandbox), so no line can be tied to the SUT's sandbox"
		return p
	}

	type keptEvent struct {
		at time.Time
		ev report.SandboxPolicyEvent
	}
	var (
		attributed, unparsed, downgraded int
		downVersion                      string
		kept                             []keptEvent
	)
	for _, ln := range read.Lines {
		lokiAt, lokiOK := nanosTime(ln.TS)
		var o ocsfLine
		if err := json.Unmarshal([]byte(ln.Line), &o); err != nil || o.ClassUID == nil {
			// Not an OCSF JSON object (a shorthand line, a truncated line, a field of an unexpected
			// type). The range runs past the window to absorb ingestion lag, so the tail is cut here by
			// the Loki time; a line with no time at all is counted, which can only make coverage worse.
			if !lokiOK || w.holds(lokiAt) {
				unparsed++
			}
			continue
		}
		at, ok := time.Time{}, false
		if o.Time != nil && *o.Time > 0 {
			at, ok = time.UnixMilli(*o.Time), true // the event's own time wins (spec 26 A5)
		} else {
			at, ok = lokiAt, lokiOK
		}
		if ok && !w.holds(at) {
			continue
		}
		if o.downgraded() {
			downgraded++
			if downVersion == "" {
				// It is named in coverage_reason, so it is redacted and cut like an event field (#448).
				downVersion = cut(RedactTokens(strings.TrimSpace(o.Metadata.Version)), maxSandboxPolicyVersion)
			}
			continue
		}
		if o.Container.UID != sandbox {
			continue // another sandbox, or a record with no sandbox association (a gateway record)
		}
		if !ok {
			unparsed++ // ours, but with no time to place it in any window
			continue
		}
		attributed++
		if !o.kept() {
			continue
		}
		action := o.Action
		if action == "" {
			action = o.Disposition
		}
		kept = append(kept, keptEvent{at: at, ev: report.SandboxPolicyEvent{
			Time:    stamp(at),
			Class:   clip(o.class()),
			Action:  clip(action),
			Target:  clip(o.target()),
			Process: clip(o.Actor.Process.Name),
			Rule:    clip(o.FirewallRule.Name),
			Reason:  clip(safeReason(o.StatusDetail)),
		}})
	}

	switch {
	case downgraded > 0:
		if downVersion == "" {
			downVersion = "unknown"
		}
		p.Coverage = report.CoverageUnavailable
		p.CoverageReason = fmt.Sprintf("%d lines carry a downgraded OCSF schema (metadata.version %s); container is stripped, so lines cannot be tied to a sandbox", downgraded, downVersion)
	case attributed == 0 && unparsed > 0:
		p.Coverage = report.CoverageUnavailable
		p.CoverageReason = fmt.Sprintf("%d lines matched the sandbox id but are not OCSF JSON (a shorthand source?); A1 reads OCSF JSONL only", unparsed)
	case attributed == 0:
		// The liveness rule (, finding A6): Loki answers a selector that matches nothing with
		// HTTP 200 and an empty result, so an empty window can never read as "no denials".
		p.Coverage = report.CoverageUnavailable
		p.CoverageReason = fmt.Sprintf("no line from sandbox %s between %s and %s: a wrong selector, a wrong sandbox id, a schema downgrade, an idle sandbox or lost lines; Argus cannot tell these apart", sandbox, stamp(w.From), stamp(w.To))
	case read.Limit > 0 && len(read.Lines) >= read.Limit:
		p.Coverage = report.CoverageLossy
		p.CoverageReason = fmt.Sprintf("the query returned its limit of %d lines; later lines in the window were not read", read.Limit)
	case unparsed > 0:
		p.Coverage = report.CoverageLossy
		p.CoverageReason = fmt.Sprintf("%d lines could not be parsed as OCSF JSON", unparsed)
	default:
		p.Coverage = report.CoverageComplete
		p.CoverageReason = fmt.Sprintf("read %d lines of sandbox %s in the window; loss inside OpenShell before a line reached Loki is not visible here (spec 26 C6)", attributed, sandbox)
	}
	if p.Coverage == report.CoverageUnavailable {
		// Nothing was measured, so nothing derived from the lines is reported: no count (null), no
		// events, no shared_with — even when some lines did parse (a window where only part of the
		// lines are downgraded). A count here would read as "no denials" or as the window's total.
		return p
	}

	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if !a.at.Equal(b.at) {
			return a.at.Before(b.at)
		}
		ka := [...]string{a.ev.Class, a.ev.Target, a.ev.Process, a.ev.Rule, a.ev.Reason, a.ev.Action}
		kb := [...]string{b.ev.Class, b.ev.Target, b.ev.Process, b.ev.Rule, b.ev.Reason, b.ev.Action}
		for k := range ka {
			if ka[k] != kb[k] {
				return ka[k] < kb[k]
			}
		}
		return false
	})
	denied := len(kept)
	p.DeniedCount = &denied
	shared := map[string]bool{}
	for i, k := range kept {
		for _, n := range neighbours {
			if n.ID != w.ID && n.holds(k.at) {
				shared[n.ID] = true
			}
		}
		if i < maxSandboxPolicyEvents {
			p.Events = append(p.Events, k.ev)
		}
	}
	p.EventsOmitted = len(kept) - len(p.Events)
	for id := range shared {
		p.SharedWith = append(p.SharedWith, id)
	}
	sort.Strings(p.SharedWith)
	return p
}
