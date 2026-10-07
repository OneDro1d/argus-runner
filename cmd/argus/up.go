package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/router"
	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// `argus up` — THE FIRST-RUN ONBOARDING VERB.
//
// It is a thin composer, not a reimplementation: everything it does is build the
// `onboarding/onboard.sh` invocation an operator would otherwise type by hand (mirroring the exact
// flag names onboard.sh's own `case "$1"` block declares — onboard.sh:991-1020) and, when asked,
// translate onboard.sh's opt-in JSON step marker (onboarding/lib/json-step.sh, fd 5) into a
// line-per-event `--json` protocol an agent can read without scraping the human transcript.
//
// ⛔ FIRST RUN ONLY, UNTIL runUp's OWN DISPATCH SAYS OTHERWISE. A second invocation against an
// instance that already has an installed manifest (internal/updatecmd.Manifest, written by
// onboarding's last step or a prior `update commit`) is a different shape entirely — a plan/apply
// update, not a fresh onboard — and runUp hands it to upSecondRun (up_second_run.go), which execs
// the SAME block the control plane's dashboard renders (updatecmd.RenderBlock) rather than
// reimplementing it.
//
// ⛔ AND IT OWNS ITS ARGV (ownargv.go). --config, --tier, --control-plane, --kube-context and
// --kubeconfig are ALSO common-flagset names (main.go's commonFlags.bind), so the partition would
// route them away exactly as V19-010 describes for `update`/`replay`/`anchor`. See ownargv_test.go.
type upArgs struct {
	ControlPlane string // --control-plane (falls back to ARGUS_CP_URL, the same env var onboard.sh itself reads — onboard.sh:880/2017/2195)
	Token        string // --token: an author session token for cloud onboarding + a future runner-id mint (falls back to ARGUS_CP_AUTHOR_TOKEN, formerly ARGUS_CP_TOKEN, onboard.sh:962/1542)
	Tier         string // --tier auto|compose|k3d|aks ("auto" means: do not pass --tier at all, so onboard.sh's own inference runs — onboard.sh:1025 on)
	ConfigPath   string // --config (default "argus-config.yaml"); its DIRECTORY is onboard.sh's --product-dir
	// ScenariosDir is a deviation from the sketch this pass was handed: onboard.sh REQUIRES
	// --scenarios-dir and its own D4 holdout guard (onboard.sh:2229-2231) refuses a value that is
	// identical to, or nested under, --product-dir. A single --config file names only one
	// directory, so this pass cannot derive a scenarios dir from --config alone without either
	// guessing at a discovery convention with no precedent (rejected — CLAUDE.md: no guessing) or
	// giving the operator an explicit escape hatch. --scenarios-dir is that hatch; see
	// resolveScenariosDir for the (documented, precedented) default when it is left unset.
	ScenariosDir string
	RunnerID     string // --runner-id: this instance's identity, threaded to onboard.sh's own --instance-id (see resolveRunnerID)
	// Image is --image: the executor image onboard.sh should onboard (first run) or the reference a
	// second run pulls fresh and checks for an update against (up_second_run.go's secondRunInstance).
	// Falls back to ARGUS_MCP_IMAGE, the SAME env var onboard.sh itself reads (onboard.sh:1212/2474) —
	// one flag, not a second resolution invented for the second-run path alone.
	Image string
	// ImageGiven is true only when --image was typed on the command line with a value — not taken from ARGUS_MCP_IMAGE,
	// which the runbook exports for onboarding and so is set, unchosen, in the shell a second run starts from (AC-D53:
	// an unconfirmed rollback is re-run forward only on an explicit --image — up_second_run.go); not an empty one.
	ImageGiven  bool
	KubeContext string // --kube-context
	Kubeconfig  string // --kubeconfig
	JSON        bool   // --json: emit the machine-readable protocol (upjson.go) instead of onboard.sh's own transcript
	Yes         bool   // --yes: accept every NON-SECRET default instead of asking (see yesCanAnswer)
	// T2.2: forwarded VERBATIM to onboard.sh's own --storage-class / --results-access-mode, only
	// when given — the same "empty means say nothing" shape every other optional pass-through in
	// this struct (Image, KubeContext, Kubeconfig) already follows.
	StorageClass      string // --storage-class
	ResultsAccessMode string // --results-access-mode
	// the observability volumes' StorageClass, forwarded to onboard.sh --obs-storage-class.
	ObsStorageClass string // --obs-storage-class
}

// upFlagSet registers every `up` flag, each bound to its field of a. Its own function so a test can drive
// the REAL flag parsing (a flag registered but bound to a throwaway variable would otherwise pass every test
// that builds an upArgs by hand: see TestUpFlags_ObsStorageClassParsedIntoUpArgs).
func upFlagSet(a *upArgs) *flag.FlagSet {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.StringVar(&a.ControlPlane, "control-plane", os.Getenv("ARGUS_CP_URL"), "the control-plane URL for a cloud onboarding (else ARGUS_CP_URL)")
	// ⛔ NO DEFAULT HERE (#44). flag's own usage text (printed by --help and by every flag error, in
	// --json mode too) renders a StringVar's default verbatim as `(default "...")` — a default of
	// os.Getenv("ARGUS_CP_AUTHOR_TOKEN"/"ARGUS_CP_TOKEN") put the token in that text. The env pair is
	// read AFTER Parse instead, below, only when the flag itself was left empty.
	fs.StringVar(&a.Token, "token", "", "an author session token for cloud onboarding (else ARGUS_CP_AUTHOR_TOKEN, formerly ARGUS_CP_TOKEN)")
	fs.StringVar(&a.Tier, "tier", "auto", "auto | compose | k3d | aks — \"auto\" lets onboard.sh infer the tier itself")
	fs.StringVar(&a.ConfigPath, "config", "argus-config.yaml", "the argus-config.yaml of the SUT under test; its directory is the product folder")
	fs.StringVar(&a.ScenariosDir, "scenarios-dir", "", "the scenarios folder onboard.sh reads (default: a \"scenarios\" folder beside --config's directory)")
	fs.StringVar(&a.RunnerID, "runner-id", "", "this instance's runner id (threaded to onboard.sh's --instance-id)")
	fs.StringVar(&a.Image, "image", os.Getenv("ARGUS_MCP_IMAGE"), "the executor image to onboard, or to check a second run against (else ARGUS_MCP_IMAGE)")
	fs.StringVar(&a.KubeContext, "kube-context", "", "k3d/aks: the kubectl context")
	fs.StringVar(&a.Kubeconfig, "kubeconfig", "", "k3d: an explicit kubeconfig path")
	fs.StringVar(&a.StorageClass, "storage-class", "", "the results volume's StorageClass, forwarded verbatim to onboard.sh --storage-class (else the tier default)")
	fs.StringVar(&a.ResultsAccessMode, "results-access-mode", "", "the results volume's access mode, forwarded verbatim to onboard.sh --results-access-mode — ReadWriteMany | ReadWriteOnce")
	fs.StringVar(&a.ObsStorageClass, "obs-storage-class", "", "the observability volumes' StorageClass (Loki's data, the Pushgateway's file; ReadWriteOnce), forwarded verbatim to onboard.sh --obs-storage-class (else managed-csi on aks, the cluster default StorageClass elsewhere)")
	fs.BoolVar(&a.JSON, "json", false, "emit one JSON object per line (upjson.go) instead of onboard.sh's own transcript")
	fs.BoolVar(&a.Yes, "yes", false, "accept every non-secret default instead of asking (see yesCanAnswer)")
	return fs
}

// cmdUp parses `up`'s own argument vector (ownsArgv hands it the whole thing) and runs it.
func cmdUp(args []string) int {
	var a upArgs
	fs := upFlagSet(&a)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if a.Token == "" {
		a.Token = envname.Lookup(envname.CPAuthorToken, envname.CPAuthorTokenDeprecated)
	}
	fs.Visit(func(f *flag.Flag) {
		// ⛔ AN EMPTY VALUE NAMES NO IMAGE: `--image "$VAR"` with VAR unset is not a choice to leave an unconfirmed
		// rollback — counted as one, the fallback took the marker's older image and ran the forward block
		if f.Name == "image" && strings.TrimSpace(a.Image) != "" {
			a.ImageGiven = true
		}
	})
	if rest := fs.Args(); len(rest) > 0 {
		// ⛔ NEVER emitErr HERE (#48b). emitErr's emit() pretty-prints with json.MarshalIndent — a
		// multi-line blob on stdout, breaking "every --json line is one JSON object" for the one kind
		// of usage error that survives flag.Parse itself (an extra positional argument). failUp goes
		// through the SAME one-line event writer every other refusal in this file uses.
		return failUp(os.Stdout, a.JSON, "usage", fmt.Sprintf("unexpected argument %q", rest[0]))
	}
	return runUp(a)
}

// runUp is the second-run/first-run dispatch.
//
// ⛔ ONE INSTANCE-ID RESOLUTION, REUSED, NOT A SECOND ONE GUESSED. resolveRunnerID is the same
// function upFirstRun itself calls; calling it here again to LOOK is harmless (it has no side
// effects — the hint/fail events it can also return are for the caller to EMIT, which only the run
// that actually takes that path does).
//
// ⛔ NO ID RESOLVED AT ALL → UNAMBIGUOUSLY A FIRST RUN. Nothing durable can exist under a name that
// was never chosen, so a manifest read against an empty id would be asking a question that cannot
// have an answer — go straight to upFirstRun instead.
func runUp(a upArgs) int {
	id, _, _ := resolveRunnerID(a)
	if id == "" {
		return upFirstRun(a)
	}
	routerState := router.StateDir()
	m, err := updatecmd.ReadManifest(routerState, id)
	if err != nil {
		return failUp(os.Stdout, a.JSON, "up", fmt.Sprintf("could not read the installed manifest for %s: %v", id, err))
	}
	// ⛔ AN ABSENT MANIFEST IS NOT A SECOND RUN. ReadManifest's own contract (internal/updatecmd/
	// manifest.go): absent comes back as a zero Manifest and a nil error — every instance's FIRST
	// `up` has no manifest yet, and that is a fact about it having never onboarded, not a failure.
	if m.InstanceID != "" {
		return upSecondRun(a, m)
	}
	return upFirstRun(a)
}

// upFD5Tee is a TEST-ONLY seam: when a test sets it, upFirstRun also copies onboard.sh's raw,
// untranslated fd-5 lines to it — the wire onboard.sh actually wrote, alongside the translated
// --json protocol this file produces, so a test can prove the translation adds the "ok" states
// rather than merely trusting it did. nil (and never touched) in every real run.
var upFD5Tee io.Writer

// upOnboardArgs is the onboard.sh argument vector upFirstRun execs: every optional pass-through is
// forwarded only when given, so an unset one says nothing and onboard.sh's own default stands. It
// is a function (not inline in upFirstRun) so that a dropped pass-through turns a test red — T2.2's
// --storage-class forwarding was reviewed deleting that line with nothing noticing.
func upOnboardArgs(a upArgs, productDir, scenariosDir, runnerID string) []string {
	args := []string{"--product-dir", productDir, "--scenarios-dir", scenariosDir}
	if a.Tier != "" && a.Tier != "auto" {
		args = append(args, "--tier", a.Tier)
	}
	if a.ControlPlane != "" {
		args = append(args, "--control-plane", a.ControlPlane)
	}
	if a.Image != "" {
		args = append(args, "--image", a.Image)
	}
	if runnerID != "" {
		args = append(args, "--instance-id", runnerID)
	}
	if a.KubeContext != "" {
		args = append(args, "--kube-context", a.KubeContext)
	}
	if a.Kubeconfig != "" {
		args = append(args, "--kubeconfig", a.Kubeconfig)
	}
	if a.StorageClass != "" {
		args = append(args, "--storage-class", a.StorageClass)
	}
	if a.ResultsAccessMode != "" {
		args = append(args, "--results-access-mode", a.ResultsAccessMode)
	}
	if a.ObsStorageClass != "" {
		args = append(args, "--obs-storage-class", a.ObsStorageClass)
	}
	return args
}

// upFirstRun composes and execs the onboard.sh invocation for a fresh instance.
func upFirstRun(a upArgs) int {
	stdout := io.Writer(os.Stdout)

	productDir, perr := resolveProductDir(a, stdout, os.Stdin)
	if perr != nil {
		return failUp(stdout, a.JSON, "product-folder", perr.Error())
	}
	scenariosDir := resolveScenariosDir(a, productDir)

	runnerID, hint, fail := resolveRunnerID(a)
	if fail != nil {
		emitStepOrPlain(stdout, a.JSON, *fail)
		return exitUsage
	}
	if hint != nil {
		emitStepOrPlain(stdout, a.JSON, *hint)
	}

	onboardPath := filepath.Join("onboarding", "onboard.sh")
	if _, statErr := os.Stat(onboardPath); statErr != nil {
		return failUp(stdout, a.JSON, "onboard", fmt.Sprintf(
			"could not find %s (%v) — run `argus up` from the kit directory `argus init` created", onboardPath, statErr))
	}

	onboardArgs := upOnboardArgs(a, productDir, scenariosDir, runnerID)

	cmd := exec.Command("bash", append([]string{onboardPath}, onboardArgs...)...)
	cmd.Env = onboardEnv(a, os.Environ())

	// #46(record): a fresh temp file onboard.sh's own "# ── #46(record)" block (just before its
	// "done" json_step) writes ONE JSON object to, reporting what it ACTUALLY installed — the
	// instance id (onboard.sh can rename it on a name collision), the tier (resolved internally
	// when --tier was "auto"), and the executor (always known there; --image can be empty here).
	// Same channel, same env var, in both plain and --json mode — recordInstalledManifest below
	// reads it back regardless of which one ran. Removed on every path this function returns
	// through, whether or not onboard.sh ever wrote to it.
	recordPath, recordCleanup := newUpRecordFile()
	defer recordCleanup()
	if upRecordPathTee != nil {
		upRecordPathTee(recordPath)
	}
	if recordPath != "" {
		cmd.Env = append(cmd.Env, "ARGUS_UP_RECORD="+recordPath)
	}

	if !a.JSON {
		// PLAIN MODE: onboard.sh's own stdout/stderr pass through unchanged — `up` must not swallow
		// or reformat the human transcript an operator pasting the command would otherwise see.
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		runErr := cmd.Run()
		code := exitCodeOf(runErr)
		recordInstalledManifest(runnerID, a, code, recordPath)
		return code
	}

	// JSON MODE: onboard.sh's own stdout carries no protocol at all — its human transcript is
	// discarded here so every line THIS process writes to its OWN stdout is one JSON object,
	// which is the contract up_json_protocol_test.go's shape test asserts.
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	cmd.Env = append(cmd.Env, "ARGUS_JSON_STEPS=1")
	// ⛔ #47 — DETACHED FROM THE CALLING TERMINAL, ON PURPOSE. `/dev/tty` (onboard.sh's HAS_TTY probe,
	// onboard.sh:976-990) resolves against the process's SESSION, not fd 0/1/2 — so discarding
	// onboard.sh's stdout above does nothing to stop it from opening /dev/tty and blocking on one of
	// its own questions (the tier question, the secrets confirmation, the existing-tunnel question,
	// the cloud workspace choice) whenever the CALLER of `up --json` happens to have a controlling
	// terminal, exactly like a coding agent driving this from an attended shell would. Setsid starts
	// onboard.sh in a NEW session with no controlling terminal at all, so every one of those probes
	// sees none and takes its already-existing non-interactive refusal path instead — which the
	// existing die()-to-json_step wiring (and #48a's "preflight" naming) already turns into a named
	// fail event; there is nothing left here to "relay" that is not that path, since none of these are
	// questions `up` itself has any protocol seam to answer from stdin. (A no-op on Windows, which has
	// no controlling terminal in this sense — see up_detach_windows.go.)
	detachFromTerminal(cmd)

	fd5r, fd5w, perr2 := os.Pipe()
	if perr2 != nil {
		return failUp(stdout, true, "onboard", fmt.Sprintf("could not open the JSON step channel: %v", perr2))
	}
	// fd 3 and fd 4 are padding, exactly as the test harness pads them (harness_test.go): onboard.sh's
	// own funnel.sh does `exec 3>&1 4>&2` before any json_step call site can run, so whatever these
	// point at is immediately overwritten and never read. fd 5 (ExtraFiles index 2) is the one that
	// matters — see onboarding/lib/json-step.sh for why it, and not fd 3, carries this channel.
	pad, perr2 := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if perr2 != nil {
		fd5r.Close()
		fd5w.Close()
		return failUp(stdout, true, "onboard", fmt.Sprintf("could not open %s: %v", os.DevNull, perr2))
	}
	cmd.ExtraFiles = []*os.File{pad, pad, fd5w}

	if serr := cmd.Start(); serr != nil {
		fd5r.Close()
		fd5w.Close()
		pad.Close()
		return failUp(stdout, true, "onboard", fmt.Sprintf("could not start onboard.sh: %v", serr))
	}
	fd5w.Close() // our copy — only the child now holds the writable end
	pad.Close()

	// #46(signal): Setsid (above) starts onboard.sh in a NEW session and process group, so a
	// SIGINT/SIGTERM delivered to `up` itself (Ctrl-C, a CI cancellation, an agent giving up) no
	// longer reaches it — a terminal's own job-control signal-fanout, and a plain `kill <pid>`, both
	// only reach processes in the SAME group as the one signalled. Left unforwarded, killing `up`
	// left onboard.sh running on, orphaned, still pulling images or registering an instance nobody is
	// waiting on anymore. Forwarding the SAME signal to the NEGATIVE of onboard.sh's own pid — the
	// standard `kill(-pgid, sig)` idiom — reaches its whole process group (Setsid made it the group
	// leader, so its pid IS the pgid) and everything onboard.sh itself spawned, not just onboard.sh.
	// (A no-op on Windows — see up_detach_windows.go.)
	stopForwarding := forwardSignals(cmd)

	done := make(chan *upStepTranslator, 1)
	go func() {
		tr := &upStepTranslator{w: stdout}
		sc := bufio.NewScanner(fd5r)
		for sc.Scan() {
			line := sc.Bytes()
			if upFD5Tee != nil {
				upFD5Tee.Write(append(append([]byte{}, line...), '\n'))
			}
			var raw onboardFD5Line
			if json.Unmarshal(line, &raw) != nil {
				continue // a malformed line off the wire is not this translator's to fail the run over
			}
			tr.onLine(raw.Step, raw.State, raw.Detail)
		}
		done <- tr
	}()

	runErr := cmd.Wait()
	stopForwarding()
	tr := <-done
	fd5r.Close()
	code := exitCodeOf(runErr)
	// ⛔ EXIT 3 INFERS SUCCESS TOO (#48c). onboard.sh's own comment (onboard.sh:3943's neighbour) is
	// explicit: exit 3 (RUNTIME_NOT_READY) fires AFTER the banner and after this pass's own "done"
	// step start, and "must never be read as onboarding failed" — it is a k8s/compose object still
	// starting, not a broken run. Treating it as anything but success here would leave the FINAL step
	// (today: "done", carrying the dashboard link) stuck at "start" forever on the single most common
	// non-zero exit a real onboard produces.
	if code == exitOK || code == onboardNotReadyExit {
		tr.onExitSuccess()
		// T2.4: the LAST line on a completed onboard is the structured result — read from the record
		// file BEFORE the deferred cleanup removes it.
		res := buildUpResult(runnerID, a, code, recordPath)
		_ = emitStep(stdout, jsonEvent{Step: "result", State: "ok", Result: &res})
	}
	recordInstalledManifest(runnerID, a, code, recordPath)
	return code
}

// onboardNotReadyExit is onboard.sh's own documented exit 3 — "a not-Ready object", never a failure.
const onboardNotReadyExit = 3

// recordInstalledManifest closes #46: onboarding itself wrote no record of what it installed, so a
// repeated `argus up` over the SAME --runner-id found no manifest (updatecmd.ReadManifest) and
// onboarded again — on cloud onboarding, registering a second instance under `<id>-vN`. Writing it
// here, right after a successful first run, is the smallest fix that does not require onboard.sh to
// call back into a NEW writable-mount subcommand of its own for one file.
//
// A silent no-op when there is nothing to record it for (no runner id was ever resolved — runUp's own
// dispatch already treats that as unambiguously a first run every time, so no manifest is needed) or
// the run did not actually finish (neither exitOK nor onboard.sh's own "not ready yet" 3 — the SAME
// two codes onExitSuccess treats as a completed onboard, above).
//
// ⚠ THREE FIELDS ONBOARDING DECIDES FOR ITSELF, NOT THIS PASS. --runner-id can be renamed away from
// on a name collision (onboard.sh's own "<id>-vN" proposal, non-interactive runs auto-accept it);
// --tier, left "auto", is resolved internally from compose/k8s signals; --image can be left empty
// (ARGUS_MCP_IMAGE or onboard.sh's own default then decide it) where a bare second `argus up` needs
// SOME executor to fall back to (secondRunInstance, up_second_run.go). recordPath — onboard.sh's own
// "#46(record)" report, read back by readUpRecord — carries what onboarding ACTUALLY did for all
// three; the argv-derived values below are used only when that report is absent or malformed (an
// older onboard.sh, or a run that died before reaching its own record block).
func recordInstalledManifest(runnerID string, a upArgs, code int, recordPath string) {
	if runnerID == "" || (code != exitOK && code != onboardNotReadyExit) {
		return
	}
	instanceID := runnerID
	tier := a.Tier
	if tier == "" || tier == "auto" {
		tier = "compose"
	}
	executor := a.Image
	if rec, ok := readUpRecord(recordPath); ok {
		instanceID = rec.InstanceID
		tier = rec.Tier
		executor = rec.Executor
	}
	m := updatecmd.Manifest{
		InstanceID: instanceID,
		Tier:       tier,
		Executor:   executor,
		Artefacts:  serviceArtefactsFromConfig(a.ConfigPath),
	}
	_ = updatecmd.WriteManifest(router.StateDir(), m) // best-effort: never turn a successful onboard into a reported failure
}

// serviceArtefactsFromConfig closes the other half of AC-19b gap #40(2): the run view
// (internal/control/runservices.go's installedServiceNames) graphs an instance's services from
// manifest artefacts of Kind "service" — nothing wrote one until now, so every instance fell back to
// the layer graph. This reads the SAME argus-config.yaml `up` itself resolved --config against (never
// a second discovery) and returns one artefact per NAMED target key across http_targets,
// message_broker_targets, database_targets and mcp_targets — the targets a scenario selects with its
// own `- **Target**: <name>` — deduplicated, Version "unknown" (no version to read from a target
// declaration), no Image. When the singular, UNNAMED targets.http is also declared, one more artefact
// is added, named by ITS base_url's hostname: that is what scenarioService (runservices.go) falls
// back to for a scenario with no declared **Target**, so a service reachable only through it still
// gets a node.
//
// A missing or unparsable config (an older config, a config `up` cannot resolve, or one this run's
// own --config never pointed at anything real) returns nil, never an error — the manifest's other
// fields are still written exactly as before this existed.
func serviceArtefactsFromConfig(configPath string) []updatecmd.Artefact {
	cfg, err := config.Load(configPath)
	if err != nil || cfg == nil {
		return nil
	}
	var names []string
	seen := map[string]bool{}
	add := func(n string) {
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		names = append(names, n)
	}
	for n := range cfg.Targets.HTTPTargets {
		add(n)
	}
	for n := range cfg.Targets.MessageBrokerTargets {
		add(n)
	}
	for n := range cfg.Targets.DatabaseTargets {
		add(n)
	}
	for n := range cfg.Targets.MCPTargets {
		add(n)
	}
	if cfg.Targets.HTTP != nil && cfg.Targets.HTTP.BaseURL != "" {
		if u, uerr := url.Parse(cfg.Targets.HTTP.BaseURL); uerr == nil && u.Hostname() != "" {
			add(u.Hostname())
		}
	}
	sort.Strings(names) // deterministic manifest bytes — a Go map's range order is not
	artefacts := make([]updatecmd.Artefact, 0, len(names))
	for _, n := range names {
		artefacts = append(artefacts, updatecmd.Artefact{Kind: "service", Name: n, Version: "unknown"})
	}
	return artefacts
}

// upRecord is onboarding's own report of what it actually installed (onboard.sh's "#46(record)"
// block, written to ARGUS_UP_RECORD just before its "done" json_step): the instance id it ended up
// registering, the tier it resolved, and the executor it pulled — the three fields recordInstalledManifest
// cannot always infer correctly from `up`'s own argv alone.
type upRecord struct {
	InstanceID string `json:"instance_id"`
	Tier       string `json:"tier"`
	Executor   string `json:"executor"`
	// T2.4: where it can be reached — read by buildUpResult for `up --json`'s closing result. URLs and
	// an id only; absent from an older onboard.sh's record, which decodes to "".
	WorkspaceID  string `json:"workspace_id"`
	ControlPlane string `json:"control_plane"`
	Web          string `json:"web"`
	Grafana      string `json:"grafana"`
	ExecutorMCP  string `json:"executor_mcp"`
}

// readUpRecord reads onboard.sh's #46(record) report. A missing or malformed file is not an error
// here — an older onboard.sh writes nothing (ok=false), and a run that died before reaching its own
// record block leaves nothing behind either; both fall back to recordInstalledManifest's own
// argv-derived values, exactly as it behaved before this record channel existed.
func readUpRecord(path string) (rec upRecord, ok bool) {
	if path == "" {
		return upRecord{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return upRecord{}, false
	}
	if jsonErr := json.Unmarshal(b, &rec); jsonErr != nil || rec.InstanceID == "" {
		return upRecord{}, false
	}
	return rec, true
}

// newUpRecordFile allocates a fresh, unique path for onboard.sh's #46(record) report and a cleanup
// func that removes it (and its containing directory) unconditionally — called via `defer` in
// upFirstRun so the file never survives the run, whether or not onboard.sh ever wrote to it.
//
// A failure to create the temp dir is not fatal to `up` itself: it degrades to exactly the pre-#46
// behaviour (recordPath == "", onboard.sh's own `if [ -n "${ARGUS_UP_RECORD:-}" ]` guard never fires,
// readUpRecord sees an empty path) rather than aborting an otherwise-successful onboard over one
// best-effort report.
func newUpRecordFile() (path string, cleanup func()) {
	dir, err := os.MkdirTemp("", "argus-up-record-*")
	if err != nil {
		return "", func() {}
	}
	return filepath.Join(dir, "record.json"), func() { os.RemoveAll(dir) }
}

// upRecordPathTee is a TEST-ONLY seam: when set, upFirstRun reports the record path it created here,
// so a test can assert it does not survive the run — nil, and never touched, in every real run.
var upRecordPathTee func(path string)

// onboardEnv builds the environment onboard.sh runs under: base (the operator's own environment),
// with --token threaded in under BOTH the new name (ARGUS_CP_AUTHOR_TOKEN) and the old one
// (ARGUS_CP_TOKEN) so the credential reaches onboard.sh the SAME way it already reads one supplied
// only through the environment (onboard.sh:962's own comment: "An author PAT may arrive via the
// environment instead of --token, so it never lands in argv/ps") — never as a literal argv element,
// which would put it in a process listing (#44: onboarding never got --token at all before this).
//
// ⛔ BOTH NAMES, FOR THIS RELEASE ONLY. onboard.sh now reads ARGUS_CP_AUTHOR_TOKEN first and falls
// back to ARGUS_CP_TOKEN itself (its SESSION_TOKEN resolution calls envname_lookup, the shell twin
// of this package, from onboarding/lib/envname.sh), so passing only the new name would already work
// against the onboard.sh shipped alongside this binary — but
// `up` can also be pointed at an OLDER checked-out onboard.sh (a carried-forward kit, a pinned
// tag) that has never heard of the new name. Setting both means this binary's `up` keeps working
// against that older onboard.sh too, for exactly as long as the old name is still read anywhere.
func onboardEnv(a upArgs, base []string) []string {
	env := append([]string{}, base...)
	if a.Token != "" {
		env = append(env, envname.CPAuthorToken+"="+a.Token, envname.CPAuthorTokenDeprecated+"="+a.Token)
	}
	return env
}

// exitCodeOf translates a `cmd.Wait` error into the exit code a caller of `up` should see: the real
// exit code of onboard.sh, not a generic 1, since onboard.sh's own `exit 3` for "not ready yet" is
// PART of its contract (onboard.sh:3142 and its neighbouring comment) and must not be flattened.
func exitCodeOf(runErr error) int {
	if runErr == nil {
		return exitOK
	}
	if ee, ok := runErr.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return exitErr
}

// failUp reports a failure THIS file detected before (or instead of) execing onboard.sh at all —
// e.g. no product folder could be resolved. It renders as a `fail` step in --json mode and a plain
// line otherwise, then the caller returns exitUsage.
func failUp(w io.Writer, jsonMode bool, step, detail string) int {
	emitStepOrPlain(w, jsonMode, jsonEvent{Step: step, State: "fail", Detail: detail})
	return exitUsage
}

// emitStepOrPlain renders one event the way `up`'s own two output modes require: the --json
// protocol line in JSON mode, a plain "up: <detail>" line otherwise. Errors writing to w are not
// this helper's to handle — a caller that cares already has runErr/exit code to report.
func emitStepOrPlain(w io.Writer, jsonMode bool, ev jsonEvent) {
	if jsonMode {
		_ = emitStep(w, ev)
		return
	}
	fmt.Fprintf(w, "up: %s: %s\n", ev.Step, ev.Detail)
}

// resolveProductDir finds the directory onboard.sh's --product-dir will point at.
//
// The ONLY discovery this pass performs is the one onboard.sh performs itself (need_dir +
// onboard.sh:1134's `-f "$PRODUCT_DIR/argus-config.yaml"` check): does a file exist at --config? If
// so, its directory IS the product folder — nothing is guessed beyond that. When it does not, this
// is the ONE question this pass wires end to end (see upjson.go's askOrDefault).
func resolveProductDir(a upArgs, w io.Writer, r io.Reader) (string, error) {
	if _, err := os.Stat(a.ConfigPath); err == nil {
		return filepath.Dir(a.ConfigPath), nil
	}
	q := jsonQuestion{
		Key:    "product-folder",
		Prompt: fmt.Sprintf("no argus-config.yaml at %q — where is the product folder?", a.ConfigPath),
	}
	cwd, cerr := os.Getwd()
	if cerr == nil {
		q.Default = cwd
	}
	return askOrDefault(w, r, a.Yes, q, a.JSON, q.Default)
}

// resolveScenariosDir applies the documented default when --scenarios-dir was not given: a
// "scenarios" folder BESIDE (not under) the product folder, mirroring the one shipped launcher that
// already derives both from a single root — onboarding/test-orderservice.sh:28-29's
// PRODUCT_DIR="$KIT/product-agent" / TEST_DIR="$KIT/test-agent" sibling layout — generalised from a
// fixed kit root to --config's own parent. A layout that instead nests scenarios INSIDE the product
// folder trips onboard.sh's own D4 holdout guard (onboard.sh:2230); --scenarios-dir is the escape
// hatch for that shape, not a second guess by this function.
func resolveScenariosDir(a upArgs, productDir string) string {
	if a.ScenariosDir != "" {
		return a.ScenariosDir
	}
	return filepath.Join(filepath.Dir(productDir), "scenarios")
}

// resolveRunnerID decides what (if anything) gets threaded into onboard.sh's own --instance-id.
//
// There is no automatic mint attempt in this pass. cmdRunnerID's mint call (runnerid_cmd.go:19,
// AC-17) itself REQUIRES --instance-id — it BINDS a fresh runner id to an instance name that already
// exists, and a first run has none yet (onboard.sh's own resolve_instance_name, onboard.sh:1742, may
// still rename whatever was requested — UC122). Minting here would mean this file guessing at a name
// onboard.sh has not resolved, which is exactly the kind of invention this pass was told not to do.
// So minting is not attempted: the operator is pointed at the web UI (Settings → Environments →
// Mint runner id) or `argus runner-id mint` instead, and a later pass —
// once it has a seam into the FINAL resolved instance name — can wire the real attempt.
func resolveRunnerID(a upArgs) (id string, hint *jsonEvent, fail *jsonEvent) {
	if a.RunnerID != "" {
		return a.RunnerID, nil, nil
	}
	hint = &jsonEvent{
		Step:  "runner-id",
		State: "skip",
		Detail: "no --runner-id given, and minting one automatically needs an instance name onboard.sh " +
			"has not resolved yet — mint one in the web UI (Settings → Environments → Mint runner id) or with `argus runner-id mint`, then pass --runner-id and re-run",
	}
	// onboard.sh tolerates a missing --instance-id ONLY when --control-plane is unset (onboard.sh:
	// 1139-1140: "cloud onboarding (--control-plane) requires --instance-id <name>"). With a control
	// plane and no runner id this pass can supply, proceeding would just hand onboard.sh an empty
	// --instance-id and let ITS die() fire several steps in — surfacing it here, before anything
	// runs, is the honest version of the same refusal.
	if a.ControlPlane != "" {
		fail = &jsonEvent{
			Step:   "runner-id",
			State:  "fail",
			Detail: "cloud onboarding (--control-plane) requires a runner id — mint one in the web UI (Settings → Environments → Mint runner id) or with `argus runner-id mint`, and re-run with --runner-id <id>",
		}
		return "", hint, fail
	}
	return "", hint, nil
}
