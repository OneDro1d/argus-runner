package certificate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/chainread"

	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
)

// Certificate v5: the precondition tier. The not_applicable count is ANCHORED (it sits in the
// verdict payload the chain holds), the precondition results are BOUND (they sit in the measurement the anchored
// evidence_bundle_hash commits to), and an older verifier REFUSES the format rather than read it with the count
// dropped. Every v1-v4 certificate verifies exactly as before.

const fixtureArtifact = "sha256:0000000000000000000000000000000000000000000000000000000000000002"

func thePreconditions() []artifactmeasure.Precondition {
	return []artifactmeasure.Precondition{
		{ID: "np-in", Kind: "networkpolicy.ingress_enforced", Verdict: "absent",
			Positive: "connected", Negative: "NOT refused: connected although a deny policy selects it"},
		{ID: "np-out", Kind: "networkpolicy.egress_enforced", Verdict: "holds",
			Positive: "connected", Negative: "not connected in 4 attempts of 2s (refused or dropped)"},
	}
}

// v5Fixture anchors a REAL run on the in-memory chain: 2 passed + 1 not_applicable of 3, verdict passed, the
// measurement carrying the preconditions, and the anchored evidence_bundle_hash being the true hash of it.
func v5Fixture(t *testing.T, verdict string, tl Tallies, pre []artifactmeasure.Precondition) (*Certificate, chainread.ChainService) {
	t.Helper()
	m := artifactmeasure.Evaluate(fixtureArtifact, nil, artifactmeasure.Reading{Source: "k8s", Digests: []string{fixtureArtifact}})
	m.Preconditions = pre
	bundle := artifactmeasure.BundleHash(root, &m)
	cert, svc, _, _ := buildVerifyFixtureBundle(t, true, verdict, tl, bundle)
	cert.Format = FormatV5
	cert.ArtifactMeasurement = &ArtifactMeasurement{
		Bound: true, State: m.State, Source: m.Source, Running: m.Running, ScenarioEvidenceRoot: root,
		CommitmentFound: m.CommitmentFound, CommitmentNotFound: m.CommitmentNotFound,
	}
	for _, p := range pre {
		cert.Preconditions = append(cert.Preconditions, Precondition(p))
	}
	return cert, svc
}

func honestV5(t *testing.T) (*Certificate, chainread.ChainService) {
	return v5Fixture(t, "passed", Tallies{Passed: 2, NotApplicable: 1}, thePreconditions())
}

func TestV5_RoundTrip_VerifiesWithTheNotApplicableCountAnchored(t *testing.T) {
	cert, svc := honestV5(t)
	results, overall := verifyAll(t, cert, svc)
	if !overall {
		t.Fatalf("an honest v5 certificate did not verify: %+v", results)
	}
	tl := talliesResult(t, results)
	if tl.Status != StatusVerified || !strings.Contains(tl.Detail, "not_applicable 1") || !strings.Contains(tl.Detail, "compared with the anchored verdict payload") {
		t.Fatalf("the tallies line must show N/A = 1 compared with the chain: %s", tl.Line())
	}
	if r := measurementResult(t, cert); r.Status != StatusVerified {
		t.Fatalf("measurement: %s", r.Line())
	}
	// and the JSON round-trips through the typed struct (what `argus certificate get | verify` does)
	b, err := json.Marshal(cert)
	if err != nil {
		t.Fatal(err)
	}
	var back Certificate
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Format != FormatV5 || back.Tallies.NotApplicable != 1 || len(back.Preconditions) != 2 {
		t.Fatalf("round trip lost a field: %s", b)
	}
	if results2, overall2 := verifyAll(t, &back, svc); !overall2 {
		t.Fatalf("the round-tripped v5 certificate did not verify: %+v", results2)
	}
}

// The verifier rejects a tampered N/A count - every way of moving it, including ones that stay internally
// consistent with the verdict and scenario_count (so ONLY the anchored payload can refuse them).
func TestV5_TamperedNotApplicableCount_IsRefused(t *testing.T) {
	cases := map[string]func(c *Certificate){
		"N/A 1 -> 0, passed 2 -> 3 (consistent)":   func(c *Certificate) { c.Tallies.NotApplicable, c.Tallies.Passed = 0, 3 },
		"N/A 1 -> 2, passed 2 -> 1 (consistent)":   func(c *Certificate) { c.Tallies.NotApplicable, c.Tallies.Passed = 2, 1 },
		"N/A 1 -> 0 (the count dropped)":           func(c *Certificate) { c.Tallies.NotApplicable = 0 },
		"N/A moved into errored (consistent sums)": func(c *Certificate) { c.Tallies.NotApplicable, c.Tallies.Errored = 0, 1 },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			cert, svc := honestV5(t)
			edit(cert)
			results, overall := verifyAll(t, cert, svc)
			if overall {
				t.Fatalf("Overall = true for a tampered N/A count")
			}
			if r := resultNamed(t, results, "anchor v3 "+verifyChainName); r.Status != StatusMismatch {
				t.Errorf("the verdict anchor = %s, want mismatch: the chain holds the true tallies", r.Status)
			}
		})
	}
}

func TestV5_EveryPreconditionFieldIsBound(t *testing.T) {
	edits := map[string]func(c *Certificate){
		"id":            func(c *Certificate) { c.Preconditions[0].ID = "np-zzz" },
		"kind":          func(c *Certificate) { c.Preconditions[0].Kind = "networkpolicy.egress_enforced" },
		"verdict":       func(c *Certificate) { c.Preconditions[0].Verdict = "holds" },
		"positive":      func(c *Certificate) { c.Preconditions[0].Positive = "connected on attempt 1" },
		"negative":      func(c *Certificate) { c.Preconditions[0].Negative = "not connected (refused)" },
		"residue added": func(c *Certificate) { c.Preconditions[0].Residue = "may remain: pod x" },
		"one removed":   func(c *Certificate) { c.Preconditions = c.Preconditions[:1] },
		"one added": func(c *Certificate) {
			c.Preconditions = append(c.Preconditions, Precondition{ID: "z", Kind: "networkpolicy.egress_enforced", Verdict: "holds", Positive: "a", Negative: "b"})
		},
		"all stripped":   func(c *Certificate) { c.Preconditions = nil },
		"order swapped":  func(c *Certificate) { c.Preconditions[0], c.Preconditions[1] = c.Preconditions[1], c.Preconditions[0] },
		"absent->broken": func(c *Certificate) { c.Preconditions[0].Verdict = "broken" },
	}
	for name, edit := range edits {
		cert, _ := honestV5(t)
		edit(cert)
		if r := measurementResult(t, cert); r.Status != StatusMismatch {
			t.Errorf("%s: %s - an edited precondition list was not refused", name, r.Line())
		}
	}
}

// N/A in the tallies must be explained by a recorded absent precondition.
func TestV5_NotApplicableWithoutAnAbsentPreconditionIsRefused(t *testing.T) {
	pre := thePreconditions()
	pre[0].Verdict = "holds" // nothing absent
	cert, _ := v5Fixture(t, "passed", Tallies{Passed: 2, NotApplicable: 1}, pre)
	if r := measurementResult(t, cert); r.Status != StatusMismatch || !strings.Contains(r.Detail, "no precondition is recorded absent") {
		t.Fatalf("%s", r.Line())
	}
}

// A run in which EVERY scenario was N/A is verdict not_applicable, never passed.
func TestV5_AllNotApplicable_IsNeverPassed(t *testing.T) {
	cert, svc := v5Fixture(t, "not_applicable", Tallies{NotApplicable: 3}, thePreconditions())
	if results, overall := verifyAll(t, cert, svc); !overall {
		t.Fatalf("an honest all-N/A certificate did not verify: %+v", results)
	}
	cert, _ = v5Fixture(t, "passed", Tallies{NotApplicable: 3}, thePreconditions())
	if r := checkTallies(cert, nil); r.Status != StatusMismatch || !strings.Contains(r.Detail, "never a pass") {
		t.Fatalf("a passed verdict over all-N/A tallies must be refused: %s", r.Line())
	}
	cert, _ = v5Fixture(t, "not_applicable", Tallies{Passed: 1, NotApplicable: 2}, thePreconditions())
	if r := checkTallies(cert, nil); r.Status != StatusMismatch {
		t.Fatalf("a not_applicable verdict with a pass in the tallies must be refused: %s", r.Line())
	}
}

// Passed + N/A is passed, and a failure outranks it.
func TestV5_MixedVerdicts(t *testing.T) {
	cert, _ := v5Fixture(t, "failed", Tallies{Passed: 1, Failed: 1, NotApplicable: 1}, thePreconditions())
	if r := checkTallies(cert, nil); r.Status != StatusVerified {
		t.Fatalf("failed + passed + N/A: %s", r.Line())
	}
	cert, _ = v5Fixture(t, "passed", Tallies{Passed: 1, Errored: 1, NotApplicable: 1}, thePreconditions())
	if r := checkTallies(cert, nil); r.Status != StatusMismatch {
		t.Fatalf("a passed verdict over an errored scenario: %s", r.Line())
	}
}

// The bucket exists on v5 only: it cannot be hidden under an older label, and relabelling v5 down fails.
func TestV5_RelabelledDownIsRefused(t *testing.T) {
	for _, f := range []string{Format, FormatV2, FormatV4} {
		cert, svc := honestV5(t)
		cert.Format = f
		results, overall := verifyAll(t, cert, svc)
		if overall {
			t.Errorf("v5 relabelled %s verified", f)
		}
		if r := resultNamed(t, results, "certificate tallies"); r.Status != StatusMismatch {
			t.Errorf("v5 relabelled %s: tallies = %s", f, r.Line())
		}
	}
	// and an N/A verdict word under an older label
	cert, _ := v5Fixture(t, "not_applicable", Tallies{NotApplicable: 3}, thePreconditions())
	cert.Format = FormatV4
	if r := checkTallies(cert, nil); r.Status != StatusMismatch {
		t.Errorf("a not_applicable verdict on v4: %s", r.Line())
	}
}

func TestV5_StrippedOrMisplacedPreconditionsAreRefused(t *testing.T) {
	cert, _ := honestV5(t)
	cert.Preconditions = nil
	if r := measurementResult(t, cert); r.Status != StatusMismatch {
		t.Errorf("a v5 certificate with no preconditions: %s", r.Line())
	}
	plain := measuredCert(artifactmeasure.Reading{Source: "k8s", Digests: []string{tA}})
	plain.Preconditions = []Precondition{{ID: "x", Kind: "networkpolicy.ingress_enforced", Verdict: "holds", Positive: "a", Negative: "b"}}
	if r := measurementResult(t, plain); r.Status != StatusMismatch {
		t.Errorf("preconditions injected into a v3 certificate: %s", r.Line())
	}
}

// legacyKnownFormat is KnownFormat exactly as the v4-era verifier (internal/certificate at aedbdcab) wrote it.
// It is the version gate an OLDER verifier applies: a format it does not list is refused by name before any
// payload is rebuilt - so it can never verify a v5 certificate with the not_applicable count dropped.
func legacyKnownFormat(format string) bool {
	return format == "argus-certificate/v3" || format == "argus-certificate/v2" || format == "argus-certificate/v1" || format == "argus-certificate/v4"
}

func TestFormat_AnOlderVerifierRefusesV5(t *testing.T) {
	if legacyKnownFormat(FormatV5) {
		t.Fatal("the v4-era gate accepts v5: an older verifier would verify it with the N/A count dropped")
	}
	for _, f := range []string{Format, FormatV2, FormatV1, FormatV4} {
		if !legacyKnownFormat(f) || !KnownFormat(f) {
			t.Errorf("%s must be known to both the old gate and this verifier", f)
		}
	}
	if !KnownFormat(FormatV5) {
		t.Fatal("this verifier must know v5")
	}
	// And if an older verifier WERE handed the N/A-dropped document under a v4 label, the chain still refuses it:
	// the anchored payload contains not_applicable:1, the v4 rebuild does not.
	cert, svc := honestV5(t)
	cert.Format = FormatV4
	cert.Tallies = Tallies{Passed: 3} // N/A dropped, still sums to the set size
	cert.Preconditions = nil
	if _, overall := verifyAll(t, cert, svc); overall {
		t.Fatal("a v5 run with its N/A count dropped verified as v4")
	}
}

// Every certificate that is not v5 verifies and RENDERS exactly as before: the payload bytes have no
// not_applicable key, and the tallies line carries no N/A term.
func TestV4AndEarlier_AreByteIdenticalWithNoNotApplicableKey(t *testing.T) {
	for _, f := range []string{Format, FormatV2, FormatV4} {
		cert, _, _, _ := verifyFixtureTallied(t)
		cert.Format = f
		b, err := verdictPayload(cert)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "not_applicable") {
			t.Errorf("%s: the anchored payload grew a key: %s", f, b)
		}
		if r := checkTallies(cert, nil); r.Status != StatusVerified || strings.Contains(r.Detail, "not_applicable") {
			t.Errorf("%s: tallies line changed: %s", f, r.Line())
		}
	}
	// a v5 payload whose N/A is zero is the same bytes as v4's
	cert, _, _, _ := verifyFixtureTallied(t)
	cert.Format = FormatV4
	b4, _ := verdictPayload(cert)
	cert.Format = FormatV5
	b5, _ := verdictPayload(cert)
	if string(b4) != string(b5) {
		t.Errorf("a v5 payload with no N/A differs from v4's:\n%s\n%s", b5, b4)
	}
}

func TestV5_UnanchoredSaysTheResultsAreExecutorAttested(t *testing.T) {
	cert, _ := honestV5(t)
	all := strings.Join(Unanchored(cert), "\n")
	for _, want := range []string{"preconditions:", "attested by the executor", "not_applicable: 1 scenario(s) were NOT EXECUTED"} {
		if !strings.Contains(all, want) {
			t.Errorf("Unanchored omits %q:\n%s", want, all)
		}
	}
}

// A v4 certificate still verifies, faults and all (the other half of "v1-v4 unchanged").
func TestV4_FaultedCertificateStillVerifies(t *testing.T) {
	c := faultedCert()
	if r := measurementResult(t, c); r.Status != StatusVerified {
		t.Fatalf("an honest v4 certificate: %s", r.Line())
	}
	// and v5 may carry faults too
	cert, _ := honestV5(t)
	cert.Faults = nil
	if r := measurementResult(t, cert); r.Status != StatusVerified {
		t.Fatalf("v5 without faults: %s", r.Line())
	}
}
