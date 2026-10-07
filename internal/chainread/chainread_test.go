package chainread

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

const (
	signerA = "0x1111111111111111111111111111111111111111"
	signerB = "0x2222222222222222222222222222222222222222"
	corrID  = "0123456789abcdef0123456789abcdef"
)

// memChain is a read-only ChainService over canned blocks.
type memChain map[int64][]BlockTx

func (m memChain) ReadBlock(n int64) ([]BlockTx, error) {
	txs, ok := m[n]
	if !ok {
		return nil, fmt.Errorf("no block %d", n)
	}
	return txs, nil
}
func (m memChain) BlockNumber() (uint64, error) { return uint64(len(m)), nil }

func anchorTx(t *testing.T, from string, db Datablock) BlockTx {
	t.Helper()
	b, err := json.Marshal(db)
	if err != nil {
		t.Fatal(err)
	}
	return BlockTx{TxHash: "0xabc", From: from, Data: b}
}

func TestReadAnchor_ReturnsPayloadByteExactAndSigner(t *testing.T) {
	payload := `{"run_id":"r1","verdict":"passed"}`
	c := memChain{5: {anchorTx(t, signerA, Datablock{CorrelationID: corrID, Mode: "hash", Payload: json.RawMessage(payload)})}}
	rec, err := ReadAnchor(c, AnchorRef{CorrelationID: corrID, Block: 5})
	if err != nil {
		t.Fatalf("ReadAnchor: %v", err)
	}
	if string(rec.Payload) != payload || rec.From != signerA || rec.Previous != nil {
		t.Fatalf("got %+v", rec)
	}
}

func TestReadAnchor_LinksThePreviousVersion(t *testing.T) {
	c := memChain{9: {anchorTx(t, signerA, Datablock{CorrelationID: corrID, Mode: "hash", Payload: json.RawMessage(`{}`),
		PreviousChunkBlockID: "4", PreviousCorrelationID: "ffffffffffffffffffffffffffffffff"})}}
	rec, err := ReadAnchor(c, AnchorRef{CorrelationID: corrID, Block: 9})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Previous == nil || rec.Previous.Block != 4 || rec.Previous.CorrelationID != "ffffffffffffffffffffffffffffffff" {
		t.Fatalf("previous = %+v", rec.Previous)
	}
}

func TestReadAnchor_AllowListFailsClosed(t *testing.T) {
	c := memChain{5: {anchorTx(t, signerB, Datablock{CorrelationID: corrID, Mode: "hash", Payload: json.RawMessage(`{}`)})}}
	if _, err := ReadAnchor(c, AnchorRef{CorrelationID: corrID, Block: 5}, WithAllowedSigners(signerA)); !errors.Is(err, ErrSignerNotAllowed) {
		t.Fatalf("signer B under an allow-list of A: err = %v, want ErrSignerNotAllowed", err)
	}
	if _, err := ReadAnchor(c, AnchorRef{CorrelationID: corrID, Block: 5}, WithAllowedSigners(signerB)); err != nil {
		t.Fatalf("signer B under an allow-list of B: %v", err)
	}
	// case-insensitive
	if _, err := ReadAnchor(c, AnchorRef{CorrelationID: corrID, Block: 5}, WithAllowedSigners("0x2222222222222222222222222222222222222222")); err != nil {
		t.Fatal(err)
	}
}

func TestReadAnchor_EmptyAllowListIsRefusedNotIgnored(t *testing.T) {
	c := memChain{5: {}}
	if _, err := ReadAnchor(c, AnchorRef{CorrelationID: corrID, Block: 5}, WithAllowedSigners()); !errors.Is(err, ErrEmptyAllowlist) {
		t.Fatalf("err = %v, want ErrEmptyAllowlist", err)
	}
}

func TestReadAnchor_MalformedSignerAddressIsRefused(t *testing.T) {
	c := memChain{5: {}}
	if _, err := ReadAnchor(c, AnchorRef{CorrelationID: corrID, Block: 5}, WithAllowedSigners("not-an-address")); err == nil {
		t.Fatal("a malformed allowed-signer address was accepted")
	}
}

func TestReadAnchor_FullModeChunkIsNotAnAnchor(t *testing.T) {
	c := memChain{5: {anchorTx(t, signerA, Datablock{CorrelationID: corrID, ChunkData: "x"})}}
	if _, err := ReadAnchor(c, AnchorRef{CorrelationID: corrID, Block: 5}); !errors.Is(err, ErrModeMismatch) {
		t.Fatalf("err = %v, want ErrModeMismatch", err)
	}
}

func TestReadAnchor_AbsentCorrelationIDIsAnError(t *testing.T) {
	c := memChain{5: {anchorTx(t, signerA, Datablock{CorrelationID: "other", Mode: "hash", Payload: json.RawMessage(`{}`)})}}
	if _, err := ReadAnchor(c, AnchorRef{CorrelationID: corrID, Block: 5}); err == nil {
		t.Fatal("an anchor with a different correlation id was returned")
	}
}

func TestReadAnchor_ChainReadErrorIsReturned(t *testing.T) {
	if _, err := ReadAnchor(memChain{}, AnchorRef{CorrelationID: corrID, Block: 5}); err == nil {
		t.Fatal("a failed block read was swallowed")
	}
}
