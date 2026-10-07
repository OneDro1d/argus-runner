package obsquery

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// a caller-supplied correlation id reached a LogQL string literal raw, and the only
// checks were "" and a run- prefix. So (1) ANY substring of a line was accepted ("tr-" matched every
// line of every run in the window, certification runs included — the ids a builder is deliberately
// never given), and (2) a `"` or `\` broke out of the literal and rewrote the query.
//
// The rule: a correlation id is EXACTLY the shape Argus mints, tr-<run_id>-<scenario_id>-<8 hex>
// (argus.newScenarioCorrelationID), and it is refused BEFORE any backend is contacted.

const canonicalCorr = "tr-20260902T134305716-ORDE-017-3f9a1c2b"

// goodCorrs are ids the real generator can produce. The scenario id is validate.go's idFormat
// (`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`), so it may itself contain dashes and underscores.
var goodCorrs = []string{
	canonicalCorr,
	"tr-20260902T134305716-A-00000000",
	"tr-20260902T134305716-ord_017-3f9a1c2b",
	"tr-20260902T134305716-Mixed-Case_and-dashes-9-3f9a1c2b",
	"tr-20260902T134305716-" + strings.Repeat("S", 64) + "-3f9a1c2b",
	"tr-20260902T134305-ORDE-017-3f9a1c2b",       // pre-ms run id (15 chars), still in old ledgers
	"tr-20260902T134305a1b2c3-ORDE-017-3f9a1c2b", // pre-ms run id + 6 random hex (R7)
}

// badCorrs are everything else. Each is a way to widen the match or break the literal.
var badCorrs = map[string]string{
	"bare prefix":                "tr-",
	"short fixture":              "tr-x",
	"run-id prefix":              "tr-20260902T134305716",
	"run id + dash":              "tr-20260902T134305716-",
	"run + scenario, no hash":    "tr-20260902T134305716-ORDE-017",
	"seven hex":                  "tr-20260902T134305716-ORDE-017-3f9a1c2",
	"nine hex":                   "tr-20260902T134305716-ORDE-017-3f9a1c2bd",
	"uppercase hex":              "tr-20260902T134305716-ORDE-017-3F9A1C2B",
	"non-hex hash":               "tr-20260902T134305716-ORDE-017-zzzzzzzz",
	"legacy tr-16hex":            "tr-0123456789abcdef",
	"wildcard scenario":          "tr-20260902T134305716-.*-3f9a1c2b",
	"regex alternation":          "tr-20260902T134305716-A|B-3f9a1c2b",
	"double quote":               `tr-20260902T134305716-ORDE"-3f9a1c2b`,
	"quote breakout":             `tr-20260902T134305716-A" or {x=~".+"} |= "-3f9a1c2b`,
	"backslash":                  `tr-20260902T134305716-A\-3f9a1c2b`,
	"trailing backslash":         `tr-20260902T134305716-ORDE-017-3f9a1c2b\`,
	"embedded newline":           "tr-20260902T134305716-ORDE\n-017-3f9a1c2b",
	"embedded space":             "tr-20260902T134305716-ORDE 017-3f9a1c2b",
	"nul":                        "tr-20260902T134305716-ORDE\x00-3f9a1c2b",
	"scenario over 64":           "tr-20260902T134305716-" + strings.Repeat("S", 65) + "-3f9a1c2b",
	"scenario starts with dash":  "tr-20260902T134305716--ORDE-3f9a1c2b",
	"id with suffix":             canonicalCorr + ".child",
	"upper-case prefix":          "TR-20260902T134305716-ORDE-017-3f9a1c2b",
	"leading garbage":            "x" + canonicalCorr,
	"unicode lookalike scenario": "tr-20260902T134305716-ORDЕ-017-3f9a1c2b",
}

func TestValidateCorrelationID_ShapeArgusMints(t *testing.T) {
	for _, c := range goodCorrs {
		if err := ValidateCorrelationID(c); err != nil {
			t.Errorf("a minted id must be accepted: %q -> %v", c, err)
		}
	}
	for name, c := range badCorrs {
		err := ValidateCorrelationID(c)
		if err == nil {
			t.Errorf("%s: %q must be refused", name, c)
			continue
		}
		if !strings.Contains(err.Error(), "correlation_id") {
			t.Errorf("%s: the refusal must name the correlation_id, got %q", name, err)
		}
	}
}

// hitCounter is a Loki stand-in that records every query it is sent.
type hitCounter struct {
	mu      sync.Mutex
	queries []string
}

func (h *hitCounter) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.queries = append(h.queries, r.URL.Query().Get("query"))
		h.mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	})
}

func (h *hitCounter) n() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.queries)
}

// The refusal must happen BEFORE the backend is contacted: a widened or injected query that is
// merely "answered with nothing" is still the defect, so the proof is zero requests.
func TestLoki_RefusesNonCanonicalCorrelationID_BeforeAnyQuery(t *testing.T) {
	for name, c := range badCorrs {
		t.Run(name, func(t *testing.T) {
			h := &hitCounter{}
			srv := httptest.NewServer(h.handler())
			defer srv.Close()
			l := &Loki{BaseURL: srv.URL}

			s := l.Sagas(c, "10m", time.Time{})
			if s.Available || !strings.Contains(s.Note, "correlation_id") {
				t.Errorf("Sagas(%q): want unavailable + a correlation_id refusal, got available=%v note=%q", c, s.Available, s.Note)
			}
			lg := l.Logs(c, "10m", time.Time{})
			if lg.Available || !strings.Contains(lg.Note, "correlation_id") {
				t.Errorf("Logs(%q): want unavailable + a correlation_id refusal, got available=%v note=%q", c, lg.Available, lg.Note)
			}
			if got := h.n(); got != 0 {
				t.Errorf("%q reached Loki %d time(s): %v", c, got, h.queries)
			}
		})
	}
}

func TestBetterStack_RefusesNonCanonicalCorrelationID_BeforeAnyQuery(t *testing.T) {
	for name, c := range badCorrs {
		t.Run(name, func(t *testing.T) {
			f, ts := newBSFakeServer(t, map[string][]bsRow{"accounting": {}})
			defer ts.Close()
			b := &BetterStack{QueryURL: ts.URL, TeamID: "123456", Sources: map[string]string{"accounting": "accounting-service"}}

			s := b.Sagas(c, "10m", time.Time{})
			if s.Available || !strings.Contains(s.Note, "correlation_id") {
				t.Errorf("Sagas(%q): want unavailable + a correlation_id refusal, got available=%v note=%q", c, s.Available, s.Note)
			}
			lg := b.Logs(c, "10m", time.Time{})
			if lg.Available || !strings.Contains(lg.Note, "correlation_id") {
				t.Errorf("Logs(%q): want unavailable + a correlation_id refusal, got available=%v note=%q", c, lg.Available, lg.Note)
			}
			if len(f.mu) != 0 {
				t.Errorf("%q reached BetterStack %d time(s)", c, len(f.mu))
			}
		})
	}
}

// A minted id still gets through, and is queried EXACTLY (trimmed of surrounding space, as the
// validator reads it — the query must carry what was validated, not the raw argument).
func TestLoki_CanonicalCorrelationID_IsQueried(t *testing.T) {
	for _, c := range goodCorrs {
		h := &hitCounter{}
		srv := httptest.NewServer(h.handler())
		l := &Loki{BaseURL: srv.URL}
		_ = l.Logs("  "+c+" \n", "10m", time.Time{})
		srv.Close()
		if h.n() != 1 {
			t.Fatalf("%q: want exactly one query, got %d", c, h.n())
		}
		want := `{service=~".+",service!~"` + ObsInfraExclusion + `"} |= "` + c + `"`
		if h.queries[0] != want {
			t.Errorf("query = %q, want %q", h.queries[0], want)
		}
	}
}

// Defence in depth, tested at the layer that builds the literal (validation cannot save us here:
// this calls queryLines directly). A `"` or `\` in the value must stay INSIDE the string literal.
func TestQueryLines_EscapesTheLogQLLiteral(t *testing.T) {
	cases := map[string]string{
		"quote breakout": `a"} or {x=~".+"} |= "b`,
		"backslash":      `a\b`,
		"trailing slash": `ab\`,
		"quote+slash":    `a\"b`,
		"newline":        "a\nb",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			h := &hitCounter{}
			srv := httptest.NewServer(h.handler())
			defer srv.Close()
			l := &Loki{BaseURL: srv.URL}
			if _, ok := l.queryLines(raw, "10m", time.Time{}); !ok {
				t.Fatal("query was not sent")
			}
			if h.n() != 1 {
				t.Fatalf("want one query, got %d", h.n())
			}
			q := h.queries[0]
			prefix := `{service=~".+",service!~"` + ObsInfraExclusion + `"} |= "`
			if !strings.HasPrefix(q, prefix) || !strings.HasSuffix(q, `"`) {
				t.Fatalf("query lost its shape: %q", q)
			}
			lit := q[len(prefix) : len(q)-1]
			// Walk the literal: every `"` must be preceded by an escaping backslash, and a `\` must
			// escape the next character — i.e. the literal can never close early.
			for i := 0; i < len(lit); i++ {
				switch lit[i] {
				case '\\':
					i++ // the escaped character is consumed
					if i >= len(lit) {
						t.Fatalf("literal ends in a dangling backslash that would eat the closing quote: %q", q)
					}
				case '"':
					t.Fatalf("an unescaped quote closes the literal early: %q", q)
				case '\n':
					t.Fatalf("a raw newline inside the literal: %q", q)
				}
			}
			// And it must say what the caller said: unquoting the literal gives the original value.
			if got := unescapeLogQL(t, lit); got != raw {
				t.Errorf("literal %q decodes to %q, want %q", lit, got, raw)
			}
		})
	}
}

// unescapeLogQL decodes a LogQL double-quoted string body (Go-style escapes).
func unescapeLogQL(t *testing.T, lit string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < len(lit); i++ {
		if lit[i] != '\\' {
			b.WriteByte(lit[i])
			continue
		}
		i++
		switch lit[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		default:
			b.WriteByte(lit[i])
		}
	}
	return b.String()
}
