package ots

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// headers.go — the HeaderSource implementations behind --btc-headers:
// an HTTP implementation against a public block-explorer API
// (a common shape several such APIs share: GET {base}/block-height/{height} -> the block hash as plain
// text; GET {base}/block/{hash} -> JSON carrying "merkle_root") and a file implementation for offline
// use (a JSON object mapping decimal height strings to hex merkle roots). This package's own test suite
// reaches them only through a loopback httptest.Server or a temp file — no real network.
//
// BYTE ORDER. Bitcoin hashes have two orders. Inside the block header, and in what an
// OpenTimestamps receipt folds to, the merkle root is in INTERNAL order. Every block explorer
// (blockstream.info, mempool.space) and `bitcoin-cli getblockheader` print it in DISPLAY order, the
// byte-reverse. The HeaderSource contract is the INTERNAL order and compares byte for byte,
// so BOTH sources here take the root in DISPLAY order — exactly as a human copies it from an explorer —
// and reverse it before returning it. Example, block 969288: the explorer prints
// b8100af53f7ae4f30d4fb59da3c0a016ae2f1c8e29f27e3e2d011b9e0d37b8d9, and a valid receipt folds to
// d9b8370d9e1b012d3e7ef2298e1c2fae16a0c0a39db54f0df3e47a3ff50a10b8. A file in the receipt's own order
// therefore reads as a different block's root: a MISMATCH, not a pass.

// HTTPBlockExplorerHeaders resolves a Bitcoin block height to its transaction merkle root through a
// public block-explorer's HTTP API, named by BaseURL — the caller's own choice, never hardcoded, so
// an offline or air-gapped verify never has to reach one.
type HTTPBlockExplorerHeaders struct {
	BaseURL string
	Client  *http.Client
}

func (h HTTPBlockExplorerHeaders) MerkleRootAt(ctx context.Context, height uint64) ([32]byte, error) {
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	base := strings.TrimRight(h.BaseURL, "/")

	hash, err := httpGetString(ctx, client, base+"/block-height/"+strconv.FormatUint(height, 10))
	if err != nil {
		return [32]byte{}, fmt.Errorf("btc-headers: block hash at height %d: %w", height, err)
	}
	hash = strings.TrimSpace(hash)

	body, err := httpGetString(ctx, client, base+"/block/"+hash)
	if err != nil {
		return [32]byte{}, fmt.Errorf("btc-headers: block %s: %w", hash, err)
	}
	var parsed struct {
		MerkleRoot string `json:"merkle_root"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return [32]byte{}, fmt.Errorf("btc-headers: parse block %s: %w", hash, err)
	}
	return decodeDisplayRoot(parsed.MerkleRoot)
}

func httpGetString(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// FileHeaders resolves a Bitcoin block height to its transaction merkle root from a local JSON file —
// {"<height>": "<hex merkle root>", ...} — for a verify run with no network at all. The root is in
// DISPLAY order, as a block explorer or `bitcoin-cli getblockheader` prints it (see the file comment):
//
//	{"969288": "b8100af53f7ae4f30d4fb59da3c0a016ae2f1c8e29f27e3e2d011b9e0d37b8d9"}
//

type FileHeaders struct {
	Path string
}

func (f FileHeaders) MerkleRootAt(ctx context.Context, height uint64) ([32]byte, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return [32]byte{}, fmt.Errorf("btc-headers: read %s: %w", f.Path, err)
	}
	var byHeight map[string]string
	if err := json.Unmarshal(data, &byHeight); err != nil {
		return [32]byte{}, fmt.Errorf("btc-headers: parse %s: %w", f.Path, err)
	}
	hexRoot, ok := byHeight[strconv.FormatUint(height, 10)]
	if !ok {
		return [32]byte{}, fmt.Errorf("btc-headers: %s carries no entry for height %d", f.Path, height)
	}
	return decodeDisplayRoot(hexRoot)
}

// decodeDisplayRoot decodes a merkle root written in DISPLAY order and returns it in the block header's
// INTERNAL order (the byte-reverse), the order an OTS receipt folds to.
func decodeDisplayRoot(hexRoot string) ([32]byte, error) {
	b, err := decodeRoot(hexRoot)
	if err != nil {
		return b, err
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return b, nil
}

func decodeRoot(hexRoot string) ([32]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(hexRoot))
	if err != nil {
		return [32]byte{}, fmt.Errorf("btc-headers: merkle root %q is not valid hex: %w", hexRoot, err)
	}
	if len(b) != 32 {
		return [32]byte{}, fmt.Errorf("btc-headers: merkle root %q is %d bytes, want 32", hexRoot, len(b))
	}
	var out [32]byte
	copy(out[:], b)
	return out, nil
}
