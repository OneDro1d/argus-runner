package artifactmeasure

import (
	"errors"
	"strings"
	"testing"
)

const (
	dA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	dC = "sha256:" + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestEvaluate_DeclaredAmongRunning_IsMatched(t *testing.T) {
	m := Evaluate(dA, nil, Reading{Source: "k8s", Digests: []string{dB, dA, dA}})
	if m.State != StateMatched || m.Reason != "" {
		t.Fatalf("state %q reason %q, want matched", m.State, m.Reason)
	}
	if strings.Join(m.Running, ",") != dA+","+dB {
		t.Fatalf("running must be sorted and unique, got %v", m.Running)
	}
	if err := Check(m, dA); err != nil {
		t.Fatalf("a matched measurement must pass Check: %v", err)
	}
}

func TestEvaluate_DeclaredIsCompared_CaseInsensitively(t *testing.T) {
	m := Evaluate(strings.ToUpper(dA[:7])+dA[7:], nil, Reading{Source: "k8s", Digests: []string{dA}})
	if m.State != StateMatched {
		t.Fatalf("digest case must not decide the outcome, got %q (%s)", m.State, m.Reason)
	}
}

func TestEvaluate_DeclaredNotRunning_AllResolved_IsAProvenMismatch(t *testing.T) {
	m := Evaluate(dC, nil, Reading{Source: "k8s", Digests: []string{dA, dB}})
	if m.State != StateMismatch {
		t.Fatalf("state %q, want mismatch", m.State)
	}
	err := RefuseIfMismatch(m)
	var me *MismatchError
	if !errors.As(err, &me) {
		t.Fatalf("RefuseIfMismatch = %v, want *MismatchError", err)
	}
	for _, want := range []string{dC, dA, dB} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name both the declared and the measured digests; %q missing from: %s", want, err)
		}
	}
	if Check(m, dC) == nil {
		t.Fatal("a mismatch must never pass Check: it is not bindable")
	}
}

// A container that reported no digest could be the declared one, so a mismatch is not PROVEN.
func TestEvaluate_UnresolvedContainer_MeansNotMeasured_NeverMismatch_NeverMatched(t *testing.T) {
	m := Evaluate(dC, nil, Reading{Source: "k8s", Digests: []string{dA}, Unresolved: 2})
	if m.State != StateNotMeasured {
		t.Fatalf("state %q, want not_measured", m.State)
	}
	if !strings.Contains(m.Reason, "2 container") {
		t.Errorf("reason must say how many containers gave no digest: %q", m.Reason)
	}
	if RefuseIfMismatch(m) != nil {
		t.Fatal("an unproven mismatch must not refuse the run")
	}
	if err := Check(m, dC); err != nil {
		t.Fatalf("an honest not_measured record must be bindable: %v", err)
	}
	// but if the declared digest IS running, an unresolved sidecar does not stop it matching
	m2 := Evaluate(dA, nil, Reading{Source: "k8s", Digests: []string{dA}, Unresolved: 1})
	if m2.State != StateMatched {
		t.Fatalf("declared is running and unresolved=1: state %q, want matched", m2.State)
	}
}

func TestEvaluate_NothingMeasured_IsNotMeasuredWithTheReason_NeverMatched(t *testing.T) {
	for name, r := range map[string]Reading{
		"reason given":     {Source: "k8s", Reason: "forbidden: pods in namespace app"},
		"no digests":       {Source: "k8s"},
		"empty everything": {},
	} {
		m := Evaluate(dA, nil, r)
		if m.State != StateNotMeasured {
			t.Errorf("%s: state %q, want not_measured", name, m.State)
		}
		if m.Reason == "" {
			t.Errorf("%s: a not_measured record must say why", name)
		}
		if len(m.Running) != 0 {
			t.Errorf("%s: running %v, want none", name, m.Running)
		}
	}
	m := Evaluate(dA, nil, Reading{Source: "k8s", Reason: "forbidden: pods in namespace app"})
	if !strings.Contains(m.Reason, "forbidden") {
		t.Errorf("the reading's own reason must be kept: %q", m.Reason)
	}
}

func TestEvaluate_NoDeclaredDigest_NeverMatches(t *testing.T) {
	m := Evaluate("", nil, Reading{Source: "k8s", Digests: []string{dA}})
	if m.State != StateNotMeasured || !strings.Contains(m.Reason, "no artifact digest") {
		t.Fatalf("state %q reason %q, want not_measured naming the missing declared digest", m.State, m.Reason)
	}
}

func TestEvaluate_RecordsWhichCommitmentImagesWereFoundRunning(t *testing.T) {
	m := Evaluate(dA, []string{dB, dC, strings.ToUpper(dB[:7]) + dB[7:]}, Reading{Source: "k8s", Digests: []string{dA, dB}})
	if strings.Join(m.CommitmentFound, ",") != dB || strings.Join(m.CommitmentNotFound, ",") != dC {
		t.Fatalf("found %v not-found %v, want found [B] not-found [C]", m.CommitmentFound, m.CommitmentNotFound)
	}
	// a commitment image that is not running does NOT refuse the run: it is recorded, not enforced.
	if m.State != StateMatched {
		t.Fatalf("state %q: commitment images are recorded, only the declared digest decides", m.State)
	}
	// nothing measured: nothing can be said about the commitment images either way
	m2 := Evaluate(dA, []string{dB}, Reading{Reason: "x"})
	if len(m2.CommitmentFound)+len(m2.CommitmentNotFound) != 0 {
		t.Fatalf("an unmeasured run must not claim commitment images are missing: %+v", m2)
	}
}

func TestCheck_RefusesEveryInconsistentRecord(t *testing.T) {
	good := Evaluate(dA, nil, Reading{Source: "k8s", Digests: []string{dA}})
	cases := map[string]func(m *Measurement){
		"matched but declared not running": func(m *Measurement) { m.Running = []string{dB} },
		"matched with a reason":            func(m *Measurement) { m.Reason = "fine" },
		"wrong declared":                   func(m *Measurement) { m.Declared = dB },
		"unknown state":                    func(m *Measurement) { m.State = "verified" },
		"unknown version":                  func(m *Measurement) { m.Version = 9 },
		"unsorted running":                 func(m *Measurement) { m.Running = []string{dB, dA} },
		"garbage digest":                   func(m *Measurement) { m.Running = []string{dA, "nope"} },
		"not_measured hiding a proven mismatch": func(m *Measurement) {
			m.State, m.Reason, m.Running = StateNotMeasured, "could not measure", []string{dB}
		},
		"not_measured without a reason": func(m *Measurement) { m.State, m.Reason, m.Running = StateNotMeasured, "", nil },
		"matched with nothing running":  func(m *Measurement) { m.Running = nil },
	}
	for name, mutate := range cases {
		m := good
		m.Running = append([]string(nil), good.Running...)
		mutate(&m)
		if err := Check(m, dA); err == nil {
			t.Errorf("%s: Check accepted %+v", name, m)
		}
	}
	if err := Check(good, dA); err != nil {
		t.Fatalf("control: the untouched record must pass: %v", err)
	}
}

func TestBundleHash_BindsTheMeasurement(t *testing.T) {
	root := strings.Repeat("ab", 32)
	m := Evaluate(dA, nil, Reading{Source: "k8s", Digests: []string{dA}})
	h := BundleHash(root, &m)
	if h == root || len(h) != 64 {
		t.Fatalf("the bundle must differ from the scenario root once a measurement is bound: %s", h)
	}
	if BundleHash(root, &m) != h {
		t.Fatal("BundleHash must be deterministic")
	}
	if BundleHash(root, nil) != root {
		t.Fatal("no measurement (an old executor): the bundle is the scenario root, unchanged")
	}
	other := Evaluate(dA, nil, Reading{Source: "k8s", Digests: []string{dA, dB}})
	if BundleHash(root, &other) == h {
		t.Fatal("changing the measured digests must change the bundle hash")
	}
	nm := Evaluate(dA, nil, Reading{Reason: "no permission"})
	nm2 := Evaluate(dA, nil, Reading{Reason: "another reason"})
	if BundleHash(root, &nm) == BundleHash(root, &nm2) {
		t.Fatal("the not-measured reason is part of what is bound")
	}
	if BundleHash(strings.Repeat("cd", 32), &m) == h {
		t.Fatal("changing the scenario root must change the bundle hash")
	}
}

func TestDigestFromImageID(t *testing.T) {
	good := map[string]string{
		"docker.io/library/nginx@" + dA:                                 dA,
		"docker-pullable://ghcr.io/x/y@" + dA:                           dA,
		"acr.example/control-plane@" + strings.ToUpper(dA[:7]) + dA[7:]: dA,
	}
	for in, want := range good {
		if got, ok := DigestFromImageID(in); !ok || got != want {
			t.Errorf("DigestFromImageID(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"", "nginx:1.25", "sha256:" + strings.Repeat("d", 64), // a bare config id is not a registry digest
		"docker-pullable://nginx", "repo@sha256:short", "repo@",
	} {
		if got, ok := DigestFromImageID(in); ok {
			t.Errorf("DigestFromImageID(%q) = %q, want no digest", in, got)
		}
	}
}
