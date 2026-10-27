package httpload

// acf22_bytes_test.go — AC-F2.2: for these fixtures, a load report WITHOUT load errors and without a broker pause is
// byte for byte what it was before AC-F2.2, and a report WITH load errors differs only by the added words, apart from
// one pinned change: a reason that the normaliser empties was "" and is now "transport failure (no detail)".
//
// The goldens were written by THIS SAME FILE on the code before AC-F2.2 (dev 0ef470ae): ACF22_WRITE_GOLDEN=1 go test
// ./internal/httpload/ -run TestACF22Bytes. The file uses only what exists on both sides (report.LoadErrorsFrom,
// report.LoadTimelineFrom, this package's and amqpload's AggregateStep, the report types), so the same fixture is built
// by the old and the new code. Never regenerate the goldens on a newer commit: they are the pre-AC-F2.2 bytes.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/amqpload"
	"github.com/OneDro1d/argus-runner/internal/report"
)

func acf22Sample(start int64, el int, code, msg string) report.LoadSample {
	return report.LoadSample{StartMs: start, ElapsedMs: el, Code: code, Message: msg}
}

// acf22Load is a plain `## LOAD` record built the way argus builds it: errors and timeline from the samples.
func acf22Load(ss []report.LoadSample) *report.LoadStats {
	ls := &report.LoadStats{Samples: len(ss), P50Ms: 12, P95Ms: 40, P99Ms: 90, TargetP95Ms: 500, MaxErrorRate: 0.5}
	ls.Errors = report.LoadErrorsFrom(ss, nil)
	ls.TimelineBucketS, ls.Timeline = report.LoadTimelineFrom(ss)
	return ls
}

func acf22HTTPRow(ts int64, el int, code, msg string) amqpload.Row {
	return amqpload.Row{TimeStampMs: ts, ElapsedMs: el, Label: "HL-1", Code: code, Message: msg}
}

// acf22AMQPRows: 2 sessions x 10 s of steady publishing and delivery, no broker pause.
func acf22AMQPRows() []amqpload.Row {
	const t0 = int64(1_000_000)
	rows := []amqpload.Row{{TimeStampMs: t0, Label: "X-amqp-setup", Code: "200", Success: true, Message: "session ready"}}
	for k := 1; k <= 10; k++ {
		ts := t0 + 10000 + int64(k)*1000 - 500
		for s := 0; s < 2; s++ {
			rows = append(rows,
				amqpload.Row{TimeStampMs: ts, ElapsedMs: k, Label: "X-amqp-publish", Code: "200", Success: true, Message: "published 1000B confirm=each us=1500"},
				amqpload.Row{TimeStampMs: ts, ElapsedMs: k * 3, Label: "X-amqp-deliver", Code: "200", Success: true, Message: "delivered 1000B us=4200 redelivered=false"})
		}
	}
	return rows
}

func acf22AMQPStep() report.LoadStep {
	return amqpload.AggregateStep(acf22AMQPRows(), amqpload.StepInput{Step: 1, Sessions: 2, RampSeconds: 10, RatePerSession: 1,
		Confirm: "each", ScenarioID: "X", TargetP95Ms: 250, MaxErrorRate: 0.01, MinDeliveredRatio: 0.95})
}

// acf22Bare: no load error anywhere, no broker pause.
func acf22Bare() []report.ScenarioResult {
	t0 := int64(2_000_000)
	var ss []report.LoadSample
	hrows := []amqpload.Row{acf22HTTPRow(t0, 5, "200", "OK")} // the first row: the steady window starts 10 s (the ramp) after it
	for i := 0; i < 20; i++ {
		ss = append(ss, acf22Sample(t0+int64(i)*250, 10+i, "200", "OK"))
		hrows = append(hrows, acf22HTTPRow(t0+10_000+int64(i)*500, 10+i, "200", "OK"))
	}
	ss = append(ss, acf22Sample(t0+6000, 8, "503", "Service Unavailable")) // an answer, not a transport failure
	hrows = append(hrows, acf22HTTPRow(t0+15_000, 8, "503", "Service Unavailable"))
	h := AggregateStep(hrows, StepInput{Step: 1, Users: 5, RampSeconds: 10, ScenarioID: "HL-1", TargetP95Ms: 1000, MaxErrorRate: 0.5})
	return []report.ScenarioResult{
		{ID: "LOAD-001", Status: "passed", DurationMs: 30000, Load: acf22Load(ss)},
		{ID: "HL-1", Status: "passed", DurationMs: 30000, LoadDriver: "http", LoadTarget: "api-lab", LoadSteps: []report.LoadStep{h}},
		{ID: "X", Status: "passed", DurationMs: 30000, LoadDriver: "amqp", LoadTarget: "lab-broker", LoadSteps: []report.LoadStep{acf22AMQPStep()}},
	}
}

// acf22Reorder: ordinary transport failures, one reason with a query the normaliser cuts, and two samples whose whole
// text the normaliser empties (no class, a bare query). Those two are the reorder in loadErrorReason: before AC-F2.2 the
// "no detail" fallback ran BEFORE normalizeReason, so their reason was ""; now it runs after. Two of them, so their entry
// sorts by its count on both sides and the order cannot move.
func acf22Reorder() []report.ScenarioResult {
	t0 := int64(3_000_000)
	refused := "Non HTTP response code: org.apache.http.conn.HttpHostConnectException"
	refusedMsg := "Non HTTP response message: Connect to sut:8080 [sut/10.0.0.1] failed: Connection refused"
	timeout := "Non HTTP response code: java.net.SocketTimeoutException"
	query := "Non HTTP response code: java.net.URISyntaxException"
	queryMsg := "Non HTTP response message: Illegal character in query at index 20: http://sut:8080/a?token=s3cr3t x"
	emptied, emptiedMsg := "Non HTTP response code:", "Non HTTP response message: ?q=1"
	var ss []report.LoadSample
	hrows := []amqpload.Row{acf22HTTPRow(t0, 5, "200", "OK")} // the first row: the steady window starts 10 s (the ramp) after it
	for i := 0; i < 10; i++ {
		ss = append(ss, acf22Sample(t0+int64(i)*250, 10+i, "200", "OK"))
		hrows = append(hrows, acf22HTTPRow(t0+10_000+int64(i)*500, 10+i, "200", "OK"))
	}
	for i := 0; i < 3; i++ {
		ss = append(ss, acf22Sample(t0+4000+int64(i), 2, refused, refusedMsg))
		hrows = append(hrows, acf22HTTPRow(t0+16_000+int64(i), 2, refused, refusedMsg))
	}
	ss = append(ss, acf22Sample(t0+5000, 3000, timeout, "Non HTTP response message: Read timed out"))
	hrows = append(hrows, acf22HTTPRow(t0+17_000, 3000, timeout, "Non HTTP response message: Read timed out"))
	ss = append(ss, acf22Sample(t0+5500, 1, query, queryMsg))
	hrows = append(hrows, acf22HTTPRow(t0+17_500, 1, query, queryMsg))
	for i := 0; i < 2; i++ {
		ss = append(ss, acf22Sample(t0+5700+int64(i), 1, emptied, emptiedMsg))
		hrows = append(hrows, acf22HTTPRow(t0+17_700+int64(i), 1, emptied, emptiedMsg))
	}
	h := AggregateStep(hrows, StepInput{Step: 1, Users: 5, RampSeconds: 10, ScenarioID: "HL-1", TargetP95Ms: 1000, MaxErrorRate: 0.9})
	return []report.ScenarioResult{
		{ID: "LOAD-002", Status: "failed", DurationMs: 30000, Load: acf22Load(ss)},
		{ID: "HL-1", Status: "passed", DurationMs: 30000, LoadDriver: "http", LoadTarget: "api-lab", LoadSteps: []report.LoadStep{h}},
	}
}

// the word AC-F2.2 adds to each load error entry; the reorder test compares the rest
var acf22KindRe = regexp.MustCompile(`,"kind":"[a-z_]+"`)

// the one intended change of a reason's bytes: the emptied reason, once in `## LOAD` errors and once in the step's
// error_reasons
const (
	acf22EmptiedBefore = `"reason":""`
	acf22EmptiedNow    = `"reason":"transport failure (no detail)"`
	acf22EmptiedTimes  = 2
)

// acf22Golden returns the golden bytes. With ACF22_WRITE_GOLDEN set it writes got as the golden and skips the test:
// writing is not a comparison.
func acf22Golden(t *testing.T, name string, got []byte) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("ACF22_WRITE_GOLDEN") != "" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skipf("wrote %s (%d bytes)", path, len(got))
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v", path, err)
	}
	return want
}

// A load report with no load error and no broker pause: the same bytes as before AC-F2.2 — so the same evidence hash.
func TestACF22Bytes_ABareLoadReportIsUnchanged(t *testing.T) {
	got, err := json.Marshal(acf22Bare())
	if err != nil {
		t.Fatal(err)
	}
	if want := acf22Golden(t, "acf22-bare.golden.json", got); string(got) != string(want) {
		t.Fatalf("a load report without errors changed:\nwant %s\nhave %s", want, got)
	}
}

// A load report with load errors: the same bytes as before AC-F2.2 once each entry's word is taken out and the emptied
// reason is the "no detail" text — the other reason texts, the grouping, the order and the counts are unchanged.
func TestACF22Bytes_ErrorsDifferOnlyByTheirWords(t *testing.T) {
	got, err := json.Marshal(acf22Reorder())
	if err != nil {
		t.Fatal(err)
	}
	golden := acf22Golden(t, "acf22-reorder.golden.json", got)
	if n := bytes.Count(golden, []byte(acf22EmptiedBefore)); n != acf22EmptiedTimes {
		t.Fatalf("the golden holds %s %d times, want %d: the fixture no longer exercises the reorder", acf22EmptiedBefore, n, acf22EmptiedTimes)
	}
	want := bytes.ReplaceAll(golden, []byte(acf22EmptiedBefore), []byte(acf22EmptiedNow))
	if stripped := acf22KindRe.ReplaceAll(got, nil); string(stripped) != string(want) {
		t.Fatalf("a load report with errors changed beyond its words and the pinned reason:\nwant %s\nhave %s", want, stripped)
	}
	if !acf22KindRe.Match(got) {
		t.Fatalf("no word in a report with load errors: %s", got)
	}
}
