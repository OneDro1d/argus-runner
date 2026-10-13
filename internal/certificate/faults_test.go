package certificate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
)

// The faults a run applied are bound into the anchored evidence_bundle_hash and carried on
// a certificate of their own format (v4), so an older verifier refuses it by name instead of passing a
// document whose new binding it cannot check. A certificate of a run WITHOUT faults is the v3 it always was.

func theFaults() []artifactmeasure.Fault {
	return []artifactmeasure.Fault{
		{Op: "stop", Target: "trades", Params: map[string]int{"replicas": 0}, AppliedAt: "2026-10-09T10:00:00Z", RestoredAt: "2026-10-09T10:00:09Z", RestoreOutcome: "restored"},
		{Op: "delay", Target: "mcp-to-trades", Params: map[string]int{"latency_ms": 5000}, AppliedAt: "2026-10-09T10:00:10Z", RestoredAt: "2026-10-09T10:00:19Z", RestoreOutcome: "restored", Toxiproxy: true},
	}
}

// faultedCert builds a v4 certificate the way the control plane does.
func faultedCert() *Certificate {
	m := artifactmeasure.Evaluate(tA, nil, artifactmeasure.Reading{Source: "k8s", Digests: []string{tA}})
	m.Faults = theFaults()
	c := &Certificate{
		Format: FormatV4, RunID: "run-1", Verdict: "passed", FinishedAt: "2026-10-09T10:05:00Z",
		Tallies: Tallies{Passed: 1}, Set: Set{ScenarioCount: 1},
		VerdictRecord: VerdictRecord{ArtifactDigest: tA, EvidenceBundleHash: artifactmeasure.BundleHash(root, &m)},
		ArtifactMeasurement: &ArtifactMeasurement{
			Bound: true, State: m.State, Source: m.Source, Running: m.Running, ScenarioEvidenceRoot: root,
			CommitmentFound: m.CommitmentFound, CommitmentNotFound: m.CommitmentNotFound,
		},
	}
	for _, f := range m.Faults {
		c.Faults = append(c.Faults, Fault(f))
	}
	return c
}

func TestFormat_NewCertificatesWithoutFaultsStillDeclareV3(t *testing.T) {
	if Format != "argus-certificate/v3" {
		t.Fatalf("Format is %q: a no-fault certificate must keep declaring v3, byte for byte", Format)
	}
	if FormatV4 != "argus-certificate/v4" {
		t.Fatalf("FormatV4 = %q", FormatV4)
	}
	for _, f := range []string{Format, FormatV2, FormatV1, FormatV4, FormatV5} {
		if !KnownFormat(f) {
			t.Errorf("%s is refused as unknown", f)
		}
	}
	if KnownFormat("argus-certificate/v6") {
		t.Error("an unknown format was accepted")
	}
	if !Tallied(FormatV4) || !HasMeasurement(FormatV4) || !HasMeasurement(Format) || HasMeasurement(FormatV2) {
		t.Error("v4 must be tallied and measured; v2 must carry no measurement")
	}
}

func TestVerify_FaultedCertificateVerifies_AndEveryFaultFieldIsBound(t *testing.T) {
	c := faultedCert()
	if r := measurementResult(t, c); r.Status != StatusVerified {
		t.Fatalf("an honest v4 certificate: %s", r.Line())
	}
	edits := map[string]func(c *Certificate){
		"op":              func(c *Certificate) { c.Faults[0].Op = "scale" },
		"target":          func(c *Certificate) { c.Faults[0].Target = "payments" },
		"params":          func(c *Certificate) { c.Faults[1].Params = map[string]int{"latency_ms": 50} },
		"applied_at":      func(c *Certificate) { c.Faults[0].AppliedAt = "2026-10-09T09:00:00Z" },
		"restored_at":     func(c *Certificate) { c.Faults[0].RestoredAt = "2026-10-09T10:00:01Z" },
		"restore_outcome": func(c *Certificate) { c.Faults[0].RestoreOutcome = "already-restored" },
		"toxiproxy":       func(c *Certificate) { c.Faults[1].Toxiproxy = false },
		"a fault removed": func(c *Certificate) { c.Faults = c.Faults[:1] },
		"a fault added":   func(c *Certificate) { c.Faults = append(c.Faults, c.Faults[0]) },
		"order swapped":   func(c *Certificate) { c.Faults[0], c.Faults[1] = c.Faults[1], c.Faults[0] },
		"all stripped":    func(c *Certificate) { c.Faults = nil },
	}
	for name, edit := range edits {
		c := faultedCert()
		edit(c)
		if r := measurementResult(t, c); r.Status != StatusMismatch {
			t.Errorf("%s: %s - an edited fault list was not refused", name, r.Line())
		}
	}
}

func TestVerify_FaultsOnAV3CertificateAreRefused_AndAV4WithoutFaultsIsToo(t *testing.T) {
	c := faultedCert()
	c.Format = Format // relabelled to claim less
	if r := measurementResult(t, c); r.Status != StatusMismatch {
		t.Errorf("faults hidden under a v3 label: %s", r.Line())
	}
	plain := measuredCert(artifactmeasure.Reading{Source: "k8s", Digests: []string{tA}})
	plain.Faults = []Fault{{Op: "stop", Target: "trades", AppliedAt: "x", RestoredAt: "y", RestoreOutcome: "restored"}}
	if r := measurementResult(t, plain); r.Status != StatusMismatch {
		t.Errorf("faults injected into a v3 certificate: %s", r.Line())
	}
	v4 := measuredCert(artifactmeasure.Reading{Source: "k8s", Digests: []string{tA}})
	v4.Format = FormatV4
	if r := measurementResult(t, v4); r.Status != StatusMismatch {
		t.Errorf("a v4 certificate with no faults: %s", r.Line())
	}
}

func TestVerify_V4VerdictPayloadIsTheTalliedOne(t *testing.T) {
	c := faultedCert()
	b4, err := verdictPayload(c)
	if err != nil {
		t.Fatal(err)
	}
	c.Format = Format
	b3, _ := verdictPayload(c)
	if string(b3) != string(b4) {
		t.Fatalf("v4 must anchor the v3 verdict payload so no chain shape changes:\n%s\n%s", b4, b3)
	}
}

func TestUnanchored_SaysAToxiproxyWasInThePathAndTheFaultsAreExecutorAttested(t *testing.T) {
	all := strings.Join(Unanchored(faultedCert()), "\n")
	if !strings.Contains(all, "Toxiproxy") || !strings.Contains(all, "attested by the executor") {
		t.Fatalf("a link fault must be reported as ran-through-a-Toxiproxy and executor-attested:\n%s", all)
	}
}

func TestFaultedCertificate_PassesTheWholeVerifyAsOneCertificate(t *testing.T) {
	c := faultedCert()
	for _, r := range Verify(context.Background(), c, nil, nil, nil) {
		if r.Name == "artifact measurement" && r.Status == StatusMismatch {
			t.Fatalf("%s", r.Line())
		}
	}
	b, _ := json.Marshal(c)
	if !strings.Contains(string(b), `"faults"`) || strings.Contains(string(b), "step") {
		t.Fatalf("the certificate must carry faults and no scenario-internal step name: %s", b)
	}
}
