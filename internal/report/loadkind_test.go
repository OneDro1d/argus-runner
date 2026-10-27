package report

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// AC-F2.2: a load error leaves the environment only as a word from a closed list and a count; a broker pause only as
// memory, disk or other plus its times. These tests pin the words, where they are chosen, and the projection both exits
// (the limited report and the results push) send.

func row(class, msg string) LoadSample {
	return LoadSample{StartMs: 1, ElapsedMs: 1, Code: "Non HTTP response code: " + class, Message: "Non HTTP response message: " + msg}
}

// One row per type of the owner's table (issue #692 §2.1), the two measured additions, the SocketException messages,
// an unknown type and an empty one.
func TestLoadErrorKind_OneRowPerTypeOfTheOwnersTable(t *testing.T) {
	cases := []struct{ class, msg, want string }{
		{"java.net.ConnectException", "Connection refused", "refused"},
		{"org.apache.http.conn.HttpHostConnectException", "Connect to sut:8080 [sut/10.0.0.1] failed: Connection refused", "refused"},
		{"java.net.SocketTimeoutException", "Read timed out", "timeout"},
		{"org.apache.http.conn.ConnectTimeoutException", "Connect to sut:8080 [sut/10.0.0.1] failed: Connect timed out", "timeout"},
		// measured: a subclass of ConnectTimeoutException (the pool wait)
		{"org.apache.http.conn.ConnectionPoolTimeoutException", "Timeout waiting for connection from pool", "timeout"},
		{"java.net.SocketException", "Connection reset", "reset"},
		{"java.net.SocketException", "Connection reset by peer", "reset"},
		{"java.net.SocketException", "Broken pipe", "reset"},
		{"java.net.SocketException", "Broken pipe (Write failed)", "reset"},
		{"java.net.SocketException", "Network is unreachable", "other"},
		{"java.net.SocketException", "Socket closed", "other"},
		{"java.net.UnknownHostException", "sut.invalid: Name or service not known", "unknown_host"},
		{"javax.net.ssl.SSLException", "Unsupported or unrecognized SSL message", "tls"},
		{"javax.net.ssl.SSLHandshakeException", "Received fatal alert: handshake_failure", "tls"},
		{"javax.net.ssl.SSLPeerUnverifiedException", "peer not authenticated", "tls"},
		{"javax.net.ssl.SSLProtocolException", "x", "tls"},
		{"org.apache.http.NoHttpResponseException", "sut:8080 failed to respond", "protocol"},
		{"org.apache.http.MalformedChunkCodingException", "Bad chunk header: zz", "protocol"},
		{"org.apache.http.TruncatedChunkException", "Truncated chunk (expected size: 10; actual size: 3)", "protocol"},
		{"org.apache.http.ProtocolException", "Invalid header: session=hunter2", "protocol"},
		{"org.apache.http.ParseException", "Invalid content", "protocol"},
		{"org.apache.http.ConnectionClosedException", "Premature end of Content-Length delimited message body (expected: 10; received: 3)", "protocol"},
		// measured: the class an invalid header line or status line reaches the JTL as, with message "null"
		{"org.apache.http.client.ClientProtocolException", "null", "protocol"},
		{"java.net.NoRouteToHostException", "No route to host", "other"},
		{"java.net.URISyntaxException", "Malformed escape pair at index 25: http://sut.invalid:8080/a%zz", "other"},
		{"com.example.Unheard", "whatever", "other"},
		{"", "", "other"},
	}
	for _, c := range cases {
		if got := LoadErrorKind(row(c.class, c.msg)); got != c.want {
			t.Errorf("%s: %q → %q, want %q", c.class, c.msg, got, c.want)
		}
	}
}

// The message never chooses a word outside SocketException: an app that writes "timeout" or "Connection reset" into
// what the load tool quotes cannot move a protocol error to another word.
func TestLoadErrorKind_TheMessageChoosesOnlyForSocketException(t *testing.T) {
	if got := LoadErrorKind(row("org.apache.http.MalformedChunkCodingException", "Bad chunk header: Connection reset timeout")); got != "protocol" {
		t.Errorf("got %q, want protocol", got)
	}
	if got := LoadErrorKind(row("com.example.Unheard", "Connection reset")); got != "other" {
		t.Errorf("got %q, want other", got)
	}
}

// Every word LoadErrorKind can return is on the closed list: the list itself, every word of the type table, and the
// three words chosen outside it (tls, reset, other).
func TestLoadErrorKind_EveryWordIsOnTheClosedList(t *testing.T) {
	want := []string{"refused", "timeout", "reset", "unknown_host", "tls", "protocol", "other"}
	if !reflect.DeepEqual(LoadErrorKinds, want) {
		t.Fatalf("LoadErrorKinds = %v, want %v", LoadErrorKinds, want)
	}
	for typ, w := range loadErrorKindOfType {
		if !isLoadErrorKind(w) {
			t.Errorf("type %s maps to %q, which is not on the closed list", typ, w)
		}
		// and the table entry is what LoadErrorKind answers for that type
		if got := LoadErrorKind(row("pkg."+typ, "m")); got != w {
			t.Errorf("LoadErrorKind(%s) = %q, want %q", typ, got, w)
		}
	}
	for _, w := range []string{LoadErrorKindTLS, LoadErrorKindReset, LoadErrorKindOther} {
		if !isLoadErrorKind(w) {
			t.Errorf("%q is not on the closed list", w)
		}
	}
}

// A reason that normalizeReason empties (an empty type and a bare query, or a key=value list) is the fixed fallback,
// never "": the full report keeps a reason key on every entry.
func TestLoadErrorsFrom_AReasonIsNeverEmpty(t *testing.T) {
	for _, msg := range []string{"?q=1", "a=1&b=2", ""} {
		got := LoadErrorsFrom([]LoadSample{{StartMs: 1, ElapsedMs: 1, Code: "Non HTTP response code: ", Message: "Non HTTP response message: " + msg}}, nil)
		if len(got) != 1 || got[0].Reason != "transport failure (no detail)" || got[0].Kind != "other" {
			t.Errorf("message %q: errors = %+v", msg, got)
		}
	}
}

const marker = "AC-F22-MARKER-5b1e"

// The issue's two rows: the word is protocol, and the full report keeps the message.
func TestLoadErrorsFrom_TheIssueRowsAreProtocolAndKeepTheirMessage(t *testing.T) {
	ss := []LoadSample{
		row("org.apache.http.ProtocolException", "Invalid header: session=hunter2"),
		row("org.apache.http.ProtocolException", "Invalid header: session=hunter2"),
		row("org.apache.http.MalformedChunkCodingException", "Bad chunk header: "+marker),
	}
	got := LoadErrorsFrom(ss, nil)
	want := []LoadError{
		{Reason: "org.apache.http.ProtocolException: Invalid header: session=hunter2", Kind: "protocol", Count: 2},
		{Reason: "org.apache.http.MalformedChunkCodingException: Bad chunk header: " + marker, Kind: "protocol", Count: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("errors = %+v\nwant %+v", got, want)
	}
}

// The kind is decided per sample, before the cap: past 8 groups the remainder is one entry per kind, so every word's
// count is exact and the total is today's.
func TestLoadErrorsFrom_TheRemainderKeepsEachKindAndTheTotal(t *testing.T) {
	var ss []LoadSample
	total := 0
	// 8 groups that stay listed: E0..E7, each 20 times
	for k := 0; k < 8; k++ {
		for n := 0; n < 20; n++ {
			ss = append(ss, row(fmt.Sprintf("E%d", k), "m"))
			total++
		}
	}
	// the remainder: 3 refused texts (2+2+1), 2 timeout texts (3+1), 1 unknown (1)
	add := func(class, msg string, n int) {
		for i := 0; i < n; i++ {
			ss = append(ss, row(class, msg))
			total++
		}
	}
	add("java.net.ConnectException", "Connection refused a", 2)
	add("java.net.ConnectException", "Connection refused b", 2)
	add("java.net.ConnectException", "Connection refused c", 1)
	add("java.net.SocketTimeoutException", "Read timed out a", 3)
	add("java.net.SocketTimeoutException", "Read timed out b", 1)
	add("X9", "m", 1)
	got := LoadErrorsFrom(ss, nil)
	if len(got) != MaxLoadErrorReasons+3 {
		t.Fatalf("len = %d, want 8 listed + 3 remainder entries: %+v", len(got), got)
	}
	rest := got[MaxLoadErrorReasons:]
	wantRest := []LoadError{
		{Reason: LoadErrorOther, Kind: "refused", Count: 5},
		{Reason: LoadErrorOther, Kind: "timeout", Count: 4},
		{Reason: LoadErrorOther, Kind: "other", Count: 1},
	}
	if !reflect.DeepEqual(rest, wantRest) {
		t.Errorf("remainder = %+v\nwant %+v", rest, wantRest)
	}
	sum := 0
	for _, e := range got {
		sum += e.Count
	}
	if sum != total {
		t.Errorf("sum = %d, want %d", sum, total)
	}
}

// A remainder tie is ordered by the word, so the bytes never depend on map order.
func TestLoadErrorsFrom_RemainderTiesAreOrderedByWord(t *testing.T) {
	var ss []LoadSample
	for k := 0; k < 8; k++ {
		for n := 0; n < 10; n++ {
			ss = append(ss, row(fmt.Sprintf("E%d", k), "m"))
		}
	}
	ss = append(ss, row("java.net.SocketTimeoutException", "t"), row("java.net.SocketTimeoutException", "t"))
	ss = append(ss, row("java.net.ConnectException", "r"), row("java.net.ConnectException", "r"))
	got := LoadErrorsFrom(ss, nil)
	rest := got[MaxLoadErrorReasons:]
	if len(rest) != 2 || rest[0].Kind != "refused" || rest[1].Kind != "timeout" {
		t.Errorf("remainder = %+v, want refused then timeout (equal counts, by word)", rest)
	}
}

// The same reason text with two kinds stays two entries (an empty type whose message names the class, beside the
// class itself), ordered by the word on a tie.
func TestLoadErrorsFrom_OneReasonTwoKindsStaysTwoEntries(t *testing.T) {
	ss := []LoadSample{
		{StartMs: 1, ElapsedMs: 1, Code: "Non HTTP response code: ", Message: "Non HTTP response message: java.net.ConnectException: Connection refused"},
		row("java.net.ConnectException", "Connection refused"),
	}
	want := []LoadError{
		{Reason: "java.net.ConnectException: Connection refused", Kind: "other", Count: 1},
		{Reason: "java.net.ConnectException: Connection refused", Kind: "refused", Count: 1},
	}
	// the groups come out of a map, whose order Go randomises: only the tie-break by word makes this stable
	for i := 0; i < 50; i++ {
		if got := LoadErrorsFrom(ss, nil); !reflect.DeepEqual(got, want) {
			t.Fatalf("call %d: errors = %+v\nwant %+v", i, got, want)
		}
	}
}

// The same samples in any order give the same bytes (the evidence hash commits to them).
func TestLoadErrorsFrom_TheSameSamplesGiveTheSameBytes(t *testing.T) {
	var ss []LoadSample
	for k := 0; k < 12; k++ {
		for n := 0; n < 1+k%3; n++ {
			ss = append(ss, row([]string{"java.net.ConnectException", "java.net.SocketTimeoutException", "X"}[k%3], fmt.Sprintf("m%d", k)))
		}
	}
	// one reason with two words and equal counts, frequent enough to stay among the 8 listed groups: only the
	// tie-break by word orders them
	for i := 0; i < 30; i++ {
		ss = append(ss,
			LoadSample{StartMs: 1, ElapsedMs: 1, Code: "Non HTTP response code: ", Message: "Non HTTP response message: java.net.ConnectException: Connection refused"},
			row("java.net.ConnectException", "Connection refused"))
	}
	first, _ := json.Marshal(LoadErrorsFrom(ss, nil))
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 50; i++ {
		r.Shuffle(len(ss), func(a, b int) { ss[a], ss[b] = ss[b], ss[a] })
		if b, _ := json.Marshal(LoadErrorsFrom(ss, nil)); string(b) != string(first) {
			t.Fatalf("shuffle %d gave other bytes:\n%s\nwant\n%s", i, b, first)
		}
	}
}

// A LoadError without a kind encodes exactly as before this change (omitempty), so a report without load errors, and
// one written before it, keep their bytes.
func TestLoadError_WithoutAKindEncodesAsBefore(t *testing.T) {
	b, _ := json.Marshal(LoadError{Reason: "x", Count: 1})
	if string(b) != `{"reason":"x","count":1}` {
		t.Errorf("got %s", b)
	}
	b, _ = json.Marshal(BlockedPeriod{SinceMs: 1, UntilMs: 2, Reason: "low on memory"})
	if string(b) != `{"since_ms":1,"until_ms":2,"reason":"low on memory"}` {
		t.Errorf("got %s", b)
	}
	// and a projected entry carries no reason key at all
	b, _ = json.Marshal(LoadError{Kind: "protocol", Count: 2})
	if string(b) != `{"kind":"protocol","count":2}` {
		t.Errorf("got %s", b)
	}
}

func TestLoadErrorsByKind_SumsByWordAndDropsTheReason(t *testing.T) {
	in := []LoadError{
		{Reason: "org.apache.http.ProtocolException: Invalid header: session=hunter2", Kind: "protocol", Count: 2},
		{Reason: "java.net.ConnectException: Connection refused", Kind: "refused", Count: 5},
		{Reason: "org.apache.http.MalformedChunkCodingException: Bad chunk header: " + marker, Kind: "protocol", Count: 1},
		{Reason: LoadErrorOther, Kind: "refused", Count: 3},
	}
	got := LoadErrorsByKind(in)
	want := []LoadError{{Kind: "refused", Count: 8}, {Kind: "protocol", Count: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	b, _ := json.Marshal(got)
	for _, bad := range []string{`"reason":`, "hunter2", marker, "Invalid header", "Bad chunk"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("%q left in %s", bad, b)
		}
	}
	if in[0].Reason == "" || in[0].Kind != "protocol" {
		t.Errorf("the input was changed: %+v", in[0])
	}
}

// A report written before this change has no kind: the entry is classified from its own reason by the same rule (a
// SocketException's message still decides reset).
func TestLoadErrorsByKind_AnEntryWithoutAKindIsClassifiedFromItsReason(t *testing.T) {
	in := []LoadError{
		{Reason: "java.net.SocketException: Connection reset", Count: 40},
		{Reason: "java.net.SocketException: Network is unreachable", Count: 2},
		{Reason: "org.apache.http.MalformedChunkCodingException: Bad chunk header: " + marker, Count: 1},
		{Reason: LoadErrorOther, Count: 3},
		{Reason: "transport failure (no detail)", Count: 1},
	}
	got := LoadErrorsByKind(in)
	want := []LoadError{{Kind: "reset", Count: 40}, {Kind: "other", Count: 6}, {Kind: "protocol", Count: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// The exit is the barrier: a kind that is not on the closed list is counted as other, never sent as written.
func TestLoadErrorsByKind_AWordOffTheListIsOther(t *testing.T) {
	got := LoadErrorsByKind([]LoadError{{Reason: "x", Kind: "session=hunter2", Count: 4}, {Kind: "refused", Count: 1}})
	want := []LoadError{{Kind: "other", Count: 4}, {Kind: "refused", Count: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestLoadErrorsByKind_NoneIsNil(t *testing.T) {
	if got := LoadErrorsByKind(nil); got != nil {
		t.Errorf("got %+v", got)
	}
}

// Measured in RabbitMQ 3.13.7: "low on " + the words joined by " & ". The Java sampler writes "no reason given" for an
// empty reason. "memory alarm" is the wording older fixtures used.
func TestBlockedKind(t *testing.T) {
	cases := map[string]string{
		"low on memory":                      "memory",
		"low on disk":                        "disk",
		"low on memory & disk":               "memory",
		"low on disk & memory":               "memory",
		"LOW ON DISC":                        "disk",
		"no reason given":                    "other",
		"":                                   "other",
		"memory alarm":                       "memory",
		"something else " + marker:           "other",
		"low on memory (amqp://u:p@broker/)": "memory",
	}
	for in, want := range cases {
		if got := BlockedKind(in); got != want {
			t.Errorf("BlockedKind(%q) = %q, want %q", in, got, want)
		}
	}
	if !reflect.DeepEqual(BlockedKinds, []string{"memory", "disk", "other"}) {
		t.Errorf("BlockedKinds = %v", BlockedKinds)
	}
}

func TestLoadStepWithoutTexts_SendsWordsCountsAndTimesAndChangesNothingElse(t *testing.T) {
	share := 0.25
	st := LoadStep{
		Step: 2, Sessions: 4, Status: LoadStepBlocked, WindowSeconds: 30, OfferedPerS: 10,
		Errors: map[string]int{"transport": 3, "status:503": 1},
		Blocked: []BlockedPeriod{
			{SinceMs: 10, UntilMs: 20, Reason: "low on disk " + marker, Kind: "disk"},
			{SinceMs: 30, UntilMs: 0, Reason: "low on memory"},               // written before this change
			{SinceMs: 40, UntilMs: 50, Reason: "x", Kind: "session=hunter2"}, // a word off the list
		},
		BlockedSeconds:             1.5,
		GeneratorCPUThrottledShare: &share,
		ErrorReasons: []LoadError{
			{Reason: "org.apache.http.MalformedChunkCodingException: Bad chunk header: " + marker, Kind: "protocol", Count: 2},
			{Reason: "java.net.ConnectException: Connection refused", Kind: "refused", Count: 1},
		},
	}
	before, _ := json.Marshal(st)
	got := LoadStepWithoutTexts(st)
	after, _ := json.Marshal(st)
	if string(before) != string(after) {
		t.Fatalf("the input step was changed:\n%s\n%s", before, after)
	}
	b, _ := json.Marshal(got)
	for _, bad := range []string{`"reason":`, marker, "hunter2", "Bad chunk", "low on"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("%q left in %s", bad, b)
		}
	}
	wantBlocked := []BlockedPeriod{{SinceMs: 10, UntilMs: 20, Kind: "disk"}, {SinceMs: 30, Kind: "memory"}, {SinceMs: 40, UntilMs: 50, Kind: "other"}}
	if !reflect.DeepEqual(got.Blocked, wantBlocked) {
		t.Errorf("blocked = %+v, want %+v", got.Blocked, wantBlocked)
	}
	if !reflect.DeepEqual(got.ErrorReasons, []LoadError{{Kind: "protocol", Count: 2}, {Kind: "refused", Count: 1}}) {
		t.Errorf("error_reasons = %+v", got.ErrorReasons)
	}
	// every other field is the same
	got.Blocked, got.ErrorReasons = st.Blocked, st.ErrorReasons
	if !reflect.DeepEqual(got, st) {
		t.Errorf("another field changed:\n%+v\n%+v", got, st)
	}
	// and the copy shares no slice with the input
	got2 := LoadStepWithoutTexts(st)
	got2.Blocked[0].SinceMs = 999
	got2.ErrorReasons[0].Count = 999
	if st.Blocked[0].SinceMs == 999 || st.ErrorReasons[0].Count == 999 {
		t.Error("the projection shares a slice with the report")
	}
}

func TestLoadStepWithoutTexts_AStepWithoutErrorsOrPausesIsUnchanged(t *testing.T) {
	st := LoadStep{Step: 1, Sessions: 2, Status: LoadStepMeasured, Comfortable: true}
	a, _ := json.Marshal(st)
	b, _ := json.Marshal(LoadStepWithoutTexts(st))
	if string(a) != string(b) {
		t.Errorf("%s\n%s", a, b)
	}
}
