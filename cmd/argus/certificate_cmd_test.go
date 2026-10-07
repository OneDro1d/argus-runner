package main

// certificate_cmd_test.go — T6.3: `argus certificate verify` never contacts the control plane (no
// --control-plane flag exists on it at all, and an unreachable ARGUS_CP_URL in the environment makes
// no difference), plus its verified/mismatch per-anchor output over a hash-only (OTS) anchor — the
// one chain kind this package can exercise without dialing a live Ethereum RPC endpoint.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/certificate"
)

const certCLIFormat = "argus-certificate/v1"

// displayOrderHex is a receipt fold (the block header's INTERNAL byte order) as a block explorer or
// `bitcoin-cli getblockheader` prints it: byte-reversed. The --btc-headers file takes the root in that
// display order, so a header file written here must never carry the fold's own order.
func displayOrderHex(fold [32]byte) string {
	for i, j := 0, len(fold)-1; i < j; i, j = i+1, j-1 {
		fold[i], fold[j] = fold[j], fold[i]
	}
	return hex.EncodeToString(fold[:])
}

// writeCertFile marshals a minimal but well-formed certificate carrying exactly one hash-only (OTS)
// v3 anchor, whose Receipt commits to sha256 of the verdict payload — mirroring
// internal/certificate's own TestVerify_OTS_MatchingAndMismatchingReceipt fixture, but produced (and
// consumed) entirely through the CLI.
func writeCertFile(t *testing.T, dir string, tamperVerdict bool) (certPath string) {
	t.Helper()
	verdict := "passed"
	cert := map[string]any{
		"format": certCLIFormat, "run_id": "run-cli-1", "verdict": verdict,
		"tallies":        map[string]any{"passed": 1, "failed": 0, "errored": 0},
		"set":            map[string]any{"holdout_root": "root", "criteria_hash": "crit", "set_hash": "sh", "scenario_count": 1},
		"verdict_record": map[string]any{"artifact_digest": "sha256:aa", "evidence_bundle_hash": "sha256:bb"},
		"issued_at":      "2026-09-24T00:00:00Z",
	}
	// the SAME payload shape certificate.VerdictV2 marshals — {run_id, artifact_digest, evidence_bundle_hash, verdict}.
	verdictJSON := verdict
	if tamperVerdict {
		verdictJSON = "failed"
	}
	payload, _ := json.Marshal(certificate.VerdictV2{
		RunID: "run-cli-1", ArtifactDigest: "sha256:aa", EvidenceBundleHash: "sha256:bb", Verdict: verdictJSON,
	})
	digest := sha256.Sum256(payload)
	receipt, _ := json.Marshal(map[string]any{"digest": digest[:], "height": 700000})
	cert["anchors"] = []map[string]any{
		{"version": 3, "chain": "bitcoin-ots", "signer": "s1", "receipt": receipt},
	}
	b, _ := json.Marshal(cert)
	certPath = filepath.Join(dir, "cert.json")
	if err := os.WriteFile(certPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	// the header file the FAKE fixture reads: for the TAMPERED case this file is still keyed by the
	// UNTAMPERED payload's digest (the header source is independent of what the certificate claims —
	// it is the certificate's receipt that lies, not the chain).
	realPayload, _ := json.Marshal(certificate.VerdictV2{
		RunID: "run-cli-1", ArtifactDigest: "sha256:aa", EvidenceBundleHash: "sha256:bb", Verdict: verdict,
	})
	realDigest := sha256.Sum256(realPayload)
	headers := map[string]string{"700000": displayOrderHex(realDigest)}
	hb, _ := json.Marshal(headers)
	if err := os.WriteFile(filepath.Join(dir, "btc-headers.json"), hb, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath
}

func TestCertificateVerify_NeverContactsControlPlane(t *testing.T) {
	// an unreachable control-plane URL in the environment — `certificate verify` declares no
	// --control-plane flag at all and must not read ARGUS_CP_URL either.
	t.Setenv("ARGUS_CP_URL", "http://127.0.0.1:1/unreachable")
	dir := t.TempDir()
	certPath := writeCertFile(t, dir, false)

	rc := 999
	out := captureEmitRaw(t, func() {
		rc = dispatch([]string{"certificate", "verify", certPath, "--btc-headers", filepath.Join(dir, "btc-headers.json"), "--json"})
	})
	var payload struct {
		Verified bool `json:"verified"`
		Results  []struct {
			Name, Status, Detail string
		} `json:"results"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("certificate verify did not emit JSON: %v\nout: %s", err, out)
	}
	if !payload.Verified || rc != exitOK {
		t.Fatalf("verified=%v rc=%d, want true/exitOK with an UNREACHABLE control plane in the environment — the verify path must be indifferent to it entirely: %+v", payload.Verified, rc, payload)
	}
	if len(payload.Results) != 2 || payload.Results[0].Status != "verified" || payload.Results[1].Name != "certificate tallies" {
		t.Fatalf("results = %+v, want the OTS anchor verified plus the tallies check", payload.Results)
	}
}

func TestCertificateVerify_TamperedReceiptIsMismatch(t *testing.T) {
	dir := t.TempDir()
	certPath := writeCertFile(t, dir, true) // the certificate's own receipt commits to a DIFFERENT verdict than it declares

	rc := 999
	out := captureEmitRaw(t, func() {
		rc = dispatch([]string{"certificate", "verify", certPath, "--btc-headers", filepath.Join(dir, "btc-headers.json"), "--json"})
	})
	var payload struct {
		Verified bool `json:"verified"`
		Results  []struct {
			Name, Status, Detail string
		} `json:"results"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("certificate verify did not emit JSON: %v\nout: %s", err, out)
	}
	if payload.Verified || rc == exitOK {
		t.Fatalf("verified=%v rc=%d, want false/non-zero for a tampered receipt: %+v", payload.Verified, rc, payload)
	}
	if len(payload.Results) != 2 || payload.Results[0].Status != "mismatch" {
		t.Fatalf("results = %+v, want the OTS anchor a mismatch", payload.Results)
	}
}

// editCertFile rewrites a certificate file written by writeCertFile with edit applied — the forger's
// step: the anchors are untouched, only a declared field changes.
func editCertFile(t *testing.T, certPath string, edit func(c map[string]any)) {
	t.Helper()
	b, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	edit(c)
	b, _ = json.Marshal(c)
	if err := os.WriteFile(certPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// , end to end through the CLI, on a certificate in TODAY's (v1) format whose anchor still
// verifies: forging passed=250 against a 1-scenario set must exit non-zero with a named reason.
func TestCertificateVerify_ForgedTalliesExitNonZero_WithANamedReason(t *testing.T) {
	dir := t.TempDir()
	certPath := writeCertFile(t, dir, false)
	editCertFile(t, certPath, func(c map[string]any) { c["tallies"].(map[string]any)["passed"] = 250 })

	rc := 999
	out := captureEmitRaw(t, func() {
		rc = dispatch([]string{"certificate", "verify", certPath, "--btc-headers", filepath.Join(dir, "btc-headers.json")})
	})
	t.Logf("verify output:\n%s", out)
	if rc == exitOK {
		t.Fatalf("rc = exitOK for a certificate claiming passed=250 of 1 scenario")
	}
	for _, want := range []string{"VERIFIED: anchor v3 bitcoin-ots", "MISMATCH: certificate tallies", "exceeds set.scenario_count 1", "OVERALL: NOT verified"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

// The honest v1 certificate still exits 0, and says plainly what it does not anchor.
func TestCertificateVerify_HonestV1_SaysWhatIsNotAnchored(t *testing.T) {
	dir := t.TempDir()
	certPath := writeCertFile(t, dir, false)
	rc := 999
	out := captureEmitRaw(t, func() {
		rc = dispatch([]string{"certificate", "verify", certPath, "--btc-headers", filepath.Join(dir, "btc-headers.json")})
	})
	t.Logf("verify output:\n%s", out)
	if rc != exitOK {
		t.Fatalf("rc = %d for an honest v1 certificate", rc)
	}
	for _, want := range []string{"NOT ANCHORED: tallies", "NOT ANCHORED: finished_at", "OVERALL: verified"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

func TestCertificateVerify_UnknownFormatIsRefused(t *testing.T) {
	dir := t.TempDir()
	certPath := writeCertFile(t, dir, false)
	editCertFile(t, certPath, func(c map[string]any) { c["format"] = "argus-certificate/v9" })
	if rc := dispatch([]string{"certificate", "verify", certPath}); rc == exitOK {
		t.Fatalf("rc = exitOK for an unknown certificate format")
	}
}

func TestCertificateGet_RequiresInstanceAndRun(t *testing.T) {
	rc := dispatch([]string{"certificate", "get", "--control-plane", "http://127.0.0.1:1", "--token", "tok"})
	if rc != exitUsage {
		t.Fatalf("certificate get with no --instance-id/--run-id = rc %d, want exitUsage", rc)
	}
}
