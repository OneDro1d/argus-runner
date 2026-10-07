package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	argusrun "github.com/OneDro1d/argus-runner/internal/argus"
	"github.com/OneDro1d/argus-runner/internal/buildinfo"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/doctor"
	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/onboard"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// `argus doctor` — the first-run diagnosis. `preflight` asks "can this machine DEPLOY an instance";
// `doctor` asks "what will the first RUN from this machine trip over" — and names the cause where
// the run would have named the symptom.
//
// ⛔ READ-ONLY by construction, like preflight: every check here reads an env var, a file or a
// directory listing — and one, executor-version, reads GET /api/instances with the credential already
// present, and, when an update block it returns runs Docker, asks `docker version` whether a daemon
// answers. A session's access token lives 15 minutes, so it is usually expired; doctor then renews it
// IN MEMORY (one refresh_token grant, the new access token used for that one read and dropped) and
// writes nothing. The control plane does not rotate refresh tokens (oauth/endpoints.go grantRefresh);
// if it ever does, the rotated one is saved exactly as every cloud-* command saves it, because dropping
// it would sign the machine out. It is what you run when you do not yet know whether it is safe to
// act, so it must never be the thing that acts.
//
// ⛔ A credential VALUE never enters the report. The value is classified (internal/doctor.Classify)
// into a kind, a scope and an expiry, and only those are printed. A transcript is stored. A
// --token-file's value is read the same way, and is never presented to anyone: the file is checked
// BEFORE it is handed off, and sending it anywhere would be the hand-off.
const doctorUsage = "usage: argus doctor [--config <argus-config.yaml>] [--scenarios <dir>] [--control-plane <url>] [--instance <id>]\n" +
	"                    [--token-file <path> [--token-for author|runner]]\n" +
	"       argus doctor --tester [--app <app>] [--env-file <path>] [--control-plane <url>] [--instance <id>]\n" +
	"                    [--kubeconfig <path>] [--kube-context <ctx>] [--skip-cluster]\n" +
	"  doctor --tester  Onboarding evidence, one line per phase, each PASS, WARN, FAIL or SKIP with what was found\n" +
	"                  (JSON report on stdout, the same lines on stderr; exit 4 on any FAIL, SKIP never fails):\n" +
	"                    P3 tester-token      the token variable is set in the env file (yes/no; never the value)\n" +
	"                    P3 tester-tools      tools/list with that token: how many tools, how many author tools\n" +
	"                    P3 tester-workspace  the workspace the token belongs to\n" +
	"                    P4 tester-cluster    kubectl reaches the cluster (--kubeconfig / --kube-context)\n" +
	"                    P5 tester-namespace  argus-inst-<instance> exists (--instance, or the only registered one)\n" +
	"                    P5 tester-pullsecrets every imagePullSecret the executor Deployment names exists (name only, never the\n" +
	"                                         Secret's contents); SKIP, not PASS, when that cannot be determined\n" +
	"                    P5 tester-executor   registered and polling, last poll under 60 s ago (as cloud-executor-status)\n" +
	"                    P5 tester-executor-version  the executor's version against the control plane's floors (the judgment of\n" +
	"                                         executor-version, from the rows tester-workspace read); warn when below the\n" +
	"                                         recommended floor, SKIP when the token check failed\n" +
	"                  The token is read from ~/.config/argus/tester.env (what `argus tester init` writes and the\n" +
	"                  headersHelper reads), not from the environment. It replaces the first-run checks below.\n" +
	"  Without --tester:\n" +
	"  Reports, as JSON on stdout (plus the same one-line-per-check form as `lines`, and on stderr), what a first run from this machine would trip over — the KIND of control-plane\n" +
	"  credential present (author PAT, 15-minute access token, runner token) and whether it has expired,\n" +
	"  the local hat map (ARGUS_TOKEN / ARGUS_RUNNER_TOKEN / ARGUS_EXECUTOR_SECRET), the SUT config, and\n" +
	"  whether --scenarios points at your product or at the bundled demo, and the executor your runs\n" +
	"  land on (below the control plane's floor, missing fixes first runs tripped over, or newer than the\n" +
	"  control plane's recommendation so its update block would downgrade it) — with, for each, the\n" +
	"  literal command that fixes it. It writes nothing except a rotated session token, and never prints\n" +
	"  a credential. It reads GET /api/instances on the control plane (--control-plane, else ARGUS_CP_URL,\n" +
	"  else the one the session was issued by), renewing an expired session's access token in memory.\n" +
	"  Exit 0 when the verdict is \"ok\" or \"warn\", 4 when it is \"fail\" (a check that could not run\n" +
	"  counts as fail). The credential is looked for in ARGUS_CP_AUTHOR_TOKEN (formerly ARGUS_CP_TOKEN,\n" +
	"  still read as a fallback), else the session file `argus cloud-login` wrote — the same order every\n" +
	"  cloud-* command uses. --token-file names what a token file holds (odts_ PAT, runner or author\n" +
	"  access token, and when it expires) before you hand it to someone; --token-for says which side it\n" +
	"  is going to. The file is read here and sent nowhere."

// defaultScenariosDir is `run`'s built-in --scenarios default: the OrderService demo bundled with the
// kit. Named once, here, so `run` and `doctor` cannot disagree about which directory "the operator
// did not choose one" means.
const defaultScenariosDir = "scenarios"

func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		configPath = fs.String("config", "", "the argus-config.yaml you will pass to run (optional here; its absence is reported, not refused)")
		scenarios  = fs.String("scenarios", defaultScenariosDir, "the scenarios dir you will pass to run")
		cp         = fs.String("control-plane", envOr("ARGUS_CP_URL", ""), "the control-plane URL (matched against the session, and used in fix commands)")
		instance   = fs.String("instance", envOr("ARGUS_INSTANCE_ID", ""), "the executor your runs use (default: report every executor in the workspace)")
		tokenFile  = fs.String("token-file", "", "a token file you are about to hand off (a Vault wrap, a hub connection): says what it holds; read here, sent nowhere")
		tokenFor   = fs.String("token-for", "", "with --token-file: the side it is going to, author or runner")
		tester     = fs.Bool("tester", false, "report the tester onboarding phases (token, tools/list, workspace, cluster, namespace, executor) as PASS/WARN/FAIL/SKIP lines with evidence, instead of the first-run checks")
		app        = fs.String("app", "", "with --tester: the app (default: the only ARGUS_TESTER_TOKEN_<APP> in the env file)")
		envFile    = fs.String("env-file", "", "with --tester: the env file holding the token (default ~/.config/argus/tester.env)")
		kubeconfig = fs.String("kubeconfig", "", "with --tester: the kubeconfig kubectl uses for the cluster checks (an in-cluster tester's is not the default)")
		kubeCtx    = fs.String("kube-context", "", "with --tester: the kubectl context for the cluster checks")
		skipClust  = fs.Bool("skip-cluster", false, "with --tester: skip the cluster and namespace checks (a tester with no Kubernetes)")
	)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, doctorUsage)
		if errors.Is(err, flag.ErrHelp) {
			return exitOK // asking for help is not a mistake
		}
		return exitUsage
	}
	switch {
	case *tokenFor != "" && *tokenFile == "":
		return emitErr(exitUsage, "doctor: --token-for says which side a --token-file is going to; name the file with --token-file")
	case *tokenFor != "" && *tokenFor != doctor.SideAuthor && *tokenFor != doctor.SideRunner:
		return emitErr(exitUsage, "doctor: --token-for %q: want %s or %s", *tokenFor, doctor.SideAuthor, doctor.SideRunner)
	}

	if *tester {
		ef := *envFile
		if ef == "" {
			ef = defaultTesterEnvFile()
		}
		return runTesterDoctor(testerDoctorOpts{app: *app, envFile: ef, cp: *cp, instance: *instance,
			kubeconfig: *kubeconfig, kubeContext: *kubeCtx, skipCluster: *skipClust})
	}
	if *app != "" || *envFile != "" || *kubeconfig != "" || *kubeCtx != "" || *skipClust {
		return emitErr(exitUsage, "doctor: --app, --env-file, --kubeconfig, --kube-context and --skip-cluster belong to --tester")
	}

	now := time.Now()
	cfg := gatherConfig(*configPath, *scenarios, *scenarios != defaultScenariosDir)
	cred := lookupCPCredential(*cp)
	credIn := gatherCredential(cred, *cp, now)
	// Asked first, reported last: the control plane's answer to the credential belongs in the credential check.
	exIn, refusal := gatherExecutors(cred, credIn.Facts, *cp, *instance, now)
	credIn.Refused = refusal
	credIn.Accepted = exIn.Accepted
	// CredentialNote is set only by the in-memory renewal: the read then carried the renewed token, not this one.
	credIn.Renewed = exIn.Accepted && exIn.CredentialNote != ""
	// What --config declares, for sut-reachable: with no targets there is nothing for the executor to probe.
	exIn.ConfigTargets = cfg.Targets
	checks := []doctor.Check{
		doctor.CheckCredential(credIn),
		doctor.CheckLocalHats(doctor.HatsFromValues(os.Getenv("ARGUS_TOKEN"), os.Getenv("ARGUS_RUNNER_TOKEN"), envname.Lookup(envname.ExecutorSecret, envname.ExecutorSecretDeprecated))),
		doctor.CheckConfig(cfg),
		doctor.CheckScenarios(gatherScenarios(*scenarios, *scenarios == defaultScenariosDir, cfg.ProjectName)),
		doctor.CheckExecutorVersion(exIn),
		doctor.CheckSUTReachable(exIn),
	}
	if *tokenFile != "" {
		cpForFix := *cp
		if cpForFix == "" {
			cpForFix = cred.controlPlane
		}
		checks = append(checks, doctor.CheckTokenFile(gatherTokenFile(*tokenFile, *tokenFor, cpForFix, now)))
	}
	rep := doctor.Summarize(checks)
	// The same one-line-per-check form `--tester` prints: on stderr for the reader, in the JSON for the agent.
	for _, c := range checks {
		rep.Lines = append(rep.Lines, c.Line())
	}
	for _, l := range rep.Lines {
		fmt.Fprintln(os.Stderr, l)
	}
	emit(rep)

	if rep.Verdict == doctor.VerdictFail {
		// exitFailed, not exitErr: the command worked. It is the MACHINE that is not ready.
		return exitFailed
	}
	return exitOK
}

// gatherCredential reduces the credential, resolved the way every cloud-* command does
// (lookupCPCredential, shared with preflight), to facts. The value goes no further than doctor.Classify.
func gatherCredential(cred cpCredential, cp string, now time.Time) doctor.CredentialInput {
	in := doctor.CredentialInput{
		Source:          cred.source,
		Present:         cred.present,
		FromSession:     cred.fromSession,
		RefreshPresent:  cred.refreshPresent,
		ObtainedAt:      cred.obtainedAt,
		ControlPlaneURL: cpForReport(cp),
		Now:             now,
	}
	if cred.present {
		in.Facts = doctor.Classify(cred.value)
	}
	return in
}

// gatherTokenFile reads a token file and reduces it to facts. The value goes no further than
// doctor.Classify: it is not printed, and it is not presented to the control plane or anyone else.
func gatherTokenFile(path, side, cp string, now time.Time) doctor.TokenFileInput {
	in := doctor.TokenFileInput{Path: path, For: side, ControlPlaneURL: cpForReport(cp), Now: now}
	blob, err := os.ReadFile(path)
	if err != nil {
		in.Err = err.Error()
		return in
	}
	vals := strings.Fields(string(blob))
	in.Values = len(vals)
	if len(vals) == 1 {
		in.Facts = doctor.Classify(vals[0])
	}
	return in
}

// doctorListInstances is doctor's one read: GET /api/instances with the token as given. A var so tests
// can answer it without a network.
var doctorListInstances = func(ctx context.Context, cpURL, token string) (string, []json.RawMessage, error) {
	return onboard.NewCloudClient(cpURL).ListInstancesRaw(ctx, token)
}

// doctorRefresh is one refresh_token grant, used to renew an expired access token in memory. A var so
// tests can answer it without a network.
var doctorRefresh = func(ctx context.Context, cpURL, refresh string) (*onboard.RefreshedTokens, error) {
	return onboard.NewCloudClient(cpURL).Refresh(ctx, refresh)
}

// gatherExecutors asks the control plane which executors this workspace has and what it thinks of
// their versions — or says why it did not ask. An expired session access token is renewed in memory
// (see the READ-ONLY note at the top of this file); an expired env-var token cannot be, and is not
// presented. When the control plane refuses the credential itself, that is returned too, for
// control-plane-credential to report.
func gatherExecutors(cred cpCredential, facts doctor.TokenFacts, cp, instance string, now time.Time) (doctor.ExecutorInput, doctor.CredentialRefusal) {
	if cp == "" {
		cp = cred.controlPlane
	}
	// The report gets the URL without its credentials (cpForReport); cp itself stays raw for the requests below.
	in := doctor.ExecutorInput{ControlPlaneURL: cpForReport(cp), InstanceID: instance}
	switch {
	case cp == "":
		in.NotAsked = "no control-plane URL — --control-plane and ARGUS_CP_URL are unset, and no session records one"
		return in, doctor.CredentialRefusal{}
	case !cred.present:
		in.NotAsked = "no control-plane credential to ask with (see control-plane-credential)"
		return in, doctor.CredentialRefusal{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	token := cred.value
	// A minute of margin: a token that expires mid-request answers 401 and reads as a refusal.
	if facts.Kind == doctor.KindJWT && facts.HasExpiry && !facts.ExpiresAt.After(now.Add(time.Minute)) {
		renewed, note, notAsked, err := renewInMemory(ctx, cred, cp, now)
		switch {
		case notAsked != "":
			in.NotAsked = notAsked
			return in, doctor.CredentialRefusal{}
		case err != nil:
			in.Err, in.ErrFix = err.Error(), loginFixFor(cp)
			// The oauth token endpoint answers a dead refresh token 400 invalid_grant (oauth/endpoints.go).
			if s := onboard.StatusOf(err); s == http.StatusBadRequest || s == http.StatusUnauthorized {
				return in, doctor.CredentialRefusal{Status: s, Renewal: true, Msg: onboardMsg(err)}
			}
			return in, doctor.CredentialRefusal{}
		}
		token, in.CredentialNote = renewed, note
	}
	ws, rows, err := doctorListInstances(ctx, cp, token)
	if err != nil {
		in.Err = err.Error()
		// 401 = the credential is not accepted; 403 = accepted, but not for the author API (control/web.go authenticate).
		if s := onboard.StatusOf(err); s == http.StatusUnauthorized || s == http.StatusForbidden {
			in.ErrFix = credentialFirst
			return in, doctor.CredentialRefusal{Status: s, Msg: onboardMsg(err)}
		}
		return in, doctor.CredentialRefusal{}
	}
	in.Workspace, in.Accepted = ws, true
	for _, raw := range rows {
		var e doctor.ExecutorStatus
		if err := json.Unmarshal(raw, &e); err != nil {
			in.Err = "an /api/instances row did not decode: " + err.Error()
			return in, doctor.CredentialRefusal{}
		}
		in.Executors = append(in.Executors, e)
	}
	for _, e := range in.Executors {
		if doctor.BlockNeedsDocker(e.UpdateCommand) {
			in.HostDocker = probeDocker()
			break
		}
	}
	return in, doctor.CredentialRefusal{}
}

// credentialFirst is executor-version's fix when the control plane refused the credential: re-running
// changes nothing until the credential does.
const credentialFirst = "fix control-plane-credential first (its fix is in this report), then re-run argus doctor"

// onboardMsg is the control plane's answer as the client reported it: the innermost HTTP status error's
// message, without doctor's own wrapping.
func onboardMsg(err error) string {
	var se *onboard.HTTPStatusError
	if errors.As(err, &se) {
		return se.Msg
	}
	return err.Error()
}

// probeDocker asks whether a Docker daemon answers on this host, because the control plane's update
// block runs `docker pull` and `docker run` here. `docker version` reads and changes nothing. Asked only
// when a block needs it, so a machine that will never paste one is never touched.
func probeDocker() doctor.DockerProbe {
	if _, err := exec.LookPath("docker"); err != nil {
		return doctor.DockerProbe{Probed: true, Reason: "no docker command on PATH"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return doctor.DockerProbe{Probed: true, Reason: "docker version did not answer within 10s"}
		}
		reason, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		if reason == "" {
			reason = "docker version: " + err.Error()
		}
		return doctor.DockerProbe{Probed: true, Reason: reason}
	}
	return doctor.DockerProbe{Probed: true, Answers: true}
}

// renewInMemory swaps an expired session access token for a fresh one without writing the session
// file. It returns the token and a note for the report, or why it did not try (notAsked), or why it
// failed (err, whose HTTP status onboard.StatusOf can read). The one write it can make is the
// rotated-refresh-token save described at the top of this file.
func renewInMemory(ctx context.Context, cred cpCredential, cp string, now time.Time) (token, note, notAsked string, err error) {
	if !cred.fromSession || !cred.refreshPresent {
		return "", "", "the credential has expired and carries nothing to renew it with (see control-plane-credential)", nil
	}
	d, err := onboard.LoadSessionFile(cred.source)
	if err != nil || d.RefreshToken == "" {
		return "", "", "the session's access token has expired, and its refresh token could not be read back from " + cred.source, nil
	}
	rt, err := doctorRefresh(ctx, cp, d.RefreshToken)
	if err != nil {
		return "", "", "", fmt.Errorf("renewing the session's expired access token failed: %w", err)
	}
	if rt.RefreshToken != "" && rt.RefreshToken != d.RefreshToken {
		d.AccessToken, d.RefreshToken, d.ObtainedAt = rt.AccessToken, rt.RefreshToken, now.UTC()
		if err := onboard.SaveSession(cred.source, *d); err != nil {
			return "", "", "", fmt.Errorf("the control plane rotated the refresh token and saving it failed: %w", err)
		}
		return rt.AccessToken, "access token renewed; the control plane ROTATED the refresh token, so it was saved to " + cred.source + " as every cloud-* command would", "", nil
	}
	return rt.AccessToken, "the session's expired access token was renewed in memory; the session file was not written", "", nil
}

func loginFixFor(cp string) string {
	return "argus cloud-login --control-plane " + orPlaceholder(cpForReport(cp)) + " --scope author"
}

// cpForReport is the control-plane URL as doctor's report may repeat it, in a check's subject and in its fix
// commands: without userinfo, query or fragment, shell-quoted when needed, and the placeholder when the value
// does not parse. Empty stays empty: internal/doctor reads that as "no URL known" and says
// so. The raw value is for the requests only and never enters a doctor input.
func cpForReport(cp string) string {
	if cp == "" {
		return ""
	}
	return onboard.ControlPlaneForCommand(cp)
}

func orPlaceholder(s string) string {
	if s == "" {
		return "<url>"
	}
	return s
}

// gatherConfig parses --config WITHOUT resolving ${VAR}s: an unset variable is a finding, not a
// reason to stop diagnosing.
// gatherConfig parses --config without resolving ${VAR}s, then runs validate-config's local checks
// against --scenarios — only when --scenarios was given: against the bundled demo their refusals would
// blame the config for the scenarios mistake the scenarios check already names. It does not probe the
// SUT (validate-config's ProbeTargets): doctor contacts nothing but the control plane. Whether the SUT
// answers is sut-reachable's, read from the executor's own probe on that same control-plane call.
func gatherConfig(path, scenarios string, validate bool) doctor.ConfigInput {
	if path == "" {
		return doctor.ConfigInput{}
	}
	c, err := config.ParseUnresolved(path)
	if err != nil {
		return doctor.ConfigInput{Path: path, Err: err.Error()}
	}
	in := doctor.ConfigInput{Path: path, ProjectName: c.Project.Name}
	seen := map[string]bool{}
	for _, r := range c.EnvRefs() {
		// The same test config.Load applies: unset OR empty is missing. Only the name is kept.
		if v, ok := os.LookupEnv(r.Name); ok && v != "" {
			continue
		}
		if ref := r.Name + " (" + r.Field + ")"; !seen[ref] {
			seen[ref] = true
			in.Unset = append(in.Unset, ref)
		}
	}
	n := toolcore.TargetCount(c)
	in.Targets = &n
	in.Portless = portlessHTTP(c)
	if validate && c.Project.Name != "" {
		errs, n, verr := c.Validate(scenarios)
		if verr != nil {
			in.Invalid = []string{"the checks could not run: " + verr.Error()}
		}
		for _, e := range errs {
			line := e.Message
			if e.Scenario != "" {
				line = e.Scenario + ": " + e.Message
			}
			in.Invalid = append(in.Invalid, line)
		}
		in.Validated, in.ScenariosChecked, in.ScenariosDir = true, n, scenarios
		// the warnings validate-config prints, from the SAME function, so doctor can
		// never call a config clean that validate-config warns about.
		in.Warnings = toolcore.ConfigWarnings(c, scenarios, argusrun.RequestsEstimated(scenarios))
	}
	return in
}

// portlessHTTP lists the http targets whose base_url names no port: config.HTTPTarget.HostPort reaches
// those on :8080, not on http's :80 (B:35). One whose host is still a ${VAR} does not parse (a brace is
// not a host character), so it is not judged; a ${VAR} later in the URL leaves the host literal, and is.
func portlessHTTP(c *config.Config) []string {
	var out []string
	check := func(t *config.HTTPTarget, label string) {
		if t == nil || !strings.HasPrefix(t.BaseURL, "http://") {
			return
		}
		if u, err := url.Parse(t.BaseURL); err == nil && u.Port() == "" {
			out = append(out, label)
		}
	}
	check(c.Targets.HTTP, "targets.http.base_url")
	names := make([]string, 0, len(c.Targets.HTTPTargets))
	for n := range c.Targets.HTTPTargets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		check(c.Targets.HTTPTargets[n], "targets.http_targets."+n+".base_url")
	}
	return out
}

func gatherScenarios(dir string, isDefault bool, project string) doctor.ScenariosInput {
	in := doctor.ScenariosInput{Dir: dir, IsDefault: isDefault, ProjectName: project}
	st, err := os.Stat(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			// It may well be there. "Could not look" is unknown, not absent.
			in.Exists = true
			in.WalkErr = err.Error()
		}
		return in
	}
	in.Exists = true
	if !st.IsDir() {
		in.WalkErr = "not a directory"
		return in
	}
	n, err := doctor.CountScenarioFiles(dir)
	if err != nil {
		in.WalkErr = err.Error()
		return in
	}
	in.Files = n
	// The import preview: the files `cloud-seed-scenarios` would upload (its own reader), each through the
	// rules the control plane's author_write_scenario refuses by (toolcore.ValidateAll). Local; no network.
	in.Version = buildinfo.Resolve(os.Getenv("ARGUS_VERSION"))
	files, err := onboard.LoadScenarioDir(dir)
	if err != nil {
		in.ImportErr = err.Error()
		return in
	}
	in.Imported, in.ImportFiles = true, len(files)
	for _, f := range files {
		s, errs := toolcore.ValidateAll(f.Content)
		if len(errs) == 0 {
			continue
		}
		r := doctor.ScenarioRefusal{Path: f.Path, Errors: len(errs), First: oneLine(errs[0].String())}
		if s != nil {
			r.ID = s.ID
		}
		in.Refused = append(in.Refused, r)
	}
	return in
}
