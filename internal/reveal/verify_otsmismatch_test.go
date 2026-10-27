package reveal

// verify_otsmismatch_test.go — a hash-only (OTS) receipt the Bitcoin header source CONTRADICTS (a wrong
// merkle root at its height, or a receipt that does not parse) is a MISMATCH and fails the run; a header
// source that cannot ANSWER is UNREACHABLE and does not. No database: the document is built by hand.

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

// otsFixture is a hand-built document with one OTS verdict anchor that genuinely verifies at height
// 700000, plus a header source that confirms it. A second anchor is added by each test.
func otsFixture(t *testing.T) (doc *Document, verified AnchorEntry, hdr mapHeaders, setDigest [32]byte) {
	t.Helper()
	doc = &Document{RunID: "run-ots-mismatch", Outcome: "passed"}
	verdictP, err := jsonMarshalVerdictV2(doc)
	if err != nil {
		t.Fatal(err)
	}
	setP, err := jsonMarshalSetV2(doc)
	if err != nil {
		t.Fatal(err)
	}
	vd := sha256.Sum256(verdictP)
	setDigest = sha256.Sum256(setP)
	receipt, _ := json.Marshal(map[string]any{"digest": vd[:], "height": 700000})
	verified = AnchorEntry{Version: 3, Chain: "bitcoin-ots", Receipt: receipt}
	hdr = mapHeaders{roots: map[uint64][32]byte{700000: vd}}
	return doc, verified, hdr, setDigest
}

func statusOf(results []CheckResult, name string) Status {
	for _, r := range results {
		if r.Name == name {
			return r.Status
		}
	}
	return ""
}

func TestAnchorVerify_OTSWrongMerkleRootIsMismatchAndFailsTheRun(t *testing.T) {
	doc, verified, hdr, setDigest := otsFixture(t)
	wrongRoot := sha256.Sum256([]byte("a different block's merkle root"))
	hdr.roots[700001] = wrongRoot
	contradicted, _ := json.Marshal(map[string]any{"digest": setDigest[:], "height": 700001})
	doc.Anchors = []AnchorEntry{verified, {Version: 2, Chain: "bitcoin-ots", Receipt: contradicted}}

	results := VerifyAnchors(context.Background(), doc, nil, hdr)
	out := lines(results)
	t.Log("\n" + out)
	if got := statusOf(results, "anchor v2 bitcoin-ots"); got != StatusMismatch {
		t.Fatalf("a receipt whose fold differs from the block's merkle root is %q, want MISMATCH:\n%s", got, out)
	}
	if Overall(results) {
		t.Fatalf("Overall = true with a contradicted receipt; the run must fail:\n%s", out)
	}
}

func TestAnchorVerify_OTSUnparseableReceiptIsMismatchAndFailsTheRun(t *testing.T) {
	doc, verified, hdr, _ := otsFixture(t)
	doc.Anchors = []AnchorEntry{verified, {Version: 2, Chain: "bitcoin-ots", Receipt: []byte("this is not a receipt")}}

	results := VerifyAnchors(context.Background(), doc, nil, hdr)
	out := lines(results)
	t.Log("\n" + out)
	if got := statusOf(results, "anchor v2 bitcoin-ots"); got != StatusMismatch {
		t.Fatalf("an unparseable receipt is %q, want MISMATCH:\n%s", got, out)
	}
	if Overall(results) {
		t.Fatalf("Overall = true with an unparseable receipt; the run must fail:\n%s", out)
	}
}

func TestAnchorVerify_OTSHeaderSourceFailureStaysUnreachable(t *testing.T) {
	doc, verified, hdr, setDigest := otsFixture(t)
	// height 700002 is not in the source: it cannot answer, which is "could not check".
	unanswered, _ := json.Marshal(map[string]any{"digest": setDigest[:], "height": 700002})
	doc.Anchors = []AnchorEntry{verified, {Version: 2, Chain: "bitcoin-ots", Receipt: unanswered}}

	results := VerifyAnchors(context.Background(), doc, nil, hdr)
	out := lines(results)
	t.Log("\n" + out)
	if got := statusOf(results, "anchor v2 bitcoin-ots"); got != StatusUnreachable {
		t.Fatalf("a header source that cannot answer is %q, want UNREACHABLE:\n%s", got, out)
	}
	if !strings.Contains(out, "header source") {
		t.Errorf("the UNREACHABLE line lost the header-source detail:\n%s", out)
	}
	if !Overall(results) {
		t.Fatalf("Overall = false: an unanswerable header source must not fail a run whose verdict anchor verified:\n%s", out)
	}
}
