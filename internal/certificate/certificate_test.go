package certificate

// certificate_test.go — T6.3's pinned-fields test, the local half of publicview_test.go's
// TestPublicSlug_PinnedFields discipline: the EXACT recursive key set a populated Certificate must
// marshal to. Adding a field to Certificate/Anchor/Set/Tallies/VerdictRecord without adding it here
// fails this test.

import (
	"encoding/json"
	"sort"
	"testing"
)

var certGoldenKeys = []string{
	"artifact_measurement", "artifact_measurement.bound", "artifact_measurement.state",
	"artifact_measurement.reason", "artifact_measurement.source", "artifact_measurement.running",
	"artifact_measurement.unresolved", "artifact_measurement.commitment_images_found",
	"artifact_measurement.commitment_images_not_found", "artifact_measurement.scenario_evidence_root",
	"format",
	"run_id",
	"finished_at",
	"verdict",
	"tallies",
	"tallies.passed",
	"tallies.failed",
	"tallies.errored",
	"tallies.degraded",
	"set",
	"set.holdout_root",
	"set.criteria_hash",
	"set.set_hash",
	"set.scenario_count",
	"verdict_record",
	"verdict_record.artifact_digest",
	"verdict_record.evidence_bundle_hash",
	"anchors",
	"anchors[].version",
	"anchors[].chain",
	"anchors[].chain_correlation_id",
	"anchors[].block",
	"anchors[].tx_hash",
	"anchors[].signer",
	"anchors[].receipt",
	"anchors[].chain_id",
	"issued_at",
	"anchoring",
	"anchoring.status",
	"anchoring.chains_awaiting_verdict",
	"anchoring.ots_awaiting_bitcoin",
	"anchoring.note",
	"revealed_at",
	"reveal_document_sha256",
}

func collectKeys(prefix string, v any, out map[string]struct{}) {
	switch val := v.(type) {
	case map[string]any:
		for k, cv := range val {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			out[path] = struct{}{}
			collectKeys(path, cv, out)
		}
	case []any:
		path := prefix + "[]"
		for _, item := range val {
			collectKeys(path, item, out)
		}
	}
}

func TestCertificate_PinnedFields(t *testing.T) {
	block := int64(42)
	cert := Certificate{
		Format: Format, RunID: "run-1", FinishedAt: "2026-09-24T00:00:00Z", Verdict: "passed",
		Tallies:       Tallies{Passed: 1, Failed: 0, Errored: 0, Degraded: 1},
		Set:           Set{HoldoutRoot: "root", CriteriaHash: "crit", SetHash: "sh", ScenarioCount: 3},
		VerdictRecord: VerdictRecord{ArtifactDigest: "sha256:aa", EvidenceBundleHash: "sha256:bb"},
		Anchors: []Anchor{
			{Version: 2, Chain: "c1", ChainCorrelationID: "corr", Block: &block, TxHash: "0xabc", Signer: "0xsigner", ChainID: 2026},
			{Version: 3, Chain: "c2", Receipt: []byte("receipt-bytes"), Signer: "0xsigner"},
			{Version: 4, Chain: "c1", ChainCorrelationID: "corr2", Block: &block, TxHash: "0xdef", Signer: "0xsigner"},
		},
		IssuedAt:             "2026-09-24T00:01:00Z",
		Anchoring:            &Anchoring{Status: AnchoringInProgress, ChainsAwaitingVerdict: []string{"base-sepolia"}, OTSAwaitingBitcoin: []string{"bitcoin-ots"}, Note: "n"},
		RevealedAt:           "2026-09-24T00:02:00Z",
		RevealDocumentSHA256: "deadbeef",
		ArtifactMeasurement: &ArtifactMeasurement{
			Bound: true, State: "matched", Reason: "r", Source: "k8s", Running: []string{"sha256:aa"}, Unresolved: 1,
			CommitmentFound: []string{"sha256:aa"}, CommitmentNotFound: []string{"sha256:cc"}, ScenarioEvidenceRoot: "root",
		},
	}
	b, err := json.Marshal(cert)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	t.Logf("sample certificate JSON: %s", b)

	var parsed any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := map[string]struct{}{}
	collectKeys("", parsed, got)
	gotList := make([]string, 0, len(got))
	for k := range got {
		gotList = append(gotList, k)
	}
	sort.Strings(gotList)
	want := append([]string(nil), certGoldenKeys...)
	sort.Strings(want)

	if len(gotList) != len(want) {
		t.Fatalf("key set size = %d, want %d\ngot:  %v\nwant: %v", len(gotList), len(want), gotList, want)
	}
	for i := range want {
		if gotList[i] != want[i] {
			t.Fatalf("key set mismatch at %d: got %q, want %q\nfull got:  %v\nfull want: %v", i, gotList[i], want[i], gotList, want)
		}
	}

	// The forbidden-key walk: no scenario id/path/body, no EXPECT, no observed value, no
	// commitment_id, no workspace/org/instance id, no host, no dashboard link, no v1 anchor version.
	forbidden := map[string]bool{
		"scenario_id": true, "path": true, "body": true, "expect": true, "observed": true,
		"commitment_id": true, "workspace_id": true, "instance_id": true, "org_id": true,
		"host": true, "deep_link": true, "title": true,
	}
	var walk func(v any)
	walk = func(v any) {
		switch t2 := v.(type) {
		case map[string]any:
			for k, vv := range t2 {
				if forbidden[k] {
					t.Errorf("certificate carries a forbidden key %q", k)
				}
				walk(vv)
			}
		case []any:
			for _, vv := range t2 {
				walk(vv)
			}
		}
	}
	walk(parsed)
	for _, a := range cert.Anchors {
		if a.Version == 1 {
			t.Errorf("certificate carries a version-1 (commit-level) anchor, which must never appear")
		}
	}
}
