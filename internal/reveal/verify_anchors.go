package reveal

// verify_anchors.go — `argus anchor verify`'s anchor read-backs, judged the way `argus certificate
// verify` judges a certificate (certificate.Verify / certificate.Overall): every anchor is VERIFIED,
// MISMATCH (a real contradiction) or UNREACHABLE / PENDING (could not be checked — not a failure by
// itself). Holdout-root, inclusion-proof and bundle-hash checks stay local computations, so their
// failures are MISMATCH. See Overall for the pass rule.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/OneDro1d/argus-runner/internal/certificate"
	"github.com/OneDro1d/argus-runner/internal/chainread"
	"github.com/OneDro1d/argus-runner/internal/chainread/ots"
)

// readFailure is the detail for an Ethereum-style anchor that could not be read back. The wording is
// certificate's own (certificate.ExplainRead). A reveal document does not record an anchor's chain id
// (that would change reveal_document_sha256 for documents already anchored), so the "chain id not
// recorded" wording applies: it names the anchor's chain and the chain id --rpc serves.
func readFailure(a AnchorEntry, err error, rpcID *big.Int) string {
	if msg, ok := certificate.ExplainRead(a.Chain, 0, err, rpcID); ok {
		return msg
	}
	return fmt.Sprintf("anchor is on chain %q but --rpc serves chain id %s (a reveal document does not record the anchor's chain id) — if %s is not %q's chain id, --rpc is the wrong endpoint for this anchor; use an --rpc endpoint for %q; the node said: %v",
		a.Chain, rpcID, rpcID, a.Chain, a.Chain, err)
}

// readEth reads a's Ethereum-style anchor back through svc under its own declared signer. On failure
// it returns the result to report: a signer the chain says is not allowed is a MISMATCH (the data is
// there, signed by someone else); any other failed read is UNREACHABLE, exactly as certificate.Verify.
func readEth(ctx context.Context, name string, a AnchorEntry, svc chainread.ChainService) (chainread.AnchorRecord, *CheckResult) {
	if svc == nil {
		return chainread.AnchorRecord{}, &CheckResult{Name: name, Status: StatusUnreachable, Detail: "no independent chain client configured (--rpc)"}
	}
	var block uint64
	if a.Block != nil {
		block = uint64(*a.Block)
	}
	rec, err := chainread.ReadAnchor(svc, chainread.AnchorRef{CorrelationID: a.ChainCorrelationID, Block: block}, chainread.WithAllowedSigners(a.Signer))
	if err != nil {
		if errors.Is(err, chainread.ErrSignerNotAllowed) {
			return rec, &CheckResult{Name: name, Status: StatusMismatch, Detail: err.Error()}
		}
		return rec, &CheckResult{Name: name, Status: StatusUnreachable, Detail: readFailure(a, err, certificate.RPCChainID(ctx, svc))}
	}
	return rec, nil
}

// checkOTS verifies a hash-only anchor's receipt against payload through hdr.
func checkOTS(ctx context.Context, name string, a AnchorEntry, payload []byte, hdr ots.HeaderSource, okDetail string) CheckResult {
	if hdr == nil {
		return CheckResult{Name: name, Status: StatusUnreachable, Detail: "no independent Bitcoin header source configured (--btc-headers)"}
	}
	p := ots.New(hdr, a.Chain)
	proof, err := p.Verify(ctx, chainread.Ref{Chain: a.Chain, Receipt: a.Receipt}, payload)
	if err != nil {
		status := StatusUnreachable
		if errors.Is(err, chainread.ErrMismatch) {
			// chainread wraps ErrMismatch for every way the receipt itself contradicts the evidence: a
			// digest other than the declared payload's, a fold that is not the block's merkle root,
			// and a receipt that does not parse. A header-source failure is NOT wrapped, so it stays
			// UNREACHABLE ("could not check").
			status = StatusMismatch
		}
		return CheckResult{Name: name, Status: status, Detail: err.Error()}
	}
	if proof.Pending {
		return CheckResult{Name: name, Status: StatusPending, Detail: "receipt is still pending a Bitcoin block-header attestation"}
	}
	return CheckResult{Name: name, Status: StatusVerified, Detail: okDetail}
}

// VerifyAnchors reads each of doc's Ethereum-style anchor entries back through svc (an INDEPENDENT
// chain client the caller dialed — never the control plane), under that entry's OWN declared signer
// (WithAllowedSigners), and verifies each hash-only (OTS) entry's receipt through hdr (an
// INDEPENDENT Bitcoin header source the caller supplied). svc/hdr may be nil: an entry that needs
// one is then UNREACHABLE by name, never a panic and never a failure.
//
// The verdict version's payload ({run_id, artifact_digest, evidence_bundle_hash, verdict}, version 3
// or later) and the certification set's version-2 payload ({holdout_root, criteria_hash, set_hash,
// scenario_count}) are both fully reconstructable from doc's own declared fields, so each entry's
// read-back payload is compared byte-exact against the matching one. Version 1's payload
// ({commit, image_digests, runner_digest}) is NOT carried on the document — a v1 Ethereum-style
// entry is checked by successful, signer-verified read-back alone; a v1 hash-only entry is checked
// against whatever v1 Ethereum-style payload this same call already read back, and reported NOT BOUND
// (neither pass nor failure) when no v1 sibling exists to supply it.
//
// Results for version >= 3 (this run's verdict) carry Required=true; see Overall.
func VerifyAnchors(ctx context.Context, doc *Document, svc chainread.ChainService, hdr ots.HeaderSource) []CheckResult {
	var results []CheckResult
	verdictPayload, _ := jsonMarshalVerdictV2(doc)
	setPayload, _ := jsonMarshalSetV2(doc)

	var v1EthPayload []byte
	var haveV1Eth bool
	var otsEntries []AnchorEntry
	add := func(a AnchorEntry, r CheckResult) {
		r.Required = a.Version >= 3
		results = append(results, r)
	}

	// pass 1: every Ethereum-style entry (no Receipt) — these supply the payloads pass 2 needs.
	for _, a := range doc.Anchors {
		name := fmt.Sprintf("anchor v%d %s", a.Version, a.Chain)
		if len(a.Receipt) > 0 {
			otsEntries = append(otsEntries, a)
			continue
		}
		rec, fail := readEth(ctx, name, a, svc)
		if fail != nil {
			add(a, *fail)
			continue
		}
		switch a.Version {
		case 1:
			v1EthPayload, haveV1Eth = rec.Payload, true
			add(a, CheckResult{Name: name, Status: StatusVerified, Detail: "read back under declared signer " + a.Signer})
		case 2:
			match := bytes.Equal(rec.Payload, setPayload)
			add(a, CheckResult{Name: name, Status: local(match),
				Detail: fmt.Sprintf("read back under declared signer %s; payload matches the declared certification set: %v", a.Signer, match)})
		default:
			match := bytes.Equal(rec.Payload, verdictPayload)
			add(a, CheckResult{Name: name, Status: local(match),
				Detail: fmt.Sprintf("read back under declared signer %s; payload matches the declared verdict: %v", a.Signer, match)})
		}
	}

	// pass 2: every hash-only (OTS) entry, now that pass 1 may have supplied a v1 payload.
	for _, a := range otsEntries {
		name := fmt.Sprintf("anchor v%d %s", a.Version, a.Chain)
		var payload []byte
		switch {
		case a.Version >= 3:
			payload = verdictPayload
		case a.Version == 2:
			payload = setPayload
		case a.Version == 1 && haveV1Eth:
			payload = v1EthPayload
		default:
			add(a, CheckResult{Name: name, Status: StatusNotBound, Detail: "SKIP — no independently known version-1 payload to check this receipt against"})
			continue
		}
		// the document served with a receipt the ledger has upgraded since the reveal was
		// assembled carries it in current_receipts, beside the frozen one in anchors[]. Prefer it, and say so on
		// the line — but it is checked exactly like the frozen one: it must commit to the declared payload and
		// attest to the block header, so a wrong or forged one is a MISMATCH, never a pass.
		if cur, ok := currentReceiptFor(doc, a); ok {
			checked := a
			checked.Receipt = cur
			r := checkOTS(ctx, name, checked, payload, hdr, "receipt commits to the declared payload's digest")
			r.Detail = currentReceiptNote + r.Detail
			add(a, r)
			continue
		}
		add(a, checkOTS(ctx, name, a, payload, hdr, "receipt commits to the declared payload's digest"))
	}
	return results
}

// currentReceiptNote prefixes the detail of every anchor line judged on a served current receipt.
const currentReceiptNote = "current receipt used (served beside the frozen one in anchors[], which is older): "

// currentReceiptFor returns the current receipt doc serves for anchor a — matched on version, chain and
// chain correlation id — when it differs from a's frozen receipt. Matching decides only WHICH receipt is
// checked; whether it is any good is checkOTS's call, like any receipt.
func currentReceiptFor(doc *Document, a AnchorEntry) ([]byte, bool) {
	for _, c := range doc.CurrentReceipts {
		if c.Version == a.Version && c.Chain == a.Chain && c.ChainCorrelationID == a.ChainCorrelationID &&
			len(c.Receipt) > 0 && !bytes.Equal(c.Receipt, a.Receipt) {
			return c.Receipt, true
		}
	}
	return nil, false
}

// VerifyRevealAnchors checks doc's RevealAnchors — T6.3 option C's proof the holdout was broken —
// independently: reconstructs the exact certificate.RevealV4 payload from doc's own declared fields
// (unlike certificate.Verify's version-4 check, a reveal.Document carries CommitmentID, so this
// reconstruction IS byte-exact) and compares it to what each chain (or OTS receipt) actually committed
// to. A tampered RevealDocumentSHA256 — or a tampered RevealedAt/RunID/CommitmentID — changes the
// reconstructed payload and so is a MISMATCH on every entry that can be read. No entries at all is
// reported as no results, not a failure.
func VerifyRevealAnchors(ctx context.Context, doc *Document, svc chainread.ChainService, hdr ots.HeaderSource) []CheckResult {
	var results []CheckResult
	if len(doc.RevealAnchors) == 0 {
		return results
	}
	if doc.RevealedAt == nil || doc.RevealDocumentSHA256 == "" {
		return []CheckResult{{Name: "reveal anchor", Status: StatusMismatch,
			Detail: "reveal_anchors present but revealed_at/reveal_document_sha256 is missing — cannot reconstruct the anchored payload"}}
	}
	want, _ := json.Marshal(certificate.RevealV4{
		Kind: "reveal", RunID: doc.RunID, CommitmentID: doc.CommitmentID,
		RevealedAt: *doc.RevealedAt, DocumentSHA256: doc.RevealDocumentSHA256, ScenarioCount: len(doc.SealedFiles),
	})
	for _, a := range doc.RevealAnchors {
		name := fmt.Sprintf("reveal anchor v%d %s", a.Version, a.Chain)
		if len(a.Receipt) > 0 {
			results = append(results, checkOTS(ctx, name, a, want, hdr, "receipt commits to the declared reveal payload"))
			continue
		}
		rec, fail := readEth(ctx, name, a, svc)
		if fail != nil {
			results = append(results, *fail)
			continue
		}
		match := bytes.Equal(rec.Payload, want)
		results = append(results, CheckResult{Name: name, Status: local(match),
			Detail: fmt.Sprintf("read back under declared signer %s; payload matches the declared reveal: %v", a.Signer, match)})
	}
	return results
}

// VerifyReveal runs every offline/independent check `argus anchor verify` prints: the holdout root +
// its per-file inclusion proofs, the evidence bundle hash, every verdict/set anchor receipt, and (T6.3
// option C) the reveal anchor, if this run has one. The exit decision is Overall's.
func VerifyReveal(ctx context.Context, doc *Document, svc chainread.ChainService, hdr ots.HeaderSource) []CheckResult {
	var results []CheckResult
	results = append(results, VerifyHoldout(doc)...)
	results = append(results, VerifyEvidenceBundleHash(doc))
	results = append(results, VerifyAnchors(ctx, doc, svc, hdr)...)
	results = append(results, VerifyRevealAnchors(ctx, doc, svc, hdr)...)
	return results
}

// Overall is the exit-worthy verdict over results, the rule certificate.Overall applies: true only
// when no check is a MISMATCH AND at least one anchor of this run's verdict (the required record) is
// VERIFIED. UNREACHABLE and PENDING anchors, and NOT BOUND lines, neither pass nor fail the run — but an
// all-unreachable or all-pending set is NOT a pass: absence of contradiction is not confirmation.
func Overall(results []CheckResult) bool {
	verdictVerified := false
	for _, r := range results {
		if r.Status == StatusMismatch {
			return false
		}
		if r.Required && r.Status == StatusVerified {
			verdictVerified = true
		}
	}
	return verdictVerified
}

// Render is the human summary `argus anchor verify` prints: every result line, then the overall verdict.
func Render(results []CheckResult) []string {
	var lines []string
	for _, r := range results {
		lines = append(lines, r.Line())
	}
	if Overall(results) {
		return append(lines, "OVERALL: verified")
	}
	return append(lines, "OVERALL: NOT verified")
}
