package certificate

// verify_otsmismatch_test.go — `argus certificate verify` judges a hash-only (OTS) anchor the way
// `argus anchor verify` does: a receipt the Bitcoin header source CONTRADICTS (wrong merkle root at its
// height, or a receipt that does not parse) is a MISMATCH and fails the run; a header source that cannot
// ANSWER is UNREACHABLE and does not. No database: the certificate is built by hand.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mapHeaders answers a fixed set of heights and fails every other one (a source that cannot be asked).
type mapHeaders struct{ roots map[uint64][32]byte }

func (h mapHeaders) MerkleRootAt(ctx context.Context, height uint64) ([32]byte, error) {
	root, ok := h.roots[height]
	if !ok {
		return [32]byte{}, fmt.Errorf("mapHeaders: header source unavailable for height %d", height)
	}
	return root, nil
}

// otsCertFixture is a hand-built certificate whose v3 (verdict) OTS anchor genuinely verifies at height
// 700000; each test appends a v2 (set) anchor that the header source contradicts or cannot answer.
func otsCertFixture(t *testing.T) (cert *Certificate, verified Anchor, hdr mapHeaders, setDigest [32]byte, allow AllowList) {
	t.Helper()
	cert = &Certificate{
		Format: FormatV2, RunID: "run-ots-mismatch", Verdict: "passed",
		VerdictRecord: VerdictRecord{ArtifactDigest: "sha256:aa", EvidenceBundleHash: "sha256:bb"},
	}
	verdictP, err := verdictPayload(cert)
	if err != nil {
		t.Fatal(err)
	}
	setP, err := setPayload(cert)
	if err != nil {
		t.Fatal(err)
	}
	vd := sha256.Sum256(verdictP)
	setDigest = sha256.Sum256(setP)
	receipt, _ := json.Marshal(map[string]any{"digest": vd[:], "height": 700000})
	verified = Anchor{Version: 3, Chain: "bitcoin-ots", Receipt: receipt, Signer: "s1"}
	hdr = mapHeaders{roots: map[uint64][32]byte{700000: vd}}
	return cert, verified, hdr, setDigest, AllowList{"bitcoin-ots": {"s1"}}
}

func anchorResult(t *testing.T, results []CheckResult, name string) CheckResult {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result named %q in %+v", name, results)
	return CheckResult{}
}

func TestCertificateVerify_OTSWrongMerkleRootIsMismatchAndFailsTheRun(t *testing.T) {
	cert, verified, hdr, setDigest, allow := otsCertFixture(t)
	hdr.roots[700001] = sha256.Sum256([]byte("a different block's merkle root"))
	contradicted, _ := json.Marshal(map[string]any{"digest": setDigest[:], "height": 700001})
	cert.Anchors = []Anchor{verified, {Version: 2, Chain: "bitcoin-ots", Receipt: contradicted, Signer: "s1"}}

	results := Verify(context.Background(), cert, nil, hdr, allow)
	r := anchorResult(t, results, "anchor v2 bitcoin-ots")
	t.Log(r.Line())
	if r.Status != StatusMismatch {
		t.Fatalf("a receipt whose fold differs from the block's merkle root is %q, want MISMATCH: %s", r.Status, r.Line())
	}
	if Overall(cert, results) {
		t.Fatalf("Overall = true with a contradicted receipt; the run must fail")
	}
}

func TestCertificateVerify_OTSUnparseableReceiptIsMismatchAndFailsTheRun(t *testing.T) {
	cert, verified, hdr, _, allow := otsCertFixture(t)
	cert.Anchors = []Anchor{verified, {Version: 2, Chain: "bitcoin-ots", Receipt: []byte("this is not a receipt"), Signer: "s1"}}

	results := Verify(context.Background(), cert, nil, hdr, allow)
	r := anchorResult(t, results, "anchor v2 bitcoin-ots")
	t.Log(r.Line())
	if r.Status != StatusMismatch {
		t.Fatalf("an unparseable receipt is %q, want MISMATCH: %s", r.Status, r.Line())
	}
	if Overall(cert, results) {
		t.Fatalf("Overall = true with an unparseable receipt; the run must fail")
	}
}

func TestCertificateVerify_OTSHeaderSourceFailureStaysUnreachable(t *testing.T) {
	cert, verified, hdr, setDigest, allow := otsCertFixture(t)
	unanswered, _ := json.Marshal(map[string]any{"digest": setDigest[:], "height": 700002})
	cert.Anchors = []Anchor{verified, {Version: 2, Chain: "bitcoin-ots", Receipt: unanswered, Signer: "s1"}}

	results := Verify(context.Background(), cert, nil, hdr, allow)
	r := anchorResult(t, results, "anchor v2 bitcoin-ots")
	t.Log(r.Line())
	if r.Status != StatusUnreachable {
		t.Fatalf("a header source that cannot answer is %q, want UNREACHABLE: %s", r.Status, r.Line())
	}
	if !strings.Contains(r.Detail, "header source") {
		t.Errorf("the UNREACHABLE line lost the header-source detail: %s", r.Line())
	}
	if !Overall(cert, results) {
		t.Fatalf("Overall = false: an unanswerable header source must not fail a run whose verdict anchor verified")
	}
}
