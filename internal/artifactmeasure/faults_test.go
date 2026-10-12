package artifactmeasure

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// Faults ride in the measurement record, so they are inside the anchored
// evidence_bundle_hash; a run with NO fault must hash exactly as it did before the field existed.

// legacyMeasurement is Measurement as it was declared BEFORE Faults: the bytes a no-fault run must still hash.
type legacyMeasurement struct {
	Version            int      `json:"version"`
	State              string   `json:"state"`
	Reason             string   `json:"reason,omitempty"`
	Source             string   `json:"source,omitempty"`
	Declared           string   `json:"declared,omitempty"`
	Running            []string `json:"running"`
	Unresolved         int      `json:"unresolved,omitempty"`
	CommitmentFound    []string `json:"commitment_found"`
	CommitmentNotFound []string `json:"commitment_not_found"`
}

const dig = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func baseMeasurement() Measurement {
	return Measurement{Version: Version, State: StateMatched, Source: "k8s", Declared: dig, Running: []string{dig},
		CommitmentFound: []string{}, CommitmentNotFound: []string{}}
}

func TestDigest_ARunWithoutFaultsIsByteIdenticalToTheOldRecord(t *testing.T) {
	m := baseMeasurement()
	old := legacyMeasurement{Version: m.Version, State: m.State, Source: m.Source, Declared: m.Declared,
		Running: m.Running, CommitmentFound: []string{}, CommitmentNotFound: []string{}}
	b, _ := json.Marshal(old)
	sum := sha256.Sum256(b)
	if got := m.Digest(); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("a measurement with no faults hashes differently from the pre-fault record: %s vs %x", got, sum)
	}
	m.Faults = []Fault{} // empty, not nil: still no key
	if got := m.Digest(); got != hex.EncodeToString(sum[:]) {
		t.Fatal("an empty fault list changed the digest")
	}
}

func TestBundleHash_FaultsAreBound(t *testing.T) {
	const root = "scenario-root"
	plain := baseMeasurement()
	withFault := baseMeasurement()
	withFault.Faults = []Fault{{Op: "stop", Target: "trades", Params: map[string]int{"replicas": 0},
		AppliedAt: "2026-10-09T10:00:00Z", RestoredAt: "2026-10-09T10:00:05Z", RestoreOutcome: "restored"}}
	if BundleHash(root, &plain) == BundleHash(root, &withFault) {
		t.Fatal("the fault list is not inside the bundle hash")
	}
	// every field of a fault is covered: editing any one changes the hash
	for name, mut := range map[string]func(*Fault){
		"op":        func(f *Fault) { f.Op = "scale" },
		"target":    func(f *Fault) { f.Target = "other" },
		"params":    func(f *Fault) { f.Params = map[string]int{"replicas": 3} },
		"applied":   func(f *Fault) { f.AppliedAt = "2026-10-09T11:00:00Z" },
		"restored":  func(f *Fault) { f.RestoredAt = "" },
		"outcome":   func(f *Fault) { f.RestoreOutcome = "failed" },
		"toxiproxy": func(f *Fault) { f.Toxiproxy = true },
	} {
		m := baseMeasurement()
		m.Faults = append([]Fault(nil), withFault.Faults...)
		m.Faults[0].Params = map[string]int{"replicas": 0}
		mut(&m.Faults[0])
		if BundleHash(root, &m) == BundleHash(root, &withFault) {
			t.Errorf("editing the fault's %s did not change the bundle hash", name)
		}
	}
}

func TestMeasurement_FaultsRoundTripThroughJSON(t *testing.T) {
	m := baseMeasurement()
	m.Faults = []Fault{{Op: "delay", Target: "mcp-to-trades", Params: map[string]int{"latency_ms": 5000, "jitter_ms": 100},
		AppliedAt: "a", RestoredAt: "b", RestoreOutcome: "restored", Toxiproxy: true}}
	b, _ := json.Marshal(m)
	var back Measurement
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Digest() != m.Digest() {
		t.Fatal("a measurement with faults does not survive the wire")
	}
}
