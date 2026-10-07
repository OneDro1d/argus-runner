package runner

// compare_fix_test.go — ARGUS-CMP-3 fixer round (PR #499): the relay answer's size, one bad row in the
// push, and retention after a push that carried no outputs. Deletion tests use t.TempDir() and a
// recording fake only.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/argus"
	"github.com/OneDro1d/argus-runner/internal/compare"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
)

func TestRelay_GetOutput_AnswerIsAlwaysUnderTheCommandResultCap(t *testing.T) {
	root := t.TempDir()
	base := argus.OutputsBase(filepath.Join(root, "local"))
	spec := (&compare.Rules{Reference: compare.RefMeasured, Output: compare.OutputSel{Status: true, Body: true}}).Spec()
	cmd := NewCommandFunc(ExecConfig{ResultsRoot: root})
	get := func(id string) (json.RawMessage, error) {
		return cmd(context.Background(), "get_output", json.RawMessage(`{"run_id":"run-1","scenario_id":"`+id+`","step":"","sample":1}`))
	}

	// worst case: 300 KiB of 0x01 is a 1.5 MiB file once each byte is a 6-byte escape
	rec, st := compare.BuildRecord(compare.Response{Status: 200, Body: []byte(strings.Repeat("\x01", 300<<10))}, spec, "", 1)
	if err := argus.WriteOutputFile(base, "run-1", "WORST", "", 1, st.File()); err != nil {
		t.Fatal(err)
	}
	out, err := get("WORST")
	if err == nil && len(out) >= 1<<20 {
		t.Fatalf("the relay answer is %d bytes: over the 1 MiB command-result cap (design 10.3)", len(out))
	}
	if err != nil && !strings.Contains(err.Error(), "too large") {
		t.Fatalf("the only other acceptable answer is the closed too-large error: %v", err)
	}
	if !rec.Truncated {
		t.Errorf("the record must say the stored form was cut: %+v", rec)
	}

	// an ordinary 256 KiB text body still returns whole
	plain := strings.Repeat("a", compare.StoredBodyLimit)
	prec, pst := compare.BuildRecord(compare.Response{Status: 200, Body: []byte(plain)}, spec, "", 1)
	if err := argus.WriteOutputFile(base, "run-1", "PLAIN", "", 1, pst.File()); err != nil {
		t.Fatal(err)
	}
	out, err = get("PLAIN")
	if err != nil || prec.Truncated || !bytes.Contains(out, []byte(plain)) {
		t.Fatalf("an ordinary 256 KiB body must come back whole: err %v truncated %v len %d", err, prec.Truncated, len(out))
	}

	// a file already on disk that is over the cap (written by an older executor) is answered with the
	// closed error, never with the body
	if err := argus.WriteOutputFile(base, "run-1", "OLD", "", 1, bytes.Repeat([]byte("x"), (1<<20)+5)); err != nil {
		t.Fatal(err)
	}
	out, err = get("OLD")
	if err == nil || !strings.Contains(err.Error(), "too large") || len(out) != 0 {
		t.Fatalf("an over-cap file must be the closed too-large error: %d bytes, %v", len(out), err)
	}
	if strings.Contains(err.Error(), root) {
		t.Errorf("the error carries a path: %v", err)
	}
}

func okRow(sample int) compare.OutputRecord {
	h := strings.Repeat("b", 64)
	return compare.OutputRecord{V: 1, Step: "s", Sample: sample, State: compare.StateRecorded, Status: 200,
		Parts: compare.Parts{Status: h, Body: h}, Hash: h, BodyKind: compare.KindJSON, BodyBytes: 3}
}

func TestAttachCompareOutputs_OneInvalidRowDropsOnlyThatRow(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	long := strings.Repeat("L", 65) // legal in the local store, refused by the control plane's reader
	rep := &report.Report{Layers: []report.Layer{{Layer: "Permissions", Scenarios: []report.ScenarioResult{
		{ID: "CHN-A", Outputs: []compare.OutputRecord{okRow(1)}},
		{ID: long, Outputs: []compare.OutputRecord{okRow(1)}},
		{ID: "CHN-C", Outputs: []compare.OutputRecord{okRow(1)}},
	}}}}
	var push federation.ResultsPush
	attachCompareOutputs(&push, rep)

	rows, norm, err := compare.DecodeOutputs(push.Outputs)
	if err != nil {
		t.Fatalf("the push carries no outputs, or ones the control plane refuses: %v (%q)", err, push.Outputs)
	}
	if len(rows) != 2 || rows[0].ScenarioID != "CHN-A" || rows[1].ScenarioID != "CHN-C" || string(norm) != string(push.Outputs) {
		t.Fatalf("rows %+v: want exactly CHN-A and CHN-C", rows)
	}
	if push.OutputsRoot != compare.OutputsRoot(rows) {
		t.Errorf("outputs_root %q is not the root of the 2 rows actually sent", push.OutputsRoot)
	}
	if strings.Contains(string(push.Outputs), long) {
		t.Errorf("the invalid row was sent")
	}
	var warns []string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "level=WARN") {
			warns = append(warns, l)
		}
	}
	if len(warns) != 1 || !strings.Contains(warns[0], long) {
		t.Errorf("want one warn naming the dropped scenario id, got %q", warns)
	}
	if len(warns) == 1 && !strings.Contains(warns[0], "scenario_id") {
		t.Errorf("the warn must name the rule: %s", warns[0])
	}
}

func TestOutputRetention_PrunesAfterAPushThatCarriedNoOutputs_RecordingFake(t *testing.T) {
	root := t.TempDir()
	base := argus.OutputsBase(filepath.Join(root, "local"))
	now := time.Now()
	for i := 1; i <= 23; i++ {
		d := filepath.Join(base, "r"+string(rune('a'+i-1)))
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		ts := now.Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(d, ts, ts)
	}
	var calls []string
	hook := OutputRetention(ExecConfig{ResultsRoot: root}, func(p string) error { calls = append(calls, p); return nil })

	// the run just pushed stored files (its directory exists) but the push carried no outputs (omitted)
	hook(federation.ResultsPush{RunID: "zz-new"})
	if len(calls) != 0 {
		t.Fatalf("a push of a run that stored nothing pruned %v", calls)
	}
	if err := os.MkdirAll(filepath.Join(base, "zz-new"), 0o700); err != nil {
		t.Fatal(err)
	}
	newest := now.Add(time.Hour)
	_ = os.Chtimes(filepath.Join(base, "zz-new"), newest, newest)
	hook(federation.ResultsPush{RunID: "zz-new"})
	if len(calls) != 4 {
		t.Fatalf("a successful push of a run that stored outputs must prune to the newest 20 (24 dirs, so 4 removed), got %v", calls)
	}
	for _, c := range calls {
		if filepath.Dir(c) != base || filepath.Base(c) == "zz-new" {
			t.Errorf("the remover was given %q", c)
		}
	}
}
