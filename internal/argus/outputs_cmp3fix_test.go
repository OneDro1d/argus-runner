package argus

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/compare"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// ARGUS-CMP-3 fixer round (PR #499): the stored-output file name, the silent store failure, and the stale
// files of an earlier attempt. Every tree is under t.TempDir(); the remover is a recording fake that
// refuses to delete anything outside the temp tree it was built for.

func TestOutputFileName_IsInjective_CollidingPairsReadBackTheirOwnBody(t *testing.T) {
	type key struct{ id, step string }
	pairs := [][2]key{
		{{"a", "b"}, {"a__b", ""}},
		{{"a", "b__c"}, {"a__b", "c"}},
		{{"a_", "b"}, {"a", "_b"}},
		{{"a", "b"}, {"a~5f~5fb", ""}},
		{{"S-1", "create"}, {"S-1__create", ""}},
	}
	base := filepath.Join(t.TempDir(), "outputs")
	for i, p := range pairs {
		n0, e0 := OutputFileName(p[0].id, p[0].step, 1)
		n1, e1 := OutputFileName(p[1].id, p[1].step, 1)
		if e0 != nil || e1 != nil {
			t.Fatalf("pair %d: %v %v", i, e0, e1)
		}
		if n0 == n1 {
			t.Errorf("pair %d: %+v and %+v both map to %q", i, p[0], p[1], n0)
		}
		run := "run-" + string(rune('a'+i))
		b0, b1 := []byte(`{"from":"first"}`), []byte(`{"from":"second"}`)
		if err := WriteOutputFile(base, run, p[0].id, p[0].step, 1, b0); err != nil {
			t.Fatal(err)
		}
		if err := WriteOutputFile(base, run, p[1].id, p[1].step, 1, b1); err != nil {
			t.Fatal(err)
		}
		g0, _ := ReadOutputFile(base, run, p[0].id, p[0].step, 1)
		g1, _ := ReadOutputFile(base, run, p[1].id, p[1].step, 1)
		if string(g0) != string(b0) || string(g1) != string(b1) {
			t.Errorf("pair %d: read back %q and %q, want each its own body", i, g0, g1)
		}
	}

	// exhaustive over a small alphabet that holds every character the mapping treats specially
	const alphabet = "a_~.1"
	var parts []string
	var gen func(prefix string, n int)
	gen = func(prefix string, n int) {
		parts = append(parts, prefix)
		if n == 0 {
			return
		}
		for i := 0; i < len(alphabet); i++ {
			gen(prefix+string(alphabet[i]), n-1)
		}
	}
	gen("", 3)
	seen := map[string]key{}
	for _, id := range parts {
		if id == "" || id == "." {
			continue
		}
		for _, step := range parts {
			if step == "." {
				continue
			}
			for s := 1; s <= 2; s++ {
				name, err := OutputFileName(id, step, s)
				if err != nil {
					continue // a refused input (".." inside) is not a name
				}
				k := key{id + "\x00" + string(rune('0'+s)), step}
				if prev, dup := seen[name]; dup && prev != k {
					t.Fatalf("%q is the file of both %+v and %+v", name, prev, k)
				}
				seen[name] = k
				if strings.ContainsAny(name, `/\`) || name == "." || name == ".." || filepath.Base(name) != name {
					t.Fatalf("%q is not path-safe", name)
				}
			}
		}
	}
	if len(seen) < 1000 {
		t.Fatalf("the exhaustive sweep covered only %d names", len(seen))
	}
}

// warnRecords captures slog and returns the WARN-level lines.
func captureWarns(t *testing.T) (all func() string, warns func() []string) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	all = func() string { return buf.String() }
	warns = func() []string {
		var out []string
		for _, l := range strings.Split(buf.String(), "\n") {
			if strings.Contains(l, "level=WARN") {
				out = append(out, l)
			}
		}
		return out
	}
	return
}

func cmpStored(t *testing.T, body string) (compare.OutputRecord, *compare.Stored) {
	t.Helper()
	spec := (&compare.Rules{Reference: compare.RefMeasured, Output: compare.OutputSel{Status: true, Body: true}}).Spec()
	rec, st := compare.BuildRecord(compare.Response{Status: 200, Body: []byte(body)}, spec, "create", 1)
	if st == nil {
		t.Fatalf("nothing stored: %+v", rec)
	}
	return rec, st
}

func TestStoreFailure_IsWarnedOnceWithTheFourIdentifiersAndNoContent(t *testing.T) {
	const canary = "zebra-quartz-meadow-lantern"
	rec, st := cmpStored(t, `{"note":"`+canary+`"}`)

	t.Run("unwritable outputs directory", func(t *testing.T) {
		_, warns := captureWarns(t)
		blocker := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		oc := &outputCtx{runID: "run-warn", storeBase: filepath.Join(blocker, "outputs")}
		oc.store("CHK-1", rec, st)
		w := warns()
		if len(w) != 1 {
			t.Fatalf("want exactly one warn, got %d: %q", len(w), w)
		}
		for _, want := range []string{"run-warn", "CHK-1", "create", "sample=1"} {
			if !strings.Contains(w[0], want) {
				t.Errorf("the warn does not carry %q: %s", want, w[0])
			}
		}
		if strings.Contains(w[0], blocker) {
			t.Errorf("the warn carries a filesystem path: %s", w[0])
		}
		if _, err := ReadOutputFile(oc.storeBase, "run-warn", "CHK-1", "create", 1); !errors.Is(err, ErrOutputNotFound) {
			t.Errorf("get_output for a record with no stored body must be the closed no-recorded-output answer, got %v", err)
		}
	})

	t.Run("scenario id too long for a file name", func(t *testing.T) {
		all, warns := captureWarns(t)
		oc := &outputCtx{runID: "run-long", storeBase: filepath.Join(t.TempDir(), "outputs")}
		id := strings.Repeat("_", 64) // legal to the control plane's reader (64 bytes)
		longRec := rec
		longRec.Step = strings.Repeat("_", 128)
		oc.store(id, longRec, st)
		if w := warns(); len(w) != 1 || !strings.Contains(w[0], "run-long") || !strings.Contains(w[0], id) {
			t.Fatalf("want exactly one warn naming run and scenario id, got %q", w)
		}
		if strings.Contains(all(), canary) {
			t.Errorf("the body canary reached the log: %s", all())
		}
		if _, err := ReadOutputFile(oc.storeBase, "run-long", id, longRec.Step, 1); !errors.Is(err, ErrOutputNotFound) {
			t.Errorf("get_output must answer the closed no-recorded-output words, got %v", err)
		}
	})

	t.Run("canary absent from every line", func(t *testing.T) {
		all, _ := captureWarns(t)
		blocker := filepath.Join(t.TempDir(), "f")
		_ = os.WriteFile(blocker, []byte("x"), 0o600)
		oc := &outputCtx{runID: "r", storeBase: filepath.Join(blocker, "o")}
		oc.store("CHK-2", rec, st)
		if strings.Contains(all(), canary) {
			t.Errorf("the body canary reached the log: %s", all())
		}
	})
}

// fakeRemover records every path and deletes it ONLY when it is inside root (the test's own temp tree).
type fakeRemover struct {
	root  string
	calls []string
}

func (f *fakeRemover) remove(p string) error {
	f.calls = append(f.calls, p)
	if !strings.HasPrefix(p, f.root+string(filepath.Separator)) {
		return errors.New("fake: refusing to delete outside the test's temp tree")
	}
	return os.Remove(p)
}

func TestRefire_RemovesTheEarlierAttemptsHigherSamplesAndSteps_RecordingFake(t *testing.T) {
	cfg := &config.Config{}
	s := mustParse(t, cmpHTTPMD+cmpSection)

	t.Run("jmeter samples 3 then 1", func(t *testing.T) {
		root := t.TempDir()
		fake := &fakeRemover{root: root}
		oc := &outputCtx{runID: "run-1", storeBase: filepath.Join(root, "outputs"), captureRoot: filepath.Join(root, "capture"), remove: fake.remove}
		attempt := func(bodies ...string) {
			dir := filepath.Join(oc.captureRoot, "c-x")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			for i, b := range bodies {
				n := string(rune('1' + i))
				_ = os.WriteFile(filepath.Join(dir, n+".status"), []byte("200"), 0o600)
				_ = os.WriteFile(filepath.Join(dir, n+".body"), []byte(b), 0o600)
			}
			cp := &jmeterCapture{oc: oc, hostDir: dir, jmDir: dir}
			cp.collect(cfg, s)
		}
		attempt(`{"n":1}`, `{"n":2}`, `{"n":3}`)
		for n := 1; n <= 3; n++ {
			if _, err := ReadOutputFile(oc.storeBase, "run-1", s.ID, "", n); err != nil {
				t.Fatalf("attempt 1 sample %d: %v", n, err)
			}
		}
		attempt(`{"n":10}`)
		if b, err := ReadOutputFile(oc.storeBase, "run-1", s.ID, "", 1); err != nil || !strings.Contains(string(b), "10") {
			t.Fatalf("sample 1 must be attempt 2's: %q %v", b, err)
		}
		for n := 2; n <= 3; n++ {
			if b, err := ReadOutputFile(oc.storeBase, "run-1", s.ID, "", n); !errors.Is(err, ErrOutputNotFound) {
				t.Errorf("sample %d of the earlier attempt is still readable: %q %v", n, b, err)
			}
		}
		runDir := filepath.Join(oc.storeBase, "run-1")
		for _, c := range fake.calls {
			if filepath.Dir(c) != runDir {
				t.Errorf("the remover was handed %q, outside this run's directory", c)
			}
		}
	})

	t.Run("chain steps a,b then a; another check and another run are untouched", func(t *testing.T) {
		root := t.TempDir()
		fake := &fakeRemover{root: root}
		oc := &outputCtx{runID: "run-1", storeBase: filepath.Join(root, "outputs"), remove: fake.remove}
		cs := mustParse(t, cmpChainMD("CHN-R", oneStepTrigger, oneStepExpect, cmpSection))
		step := func(name string) report.StepResult {
			rec, st := cmpStored(t, `{"s":"`+name+`"}`)
			rec.Step = name
			return report.StepResult{Output: &report.RecordedOutput{Record: rec, Stored: st}}
		}
		// neighbours that must survive: same run other check, and the same check in another run
		_ = WriteOutputFile(oc.storeBase, "run-1", "CHN-R2", "a", 1, []byte(`{"keep":1}`))
		_ = WriteOutputFile(oc.storeBase, "run-0", "CHN-R", "a", 1, []byte(`{"keep":2}`))

		res := report.ScenarioResult{ID: cs.ID, Steps: []report.StepResult{step("a"), step("b")}}
		oc.attachChainOutputs(cs, &res, true)
		res2 := report.ScenarioResult{ID: cs.ID, Steps: []report.StepResult{step("a")}}
		oc.attachChainOutputs(cs, &res2, true)

		if _, err := ReadOutputFile(oc.storeBase, "run-1", cs.ID, "a", 1); err != nil {
			t.Errorf("step a of attempt 2: %v", err)
		}
		if b, err := ReadOutputFile(oc.storeBase, "run-1", cs.ID, "b", 1); !errors.Is(err, ErrOutputNotFound) {
			t.Errorf("step b of the earlier attempt is still readable: %q %v", b, err)
		}
		if _, err := ReadOutputFile(oc.storeBase, "run-1", "CHN-R2", "a", 1); err != nil {
			t.Errorf("another check's file was removed: %v", err)
		}
		if _, err := ReadOutputFile(oc.storeBase, "run-0", "CHN-R", "a", 1); err != nil {
			t.Errorf("another run's file was removed: %v", err)
		}
	})
}
