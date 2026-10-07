package runner

import (
	"time"

	"context"
	"crypto/ecdh"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/argus"
	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// ExecConfig configures the real run path: how to reach the SUT (its argus-config + jmeter mode) and
// where results land. The scenario set is NOT here — it arrives per-run in the assignment (D-FED.3).
type ExecConfig struct {
	InstanceID string
	// ToolInstance is the identity the in-env MCP surface reads results under (the runner__* tools
	// VALIDATE instance_id:"local", so that is what they scope to). R5-3: a federated run MUST write
	// its report here — not under InstanceID — or runner__get_report cannot see a cloud run's evidence
	// and the test agent re-runs directly to obtain it (two ledger rows for one request). Empty → "local".
	ToolInstance string
	ConfigPath   string // the SUT's argus-config.yaml (declares endpoints/targets)
	// ScenariosDir (AC-17) is the local scenario source a RELAYED builder run/validate_config executes
	// against — the SAME directory the merged process's own local router runs against. Never used by
	// the federated run_request path, which materializes its own scoped set per run (NewRunFunc);
	// empty here simply means no relayed run/validate_config was ever configured with one, and
	// toolcore reports the honest "no scenarios to run" refusal rather than executing zero silently.
	ScenariosDir string
	ResultsRoot  string
	ComposeFile  string
	JMeterLocal  bool
	TemplatesDir string
	Grafana      string
	Loki         string
	LokiTenant   string // T3.3: X-Scope-OrgID for a shared Loki; "" sends none (toolcore.Env.LokiTenant)
	Pushgateway  string
	Cluster      string // R6/ADR-10: the deploy-derived environment label; empty → "compose"
	// Tier is the deployment tier (compose | k3d | managed), normalized by federation.WireTier.
	// Distinct from Cluster: tier drives CONFIGURATION (VR-E8), cluster drives where telemetry lands.
	Tier string
	// ObsMode is the `--obs` mode the instance was onboarded with (ARGUS_OBS_MODE in the executor's
	// environment; "" = not supplied). under "none" the validator does not require
	// observability.grafana.public_url, for the executor's own validate_config as much as for onboarding's.
	ObsMode string
	// X25519Priv (AC-4b) opens a Sealed scenario body in memory before it is written to the run
	// directory (federation.OpenBody), keyed to the assignment's RunID. nil when this executor has
	// not minted one yet — SealAssignmentScenarios never seals for such an executor in the first
	// place, so nil here always meets an assignment carrying only clear Body values.
	X25519Priv *ecdh.PrivateKey
	// SutFingerprint (migration 050) reads the SUT's CURRENT deployment fingerprint, so each run can
	// record which revision it actually exercised. nil = not measured, and the push carries nothing —
	// which is the honest answer for an executor whose SUT declares no deployment_probe.
	//
	// ⛔ THIS IS THE RAW PROBE FUNCTION, NOT THE DeploymentWatcher, AND DELIBERATELY SO. The watcher's
	// Check() is EDGE-triggered: it owns a baseline (lastSeen/have/lastProbe) and emits a marker only
	// when the fingerprint changes. A level read that went through the watcher could advance that
	// baseline, and a redeploy the run had already observed would then be SILENTLY SWALLOWED — no
	// marker, no Timeline entry, nothing red to say so. Holding only the FingerprintFunc makes that
	// mistake unrepresentable rather than merely discouraged: the run path cannot reach the state it
	// must not touch. runner.go hands the SAME probe closure to both, so the two readings agree.
	SutFingerprint FingerprintFunc
	// MeasureArtifact reads the image digests the SUT is RUNNING. A final or scheduled run calls it before
	// any scenario runs, refuses when the declared artifact digest is provably not among them, and binds
	// what it read into the evidence bundle hash (measure.go). nil = no measurer wired: the run still
	// happens and its record says "not measured: no measurer" — never "matched". Set by Bootstrap
	// (systemMeasurer), before NewRunFunc copies this struct, for the reason SutFingerprint's comment gives.
	MeasureArtifact artifactmeasure.Func
	// ReadNamespaceVersion (migration 077) reads the running image digests of ONE namespace,
	// so every run, of every mode, can record which version of each namespace it touches it began against.
	// nil = no readings are taken and the push carries none. Deliberately NOT MeasureArtifact: that one is
	// evidence (bound into the bundle hash, may refuse a run) and is taken only for final / scheduled /
	// compare; this one is neither. Set by Bootstrap (namespaceVersionReader) before NewRunFunc copies this
	// struct, for the reason SutFingerprint's comment gives.
	ReadNamespaceVersion artifactmeasure.NamespaceFunc
	// Log (item 7b, msgbus tester 2026-09-28), when set, is the executor's own structured logger
	// (runner.Config.Log) — wired here so the relay's `run` verb can report a toolcore.Run failure
	// BY RUN ID instead of discarding it silently (relay.go used to `_, _, _ = toolcore.Run(...)`,
	// and the msgbus builder's relayed run left no trace anywhere: no report file, no log line, no
	// ledger row). nil is fine — the field simply did not exist before this ticket, and a relayed
	// run error is then not logged, same as always.
	Log func(format string, args ...any)
	// CatalogClient (item 7b), when set, is the SAME federation client Bootstrap already
	// authenticates the poll loop with (runner.go) — wired into the relay's toolcore.Env exactly the
	// way cpWireReporter wires SetFetcher/SetHasher for the in-env MCP surface (cmd/argus/main.go's
	// bare `argus serve` / merged `serve --mode runner` profiles), so a relayed `run` or
	// `validate_config` on a CATALOG-DRIVEN executor (no local scenarios/ dir — k8s, where
	// /scenarios is a per-pod emptyDir) can run/count the catalog BUILD set instead of hitting
	// toolcore's ErrNoScenarioSource refusal (relay.go's env is built from THIS ExecConfig,
	// independently of the federated run path's own materialized-per-assignment Env). nil means "no
	// catalog available" — unchanged behaviour; a test never sets this and gets the pre-7b Env.
	CatalogClient *Client
}

// execEnv builds the runner-core Env for a federated run. R5-3: the RESULTS identity is the TOOL
// identity (ToolInstance, "local") so the in-env MCP surface can read this run's report/sagas/logs,
// while the TELEMETRY label (ObsInstance) stays the REGISTERED instance id so the pushed metrics and
// the Grafana deep-link land under the real instance. These two were conflated, which made a cloud
// run's evidence unreachable in-env and provoked a duplicate direct re-run.
func execEnv(cfg ExecConfig, scenariosDir string) toolcore.Env {
	tool := cfg.ToolInstance
	if tool == "" {
		tool = "local"
	}
	return toolcore.Env{
		Instance: tool, ObsInstance: cfg.InstanceID, ConfigPath: cfg.ConfigPath, ScenariosDir: scenariosDir,
		ResultsRoot: cfg.ResultsRoot, ComposeFile: cfg.ComposeFile,
		JMeterLocal: cfg.JMeterLocal, TemplatesDir: cfg.TemplatesDir,
		Grafana: cfg.Grafana, Loki: cfg.Loki, LokiTenant: cfg.LokiTenant, Pushgateway: cfg.Pushgateway,
		Cluster: cfg.Cluster, Tier: cfg.Tier,
		ObsMode: cfg.ObsMode, // the relayed validate_config validates as onboarding did
		// the directory is the set the control plane ASSIGNED (materialize, below) — report it as such
		AssignedSet: true,
	}
}

// NewRunFunc returns the real RunFunc: it atomically materializes the assignment's (already
// scope-filtered) scenario set into a temp dir, drives the UNCHANGED M2.5 runner core over it, reads
// the in-env report, and maps it to an evidence-free ResultsPush. EXPECT values reach only this
// test-side dir (D-FED.3); the push carries none of it (mapper.go).
func NewRunFunc(cfg ExecConfig) RunFunc {
	return func(ctx context.Context, a *federation.RunAssignment) (federation.ResultsPush, error) {
		// AC-3, the SECOND refusal: the control plane's closed tool schema already refuses to enqueue
		// a `final` request without an artifact_digest, but the runner does not trust that this
		// particular assignment passed through a control plane that still has the check. Refused
		// before any materialization or SUT effect — a final run against no pinned artifact must never
		// execute, not just never be recorded as one.
		if a.Mode == "final" && a.ArtifactDigest == "" {
			return federation.ResultsPush{}, fmt.Errorf("refused: a final run requires an artifact digest")
		}
		// AC-4/AC-4b: a `final` or `scheduled` assignment must arrive SEALED — EXPECT must never
		// cross to this executor in the clear for a certifying run. The control plane's own pickup
		// already refuses to hand one out unsealed (defense in depth, both there and here); this is
		// the runner's own check on the assignment it was actually given, same posture as the digest
		// check just above. `build` assignments are unchanged — sealing there stays a fallback to
		// clear for a keyless executor, never a fault.
		// a `rehearsal` (the draft certification set, run before sealing) is held to it too.
		if a.Mode == "final" || a.Mode == "scheduled" || a.Mode == "rehearsal" {
			for _, sc := range a.Scenarios {
				if sc.Sealed == nil {
					return federation.ResultsPush{}, fmt.Errorf("refused: a %s run's scenarios must arrive sealed", a.Mode)
				}
			}
		}
		// ARGUS-CMP-3: a `compare` run carries its set in CompareScenarios (the ordinary
		// list is empty, so an executor that does not know the field has nothing to run), and the same
		// rule holds for it: every body arrives sealed, never in the clear.
		scenarios := a.Scenarios
		if a.Mode == report.ModeCompare {
			scenarios = a.CompareScenarios
			for _, sc := range scenarios {
				if sc.Sealed == nil {
					return federation.ResultsPush{}, fmt.Errorf("refused: a %s run's scenarios must arrive sealed", a.Mode)
				}
			}
		}
		// Artifact measurement: what is the SUT actually running? Taken HERE, after the cheap refusals
		// and before materialize / any scenario / any SUT effect, so a refused run does nothing at
		// all. A proven mismatch refuses; anything unmeasurable proceeds and is recorded as such.
		var meas *artifactmeasure.Measurement
		if a.Mode == "final" || a.Mode == "scheduled" || a.Mode == report.ModeCompare {
			m := takeMeasurement(ctx, cfg, a)
			if rerr := artifactmeasure.RefuseIfMismatch(m); rerr != nil {
				return federation.ResultsPush{}, rerr
			}
			meas = &m
		}
		// A DIRECT run pre-mints its id so the W1 fence begin + this push share it (§D-3.1.2);
		// federated assignments carry none → mint here as before.
		runID := a.RunID
		if runID == "" {
			runID = argus.NewRunID()
		}
		// VR9-T1 — THE CLOCK STARTS HERE, BEFORE materialize. Materialisation and setup are INSIDE the
		// run's wall clock by the requirement's own rule: a run that spent its time preparing spent it
		// on this run, and Σ sc.DurationMs would omit exactly that.
		t0 := time.Now().UTC()
		// Migration 050: read WHICH SUT REVISION THIS RUN IS ABOUT TO EXERCISE, here, before any work.
		// Measured at the start rather than the end so the recorded revision is the one the run began
		// against; a redeploy landing mid-run is a different fact, and the marker path is what reports it.
		sutFP, sutFPAt := probeSutFingerprint(ctx, cfg.SutFingerprint)
		dir, cleanup, openFailures, err := materialize(cfg.ResultsRoot, runID, scenarios, cfg.X25519Priv)
		if err != nil {
			return federation.ResultsPush{}, fmt.Errorf("materialize: %w", err)
		}
		defer cleanup()
		// Migration 077: which VERSION of each namespace this run touches. Read here, after the scenarios are
		// opened (their ids and tags name the targets, hence the namespaces) and before toolcoreRun does
		// anything to the system. NOT evidence: it is carried to the push below, after bindMeasurement's inputs.
		versionReadings := takeVersionReadings(ctx, cfg, testTargetDecl(cfg.ConfigPath), scenarioRefsIn(dir), os.Getenv("ARGUS_SUT_NAMESPACE"))

		// The set is pre-scoped by PickupNext, so run unfiltered over the materialized dir.
		env := execEnv(cfg, dir)
		env.RunMode = reportMode(a)
		// ARGUS-CMP-11: the target a comparison member names rides the assignment; only a `compare` run reads it.
		if a.Mode == report.ModeCompare {
			env.CompareTarget = a.CompareTarget
		}
		if _, _, rerr := toolcoreRun(env, runID, "", "", ""); rerr != nil {
			return federation.ResultsPush{}, fmt.Errorf("run: %w", rerr)
		}

		rep, err := readReport(filepath.Join(cfg.ResultsRoot, env.Instance, "report.json"))
		if err != nil {
			return federation.ResultsPush{}, fmt.Errorf("read report: %w", err)
		}
		// R4-2: build the run-scoped Grafana deep-link (var-argus_instance=<instance>&var-current_run=<runID>)
		// so a CLOUD run's ledger row links to ITS run under the correct instance — the direct path already
		// does this via RunDashboardURL; the old cfg.DeepLinkBase was never set, so cloud deep_links were empty.
		deepLink := toolcore.RunDashboardURL(env, runID)
		push := mapReport(rep, runID, a.RunRequestID, a.Scope, a.SetHash, deepLink, a.Annotations, t0, time.Now().UTC(), a.ArtifactDigest)
		// Set here rather than threaded through mapReport: mapReport maps the REPORT, and the SUT
		// revision is not in the report — it is measured against the SUT, not produced by the run.
		// Both fields or neither; probeSutFingerprint guarantees that pairing.
		push.SutFingerprint, push.SutFingerprintAt = sutFP, sutFPAt
		// Migration 077: after the report is mapped and before bindMeasurement, which neither reads nor hashes it.
		push.VersionReadings = versionReadingsRaw(versionReadings)
		// ARGUS-CMP-3: a compare run, and only a compare run, carries the output digests, their root and
		// the environment fingerprint on the push. Bodies and header values have no field here.
		if a.Mode == report.ModeCompare {
			attachCompareOutputs(&push, rep)
		}
		bindMeasurement(&push, meas)
		// AC-4b: a scenario whose sealed body failed to open (wrong key, wrong run_id, tampered) NEVER
		// reached the materialized dir, so toolcore.Run never saw it and never counted it — it must
		// surface HERE, by its registry path, as its own errored scenario, or it silently vanishes from
		// the run's tallies instead of failing loudly.
		for _, f := range openFailures {
			push.Scenarios = append(push.Scenarios, federation.ScenarioResult{
				ID: f.Path, Outcome: "errored", Summary: f.Err.Error(),
			})
			push.Tallies.Errored++
			push.Tallies.Total++
		}
		if len(openFailures) > 0 {
			push.Status = "failed"
			push.Outcome = "failed"
			bindMeasurement(&push, meas)
		}
		return push, nil
	}
}

// reportMode is the mode the report this run writes records: the assignment's own, so a
// final, scheduled or rehearsal run is never stamped as a local "ci" run. A DIRECT run (no run request,
// no mode: the builder started it here) returns "" and keeps the "ci" stamp. A federated assignment that
// arrived with no mode is "unknown", which nothing that reads a report treats as a build run.
func reportMode(a *federation.RunAssignment) string {
	if a.Mode == "" && a.RunRequestID != "" {
		return "unknown"
	}
	return a.Mode
}

// scenarioOpenFailure names ONE scenario (by its wire registry Path — the only identity the executor
// has for a body it could not open; the author's own **ID** metadata lives inside the very text that
// failed to decrypt) and the exact error OpenBody gave.
type scenarioOpenFailure struct {
	Path string
	Err  error
}

// materialize writes the scenario bodies into a fresh dir under <base>/materialized/<runID>, built via
// a temp dir + rename so a reader never sees a partial set (D-FED.3 atomic materialization). Returns
// the dir + a cleanup + any scenarios whose Sealed body FAILED to open (AC-4b) — never written, named
// by their registry Path, so the caller can fail them by name rather than silently shrink the run.
//
// priv is the executor's X25519 private key (nil when none minted — every scenario is then a clear
// Body by construction, since SealAssignmentScenarios never seals for a keyless executor).
// probeSutFingerprint reads the SUT's current deployment fingerprint for migration 050's per-run
// record. It returns ("", nil) — NOT MEASURED — for every way the reading can fail to be trustworthy:
// no probe configured, a probe error, or an empty/unusable fingerprint.
//
// ⛔ A FAILED PROBE MUST NOT DEGRADE INTO A GUESS, AND MUST NOT FAIL THE RUN. Returning the last known
// fingerprint, or the annotation's commit, would put a revision the executor did not measure into a
// column whose whole contract is "this is what I measured" — and it would be wrong in precisely the
// case the column exists to catch, an executor that missed a redeploy. Blank is the honest answer.
// Equally, a SUT that answers tests but not its health probe has not had a bad run: the run stands,
// the measurement is simply absent.
//
// The two return values move together by construction — a caller cannot end up with a fingerprint and
// no timestamp, which is the state the panel has no honest way to render.
func probeSutFingerprint(ctx context.Context, fp FingerprintFunc) (string, *time.Time) {
	if fp == nil {
		return "", nil
	}
	s, err := fp(ctx)
	if err != nil {
		return "", nil
	}
	if s = strings.TrimSpace(s); s == "" {
		return "", nil
	}
	at := time.Now().UTC()
	return s, &at
}

func materialize(base, runID string, scenarios []federation.ScenarioPayload, priv *ecdh.PrivateKey) (dir string, cleanup func(), openFailures []scenarioOpenFailure, err error) {
	root := filepath.Join(base, "materialized")
	final := filepath.Join(root, runID)
	tmp := final + ".tmp"
	_ = os.RemoveAll(tmp)
	_ = os.RemoveAll(final)
	// create the temp dir UP FRONT so a 0-scenario run (e.g. a run-request whose scenario_ref matched
	// nothing in the registry) still materializes an empty set instead of failing the rename below with
	// "no such file or directory" (found live at the M3-alpha JOIN, 2026-07-14).
	if err = os.MkdirAll(tmp, 0o755); err != nil {
		return "", nil, nil, err
	}
	for _, sc := range scenarios {
		body := sc.Body
		if sc.Sealed != nil {
			// AC-4b: opened IN MEMORY, before anything about this scenario touches the run directory.
			// A body that fails to open (wrong key, wrong run_id, tampered ciphertext) is named here and
			// skipped — NEVER written, partial or otherwise; the caller turns openFailures into the
			// scenario's own errored result. It must never abort materializing the REST of the run.
			opened, operr := openSealedBody(priv, *sc.Sealed, runID)
			if operr != nil {
				openFailures = append(openFailures, scenarioOpenFailure{Path: sc.Path, Err: operr})
				continue
			}
			body = opened
		}
		// keep the scenario's registry path shape under the temp dir; reject path escapes.
		clean := filepath.Clean("/" + sc.Path) // force-absolute then strip, defeats ../ escapes
		dst := filepath.Join(tmp, clean)
		if err = os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", nil, nil, err
		}
		if err = os.WriteFile(dst, []byte(body), 0o644); err != nil {
			return "", nil, nil, err
		}
	}
	if err = os.MkdirAll(root, 0o755); err != nil {
		return "", nil, nil, err
	}
	if err = os.Rename(tmp, final); err != nil {
		return "", nil, nil, err
	}
	return final, func() { _ = os.RemoveAll(final) }, openFailures, nil
}

// openSealedBody opens one Sealed body under priv, scoped to runID (federation.OpenBody) — refusing
// up front, with the SAME distinguished shape OpenBody itself would fail with, when this executor has
// no X25519 identity to open anything at all (nil priv). A missing key is not a different code path
// from a wrong one: both are "this executor cannot open this body", named the same way.
func openSealedBody(priv *ecdh.PrivateKey, sealed federation.SealedBody, runID string) (string, error) {
	if priv == nil {
		return "", fmt.Errorf("sealedexpect: this executor has no X25519 identity to open a sealed body")
	}
	return federation.OpenBody(priv, sealed, runID)
}

func readReport(path string) (*report.Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r report.Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
