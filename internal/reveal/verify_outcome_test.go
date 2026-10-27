package reveal

// verify_outcome_test.go — `argus anchor verify` judges a reveal document the way `argus certificate
// verify` judges a certificate: VERIFIED / MISMATCH / UNREACHABLE-or-PENDING, and the exit decision
// (Overall) is "no mismatch AND at least one verdict anchor verified". The fixture is built by hand over an in-memory chain.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/chainread"
)

// outcomeFixture is verifyFixture without its reveal anchors, which have their own tests, so that every run
// here is decided by the verdict, set and commit anchors alone.
func outcomeFixture(t *testing.T) (*Document, chainread.ChainService, string) {
	t.Helper()
	doc, svc, addr := verifyFixture(t)
	doc.RevealAnchors = nil
	return doc, svc, addr
}

// overallOK is the exit decision: exit 0 iff true.
func overallOK(results []CheckResult) bool { return Overall(results) }

// chainIDSvc is an --rpc client that can also say which chain it serves (what cmd/argus's rpcService does).
type chainIDSvc struct {
	chainread.ChainService
	id int64
}

func (c chainIDSvc) ChainID(context.Context) (*big.Int, error) { return big.NewInt(c.id), nil }

func lines(results []CheckResult) string {
	var b strings.Builder
	for _, r := range results {
		b.WriteString(r.Line() + "\n")
	}
	return b.String()
}

// otherChainAnchor is an anchor on a chain the --rpc does not serve: nothing at that correlation id.
func otherChainAnchor(version int, signer string) AnchorEntry {
	blk := int64(12345)
	return AnchorEntry{Version: version, Chain: "argus-dev-poa", ChainCorrelationID: "no-such-correlation-id", Block: &blk, Signer: signer}
}

// verdictAnchor is the fixture's own verdict (v3) entry, which reads back cleanly through svc.
func verdictAnchor(t *testing.T, doc *Document) AnchorEntry {
	t.Helper()
	for _, a := range doc.Anchors {
		if a.Version == 3 {
			return a
		}
	}
	t.Fatal("fixture has no version-3 anchor")
	return AnchorEntry{}
}

func TestAnchorVerify_UnreachableChainDoesNotFailWhenAVerdictAnchorVerifies(t *testing.T) {
	doc, svc, addr := outcomeFixture(t)
	doc.Anchors = []AnchorEntry{otherChainAnchor(2, addr), otherChainAnchor(4, addr), verdictAnchor(t, doc)}
	results := VerifyReveal(context.Background(), doc, chainIDSvc{svc, 84532}, nil)
	out := lines(results)
	t.Log("\n" + out)
	if !overallOK(results) {
		t.Fatalf("one verdict anchor verified and nothing mismatched, but the run fails:\n%s", out)
	}
	if !strings.Contains(out, "UNREACHABLE: anchor v2 argus-dev-poa") {
		t.Errorf("the other-chain anchor is not reported UNREACHABLE:\n%s", out)
	}
}

func TestAnchorVerify_NoRPCIsUnreachableNotFailure(t *testing.T) {
	doc, svc, _ := outcomeFixture(t)
	// same document, verified once with --rpc and once without: without it the verdict is UNREACHABLE and
	// the run is NOT verified (nothing confirmed), but no line is a mismatch.
	if !overallOK(VerifyReveal(context.Background(), doc, svc, nil)) {
		t.Fatal("the clean fixture must verify with --rpc")
	}
	results := VerifyReveal(context.Background(), doc, nil, nil)
	for _, r := range results {
		if r.Status == StatusMismatch {
			t.Errorf("no --rpc must be UNREACHABLE, got a mismatch: %s", r.Line())
		}
	}
	if overallOK(results) {
		t.Errorf("no verdict anchor was verified, yet the run passed:\n%s", lines(results))
	}
}

func TestAnchorVerify_PendingOrHeaderlessOTSDoesNotFailTheRun(t *testing.T) {
	doc, svc, _ := outcomeFixture(t)
	setP, _ := jsonMarshalSetV2(doc)
	digest := sha256.Sum256(setP)
	pending, _ := json.Marshal(map[string]any{"digest": digest[:], "height": 0})
	doc.Anchors = append(doc.Anchors, AnchorEntry{Version: 2, Chain: "bitcoin-ots", Receipt: pending})

	// (a) --btc-headers absent: UNREACHABLE.
	results := VerifyReveal(context.Background(), doc, svc, nil)
	out := lines(results)
	t.Log("\n" + out)
	if !overallOK(results) {
		t.Fatalf("an OTS receipt with no --btc-headers failed the run:\n%s", out)
	}
	if !strings.Contains(out, "UNREACHABLE: anchor v2 bitcoin-ots") {
		t.Errorf("headerless OTS receipt is not UNREACHABLE:\n%s", out)
	}
	// (b) headers given, receipt not yet attested: PENDING.
	results = VerifyReveal(context.Background(), doc, svc, fakeBTCHeaderSource{height: 1})
	out = lines(results)
	t.Log("\n" + out)
	if !overallOK(results) {
		t.Fatalf("a pending OTS receipt failed the run:\n%s", out)
	}
	if !strings.Contains(out, "PENDING: anchor v2 bitcoin-ots") {
		t.Errorf("pending OTS receipt is not PENDING:\n%s", out)
	}
}

func TestAnchorVerify_TamperedPayloadOnReachableChainStillFails(t *testing.T) {
	doc, svc, addr := outcomeFixture(t)
	doc.Anchors = append(doc.Anchors, otherChainAnchor(2, addr)) // an unreachable sibling changes nothing
	doc.Outcome = "failed"                                       // the chain committed "passed"
	results := VerifyReveal(context.Background(), doc, svc, nil)
	out := lines(results)
	t.Log("\n" + out)
	if overallOK(results) {
		t.Fatalf("a tampered verdict on the reachable chain passed:\n%s", out)
	}
	if !strings.Contains(out, "MISMATCH: anchor v3 argus-verify-test-chain") {
		t.Errorf("the tampered verdict anchor is not a MISMATCH:\n%s", out)
	}
}

func TestAnchorVerify_LocalChecksStayHardFailures(t *testing.T) {
	doc, svc, _ := outcomeFixture(t)
	doc.SealedFiles[0].Body = "TAMPERED"
	results := VerifyReveal(context.Background(), doc, svc, nil)
	if overallOK(results) {
		t.Fatalf("a tampered sealed file passed:\n%s", lines(results))
	}
	doc2, svc2, _ := outcomeFixture(t)
	doc2.EvidenceBundleHash = strings.Repeat("0", 64)
	if overallOK(VerifyReveal(context.Background(), doc2, svc2, nil)) {
		t.Fatal("a tampered evidence bundle hash passed")
	}
}

func TestAnchorVerify_ChainIDMismatchIsNamed(t *testing.T) {
	doc, svc, addr := outcomeFixture(t)
	doc.Anchors = []AnchorEntry{otherChainAnchor(2, addr), verdictAnchor(t, doc)}
	results := VerifyReveal(context.Background(), doc, chainIDSvc{svc, 84532}, nil)
	var detail string
	for _, r := range results {
		if r.Name == "anchor v2 argus-dev-poa" {
			detail = r.Detail
		}
	}
	t.Log(detail)
	for _, want := range []string{`anchor is on chain "argus-dev-poa"`, "--rpc serves chain id 84532", "use an --rpc endpoint for"} {
		if !strings.Contains(detail, want) {
			t.Errorf("chain-id message lacks %q: %s", want, detail)
		}
	}
	if strings.Contains(detail, "pruned history unavailable") && !strings.Contains(detail, "--rpc serves chain id 84532") {
		t.Errorf("the message is only the node's raw error: %s", detail)
	}
}
