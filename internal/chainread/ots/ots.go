// Package ots is the READ-ONLY half of an OpenTimestamps subset: it checks that a receipt commits to
// sha256(payload) and that the receipt's operations (append, prepend, sha256) fold to the merkle root of
// the Bitcoin block it names. It never contacts a calendar server and never submits anything.
//
// The Bitcoin header comes from a HeaderSource the caller chooses; see HTTPBlockExplorerHeaders and
// FileHeaders.
package ots

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/OneDro1d/argus-runner/internal/chainread"
)

// HeaderSource resolves a Bitcoin block height to that block's transaction merkle root, in the block
// header's INTERNAL byte order, the value an upgraded receipt's operations must reduce to.
type HeaderSource interface {
	MerkleRootAt(ctx context.Context, height uint64) ([32]byte, error)
}

// Provider verifies receipts against a HeaderSource.
type Provider struct {
	hdr  HeaderSource
	name string
}

// New constructs a Provider identified by name (the chain's name in the certificate).
func New(hdr HeaderSource, name string) *Provider {
	return &Provider{hdr: hdr, name: name}
}

// receipt is the minimal OTS-style commitment carried as Ref.Receipt: the stamped digest, the
// append/prepend/sha256 operations chaining it toward a Bitcoin block's merkle root, and that block's
// height once attested (0 = pending: the calendar has not yet included the digest in a mined block).
type receipt struct {
	Digest   [32]byte `json:"digest"`
	Ops      []op     `json:"ops,omitempty"`
	Height   uint64   `json:"height,omitempty"`
	Calendar string   `json:"calendar,omitempty"`
}

type opKind uint8

const (
	opAppend opKind = iota
	opPrepend
	opSHA256
)

type op struct {
	Kind opKind `json:"kind"`
	Data []byte `json:"data,omitempty"`
}

// UnmarshalJSON refuses a digest that does not decode to exactly 32 bytes. encoding/json unmarshals a
// short JSON array into a fixed-size [32]byte array by zero-filling the remainder, with no error, which
// would silently turn a 31-byte digest into a different, wrong one.
func (r *receipt) UnmarshalJSON(data []byte) error {
	var aux struct {
		Digest   []byte `json:"digest"`
		Ops      []op   `json:"ops,omitempty"`
		Height   uint64 `json:"height,omitempty"`
		Calendar string `json:"calendar,omitempty"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(aux.Digest) != 32 {
		return fmt.Errorf("ots: receipt digest is %d bytes, want exactly 32", len(aux.Digest))
	}
	copy(r.Digest[:], aux.Digest)
	r.Ops = aux.Ops
	r.Height = aux.Height
	r.Calendar = aux.Calendar
	return nil
}

// commitment applies Ops to Digest, in order, to the value the attested block's merkle root must equal
// for the receipt to be confirmed. The result is compared byte-for-byte against the root by the caller,
// never padded or truncated to fit.
func (r receipt) commitment() []byte {
	cur := append([]byte(nil), r.Digest[:]...)
	for _, o := range r.Ops {
		switch o.Kind {
		case opAppend:
			cur = append(cur, o.Data...)
		case opPrepend:
			cur = append(append([]byte(nil), o.Data...), cur...)
		case opSHA256:
			sum := sha256.Sum256(cur)
			cur = sum[:]
		}
	}
	return cur
}

// Verify decodes ref.Receipt and checks it commits to sha256(payload) (chainread.ErrMismatch
// otherwise). An unattested receipt (Height == 0) reports Proof.Pending; an attested one is confirmed
// against HeaderSource.MerkleRootAt(Height).
//
// Every way the receipt itself contradicts the evidence wraps chainread.ErrMismatch: a different
// digest, an unparseable receipt, and a fold that does not equal the block's merkle root. A
// HeaderSource failure does not: that is "could not check", not a contradiction.
func (p *Provider) Verify(ctx context.Context, ref chainread.Ref, payload []byte, _ ...chainread.Option) (chainread.Proof, error) {
	var r receipt
	if err := json.Unmarshal(ref.Receipt, &r); err != nil {
		return chainread.Proof{}, fmt.Errorf("ots: invalid receipt: %v: %w", err, chainread.ErrMismatch)
	}

	digest := sha256.Sum256(payload)
	if r.Digest != digest {
		return chainread.Proof{}, chainread.ErrMismatch
	}

	if r.Height == 0 {
		return chainread.Proof{Ref: ref, Pending: true}, nil
	}

	root, err := p.hdr.MerkleRootAt(ctx, r.Height)
	if err != nil {
		return chainread.Proof{}, fmt.Errorf("ots: header source at height %d: %v", r.Height, err)
	}
	got := r.commitment()
	if !bytes.Equal(got, root[:]) {
		return chainread.Proof{}, fmt.Errorf("ots: receipt does not attest to the block %d merkle root: %w", r.Height, chainread.ErrMismatch)
	}

	return chainread.Proof{Ref: ref, Pending: false}, nil
}
