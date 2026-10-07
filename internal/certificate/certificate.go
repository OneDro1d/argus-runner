// Package certificate is T6.3 (MVP2-SPRINT.md §E6): the certificate artefact — PROOF WITHOUT THE
// TESTS. It answers one question a third party can check with no Argus credential and no access to
// the scenario catalog: "did this verdict get anchored against a set that was sealed before the run
// happened?" It never answers "what did the set contain" or "what did the run observe" — that is the
// reveal path (internal/reveal, AC-7), a DIFFERENT and strictly larger disclosure this package never
// calls into.
//
// Certificate is a TYPED ALLOWLIST PROJECTION, the same discipline internal/control/publicview.go
// uses for a struct whose fields are exactly what may leave, filled field by field in
// internal/control/certificate.go — never by marshalling a store row or a reveal.Document. See that
// file's fillCertificate for the assembly and this package's TestCertificate_PinnedFields (mirrored
// from publicview_test.go's) for what the struct excludes in practice: no scenario id/path/body, no
// EXPECT, no observed value, no commitment_id, no workspace/org/instance id or name, no host, no
// dashboard link, no v1 (commit-level) anchor.
package certificate

// Format is the certificate's own format tag — a version marker a verifier checks before trusting the
// shape of anything else in the document. It also selects which verdict payload verify rebuilds:
//
//   - FormatV1 ("argus-certificate/v1"): the verdict anchor holds VerdictV2, which carries no
//     tallies and no finished_at. Those two fields are NOT anchored on a v1 certificate (see
//     Unanchored) and verify can only check them for internal consistency (checkTallies).
//   - FormatV2 ("argus-certificate/v2"): the verdict anchor holds VerdictTallied,
//     so tallies and finished_at are anchored and verify compares them byte for byte.
//   - Format ("argus-certificate/v3"): the same verdict payload as v2, plus a REQUIRED artifact_measurement
//     block saying whether the build the executor found running was the declared artifact digest. The
//     block is covered by the anchored evidence_bundle_hash (internal/artifactmeasure.BundleHash), so no
//     new chain payload shape exists; verify recomputes the hash from the block. A v3 certificate with
//     the block stripped is refused. An older verifier does not know v3 and refuses it by name, rather
//     than passing a document whose new binding it cannot check.
//
// Format is what NEW certificates declare. FormatV1 and FormatV2 are still accepted on verify: every
// certificate issued before v3 keeps verifying, as long as it is honest.
const (
	Format   = "argus-certificate/v3"
	FormatV2 = "argus-certificate/v2"
	FormatV1 = "argus-certificate/v1"
)

// Tallied reports whether format's verdict anchor is VerdictTallied (v2 and v3).
func Tallied(format string) bool { return format == Format || format == FormatV2 }

// ArtifactMeasurement says whether the build that was running when the run began was the declared
// artifact digest, and how far that statement is bound.
//
// Bound=true: the executor measured (or tried to and recorded why not) and the record is inside the
// anchored evidence_bundle_hash — verify rebuilds that hash from these fields and ScenarioEvidenceRoot
// and refuses any edit. Bound=false: the executor reported no measurement at all (released before measurement); the
// state is "not_measured" and NOTHING binds what was running.
//
// The running digests are what the EXECUTOR reported reading from the SUT's cluster or docker daemon.
// They are bound to the verdict, but the chain and the control plane cannot confirm them independently.
type ArtifactMeasurement struct {
	Bound  bool   `json:"bound"`
	State  string `json:"state"` // "matched" | "not_measured"
	Reason string `json:"reason,omitempty"`
	Source string `json:"source,omitempty"` // "k8s" | "compose"
	// Running are the distinct image digests found running; the declared digest is
	// verdict_record.artifact_digest.
	Running    []string `json:"running,omitempty"`
	Unresolved int      `json:"unresolved,omitempty"`
	// CommitmentFound / CommitmentNotFound: which of the sealed commitment's image_digests were seen
	// running. Recorded, not enforced.
	CommitmentFound    []string `json:"commitment_images_found,omitempty"`
	CommitmentNotFound []string `json:"commitment_images_not_found,omitempty"`
	// ScenarioEvidenceRoot is the scenario-only hash the bundle hash was built from (Bound only).
	ScenarioEvidenceRoot string `json:"scenario_evidence_root,omitempty"`
}

// Tallies is the run's scenario outcome counts — the buckets federation.Tallies carries, without Total
// (derivable, and not worth a field on a document meant to be re-typed by hand). Degraded (AC-11) is a
// bucket of its own: internal/argus.summarize counts a degraded scenario in neither Passed, Failed nor
// Errored. A v1 certificate has no such field (checkTallies refuses a v1 document that declares one);
// omitempty keeps it off the wire when it is zero.
type Tallies struct {
	Passed   int `json:"passed"`
	Failed   int `json:"failed"`
	Errored  int `json:"errored"`
	Degraded int `json:"degraded,omitempty"`
}

// Set is the certification set's own version-2 ledger commitment (SetV2), carried onto the
// certificate unchanged: the set's Merkle root, the acceptance-criteria hash it was authored against,
// its own content hash, and how many scenarios it held. Never the scenarios themselves.
type Set struct {
	HoldoutRoot   string `json:"holdout_root"`
	CriteriaHash  string `json:"criteria_hash"`
	SetHash       string `json:"set_hash"`
	ScenarioCount int    `json:"scenario_count"`
}

// VerdictRecord is the two content-addressed digests the run's version-3 ledger verdict
// (VerdictV2) carries besides RunID/Verdict, which the certificate holds at its own top level
// instead (RunID/Verdict, not VerdictRecord.RunID/VerdictRecord.Verdict — one place each).
type VerdictRecord struct {
	ArtifactDigest     string `json:"artifact_digest"`
	EvidenceBundleHash string `json:"evidence_bundle_hash"`
}

// Anchor is one ledger_anchors row the certificate carries — version 2 (the certification set's own
// anchor, always), this run's own verdict anchor (T6.3 option C: NOT always version 3 — AC-12 lets a
// later scheduled run's verdict land wherever the commitment's shared head then was, per
// ) or this run's reveal anchor (present only once this run's reveal has been anchored —
// "the seal is broken"; NOT always version 4, for the same reason: it lands at whatever the head was
// AT REVEAL TIME, which may be well past this run's own verdict). NEVER version 1 (the commit-level
// anchor), which a certificate never includes. Which role a given Anchor plays is NEVER determined by
// its absolute Version number — see internal/certificate/verify.go's roleOfAnchors, which classifies by
// relative position instead. Receipt is only ever populated for a hash-only chain (an Ethereum-style
// chain instead carries Block/TxHash and no Receipt) — base64 on the wire via Go's default []byte JSON
// encoding.
type Anchor struct {
	Version            int    `json:"version"` // >= 2; never a fixed 2/3/4 — see the type comment
	Chain              string `json:"chain"`
	ChainCorrelationID string `json:"chain_correlation_id,omitempty"`
	Block              *int64 `json:"block,omitempty"`
	TxHash             string `json:"tx_hash,omitempty"`
	Signer             string `json:"signer,omitempty"`
	Receipt            []byte `json:"receipt,omitempty"` // base64; hash-only chains only

	// ChainID (format v2 on) is the EVM chain id the anchor's chain runs on, from the control plane's
	// own chains.json (network_id). It is what lets verify say "this anchor is on chain 2026 but --rpc
	// serves 84532". Advisory: it is not in the anchored payload, and a wrong value can
	// only change the wording of a failed read, never make one succeed. Absent on a v1 certificate and
	// on a hash-only anchor.
	ChainID int64 `json:"chain_id,omitempty"`
}

// Anchoring says how far the anchoring of THIS run's certification record had got when the certificate
// was issued. A certificate is issued as soon as ANY chain holds the verdict, which
// for a run fetched right after it finished can be before the public chain does; without this block a
// reader cannot tell "anchored on one chain" from "anchored everywhere it will be". Advisory and NOT
// anchored: it describes the moment of issue, so re-fetch rather than trust an old copy's status.
type Anchoring struct {
	// Status is AnchoringComplete when every certification chain of the commitment's snapshot holds this
	// run's verdict; AnchoringInProgress when one does not yet but the control plane is still going to
	// write it (re-fetch); AnchoringIncomplete when one does not and will not be written any more.
	Status string `json:"status"`
	// ChainsAwaitingVerdict names each certification chain that has no verdict anchor yet. Re-fetch.
	ChainsAwaitingVerdict []string `json:"chains_awaiting_verdict,omitempty"`
	// OTSAwaitingBitcoin names each hash-only chain whose verdict IS anchored but whose receipt no
	// Bitcoin block has attested yet. Expected, takes hours, and does not make Status in-progress.
	OTSAwaitingBitcoin []string `json:"ots_awaiting_bitcoin,omitempty"`
	// Note is one plain sentence saying what the above means for the reader.
	Note string `json:"note"`
}

const (
	AnchoringComplete   = "complete"
	AnchoringInProgress = "in_progress"
	// AnchoringIncomplete: at least one certification chain holds no verdict for this run
	// and the control plane will NOT write it any more (the retry window passed, or the retry was refused
	// for a stated reason). Re-fetching will not change it. `argus certificate verify` (v0.3.46 and later)
	// never reads Status, so a client that predates this value ignores it.
	AnchoringIncomplete = "incomplete"
)

// Certificate is the ENTIRE typed projection — the struct json.Marshal walks for author_get_certificate
// and for `argus certificate get`'s --out file. Nothing reaches the wire that is not a field here: no
// scenario ids/paths/bodies/titles, no EXPECT, no observed values, no commitment_id, no
// workspace/org/instance id or name, no host, no dashboard link, no v1 anchor. See
// TestCertificate_PinnedFields for the exact recursive key set this must never grow without a matching
// update there.
type Certificate struct {
	Format        string        `json:"format"`
	RunID         string        `json:"run_id"`
	FinishedAt    string        `json:"finished_at"` // RFC3339 UTC
	Verdict       string        `json:"verdict"`
	Tallies       Tallies       `json:"tallies"`
	Set           Set           `json:"set"`
	VerdictRecord VerdictRecord `json:"verdict_record"`
	Anchors       []Anchor      `json:"anchors"`
	IssuedAt      string        `json:"issued_at"` // RFC3339 UTC; when THIS certificate was produced

	// Anchoring is the state of the run's anchoring at issue time. Always present on a
	// certificate the control plane issues, nil only on one issued before it existed.
	Anchoring *Anchoring `json:"anchoring,omitempty"`

	// ArtifactMeasurement is required on a v3 certificate and absent on older ones.
	ArtifactMeasurement *ArtifactMeasurement `json:"artifact_measurement,omitempty"`

	// T6.3 option C / once this run's reveal has been anchored (a roleReveal Anchor
	// above exists — see verify.go's roleOfAnchors; not necessarily version 4), the certificate states
	// that the seal is broken — WHEN, and the digest the chain committed to. Both empty together for a
	// run whose reveal has not (yet) been anchored — the seal is intact; NEVER populated without a
	// matching reveal entry in Anchors (see internal/control/certificate.go's buildCertificate, which
	// fills them from the SAME chain read-back, never fabricated from a local, unverified value).
	RevealedAt           string `json:"revealed_at,omitempty"` // RFC3339 UTC
	RevealDocumentSHA256 string `json:"reveal_document_sha256,omitempty"`
}
