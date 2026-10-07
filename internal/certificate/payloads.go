package certificate

import "time"

// payloads.go — the JSON shapes a certificate's anchors carry on chain. Verify rebuilds each one from the
// certificate's declared fields with json.Marshal and compares the bytes with what the chain returns, so
// field names and field ORDER here are part of the contract: they are the exact bytes OneDroid Argus
// anchored. This package only reads and compares them; nothing here writes to a chain.

// SetV2 is the certification set's own commitment, anchored once the set is sealed. Its holdout root is
// what a `final` run's assignment carries and what a third party later checks a reveal against.
type SetV2 struct {
	HoldoutRoot   string `json:"holdout_root"`
	CriteriaHash  string `json:"criteria_hash"`
	SetHash       string `json:"set_hash"`
	ScenarioCount int    `json:"scenario_count"`
}

// VerdictV2 is the verdict payload of certificate format v1: RunID, the artifact digest it certified, its
// evidence-bundle hash and the verdict itself.
type VerdictV2 struct {
	RunID              string `json:"run_id"`
	ArtifactDigest     string `json:"artifact_digest"`
	EvidenceBundleHash string `json:"evidence_bundle_hash"`
	Verdict            string `json:"verdict"`
}

// VerdictTallies is the outcome count a tallied verdict anchors. Four buckets, all always present in the
// JSON (no omitempty): a zero is a fact the chain holds, not an absence.
type VerdictTallies struct {
	Passed   int `json:"passed"`
	Failed   int `json:"failed"`
	Errored  int `json:"errored"`
	Degraded int `json:"degraded"`
}

// VerdictTallied is the verdict payload from certificate format v2 on: VerdictV2's four fields PLUS the
// run's tallies and its finished_at, so a certificate cannot restate either without the chain
// contradicting it. Field order is the marshalled order. FinishedAt is RFC3339 UTC to the second, "" when
// the run has no finished_at.
type VerdictTallied struct {
	RunID              string         `json:"run_id"`
	ArtifactDigest     string         `json:"artifact_digest"`
	EvidenceBundleHash string         `json:"evidence_bundle_hash"`
	Verdict            string         `json:"verdict"`
	Tallies            VerdictTallies `json:"tallies"`
	FinishedAt         string         `json:"finished_at"`
}

// FinishedAtString renders a run's finished_at the one way every writer and reader of a VerdictTallied
// must: UTC, RFC3339, second precision; "" for nil.
func FinishedAtString(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// RevealV4 is the reveal anchor's payload. It proves the holdout was broken without carrying what the
// reveal disclosed: no scenario text, title, body, expectation or cleanup, and no workspace id.
// DocumentSHA256 is the sha256 of the exact bytes of the reveal document.
type RevealV4 struct {
	Kind           string    `json:"kind"` // always "reveal"
	RunID          string    `json:"run_id"`
	CommitmentID   string    `json:"commitment_id"`
	RevealedAt     time.Time `json:"revealed_at"`
	DocumentSHA256 string    `json:"document_sha256"`
	ScenarioCount  int       `json:"scenario_count,omitempty"`
}
