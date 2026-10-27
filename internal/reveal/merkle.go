package reveal

// merkle.go — the checking half of the holdout Merkle tree: recompute a root from the sealed files in a
// reveal document, and check one file's inclusion proof against a declared root. The leaves are
// sha256(len(path)||0||path||len(body)||0||body||0) in path-ascending order, and a node is
// sha256(left||right); an odd level pairs its last node with itself. Pure functions with no I/O.
//
// Only the CHECK lives here. Building a proof for a leaf is done by whoever seals the set, not by a
// verifier.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
)

// MerkleLeaf is one scenario's (path, body) pair, the tree's input unit.
type MerkleLeaf struct {
	Path string
	Body string
}

// ProofStep is one sibling on the path from a leaf to the root, plus which side it sits on (relative to
// the node under proof AT THAT LEVEL): Left=true means the sibling is the LEFT child (the node under
// proof is the right one), so the check knows which order to re-hash the pair in. The Go field names are
// the wire keys of the reveal document.
type ProofStep struct {
	Sibling string // hex sha256
	Left    bool
}

func leafHash(path, body string) [32]byte {
	h := sha256.New()
	h.Write([]byte(strconv.Itoa(len(path))))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte(strconv.Itoa(len(body))))
	h.Write([]byte{0})
	h.Write([]byte(body))
	h.Write([]byte{0})
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func parentHash(l, r [32]byte) [32]byte {
	h := sha256.New()
	h.Write(l[:])
	h.Write(r[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// MerkleRoot recomputes the root over leaves IN THE ORDER GIVEN, as a hex sha256 digest. An empty input is
// refused: a set with no scenarios has no root.
func MerkleRoot(leaves []MerkleLeaf) (string, error) {
	if len(leaves) == 0 {
		return "", errors.New("merkle: cannot build a tree over zero leaves")
	}
	level := make([][32]byte, len(leaves))
	for i, lf := range leaves {
		level[i] = leafHash(lf.Path, lf.Body)
	}
	for len(level) > 1 {
		padded := level
		if len(padded)%2 == 1 {
			padded = make([][32]byte, len(level)+1)
			copy(padded, level)
			padded[len(level)] = level[len(level)-1] // duplicate the last node on an odd level
		}
		next := make([][32]byte, len(padded)/2)
		for i := range next {
			next[i] = parentHash(padded[2*i], padded[2*i+1])
		}
		level = next
	}
	return hex.EncodeToString(level[0][:]), nil
}

// VerifyInclusion recomputes the root from (path, body, proof) alone and reports whether it matches root:
// a party who never saw the rest of the set can confirm ONE scenario was part of the committed one. Any
// change to path or body — or a tampered proof step — changes the recomputed root, so it fails closed.
func VerifyInclusion(root, path, body string, proof []ProofStep) bool {
	cur := leafHash(path, body)
	for _, step := range proof {
		sibBytes, err := hex.DecodeString(step.Sibling)
		if err != nil || len(sibBytes) != 32 {
			return false
		}
		var sib [32]byte
		copy(sib[:], sibBytes)
		if step.Left {
			cur = parentHash(sib, cur)
		} else {
			cur = parentHash(cur, sib)
		}
	}
	return hex.EncodeToString(cur[:]) == root
}
