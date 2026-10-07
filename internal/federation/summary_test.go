package federation

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func f64(v float64) *float64 { return &v }

func TestScrubReadingError_StripsURLsCredentialsAndTruncates(t *testing.T) {
	cases := []struct {
		name, in string
		secrets  []string
		banned   []string
	}{
		{"url with userinfo", `Get "http://admin:hunter2@prom.lab:9090/api/v1/query?query=up": dial tcp 10.0.0.1:9090: connect: connection refused`, nil,
			[]string{"hunter2", "admin", "prom.lab", "/api/v1"}},
		{"url with token in query", `Get https://m.example/metrics?token=abc123secret: EOF`, nil, []string{"abc123secret", "m.example"}},
		{"bare key=value", `upstream said api_key=ZZZ999 is invalid`, nil, []string{"ZZZ999"}},
		{"bearer header echoed", `401 Authorization: Bearer eyJhbGciOi.payload.sig rejected`, nil, []string{"eyJhbGciOi"}},
		{"resolved credential appears bare", `bad login for svc:p4ssw0rd on host`, []string{"svc:p4ssw0rd", "p4ssw0rd"}, []string{"p4ssw0rd"}},
	}
	for _, c := range cases {
		got := ScrubReadingError(c.in, c.secrets...)
		for _, b := range c.banned {
			if strings.Contains(got, b) {
				t.Errorf("%s: %q still contains %q", c.name, got, b)
			}
		}
		if got == "" {
			t.Errorf("%s: scrubbed to empty; the reason must survive", c.name)
		}
	}
	long := strings.Repeat("é", 500)
	got := ScrubReadingError(long)
	if n := utf8.RuneCountInString(got); n > SummaryErrorMax {
		t.Errorf("not truncated: %d runes", n)
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncation split a rune")
	}
	if got := ScrubReadingError("line one\nline two\x00\ttab"); strings.ContainsAny(got, "\n\x00\t") {
		t.Errorf("control characters survived: %q", got)
	}
}

func TestNormalizeSummaryReadings_BoundsAndValidates(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	var in []SummaryReading
	for i := 0; i < 20; i++ {
		in = append(in, SummaryReading{Name: "m" + string(rune('a'+i)), Value: f64(1), ObservedAt: now})
	}
	out := NormalizeSummaryReadings(in, now)
	if len(out) != SummaryMaxReadings {
		t.Fatalf("len = %d, want the bound %d", len(out), SummaryMaxReadings)
	}

	out = NormalizeSummaryReadings([]SummaryReading{
		{Name: "Bad Name", Value: f64(1), ObservedAt: now},
		{Name: "", Value: f64(1), ObservedAt: now},
		{Name: "ok_one", Value: f64(1), ObservedAt: now},
		{Name: "ok_one", Value: f64(2), ObservedAt: now}, // duplicate: first wins
	}, now)
	if len(out) != 1 || out[0].Name != "ok_one" || *out[0].Value != 1 {
		t.Fatalf("invalid/duplicate names not dropped: %+v", out)
	}

	out = NormalizeSummaryReadings([]SummaryReading{
		{Name: "nan", Value: f64(math.NaN()), ObservedAt: now},
		{Name: "inf", Value: f64(math.Inf(1)), ObservedAt: now},
		{Name: "noreason", Value: nil, ObservedAt: now},
		{Name: "leaky", Value: nil, Error: `Get http://u:pw@h:1/x?token=SECRET: refused`, ObservedAt: now},
		{Name: "both", Value: f64(3), Error: "stale error", ObservedAt: now},
	}, now)
	if len(out) != 5 {
		t.Fatalf("len = %d: %+v", len(out), out)
	}
	for _, r := range out[:3] {
		if r.Value != nil || r.Error == "" {
			t.Errorf("%s: non-finite/absent value must become unreadable WITH a reason, got value=%v err=%q", r.Name, r.Value, r.Error)
		}
	}
	if strings.Contains(out[3].Error, "SECRET") || strings.Contains(out[3].Error, "pw") {
		t.Errorf("CP-side scrub missed a credential: %q", out[3].Error)
	}
	if out[4].Value == nil || out[4].Error != "" {
		t.Errorf("a reading with a value must not also carry an error: %+v", out[4])
	}

	out = NormalizeSummaryReadings([]SummaryReading{
		{Name: "future", Value: f64(1), ObservedAt: now.Add(48 * time.Hour)},
		{Name: "zero", Value: f64(1)},
		{Name: "limits", Value: f64(1), ObservedAt: now, Unit: strings.Repeat("u", 99), Target: strings.Repeat("t", 99), EverySeconds: 9999999},
	}, now)
	if len(out) != 3 {
		t.Fatalf("len = %d", len(out))
	}
	if out[0].ObservedAt.After(now) || out[1].ObservedAt.After(now) || out[1].ObservedAt.IsZero() {
		t.Errorf("observed_at must be clamped to the receive time: %v %v", out[0].ObservedAt, out[1].ObservedAt)
	}
	if len(out[2].Unit) > SummaryUnitMax || len(out[2].Target) > SummaryTargetMax {
		t.Errorf("label bounds not enforced: %+v", out[2])
	}
	if out[2].EverySeconds != 0 {
		t.Errorf("an absurd every_seconds must be dropped, got %d", out[2].EverySeconds)
	}
}

// An executor that sends readings, an old control plane that does not know the field, and an old
// executor that never sends it: none of the three may break, and unreadable must stay null.
func TestSummaryReadings_WireRoundTripAndOldPeers(t *testing.T) {
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	req := PollRequest{RunnerVersion: "0.3.60", SummaryReadings: []SummaryReading{
		{Name: "agents_connected", Target: "live", Unit: "agents", Value: f64(212), ObservedAt: at, EverySeconds: 300},
		{Name: "msgs_per_s", Unit: "msg/s", Value: nil, Error: "prometheus: HTTP 503", ObservedAt: at},
	}}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	_ = json.Unmarshal(b, &generic)
	rs, _ := generic["summary_readings"].([]any)
	if len(rs) != 2 {
		t.Fatalf("summary_readings missing from the wire: %s", b)
	}
	if v, present := rs[1].(map[string]any)["value"]; !present || v != nil {
		t.Errorf("an unreadable reading must carry an explicit null value, never 0 and never absent: %s", b)
	}
	var back PollRequest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.SummaryReadings) != 2 || *back.SummaryReadings[0].Value != 212 || back.SummaryReadings[1].Value != nil ||
		!back.SummaryReadings[0].ObservedAt.Equal(at) {
		t.Fatalf("round trip lost data: %+v", back.SummaryReadings)
	}

	// OLD CONTROL PLANE: a decoder that has never heard of the field (the CP decodes without
	// DisallowUnknownFields). Modelled by a struct with only the pre-UI-7a poll fields.
	type oldPoll struct {
		RunnerVersion string `json:"runner_version"`
	}
	var old oldPoll
	if err := json.Unmarshal(b, &old); err != nil || old.RunnerVersion != "0.3.60" {
		t.Fatalf("old control plane choked on the new field: %v %+v", err, old)
	}

	// OLD EXECUTOR / no summary_metrics: the key is absent from the body entirely.
	nb, _ := json.Marshal(PollRequest{RunnerVersion: "0.3.50"})
	if strings.Contains(string(nb), "summary_readings") {
		t.Errorf("an executor with no readings must not emit the key: %s", nb)
	}
	var fresh PollRequest
	if err := json.Unmarshal(nb, &fresh); err != nil || fresh.SummaryReadings != nil {
		t.Errorf("new CP must read an old poll as no readings: %v %+v", err, fresh.SummaryReadings)
	}
}
