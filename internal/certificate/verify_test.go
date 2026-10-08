package certificate

// verify_test.go — Verify's pure logic, offline and independent: the certificate is built by hand
// from anchors written to an in-memory chain (fakechain_test.go) — no store, no control plane.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/chainread"
)

const verifyChainName = "cert-verify-test-chain"

// verifyFixture anchors a real v1 -> v2 (set) -> v3 (verdict) chain on chain librarytest and returns a
// Certificate built by hand from the anchored results (never through reveal/store), plus the chain's
// Service (the "independent client" a test dials) and its signer address.
func verifyFixture(t *testing.T) (cert *Certificate, svc chainread.ChainService, signerAddr string, chainsPath string) {
	t.Helper()
	return buildVerifyFixture(t, false)
}

// verifyFixtureTallied is verifyFixture for a NEW-format certificate: the chain holds a
// ledger.VerdictTallied and the certificate declares format v2 with the same tallies and finished_at.
func verifyFixtureTallied(t *testing.T) (cert *Certificate, svc chainread.ChainService, signerAddr string, chainsPath string) {
	t.Helper()
	return buildVerifyFixture(t, true)
}

const (
	fixtureFinishedAt = "2026-09-30T08:15:00Z"
	fixtureChainID    = 2026
)

// anchorResults is the per-anchor part of Verify's results (Verify appends the tallies result after).
func anchorResults(cert *Certificate, results []CheckResult) []CheckResult {
	return results[:len(cert.Anchors)]
}

func resultNamed(t *testing.T, results []CheckResult, name string) CheckResult {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result named %q in %+v", name, results)
	return CheckResult{}
}

func buildVerifyFixture(t *testing.T, tallied bool) (cert *Certificate, svc chainread.ChainService, signerAddr string, chainsPath string) {
	t.Helper()
	return buildVerifyFixtureFor(t, tallied, "passed", Tallies{Passed: 3})
}

// buildVerifyFixtureFor anchors a run with the given verdict and tallies (set scenario_count is 3).
func buildVerifyFixtureFor(t *testing.T, tallied bool, verdictWord string, tl Tallies) (cert *Certificate, svc chainread.ChainService, signerAddr string, chainsPath string) {
	t.Helper()
	chain := newFakeChain()
	addr, err := signerAddress(fixtureSignerKey)
	if err != nil {
		t.Fatalf("derive fixture signer: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "chains.json")
	body, _ := json.Marshal(map[string]any{"chains": map[string]any{
		verifyChainName: map[string]any{"name": verifyChainName, "chainType": "ethereum", "allowedSigners": []string{addr}},
	}})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write chains.json: %v", err)
	}

	v1raw, _ := json.Marshal(map[string]any{"commit": "a1b2c3d", "image_digests": []string{}, "runner_digest": "sha256:" + fmt.Sprintf("%064d", 1), "commitment_id": "cm-1"})
	v1, _ := chain.anchor(t, addr, v1raw, nil)
	setV2 := SetV2{HoldoutRoot: "root-1", CriteriaHash: "crit-1", SetHash: "sh-1", ScenarioCount: 3}
	setRaw, _ := json.Marshal(setV2)
	v2, v2tx := chain.anchor(t, addr, setRaw, &v1)
	verdict := VerdictV2{RunID: "run-verify-1", ArtifactDigest: "sha256:" + fmt.Sprintf("%064d", 2), EvidenceBundleHash: "sha256:" + fmt.Sprintf("%064d", 3), Verdict: verdictWord}
	var verdictRaw []byte
	if tallied {
		verdictRaw, _ = json.Marshal(VerdictTallied{
			RunID: verdict.RunID, ArtifactDigest: verdict.ArtifactDigest, EvidenceBundleHash: verdict.EvidenceBundleHash, Verdict: verdict.Verdict,
			Tallies: VerdictTallies{Passed: tl.Passed, Failed: tl.Failed, Errored: tl.Errored, Degraded: tl.Degraded}, FinishedAt: fixtureFinishedAt,
		})
	} else {
		verdictRaw, _ = json.Marshal(verdict)
	}
	v3, v3tx := chain.anchor(t, addr, verdictRaw, &v2)

	block2 := int64(v2.Block)
	block3 := int64(v3.Block)
	cert = &Certificate{
		Format: FormatV1, RunID: verdict.RunID, Verdict: verdict.Verdict, Tallies: tl,
		Set:           Set{HoldoutRoot: setV2.HoldoutRoot, CriteriaHash: setV2.CriteriaHash, SetHash: setV2.SetHash, ScenarioCount: setV2.ScenarioCount},
		VerdictRecord: VerdictRecord{ArtifactDigest: verdict.ArtifactDigest, EvidenceBundleHash: verdict.EvidenceBundleHash},
		Anchors: []Anchor{
			{Version: 2, Chain: verifyChainName, ChainCorrelationID: v2.CorrelationID, Block: &block2, TxHash: v2tx, Signer: addr},
			{Version: 3, Chain: verifyChainName, ChainCorrelationID: v3.CorrelationID, Block: &block3, TxHash: v3tx, Signer: addr},
		},
	}
	if tallied {
		cert.Format, cert.FinishedAt = FormatV2, fixtureFinishedAt
		for i := range cert.Anchors {
			cert.Anchors[i].ChainID = fixtureChainID
		}
	}
	fixtureChains[t.Name()] = chain
	return cert, chain, addr, path
}

// fixtureSignerKey is a throwaway secp256k1 key whose ADDRESS is the fixture's signer. It is used only to
// derive that address; nothing here signs anything.
const fixtureSignerKey = "0000000000000000000000000000000000000000000000000000000000000007"

// fixtureChains lets the reveal fixture find the fake chain behind a ChainService it was handed.
var fixtureChains = map[string]*fakeChain{}

func TestVerify_CleanCertificate_AllVerified(t *testing.T) {
	cert, svc, _, _ := verifyFixture(t)
	results := Verify(context.Background(), cert, svc, nil, nil)
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3 (v2, v3, tallies)", len(results))
	}
	for _, r := range results {
		t.Log(r.Line())
		if r.Status != StatusVerified {
			t.Errorf("expected verified, got: %s", r.Line())
		}
	}
	if !Overall(cert, results) {
		t.Errorf("Overall = false, want true (a v3 anchor verified, nothing mismatched)")
	}
}

func TestVerify_TamperedVerdictFails_Mismatch(t *testing.T) {
	cert, svc, _, _ := verifyFixture(t)
	cert.Verdict = "failed" // tamper: the certificate's own declared verdict no longer matches what was anchored
	results := Verify(context.Background(), cert, svc, nil, nil)
	var sawV3Mismatch bool
	for _, r := range results {
		t.Log(r.Line())
		if r.Name == "anchor v3 "+verifyChainName {
			if r.Status != StatusMismatch {
				t.Errorf("v3 anchor status = %s, want mismatch", r.Status)
			}
			sawV3Mismatch = true
		}
	}
	if !sawV3Mismatch {
		t.Fatalf("no v3 result found: %+v", results)
	}
	if Overall(cert, results) {
		t.Errorf("Overall = true with a mismatched anchor, want false")
	}
}

// The tallies line may say "compared with the anchored verdict payload" only when a verdict anchor
// really verified.
func talliesResult(t *testing.T, results []CheckResult) CheckResult {
	t.Helper()
	return resultNamed(t, results, "certificate tallies")
}

func TestVerify_TalliesLine_TamperedButInternallyConsistent_IsMismatch(t *testing.T) {
	cert, svc, _, _ := verifyFixtureTallied(t)
	// consistent with itself (failed 1 + passed 2 of 3, verdict failed) but not what was anchored
	cert.Verdict = "failed"
	cert.Tallies = Tallies{Passed: 2, Failed: 1}
	results := Verify(context.Background(), cert, svc, nil, nil)
	r := talliesResult(t, results)
	t.Log(r.Line())
	if r.Status != StatusMismatch {
		t.Fatalf("tallies line = %s, want MISMATCH when the anchored verdict payload disagrees", r.Line())
	}
	if strings.Contains(r.Detail, "compared with the anchored verdict payload on") {
		t.Errorf("a mismatching line still claims the comparison: %s", r.Line())
	}
}

func TestVerify_TalliesLine_AnchorNotReadBack_SaysInternalConsistencyOnly(t *testing.T) {
	cert, _, _, _ := verifyFixtureTallied(t)
	results := Verify(context.Background(), cert, nil, nil, nil) // no --rpc: nothing is read back
	r := talliesResult(t, results)
	t.Log(r.Line())
	if strings.Contains(r.Detail, "compared with the anchored verdict payload on") {
		t.Errorf("claims a comparison with an anchor that was never read: %s", r.Line())
	}
	if !strings.Contains(r.Detail, "internal consistency only") {
		t.Errorf("want an 'internal consistency only' wording: %s", r.Line())
	}
}

func TestVerify_TalliesLine_VerifiedAnchor_NamesTheChainCompared(t *testing.T) {
	cert, svc, _, _ := verifyFixtureTallied(t)
	results := Verify(context.Background(), cert, svc, nil, nil)
	r := talliesResult(t, results)
	t.Log(r.Line())
	if r.Status != StatusVerified || !strings.Contains(r.Detail, "(and compared with the anchored verdict payload on "+verifyChainName+")") {
		t.Errorf("clean tallied certificate: %s", r.Line())
	}
}

// One verdict anchor VERIFIED (read back), a second UNREACHABLE (its chain is not served): the payload
// WAS compared on the first chain, so the line is VERIFIED naming that chain, never "not read back".
func TestVerify_TalliesLine_OneVerifiedOneUnreachable_ClaimsComparisonOnTheVerifiedChain(t *testing.T) {
	cert, svc, _, _ := verifyFixtureTallied(t)
	second := cert.Anchors[1]
	second.Chain = "other-unserved-chain"
	second.ChainCorrelationID = "no-such-correlation-id" // the service cannot read this one back
	cert.Anchors = append(cert.Anchors, second)
	results := Verify(context.Background(), cert, svc, nil, nil)
	var sawVerified, sawUnreachable bool
	for _, r := range results {
		t.Log(r.Line())
		if r.Name == "anchor v3 "+verifyChainName && r.Status == StatusVerified {
			sawVerified = true
		}
		if r.Name == "anchor v3 other-unserved-chain" && r.Status == StatusUnreachable {
			sawUnreachable = true
		}
	}
	if !sawVerified || !sawUnreachable {
		t.Fatalf("fixture did not produce one verified + one unreachable verdict anchor: %+v", results)
	}
	r := talliesResult(t, results)
	if r.Status != StatusVerified {
		t.Fatalf("tallies line = %s, want VERIFIED", r.Line())
	}
	if !strings.Contains(r.Detail, "compared with the anchored verdict payload on "+verifyChainName) {
		t.Errorf("want the comparison claim naming %s: %s", verifyChainName, r.Line())
	}
	if strings.Contains(r.Detail, "internal consistency only") {
		t.Errorf("says the payload was not read back, which is false: %s", r.Line())
	}
}

func TestVerify_WrongSignerFails(t *testing.T) {
	cert, svc, _, _ := verifyFixture(t)
	wrongSigner, err := signerAddress("0000000000000000000000000000000000000000000000000000000000000001")
	if err != nil {
		t.Fatalf("derive a wrong-but-valid signer: %v", err)
	}
	for i := range cert.Anchors {
		cert.Anchors[i].Signer = wrongSigner
	}
	results := Verify(context.Background(), cert, svc, nil, nil)
	for _, r := range anchorResults(cert, results) {
		t.Log(r.Line())
		if r.Status == StatusVerified {
			t.Errorf("anchor verified under a WRONG declared signer: %s", r.Line())
		}
	}
}

func TestVerify_UnreachableChain_NoClientConfigured(t *testing.T) {
	cert, _, _, _ := verifyFixture(t)
	results := Verify(context.Background(), cert, nil, nil, nil) // svc=nil: no --rpc given
	for _, r := range anchorResults(cert, results) {
		t.Log(r.Line())
		if r.Status != StatusUnreachable {
			t.Errorf("status = %s, want unreachable (no chain client configured)", r.Status)
		}
	}
	if Overall(cert, results) {
		t.Errorf("Overall = true with every anchor unreachable, want false")
	}
}

func TestVerify_AllowList_SignerNotInThirdPartyChainsJSON_Mismatch(t *testing.T) {
	cert, svc, addr, chainsPath := verifyFixture(t)
	_ = addr
	// an allow-list naming a DIFFERENT signer than the one that actually anchored — the third party's
	// own chains.json disagrees with what the certificate declares.
	other, err := signerAddress("0000000000000000000000000000000000000000000000000000000000000002")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	allow := AllowList{verifyChainName: {other}}
	results := Verify(context.Background(), cert, svc, nil, allow)
	for _, r := range anchorResults(cert, results) {
		t.Log(r.Line())
		if r.Status != StatusMismatch {
			t.Errorf("status = %s, want mismatch (signer not in the supplied allow-list)", r.Status)
		}
	}
	_ = chainsPath
}

func TestLoadAllowList_RoundTrip(t *testing.T) {
	cert, svc, addr, chainsPath := verifyFixture(t)
	allow, err := LoadAllowList(chainsPath)
	if err != nil {
		t.Fatalf("LoadAllowList: %v", err)
	}
	if !allow.allows(verifyChainName, addr) {
		t.Fatalf("allow-list loaded from the real chains.json does not allow the chain's own signer: %+v", allow)
	}
	results := Verify(context.Background(), cert, svc, nil, allow)
	for _, r := range results {
		t.Log(r.Line())
		if r.Status != StatusVerified {
			t.Errorf("status = %s, want verified (allow-list matches the real signer)", r.Status)
		}
	}
}

// TestVerify_OTS_MatchingAndMismatchingReceipt exercises the hash-only branch directly (no
// chain librarytest chain needed): a fake header source and a receipt built by hand, mirroring
// reveal's TestVerifyAnchors_OTS_MatchingAndMismatchingReceipt.
func TestVerify_OTS_MatchingAndMismatchingReceipt(t *testing.T) {
	cert := &Certificate{
		Format: FormatV2, RunID: "run-ots-1", Verdict: "passed",
		VerdictRecord: VerdictRecord{ArtifactDigest: "sha256:aa", EvidenceBundleHash: "sha256:bb"},
	}
	payload, err := verdictPayload(cert)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	goodReceipt, _ := json.Marshal(map[string]any{"digest": digest[:], "height": 700000})
	hdr := fakeBTCHeaderSource{height: 700000, root: digest}

	cert.Anchors = []Anchor{{Version: 3, Chain: "bitcoin-ots", Receipt: goodReceipt, Signer: "s1"}}
	allow := AllowList{"bitcoin-ots": {"s1"}}
	results := Verify(context.Background(), cert, nil, hdr, allow)
	if len(results) != 2 || results[0].Status != StatusVerified { // anchor + tallies
		t.Fatalf("OTS receipt matching the declared verdict payload = %+v, want verified", results)
	}
	t.Log(results[0].Line())

	badDigest := sha256.Sum256([]byte("tampered"))
	badReceipt, _ := json.Marshal(map[string]any{"digest": badDigest[:], "height": 700000})
	cert.Anchors = []Anchor{{Version: 3, Chain: "bitcoin-ots", Receipt: badReceipt, Signer: "s1"}}
	results = Verify(context.Background(), cert, nil, hdr, allow)
	if len(results) != 2 || results[0].Status != StatusMismatch {
		t.Fatalf("OTS receipt for a TAMPERED digest = %+v, want mismatch", results)
	}
	t.Log(results[0].Line())
}

// verifyFixtureWithReveal extends verifyFixture's v1->v2->v3 chain with a v4 reveal anchor, and adds
// the certificate's declared reveal fields (RevealedAt/RevealDocumentSHA256).
func verifyFixtureWithReveal(t *testing.T) (cert *Certificate, svc chainread.ChainService) {
	t.Helper()
	var addr string
	cert, svc, addr, _ = verifyFixture(t)
	chain := fixtureChains[t.Name()]
	v3 := cert.Anchors[1]
	revealedAt := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	documentSHA256 := "deadbeef00112233"
	v4payload := RevealV4{Kind: "reveal", RunID: cert.RunID, CommitmentID: "cm-1", RevealedAt: revealedAt, DocumentSHA256: documentSHA256, ScenarioCount: cert.Set.ScenarioCount}
	raw, _ := json.Marshal(v4payload)
	prev := chainread.AnchorRef{CorrelationID: v3.ChainCorrelationID, Block: uint64(*v3.Block)}
	ref, txHash := chain.anchor(t, addr, raw, &prev)
	rec, err := chainread.ReadAnchor(svc, ref)
	if err != nil {
		t.Fatalf("ReadAnchor (v4): %v", err)
	}
	b4 := int64(ref.Block)
	cert.Anchors = append(cert.Anchors, Anchor{Version: 4, Chain: verifyChainName, ChainCorrelationID: ref.CorrelationID, Block: &b4, TxHash: txHash, Signer: rec.From})
	cert.RevealedAt = revealedAt.UTC().Format(time.RFC3339)
	cert.RevealDocumentSHA256 = documentSHA256
	return cert, svc
}

// TestVerify_RevealAnchor_Verified: a certificate's version-4 (reveal) anchor, read back
// independently, decodes to the SAME kind/run_id/document digest the certificate declares — verified,
// and Overall stays true (v3 already verified; v4 adds no mismatch).
func TestVerify_RevealAnchor_Verified(t *testing.T) {
	cert, svc := verifyFixtureWithReveal(t)
	results := Verify(context.Background(), cert, svc, nil, nil)
	if len(results) != 4 {
		t.Fatalf("len(results) = %d, want 4 (v2, v3, v4, tallies)", len(results))
	}
	for _, r := range results {
		t.Log(r.Line())
		if r.Status != StatusVerified {
			t.Errorf("expected verified, got: %s", r.Line())
		}
	}
	if !Overall(cert, results) {
		t.Errorf("Overall = false, want true (v3 verified, no mismatch)")
	}
}

// TestVerify_RevealAnchor_TamperedDigestFails: a certificate whose RevealDocumentSHA256 was tampered
// AFTER the fact (the chain's own record did not change) fails verification on the v4 entry — the
// promise's "a tampered digest fails verification" — and Overall flips to false, even though v2/v3
// still verify cleanly.
func TestVerify_RevealAnchor_TamperedDigestFails(t *testing.T) {
	cert, svc := verifyFixtureWithReveal(t)
	cert.RevealDocumentSHA256 = "0000000000000000" // tampered: the chain still carries the ORIGINAL digest
	results := Verify(context.Background(), cert, svc, nil, nil)
	if len(results) != 4 {
		t.Fatalf("len(results) = %d, want 4", len(results))
	}
	v4Result := results[2]
	t.Log(v4Result.Line())
	if v4Result.Status != StatusMismatch {
		t.Errorf("v4 result status = %s, want mismatch (tampered reveal_document_sha256)", v4Result.Status)
	}
	if Overall(cert, results) {
		t.Errorf("Overall = true, want false — a tampered reveal digest must fail overall verification")
	}
}

// TestVerify_RevealAnchor_NoRevealedAtDeclared_Mismatch: a certificate carrying a version-4 anchor but
// no revealed_at/reveal_document_sha256 (malformed, or itself tampered by removing those two fields)
// cannot be checked and is reported as a mismatch, never silently skipped.
func TestVerify_RevealAnchor_NoRevealedAtDeclared_Mismatch(t *testing.T) {
	cert, svc := verifyFixtureWithReveal(t)
	cert.RevealedAt = ""
	cert.RevealDocumentSHA256 = ""
	results := Verify(context.Background(), cert, svc, nil, nil)
	v4Result := results[2]
	t.Log(v4Result.Line())
	if v4Result.Status != StatusMismatch {
		t.Errorf("v4 result status = %s, want mismatch (no revealed_at/reveal_document_sha256 to check against)", v4Result.Status)
	}
}

type fakeBTCHeaderSource struct {
	height uint64
	root   [32]byte
}

func (h fakeBTCHeaderSource) MerkleRootAt(ctx context.Context, height uint64) ([32]byte, error) {
	if height != h.height {
		return [32]byte{}, fmt.Errorf("fakeBTCHeaderSource: no fixture for height %d", height)
	}
	return h.root, nil
}
