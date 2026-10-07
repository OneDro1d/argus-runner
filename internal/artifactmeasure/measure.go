// Package artifactmeasure is the record of WHAT WAS ACTUALLY RUNNING when a certifying run began.
//
// A final or scheduled run is declared against an artifact digest. Until this package the declaration
// was only echoed: nothing compared it with the system under test. The executor now reads the image
// digests of the SUT's running containers before any scenario runs (system.go), Evaluate compares them
// with the declared digest, and the resulting Measurement is bound into the run's evidence_bundle_hash
// (BundleHash) — so the digest the verdict anchors is no longer only what the tester said.
//
// Three outcomes, and only these:
//
//	matched       the declared digest is among the digests running in the SUT
//	not_measured  the executor could not establish it (no permission, unknown tier, tag-only imageIDs,
//	              no SUT found, no declared digest to compare) — with the reason, never silent
//	mismatch      a PROVEN disagreement: every running container reported a digest and the declared
//	              one is not among them. In memory only — a mismatched run is refused and never bound.
package artifactmeasure

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Version is the Measurement record's own version. It is inside the hashed bytes.
const Version = 1

const (
	StateMatched     = "matched"
	StateNotMeasured = "not_measured"
	// StateMismatch never leaves the executor: it refuses the run and nothing is bound.
	StateMismatch = "mismatch"
)

// LegacyReason is what an unbound v3 measurement says: the executor reported no measurement at all
// (executors released before measurement never do). Absence is read as "too old", never as "matched".
// The control plane issues such a run as v2, which carries no measurement; verify still accepts an
// unbound v3 block for a certificate relabelled to claim less.
const LegacyReason = "the executor that ran this predates artifact measurement, so nothing binds what was running"

// bundleDomain separates the measured bundle hash from the scenario-only one, so no scenario set can be
// crafted to collide with a bundle that includes a measurement.
const bundleDomain = "argus-evidence-bundle/v2"

var digestRe = regexp.MustCompile(`^[a-z0-9]+(?:[+._-][a-z0-9]+)*:[a-f0-9]{32,}$`)

// NormalizeDigest lower-cases and trims s and reports whether it is an OCI digest (algorithm:hex).
func NormalizeDigest(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	return s, digestRe.MatchString(s)
}

// DigestFromImageID extracts the registry digest from a Kubernetes containerStatus imageID
// ("docker.io/x/y@sha256:…", "docker-pullable://x@sha256:…"). A tag, an empty string and a bare
// "sha256:…" (a local image's config id, not a registry digest) yield no digest.
func DigestFromImageID(id string) (string, bool) {
	i := strings.LastIndex(id, "@")
	if i < 0 {
		return "", false
	}
	return NormalizeDigest(id[i+1:])
}

// Reading is what a measurer saw. Reason non-empty: it could not read at all.
type Reading struct {
	Source     string   // "k8s" | "compose"
	Digests    []string // digests of running containers that reported one
	Unresolved int      // running containers that reported none (tag-only, locally built)
	Reason     string
}

// Measurement is the bound record. Field order is the canonical JSON order (encoding/json) — the hash is
// over these bytes, so a reordered struct is a format change and needs a new Version.
type Measurement struct {
	Version  int    `json:"version"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	Source   string `json:"source,omitempty"`
	Declared string `json:"declared,omitempty"`
	// Running is sorted and unique, never nil.
	Running    []string `json:"running"`
	Unresolved int      `json:"unresolved,omitempty"`
	// CommitmentFound / CommitmentNotFound record which of the sealed commitment's image_digests were seen
	// running. Recorded, not enforced: only the declared digest decides the run.
	CommitmentFound    []string `json:"commitment_found"`
	CommitmentNotFound []string `json:"commitment_not_found"`
}

func sortedUnique(in []string) []string {
	set := map[string]bool{}
	for _, s := range in {
		if n, ok := NormalizeDigest(s); ok {
			set[n] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Evaluate compares the declared artifact digest with a Reading. The rule for several running images:
// the declared digest must be ONE OF them (a SUT runs sidecars and databases besides the build under
// test). A mismatch is only PROVEN when every running container reported a digest; a container that
// reported none could be the declared build, so that is not_measured, never a refusal.
func Evaluate(declared string, commitmentImages []string, r Reading) Measurement {
	m := Measurement{Version: Version, Source: r.Source, Unresolved: r.Unresolved,
		Running: []string{}, CommitmentFound: []string{}, CommitmentNotFound: []string{}}
	if d, ok := NormalizeDigest(declared); ok {
		declared = d
	} else {
		declared = strings.TrimSpace(declared)
	}
	m.Declared = declared
	running := sortedUnique(r.Digests)
	switch {
	case r.Reason != "":
		m.State, m.Reason = StateNotMeasured, r.Reason
		m.Unresolved = 0
		return m
	case len(running) == 0:
		m.State = StateNotMeasured
		m.Reason = fmt.Sprintf("no running container reported an image digest (%d container(s) gave a tag or a local image id only)", r.Unresolved)
		return m
	}
	m.Running = running
	found, notFound := []string{}, []string{}
	for _, ci := range sortedUnique(commitmentImages) {
		if contains(running, ci) {
			found = append(found, ci)
		} else {
			notFound = append(notFound, ci)
		}
	}
	m.CommitmentFound, m.CommitmentNotFound = found, notFound
	switch {
	case declared == "":
		m.State, m.Reason = StateNotMeasured, "not compared: no artifact digest was declared for this run"
	case contains(running, declared):
		m.State = StateMatched
	case r.Unresolved > 0:
		m.State = StateNotMeasured
		m.Reason = fmt.Sprintf("the declared digest is not among the %d digest(s) found, but %d container(s) reported no digest, so a mismatch is not proven", len(running), r.Unresolved)
	default:
		m.State = StateMismatch
	}
	return m
}

func contains(sorted []string, s string) bool {
	i := sort.SearchStrings(sorted, s)
	return i < len(sorted) && sorted[i] == s
}

// MismatchError is the refusal of a certifying run whose declared digest is not running.
type MismatchError struct {
	Declared string
	Running  []string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("refused: the declared artifact digest %s is not among the image digests running in the system under test [%s] — no scenario was run and nothing was bound",
		e.Declared, strings.Join(e.Running, ", "))
}

// RefuseIfMismatch returns a *MismatchError for a proven mismatch, nil otherwise.
func RefuseIfMismatch(m Measurement) error {
	if m.State == StateMismatch {
		return &MismatchError{Declared: m.Declared, Running: m.Running}
	}
	return nil
}

// Check is the one consistency rule shared by the executor, the control plane (before it accepts a push)
// and `certificate verify`: a record may be bound only if its state is what its own facts imply. A
// mismatch is never bindable.
func Check(m Measurement, declared string) error {
	if m.Version != Version {
		return fmt.Errorf("artifact measurement version %d is not %d", m.Version, Version)
	}
	if d, ok := NormalizeDigest(declared); ok {
		declared = d
	}
	if m.Declared != declared {
		return fmt.Errorf("artifact measurement declares %q but the verdict binds %q", m.Declared, declared)
	}
	if !sort.StringsAreSorted(m.Running) {
		return fmt.Errorf("artifact measurement running digests are not sorted")
	}
	for i, d := range m.Running {
		if n, ok := NormalizeDigest(d); !ok || n != d || (i > 0 && m.Running[i-1] == d) {
			return fmt.Errorf("artifact measurement running digest %q is not a unique normalized digest", d)
		}
	}
	switch m.State {
	case StateMatched:
		if m.Reason != "" {
			return fmt.Errorf("a matched artifact measurement carries a reason")
		}
		if declared == "" || !contains(m.Running, declared) {
			return fmt.Errorf("artifact measurement claims matched but %q is not among the running digests %v", declared, m.Running)
		}
	case StateNotMeasured:
		if m.Reason == "" {
			return fmt.Errorf("a not_measured artifact measurement must say why")
		}
		if declared != "" && len(m.Running) > 0 && m.Unresolved == 0 && !contains(m.Running, declared) {
			return &MismatchError{Declared: declared, Running: m.Running}
		}
		if declared != "" && contains(m.Running, declared) {
			return fmt.Errorf("artifact measurement says not_measured but the declared digest is among the running digests")
		}
	default:
		if m.State == StateMismatch {
			return &MismatchError{Declared: declared, Running: m.Running}
		}
		return fmt.Errorf("artifact measurement state %q is not matched or not_measured", m.State)
	}
	return nil
}

// Digest is sha256 (hex) over the canonical JSON of m.
func (m Measurement) Digest() string {
	c := m
	if c.Running == nil {
		c.Running = []string{}
	}
	if c.CommitmentFound == nil {
		c.CommitmentFound = []string{}
	}
	if c.CommitmentNotFound == nil {
		c.CommitmentNotFound = []string{}
	}
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// BundleHash is the run's evidence_bundle_hash. With a measurement it commits to BOTH the scenario
// evidence (scenarioRoot — the pre-existing hash over the per-scenario evidence hashes) and the
// measurement; without one (an executor that does not measure, or a build/rehearsal run) it is
// scenarioRoot unchanged, byte for byte what it always was.
func BundleHash(scenarioRoot string, m *Measurement) string {
	if m == nil {
		return scenarioRoot
	}
	sum := sha256.Sum256([]byte(bundleDomain + "\n" + scenarioRoot + "\n" + m.Digest()))
	return hex.EncodeToString(sum[:])
}
