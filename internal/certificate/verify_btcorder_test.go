package certificate_test

// verify_btcorder_test.go —, end to end through `certificate.Verify` with the REAL header
// sources `argus certificate verify --btc-headers` builds (ots.HTTPBlockExplorerHeaders / FileHeaders).
// It is an external test package because internal/reveal imports internal/certificate. A valid OTS
// receipt and a source serving the block's merkle root in DISPLAY order (as every explorer and
// `bitcoin-cli getblockheader` does) is VERIFIED; a source serving a DIFFERENT block's root is a MISMATCH
// (not UNREACHABLE) and fails the run.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/certificate"
	"github.com/OneDro1d/argus-runner/internal/chainread/ots"
)

func reversed(b [32]byte) string {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return hex.EncodeToString(b[:])
}

type headerSource interface {
	MerkleRootAt(ctx context.Context, height uint64) ([32]byte, error)
}

func realHeaderSource(t *testing.T, kind, served string) headerSource {
	t.Helper()
	const hash = "00000000000000000009abcffabc0123456789abcdef0123456789abcdef01"
	switch kind {
	case "http":
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/block-height/700000":
				w.Write([]byte(hash))
			case "/block/" + hash:
				json.NewEncoder(w).Encode(map[string]any{"id": hash, "height": 700000, "merkle_root": served})
			default:
				http.NotFound(w, r)
			}
		}))
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

// btcOrderCert is a v1 certificate whose one verdict OTS anchor's receipt folds to the sha256 of the
// verdict payload at height 700000; digest is that fold (the header's INTERNAL order).
func btcOrderCert(t *testing.T) (cert *certificate.Certificate, digest [32]byte) {
	t.Helper()
	cert = &certificate.Certificate{
		Format: certificate.FormatV1, RunID: "run-btc-order", Verdict: "passed",
		Tallies:       certificate.Tallies{Passed: 1},
		Set:           certificate.Set{HoldoutRoot: "root", CriteriaHash: "crit", SetHash: "sh", ScenarioCount: 1},
		VerdictRecord: certificate.VerdictRecord{ArtifactDigest: "sha256:aa", EvidenceBundleHash: "sha256:bb"},
	}
	payload, err := json.Marshal(certificate.VerdictV2{RunID: cert.RunID, ArtifactDigest: "sha256:aa", EvidenceBundleHash: "sha256:bb", Verdict: cert.Verdict})
	if err != nil {
		t.Fatal(err)
	}
	digest = sha256.Sum256(payload)
	receipt, _ := json.Marshal(map[string]any{"digest": digest[:], "height": 700000})
	cert.Anchors = []certificate.Anchor{{Version: 3, Chain: "bitcoin-ots", Receipt: receipt, Signer: "s1"}}
	return cert, digest
}

func TestCertificateVerify_RealHeaderSources_DisplayOrder(t *testing.T) {
	allow := certificate.AllowList{"bitcoin-ots": {"s1"}}
	cert, digest := btcOrderCert(t)
	display := reversed(digest)
	otherDisplay := reversed(sha256.Sum256([]byte("a different block's merkle root")))

	for _, kind := range []string{"http", "file"} {
		t.Run(kind+"/matching block, display order -> VERIFIED", func(t *testing.T) {
			results := certificate.Verify(context.Background(), cert, nil, realHeaderSource(t, kind, display), allow)
			for _, r := range results {
				t.Log(r.Line())
			}
			var got certificate.Status
			for _, r := range results {
				if r.Name == "anchor v3 bitcoin-ots" {
					got = r.Status
				}
			}
			if got != certificate.StatusVerified {
				t.Fatalf("a valid receipt against its block's display-order root is %q, want VERIFIED", got)
			}
			if !certificate.Overall(cert, results) {
				t.Fatalf("Overall = false for a valid receipt")
			}
		})
		t.Run(kind+"/different block's root -> MISMATCH", func(t *testing.T) {
			results := certificate.Verify(context.Background(), cert, nil, realHeaderSource(t, kind, otherDisplay), allow)
			for _, r := range results {
				t.Log(r.Line())
			}
			var got certificate.Status
			for _, r := range results {
				if r.Name == "anchor v3 bitcoin-ots" {
					got = r.Status
				}
			}
			if got != certificate.StatusMismatch {
				t.Fatalf("a receipt against a DIFFERENT block's root is %q, want MISMATCH (not UNREACHABLE)", got)
			}
			if certificate.Overall(cert, results) {
				t.Fatalf("Overall = true with a contradicted receipt")
			}
		})
	}
}
