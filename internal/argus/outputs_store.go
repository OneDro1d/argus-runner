package argus

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/OneDro1d/argus-runner/internal/compare"
)

// The stored outputs of compare runs (ARGUS-CMP-3), executor disk only:
//
//	<ResultsRoot>/<instance>/outputs/<run_id>/<scenario_id>[__<step>].<sample>.json
//
// File mode 0600, directories 0700 (not group- or world-readable). The newest KeepNewestOutputRuns run
// directories are kept per instance; older ones are removed only after a successful results push
// (runner.OutputRetention). Every path built here comes from five inputs and a closed grammar, and every
// one of them is checked before a path exists: nothing outside OutputsBase can be read, written or
// removed through this file, also through a symlink.

// ErrOutputNotFound is the closed answer for a recorded output that is not on disk. It carries no path.
var ErrOutputNotFound = errors.New("no recorded output for that run, check, step and sample")

// ErrBadOutputRef is the closed answer for an input that could name something outside the outputs
// directory. It echoes nothing the caller sent.
var ErrBadOutputRef = errors.New("the run id, check id, step or sample is not a valid reference to a recorded output")

// ErrOutputNameTooLong is a (scenario id, step, sample) whose file name would not fit a file system. It
// can never have been stored, so a read answers ErrOutputNotFound and a write is a warned failure.
var ErrOutputNameTooLong = errors.New("the check id and step make a file name that is too long to store")

// ErrOutputTooLarge is the closed, body-free answer for a stored file that is over the size a relayed
// answer may have (compare.MaxStoredFileBytes): the executor never returns it.
var ErrOutputTooLarge = errors.New("the recorded output is too large to return")

// KeepNewestOutputRuns is how many compare runs' directories an instance keeps.
const KeepNewestOutputRuns = 20

// maxOutputFileBytes bounds what ReadOutputFile will return: the stored file is cut at store time to
// compare.MaxStoredFileBytes (encoded size), well under the control plane's 1 MiB command-result cap, so
// a file over it is an older executor's or not ours, and is answered ErrOutputTooLarge, never returned.
const maxOutputFileBytes = compare.MaxStoredFileBytes

// OutputsBase is the directory that holds the stored outputs of one instance's compare runs; resultsDir
// is <ResultsRoot>/<instance>.
func OutputsBase(resultsDir string) string { return filepath.Join(resultsDir, "outputs") }

// validRefComponent: one path element and nothing else. Empty, "." , anything holding "..", a path
// separator of either kind, a NUL or another control character, or an absolute path is refused.
func validRefComponent(s string) bool {
	if s == "" || len(s) > 128 || s == "." || strings.Contains(s, "..") || filepath.IsAbs(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f || c == '/' || c == '\\' {
			return false
		}
	}
	return true
}

// maxOutputFileNameBytes is a file name's limit on the file systems an executor runs on, less the ".tmp"
// the writer appends while it stores.
const maxOutputFileNameBytes = 255 - len(".tmp")

// escapeRefComponent makes one component of a file name that can be told apart from every other one once
// it is joined to its neighbours by "__". "~" is always written "~7e", and an underscore that is first,
// last or next to another underscore is written "~5f", so an escaped component never holds "__" and
// never starts or ends with "_". An ordinary id ("S-1", "CHN_A1", "create") is unchanged.
func escapeRefComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '~':
			b.WriteString("~7e")
		case c == '_' && (i == 0 || i == len(s)-1 || s[i-1] == '_' || s[i+1] == '_'):
			b.WriteString("~5f")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// OutputFileName is <scenario_id>[__<step>].<sample>.json, after the four inputs are checked. The
// mapping is injective: the first "__" of the stem is the separator (an escaped component holds none),
// and the sample is the last number before ".json", so two different (scenario id, step, sample) never
// share a file. A name that cannot fit a file system is ErrOutputNameTooLong.
func OutputFileName(scenarioID, step string, sample int) (string, error) {
	if !validRefComponent(scenarioID) || (step != "" && !validRefComponent(step)) || sample < 1 || sample > compare.MaxSamples {
		return "", ErrBadOutputRef
	}
	name := escapeRefComponent(scenarioID)
	if step != "" {
		name += "__" + escapeRefComponent(step)
	}
	name += "." + strconv.Itoa(sample) + ".json"
	if len(name) > maxOutputFileNameBytes {
		return "", ErrOutputNameTooLong
	}
	return name, nil
}

// runDir returns <base>/<runID> after the run id is checked and the directory is proven to be a real
// directory (not a symlink) that resolves directly inside base. exists is false when it is simply not
// there (no error: nothing to read or remove).
func runDir(base, runID string) (dir string, exists bool, err error) {
	if !validRefComponent(runID) {
		return "", false, ErrBadOutputRef
	}
	dir = filepath.Join(base, runID)
	fi, lerr := os.Lstat(dir)
	if lerr != nil {
		// a base that sits under a regular file (an unwritable store) is simply not there either
		if os.IsNotExist(lerr) || errors.Is(lerr, syscall.ENOTDIR) {
			return dir, false, nil
		}
		return "", false, ErrBadOutputRef
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return "", false, ErrBadOutputRef
	}
	rb, berr := filepath.EvalSymlinks(base)
	rd, derr := filepath.EvalSymlinks(dir)
	if berr != nil || derr != nil || filepath.Dir(rd) != rb {
		return "", false, ErrBadOutputRef
	}
	return dir, true, nil
}

// WriteOutputFile stores one output file, mode 0600, in directories that are not group- or world-accessible.
func WriteOutputFile(base, runID, scenarioID, step string, sample int, data []byte) error {
	name, err := OutputFileName(scenarioID, step, sample)
	if err != nil {
		return err
	}
	if !validRefComponent(runID) {
		return ErrBadOutputRef
	}
	if err := os.MkdirAll(filepath.Join(base, runID), 0o700); err != nil {
		return fmt.Errorf("outputs directory: %w", err)
	}
	for _, d := range []string{base, filepath.Join(base, runID)} {
		if err := os.Chmod(d, 0o700); err != nil {
			return fmt.Errorf("outputs directory mode: %w", err)
		}
	}
	dir, _, err := runDir(base, runID)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("output file mode: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store output: %w", err)
	}
	return nil
}

// ReadOutputFile returns one stored output file. A file that is not there is ErrOutputNotFound; an input
// that could point outside the outputs directory is ErrBadOutputRef. Neither carries a path.
func ReadOutputFile(base, runID, scenarioID, step string, sample int) ([]byte, error) {
	name, err := OutputFileName(scenarioID, step, sample)
	if errors.Is(err, ErrOutputNameTooLong) {
		return nil, ErrOutputNotFound // such a name was never stored (the write was warned)
	}
	if err != nil {
		return nil, err
	}
	dir, exists, err := runDir(base, runID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrOutputNotFound
	}
	p := filepath.Join(dir, name)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, ErrOutputNotFound
	}
	if !fi.Mode().IsRegular() {
		return nil, ErrBadOutputRef
	}
	if fi.Size() > maxOutputFileBytes {
		return nil, ErrOutputTooLarge
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, ErrOutputNotFound
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxOutputFileBytes))
	if err != nil {
		return nil, ErrOutputNotFound
	}
	return b, nil
}

// RemoveOutputRun removes the directory of ONE run through remove (os.RemoveAll when nil), and only after
// the path guard: the run id must be one plain path element (not empty, not absolute, no separator, no
// ".."), the directory must be a real directory (not a symlink) and must resolve directly inside base.
// A guard failure is ErrBadOutputRef and remove is not called. A run directory that is not there is not
// an error.
func RemoveOutputRun(base, runID string, remove func(string) error) error {
	dir, exists, err := runDir(base, runID)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if remove == nil {
		remove = os.RemoveAll
	}
	return remove(dir)
}

// checkFileOf reports whether name is a stored file of the check whose escaped id is enc: <enc>.<n>.json
// or <enc>__<anything>.<n>.json. Nothing else matches (another check, a ".tmp", a directory entry).
func checkFileOf(name, enc string) bool {
	stem, ok := strings.CutSuffix(name, ".json")
	if !ok {
		return false
	}
	dot := strings.LastIndexByte(stem, '.')
	if dot < 0 || dot == len(stem)-1 {
		return false
	}
	for _, c := range stem[dot+1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	stem = stem[:dot]
	return stem == enc || strings.HasPrefix(stem, enc+"__")
}

// RemoveCheckOutputs removes, through remove (os.Remove when nil), the stored files of ONE check (every
// step and sample) in ONE run's directory: the earlier attempt's files, before a re-fire writes its own.
// The run directory goes through the same guard as RemoveOutputRun, only regular files directly inside it
// are candidates, and each path handed to remove is <run dir>/<name>. It never touches another check, another
// run, a directory or a symlink.
func RemoveCheckOutputs(base, runID, scenarioID string, remove func(string) error) error {
	if !validRefComponent(scenarioID) {
		return ErrBadOutputRef
	}
	dir, exists, err := runDir(base, runID)
	if err != nil || !exists {
		return err
	}
	if remove == nil {
		remove = os.Remove
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	enc := escapeRefComponent(scenarioID)
	var first error
	for _, e := range ents {
		if !e.Type().IsRegular() || !checkFileOf(e.Name(), enc) {
			continue
		}
		if rerr := remove(filepath.Join(dir, e.Name())); rerr != nil && first == nil {
			first = rerr
		}
	}
	return first
}

// PruneOutputs removes the directories of all but the newest keep runs (by directory modification time,
// the name breaking a tie), never the current run, each through RemoveOutputRun's guard. A file or a
// symlink in base is never a candidate. It returns the paths it handed to remove. keep < 1 removes
// nothing (a bad setting must not wipe the history).
func PruneOutputs(base string, keep int, current string, remove func(string) error) ([]string, error) {
	if keep < 1 {
		return nil, nil
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type cand struct {
		name string
		mod  int64
	}
	var runs []cand
	for _, e := range entries {
		if !e.IsDir() { // a DirEntry's type comes from lstat: a symlink to a directory is not a directory here
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		runs = append(runs, cand{e.Name(), info.ModTime().UnixNano()})
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].mod != runs[j].mod {
			return runs[i].mod > runs[j].mod
		}
		return runs[i].name > runs[j].name
	})
	var removed []string
	var first error
	for i, r := range runs {
		if i < keep || r.name == current {
			continue
		}
		var got string
		rm := remove
		if rm == nil {
			rm = os.RemoveAll
		}
		if err := RemoveOutputRun(base, r.name, func(p string) error { got = p; return rm(p) }); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if got != "" {
			removed = append(removed, got)
		}
	}
	return removed, first
}
