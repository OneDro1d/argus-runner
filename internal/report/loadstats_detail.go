package report

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LoadSample is one SUT-trigger row of the .jtl as the load detail needs it: the raw responseCode and
// responseMessage text (a transport failure has no number there: "Non HTTP response code: <class>").
type LoadSample struct {
	StartMs   int64 // .jtl timeStamp, epoch ms, the sample's START; 0 = absent
	ElapsedMs int
	Code      string
	Message   string
}

// LoadError is one reason a request got no answer, and how many did.
type LoadError struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// LoadBucket is one slice of the run's timeline. Started counts samples by START time; Answered counts
// samples with a numeric status, and Failed counts transport failures, both by END time (start + elapsed).
type LoadBucket struct {
	AtS      int `json:"at_s"`
	Started  int `json:"started"`
	Answered int `json:"answered"`
	Failed   int `json:"failed"`
}

const (
	// MaxLoadErrorReasons bounds the distinct reasons listed; the rest are summed under LoadErrorOther.
	MaxLoadErrorReasons = 8
	// MaxLoadErrorReasonLen bounds one reason, in runes.
	MaxLoadErrorReasonLen = 160
	// MaxLoadTimelineBuckets bounds the timeline.
	MaxLoadTimelineBuckets = 120
	// LoadErrorOther is the reason of the summed remainder.
	LoadErrorOther = "other"

	nonHTTPCodePrefix = "Non HTTP response code:"
	nonHTTPMsgPrefix  = "Non HTTP response message:"
)

// transportFailed reports whether a sample got no HTTP status: its responseCode is not a number.
func (s LoadSample) transportFailed() bool {
	_, err := strconv.Atoi(strings.TrimSpace(s.Code))
	return err != nil
}

// loadErrorReason builds "<class>: <message>" from the JMeter texts, whitespace collapsed.
func loadErrorReason(s LoadSample) string {
	class := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s.Code), nonHTTPCodePrefix))
	msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s.Message), nonHTTPMsgPrefix))
	r := class
	if msg != "" {
		if r != "" {
			r += ": "
		}
		r += msg
	}
	if r == "" {
		r = "transport failure (no detail)"
	}
	return normalizeReason(strings.Join(strings.Fields(r), " "))
}

var (
	reasonURLRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^\s/?#]*`)
	// "Illegal character in <part> at index N: <url>": java.net.URISyntaxException quotes the URL up to
	// the end of the text, spaces included, so everything after that colon is the URL.
	reasonURISyntaxRe = regexp.MustCompile(`at index \d+: `)
	reasonQueryRe     = regexp.MustCompile(`\?\S*`)
	reasonKVListRe    = regexp.MustCompile(`(^|\s)[\w.\-]+=\S*&\S*`)
	reasonHostPortRe  = regexp.MustCompile(`([A-Za-z][A-Za-z0-9.\-]*|(?:\d{1,3}\.){3}\d{1,3}):\d+`)
	reasonIPPortRe    = regexp.MustCompile(`^(?:\d{1,3}\.){3}\d{1,3}:\d+$`)
)

// normalizeReason removes what must not be reported and what stops equal reasons from grouping. The
// reason text comes from the target and the network, so this runs at its source, before any credential
// scrub and before truncation. It removes, and nothing else:
//   - from every URL (scheme://...): the userinfo, the path, the query and the fragment, leaving
//     scheme://host[:port]. A URL quoted by a java.net.URISyntaxException ("at index N: <url>") runs to
//     the end of the text, spaces included.
//   - from the text after a URL is gone: a bare ?query (up to the next space) and a key=value list
//     joined with & that follows a path.
//
// For grouping, an IPv4 literal followed by :<port> keeps the address and gets :* ONLY when the text
// names an earlier host:port (the target, whose own port is never touched): a connection error
// repeats the target and adds the ephemeral local address, "Connect to sut:8080 failed: /10.244.5.62:34567".
// Two different targets stay two reasons; the class name and the plain words survive.
func normalizeReason(r string) string {
	r = reduceURLs(r)
	r = reasonQueryRe.ReplaceAllString(r, "")
	r = strings.TrimSpace(reasonKVListRe.ReplaceAllString(r, "$1"))
	seen := false
	return reasonHostPortRe.ReplaceAllStringFunc(r, func(m string) string {
		if !seen {
			seen = true
			return m
		}
		if reasonIPPortRe.MatchString(m) {
			return m[:strings.LastIndex(m, ":")] + ":*"
		}
		return m
	})
}

// reduceURLs cuts every URL in r to scheme://host[:port].
func reduceURLs(r string) string {
	var b strings.Builder
	for {
		loc := reasonURLRe.FindStringIndex(r)
		if loc == nil {
			b.WriteString(r)
			return b.String()
		}
		b.WriteString(r[:loc[0]])
		u := r[loc[0]:loc[1]]
		if i := strings.Index(u, "://"); i >= 0 {
			if at := strings.LastIndex(u, "@"); at > i {
				u = u[:i+3] + u[at+1:]
			}
		}
		b.WriteString(u)
		rest := r[loc[1]:]
		if reasonURISyntaxRe.MatchString(r[:loc[0]]) {
			// the rest of the text is this URL's path, query and spaces
			return b.String()
		}
		// drop the path, query and fragment: up to the next space
		if rest != "" && (rest[0] == '/' || rest[0] == '?' || rest[0] == '#') {
			if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
				rest = rest[sp:]
			} else {
				rest = ""
			}
		}
		r = rest
	}
}

// LoadErrorsFrom groups the transport failures of samples by reason: at most MaxLoadErrorReasons
// entries, most frequent first (ties by reason), the rest summed under LoadErrorOther. The reason text
// comes from the target and the network, so scrub (when set) runs on the full text BEFORE it is cut to
// MaxLoadErrorReasonLen: a cut could otherwise leave a credential's prefix that no scrub recognises.
func LoadErrorsFrom(samples []LoadSample, scrub func(string) string) []LoadError {
	counts := map[string]int{}
	for _, s := range samples {
		if !s.transportFailed() {
			continue
		}
		r := loadErrorReason(s)
		if scrub != nil {
			r = scrub(r)
		}
		if rs := []rune(r); len(rs) > MaxLoadErrorReasonLen {
			r = string(rs[:MaxLoadErrorReasonLen])
		}
		counts[r]++
	}
	if len(counts) == 0 {
		return nil
	}
	out := make([]LoadError, 0, len(counts))
	for r, n := range counts {
		out = append(out, LoadError{Reason: r, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Reason < out[j].Reason
	})
	if len(out) > MaxLoadErrorReasons {
		rest := 0
		for _, e := range out[MaxLoadErrorReasons:] {
			rest += e.Count
		}
		out = append(out[:MaxLoadErrorReasons], LoadError{Reason: LoadErrorOther, Count: rest})
	}
	return out
}

// LoadTimelineFrom buckets samples over the span from the first start to the last end, in buckets of
// the smallest whole number of seconds that keeps them at or under MaxLoadTimelineBuckets. It returns
// that width and the buckets, or (0, nil) below two samples with a start time.
func LoadTimelineFrom(samples []LoadSample) (int, []LoadBucket) {
	var timed []LoadSample
	for _, s := range samples {
		if s.StartMs > 0 {
			if s.ElapsedMs < 0 {
				// a corrupt row: a negative elapsed counts as 0, so the sample ends where it started
				s.ElapsedMs = 0
			}
			timed = append(timed, s)
		}
	}
	if len(timed) < 2 {
		return 0, nil
	}
	first, last := timed[0].StartMs, timed[0].StartMs+int64(timed[0].ElapsedMs)
	for _, s := range timed {
		if s.StartMs < first {
			first = s.StartMs
		}
		if end := s.StartMs + int64(s.ElapsedMs); end > last {
			last = end
		}
	}
	span := last - first
	w := 1
	for (span+int64(w)*1000-1)/(int64(w)*1000) > MaxLoadTimelineBuckets {
		w++
	}
	widthMs := int64(w) * 1000
	n := int((span + widthMs - 1) / widthMs)
	if n < 1 {
		n = 1
	}
	b := make([]LoadBucket, n)
	for i := range b {
		b[i].AtS = i * w
	}
	idx := func(ms int64) int {
		i := int((ms - first) / widthMs)
		if i < 0 {
			i = 0
		}
		if i >= n {
			i = n - 1
		}
		return i
	}
	for _, s := range timed {
		b[idx(s.StartMs)].Started++
		e := idx(s.StartMs + int64(s.ElapsedMs))
		if s.transportFailed() {
			b[e].Failed++
		} else {
			b[e].Answered++
		}
	}
	return w, b
}
