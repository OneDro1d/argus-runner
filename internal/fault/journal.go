package fault

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Undo is one durable undo-journal entry: everything needed to put one fault back, written BEFORE the fault
// is applied and removed only after the undo succeeded.
//
// It carries the RESOLVED coordinates (namespace, deployment, Toxiproxy address) rather than allowlist
// names, on purpose: the entry exists for the day the executor died mid-run, and an operator who has since
// edited argus-config.yaml must not turn "undo the fault" into "cannot find the target". The file lives on
// the executor's own results volume (0600), the same custody as its identity and floor files; nothing in it
// is ever copied to a report, a log line or an error.
type Undo struct {
	ID    string `json:"id"`
	Seq   int64  `json:"seq"`
	Run   string `json:"run"`
	Owner string `json:"owner"`
	Step  string `json:"step"`
	Op    string `json:"op"`
	// Target is the allowlist name (what the report calls it).
	Target string `json:"target"`
	Kind   string `json:"kind"` // KindScale | KindToxic | KindProxy

	Namespace  string `json:"namespace,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	Replicas   int    `json:"replicas"` // KindScale: the count to return to

	ToxiAPI        string `json:"toxiproxy_api,omitempty"`
	Proxy          string `json:"proxy,omitempty"`
	Toxic          string `json:"toxic,omitempty"`           // KindToxic: the Argus-owned toxic name
	RestoreEnabled bool   `json:"restore_enabled,omitempty"` // KindProxy: it was enabled before, so enable it again

	Params    map[string]int `json:"params,omitempty"`
	AppliedAt string         `json:"applied_at"`
}

const (
	KindScale = "scale"
	KindToxic = "toxic"
	KindProxy = "proxy"
)

// Journal is a directory of undo entries, one file each. The results volume is shared by every executor
// replica (RWX), so each entry names its owner and is touched while its owner lives (see Manager): a
// replica starting up may only replay an entry whose owner is dead.
type Journal struct{ Dir string }

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func safe(s string) string { return unsafeName.ReplaceAllString(s, "_") }

func (j *Journal) file(u Undo) string {
	return filepath.Join(j.Dir, fmt.Sprintf("%020d-%s.json", u.Seq, safe(u.ID)))
}

// Write makes u durable: temp file, fsync, rename, fsync of the directory. An error means the fault MUST NOT
// be applied.
func (j *Journal) Write(u Undo) error {
	if j == nil || j.Dir == "" {
		return errors.New("no undo journal is configured")
	}
	if err := os.MkdirAll(j.Dir, 0o700); err != nil {
		return fmt.Errorf("undo journal: %w", err)
	}
	b, err := json.Marshal(u)
	if err != nil {
		return err
	}
	final := j.file(u)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("undo journal: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("undo journal: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("undo journal: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("undo journal: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("undo journal: %w", err)
	}
	if d, err := os.Open(j.Dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// Remove deletes u's entry. Absent is success.
func (j *Journal) Remove(u Undo) error {
	if err := os.Remove(j.file(u)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Touch marks u's owner as alive.
func (j *Journal) Touch(u Undo, now time.Time) { _ = os.Chtimes(j.file(u), now, now) }

// Age is how long u's entry has gone untouched.
func (j *Journal) Age(u Undo, now time.Time) time.Duration {
	st, err := os.Stat(j.file(u))
	if err != nil {
		return 0
	}
	return now.Sub(st.ModTime())
}

// Pending lists every entry still in the journal, oldest first. An entry that cannot be read is an ERROR
// returned beside the ones that can: a corrupt undo is a fault nobody can put back, and silence about it
// would read as an empty journal.
func (j *Journal) Pending() ([]Undo, error) {
	ents, err := os.ReadDir(j.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Undo
	var bad []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(j.Dir, e.Name()))
		var u Undo
		if err != nil || json.Unmarshal(b, &u) != nil || u.ID == "" || u.Kind == "" {
			bad = append(bad, e.Name())
			continue
		}
		out = append(out, u)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Seq < out[b].Seq })
	if len(bad) > 0 {
		return out, fmt.Errorf("undo journal: %d unreadable entr(y/ies) left in place for an operator: %s", len(bad), strings.Join(bad, ", "))
	}
	return out, nil
}

// ── target locks ────────────────────────────────────────────────────────────────────────────────

// ErrLocked is a target faulted by another live run.
var ErrLocked = errors.New("faulted by another run")

func (j *Journal) lockFile(key string) string {
	return filepath.Join(j.Dir, "locks", safe(key)+".lock")
}

// Lock claims a target for owner/run. A live claim by someone else (touched within stale) is ErrLocked; a
// stale one is taken over. Two runs faulting one deployment would each record "found N" and the second's
// undo would put back the first's fault.
func (j *Journal) Lock(key, owner, run string, stale time.Duration, now time.Time) error {
	p := j.lockFile(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	mine := owner + "/" + run
	if b, err := os.ReadFile(p); err == nil {
		st, serr := os.Stat(p)
		if string(b) != mine && serr == nil && now.Sub(st.ModTime()) < stale {
			return ErrLocked
		}
	}
	if err := os.WriteFile(p, []byte(mine), 0o600); err != nil {
		return err
	}
	_ = os.Chtimes(p, now, now)
	return nil
}

func (j *Journal) TouchLock(key string, now time.Time) { _ = os.Chtimes(j.lockFile(key), now, now) }

func (j *Journal) Unlock(key string) { _ = os.Remove(j.lockFile(key)) }
