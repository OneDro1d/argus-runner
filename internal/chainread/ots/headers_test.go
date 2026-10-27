package ots

// btcheaders_test.go — AC-7: the HTTP implementation against a loopback httptest.Server (no real
// network) and the file implementation against a temp file.
//
// both sources take the merkle root in DISPLAY order — the order a block explorer's
// "merkle_root" and `bitcoin-cli getblockheader` print — and return it in the block header's INTERNAL
// order, which is what an OTS receipt folds to. Every fixture here therefore serves the root REVERSED
// relative to what is asserted, as a real explorer does; a fixture that serves the receipt's own order
// is the bug this file used to hide.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Real data: Bitcoin block 969288.
const (
	block969288Height   = 969288
	block969288Hash     = "000000000000000000000c1968d48eddeb5dbc890b676f1abf938f6befc34a57"
	block969288Display  = "b8100af53f7ae4f30d4fb59da3c0a016ae2f1c8e29f27e3e2d011b9e0d37b8d9" // what an explorer returns as "merkle_root"
	block969288Internal = "d9b8370d9e1b012d3e7ef2298e1c2fae16a0c0a39db54f0df3e47a3ff50a10b8" // what a valid OTS receipt folds to
)

// reverse32 returns b reversed — display order <-> internal order.
func reverse32(b [32]byte) [32]byte {
	var out [32]byte
	for i := range b {
		out[i] = b[len(b)-1-i]
	}
	return out
}

func mustRoot(t *testing.T, h string) [32]byte {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 32 {
		t.Fatalf("bad fixture hex %q: %v", h, err)
	}
	var out [32]byte
	copy(out[:], b)
	return out
}

// explorerServer is a loopback stand-in for blockstream.info/api: GET /block-height/<h> -> the block hash,
// GET /block/<hash> -> JSON whose "merkle_root" is served EXACTLY as given (display order for a real one).
func explorerServer(height uint64, hash, merkleRootAsServed string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/block-height/" + strconv.FormatUint(height, 10):
			w.Write([]byte(hash))
		case "/block/" + hash:
			json.NewEncoder(w).Encode(map[string]any{"id": hash, "height": height, "merkle_root": merkleRootAsServed})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestHTTPBlockExplorerHeaders_Block969288_DisplayOrderInInternalOrderOut(t *testing.T) {
	srv := explorerServer(block969288Height, block969288Hash, block969288Display)
	defer srv.Close()

	got, err := HTTPBlockExplorerHeaders{BaseURL: srv.URL}.MerkleRootAt(context.Background(), block969288Height)
	if err != nil {
		t.Fatalf("MerkleRootAt: %v", err)
	}
	if want := mustRoot(t, block969288Internal); got != want {
		t.Errorf("MerkleRootAt(969288) = %x, want %x (the explorer's display-order root, byte-reversed)", got, want)
	}
}

func TestFileHeaders_Block969288_DisplayOrderInInternalOrderOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "headers.json")
	if err := os.WriteFile(path, []byte(`{"969288": "`+block969288Display+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := FileHeaders{Path: path}.MerkleRootAt(context.Background(), block969288Height)
	if err != nil {
		t.Fatalf("MerkleRootAt: %v", err)
	}
	if want := mustRoot(t, block969288Internal); got != want {
		t.Errorf("MerkleRootAt(969288) = %x, want %x (the file's display-order root, byte-reversed)", got, want)
	}
}

func TestHTTPBlockExplorerHeaders_MerkleRootAt(t *testing.T) {
	var root [32]byte // the header's INTERNAL order, what the source must return
	for i := range root {
		root[i] = byte(i)
	}
	display := reverse32(root)
	const height = 700123
	const hash = "00000000000000000009abcffabc0123456789abcdef0123456789abcdef01"

	srv := explorerServer(height, hash, hex.EncodeToString(display[:]))
	defer srv.Close()

	got, err := HTTPBlockExplorerHeaders{BaseURL: srv.URL}.MerkleRootAt(context.Background(), height)
	if err != nil {
		t.Fatalf("MerkleRootAt: %v", err)
	}
	if got != root {
		t.Errorf("MerkleRootAt = %x, want %x", got, root)
	}
}

func TestHTTPBlockExplorerHeaders_UnknownHeight_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	if _, err := (HTTPBlockExplorerHeaders{BaseURL: srv.URL}).MerkleRootAt(context.Background(), 1); err == nil {
		t.Fatal("MerkleRootAt against a 404 succeeded; want an error")
	}
}

func TestFileHeaders_MerkleRootAt(t *testing.T) {
	var root [32]byte // INTERNAL order, what the source must return
	for i := range root {
		root[i] = byte(255 - i)
	}
	display := reverse32(root)
	dir := t.TempDir()
	path := filepath.Join(dir, "headers.json")
	body, _ := json.Marshal(map[string]string{"700123": hex.EncodeToString(display[:])})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := FileHeaders{Path: path}.MerkleRootAt(context.Background(), 700123)
	if err != nil {
		t.Fatalf("MerkleRootAt: %v", err)
	}
	if got != root {
		t.Errorf("MerkleRootAt = %x, want %x", got, root)
	}
}

func TestFileHeaders_UnknownHeight_Errors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "headers.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileHeaders{Path: path}).MerkleRootAt(context.Background(), 1); err == nil {
		t.Fatal("MerkleRootAt for an absent height succeeded; want an error")
	}
}
