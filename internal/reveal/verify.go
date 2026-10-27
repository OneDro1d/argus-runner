package reveal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/certificate"
	"github.com/OneDro1d/argus-runner/internal/federation"
)

// verify.go — `argus anchor verify`'s pure logic: OFFLINE and INDEPENDENT of the OneDroid Argus
// control plane. Every function here takes an already-built chain client / header source — cmd/argus
// dials --rpc and builds --btc-headers into those before calling in; a test hands in a fake chain and a
// fake header source. Nothing here ever calls back into the control plane.

// Status is one check's outcome. It is certificate.Status, the SAME three-way judgement
// `argus certificate verify` makes: verified, mismatch (a real contradiction) or unreachable / pending
// (could not be checked — never a failure by itself).
type Status = certificate.Status

const (
	StatusVerified    = certificate.StatusVerified
	StatusMismatch    = certificate.StatusMismatch
	StatusUnreachable = certificate.StatusUnreachable
	StatusPending     = certificate.StatusPending
	StatusNotBound    = certificate.StatusNotBound
)

// CheckResult is one check's outcome line.
type CheckResult struct {
	Name   string
	Status Status
	Detail string
	// Required marks an anchor of the record Overall demands at least one verified anchor of: this
	// run's verdict (see Overall).
	Required bool
}

// Line renders one "<STATUS>: <name> — <detail>" line, the shape certificate.CheckResult.Line uses.
func (c CheckResult) Line() string {
	if c.Detail == "" {
		return fmt.Sprintf("%s: %s", strings.ToUpper(string(c.Status)), c.Name)
	}
	return fmt.Sprintf("%s: %s — %s", strings.ToUpper(string(c.Status)), c.Name, c.Detail)
}

// local is the status of a check computed entirely from the document itself (holdout root, inclusion
// proof, bundle hash): it can only verify or contradict, never be "unreachable", so a failure is a
// hard MISMATCH.
func local(ok bool) Status {
	if ok {
		return StatusVerified
	}
	return StatusMismatch
}

// VerifyHoldout recomputes the Merkle root from doc's own SealedFiles and checks it against
// HoldoutRoot, then checks every sealed file's OWN inclusion proof against that declared root — a
// tampered file's body changes its leaf hash, which fails BOTH the root recompute and its own proof.
func VerifyHoldout(doc *Document) []CheckResult {
	var results []CheckResult
	leaves := make([]MerkleLeaf, len(doc.SealedFiles))
	for i, f := range doc.SealedFiles {
		leaves[i] = MerkleLeaf{Path: f.Path, Body: f.Body}
	}
	root, err := MerkleRoot(leaves)
	if err != nil {
		results = append(results, CheckResult{Name: "holdout root", Status: StatusMismatch, Detail: err.Error()})
		return results
	}
	results = append(results, CheckResult{
		Name: "holdout root", Status: local(root == doc.HoldoutRoot),
		Detail: fmt.Sprintf("recomputed=%s declared=%s", root, doc.HoldoutRoot),
	})

	proofByPath := make(map[string][]ProofStep, len(doc.Proofs))
	for _, p := range doc.Proofs {
		proofByPath[p.Path] = p.Proof
	}
	for _, f := range doc.SealedFiles {
		ok := VerifyInclusion(doc.HoldoutRoot, f.Path, f.Body, proofByPath[f.Path])
		results = append(results, CheckResult{Name: "inclusion proof " + f.Path, Status: local(ok)})
	}
	return results
}

// VerifyEvidenceBundleHash recomputes doc's evidence_bundle_hash from its own per-scenario
// EvidenceHash values, ascending scenario-id order — the SAME algorithm the runner used to produce
// it (internal/runner/mapper.go's evidenceBundleHash), so a tampered EvidenceHash or a tampered
// declared bundle hash both fail this one check.
func VerifyEvidenceBundleHash(doc *Document) CheckResult {
	got := evidenceBundleHash(doc.Verdicts)
	return CheckResult{
		Name: "evidence bundle hash", Status: local(got == doc.EvidenceBundleHash),
		Detail: fmt.Sprintf("recomputed=%s declared=%s", got, doc.EvidenceBundleHash),
	}
}

func evidenceBundleHash(scenarios []federation.ScenarioResult) string {
	ids := make([]string, 0, len(scenarios))
	byID := make(map[string]string, len(scenarios))
	for _, sc := range scenarios {
		ids = append(ids, sc.ID)
		byID[sc.ID] = sc.EvidenceHash
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(byID[id]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// jsonMarshalSetV2 reconstructs the certification set's version-2 payload from the document's own
// declared root, criteria hash, set hash and file count — the same bytes author_seal_set anchored.
func jsonMarshalSetV2(doc *Document) ([]byte, error) {
	return json.Marshal(certificate.SetV2{HoldoutRoot: doc.HoldoutRoot, CriteriaHash: doc.CriteriaHash, SetHash: doc.SetHash, ScenarioCount: len(doc.SealedFiles)})
}

func jsonMarshalVerdictV2(doc *Document) ([]byte, error) {
	if doc.VerdictPayloadFormat == certificate.VerdictPayloadTallied && doc.VerdictTallies != nil {
		// this run's verdict anchored the tallied shape; rebuild those exact bytes.
		return json.Marshal(certificate.VerdictTallied{
			RunID: doc.RunID, ArtifactDigest: doc.ArtifactDigest, EvidenceBundleHash: doc.EvidenceBundleHash, Verdict: doc.Outcome,
			Tallies: *doc.VerdictTallies, FinishedAt: doc.FinishedAt,
		})
	}
	return json.Marshal(certificate.VerdictV2{RunID: doc.RunID, ArtifactDigest: doc.ArtifactDigest, EvidenceBundleHash: doc.EvidenceBundleHash, Verdict: doc.Outcome})
}
