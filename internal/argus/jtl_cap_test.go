package argus

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/memlimit"
)

// the .jtl readers stream, and a .jtl with more rows than the executor holds is never judged
// on a prefix: the scenario is `error` with the words, not `passed`, not `failed`.

// pinMemory makes the cap read a fake container: cgroupBytes is the content of a v2 memory.max ("" = no cgroup
// file at all) and gomemlimit the GOMEMLIMIT in the environment ("" = unset). The real box is never read.
func pinMemory(t *testing.T, cgroupBytes, gomemlimit string) {
	t.Helper()
	dir := t.TempDir()
	o := memlimit.Options{V2File: filepath.Join(dir, "v2-missing"), V1File: filepath.Join(dir, "v1-missing"),
		Getenv: func(k string) string {
			if k == "GOMEMLIMIT" {
				return gomemlimit
			}
			return ""
		}}
	if cgroupBytes != "" {
		o.V2File = filepath.Join(dir, "memory.max")
		if err := os.WriteFile(o.V2File, []byte(cgroupBytes+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prev := jtlMemOptions
	jtlMemOptions = o
	t.Cleanup(func() { jtlMemOptions = prev })
}

// withJTLFloor lowers the floor of the plain-LOAD cap to n.
func withJTLFloor(t *testing.T, n int) {
	t.Helper()
	prev := jtlSampleFloor
	jtlSampleFloor = n
	t.Cleanup(func() { jtlSampleFloor = prev })
}

// withJTLCap makes the cap EXACTLY n: a floor of n on a container that cannot be read (the floor is the cap).
func withJTLCap(t *testing.T, n int) {
	t.Helper()
	withJTLFloor(t, n)
	pinMemory(t, "", "")
}

const (
	gi4    = "4294967296" // a 4Gi container limit
	mi1536 = "1610612736"
)

const jtlHeader = "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n"

func writeSamples(t *testing.T, rows int, label string, success bool) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(jtlHeader)
	for i := 0; i < rows; i++ {
		fmt.Fprintf(&b, "%d,%d,%s,202,OK,T 1-1,%t,\n", 1781024939842+int64(i), 10+i, label, success)
	}
	p := filepath.Join(t.TempDir(), "x.jtl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEachJTLRow_StopsOverTheCapAndCountsAtIt(t *testing.T) {
	p := writeSamples(t, 4, "S", true)
	n, err := eachJTLRow(p, 4, func(func(string) string) {})
	if n != 4 || err != nil {
		t.Fatalf("at the cap: n=%d err=%v", n, err)
	}
	n, err = eachJTLRow(p, 3, func(func(string) string) {})
	if !errors.Is(err, errJTLOverCap) || n != 3 {
		t.Fatalf("over the cap: n=%d err=%v, want it to stop at 3 with errJTLOverCap", n, err)
	}
	withJTLCap(t, 3)
	if !jtlOverCap(p) {
		t.Fatal("jtlOverCap must be true for 4 rows with a cap of 3")
	}
	withJTLCap(t, 4)
	if jtlOverCap(p) {
		t.Fatal("jtlOverCap must be false at the cap")
	}
}

func TestReadJTL_OverTheCapReturnsNothingNotAPrefix(t *testing.T) {
	withJTLCap(t, 3)
	p := writeSamples(t, 5, "S", true)
	codes, labels, tsMs, per, max, msgs := readJTLCodesElapsed(p)
	if codes != nil || labels != nil || tsMs != nil || per != nil || max != 0 || msgs != nil {
		t.Fatalf("readJTLCodesElapsed over the cap returned a prefix: %v %v", codes, labels)
	}
	if got := readJTLLoadSamples(p, "S"); got != nil {
		t.Fatalf("readJTLLoadSamples over the cap returned a prefix: %v", got)
	}
}

// Positive control on the same readers: under the cap they return every row, so the "nothing" above is the cap
// and not a reader that returns nothing.
func TestReadJTL_UnderTheCapReturnsEveryRow(t *testing.T) {
	withJTLCap(t, 5)
	p := writeSamples(t, 5, "S", true)
	codes, labels, tsMs, per, max, msgs := readJTLCodesElapsed(p)
	if len(codes) != 5 || len(labels) != 5 || len(tsMs) != 5 || len(per) != 5 || len(msgs) != 5 || max != 14 || codes[0] != 202 || labels[4] != "S" {
		t.Fatalf("got %v %v %v %v %d %v", codes, labels, tsMs, per, max, msgs)
	}
	if got := readJTLLoadSamples(p, "S"); len(got) != 5 || got[0].Code != "202" || got[4].ElapsedMs != 14 {
		t.Fatalf("load samples: %+v", got)
	}
}

// readJTLSuccess holds no row, so it has no cap, and it still reports the FIRST failure.
func TestReadJTLSuccess_StreamsAndReportsTheFirstFailure(t *testing.T) {
	withJTLCap(t, 2) // must not matter
	var b strings.Builder
	b.WriteString(jtlHeader)
	b.WriteString("1,5,S,202,OK,T,true,\n")
	b.WriteString("2,5,S,500,Boom,T,false,first\n")
	b.WriteString("3,5,S,500,Boom2,T,false,second\n")
	b.WriteString("4,5,S,202,OK,T,true,\n")
	p := filepath.Join(t.TempDir(), "s.jtl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, msg := readJTLSuccess(p)
	if ok || msg != "S: 500 Boom first" {
		t.Fatalf("ok=%v msg=%q", ok, msg)
	}
	if ok, msg := readJTLSuccess(writeSamples(t, 4, "S", true)); !ok || msg != "" {
		t.Fatalf("all-success file: ok=%v msg=%q", ok, msg)
	}
	if ok, _ := readJTLSuccess(filepath.Join(t.TempDir(), "missing.jtl")); ok {
		t.Fatal("a missing file is never a success")
	}
}

// bigRunner writes `rows` passing samples for the scenario.
type bigRunner struct{ rows int }

func (b bigRunner) Run(_, jtlPath string, props map[string]string, _ time.Duration) error {
	f, err := os.Create(jtlPath)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20) // streamed: a 1.7M-row file is never held as one string
	w.WriteString(jtlHeader)
	for i := 0; i < b.rows; i++ {
		fmt.Fprintf(w, "%d,%d,%s,202,OK,T 1-1,true,\n", 1781024939842+int64(i), 10+i, props["scenario.id"])
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func runBig(t *testing.T, rows int) (status, observed string) {
	t.Helper()
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-900", "status=202")
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", bigRunner{rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("ORD-900")
	if res == nil {
		t.Fatal("no result for ORD-900")
	}
	if res.Failure != nil {
		observed = res.Failure.Observed
	}
	return res.Status, observed
}

func TestRunOne_AJTLOverTheCapIsErrorWithTheWordsNeverAPass(t *testing.T) {
	withJTLCap(t, 5)
	if status, observed := runBig(t, 5); status != "passed" {
		t.Fatalf("positive control: 5 samples at a cap of 5 must pass, got %s (%s)", status, observed)
	}
	status, observed := runBig(t, 6)
	if status != "error" {
		t.Fatalf("6 samples over a cap of 5: status = %q, want \"error\" (observed %q)", status, observed)
	}
	// the words name the cap that was hit, the samples seen and the limit it was derived from
	for _, want := range []string{"cap of 5", "wrote 6 samples", "no memory limit could be read", "NOT judged", "--executor-memory"} {
		if !strings.Contains(observed, want) {
			t.Errorf("observed must say %q: %q", want, observed)
		}
	}
}

// The cap grows with the memory the executor is given. These two real runs on
// shop-dev-aks (limit 4Gi) PASSED on the old whole-file reader with 1,662,518 and 1,572,671 samples; a fixed
// cap of 1,000,000 would have turned them into `error`, with advice (more memory) that could not help.
func TestRunOne_At4GiAPlainLoadCheckOf1_7MSamplesIsJudged(t *testing.T) {
	pinMemory(t, gi4, "")
	if got := maxJTLSamples(); got != 2_666_666 {
		t.Errorf("cap at 4Gi = %d, want 2666666 (1,000,000 x 4096/1536)", got)
	}
	status, observed := runBig(t, 1_700_000)
	if status != "passed" {
		t.Fatalf("1,700,000 samples at a 4Gi limit: status = %q (%s), want it judged (\"passed\")", status, observed)
	}
	// negative control on the same data: at the default container it is over the cap and never judged.
	pinMemory(t, mi1536, "")
	if status, observed := runBig(t, 1_700_000); status != "error" || !strings.Contains(observed, "cap of 1000000") {
		t.Fatalf("1,700,000 samples at 1536Mi: status = %q (%s), want error naming the cap of 1000000", status, observed)
	}
}

// The default container and "no limit could be read" keep the cap EXACTLY as it was.
func TestJTLCap_IsExactlyTheFloorAtTheDefaultAndWithNoReadableLimit(t *testing.T) {
	for name, c := range map[string]struct{ cgroup, gomem string }{
		"1536Mi":                                {mi1536, ""},
		"no cgroup file":                        {"", ""},
		"cgroup v2 max":                         {"max", ""},
		"below the default (1Gi)":               {"1073741824", ""},
		"GOMEMLIMIT=off":                        {"", "off"},
		"GOMEMLIMIT below its 60% share of 4Gi": {gi4, "512MiB"},
	} {
		pinMemory(t, c.cgroup, c.gomem)
		if got := maxJTLSamples(); got != 1_000_000 {
			t.Errorf("%s: cap = %d, want exactly 1000000", name, got)
		}
	}
}

// GOMEMLIMIT set by the operator: the runtime holds the heap to it, so the smaller of it and the cgroup limit
// governs; one above the cgroup limit never raises a cap.
func TestJTLCap_FollowsTheSmallerOfGOMEMLIMITAndTheCgroupLimit(t *testing.T) {
	pinMemory(t, gi4, "2GiB") // Shop's executor: limit 4Gi, GOMEMLIMIT 2GiB -> read as a 3413 MiB container
	if got := maxJTLSamples(); got < 1_700_000 || got >= 2_666_666 {
		t.Errorf("GOMEMLIMIT=2GiB under a 4Gi limit: cap = %d, want within [1700000, 2666666)", got)
	}
	pinMemory(t, gi4, "64GiB")
	if got := maxJTLSamples(); got != 2_666_666 {
		t.Errorf("GOMEMLIMIT above the cgroup limit: cap = %d, want the cgroup's 2666666", got)
	}
}

// A cap hit at a scaled limit says which: the cap, the samples seen and the limit it came from.
func TestRunOne_ScaledCapErrorNamesTheCapTheSamplesAndTheLimit(t *testing.T) {
	withJTLFloor(t, 1000)
	pinMemory(t, gi4, "")
	if got := maxJTLSamples(); got != 2666 {
		t.Fatalf("cap = %d, want 2666", got)
	}
	status, observed := runBig(t, 2700)
	if status != "error" {
		t.Fatalf("status = %q (%s), want error", status, observed)
	}
	for _, want := range []string{"wrote 2700 samples", "cap of 2666", "4096 MiB container limit", "memory.max", "NOT judged", "--executor-memory"} {
		if !strings.Contains(observed, want) {
			t.Errorf("observed must say %q: %q", want, observed)
		}
	}
}
