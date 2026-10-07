package report

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A corrupt .jtl row with a negative elapsed must not take the executor down: it counts as 0.
func TestLoadTimelineFrom_NegativeElapsedDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANIC: %v", r)
		}
	}()
	s := []LoadSample{
		{StartMs: 1_000_000, ElapsedMs: -5000, Code: "200"},
		{StartMs: 1_003_000, ElapsedMs: 10, Code: "200"},
		{StartMs: 1_002_000, ElapsedMs: -1 << 30, Code: "Non HTTP response code: x"},
	}
	_, b := LoadTimelineFrom(s)
	st, an, fa := 0, 0, 0
	for _, x := range b {
		st += x.Started
		an += x.Answered
		fa += x.Failed
	}
	if st != 3 || an != 2 || fa != 1 {
		t.Errorf("started=%d answered=%d failed=%d, want 3/2/1 (a negative elapsed counts as 0)", st, an, fa)
	}
	if b[0].Started != 1 || b[0].Answered != 1 {
		t.Errorf("the negative-elapsed sample must land in the bucket of its start: %+v", b[0])
	}
}

// Samples without a parsable timeStamp (StartMs 0) stay out of the timeline, as documented.
func TestLoadTimelineFrom_SamplesWithoutStartAreLeftOut(t *testing.T) {
	s := []LoadSample{
		{StartMs: 0, ElapsedMs: 10, Code: "200"},
		{StartMs: -7, ElapsedMs: 10, Code: "200"},
		{StartMs: 1_000_000, ElapsedMs: 10, Code: "200"},
		{StartMs: 1_002_000, ElapsedMs: 10, Code: "200"},
	}
	_, b := LoadTimelineFrom(s)
	st := 0
	for _, x := range b {
		st += x.Started
	}
	if st != 2 {
		t.Errorf("started=%d, want 2: only the two samples with a start time are bucketed", st)
	}
	if w, b := LoadTimelineFrom(s[:3]); w != 0 || b != nil {
		t.Errorf("one timed sample must give (0, nil), got (%d, %v)", w, b)
	}
}

// Sums: started and answered+failed must each equal the timed samples, including at huge spans.
func TestLoadTimelineFrom_SumsHoldAtLargeSpans(t *testing.T) {
	var s []LoadSample
	for i := 0; i < 5000; i++ {
		code := "200"
		if i%7 == 0 {
			code = "Non HTTP response code: x"
		}
		s = append(s, LoadSample{StartMs: 1_700_000_000_000 + int64(i)*17_000, ElapsedMs: i % 900, Code: code})
	}
	w, b := LoadTimelineFrom(s)
	st, an, fa := 0, 0, 0
	for _, x := range b {
		st += x.Started
		an += x.Answered
		fa += x.Failed
	}
	if st != 5000 || an+fa != 5000 || len(b) > MaxLoadTimelineBuckets || w < 1 {
		t.Fatalf("w=%d buckets=%d started=%d answered=%d failed=%d", w, len(b), st, an, fa)
	}
}

func TestLoadErrorsFrom_TruncationKeepsValidUTF8(t *testing.T) {
	long := strings.Repeat("é", 200)
	e := LoadErrorsFrom([]LoadSample{{Code: "Non HTTP response code: java.lang.IllegalArgumentException", Message: "Non HTTP response message: " + long}}, nil)
	if !utf8.ValidString(e[0].Reason) || utf8.RuneCountInString(e[0].Reason) != MaxLoadErrorReasonLen {
		t.Fatalf("reason runes=%d valid=%v", utf8.RuneCountInString(e[0].Reason), utf8.ValidString(e[0].Reason))
	}
}

// A numeric code that is not an HTTP status counts as an answer; an empty or non-numeric code does not.
func TestLoadSample_OddCodes(t *testing.T) {
	want := map[string]bool{"": true, "  ": true, "-1": false, "200 OK": true, "0": false, "Non HTTP response code:": true}
	for c, failed := range want {
		if got := (LoadSample{Code: c, Message: "x"}).transportFailed(); got != failed {
			t.Errorf("code=%q transportFailed=%v, want %v", c, got, failed)
		}
	}
}

// The reason is built at the source: every URL is cut to scheme://host[:port], and what follows a path
// (a bare ?query, a key=value list) goes with it. The class name and the plain words survive.
func TestLoadErrorReason_URLsAreReducedToSchemeAndHost(t *testing.T) {
	cases := []struct{ code, msg, want string }{
		{"java.net.SocketException", "Network is unreachable", "java.net.SocketException: Network is unreachable"},
		{"org.apache.http.conn.HttpHostConnectException", "Connect to example.com:443 failed: Connection refused",
			"org.apache.http.conn.HttpHostConnectException: Connect to example.com:443 failed: Connection refused"},
		{"java.lang.IllegalArgumentException", "Illegal character in query at index 31: http://sut.internal/api/v1/orders?token=hunter2&x=a b",
			"java.lang.IllegalArgumentException: Illegal character in query at index 31: http://sut.internal"},
		{"x.Y", "Illegal character in path at index 20: https://sut/pay/key-sk_live_short/x y", "x.Y: Illegal character in path at index 20: https://sut"},
		{"x.Y", "http://user:pass@sut.internal:8443/x unreachable", "x.Y: http://sut.internal:8443 unreachable"},
		{"x.Y", "refused http://sut/x?api_key=AbC123xyZ9Qw&y=1#frag and then https://other:9/p?q=1 failed",
			"x.Y: refused http://sut and then https://other:9 failed"},
		{"x.Y", "GET /pay/key-1?api_key=AbC123&y=1 failed", "x.Y: GET /pay/key-1 failed"},
	}
	for _, c := range cases {
		got := loadErrorReason(LoadSample{Code: "Non HTTP response code: " + c.code, Message: "Non HTTP response message: " + c.msg})
		if got != c.want {
			t.Errorf("\n got  %q\n want %q", got, c.want)
		}
		for _, secret := range []string{"hunter2", "sk_live_short", "user:pass", "AbC123"} {
			if strings.Contains(got, secret) {
				t.Errorf("secret %q survives: %s", secret, got)
			}
		}
	}
}

// Grouping: an ephemeral address:port after the target is normalised to :*, the target's own port is not,
// and two different targets stay two reasons.
func TestLoadErrorsFrom_EphemeralPortsGroup(t *testing.T) {
	mk := func(msg string) LoadSample {
		return LoadSample{Code: "Non HTTP response code: java.net.ConnectException", Message: "Non HTTP response message: " + msg}
	}
	same := LoadErrorsFrom([]LoadSample{
		mk("Connect to sut:8080 failed: /10.244.5.62:34567 refused"),
		mk("Connect to sut:8080 failed: /10.244.5.62:41000 refused"),
		mk("Connect to sut:8080 failed: /10.244.9.9:2222 refused"),
	}, nil)
	if len(same) != 2 || same[0].Count != 2 || !strings.Contains(same[0].Reason, "sut:8080") || !strings.Contains(same[0].Reason, "/10.244.5.62:*") {
		t.Errorf("same target, other local ports must group (the address itself stays): %+v", same)
	}
	diff := LoadErrorsFrom([]LoadSample{
		mk("Connect to sut-a:8080 failed: Connection refused"),
		mk("Connect to sut-b:8080 failed: Connection refused"),
		mk("Connect to sut-a:8081 failed: Connection refused"),
	}, nil)
	if len(diff) != 3 {
		t.Errorf("different targets must stay different reasons: %+v", diff)
	}
	ipTarget := LoadErrorsFrom([]LoadSample{mk("Connect to 10.0.0.5:8080 failed"), mk("Connect to 10.0.0.5:8081 failed")}, nil)
	if len(ipTarget) != 2 {
		t.Errorf("the target's own port is never rewritten, even for an IP target: %+v", ipTarget)
	}
}

// Where the dial text names the target as a host and an address list, the list carries no port and
// both samples already read the same.
func TestLoadErrorsFrom_BracketedAddressListGroups(t *testing.T) {
	mk := func(msg string) LoadSample {
		return LoadSample{Code: "Non HTTP response code: org.apache.http.conn.HttpHostConnectException", Message: "Non HTTP response message: " + msg}
	}
	e := LoadErrorsFrom([]LoadSample{
		mk("Connect to localhost:8080 [localhost/127.0.0.1] failed: Connection refused"),
		mk("Connect to localhost:8080 [localhost/127.0.0.1] failed: Connection refused"),
	}, nil)
	if len(e) != 1 || e[0].Count != 2 {
		t.Errorf("%+v", e)
	}
}
