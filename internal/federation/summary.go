package federation

import (
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// UI-7a (A'3) -- summary readings: a handful of NAMED NUMBERS an executor reads from
// the environment's own metrics source and reports on its poll. The raw series never leave the
// environment; this is the whole contract that does.

const (
	// SummaryMaxReadings is the most readings one instance may declare, send or have stored.
	SummaryMaxReadings = 12
	// SummaryMinEvery / SummaryMaxEvery / SummaryDefaultEvery bound how often the executor reads.
	SummaryMinEvery     = time.Minute
	SummaryMaxEvery     = time.Hour
	SummaryDefaultEvery = 5 * time.Minute
	// SummaryErrorMax is the longest error text carried on the wire or stored (characters).
	SummaryErrorMax = 200
	// SummaryNameMax / SummaryUnitMax / SummaryTargetMax bound the label fields.
	SummaryNameMax   = 40
	SummaryUnitMax   = 16
	SummaryTargetMax = 40
)

// SummaryReading is one named number. Value is a pointer on purpose: nil means "the source could not be
// read", and then Error says why. An unreadable source is NEVER reported as 0.
type SummaryReading struct {
	Name             string    `json:"name"`
	Target           string    `json:"target,omitempty"`
	Unit             string    `json:"unit,omitempty"`
	Value            *float64  `json:"value"`
	Error            string    `json:"error,omitempty"`
	ComfortableLimit *float64  `json:"comfortable_limit,omitempty"`
	EverySeconds     int       `json:"every_seconds,omitempty"`
	ObservedAt       time.Time `json:"observed_at"`
}

var (
	summaryURLRE    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^\s"'<>()\[\]{}]+`)
	summaryAddrRE   = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`)
	summaryKVRE     = regexp.MustCompile(`(?i)\b(access_token|api_key|apikey|token|secret|password|passwd|pwd|key|auth)=\S+`)
	summaryBearerRE = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=\-]{4,}`)
	summaryNameRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
)

// ScrubReadingError makes an error text safe to send, store and log: every scheme://... URL becomes
// <url> (a source URL can carry userinfo, a key in its path or a ?token= -- the way the credential
// banner and SanitizeChainError treat theirs), IPv4 addresses become <addr>, key=value pairs that look
// like credentials and Bearer/Basic values are masked, every secret passed in (the resolved credential
// and its parts) is replaced wherever it appears, control characters become spaces, and the result is cut
// to SummaryErrorMax characters on a rune boundary.
func ScrubReadingError(s string, secrets ...string) string {
	for _, sec := range secrets {
		for _, part := range append([]string{sec}, strings.Split(sec, ":")...) {
			if len(part) >= 3 {
				s = strings.ReplaceAll(s, part, "<redacted>")
			}
		}
	}
	s = summaryURLRE.ReplaceAllString(s, "<url>")
	s = summaryAddrRE.ReplaceAllString(s, "<addr>")
	s = summaryKVRE.ReplaceAllString(s, "$1=<redacted>")
	s = summaryBearerRE.ReplaceAllString(s, "$1 <redacted>")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return cutRunes(strings.ToValidUTF8(s, ""), SummaryErrorMax)
}

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// NormalizeSummaryReadings is the control plane's bound on what an executor sent -- it trusts nothing:
// at most SummaryMaxReadings, each name valid and unique (the first occurrence wins), labels cut, a
// non-finite or absent value becomes UNREADABLE with a reason (never 0), a value never travels with an
// error, the error is scrubbed again, and observed_at is clamped to the receive time so a wrong clock
// cannot make a reading look permanently fresh.
func NormalizeSummaryReadings(in []SummaryReading, now time.Time) []SummaryReading {
	out := make([]SummaryReading, 0, len(in))
	seen := map[string]bool{}
	for _, r := range in {
		if len(out) >= SummaryMaxReadings {
			break
		}
		if !summaryNameRE.MatchString(r.Name) || seen[r.Name] {
			continue
		}
		seen[r.Name] = true
		r.Unit = cutRunes(strings.TrimSpace(ScrubReadingError(r.Unit)), SummaryUnitMax)
		r.Target = cutRunes(strings.TrimSpace(ScrubReadingError(r.Target)), SummaryTargetMax)
		switch {
		case r.Value != nil && !finite(*r.Value):
			r.Value = nil
			r.Error = "the source returned a non-finite value"
		case r.Value != nil:
			r.Error = ""
		default:
			r.Error = ScrubReadingError(r.Error)
			if r.Error == "" {
				r.Error = "unreadable (no reason reported)"
			}
		}
		if r.ComfortableLimit != nil && !finite(*r.ComfortableLimit) {
			r.ComfortableLimit = nil
		}
		if r.EverySeconds < int(SummaryMinEvery/time.Second) || r.EverySeconds > int(SummaryMaxEvery/time.Second) {
			r.EverySeconds = 0
		}
		if r.ObservedAt.IsZero() || r.ObservedAt.After(now) {
			r.ObservedAt = now
		}
		out = append(out, r)
	}
	return out
}
