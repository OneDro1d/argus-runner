package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// ── #46(record): onboarding's own report of what it installed ───────────────────────────────────
//
// The tests above drive a REAL onboard.sh. These drive a FAKE one instead: onboard.sh can silently
// rename the instance (a name collision), resolve the tier itself (--tier left "auto"), and always
// knows the executor it actually pulled (a bare --image, relying on ARGUS_MCP_IMAGE or the built-in
// default) — none of which recordInstalledManifest could see before the ARGUS_UP_RECORD channel. A
// fake onboard.sh isolates exactly that channel from everything else a real one does.

// upFakeOnboardHarness builds a MINIMAL kit for these tests — just onboarding/onboard.sh, replaced by
// the fake script given — and chdirs the test process into it (mirroring upPrepareHarness's own
// chdir), without pulling in the docker/kubectl/curl stub harness upPrepareHarness needs: none of
// these tests exercise a real onboard.sh, so nothing here shells out to any of those tools.
func upFakeOnboardHarness(t *testing.T, script string) {
	t.Helper()
	kit := t.TempDir()
	onboardDir := filepath.Join(kit, "onboarding")
	if err := os.MkdirAll(onboardDir, 0o755); err != nil {
		t.Fatalf("mkdir onboarding: %v", err)
	}
	if err := os.WriteFile(filepath.Join(onboardDir, "onboard.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake onboard.sh: %v", err)
	}
	routerState := filepath.Join(t.TempDir(), "argus", "router")
	t.Setenv("ARGUS_ROUTER_STATE", filepath.ToSlash(routerState))

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(kit); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(prevWD); err != nil {
			t.Fatalf("chdir back: %v", err)
		}
	})
}

// upManifestRecordArgs builds a fresh product/scenarios sandbox and the `up` argv the tests below
// share: plain mode (no --json) — none of these need the JSON translator, only the exit code and the
// manifest recordInstalledManifest wrote. tier/image, when non-empty, are threaded onto argv exactly
// as an operator's own --tier/--image would be; left empty, the flag is not passed at all (so --tier
// keeps its own "auto" default, and --image its own ARGUS_MCP_IMAGE-or-empty default).
func upManifestRecordArgs(t *testing.T, instanceID, tier, image string) []string {
	t.Helper()
	sand := t.TempDir()
	prod := filepath.Join(sand, "prod")
	scen := filepath.Join(sand, "scenarios")
	if err := os.MkdirAll(prod, 0o755); err != nil {
		t.Fatalf("mkdir prod: %v", err)
	}
	if err := os.MkdirAll(scen, 0o755); err != nil {
		t.Fatalf("mkdir scen: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prod, "argus-config.yaml"), []byte("project:\n  name: probe\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	args := []string{
		"--config", filepath.Join(prod, "argus-config.yaml"),
		"--scenarios-dir", scen,
		"--runner-id", instanceID,
		"--yes",
	}
	if tier != "" {
		args = append(args, "--tier", tier)
	}
	if image != "" {
		args = append(args, "--image", image)
	}
	return args
}

// fakeOnboardRecording is a fake onboard.sh that, when ARGUS_UP_RECORD is set, writes ONE JSON record
// for it: the instance id it was given (with renameSuffix appended, mimicking onboard.sh's own
// "<id>-vN" collision rename), and the tier/executor this test wants it to report — regardless of what
// argv it was actually given, exactly like a real onboard.sh deciding the tier for itself would.
func fakeOnboardRecording(renameSuffix, tier, executor string) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
INSTANCE_ID=""
while [ $# -gt 0 ]; do
  case "$1" in
    --instance-id) INSTANCE_ID="$2"; shift 2;;
    *) shift;;
  esac
done
RECORD_ID="${INSTANCE_ID}%s"
if [ -n "${ARGUS_UP_RECORD:-}" ]; then
  printf '{"instance_id":"%%s","tier":"%s","executor":"%s"}\n' "$RECORD_ID" > "$ARGUS_UP_RECORD"
fi
exit 0
`, renameSuffix, tier, executor)
}

// fakeOnboardNoRecord is a fake onboard.sh that ignores ARGUS_UP_RECORD entirely — an older
// onboard.sh, from before this record channel existed.
const fakeOnboardNoRecord = `#!/usr/bin/env bash
set -euo pipefail
exit 0
`

// fakeOnboardMalformedRecord writes something to ARGUS_UP_RECORD that is not valid JSON — a run that
// died mid-write, or a future onboard.sh with an incompatible shape.
const fakeOnboardMalformedRecord = `#!/usr/bin/env bash
set -euo pipefail
if [ -n "${ARGUS_UP_RECORD:-}" ]; then
  printf 'not-json-at-all' > "$ARGUS_UP_RECORD"
fi
exit 0
`

// TestUp_ManifestRecord_RenamedInstance_UsesRecordedID: onboard.sh renamed the instance to
// "<id>-v1" (a name collision) — the manifest must be recorded under the id onboarding ACTUALLY
// registered, and no manifest may exist under the original, never-registered name.
func TestUp_ManifestRecord_RenamedInstance_UsesRecordedID(t *testing.T) {
	instanceID := "probe-rename"
	args := upManifestRecordArgs(t, instanceID, "compose", "img:given")
	upFakeOnboardHarness(t, fakeOnboardRecording("-v1", "compose", "img:given"))

	_, code := upRun(t, "", args...)
	if code != 0 {
		t.Fatalf("fake onboard.sh run did not succeed: exit=%d", code)
	}

	routerState := os.Getenv("ARGUS_ROUTER_STATE")
	renamedID := instanceID + "-v1"
	m, err := updatecmd.ReadManifest(routerState, renamedID)
	if err != nil {
		t.Fatalf("ReadManifest(%q): %v", renamedID, err)
	}
	if m.InstanceID != renamedID {
		t.Fatalf("manifest was not recorded under the RENAMED instance id %q onboarding actually "+
			"registered — got %+v", renamedID, m)
	}

	orig, err := updatecmd.ReadManifest(routerState, instanceID)
	if err != nil {
		t.Fatalf("ReadManifest(%q) (the ORIGINAL, never-registered id): %v", instanceID, err)
	}
	if orig.InstanceID != "" {
		t.Fatalf("a manifest was recorded under %q, an instance name onboarding renamed away from and "+
			"never registered: %+v", instanceID, orig)
	}
}

// TestUp_ManifestRecord_ReportedTier_UsesRecordedTier: --tier was left "auto" (onboard.sh resolves
// it); the manifest must carry the tier onboarding ACTUALLY resolved ("k3d"), never the "compose"
// fallback recordInstalledManifest used before this record channel existed.
func TestUp_ManifestRecord_ReportedTier_UsesRecordedTier(t *testing.T) {
	instanceID := "probe-tier"
	args := upManifestRecordArgs(t, instanceID, "", "img:given") // --tier left at its own "auto" default
	upFakeOnboardHarness(t, fakeOnboardRecording("", "k3d", "img:given"))

	_, code := upRun(t, "", args...)
	if code != 0 {
		t.Fatalf("fake onboard.sh run did not succeed: exit=%d", code)
	}

	routerState := os.Getenv("ARGUS_ROUTER_STATE")
	m, err := updatecmd.ReadManifest(routerState, instanceID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Tier != "k3d" {
		t.Fatalf("manifest tier = %q, want the tier onboarding actually resolved (\"k3d\"), not the "+
			"stale \"compose\" fallback", m.Tier)
	}
}

// TestUp_ManifestRecord_ReportedExecutor_UsesRecordedExecutor: --image was left empty (the operator
// relied on ARGUS_MCP_IMAGE or onboard.sh's own default); the manifest must carry the executor
// onboarding actually installed, not an empty Executor a bare second `argus up` has nothing to fall
// back to from (secondRunInstance, up_second_run.go).
func TestUp_ManifestRecord_ReportedExecutor_UsesRecordedExecutor(t *testing.T) {
	instanceID := "probe-executor"
	args := upManifestRecordArgs(t, instanceID, "compose", "") // --image left empty
	upFakeOnboardHarness(t, fakeOnboardRecording("", "compose", "ghcr.io/example/executor:v9"))

	_, code := upRun(t, "", args...)
	if code != 0 {
		t.Fatalf("fake onboard.sh run did not succeed: exit=%d", code)
	}

	routerState := os.Getenv("ARGUS_ROUTER_STATE")
	m, err := updatecmd.ReadManifest(routerState, instanceID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Executor != "ghcr.io/example/executor:v9" {
		t.Fatalf("manifest executor = %q, want the executor onboarding actually installed, not an "+
			"empty --image this pass was given", m.Executor)
	}
}

// TestUp_ManifestRecord_NoRecordWritten_FallsBackToGivenArgs: an older onboard.sh that never wrote
// ARGUS_UP_RECORD at all — recordInstalledManifest's pre-existing fallback (the runner id, --tier,
// --image it was given) must still write a manifest exactly as it did before this record channel
// existed.
func TestUp_ManifestRecord_NoRecordWritten_FallsBackToGivenArgs(t *testing.T) {
	instanceID := "probe-no-record"
	args := upManifestRecordArgs(t, instanceID, "compose", "img:given")
	upFakeOnboardHarness(t, fakeOnboardNoRecord)

	_, code := upRun(t, "", args...)
	if code != 0 {
		t.Fatalf("fake onboard.sh run did not succeed: exit=%d", code)
	}

	routerState := os.Getenv("ARGUS_ROUTER_STATE")
	m, err := updatecmd.ReadManifest(routerState, instanceID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.InstanceID != instanceID || m.Tier != "compose" || m.Executor != "img:given" {
		t.Fatalf("an onboard.sh that never wrote a record must leave recordInstalledManifest's "+
			"existing fallback untouched: got %+v", m)
	}
}

// TestUp_ManifestRecord_MalformedRecord_FallsBackToGivenArgs: onboard.sh wrote something to
// ARGUS_UP_RECORD that is not valid JSON — recordInstalledManifest must fall back exactly as it does
// when nothing was written at all, never fail the run over it (the write is best-effort).
func TestUp_ManifestRecord_MalformedRecord_FallsBackToGivenArgs(t *testing.T) {
	instanceID := "probe-malformed-record"
	args := upManifestRecordArgs(t, instanceID, "compose", "img:given")
	upFakeOnboardHarness(t, fakeOnboardMalformedRecord)

	_, code := upRun(t, "", args...)
	if code != 0 {
		t.Fatalf("fake onboard.sh run did not succeed: exit=%d", code)
	}

	routerState := os.Getenv("ARGUS_ROUTER_STATE")
	m, err := updatecmd.ReadManifest(routerState, instanceID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.InstanceID != instanceID || m.Tier != "compose" || m.Executor != "img:given" {
		t.Fatalf("a malformed record must fall back exactly like no record at all: got %+v", m)
	}
}

// TestUp_ManifestRecord_RecordFile_DoesNotSurviveRun proves the temp file `up` hands onboard.sh
// through ARGUS_UP_RECORD is removed afterwards on every path, using the same TEST-ONLY tee-hook
// pattern upFD5Tee already establishes in this package.
func TestUp_ManifestRecord_RecordFile_DoesNotSurviveRun(t *testing.T) {
	instanceID := "probe-record-cleanup"
	args := upManifestRecordArgs(t, instanceID, "compose", "img:given")
	upFakeOnboardHarness(t, fakeOnboardRecording("", "compose", "img:given"))

	var capturedPath string
	upRecordPathTee = func(p string) { capturedPath = p }
	t.Cleanup(func() { upRecordPathTee = nil })

	_, code := upRun(t, "", args...)
	if code != 0 {
		t.Fatalf("fake onboard.sh run did not succeed: exit=%d", code)
	}
	if capturedPath == "" {
		t.Fatalf("upRecordPathTee was never called — no record path was created for this run")
	}
	if _, err := os.Stat(capturedPath); !os.IsNotExist(err) {
		t.Fatalf("the record file %q still exists after the run (err=%v) — it must be removed on "+
			"every path", capturedPath, err)
	}
}

// ── AC-19b gap #40(2) follow-up: the manifest also records the app's OWN services ────────────────
//
// The run view (internal/control/runservices.go) graphs an instance's services from manifest
// artefacts of kind "service"; nothing wrote one until now. These drive recordInstalledManifest
// (via a fake onboard.sh, exactly like the fallback tests above) against a REAL argus-config.yaml,
// and assert on the artefacts updatecmd.ReadManifest reads back.

// upManifestRecordArgsWithConfig is upManifestRecordArgs, but the product folder's argus-config.yaml
// carries configYAML verbatim instead of the bare `project: {name: probe}` stub — for cases that
// need real targets for serviceArtefactsFromConfig to read.
func upManifestRecordArgsWithConfig(t *testing.T, instanceID, configYAML string) []string {
	t.Helper()
	sand := t.TempDir()
	prod := filepath.Join(sand, "prod")
	scen := filepath.Join(sand, "scenarios")
	if err := os.MkdirAll(prod, 0o755); err != nil {
		t.Fatalf("mkdir prod: %v", err)
	}
	if err := os.MkdirAll(scen, 0o755); err != nil {
		t.Fatalf("mkdir scen: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prod, "argus-config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return []string{
		"--config", filepath.Join(prod, "argus-config.yaml"),
		"--scenarios-dir", scen,
		"--tier", "compose", "--image", "img:test",
		"--runner-id", instanceID,
		"--yes",
	}
}

// TestUp_ManifestRecord_ConfigTargets_RecordsServiceArtefacts: a config declaring two named http
// targets, one named database target and a singular (unnamed) http target with a URL must yield
// four "service" artefacts — one per named target key, plus one named by the singular target's URL
// hostname (the fallback scenarioService itself uses for a scenario with no declared **Target**).
func TestUp_ManifestRecord_ConfigTargets_RecordsServiceArtefacts(t *testing.T) {
	instanceID := "probe-services"
	configYAML := `project:
  name: probe
targets:
  http_targets:
    web:
      base_url: http://web:8080
    api:
      base_url: http://api:9090
  database_targets:
    pg:
      type: postgres
      jdbc_url: jdbc:postgresql://db:5432/probe
  http:
    base_url: http://gateway.example.com:8080
`
	args := upManifestRecordArgsWithConfig(t, instanceID, configYAML)
	upFakeOnboardHarness(t, fakeOnboardRecording("", "compose", "img:test"))

	_, code := upRun(t, "", args...)
	if code != 0 {
		t.Fatalf("fake onboard.sh run did not succeed: exit=%d", code)
	}

	routerState := os.Getenv("ARGUS_ROUTER_STATE")
	m, err := updatecmd.ReadManifest(routerState, instanceID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	got := map[string]bool{}
	for _, a := range m.Artefacts {
		if a.Kind != "service" {
			t.Fatalf("manifest carries a non-service artefact %+v — this test's config declares no "+
				"other artefact", a)
		}
		if a.Version != "unknown" {
			t.Fatalf("service artefact %q has version %q, want \"unknown\"", a.Name, a.Version)
		}
		got[a.Name] = true
	}
	want := []string{"web", "api", "pg", "gateway.example.com"}
	if len(got) != len(want) {
		t.Fatalf("manifest carries %d service artefacts (%v), want %d (%v)", len(got), m.Artefacts, len(want), want)
	}
	for _, n := range want {
		if !got[n] {
			t.Fatalf("manifest is missing a service artefact named %q — got %v", n, m.Artefacts)
		}
	}
}

// TestUp_ManifestRecord_NoConfig_RecordsNoServiceArtefacts: --config names a file that does not
// exist (an onboard that never reaches a resolvable argus-config.yaml, or one this pass simply
// cannot parse) — the manifest must still be written, with no service artefacts and no error.
func TestUp_ManifestRecord_NoConfig_RecordsNoServiceArtefacts(t *testing.T) {
	instanceID := "probe-no-config"
	sand := t.TempDir()
	scen := filepath.Join(sand, "scenarios")
	if err := os.MkdirAll(scen, 0o755); err != nil {
		t.Fatalf("mkdir scen: %v", err)
	}
	args := []string{
		"--config", filepath.Join(sand, "does-not-exist", "argus-config.yaml"),
		"--scenarios-dir", scen,
		"--tier", "compose", "--image", "img:test",
		"--runner-id", instanceID,
		"--yes",
	}
	upFakeOnboardHarness(t, fakeOnboardRecording("", "compose", "img:test"))

	_, code := upRun(t, "", args...)
	if code != 0 {
		t.Fatalf("fake onboard.sh run did not succeed: exit=%d", code)
	}

	routerState := os.Getenv("ARGUS_ROUTER_STATE")
	m, err := updatecmd.ReadManifest(routerState, instanceID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.InstanceID != instanceID {
		t.Fatalf("the manifest must still be written when --config names nothing readable: got %+v", m)
	}
	for _, a := range m.Artefacts {
		if a.Kind == "service" {
			t.Fatalf("a missing config must record NO service artefacts — got %+v", m.Artefacts)
		}
	}
}
