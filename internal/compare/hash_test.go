package compare

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type hashVector struct {
	Name         string `json:"name"`
	StatusBytes  string `json:"status_bytes"`
	HeadersBytes string `json:"headers_bytes"`
	BodyBytes    string `json:"body_bytes"`
	Selected     struct {
		Status, Headers, Body bool
	} `json:"selected"`
	StatusPart  string `json:"status_part"`
	HeadersPart string `json:"headers_part"`
	BodyPart    string `json:"body_part"`
	Hash        string `json:"hash"`
}

// testdata/hash_vectors.json was produced by sandbox script vectors.py (Python hashlib), not by this
// package: the algorithm is pinned by an independent implementation of design section 1.2.6.
func TestHashVectors(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "hash_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vs []hashVector
	if err := json.Unmarshal(b, &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) < 6 {
		t.Fatalf("only %d vectors", len(vs))
	}
	for _, v := range vs {
		t.Run(v.Name, func(t *testing.T) {
			var s, h, bd string
			if v.Selected.Status {
				s = PartHash(DomainStatus, []byte(v.StatusBytes))
			}
			if v.Selected.Headers {
				h = PartHash(DomainHeaders, []byte(v.HeadersBytes))
			}
			if v.Selected.Body {
				bd = PartHash(DomainBody, []byte(v.BodyBytes))
			}
			if s != v.StatusPart || h != v.HeadersPart || bd != v.BodyPart {
				t.Errorf("parts = (%s, %s, %s), want (%s, %s, %s)", s, h, bd, v.StatusPart, v.HeadersPart, v.BodyPart)
			}
			if got := OutputHash(s, h, bd); got != v.Hash {
				t.Errorf("OutputHash = %s, want %s", got, v.Hash)
			}
		})
	}
}

func TestDomainStringsArePinned(t *testing.T) {
	if DomainStatus != "argus-output/1/status" || DomainHeaders != "argus-output/1/headers" || DomainBody != "argus-output/1/body" {
		t.Fatalf("domains drifted: %q %q %q", DomainStatus, DomainHeaders, DomainBody)
	}
}

func TestEachPartHasItsOwnDomain(t *testing.T) {
	same := []byte("200")
	s, h, b := PartHash(DomainStatus, same), PartHash(DomainHeaders, same), PartHash(DomainBody, same)
	if s == h || h == b || s == b {
		t.Fatal("identical bytes in different parts must not hash alike")
	}
}

func TestOutputHashIsPositional(t *testing.T) {
	x, y := PartHash(DomainBody, []byte("x")), PartHash(DomainBody, []byte("y"))
	if OutputHash(x, y, "") == OutputHash(y, x, "") || OutputHash(x, "", y) == OutputHash("", x, y) {
		t.Fatal("moving a part hash to another position must change the hash")
	}
}
