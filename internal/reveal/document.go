// Package reveal is the READING side of a reveal document: the document a `final` run's commitment
// carries once its verdict has landed. The OneDroid Argus control plane assembles it from what it SEALED (the
// frozen scenario set, a Merkle root and inclusion proofs over it) and DECIDED (the per-scenario verdicts the
// runner declared, the run's own outcome, digests and anchor receipts) — never an observed value, which
// stays in the runner's own environment.
//
// This package holds the document's type, `argus replay` (re-run one revealed scenario in your own
// environment) and `argus anchor verify` (check the document and its anchors independently). It does not
// assemble a reveal, write a ledger or a chain, or read the control plane's database: a third party can
// check a reveal with nothing but the document, a chain client and a Bitcoin header source.
package reveal

import (
	"time"

	"github.com/OneDro1d/argus-runner/internal/certificate"
	"github.com/OneDro1d/argus-runner/internal/federation"
)

// SealedFile is one scenario as it was sealed: its scenario id, registry path and full body (EXPECT
// included — EXPECT lives inside a scenario's own markdown, under its `### Runnable` /
// `### Non-runnable` sections, not as a separate field). This is the "what it sealed" half of the
// document. ID is the catalog's OWN scenario_id column — never parsed back out of Body, which a
// scenario written before an id convention existed, or one whose Metadata id drifted from its
// catalog row, could get wrong.
type SealedFile struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Body string `json:"body"`
}

// ScenarioProof is one sealed file's inclusion proof against HoldoutRoot — the sibling path
// VerifyInclusion needs to recompute the root from (path, body) alone, with no access to the rest of
// the set.
type ScenarioProof struct {
	Path  string      `json:"path"`
	Proof []ProofStep `json:"proof"`
}

// AnchorEntry is one anchor row, carried through unchanged: which chain, which version (1 = the commit,
// 2 = the certification set, 3 = the verdict bound to it), where it was written, and — for a hash-only chain —
// its OTS calendar receipt. Signer is the declared signer `argus anchor verify` reads the record back
// under; a receipt attested under any OTHER signer is refused by chainread.ReadAnchor itself, not by
// this document.
type AnchorEntry struct {
	Version int    `json:"version"`
	Chain   string `json:"chain"`
	// ChainCorrelationID is the chain's OWN per-write correlation id (not the commitment id) — together
	// with Block, the pair `argus anchor verify` re-dials the chain with to read this exact version back.
	ChainCorrelationID string     `json:"chain_correlation_id,omitempty"`
	Block              *int64     `json:"block,omitempty"`
	TxHash             string     `json:"tx_hash,omitempty"`
	BlockTime          *time.Time `json:"block_time,omitempty"`
	Receipt            []byte     `json:"receipt,omitempty"`
	Signer             string     `json:"signer,omitempty"`
}

// TriageEntry is one run of a retry chain — the retry links walked back to the first attempt. Never a
// scenario body or a log line: outcome + tallies is the same MAY-cross shape federation.ResultsPush
// already uses.
type TriageEntry struct {
	RunID   string             `json:"run_id"`
	Outcome string             `json:"outcome"`
	Tallies federation.Tallies `json:"tallies"`
}

// CurrentReceipt is the CURRENT receipt of one anchors[] entry whose anchor row now holds a receipt
// different from the frozen one in the stored document: a hash-only (bitcoin-ots) anchor starts with a
// pending receipt and a later sweep upgrades it, but the stored document is frozen. Version, Chain and
// ChainCorrelationID identify the anchors[] entry it belongs to. It is served BESIDE the frozen
// anchors[], never instead of it, is never part of the hashed bytes and is never persisted; a verifier
// checks it exactly like any receipt (it must commit to the anchor's declared payload and attest to the
// block header).
type CurrentReceipt struct {
	Version            int    `json:"version"`
	Chain              string `json:"chain"`
	ChainCorrelationID string `json:"chain_correlation_id,omitempty"`
	Receipt            []byte `json:"receipt"`
}

// Document is the reveal, as the control plane assembled it once and persisted. What is frozen is the
// persisted bytes — every field except the ones attached to the returned copy on each call (RevealedAt,
// RevealDocumentSHA256, RevealAnchors, RevealAnchorNote, CurrentReceipts) — and they come back unchanged on
// every later request for the same run, anchors[] and the receipts inside it included. What is current is
// attached beside them: the reveal anchors' rows and, for an anchors[] entry whose receipt was upgraded
// after the assembly, CurrentReceipts.
//
// ⛔ NEVER an observed value. Every field here traces to something the control plane itself sealed
// (the set, its root and proofs) or was TOLD by a terminal results push (the verdict, the digests,
// the evidence hashes — never the evidence those hashes cover).
type Document struct {
	RunID        string `json:"run_id"`
	InstanceID   string `json:"instance_id"`
	CommitmentID string `json:"commitment_id"`

	// What it sealed.
	HoldoutRoot  string          `json:"holdout_root"`
	SetHash      string          `json:"set_hash"`
	CriteriaHash string          `json:"criteria_hash"`
	SealedFiles  []SealedFile    `json:"sealed_files"`
	Proofs       []ScenarioProof `json:"proofs"`

	// What it decided.
	Verdicts           []federation.ScenarioResult `json:"verdicts"`
	Outcome            string                      `json:"outcome"`
	ArtifactDigest     string                      `json:"artifact_digest"`
	EvidenceBundleHash string                      `json:"evidence_bundle_hash"`

	// Set (all three together) when this run's verdict was anchored as a certificate.VerdictTallied, so
	// VerifyAnchors can rebuild those exact bytes. Absent on a document for a run anchored before that, and
	// on a persisted document written before this field existed: both read as the legacy
	// certificate.VerdictV2 shape, which is what those anchors hold.
	VerdictPayloadFormat string                      `json:"verdict_payload_format,omitempty"`
	VerdictTallies       *certificate.VerdictTallies `json:"verdict_tallies,omitempty"`
	FinishedAt           string                      `json:"finished_at,omitempty"`

	// Anchored is false — and Anchors empty — whenever no verdict-version anchor row exists yet for
	// this run: a final run's document is reported UNANCHORED in that case, never assumed anchored.
	Anchored bool          `json:"anchored"`
	Anchors  []AnchorEntry `json:"anchors,omitempty"`

	// ReplayCommands is one `argus replay` invocation per scenario in Verdicts, in the same order.
	ReplayCommands []string `json:"replay_commands"`

	// Attempts/FirstPassRate/TriageHistory come from the run ledger's retry chain — no new storage.
	Attempts      int           `json:"attempts"`
	FirstPassRate float64       `json:"first_pass_rate"`
	TriageHistory []TriageEntry `json:"triage_history"`

	// Overlap is left present and null here so a client parsing this document today does not need to
	// change shape the day it starts being populated.
	Overlap *int `json:"overlap"`

	// Once this run's reveal is first assembled, the fact that it happened is separately anchored on chain
	// (certificate.RevealV4) as the NEXT version after the CERTIFYING COMMITMENT'S CURRENT HEAD at that
	// moment — NOT a fixed offset from this run's own verdict anchor (a later scheduled run's verdict can
	// advance the same commitment's shared head first, so this can land well past this run's own verdict
	// version) — proof the holdout was broken, independent of this document. Deliberately NOT folded into
	// Anchors: VerifyAnchors' Ethereum pass treats every entry that is not version 1 or 2 as a
	// verdict-payload comparison, which a reveal entry would fail. Empty/zero exactly when this run's
	// reveal has not (yet) been anchored on any chain — persisted docs written before this field existed
	// are exactly that case, not an error.
	RevealedAt           *time.Time    `json:"revealed_at,omitempty"`
	RevealDocumentSHA256 string        `json:"reveal_document_sha256,omitempty"`
	RevealAnchors        []AnchorEntry `json:"reveal_anchors,omitempty"`
	// RevealAnchorNote is set only when reveal_anchors are present but RevealedAt could not be
	// established — a reveal persisted before its anchored revealed_at was stored, on chains that hold no
	// readable payload (hash-only) or whose payload does not match this document. RevealedAt is then
	// OMITTED, never guessed, and `argus anchor verify` reports the reveal anchor as unverifiable. Never
	// part of the anchored document bytes (attached to the returned copy only).
	RevealAnchorNote string `json:"reveal_anchor_note,omitempty"`

	// CurrentReceipts is the current receipt of every anchors[] entry whose anchor row has been upgraded
	// since the document was assembled (an OTS receipt that was pending then). Attached to the returned
	// copy only, AFTER every hash of the unmodified document; never persisted, never covered by
	// reveal_document_sha256. anchors[] itself is never rewritten. Omitted when no receipt differs.
	CurrentReceipts []CurrentReceipt `json:"current_receipts,omitempty"`
}
