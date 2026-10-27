package reveal

// verify_btcorder_test.go — end to end through the REAL header sources and the real
// anchor-verify path: a valid OTS receipt, and a header source that serves the block's merkle root in
// DISPLAY order as every real explorer / `bitcoin-cli getblockheader` does, must be VERIFIED; a source
// serving a DIFFERENT block's root is a MISMATCH (not UNREACHABLE) and fails the run.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/chainread/ots"
)

type btcSource interface {
	MerkleRootAt(ctx context.Context, height uint64) ([32]byte, error)
}

// realSource builds the named real header source serving `served` as height 700000's merkle root.
func realSource(t *testing.T, kind, served string) btcSource {
	t.Helper()
	switch kind {
	case "http":
		srv := explorerServer(700000, "00000000000000000009abcffabc0123456789abcdef0123456789abcdef01", served)
		t.Cleanup(srv.Close)
		return ots.HTTPBlockExplorerHeaders{BaseURL: srv.URL}
	case "file":
		p := filepath.Join(t.TempDir(), "headers.json")
		if err := os.WriteFile(p, []byte(`{"700000": "`+served+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return ots.FileHeaders{Path: p}
	}
	t.Fatalf("unknown source kind %q", kind)
	return nil
}

func TestAnchorVerify_RealHeaderSources_DisplayOrder(t *testing.T) {
	doc, verified, _, _ := otsFixture(t) // the receipt folds to the verdict digest, at height 700000
	doc.Anchors = []AnchorEntry{verified}
	var rc struct {
		Digest []byte `json:"digest"`
	}
	if err := json.Unmarshal(verified.Receipt, &rc); err != nil {
		t.Fatal(err)
	}
	var internal [32]byte
	copy(internal[:], rc.Digest)
	disp := reverse32(internal)
	display := hex.EncodeToString(disp[:])
	other := reverse32(sha256.Sum256([]byte("a different block's merkle root")))
	otherDisplay := hex.EncodeToString(other[:])

	for _, kind := range []string{"http", "file"} {
		t.Run(kind+"/matching block, display order -> VERIFIED", func(t *testing.T) {
			results := VerifyAnchors(context.Background(), doc, nil, realSource(t, kind, display))
			out := lines(results)
			t.Log("\n" + out)
			if got := statusOf(results, "anchor v3 bitcoin-ots"); got != StatusVerified {
				t.Fatalf("a valid receipt against its block's display-order root is %q, want VERIFIED:\n%s", got, out)
			}
			if !Overall(results) {
				t.Fatalf("Overall = false for a valid receipt:\n%s", out)
			}
		})
		t.Run(kind+"/different block's root -> MISMATCH", func(t *testing.T) {
			results := VerifyAnchors(context.Background(), doc, nil, realSource(t, kind, otherDisplay))
			out := lines(results)
			t.Log("\n" + out)
			if got := statusOf(results, "anchor v3 bitcoin-ots"); got != StatusMismatch {
				t.Fatalf("a receipt against a DIFFERENT block's root is %q, want MISMATCH (not UNREACHABLE):\n%s", got, out)
			}
			if Overall(results) {
				t.Fatalf("Overall = true with a contradicted receipt:\n%s", out)
			}
		})
	}
}
