package ots

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/chainread"
)

type fixedRoot struct {
	height uint64
	root   [32]byte
}

func (f fixedRoot) MerkleRootAt(_ context.Context, h uint64) ([32]byte, error) {
	if h != f.height {
		return [32]byte{}, errors.New("no such height")
	}
	return f.root, nil
}

func receiptJSON(t *testing.T, v map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVerify_FoldsOpsToTheBlockRoot(t *testing.T) {
	payload := []byte(`{"run_id":"r1"}`)
	d := sha256.Sum256(payload)
	// ops: append 0x01, then sha256
	folded := sha256.Sum256(append(append([]byte(nil), d[:]...), 0x01))
	rc := receiptJSON(t, map[string]any{"digest": d[:], "height": 7, "ops": []map[string]any{{"kind": 0, "data": []byte{0x01}}, {"kind": 2}}})
	p := New(fixedRoot{height: 7, root: folded}, "bitcoin-ots")
	proof, err := p.Verify(context.Background(), chainread.Ref{Chain: "bitcoin-ots", Receipt: rc}, payload)
	if err != nil || proof.Pending {
		t.Fatalf("proof=%+v err=%v, want confirmed", proof, err)
	}
	// a different root is a mismatch
	bad := New(fixedRoot{height: 7, root: [32]byte{1}}, "bitcoin-ots")
	if _, err := bad.Verify(context.Background(), chainread.Ref{Receipt: rc}, payload); !errors.Is(err, chainread.ErrMismatch) {
		t.Fatalf("err = %v, want ErrMismatch", err)
	}
}

func TestVerify_UnattestedReceiptIsPending(t *testing.T) {
	payload := []byte(`{}`)
	d := sha256.Sum256(payload)
	rc := receiptJSON(t, map[string]any{"digest": d[:]})
	proof, err := New(fixedRoot{}, "x").Verify(context.Background(), chainread.Ref{Receipt: rc}, payload)
	if err != nil || !proof.Pending {
		t.Fatalf("proof=%+v err=%v, want pending", proof, err)
	}
}

func TestVerify_WrongDigestAndShortDigestAreMismatches(t *testing.T) {
	d := sha256.Sum256([]byte("other"))
	rc := receiptJSON(t, map[string]any{"digest": d[:], "height": 1})
	if _, err := New(fixedRoot{height: 1}, "x").Verify(context.Background(), chainread.Ref{Receipt: rc}, []byte("payload")); !errors.Is(err, chainread.ErrMismatch) {
		t.Fatalf("wrong digest: err = %v", err)
	}
	short := receiptJSON(t, map[string]any{"digest": d[:31], "height": 1})
	if _, err := New(fixedRoot{height: 1}, "x").Verify(context.Background(), chainread.Ref{Receipt: short}, []byte("payload")); !errors.Is(err, chainread.ErrMismatch) {
		t.Fatalf("31-byte digest: err = %v, want ErrMismatch (a short digest must not be zero-filled into a different one)", err)
	}
}

func TestVerify_HeaderSourceFailureIsNotAMismatch(t *testing.T) {
	payload := []byte(`{}`)
	d := sha256.Sum256(payload)
	rc := receiptJSON(t, map[string]any{"digest": d[:], "height": 9})
	_, err := New(fixedRoot{height: 1}, "x").Verify(context.Background(), chainread.Ref{Receipt: rc}, payload)
	if err == nil || errors.Is(err, chainread.ErrMismatch) {
		t.Fatalf("err = %v, want a could-not-check error that is not ErrMismatch", err)
	}
}
