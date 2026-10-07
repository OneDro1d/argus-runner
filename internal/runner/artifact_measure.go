package runner

import (
	"context"
	"encoding/json"
	"os"

	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// toolcoreRun is the run core. A variable only so a test can replace JMeter and a SUT with a stub; nothing
// else assigns it.
var toolcoreRun = toolcore.Run

// takeMeasurement reads the running SUT and compares it with the assignment's declared artifact digest.
// It never fails: an unmeasurable SUT is a Measurement in state not_measured with the reason.
func takeMeasurement(ctx context.Context, cfg ExecConfig, a *federation.RunAssignment) artifactmeasure.Measurement {
	if cfg.MeasureArtifact == nil {
		return artifactmeasure.Evaluate(a.ArtifactDigest, a.CommitmentImageDigests,
			artifactmeasure.Reading{Reason: "this executor has no artifact measurer configured"})
	}
	return artifactmeasure.Evaluate(a.ArtifactDigest, a.CommitmentImageDigests, cfg.MeasureArtifact(ctx))
}

// bindMeasurement sets the push's evidence bundle hash. Without a measurement it is the scenario-only
// hash, exactly as before; with one it commits to the scenarios AND the measurement, and the pieces the
// hash was built from ride along so the control plane (and, through the certificate, a third party) can
// recompute it.
func bindMeasurement(push *federation.ResultsPush, m *artifactmeasure.Measurement) {
	root := evidenceBundleHash(push.Scenarios)
	if m == nil {
		push.EvidenceBundleHash, push.ScenarioEvidenceRoot, push.ArtifactMeasurement = root, "", nil
		return
	}
	raw, _ := json.Marshal(m)
	push.ArtifactMeasurement = raw
	push.ScenarioEvidenceRoot = root
	push.EvidenceBundleHash = artifactmeasure.BundleHash(root, m)
}

// systemMeasurer is the measurer Bootstrap installs: the executor's own tier, the SUT namespace it was
// rendered with (ARGUS_SUT_NAMESPACE) and the compose project its argus-config declares.
func systemMeasurer(cfg ExecConfig) artifactmeasure.Func {
	return artifactmeasure.System(artifactmeasure.SystemConfig{
		Tier:      cfg.Tier,
		Namespace: os.Getenv("ARGUS_SUT_NAMESPACE"),
		ComposeProject: func() string {
			if cfg.ConfigPath == "" {
				return ""
			}
			c, err := config.ParseUnresolved(cfg.ConfigPath)
			if err != nil {
				return ""
			}
			return c.SUTProject()
		},
	})
}
