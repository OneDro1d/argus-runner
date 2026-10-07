package main

// certificate_measurement_cmd_test.go — `argus certificate verify` on an argus-certificate/v3 file: the
// artifact measurement it carries must agree with what the (hash-only, OTS) verdict anchor commits to.

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
	"github.com/OneDro1d/argus-runner/internal/certificate"
)

const (
	cliDeclared = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cliOther    = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cliRoot     = "9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f"
)

// writeMeasuredCertFile writes a v3 certificate whose one OTS verdict anchor commits to the tallied verdict
// payload carrying BundleHash(root, measurement) — what the control plane anchors for a measured run.
func writeMeasuredCertFile(t *testing.T, dir string, r artifactmeasure.Reading) string {
	t.Helper()
	m := artifactmeasure.Evaluate(cliDeclared, nil, r)
	bundle := artifactmeasure.BundleHash(cliRoot, &m)
	const finished = "2026-09-30T08:15:00Z"
	payload, _ := json.Marshal(certificate.VerdictTallied{
		RunID: "run-cli-m1", ArtifactDigest: cliDeclared, EvidenceBundleHash: bundle, Verdict: "passed",
		Tallies: certificate.VerdictTallies{Passed: 1}, FinishedAt: finished,
	})
	digest := sha256.Sum256(payload)
	receipt, _ := json.Marshal(map[string]any{"digest": digest[:], "height": 700000})
	cert := map[string]any{
		"format": "argus-certificate/v3", "run_id": "run-cli-m1", "verdict": "passed", "finished_at": finished,
		"tallies":        map[string]any{"passed": 1, "failed": 0, "errored": 0},
		"set":            map[string]any{"holdout_root": "root", "criteria_hash": "crit", "set_hash": "sh", "scenario_count": 1},
		"verdict_record": map[string]any{"artifact_digest": cliDeclared, "evidence_bundle_hash": bundle},
		"issued_at":      "2026-09-30T09:00:00Z",
		"anchors":        []map[string]any{{"version": 3, "chain": "bitcoin-ots", "signer": "s1", "receipt": receipt}},
		"artifact_measurement": map[string]any{
			"bound": true, "state": m.State, "source": m.Source, "running": m.Running, "scenario_evidence_root": cliRoot,
		},
	}
	if m.Reason != "" {
		cert["artifact_measurement"].(map[string]any)["reason"] = m.Reason
	}
	b, _ := json.Marshal(cert)
	p := filepath.Join(dir, "cert.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	hb, _ := json.Marshal(map[string]string{"700000": displayOrderHex(digest)})
	if err := os.WriteFile(filepath.Join(dir, "btc-headers.json"), hb, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func verifyCLI(t *testing.T, dir, certPath string) (int, string) {
	t.Helper()
	rc := 999
	out := captureEmitRaw(t, func() {
		rc = dispatch([]string{"certificate", "verify", certPath, "--btc-headers", filepath.Join(dir, "btc-headers.json")})
	})
	t.Logf("`argus certificate verify` (exit %d):\n%s", rc, out)
	return rc, string(out)
}

func TestCertificateVerify_MatchedV3_ExitsZero_AndSaysExecutorAttested(t *testing.T) {
	dir := t.TempDir()
	p := writeMeasuredCertFile(t, dir, artifactmeasure.Reading{Source: "k8s", Digests: []string{cliDeclared, cliOther}})
	rc, out := verifyCLI(t, dir, p)
	if rc != exitOK {
		t.Fatalf("rc = %d for an honest matched certificate", rc)
	}
	for _, want := range []string{"VERIFIED: artifact measurement — matched", "NOT ANCHORED: artifact measurement: the running digests are what the executor reported", "OVERALL: verified"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

// The forger's edit: the anchor is untouched, the certificate's own match claim is changed.
func TestCertificateVerify_EditedMatchClaim_ExitsNonZero_WithANamedReason(t *testing.T) {
	edits := map[string]func(c map[string]any){
		"a not-measured run relabelled matched": nil, // built below on its own base
		"a digest added to the running list": func(c map[string]any) {
			am := c["artifact_measurement"].(map[string]any)
			am["running"] = append(am["running"].([]any), "sha256:"+strings.Repeat("d", 64))
		},
		"the declared digest repointed to another running digest": func(c map[string]any) {
			c["verdict_record"].(map[string]any)["artifact_digest"] = cliOther
		},
		"the measurement block deleted": func(c map[string]any) { delete(c, "artifact_measurement") },
	}
	for name, edit := range edits {
		dir := t.TempDir()
		var p string
		if edit == nil {
			p = writeMeasuredCertFile(t, dir, artifactmeasure.Reading{Source: "k8s", Reason: "forbidden: pods in namespace sut"})
			editCertFile(t, p, func(c map[string]any) {
				am := c["artifact_measurement"].(map[string]any)
				am["state"] = "matched"
				delete(am, "reason")
				am["running"] = []any{cliDeclared}
			})
		} else {
			p = writeMeasuredCertFile(t, dir, artifactmeasure.Reading{Source: "k8s", Digests: []string{cliDeclared, cliOther}})
			editCertFile(t, p, edit)
		}
		rc, out := verifyCLI(t, dir, p)
		if rc == exitOK {
			t.Errorf("%s: exit 0 for an edited certificate", name)
		}
		// repointing the declared digest also breaks the OTS receipt (the anchored payload names it), so
		// that case is refused by the anchor AND by the measurement.
		for _, want := range []string{"MISMATCH: artifact measurement", "OVERALL: NOT verified"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: output lacks %q", name, want)
			}
		}
	}
}

func TestCertificateVerify_NotMeasuredV3_ExitsZero_ButNeverSaysMatched(t *testing.T) {
	dir := t.TempDir()
	p := writeMeasuredCertFile(t, dir, artifactmeasure.Reading{Source: "k8s", Reason: "forbidden: pods in namespace sut"})
	rc, out := verifyCLI(t, dir, p)
	if rc != exitOK {
		t.Fatalf("rc = %d: the verdict anchor verifies, only the measurement is absent", rc)
	}
	if !strings.Contains(out, "NOT BOUND: artifact measurement — NOT MEASURED") || strings.Contains(strings.ToLower(out), "matched") {
		t.Fatalf("output must say NOT MEASURED and never matched:\n%s", out)
	}
}
