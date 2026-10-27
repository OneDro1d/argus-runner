package reveal

// verify_test.go — `argus anchor verify`'s pure logic, offline and independent. The reveal document is
// built by hand (two sealed files, their Merkle root and inclusion proofs, three anchors) over an
// in-memory chain and a fake Bitcoin header source: no database, no real network anywhere in this file.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/certificate"
	"github.com/OneDro1d/argus-runner/internal/chainread"
	"github.com/OneDro1d/argus-runner/internal/federation"
)

// fixtureSignerKey is a throwaway secp256k1 key whose ADDRESS is the fixture's signer.
const fixtureSignerKey = "0000000000000000000000000000000000000000000000000000000000000007"

const verifyChainName = "argus-verify-test-chain"

// verifyFixture builds a document that sealed TWO scenarios (so one tampered file leaves the other one's
// own proof still verifiable against the untouched declared root), decided "passed", anchored v1, v2 and
// v3 on an in-memory chain under the fixture signer, and the reveal anchor of that document. It returns the
// document, the chain as an independent client, and the signer's address.
func verifyFixture(t *testing.T) (doc *Document, svc chainread.ChainService, signerAddr string) {
	t.Helper()
	addr, err := signerAddress(fixtureSignerKey)
	if err != nil {
		t.Fatalf("derive fixture signer: %v", err)
	}
	files := []SealedFile{
		{ID: "VERI-00A", Path: "http-ingestion/VERI-00A.md", Body: "# VERI-00A\nTRIGGER\nVERIFY\nEXPECT\n### Runnable\nok A"},
		{ID: "VERI-00B", Path: "http-ingestion/VERI-00B.md", Body: "# VERI-00B\nTRIGGER\nVERIFY\nEXPECT\n### Runnable\nok B"},
	}
	root, err := MerkleRoot([]MerkleLeaf{{Path: files[0].Path, Body: files[0].Body}, {Path: files[1].Path, Body: files[1].Body}})
	if err != nil {
		t.Fatal(err)
	}
	// With two leaves each one's proof is the other leaf's hash, on the side it sits.
	h0, h1 := leafHash(files[0].Path, files[0].Body), leafHash(files[1].Path, files[1].Body)
	proofs := []ScenarioProof{
		{Path: files[0].Path, Proof: []ProofStep{{Sibling: fmt.Sprintf("%x", h1[:]), Left: false}}},
		{Path: files[1].Path, Proof: []ProofStep{{Sibling: fmt.Sprintf("%x", h0[:]), Left: true}}},
	}
	verdicts := []federation.ScenarioResult{
		{ID: "VERI-00A", Outcome: "passed", EvidenceHash: "evhashA"},
		{ID: "VERI-00B", Outcome: "passed", EvidenceHash: "evhashB"},
	}
	digest := "sha256:" + strings.Repeat("dd", 32)
	doc = &Document{
		RunID: "run-verify-1", InstanceID: "inst-verify", CommitmentID: "cm-verify-1",
		HoldoutRoot: root, SetHash: "set-hash-1", CriteriaHash: "criteria-1",
		SealedFiles: files, Proofs: proofs,
		Verdicts: verdicts, Outcome: "passed", ArtifactDigest: digest,
		EvidenceBundleHash: evidenceBundleHash(verdicts),
		Anchored:           true,
	}

	chain := newFakeChain()
	v1raw, _ := json.Marshal(map[string]any{"commit": "a1b2c3d", "runner_digest": digest, "commitment_id": doc.CommitmentID})
	v1, v1tx := chain.anchor(t, addr, v1raw, nil)
	setRaw, err := jsonMarshalSetV2(doc)
	if err != nil {
		t.Fatal(err)
	}
	v2, v2tx := chain.anchor(t, addr, setRaw, &v1)
	verdictRaw, err := jsonMarshalVerdictV2(doc)
	if err != nil {
		t.Fatal(err)
	}
	v3, v3tx := chain.anchor(t, addr, verdictRaw, &v2)

	entry := func(version int, ref chainread.AnchorRef, tx string) AnchorEntry {
		blk := int64(ref.Block)
		return AnchorEntry{Version: version, Chain: verifyChainName, ChainCorrelationID: ref.CorrelationID, Block: &blk, TxHash: tx, Signer: addr}
	}
	doc.Anchors = []AnchorEntry{entry(1, v1, v1tx), entry(2, v2, v2tx), entry(3, v3, v3tx)}

	// The reveal anchor: the fact that the reveal happened, anchored as the next version after the verdict.
	revealedAt := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	doc.RevealedAt = &revealedAt
	doc.RevealDocumentSHA256 = strings.Repeat("ab", 32)
	revealRaw, _ := json.Marshal(certificate.RevealV4{
		Kind: "reveal", RunID: doc.RunID, CommitmentID: doc.CommitmentID,
		RevealedAt: revealedAt, DocumentSHA256: doc.RevealDocumentSHA256, ScenarioCount: len(files),
	})
	v4, v4tx := chain.anchor(t, addr, revealRaw, &v3)
	doc.RevealAnchors = []AnchorEntry{entry(4, v4, v4tx)}
	return doc, chain, addr
}

func allPass(t *testing.T, results []CheckResult) {
	t.Helper()
	for _, r := range results {
		t.Log(r.Line())
		if r.Status != StatusVerified {
			t.Errorf("expected VERIFIED, got: %s", r.Line())
		}
	}
}

func TestMerkle_RootOfOneLeafIsItsLeafHashAndAnOddLevelPairsTheLastNodeWithItself(t *testing.T) {
	one, err := MerkleRoot([]MerkleLeaf{{Path: "a.md", Body: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	want := leafHash("a.md", "x")
	if one != fmt.Sprintf("%x", want[:]) {
		t.Errorf("one-leaf root = %s, want its leaf hash %x", one, want)
	}
	if _, err := MerkleRoot(nil); err == nil {
		t.Error("an empty set has no root; want an error")
	}
	three := []MerkleLeaf{{Path: "a.md", Body: "1"}, {Path: "b.md", Body: "2"}, {Path: "c.md", Body: "3"}}
	root, err := MerkleRoot(three)
	if err != nil {
		t.Fatal(err)
	}
	// c has no sibling at its level: it is hashed with itself, then that node meets (a,b)'s parent.
	a, b, c := leafHash("a.md", "1"), leafHash("b.md", "2"), leafHash("c.md", "3")
	ab, cc := parentHash(a, b), parentHash(c, c)
	top := parentHash(ab, cc)
	if root != fmt.Sprintf("%x", top[:]) {
		t.Errorf("three-leaf root = %s, want %x", root, top)
	}
	if !VerifyInclusion(root, "c.md", "3", []ProofStep{{Sibling: fmt.Sprintf("%x", c[:]), Left: false}, {Sibling: fmt.Sprintf("%x", ab[:]), Left: true}}) {
		t.Error("the odd-level proof for the last leaf does not verify")
	}
	if VerifyInclusion(root, "c.md", "TAMPERED", []ProofStep{{Sibling: fmt.Sprintf("%x", c[:]), Left: false}, {Sibling: fmt.Sprintf("%x", ab[:]), Left: true}}) {
		t.Error("a changed body still verified against the root")
	}
	if VerifyInclusion(root, "c.md", "3", []ProofStep{{Sibling: "not-hex", Left: false}}) {
		t.Error("a proof step that is not 32 bytes of hex verified")
	}
}

func TestVerifyHoldout_CleanDocument_AllPass(t *testing.T) {
	doc, _, _ := verifyFixture(t)
	allPass(t, VerifyHoldout(doc))
}

func TestVerifyHoldout_TamperedFileFails(t *testing.T) {
	doc, _, _ := verifyFixture(t)
	doc.SealedFiles[0].Body = "TAMPERED"
	results := VerifyHoldout(doc)
	var sawFail, otherStillFine bool
	for _, r := range results {
		t.Log(r.Line())
		if r.Name == "inclusion proof "+doc.SealedFiles[0].Path && r.Status != StatusVerified {
			sawFail = true
		}
		if r.Name == "inclusion proof "+doc.SealedFiles[1].Path && r.Status == StatusVerified {
			otherStillFine = true
		}
	}
	if !sawFail {
		t.Errorf("a tampered sealed file did not fail its own inclusion-proof check: %+v", results)
	}
	if !otherStillFine {
		t.Errorf("the untouched file's own proof should still verify against the declared root: %+v", results)
	}
}

func TestVerifyHoldout_NoSealedFilesIsAMismatchNotAPanic(t *testing.T) {
	results := VerifyHoldout(&Document{HoldoutRoot: "x"})
	if len(results) != 1 || results[0].Status != StatusMismatch {
		t.Fatalf("a document with no sealed files = %+v, want one MISMATCH", results)
	}
}

func TestVerifyEvidenceBundleHash_CleanDocument_Passes(t *testing.T) {
	doc, _, _ := verifyFixture(t)
	r := VerifyEvidenceBundleHash(doc)
	t.Log(r.Line())
	if r.Status != StatusVerified {
		t.Errorf("expected VERIFIED, got: %s", r.Line())
	}
}

func TestVerifyEvidenceBundleHash_TamperedHashFails(t *testing.T) {
	doc, _, _ := verifyFixture(t)
	doc.EvidenceBundleHash = "0000000000000000000000000000000000000000000000000000000000000000"
	r := VerifyEvidenceBundleHash(doc)
	t.Log(r.Line())
	if r.Status == StatusVerified {
		t.Error("a tampered evidence_bundle_hash passed verification; want a MISMATCH")
	}
}

func TestVerifyAnchors_CleanDocument_AllPass(t *testing.T) {
	doc, svc, _ := verifyFixture(t)
	allPass(t, VerifyAnchors(context.Background(), doc, svc, nil))
}

func TestVerifyAnchors_TalliedVerdictRebuildsTheTalliedBytes(t *testing.T) {
	doc, _, addr := verifyFixture(t)
	doc.VerdictPayloadFormat = certificate.VerdictPayloadTallied
	doc.VerdictTallies = &certificate.VerdictTallies{Passed: 2}
	doc.FinishedAt = "2026-10-20T11:00:00Z"
	// the chain now holds the tallied bytes for the verdict
	chain := newFakeChain()
	raw, err := jsonMarshalVerdictV2(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"tallies":{"passed":2,"failed":0,"errored":0,"degraded":0}`) || !strings.Contains(string(raw), `"finished_at":"2026-10-20T11:00:00Z"`) {
		t.Fatalf("tallied payload = %s", raw)
	}
	ref, tx := chain.anchor(t, addr, raw, nil)
	blk := int64(ref.Block)
	doc.Anchors = []AnchorEntry{{Version: 3, Chain: verifyChainName, ChainCorrelationID: ref.CorrelationID, Block: &blk, TxHash: tx, Signer: addr}}
	allPass(t, VerifyAnchors(context.Background(), doc, chain, nil))
}

func TestVerifyAnchors_WrongSignerFails(t *testing.T) {
	doc, svc, _ := verifyFixture(t)
	// a real, well-formed address — just not the one that actually signed these anchors.
	wrongSigner, err := signerAddress("0000000000000000000000000000000000000000000000000000000000000001")
	if err != nil {
		t.Fatalf("derive a wrong-but-valid signer address: %v", err)
	}
	for i := range doc.Anchors {
		doc.Anchors[i].Signer = wrongSigner
	}
	results := VerifyAnchors(context.Background(), doc, svc, nil)
	for _, r := range results {
		t.Log(r.Line())
		if r.Status == StatusVerified {
			t.Errorf("anchor check passed under a WRONG declared signer: %s", r.Line())
		}
		if r.Status != StatusMismatch {
			t.Errorf("a signer the chain refuses is a MISMATCH (the data is there, signed by someone else), got %s", r.Line())
		}
	}
}

func TestVerifyAnchors_OTS_MatchingAndMismatchingReceipt(t *testing.T) {
	doc, _, _ := verifyFixture(t)
	payload, err := jsonMarshalVerdictV2(doc)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	goodReceipt, _ := json.Marshal(map[string]any{"digest": digest[:], "height": 700000})
	hdr := fakeBTCHeaderSource{height: 700000, root: digest}

	doc.Anchors = []AnchorEntry{{Version: 3, Chain: "bitcoin-ots", Receipt: goodReceipt}}
	results := VerifyAnchors(context.Background(), doc, nil, hdr)
	if len(results) != 1 || results[0].Status != StatusVerified {
		t.Fatalf("OTS receipt matching the declared verdict payload = %+v, want one VERIFIED", results)
	}
	t.Log(results[0].Line())

	badDigest := sha256.Sum256([]byte("tampered"))
	badReceipt, _ := json.Marshal(map[string]any{"digest": badDigest[:], "height": 700000})
	doc.Anchors = []AnchorEntry{{Version: 3, Chain: "bitcoin-ots", Receipt: badReceipt}}
	results = VerifyAnchors(context.Background(), doc, nil, hdr)
	if len(results) != 1 || results[0].Status == StatusVerified {
		t.Fatalf("OTS receipt for a TAMPERED digest = %+v, want one non-verified line", results)
	}
	t.Log(results[0].Line())
}

func TestVerifyAnchors_ACurrentReceiptBesideTheFrozenOneIsUsedAndStillChecked(t *testing.T) {
	doc, _, _ := verifyFixture(t)
	payload, _ := jsonMarshalVerdictV2(doc)
	digest := sha256.Sum256(payload)
	pending, _ := json.Marshal(map[string]any{"digest": digest[:]}) // not yet attested
	attested, _ := json.Marshal(map[string]any{"digest": digest[:], "height": 700000})
	hdr := fakeBTCHeaderSource{height: 700000, root: digest}

	doc.Anchors = []AnchorEntry{{Version: 3, Chain: "bitcoin-ots", Receipt: pending}}
	doc.CurrentReceipts = []CurrentReceipt{{Version: 3, Chain: "bitcoin-ots", Receipt: attested}}
	results := VerifyAnchors(context.Background(), doc, nil, hdr)
	t.Log(lines(results))
	if len(results) != 1 || results[0].Status != StatusVerified || !strings.Contains(results[0].Detail, currentReceiptNote) {
		t.Fatalf("a served current receipt must be used and said so: %+v", results)
	}

	// a forged current receipt (it commits to another payload) is a MISMATCH, never a pass
	other := sha256.Sum256([]byte("another payload"))
	forged, _ := json.Marshal(map[string]any{"digest": other[:], "height": 700000})
	doc.CurrentReceipts = []CurrentReceipt{{Version: 3, Chain: "bitcoin-ots", Receipt: forged}}
	results = VerifyAnchors(context.Background(), doc, nil, hdr)
	t.Log(lines(results))
	if len(results) != 1 || results[0].Status != StatusMismatch {
		t.Fatalf("a forged current receipt = %+v, want one MISMATCH", results)
	}
}

func TestVerifyRevealAnchors_ReadBackMatchesAndATamperedDocumentHashFails(t *testing.T) {
	doc, svc, _ := verifyFixture(t)
	results := VerifyRevealAnchors(context.Background(), doc, svc, nil)
	allPass(t, results)
	if len(results) != 1 {
		t.Fatalf("want one reveal anchor line, got %d", len(results))
	}
	doc.RevealDocumentSHA256 = strings.Repeat("cd", 32)
	results = VerifyRevealAnchors(context.Background(), doc, svc, nil)
	if len(results) != 1 || results[0].Status != StatusMismatch {
		t.Fatalf("a changed reveal_document_sha256 = %+v, want one MISMATCH", results)
	}
	doc.RevealedAt = nil
	results = VerifyRevealAnchors(context.Background(), doc, svc, nil)
	if len(results) != 1 || results[0].Status != StatusMismatch {
		t.Fatalf("reveal anchors with no revealed_at = %+v, want one MISMATCH (cannot rebuild the payload)", results)
	}
	doc.RevealAnchors = nil
	if got := VerifyRevealAnchors(context.Background(), doc, svc, nil); len(got) != 0 {
		t.Fatalf("a document with no reveal anchors has no reveal-anchor lines, got %+v", got)
	}
}

func TestRender_EndsWithTheOverallVerdict(t *testing.T) {
	doc, svc, _ := verifyFixture(t)
	out := Render(VerifyReveal(context.Background(), doc, svc, nil))
	if last := out[len(out)-1]; last != "OVERALL: verified" {
		t.Fatalf("last line = %q, want %q:\n%s", last, "OVERALL: verified", strings.Join(out, "\n"))
	}
	doc.Outcome = "failed"
	out = Render(VerifyReveal(context.Background(), doc, svc, nil))
	if last := out[len(out)-1]; last != "OVERALL: NOT verified" {
		t.Fatalf("last line = %q, want %q", last, "OVERALL: NOT verified")
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
