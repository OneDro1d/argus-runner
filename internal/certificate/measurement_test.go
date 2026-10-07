package certificate

import (
	"context"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
)

const (
	tA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tC = "sha256:" + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

var root = strings.Repeat("9f", 32)

// measuredCert builds a certificate the way the control plane does: the bundle hash in the verdict record
// is BundleHash(root, measurement), and the certificate's block is the measurement, field for field.
func measuredCert(r artifactmeasure.Reading) *Certificate {
	m := artifactmeasure.Evaluate(tA, []string{tB, tC}, r)
	return &Certificate{
		Format: Format, RunID: "run-1", Verdict: "passed", FinishedAt: "2026-09-30T08:15:00Z",
		Tallies: Tallies{Passed: 1}, Set: Set{ScenarioCount: 1},
		VerdictRecord: VerdictRecord{ArtifactDigest: tA, EvidenceBundleHash: artifactmeasure.BundleHash(root, &m)},
		ArtifactMeasurement: &ArtifactMeasurement{
			Bound: true, State: m.State, Reason: m.Reason, Source: m.Source, Running: m.Running, Unresolved: m.Unresolved,
			CommitmentFound: m.CommitmentFound, CommitmentNotFound: m.CommitmentNotFound, ScenarioEvidenceRoot: root,
		},
	}
}

func measurementResult(t *testing.T, c *Certificate) CheckResult {
	t.Helper()
	for _, r := range Verify(context.Background(), c, nil, nil, nil) {
		if r.Name == "artifact measurement" {
			return r
		}
	}
	t.Fatalf("Verify produced no 'artifact measurement' result for %+v", c.ArtifactMeasurement)
	return CheckResult{}
}

func TestVerify_MatchedMeasurement_IsVerifiedAndSaysWhoAttested(t *testing.T) {
	c := measuredCert(artifactmeasure.Reading{Source: "k8s", Digests: []string{tA, tB}})
	r := measurementResult(t, c)
	if r.Status != StatusVerified || !strings.Contains(r.Detail, "matched") {
		t.Fatalf("result %+v", r)
	}
	all := strings.Join(Unanchored(c), "\n")
	if !strings.Contains(all, "attested by the executor") {
		t.Errorf("a matched certificate must say the digests are executor-attested, not independently observed:\n%s", all)
	}
}

func TestVerify_EditedMatchClaim_FailsForEveryFieldThatIsBound(t *testing.T) {
	edits := map[string]func(c *Certificate){
		"state flipped to matched on a not-measured run": func(c *Certificate) { c.ArtifactMeasurement.State, c.ArtifactMeasurement.Reason = "matched", "" },
		"a digest added to running":                      func(c *Certificate) { c.ArtifactMeasurement.Running = append(c.ArtifactMeasurement.Running, tC) },
		"running replaced by the declared digest": func(c *Certificate) {
			c.ArtifactMeasurement.State, c.ArtifactMeasurement.Reason, c.ArtifactMeasurement.Running = "matched", "", []string{tA}
		},
		"reason rewritten":          func(c *Certificate) { c.ArtifactMeasurement.Reason = "all good" },
		"source rewritten":          func(c *Certificate) { c.ArtifactMeasurement.Source = "compose" },
		"scenario root swapped":     func(c *Certificate) { c.ArtifactMeasurement.ScenarioEvidenceRoot = strings.Repeat("00", 32) },
		"unresolved zeroed":         func(c *Certificate) { c.ArtifactMeasurement.Unresolved = 0 },
		"commitment lists edited":   func(c *Certificate) { c.ArtifactMeasurement.CommitmentNotFound = nil },
		"declared digest repointed": func(c *Certificate) { c.VerdictRecord.ArtifactDigest = tB },
		"block stripped from v3":    func(c *Certificate) { c.ArtifactMeasurement = nil },
		"unbound with a matched claim": func(c *Certificate) {
			c.ArtifactMeasurement = &ArtifactMeasurement{Bound: false, State: "matched", Running: []string{tA}}
		},
	}
	// the honest base is a run that could NOT be measured, so flipping it to matched is the lie under test
	for name, edit := range edits {
		base := measuredCert(artifactmeasure.Reading{Source: "k8s", Reason: "forbidden: pods in namespace sut"})
		if strings.Contains(name, "matched on a not-measured") || strings.Contains(name, "running replaced") {
			// keep the not-measured base
		} else {
			base = measuredCert(artifactmeasure.Reading{Source: "k8s", Digests: []string{tA, tB}, Unresolved: 1})
		}
		edit(base)
		r := measurementResult(t, base)
		if r.Status != StatusMismatch {
			t.Errorf("%s: result %s — an edited certificate was not refused", name, r.Line())
			continue
		}
		t.Logf("%s => %s", name, r.Line())
		if Overall(base, Verify(context.Background(), base, nil, nil, nil)) {
			t.Errorf("%s: Overall verified", name)
		}
	}
}

func TestVerify_NotMeasured_IsNeverReadAsAPass(t *testing.T) {
	c := measuredCert(artifactmeasure.Reading{Source: "k8s", Reason: "forbidden: pods in namespace sut"})
	r := measurementResult(t, c)
	if r.Status != StatusNotBound || !strings.Contains(r.Detail, "NOT MEASURED") || !strings.Contains(r.Detail, "forbidden") {
		t.Fatalf("result %+v, want NOT BOUND naming NOT MEASURED and the reason", r)
	}
	if !strings.HasPrefix(r.Line(), "NOT BOUND: artifact measurement") {
		t.Fatalf("line %q", r.Line())
	}
}

func TestVerify_LegacyRunWithoutMeasurement_SaysNotMeasuredExecutorTooOld(t *testing.T) {
	c := measuredCert(artifactmeasure.Reading{})
	c.VerdictRecord.EvidenceBundleHash = root // an old executor: the bundle is the scenario-only hash
	c.ArtifactMeasurement = &ArtifactMeasurement{Bound: false, State: "not_measured", Reason: artifactmeasure.LegacyReason}
	r := measurementResult(t, c)
	if r.Status != StatusNotBound || !strings.Contains(r.Detail, "executor") {
		t.Fatalf("result %+v", r)
	}
	if strings.Contains(strings.ToLower(r.Line()), "matched") {
		t.Fatalf("a legacy run must never read as matched: %s", r.Line())
	}
	joined := strings.Join(Unanchored(c), "\n")
	if !strings.Contains(joined, "artifact measurement") {
		t.Errorf("Unanchored must list the missing binding:\n%s", joined)
	}
	if r2 := (&Certificate{Format: Format, ArtifactMeasurement: &ArtifactMeasurement{Bound: false, State: "not_measured", Reason: "x", ScenarioEvidenceRoot: root}}); measurementResult(t, r2).Status != StatusMismatch {
		t.Error("an unbound block may not carry a scenario root")
	}
}

func TestVerify_OlderFormatsCarryNoMeasurement_AndSayWhatThatMeans(t *testing.T) {
	for _, f := range []string{FormatV1, FormatV2} {
		c := &Certificate{Format: f, Verdict: "passed", Tallies: Tallies{Passed: 1}, Set: Set{ScenarioCount: 1}}
		for _, r := range Verify(context.Background(), c, nil, nil, nil) {
			if r.Name == "artifact measurement" {
				t.Errorf("%s: an old-format certificate must not get a measurement result", f)
			}
		}
		if !strings.Contains(strings.Join(Unanchored(c), "\n"), "artifact measurement") {
			t.Errorf("%s: Unanchored must say the format carries no artifact measurement", f)
		}
	}
}
