package artifactmeasure

import (
	"encoding/json"
	"strings"
	"testing"
)

// The precondition results are part of the record the evidence bundle hash commits to.

const pmDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func pm(pre ...Precondition) Measurement {
	m := Evaluate(pmDigest, nil, Reading{Source: "k8s", Digests: []string{pmDigest}})
	m.Preconditions = pre
	return m
}

func onePre() Precondition {
	return Precondition{ID: "np-in", Kind: "networkpolicy.ingress_enforced", Verdict: "absent", Positive: "connected", Negative: "NOT refused: connected"}
}

// A measurement without preconditions marshals to the bytes it always did: no key, so its digest, its bundle
// hash and its certificate are unchanged.
func TestMeasurement_NoPreconditionsAddsNoKey_SoTheHashIsUnchanged(t *testing.T) {
	b, _ := json.Marshal(pm())
	if strings.Contains(string(b), "preconditions") {
		t.Fatalf("a measurement with no preconditions grew a key: %s", b)
	}
	if pm().Digest() != Evaluate(pmDigest, nil, Reading{Source: "k8s", Digests: []string{pmDigest}}).Digest() {
		t.Fatal("setting an empty list moved the digest")
	}
}

func TestMeasurement_EveryPreconditionFieldMovesTheDigestAndTheBundleHash(t *testing.T) {
	base := onePre()
	ref := pm(base)
	root := strings.Repeat("ab", 32)
	if ref.Digest() == pm().Digest() || BundleHash(root, &ref) == BundleHash(root, func() *Measurement { m := pm(); return &m }()) {
		t.Fatal("adding a precondition did not move the digest / bundle hash")
	}
	for name, edit := range map[string]func(p *Precondition){
		"id": func(p *Precondition) { p.ID = "x" }, "kind": func(p *Precondition) { p.Kind = "networkpolicy.egress_enforced" },
		"verdict": func(p *Precondition) { p.Verdict = "holds" }, "positive": func(p *Precondition) { p.Positive = "q" },
		"negative": func(p *Precondition) { p.Negative = "m" }, "residue": func(p *Precondition) { p.Residue = "r" },
	} {
		e := base
		edit(&e)
		m := pm(e)
		if m.Digest() == ref.Digest() {
			t.Errorf("editing %s does not move the digest", name)
		}
		if BundleHash(root, &m) == BundleHash(root, &ref) {
			t.Errorf("editing %s does not move the bundle hash", name)
		}
	}
}

func TestCheckPreconditions_Shape(t *testing.T) {
	good := onePre()
	if err := CheckPreconditions([]Precondition{good}); err != nil {
		t.Fatal(err)
	}
	if err := CheckPreconditions(nil); err != nil {
		t.Fatal("none is fine")
	}
	broken := good
	broken.Verdict = "broken" // a broken instrument is bindable: the certificate must say so
	if err := CheckPreconditions([]Precondition{broken}); err != nil {
		t.Fatalf("broken must be bindable: %v", err)
	}
	for name, mk := range map[string]func() []Precondition{
		"unknown kind":    func() []Precondition { p := good; p.Kind = "networkpolicy.nope"; return []Precondition{p} },
		"unknown verdict": func() []Precondition { p := good; p.Verdict = "maybe"; return []Precondition{p} },
		"empty positive":  func() []Precondition { p := good; p.Positive = ""; return []Precondition{p} },
		"empty negative":  func() []Precondition { p := good; p.Negative = ""; return []Precondition{p} },
		"empty id":        func() []Precondition { p := good; p.ID = ""; return []Precondition{p} },
		"duplicate ids":   func() []Precondition { return []Precondition{good, good} },
		"out of id order": func() []Precondition { q := good; q.ID = "a"; return []Precondition{good, q} },
	} {
		if err := CheckPreconditions(mk()); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// and Check (the one rule shared by the executor, the control plane and `certificate verify`) applies it
	m := pm(Precondition{ID: "x", Kind: "bogus", Verdict: "holds", Positive: "a", Negative: "b"})
	if err := Check(m, pmDigest); err == nil {
		t.Fatal("Check did not apply the precondition shape rule")
	}
}
