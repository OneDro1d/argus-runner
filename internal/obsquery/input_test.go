package obsquery

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Unit 4 (M25-FX3): the retrieval tools must give HONEST errors. An input error is never
// reported as "Loki unreachable" (4.2); correlation_id is required (4.4); a run-* id where a
// tr-* is expected is caught (4.10); a bad window is a window error (4.5).
func TestSagas_InputErrorsAreHonest(t *testing.T) {
	l := &Loki{BaseURL: "http://127.0.0.1:1"} // would fail if reached — input errors must short-circuit
	cases := []struct{ name, corr, window, mustContain, mustNotContain string }{
		{"empty corr", "", "10m", "required", "unreachable"},
		{"run id", "run-abc123def", "10m", "run_id", "unreachable"},
		{"bad window", "tr-20260902T134305716-FX-ABC-00000003", "banana", "window", "unreachable"},
		{"zero window", "tr-20260902T134305716-FX-ABC-00000003", "0s", "window", "unreachable"},
		{"negative window", "tr-20260902T134305716-FX-ABC-00000003", "-5m", "window", "unreachable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := l.Sagas(c.corr, c.window, time.Time{})
			if s.Available {
				t.Fatalf("%s: should be unavailable", c.name)
			}
			lo := strings.ToLower(s.Note)
			if !strings.Contains(lo, c.mustContain) {
				t.Errorf("%s: note should mention %q, got %q", c.name, c.mustContain, s.Note)
			}
			if strings.Contains(lo, c.mustNotContain) {
				t.Errorf("%s: note must NOT blame %q, got %q", c.name, c.mustNotContain, s.Note)
			}
		})
	}
}

func TestParseWindowStrict(t *testing.T) {
	for _, c := range []struct {
		in      string
		wantErr bool
	}{
		{"", false}, {"10m", false}, {"1h", false}, {"1s", false},
		{"banana", true}, {"-5m", true}, {"0s", true}, {"10min", true},
	} {
		if _, err := parseWindowStrict(c.in); (err != nil) != c.wantErr {
			t.Errorf("parseWindowStrict(%q): err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
	}
}

// 4.3: an empty-but-successful query lists BOTH causes (unknown id / outside window),
// not just the misleading "widen window".
func TestSagas_EmptyResultListsBothCauses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer srv.Close()
	s := (&Loki{BaseURL: srv.URL}).Sagas("tr-20260902T134305716-FX-NONE-00000004", "10m", time.Time{})
	if s.Available {
		t.Fatal("empty result should be unavailable")
	}
	lo := strings.ToLower(s.Note)
	if strings.Contains(lo, "unreachable") {
		t.Errorf("a successful empty query must NOT say unreachable: %s", s.Note)
	}
	if !strings.Contains(lo, "unknown") || !strings.Contains(lo, "window") {
		t.Errorf("empty note should list BOTH causes (unknown id / outside window): %s", s.Note)
	}
}

// C7 (M25-FX4): get_sagas also echoes the EFFECTIVE (parsed) window, like get_tail_logs.
func TestSagas_EchoesEffectiveWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer srv.Close()
	s := (&Loki{BaseURL: srv.URL}).Sagas("tr-20260902T134305716-FX-ABC-00000003", "10m", time.Time{})
	if s.Window != "10m0s" {
		t.Errorf("get_sagas should echo the effective window 10m0s, got %q", s.Window)
	}
}

// 4.5: Logs echoes the EFFECTIVE (parsed) window, not the raw request string.
func TestLogs_EchoesEffectiveWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer srv.Close()
	out := (&Loki{BaseURL: srv.URL}).Logs("tr-20260902T134305716-FX-ABC-00000003", "10m", time.Time{})
	if out.Window != "10m0s" {
		t.Errorf("window should be echoed as the effective parsed value (10m0s), got %q", out.Window)
	}
}
