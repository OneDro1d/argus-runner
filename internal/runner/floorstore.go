package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// floorstore.go — the executor's DURABLE copy of the published version floors (VR-V5 / V17-010).
//
// WHY DURABLE AND NOT JUST IN MEMORY. The owner's instruction was "assume the version numbers are the
// same as last time (so keep it in memory)". The intent is right; memory alone does not achieve it. A
// container restart clears memory, so an executor that restarts while the control plane is
// unreachable comes back knowing nothing — and would then have to either refuse everything or allow
// everything, both of which are verdicts invented from an absence.
//
// So: captured at onboarding, read at startup, and overwritten on every successful poll. "Last known"
// then genuinely means last known.
//
// WHERE. Next to the identity key, which is the executor's existing durable state and survives a pod
// recreate on the shared results volume — the same place the retired self-update guard lived, and for
// the same reason.
//
// FAIL-CLOSED, DELIBERATELY. Once floors are known they stay known: an executor already judged below
// the absolute floor does not become usable again because the control plane went away. But an
// executor that has NEVER been told anything is not judged at all — see FloorState, where an
// incomplete pair is FloorUnknown and FloorBlocks lets it work.

const floorsFileName = "version-floors.json"

// FloorStore reads and writes the executor's last-known floors. Safe for concurrent use: the poll
// loop writes while a run preflight reads.
type FloorStore struct {
	path string
	mu   sync.RWMutex
	cur  federation.VersionFloors
}

// NewFloorStore returns a store keyed to the executor's identity directory, pre-loaded from disk so a
// restart starts from what was last known rather than from nothing.
func NewFloorStore(identityPath string) *FloorStore {
	fs := &FloorStore{path: filepath.Join(filepath.Dir(identityPath), floorsFileName)}
	fs.cur = fs.readFile()
	return fs
}

// Get returns the last-known floors.
func (f *FloorStore) Get() federation.VersionFloors {
	if f == nil {
		return federation.VersionFloors{}
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.cur
}

// Observe records floors seen on a poll. INCOMPLETE FLOORS ARE IGNORED rather than stored: a control
// plane that publishes only one number, or none, must not erase what we already knew. That is the
// difference between "the other end got older" and "we forgot".
func (f *FloorStore) Observe(v federation.VersionFloors) {
	if f == nil || !v.Complete() {
		return
	}
	f.mu.Lock()
	changed := v != f.cur
	f.cur = v
	f.mu.Unlock()
	if !changed {
		return // the common case: do not rewrite a file on every poll
	}
	f.writeFile(v)
}

// State ranks a running version against the last-known floors.
func (f *FloorStore) State(version string) string {
	return federation.FloorState(version, f.Get())
}

func (f *FloorStore) readFile() federation.VersionFloors {
	var out federation.VersionFloors
	b, err := os.ReadFile(f.path)
	if err != nil {
		return out // absent → nothing known yet, which FloorState reads as unknown, not as a fault
	}
	_ = json.Unmarshal(b, &out)
	return out
}

// writeFile is best-effort by design: failing to persist must not stop an executor working. The
// in-memory copy is already updated, so the only cost of a failed write is that a RESTART falls back
// to the previous value — strictly better than refusing work over a file.
func (f *FloorStore) writeFile(v federation.VersionFloors) {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = os.WriteFile(f.path, b, 0o644)
}
