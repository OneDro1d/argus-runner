package certificate

// verify.go — `argus certificate verify`'s pure logic: OFFLINE and INDEPENDENT of the Argus control
// plane, exactly like internal/reveal/verify.go's VerifyAnchors is for `argus anchor verify` (AC-7).
// It is a PARALLEL implementation, not a call into package reveal: a certificate carries no sealed
// scenario bodies (only Set.ScenarioCount, never a reveal.Document's SealedFiles), so it cannot be fed
// through reveal.VerifyAnchors without first constructing a reveal.Document for the run — exactly the
// "reveal.Document assembly for a run" a certificate must never do. What IS reused is the same
// underlying read-back: chainread.ReadAnchor under the anchor's own declared signer
// (chainread.WithAllowedSigners) for an Ethereum-style anchor, and an ots.Provider.Verify against an
// independent Bitcoin header source for a hash-only one — the identical chain library/ots calls
// verify.go's VerifyAnchors makes, called directly here rather than duplicated business logic wrapped
// around a different document shape.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/chainread"
	"github.com/OneDro1d/argus-runner/internal/chainread/ots"

	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
)

// Status is one anchor's per-check outcome.
type Status string

const (
	StatusVerified    Status = "verified"
	StatusMismatch    Status = "mismatch"
	StatusUnreachable Status = "unreachable"
	StatusPending     Status = "pending"
	// StatusNotBound is a statement that is consistent but does NOT establish what it would need to: the
	// artifact measurement is "not measured". It is never a pass and never a failure.
	StatusNotBound Status = "not bound"
)

// CheckResult is one anchor's verify outcome.
type CheckResult struct {
	Name   string
	Status Status
	Detail string
}

// Line renders one "<STATUS>: <name> — <detail>" line, uppercased status, the same shape
// reveal.CheckResult.Line() uses for `argus anchor verify`.
func (c CheckResult) Line() string {
	if c.Detail == "" {
		return fmt.Sprintf("%s: %s", strings.ToUpper(string(c.Status)), c.Name)
	}
	return fmt.Sprintf("%s: %s — %s", strings.ToUpper(string(c.Status)), c.Name, c.Detail)
}

// AllowList is a chain name -> its configured allowed-signer addresses, the same allowedSigners a
// chains.json entry carries (engine.ChainConfig.AllowedSigners) — parsed independently here so
// `certificate verify` can cross-check a certificate's declared per-anchor Signer against a THIRD
// PARTY'S OWN copy of chains.json, not merely against itself (which chainread.WithAllowedSigners
// already enforces during read-back: a wrong declared signer fails the read outright).
type AllowList map[string][]string

// chainsJSONShape is the {"chains": {"<name>": {"allowedSigners": [...]}}} document chainread.LoadRegistry
// and ledger.Open both read — this package parses only the one field it needs, from a plain
// map[string]any so an unrelated field's shape drifting elsewhere never breaks this parse.
type chainsJSONShape struct {
	Chains map[string]struct {
		AllowedSigners []string `json:"allowedSigners"`
	} `json:"chains"`
}

// LoadAllowList reads path (a chains.json) and returns its per-chain allowedSigners. A chain with no
// allowedSigners entry (or an empty one) is simply absent from the result — Verify then reports every
// anchor on that chain as a signer mismatch, since an empty allow-list allows no one.
func LoadAllowList(path string) (AllowList, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("certificate: read %s: %w", path, err)
	}
	var parsed chainsJSONShape
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, fmt.Errorf("certificate: parse %s: %w", path, err)
	}
	out := make(AllowList, len(parsed.Chains))
	for name, cc := range parsed.Chains {
		if len(cc.AllowedSigners) > 0 {
			out[name] = append([]string(nil), cc.AllowedSigners...)
		}
	}
	return out, nil
}

func (a AllowList) allows(chain, signer string) bool {
	if a == nil {
		return true // no allow-list supplied: fall back to the anchor's own declared signer (as read back)
	}
	for _, s := range a[chain] {
		if strings.EqualFold(s, signer) {
			return true
		}
	}
	return false
}

// setPayload reconstructs the certification set's version-2 payload from the certificate's own
// declared fields — the same bytes author_seal_set anchored (SetV2), byte-identical to what
// internal/reveal's jsonMarshalSetV2 produces from a full Document, but built from Certificate.Set
// alone (no SealedFiles needed: Set.ScenarioCount is already the count).
func setPayload(cert *Certificate) ([]byte, error) {
	return json.Marshal(SetV2{
		HoldoutRoot: cert.Set.HoldoutRoot, CriteriaHash: cert.Set.CriteriaHash,
		SetHash: cert.Set.SetHash, ScenarioCount: cert.Set.ScenarioCount,
	})
}

// verdictPayload reconstructs the run's verdict payload from the certificate's own declared fields, in
// the shape the certificate's declared Format says was anchored: VerdictV2 for FormatV1,
// VerdictTallied (which adds tallies and finished_at) for Format. The shape is chosen by the
// declared format and by nothing else, so relabelling a certificate one way or the other makes the
// rebuilt bytes differ from the chain's and the verdict anchor reports a mismatch.
func verdictPayload(cert *Certificate) ([]byte, error) {
	switch cert.Format {
	case FormatV1:
		return json.Marshal(VerdictV2{
			RunID: cert.RunID, ArtifactDigest: cert.VerdictRecord.ArtifactDigest,
			EvidenceBundleHash: cert.VerdictRecord.EvidenceBundleHash, Verdict: cert.Verdict,
		})
	case Format, FormatV2:
		return json.Marshal(VerdictTallied{
			RunID: cert.RunID, ArtifactDigest: cert.VerdictRecord.ArtifactDigest,
			EvidenceBundleHash: cert.VerdictRecord.EvidenceBundleHash, Verdict: cert.Verdict,
			Tallies: VerdictTallies{
				Passed: cert.Tallies.Passed, Failed: cert.Tallies.Failed, Errored: cert.Tallies.Errored, Degraded: cert.Tallies.Degraded,
			},
			FinishedAt: cert.FinishedAt,
		})
	}
	return nil, fmt.Errorf("certificate: unknown format %q", cert.Format)
}

// checkTallies is the consistency check every certificate gets, in either format.
// It cannot tell a true tally from a plausible one — on a v1 certificate nothing anchors the tallies —
// but it refuses every tally that CONTRADICTS the two things that are anchored or sealed: the verdict
// and the set's scenario_count. Rules, from how a run is counted (internal/argus.summarize):
//
//   - the four buckets are non-negative and add up to no more than set.scenario_count;
//   - passed: every scenario passed — failed, errored and degraded are 0 and passed == scenario_count;
//   - failed: at least one failed or errored (internal/runner.MapReport: Failed() is failed+errored);
//   - degraded: nothing failed or errored (failed outranks degraded). On v2 at least one scenario is
//     counted degraded; a v1 certificate has no degraded bucket, so there the missing scenarios show as
//     passed+failed+errored < scenario_count and equality is refused.
func checkTallies(cert *Certificate) CheckResult {
	const name = "certificate tallies"
	t, n := cert.Tallies, cert.Set.ScenarioCount
	bad := func(format string, a ...any) CheckResult {
		return CheckResult{Name: name, Status: StatusMismatch, Detail: fmt.Sprintf(format, a...)}
	}
	if t.Passed < 0 || t.Failed < 0 || t.Errored < 0 || t.Degraded < 0 || n < 0 {
		return bad("a negative count (passed %d, failed %d, errored %d, degraded %d, scenario_count %d)", t.Passed, t.Failed, t.Errored, t.Degraded, n)
	}
	if cert.Format == FormatV1 && t.Degraded != 0 {
		return bad("degraded is %d, but format %s has no degraded bucket", t.Degraded, FormatV1)
	}
	sum := t.Passed + t.Failed + t.Errored + t.Degraded
	if sum > n {
		return bad("passed %d + failed %d + errored %d + degraded %d = %d exceeds set.scenario_count %d", t.Passed, t.Failed, t.Errored, t.Degraded, sum, n)
	}
	switch cert.Verdict {
	case "passed":
		if t.Failed != 0 || t.Errored != 0 || t.Degraded != 0 {
			return bad("verdict is passed but tallies show failed %d, errored %d, degraded %d", t.Failed, t.Errored, t.Degraded)
		}
		if t.Passed != n {
			return bad("verdict is passed but passed is %d of set.scenario_count %d", t.Passed, n)
		}
	case "failed":
		if t.Failed+t.Errored == 0 {
			return bad("verdict is failed but tallies show no failed or errored scenario")
		}
	case "degraded":
		if t.Failed != 0 || t.Errored != 0 {
			return bad("verdict is degraded but tallies show failed %d, errored %d (a failure outranks degraded)", t.Failed, t.Errored)
		}
		if Tallied(cert.Format) && t.Degraded == 0 {
			return bad("verdict is degraded but tallies show no degraded scenario")
		}
		if cert.Format == FormatV1 && sum == n {
			return bad("verdict is degraded but passed %d of set.scenario_count %d leaves no scenario to be degraded", t.Passed, n)
		}
	default:
		return bad("unknown verdict %q (want passed, failed or degraded)", cert.Verdict)
	}
	if cert.Format == FormatV1 {
		return CheckResult{Name: name, Status: StatusVerified,
			Detail: fmt.Sprintf("passed %d + failed %d + errored %d of set.scenario_count %d, consistent with verdict %s — NOT anchored on a v1 certificate: a consistency check only, see NOT ANCHORED below", t.Passed, t.Failed, t.Errored, n, cert.Verdict)}
	}
	return CheckResult{Name: name, Status: StatusVerified,
		Detail: fmt.Sprintf("passed %d + failed %d + errored %d + degraded %d of set.scenario_count %d, consistent with verdict %s (and compared with the anchored verdict payload)", t.Passed, t.Failed, t.Errored, t.Degraded, n, cert.Verdict)}
}

// Unanchored lists, in plain words, the certificate fields verify CANNOT confirm against a chain — so a
// reader is told what a "verified" does not cover, instead of assuming it covers the whole document.
func Unanchored(cert *Certificate) []string {
	var out []string
	if cert.Format == FormatV1 {
		out = append(out,
			"tallies (passed, failed, errored): a v1 verdict anchor holds no tallies, so the counts and the failed/errored split are only checked for consistency with the verdict and set.scenario_count, never against the chain",
			"finished_at: not in the anchored payload of a v1 certificate")
	}
	out = append(out, "issued_at and anchoring: describe this document and the moment it was issued, not the run")
	if Tallied(cert.Format) {
		out = append(out, "anchors[].chain_id: advisory, used only to explain a failed read")
	}
	if cert.RevealedAt != "" {
		out = append(out, "revealed_at: the reveal anchor's digest is compared, its timestamp is not")
	}
	switch m := cert.ArtifactMeasurement; {
	case cert.Format != Format:
		out = append(out, fmt.Sprintf("artifact measurement: format %s carries none, so nothing in this certificate shows which build was running when the run began — only that the tester declared artifact_digest", cert.Format))
	case m == nil:
		// checkMeasurement reports the stripped block as a mismatch; nothing to add here
	case !m.Bound:
		out = append(out, "artifact measurement: the executor that ran this reported no measurement, so nothing binds which build was running; artifact_digest is only what the tester declared")
	case m.State == artifactmeasure.StateMatched:
		out = append(out, "artifact measurement: the running digests are what the executor reported reading from the SUT's cluster or docker daemon — bound to this verdict by evidence_bundle_hash, but attested by the executor, not independently observed by the chain or the control plane")
	default:
		out = append(out, "artifact measurement: NOT MEASURED — artifact_digest is only what the tester declared; the reason is bound to the verdict but proves nothing about the running build")
	}
	return out
}

// checkMeasurement is the artifact-measurement check on a v3 certificate (older formats carry none). It
// rebuilds the record from the certificate's own fields and demands two things: the record is internally
// consistent (its claimed state is what its own digests imply, artifactmeasure.Check), and it is the record
// the anchored evidence_bundle_hash commits to (artifactmeasure.BundleHash). Editing the state, a digest,
// the reason, the source, the commitment lists, the scenario root or the declared digest changes one of them.
func checkMeasurement(cert *Certificate) CheckResult {
	const name = "artifact measurement"
	bad := func(format string, a ...any) CheckResult {
		return CheckResult{Name: name, Status: StatusMismatch, Detail: fmt.Sprintf(format, a...)}
	}
	m := cert.ArtifactMeasurement
	if m == nil {
		return bad("a %s certificate must carry artifact_measurement; it has been removed", Format)
	}
	if !m.Bound {
		if m.State != artifactmeasure.StateNotMeasured || m.ScenarioEvidenceRoot != "" || len(m.Running) != 0 || m.Unresolved != 0 ||
			len(m.CommitmentFound) != 0 || len(m.CommitmentNotFound) != 0 {
			return bad("an unbound measurement can only say not_measured, with no digests and no scenario root; it claims state %q", m.State)
		}
		return CheckResult{Name: name, Status: StatusNotBound,
			Detail: "NOT MEASURED — " + m.Reason + " (nothing binds which build was running)"}
	}
	rec := artifactmeasure.Measurement{
		Version: artifactmeasure.Version, State: m.State, Reason: m.Reason, Source: m.Source,
		Declared: cert.VerdictRecord.ArtifactDigest, Running: orEmpty(m.Running), Unresolved: m.Unresolved,
		CommitmentFound: orEmpty(m.CommitmentFound), CommitmentNotFound: orEmpty(m.CommitmentNotFound),
	}
	if err := artifactmeasure.Check(rec, cert.VerdictRecord.ArtifactDigest); err != nil {
		return bad("the claimed measurement contradicts itself: %v", err)
	}
	if m.ScenarioEvidenceRoot == "" {
		return bad("a bound measurement must carry scenario_evidence_root")
	}
	if got := artifactmeasure.BundleHash(m.ScenarioEvidenceRoot, &rec); got != cert.VerdictRecord.EvidenceBundleHash {
		return bad("the measurement and scenario_evidence_root do not hash to the anchored evidence_bundle_hash (rebuilt %s, anchored %s) — a field was edited", got, cert.VerdictRecord.EvidenceBundleHash)
	}
	if m.State == artifactmeasure.StateMatched {
		return CheckResult{Name: name, Status: StatusVerified,
			Detail: fmt.Sprintf("matched — the declared artifact digest is among the %d image digest(s) the executor found running, and the measurement is inside the anchored evidence_bundle_hash", len(m.Running))}
	}
	return CheckResult{Name: name, Status: StatusNotBound,
		Detail: "NOT MEASURED — " + m.Reason + " (the record is bound to the verdict, but it proves nothing about the running build)"}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ChainIDer is implemented by a chain client that can say which chain it serves (eth_chainId). Verify
// asks svc for it, when it can answer, only to explain a failed read.
type ChainIDer interface {
	ChainID(ctx context.Context) (*big.Int, error)
}

// rpcChainID is the chain id svc serves, nil when svc cannot say or does not.
func rpcChainID(ctx context.Context, svc chainread.ChainService) *big.Int {
	c, ok := svc.(ChainIDer)
	if !ok {
		return nil
	}
	id, err := c.ChainID(ctx)
	if err != nil {
		return nil
	}
	return id
}

// RPCChainID is the chain id svc serves, nil when svc cannot say or does not. Exported so
// `argus anchor verify` explains a failed read exactly as `certificate verify` does.
func RPCChainID(ctx context.Context, svc chainread.ChainService) *big.Int {
	return rpcChainID(ctx, svc)
}

// ExplainRead is the shared wording for an Ethereum-style anchor that could not be read: it names the
// anchor's chain and, when the anchor's chain id is recorded (anchorChainID != 0) and --rpc serves a
// different one, both ids. ok is false when the anchor's chain id is not recorded and rpcID is known —
// the caller words that case for its own document type.
func ExplainRead(chain string, anchorChainID int64, err error, rpcID *big.Int) (string, bool) {
	switch {
	case rpcID == nil:
		return fmt.Sprintf("anchor is on chain %q — could not be read through --rpc: %v", chain, err), true
	case anchorChainID != 0 && rpcID.Int64() != anchorChainID:
		return fmt.Sprintf("anchor is on chain %q (chain id %d) but --rpc serves chain id %s — use an --rpc endpoint for %q; the node said: %v",
			chain, anchorChainID, rpcID, chain, err), true
	case anchorChainID != 0:
		return fmt.Sprintf("anchor is on chain %q (chain id %d), and --rpc serves the same chain id — the read failed for another reason: %v", chain, anchorChainID, err), true
	}
	return "", false
}

// readFailure is the detail for an Ethereum-style anchor that could not be read back: the node's raw
// error, preceded by the two facts a reader needs to see that they pointed --rpc at the wrong chain —
// which chain the ANCHOR is on and which chain the RPC serves. It never changes the
// status: an unreadable anchor stays unreachable, it is not evidence of tampering.
func readFailure(a Anchor, err error, rpcID *big.Int) string {
	if msg, ok := ExplainRead(a.Chain, a.ChainID, err, rpcID); ok {
		return msg
	}
	return fmt.Sprintf("anchor is on chain %q; --rpc serves chain id %s, and this certificate does not record the anchor's chain id (only %s and later certificates do) — if %s is not %q's chain id, --rpc is the wrong endpoint for this anchor; the node said: %v",
		a.Chain, rpcID, FormatV2, rpcID, a.Chain, err)
}

// anchorRole is one Anchor's classified purpose within a Certificate — never a fixed version number,
// since AC-12 lets both a verdict and its reveal land at whichever version the commitment's head
// happened to be at (see roleOfAnchors).
type anchorRole string

const (
	roleSet     anchorRole = "set"     // version 2, always
	roleVerdict anchorRole = "verdict" // this run's own verdict — the LOWEST non-set version present
	roleReveal  anchorRole = "reveal"  // this run's reveal anchor — any HIGHER version, per chain
)

// roleOfAnchors classifies every entry of anchors by the SAME rule store.SplitRunAnchors uses to split
// a run's ledger rows: version 2 is always the certification set's own anchor (fixed);
// among the rest, the LOWEST version on a given chain is that run's own verdict, and anything higher on
// the SAME chain is its reveal anchor — never a fixed 3/4. AC-12 lets a commitment's shared version
// counter put a run's own verdict at v3, v4, v5, … (fed.go's bindVerdictV3 binds each scheduled run's
// verdict onto the commitment's then-current head) and its reveal anywhere further still
// (internal/reveal.anchorRevealOnChain binds onto whatever the head is AT REVEAL TIME, which may have
// moved past this run's own verdict if a later run certified first) — a certificate must classify by
// relative position, not by comparing Version to a constant, or a legitimately-anchored reveal/verdict
// at an uncommon version number is reported as tampered.
func roleOfAnchors(anchors []Anchor) map[int]anchorRole {
	lowestNonSet := make(map[string]int, len(anchors))
	for _, a := range anchors {
		if a.Version == 2 {
			continue
		}
		if cur, ok := lowestNonSet[a.Chain]; !ok || a.Version < cur {
			lowestNonSet[a.Chain] = a.Version
		}
	}
	roles := make(map[int]anchorRole, len(anchors))
	for i, a := range anchors {
		switch {
		case a.Version == 2:
			roles[i] = roleSet
		case a.Version == lowestNonSet[a.Chain]:
			roles[i] = roleVerdict
		default:
			roles[i] = roleReveal
		}
	}
	return roles
}

// verifyRevealAnchor checks one reveal anchor (roleReveal — see roleOfAnchors; not necessarily
// version 4). UNLIKE the set/verdict anchors, this is NOT a byte-exact payload comparison: the real
// on-chain RevealV4 payload carries commitment_id, which a certificate never does (package-level
// rule — see TestCertificate_PinnedFields). Instead, the read-back payload is DECODED and its
// kind/run_id/document digest are checked against the certificate's own declared fields — still enough
// for the promised guarantee ("a tampered digest fails verification"): changing
// cert.RevealDocumentSHA256 (or RevealedAt/RunID) after the fact makes the decoded chain payload
// disagree with what the certificate now claims.
func verifyRevealAnchor(ctx context.Context, cert *Certificate, a Anchor, svc chainread.ChainService, hdr ots.HeaderSource, allow AllowList) CheckResult {
	name := fmt.Sprintf("anchor v%d %s", a.Version, a.Chain)
	if cert.RevealedAt == "" || cert.RevealDocumentSHA256 == "" {
		return CheckResult{Name: name, Status: StatusMismatch,
			Detail: "certificate carries a reveal anchor but no revealed_at/reveal_document_sha256 to check it against"}
	}
	decodeAndCheck := func(payload []byte) CheckResult {
		var v4 RevealV4
		if err := json.Unmarshal(payload, &v4); err != nil {
			return CheckResult{Name: name, Status: StatusMismatch, Detail: "read-back reveal payload does not decode: " + err.Error()}
		}
		if v4.Kind != "reveal" || v4.RunID != cert.RunID || v4.DocumentSHA256 != cert.RevealDocumentSHA256 {
			return CheckResult{Name: name, Status: StatusMismatch,
				Detail: "read-back reveal payload's kind/run_id/document digest does not match the certificate's declared fields"}
		}
		return CheckResult{Name: name, Status: StatusVerified, Detail: "read back; kind/run_id/document digest match the certificate's declared fields"}
	}
	if !allow.allows(a.Chain, a.Signer) {
		return CheckResult{Name: name, Status: StatusMismatch,
			Detail: fmt.Sprintf("declared signer %s is not in chain %s's allow-list", a.Signer, a.Chain)}
	}
	if len(a.Receipt) > 0 { // hash-only (OTS): no retrievable payload, so a certificate (which lacks
		// commitment_id) cannot reconstruct what the receipt would need to commit to. Honest gap, not a
		// mismatch: use `argus anchor verify` against the FULL reveal document for an OTS-anchored reveal.
		return CheckResult{Name: name, Status: StatusUnreachable,
			Detail: "a certificate cannot independently verify an OTS-receipted reveal anchor (it does not carry commitment_id) — use `argus anchor verify` against the full reveal document instead"}
	}
	if svc == nil {
		return CheckResult{Name: name, Status: StatusUnreachable, Detail: "no independent chain client configured (--rpc)"}
	}
	var block uint64
	if a.Block != nil {
		block = uint64(*a.Block)
	}
	rec, err := chainread.ReadAnchor(svc, chainread.AnchorRef{CorrelationID: a.ChainCorrelationID, Block: block}, chainread.WithAllowedSigners(a.Signer))
	if err != nil {
		return CheckResult{Name: name, Status: StatusUnreachable, Detail: readFailure(a, err, rpcChainID(ctx, svc))}
	}
	return decodeAndCheck(rec.Payload)
}

// Verify checks every one of cert's anchors independently: an Ethereum-style anchor (no Receipt) is
// read back through svc under its OWN declared signer, cross-checked against allow (when non-nil)
// and its payload compared byte-exact to the certificate's own declared set (v2) or verdict (v3)
// fields; a hash-only anchor (Receipt present) is checked through hdr the same way `argus anchor
// verify` checks an OTS entry. svc/hdr may be nil when cert carries no anchor of that kind; an entry
// that needs one anyway is reported unreachable by name rather than panicking. allow may be nil (no
// --chains given): the cross-check is then skipped and an anchor is trusted exactly as far as its own
// declared signer, read back successfully under chainread.WithAllowedSigners — the SAME trust
// `argus anchor verify` gives a reveal document's anchors with no additional allow-list.
func Verify(ctx context.Context, cert *Certificate, svc chainread.ChainService, hdr ots.HeaderSource, allow AllowList) []CheckResult {
	var results []CheckResult
	setP, _ := setPayload(cert)
	verdictP, verr := verdictPayload(cert)
	if verr != nil {
		// An unknown format cannot be rebuilt into ANY payload, so nothing below could be compared:
		// answer once, by name, and let Overall refuse it.
		return []CheckResult{{Name: "certificate format", Status: StatusMismatch,
			Detail: fmt.Sprintf("format %q is not one this verifier knows (want %s, %s or %s)", cert.Format, Format, FormatV2, FormatV1)}}
	}
	roles := roleOfAnchors(cert.Anchors)

	for i, a := range cert.Anchors {
		name := fmt.Sprintf("anchor v%d %s", a.Version, a.Chain)
		if a.Version < 2 {
			results = append(results, CheckResult{Name: name, Status: StatusMismatch,
				Detail: "a certificate anchor's version must be at least 2 — version 1 (commit-level) never appears on a certificate"})
			continue
		}
		if roles[i] == roleReveal {
			results = append(results, verifyRevealAnchor(ctx, cert, a, svc, hdr, allow))
			continue
		}
		want := setP
		if roles[i] == roleVerdict {
			want = verdictP
		}

		if len(a.Receipt) > 0 { // hash-only (OTS) chain
			if hdr == nil {
				results = append(results, CheckResult{Name: name, Status: StatusUnreachable,
					Detail: "no independent header source configured (--btc-headers)"})
				continue
			}
			if !allow.allows(a.Chain, a.Signer) {
				results = append(results, CheckResult{Name: name, Status: StatusMismatch,
					Detail: fmt.Sprintf("declared signer %s is not in chain %s's allow-list", a.Signer, a.Chain)})
				continue
			}
			p := ots.New(hdr, a.Chain)
			proof, err := p.Verify(ctx, chainread.Ref{Chain: a.Chain, Receipt: a.Receipt}, want)
			if err != nil {
				status := StatusUnreachable
				if errors.Is(err, chainread.ErrMismatch) {
					// chain library wraps ErrMismatch when the receipt itself contradicts the evidence (a
					// different digest, a wrong merkle root at its height, an unparseable receipt) —
					// that is a MISMATCH (evidence of tampering), never "could not check". A
					// header-source failure is not wrapped and stays UNREACHABLE.
					status = StatusMismatch
				}
				results = append(results, CheckResult{Name: name, Status: status, Detail: err.Error()})
				continue
			}
			if proof.Pending {
				results = append(results, CheckResult{Name: name, Status: StatusPending,
					Detail: "receipt is still pending a Bitcoin block-header attestation"})
				continue
			}
			results = append(results, CheckResult{Name: name, Status: StatusVerified,
				Detail: "receipt commits to the certificate's declared payload"})
			continue
		}

		// Ethereum-style: read back under the anchor's OWN declared signer.
		if svc == nil {
			results = append(results, CheckResult{Name: name, Status: StatusUnreachable,
				Detail: "no independent chain client configured (--rpc)"})
			continue
		}
		if !allow.allows(a.Chain, a.Signer) {
			results = append(results, CheckResult{Name: name, Status: StatusMismatch,
				Detail: fmt.Sprintf("declared signer %s is not in chain %s's allow-list", a.Signer, a.Chain)})
			continue
		}
		var block uint64
		if a.Block != nil {
			block = uint64(*a.Block)
		}
		rec, err := chainread.ReadAnchor(svc, chainread.AnchorRef{CorrelationID: a.ChainCorrelationID, Block: block},
			chainread.WithAllowedSigners(a.Signer))
		if err != nil {
			results = append(results, CheckResult{Name: name, Status: StatusUnreachable, Detail: readFailure(a, err, rpcChainID(ctx, svc))})
			continue
		}
		if !bytes.Equal(rec.Payload, want) {
			results = append(results, CheckResult{Name: name, Status: StatusMismatch,
				Detail: "read-back payload does not match the certificate's declared fields"})
			continue
		}
		results = append(results, CheckResult{Name: name, Status: StatusVerified,
			Detail: "read back under declared signer " + a.Signer})
	}
	// After the per-anchor results, so Overall's index alignment with cert.Anchors holds.
	results = append(results, checkTallies(cert))
	if cert.Format == Format {
		results = append(results, checkMeasurement(cert))
	}
	return results
}

// Render is the human summary `argus certificate verify` prints, one line per element: every result,
// then a NOT ANCHORED line per field verify could not confirm against a chain (Unanchored), then the
// overall verdict. It lives here so the command and the tests that show its output print the same text.
func Render(cert *Certificate, results []CheckResult) []string {
	var lines []string
	for _, r := range results {
		lines = append(lines, r.Line())
	}
	for _, u := range Unanchored(cert) {
		lines = append(lines, "NOT ANCHORED: "+u)
	}
	if Overall(cert, results) {
		return append(lines, "OVERALL: verified")
	}
	return append(lines, "OVERALL: NOT verified")
}

// Overall reports the exit-worthy verdict over results: true only when this run's own verdict anchor
// (roleVerdict — see roleOfAnchors; not necessarily version 3, since AC-12 lets a scheduled run's
// verdict land at whatever the commitment's head then was) verified and no anchor mismatched. An
// all-pending or all-unreachable set of checks is NOT a pass — absence of contradiction is not
// confirmation.
func Overall(cert *Certificate, results []CheckResult) bool {
	roles := roleOfAnchors(cert.Anchors)
	for _, r := range results { // includes the non-anchor results Verify appends (tallies, format)
		if r.Status == StatusMismatch {
			return false
		}
	}
	verdictVerified := false
	for i := range cert.Anchors {
		if i >= len(results) {
			break
		}
		r := results[i]
		if roles[i] == roleVerdict && r.Status == StatusVerified {
			verdictVerified = true
		}
	}
	return verdictVerified
}
