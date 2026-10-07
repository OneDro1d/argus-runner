package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const (
	sockCode = "Non HTTP response code: java.net.SocketException"
	sockMsg  = "Non HTTP response message: Network is unreachable"
)

func TestLoadErrorsGroupsTransportFailuresByReason(t *testing.T) {
	var ss []LoadSample
	for i := 0; i < 3; i++ {
		ss = append(ss, LoadSample{StartMs: 1000, ElapsedMs: 10, Code: "202", Message: "Accepted"})
	}
	for i := 0; i < 2; i++ {
		ss = append(ss, LoadSample{StartMs: 1000, ElapsedMs: 60000, Code: sockCode, Message: sockMsg})
	}
	ss = append(ss, LoadSample{StartMs: 1000, ElapsedMs: 5, Code: "Non HTTP response code: org.apache.http.conn.HttpHostConnectException", Message: "Non HTTP response message: Connection refused"})
	got := LoadErrorsFrom(ss, nil)
	want := []LoadError{
		{Reason: "java.net.SocketException: Network is unreachable", Count: 2},
		{Reason: "org.apache.http.conn.HttpHostConnectException: Connection refused", Count: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("errors = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("errors[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLoadErrorsNoneWhenEveryResponseWasNumeric(t *testing.T) {
	ss := []LoadSample{{StartMs: 1, ElapsedMs: 1, Code: "500", Message: "boom"}, {StartMs: 2, ElapsedMs: 1, Code: "202"}}
	if got := LoadErrorsFrom(ss, nil); got != nil {
		t.Errorf("a 500 is an answer, not a transport failure: %+v", got)
	}
}

func TestLoadErrorsCapsAtEightReasonsAndSumsTheRestUnderOther(t *testing.T) {
	var ss []LoadSample
	// ten distinct reasons; reason k occurs (10-k) times so the order is deterministic.
	for k := 0; k < 10; k++ {
		for n := 0; n < 10-k; n++ {
			ss = append(ss, LoadSample{StartMs: 1, ElapsedMs: 1, Code: fmt.Sprintf("Non HTTP response code: E%d", k), Message: "Non HTTP response message: m"})
		}
	}
	got := LoadErrorsFrom(ss, nil)
	if len(got) != MaxLoadErrorReasons+1 {
		t.Fatalf("len = %d, want %d (8 reasons + other): %+v", len(got), MaxLoadErrorReasons+1, got)
	}
	if got[0].Reason != "E0: m" || got[0].Count != 10 {
		t.Errorf("first = %+v", got[0])
	}
	last := got[len(got)-1]
	// reasons E8 (2) and E9 (1) fall into other
	if last.Reason != LoadErrorOther || last.Count != 3 {
		t.Errorf("other = %+v, want {other 3}", last)
	}
}

func TestLoadErrorsReasonIsScrubbedBeforeItIsTruncated(t *testing.T) {
	secret := "s3cr3t-value-XYZ"
	long := strings.Repeat("a", 150) + secret
	ss := []LoadSample{{StartMs: 1, ElapsedMs: 1, Code: "Non HTTP response code: X", Message: "Non HTTP response message: " + long}}
	scrub := func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED]") }
	got := LoadErrorsFrom(ss, scrub)
	if len(got) != 1 {
		t.Fatalf("errors = %+v", got)
	}
	if strings.Contains(got[0].Reason, "s3cr3t") {
		t.Errorf("a credential (or a cut piece of one) reached the reason: %q", got[0].Reason)
	}
	if n := len([]rune(got[0].Reason)); n > MaxLoadErrorReasonLen {
		t.Errorf("reason is %d runes, want <= %d", n, MaxLoadErrorReasonLen)
	}
}

func TestLoadTimelineBucketsByStartAndEnd(t *testing.T) {
	// 4 samples over 0..4.5 s. width 1 s (<=120 buckets).
	ss := []LoadSample{
		{StartMs: 10000, ElapsedMs: 500, Code: "202"},     // starts s0, answered s0
		{StartMs: 10200, ElapsedMs: 1500, Code: "202"},    // starts s0, answered s1
		{StartMs: 11000, ElapsedMs: 3500, Code: sockCode}, // starts s1, failed s4
		{StartMs: 12000, ElapsedMs: 100, Code: "500"},     // starts s2, answered s2
	}
	w, tl := LoadTimelineFrom(ss)
	if w != 1 {
		t.Fatalf("bucket width = %d, want 1", w)
	}
	if len(tl) != 5 {
		t.Fatalf("buckets = %d, want 5: %+v", len(tl), tl)
	}
	want := []LoadBucket{
		{AtS: 0, Started: 2, Answered: 1},
		{AtS: 1, Started: 1, Answered: 1},
		{AtS: 2, Started: 1, Answered: 1},
		{AtS: 3},
		{AtS: 4, Failed: 1},
	}
	for i := range want {
		if tl[i] != want[i] {
			t.Errorf("bucket %d = %+v, want %+v", i, tl[i], want[i])
		}
	}
}

func TestLoadTimelineIsBoundedTo120Buckets(t *testing.T) {
	ss := []LoadSample{{StartMs: 0 + 1000, ElapsedMs: 10, Code: "202"}, {StartMs: 1000 + 1000*1000, ElapsedMs: 10, Code: "202"}}
	w, tl := LoadTimelineFrom(ss)
	if len(tl) > MaxLoadTimelineBuckets {
		t.Fatalf("%d buckets, cap is %d", len(tl), MaxLoadTimelineBuckets)
	}
	if w != 9 { // 1000.01 s span: 9 s -> 112 buckets; 8 s -> 126 buckets
		t.Errorf("width = %d, want 9", w)
	}
	started, answered := 0, 0
	for _, b := range tl {
		started += b.Started
		answered += b.Answered
	}
	if started != 2 || answered != 2 {
		t.Errorf("every sample must be counted once: started=%d answered=%d", started, answered)
	}
}

func TestLoadTimelineOmittedBelowTwoSamples(t *testing.T) {
	if _, tl := LoadTimelineFrom([]LoadSample{{StartMs: 1, ElapsedMs: 1, Code: "202"}}); tl != nil {
		t.Errorf("one sample: %+v", tl)
	}
	if _, tl := LoadTimelineFrom(nil); tl != nil {
		t.Errorf("no samples: %+v", tl)
	}
}

// An old record (no errors, no timeline) still reads, and a record without them writes neither key.
func TestLoadStatsOldRecordStillReadsAndNewFieldsAreOmittedWhenEmpty(t *testing.T) {
	var ls LoadStats
	if err := json.Unmarshal([]byte(`{"samples":10,"p50_ms":50,"p95_ms":100,"p99_ms":100,"error_rate":0.2}`), &ls); err != nil {
		t.Fatal(err)
	}
	if ls.Samples != 10 || ls.Errors != nil || ls.Timeline != nil {
		t.Errorf("old record read as %+v", ls)
	}
	b, _ := json.Marshal(ls)
	if strings.Contains(string(b), "errors") || strings.Contains(string(b), "timeline") {
		t.Errorf("empty new fields must not be written: %s", b)
	}
}
