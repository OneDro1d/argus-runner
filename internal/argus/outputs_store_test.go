package argus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ⛔ DELETION SAFETY (ARGUS-CMP-3 brief). Every tree here is built by the test under t.TempDir(). The
// guard tests inject a RECORDING FAKE for the remover: it records the path it was given and deletes
// NOTHING, so a guard that is broken or removed shows up as a recorded call, never as a real deletion.
// Nothing here ever hands a deleting function "/", "/tmp", $HOME, an empty string or a directory the
// test did not create.

type recordingRemover struct{ calls []string }

func (r *recordingRemover) remove(p string) error {
	r.calls = append(r.calls, p)
	return nil // deliberately deletes nothing
}

// outputsFixture builds <tmp>/outputs with a real run directory "run-ok", a file, and a symlink
// "link" that points at a directory OUTSIDE the outputs base (also a temp dir the test made).
func outputsFixture(t *testing.T) (base, outside string) {
	t.Helper()
	root := t.TempDir()
	base = filepath.Join(root, "outputs")
	if err := os.MkdirAll(filepath.Join(base, "run-ok"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "run-ok", "S.1.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside = t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "precious.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return base, outside
}

func TestRemoveOutputRun_GuardRefusesEveryUnsafeRunID_RecordingFake(t *testing.T) {
	base, outside := outputsFixture(t)
	abs := filepath.Join(outside, "precious.txt")
	unsafeIDs := []string{"", "/etc", abs, "a/b", `a\b`, "..", "../x", "run-ok/../..", ".", "a\x00b", "link"}
	unsafeCalls := 0
	for _, id := range unsafeIDs {
		fake := &recordingRemover{}
		err := RemoveOutputRun(base, id, fake.remove)
		unsafeCalls += len(fake.calls)
		if err == nil {
			t.Errorf("run id %q was not refused", id)
		}
		if !errors.Is(err, ErrBadOutputRef) {
			t.Errorf("run id %q: error %v is not ErrBadOutputRef", id, err)
		}
		if len(fake.calls) != 0 {
			t.Errorf("run id %q reached the remover with %q", id, fake.calls)
		}
	}
	// the positive control: a real run directory reaches the remover, with exactly its own path
	fake := &recordingRemover{}
	if err := RemoveOutputRun(base, "run-ok", fake.remove); err != nil {
		t.Fatalf("a valid run id was refused: %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != filepath.Join(base, "run-ok") {
		t.Fatalf("the remover got %q, want exactly the run directory", fake.calls)
	}
	// and the fake really deleted nothing, nor was anything outside touched
	if _, err := os.Stat(filepath.Join(base, "run-ok", "S.1.json")); err != nil {
		t.Errorf("the recording fake must delete nothing, but the run file is gone: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Errorf("a file outside the outputs directory is gone: %v", err)
	}
	t.Logf("EVIDENCE: %d unsafe run ids -> %d remover call(s); 1 valid id -> %d call(s) recorded; the recording fake deleted nothing (the run file and the outside file are both still present)", len(unsafeIDs), unsafeCalls, len(fake.calls))
}

func TestRemoveOutputRun_DefaultRemoverOnlyRemovesTheDirectoryTheTestBuilt(t *testing.T) {
	base, outside := outputsFixture(t)
	if err := RemoveOutputRun(base, "run-ok", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "run-ok")); !os.IsNotExist(err) {
		t.Errorf("the run directory is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "precious.txt")); err != nil {
		t.Errorf("the default remover reached outside the outputs base: %v", err)
	}
	if err := RemoveOutputRun(base, "link", nil); !errors.Is(err, ErrBadOutputRef) {
		t.Errorf("a symlink run id must be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "precious.txt")); err != nil {
		t.Errorf("a symlinked run id led the remover outside: %v", err)
	}
}

func TestPruneOutputs_KeepsTheNewestAndTheCurrentRun_RecordingFake(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "outputs")
	now := time.Now()
	for i := 1; i <= 25; i++ {
		d := filepath.Join(base, fmt.Sprintf("r%02d", i))
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		ts := now.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(d, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	// a stray file and a symlink in the base are never candidates
	if err := os.WriteFile(filepath.Join(base, "stray.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "r00-link")); err == nil {
		old := now.Add(-time.Hour)
		_ = os.Chtimes(filepath.Join(base, "r00-link"), old, old)
	}

	fake := &recordingRemover{}
	removed, err := PruneOutputs(base, 20, "", fake.remove)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"r01", "r02", "r03", "r04", "r05"}
	if got := baseNames(fake.calls); strings.Join(sortedCopy(got), ",") != strings.Join(want, ",") {
		t.Fatalf("removed %v, want the 5 oldest %v", got, want)
	}
	if len(removed) != 5 {
		t.Errorf("returned %v", removed)
	}

	// the run that was just pushed is never removed even when it is not among the newest
	fake = &recordingRemover{}
	if _, err := PruneOutputs(base, 20, "r02", fake.remove); err != nil {
		t.Fatal(err)
	}
	if got := baseNames(fake.calls); strings.Join(sortedCopy(got), ",") != "r01,r03,r04,r05" {
		t.Fatalf("with current=r02 removed %v", got)
	}

	// fewer than keep: nothing is removed
	fake = &recordingRemover{}
	if _, err := PruneOutputs(base, 30, "", fake.remove); err != nil || len(fake.calls) != 0 {
		t.Fatalf("under the limit removed %v (err %v)", fake.calls, err)
	}
	// a missing outputs directory is not an error
	if _, err := PruneOutputs(filepath.Join(root, "nope"), 20, "", fake.remove); err != nil {
		t.Fatalf("missing base: %v", err)
	}
	// every run directory still exists: the fake deleted nothing
	for i := 1; i <= 25; i++ {
		if _, err := os.Stat(filepath.Join(base, fmt.Sprintf("r%02d", i))); err != nil {
			t.Fatalf("r%02d is gone: the recording fake must not delete", i)
		}
	}
}

func baseNames(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func TestWriteOutputFile_ModesAndRoundTrip(t *testing.T) {
	base := filepath.Join(t.TempDir(), "outputs")
	data := []byte(`{"status":200}`)
	if err := WriteOutputFile(base, "run-1", "S-1", "create", 1, data); err != nil {
		t.Fatal(err)
	}
	if err := WriteOutputFile(base, "run-1", "S-1", "", 2, data); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(base, "run-1", "S-1__create.1.json")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("the file is not at the contract path: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v, want 0600", st.Mode().Perm())
	}
	for _, d := range []string{base, filepath.Join(base, "run-1")} {
		ds, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if ds.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s mode %v is group- or world-accessible", filepath.Base(d), ds.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(base, "run-1", "S-1.2.json")); err != nil {
		t.Errorf("a step-less file must be <scenario>.<sample>.json: %v", err)
	}
	got, err := ReadOutputFile(base, "run-1", "S-1", "create", 1)
	if err != nil || string(got) != string(data) {
		t.Fatalf("read back %q, %v", got, err)
	}
}

func TestReadOutputFile_MissingIsClosedAndEscapesAreRefused(t *testing.T) {
	base, outside := outputsFixture(t)
	if err := os.WriteFile(filepath.Join(outside, "S-1.1.json"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadOutputFile(base, "run-ok", "S-1", "", 1)
	if !errors.Is(err, ErrOutputNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	if strings.Contains(err.Error(), base) || strings.Contains(err.Error(), "outputs") && strings.Contains(err.Error(), string(filepath.Separator)) {
		t.Errorf("the error carries a filesystem path: %q", err)
	}
	cases := []struct {
		run, sc, step string
		sample        int
	}{
		{"link", "S-1", "", 1},       // run dir is a symlink to a directory holding S-1.1.json
		{"../outputs", "S-1", "", 1}, // traversal
		{"", "S-1", "", 1},
		{"run-ok", "../S-1", "", 1},
		{"run-ok", "S/1", "", 1},
		{"run-ok", "", "", 1},
		{"run-ok", "S-1", "a/b", 1},
		{"run-ok", "S-1", "..", 1},
		{"run-ok", "S-1", "", 0},
		{"run-ok", "S-1", "", 9},
		{"run-ok", "S-1", "", -1},
	}
	for _, c := range cases {
		b, err := ReadOutputFile(base, c.run, c.sc, c.step, c.sample)
		if !errors.Is(err, ErrBadOutputRef) {
			t.Errorf("%+v: got %q, %v; want ErrBadOutputRef", c, b, err)
		}
		if strings.Contains(string(b), "SECRET") {
			t.Errorf("%+v read a file outside the outputs directory", c)
		}
	}
}
