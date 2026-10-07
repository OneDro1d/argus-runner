package certificate

// fakechain_test.go — an in-memory chain for the verifier's tests. It implements the read-only
// chainread.ChainService and records, for each transaction, the sender the test says signed it. It has no
// signing code: the verifier is tested against the BLOCK CONTENTS a chain would return, which is all
// chainread.ReadAnchor reads. (Recovering a sender from a real transaction signature is chainread.ReadBlock's
// job and is covered by its own test.)

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/OneDro1d/argus-runner/internal/chainread"
)

type fakeChain struct {
	mu     sync.Mutex
	blocks map[int64][]chainread.BlockTx
	next   int64
	n      int
}

func newFakeChain() *fakeChain { return &fakeChain{blocks: map[int64][]chainread.BlockTx{}, next: 1} }

func (c *fakeChain) ReadBlock(blockNumber int64) ([]chainread.BlockTx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	txs, ok := c.blocks[blockNumber]
	if !ok {
		return nil, fmt.Errorf("fakeChain: no block %d", blockNumber)
	}
	return append([]chainread.BlockTx(nil), txs...), nil
}

func (c *fakeChain) BlockNumber() (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return uint64(c.next - 1), nil
}

// anchor appends one hash-mode anchor signed (as far as the fake is concerned) by from, in a block of
// its own, and returns its ref and tx hash. prev links it to the previous version.
func (c *fakeChain) anchor(t *testing.T, from string, payload []byte, prev *chainread.AnchorRef) (chainread.AnchorRef, string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	sum := sha256.Sum256([]byte(fmt.Sprintf("fake-correlation-%d", c.n)))
	corr := hex.EncodeToString(sum[:16])
	db := chainread.Datablock{CorrelationID: corr, Start: 1, SchemaVersion: 2, Mode: "hash", Payload: json.RawMessage(payload)}
	if prev != nil {
		db.PreviousChunkBlockID = fmt.Sprintf("%d", prev.Block)
		db.PreviousCorrelationID = prev.CorrelationID
	}
	data, err := json.Marshal(db)
	if err != nil {
		t.Fatalf("marshal datablock: %v", err)
	}
	blk := c.next
	c.next++
	txSum := sha256.Sum256(data)
	txHash := "0x" + hex.EncodeToString(txSum[:])
	c.blocks[blk] = []chainread.BlockTx{{Index: 0, TxHash: txHash, From: from, Nonce: uint64(c.n), Data: data}}
	return chainread.AnchorRef{CorrelationID: corr, Block: uint64(blk)}, txHash
}

// signerAddress derives the EIP-55 address of a secp256k1 private key given as hex. Tests use it to get
// addresses that are valid but are not the fixture's signer.
func signerAddress(keyHex string) (string, error) {
	k, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		return "", err
	}
	return crypto.PubkeyToAddress(k.PublicKey).Hex(), nil
}
