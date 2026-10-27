package reveal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
)

// reverse32 returns b reversed — display order <-> internal order.
func reverse32(b [32]byte) [32]byte {
	var out [32]byte
	for i := range b {
		out[i] = b[len(b)-1-i]
	}
	return out
}

// explorerServer is a loopback stand-in for a block explorer API: GET /block-height/<h> -> the block hash,
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
