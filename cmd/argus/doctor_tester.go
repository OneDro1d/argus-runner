package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/buildinfo"
	"github.com/OneDro1d/argus-runner/internal/doctor"
	"github.com/OneDro1d/argus-runner/internal/onboard"
	"github.com/OneDro1d/argus-runner/internal/sessioninit"
	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// doctor_tester.go — `argus doctor --tester` (msgbus onboarding review, item 18): one PASS/WARN/FAIL/SKIP line
// per onboarding phase, each with its evidence, so "which phase is stuck" is answered by the command and
// not by a question to the operator.
//
//	P3 token     tester-token, tester-tools, tester-workspace
//	P4 cluster   tester-cluster
//	P5 plane     tester-namespace, tester-pullsecrets, tester-executor, tester-executor-version
//
// ⛔ The token is read from the tester env file (the same file the headersHelper reads at call time — a
// Coder session does not source ~/.bashrc, so the environment would say "unset" while the session works).
// Its VALUE goes only into the Authorization header of two control-plane requests. It is never printed:
// every line names the variable and says set / not set.
//
// SKIP means "depends on something an earlier line already failed": no token skips the three checks that
// need one. A check that could not run for any other reason is a FAIL — not knowing is not a pass.

// executorFreshFor is how recent the executor's last poll must be for "polling": the runbook's P5 evidence.
const executorFreshFor = 60 * time.Second

type testerDoctorOpts struct {
	app, envFile, cp, instance, kubeconfig, kubeContext string
	skipCluster                                         bool
}

func defaultTesterEnvFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "argus", "tester.env")
}

func tk(id, phase, what, subject string, st doctor.Status, detail, fix string) doctor.Check {
	return doctor.Check{ID: id, Phase: phase, What: what, Subject: subject, Status: st, Detail: detail, Fix: fix}
}

// testerChecks runs every phase and returns the checks in phase order.
func testerChecks(o testerDoctorOpts) []doctor.Check {
	var checks []doctor.Check
	add := func(c doctor.Check) { checks = append(checks, c) }
	const tokenWhat = "is the tester's control-plane token in the env file the session reads?"

	if o.envFile == "" {
		add(tk("tester-token", "P3", tokenWhat, "no home directory to find ~/.config/argus/tester.env", doctor.StatusFail, "pass --env-file", "argus doctor --tester --env-file <path>"))
		return append(checks, skippedRest(o)...)
	}
	// which app: --app, else the only ARGUS_TESTER_TOKEN_* variable the file defines
	app := o.app
	names, nerr := sessioninit.TokenVarNames(o.envFile, "ARGUS_TESTER_TOKEN_")
	if nerr != nil && !errors.Is(nerr, os.ErrNotExist) {
		add(tk("tester-token", "P3", tokenWhat, o.envFile, doctor.StatusFail, "could not read it: "+nerr.Error(), ""))
		return append(checks, skippedRest(o)...)
	}
	var varName string
	switch {
	case app != "":
		up, _, err := sessioninit.AppNames(app)
		if err != nil {
			add(tk("tester-token", "P3", tokenWhat, "--app "+app, doctor.StatusFail, err.Error(), ""))
			return append(checks, skippedRest(o)...)
		}
		varName = "ARGUS_TESTER_TOKEN_" + up
	case len(names) == 1:
		varName = names[0]
	case len(names) == 0:
		add(tk("tester-token", "P3", tokenWhat, o.envFile, doctor.StatusFail,
			"the file defines no ARGUS_TESTER_TOKEN_<APP> variable (or does not exist)", "argus tester init <app>"))
		return append(checks, skippedRest(o)...)
	default:
		add(tk("tester-token", "P3", tokenWhat, o.envFile, doctor.StatusFail,
			"the file defines "+fmt.Sprint(len(names))+" tester tokens ("+strings.Join(names, ", ")+"): name the app with --app", "argus doctor --tester --app <app>"))
		return append(checks, skippedRest(o)...)
	}
	set, _ := sessioninit.TokenSet(o.envFile, varName)
	subject := varName + " in " + o.envFile
	token := ""
	switch {
	case !set:
		add(tk("tester-token", "P3", tokenWhat, subject, doctor.StatusFail, "not set (empty or absent)",
			"paste your token into "+o.envFile+" after "+varName+"="))
	case fileModeWide(o.envFile) != "":
		add(tk("tester-token", "P3", tokenWhat, subject, doctor.StatusFail,
			"is set, but the file mode is "+fileModeWide(o.envFile)+" — other users on this machine can read it", "chmod 600 "+o.envFile))
	default:
		add(tk("tester-token", "P3", tokenWhat, subject, doctor.StatusOK, "is set (yes; the value is never shown)", ""))
		token, _ = sessioninit.ReadToken(o.envFile, varName)
	}

	cp := o.cp
	if cp == "" {
		cp, _ = sessioninit.ReadToken(o.envFile, "ARGUS_CP_URL")
	}
	if cp == "" {
		cp = buildinfo.DefaultControlPlane()
	}
	client := onboard.NewCloudClient(cp)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// P3 tools/list
	const toolsWhat = "does tools/list with this token return the author tools?"
	toolsSubject := "POST " + displayControlPlane(cp) + "/mcp tools/list"
	if token == "" {
		add(tk("tester-tools", "P3", toolsWhat, toolsSubject, doctor.StatusSkip, "no usable token (see tester-token)", ""))
	} else if tools, err := client.ToolsList(ctx, token); err != nil {
		add(tk("tester-tools", "P3", toolsWhat, toolsSubject, doctor.StatusFail, refusalText(err),
			"generate an author token bound to the app's workspace in the Argus app (Tokens), then paste it after "+varName+"="))
	} else if n := sessioninit.CountAuthorTools(tools); n == 0 {
		add(tk("tester-tools", "P3", toolsWhat, toolsSubject, doctor.StatusFail,
			fmt.Sprintf("tools/list returned %d tools, 0 author tools: this is not an author token", len(tools)), "generate an AUTHOR token, not a builder (runner) one"))
	} else {
		add(tk("tester-tools", "P3", toolsWhat, toolsSubject, doctor.StatusOK, fmt.Sprintf("tools/list returned %d tools, %d author tools", len(tools), n), ""))
	}

	// P3 workspace, and the executor rows P5 reads
	const wsWhat = "which workspace does this token belong to?"
	wsSubject := "GET " + displayControlPlane(cp) + "/api/instances"
	var rows []testerInstance
	var verRows []doctor.ExecutorStatus // the same rows, read for the version check: no second request
	wsName := ""
	haveRows := false
	if token == "" {
		add(tk("tester-workspace", "P3", wsWhat, wsSubject, doctor.StatusSkip, "no usable token (see tester-token)", ""))
	} else if ws, raws, err := client.ListInstancesRaw(ctx, token); err != nil {
		add(tk("tester-workspace", "P3", wsWhat, wsSubject, doctor.StatusFail, refusalText(err), ""))
	} else if ws == "" {
		add(tk("tester-workspace", "P3", wsWhat, wsSubject, doctor.StatusFail, "the control plane named no workspace for this token", "use a workspace-bound author token"))
	} else {
		for _, raw := range raws {
			var r testerInstance
			if json.Unmarshal(raw, &r) == nil {
				rows = append(rows, r)
			}
			var v doctor.ExecutorStatus
			if json.Unmarshal(raw, &v) == nil {
				verRows = append(verRows, v)
			}
		}
		haveRows, wsName = true, ws
		add(tk("tester-workspace", "P3", wsWhat, wsSubject, doctor.StatusOK, fmt.Sprintf("workspace %q, %d executor instance(s) registered", ws, len(rows)), ""))
	}

	instance := o.instance
	if instance == "" && len(rows) == 1 {
		instance = rows[0].InstanceID
	}

	// P4 cluster, P5 namespace
	clusterOK, nsOK := false, false
	const clWhat = "can kubectl reach the cluster the execution plane runs in?"
	const nsWhat = "does the instance's namespace exist?"
	switch {
	case o.skipCluster:
		add(tk("tester-cluster", "P4", clWhat, "kubectl", doctor.StatusSkip, "--skip-cluster", ""))
		add(tk("tester-namespace", "P5", nsWhat, "kubectl", doctor.StatusSkip, "--skip-cluster", ""))
	default:
		out, err := testerKubectl(o, "get", "--raw", "/version")
		clSubject := "kubectl" + kubectlWhere(o) + " get --raw /version"
		if err != nil {
			add(tk("tester-cluster", "P4", clWhat, clSubject, doctor.StatusFail, oneLine(err.Error()),
				"pass the in-cluster kubeconfig: argus doctor --tester --kubeconfig ~/.config/argus/<app>.kubeconfig"))
		} else {
			var v struct {
				GitVersion string `json:"gitVersion"`
			}
			_ = json.Unmarshal([]byte(out), &v)
			clusterOK = true
			add(tk("tester-cluster", "P4", clWhat, clSubject, doctor.StatusOK, "the API server answered"+orSuffix(v.GitVersion, ", version "), ""))
		}
		switch {
		case instance == "" && haveRows && len(rows) == 0:
			add(tk("tester-namespace", "P5", nsWhat, "argus-inst-<instance>", doctor.StatusSkip, "no executor instance is registered, so there is no namespace to name (see tester-executor)", ""))
		case instance == "" && !haveRows:
			add(tk("tester-namespace", "P5", nsWhat, "argus-inst-<instance>", doctor.StatusSkip, "the instance list was not read (see tester-workspace)", ""))
		case instance == "":
			add(tk("tester-namespace", "P5", nsWhat, "argus-inst-<instance>", doctor.StatusSkip, fmt.Sprintf("%d instances registered: pass --instance to pick one", len(rows)), ""))
		case !clusterOK:
			add(tk("tester-namespace", "P5", nsWhat, updatecmd.InstanceNamespace(instance), doctor.StatusSkip, "the cluster was not reachable (see tester-cluster)", ""))
		default:
			ns := updatecmd.InstanceNamespace(instance)
			if _, err := testerKubectl(o, "get", "namespace", ns, "-o", "name"); err != nil {
				add(tk("tester-namespace", "P5", nsWhat, "namespace "+ns, doctor.StatusFail, oneLine(err.Error()),
					"the execution plane is not installed yet: render-k8s, then kubectl create ("+buildinfo.DocsRef("argus-tester-guide")+")"))
			} else {
				nsOK = true
				add(tk("tester-namespace", "P5", nsWhat, "namespace "+ns, doctor.StatusOK, "exists", ""))
			}
		}
	}

	// P5 pull secrets: the executor Deployment's imagePullSecrets must each exist
	add(pullSecretsCheck(o, instance, nsOK))

	// P5 executor
	const exWhat = "is the executor registered and polling?"
	switch {
	case !haveRows:
		add(tk("tester-executor", "P5", exWhat, "GET "+displayControlPlane(cp)+"/api/instances", doctor.StatusSkip, "the instance list was not read (see tester-workspace)", ""))
	case instance == "" && len(rows) == 0:
		// Nothing registered is not "ambiguous": the execution plane was never enrolled. A SKIP here let
		// the verdict read ok with no executor at all (review finding 1).
		add(tk("tester-executor", "P5", exWhat, "GET "+displayControlPlane(cp)+"/api/instances", doctor.StatusFail,
			"no executor instance is registered in this workspace", "enroll one: argus cloud-enroll, then render-k8s and kubectl create"))
	case instance == "":
		add(tk("tester-executor", "P5", exWhat, "instance list", doctor.StatusSkip, fmt.Sprintf("%d instances registered; pass --instance to pick one", len(rows)), ""))
	default:
		add(executorCheck(instance, rows, time.Now()))
	}

	// P5 executor version: the judgment executor-version makes, on the rows tester-workspace already read
	add(executorVersionCheck(haveRows, cp, wsName, instance, verRows))
	return checks
}

func skippedRest(o testerDoctorOpts) []doctor.Check {
	var out []doctor.Check
	for _, s := range []struct{ id, phase, what string }{
		{"tester-tools", "P3", "does tools/list with this token return the author tools?"},
		{"tester-workspace", "P3", "which workspace does this token belong to?"},
		{"tester-cluster", "P4", "can kubectl reach the cluster the execution plane runs in?"},
		{"tester-namespace", "P5", "does the instance's namespace exist?"},
		{"tester-pullsecrets", "P5", pullSecretsWhat},
		{"tester-executor", "P5", "is the executor registered and polling?"},
		{"tester-executor-version", "P5", executorVersionWhat},
	} {
		out = append(out, tk(s.id, s.phase, s.what, "-", doctor.StatusSkip, "no token to work with (see tester-token)", ""))
	}
	return out
}

// testerInstance is the part of a GET /api/instances row the executor check reads.
type testerInstance struct {
	InstanceID string     `json:"instance_id"`
	LastSeen   *time.Time `json:"last_seen"`
}

// executorCheck is registered AND polling, from the same fields cloud-executor-status reads: a row exists
// (registered) and last_seen, which only an accepted poll sets, is recent.
func executorCheck(instance string, rows []testerInstance, now time.Time) doctor.Check {
	const what = "is the executor registered and polling?"
	subject := "instance " + instance
	for _, r := range rows {
		if r.InstanceID != instance {
			continue
		}
		if r.LastSeen == nil || r.LastSeen.IsZero() {
			return tk("tester-executor", "P5", what, subject, doctor.StatusFail, "registered: true, poll_accepted: false — it has never polled",
				"kubectl -n "+updatecmd.InstanceNamespace(instance)+" describe pod (image pull? see the ghcr-pull secret)")
		}
		age := now.Sub(*r.LastSeen).Round(time.Second)
		if age > executorFreshFor {
			return tk("tester-executor", "P5", what, subject, doctor.StatusFail,
				fmt.Sprintf("registered: true, but the last poll was %s ago (want under %s)", age, executorFreshFor), "check the executor pod is running")
		}
		return tk("tester-executor", "P5", what, subject, doctor.StatusOK, fmt.Sprintf("registered: true, polling (last poll %s ago)", age), "")
	}
	return tk("tester-executor", "P5", what, subject, doctor.StatusFail, "registered: false — no such instance in this workspace",
		"enroll it: argus cloud-enroll, then render-k8s and kubectl create")
}

const executorVersionWhat = "is the executor at or above the control plane's recommended version?"

// executorVersionCheck reuses doctor.CheckExecutorVersion's judgment (floors, known fixes, the downgrade trap)
// on the /api/instances rows tester-workspace has already read. It makes no request of its own. Whether the
// executor exists and polls is tester-executor's question: with no row to judge this SKIPs and says so.
func executorVersionCheck(haveRows bool, cp, ws, instance string, rows []doctor.ExecutorStatus) doctor.Check {
	const id, phase = "tester-executor-version", "P5"
	subject := "GET " + displayControlPlane(cp) + "/api/instances"
	if !haveRows {
		return tk(id, phase, executorVersionWhat, subject, doctor.StatusSkip, "the instance list was not read (see tester-token and tester-workspace)", "")
	}
	var judged []doctor.ExecutorStatus
	for _, r := range rows {
		if instance == "" || r.InstanceID == instance {
			judged = append(judged, r)
		}
	}
	if len(judged) == 0 {
		return tk(id, phase, executorVersionWhat, subject, doctor.StatusSkip, "no executor row to judge (see tester-executor)", "")
	}
	c := doctor.CheckExecutorVersion(doctor.ExecutorInput{ControlPlaneURL: displayControlPlane(cp), Workspace: ws, Executors: judged, Accepted: true})
	c.ID, c.Phase, c.What = id, phase, executorVersionWhat
	return c
}

const pullSecretsWhat = "does every image pull Secret the executor Deployment names exist?"

// pullSecretsCheck asks the cluster what the executor's update guard cannot: the guard reads only the
// executor's own Deployment and holds no Secret access, so a Deployment that names a pull Secret nobody
// created passes the guard, and the same-registry update it allows then cannot pull. The Deployment is
// maxSurge:0, so that strands the executor.
//
// ⛔ Existence only. A Secret is asked for with `get secret <name> -o name`, which prints the name and
// nothing else: its .data is never fetched, so it cannot be printed. Whatever cannot be determined (no
// cluster access, RBAC refusing the Deployment or Secret reads, --skip-cluster) is SKIP with the reason —
// neither PASS nor FAIL. A Secret the cluster says is NotFound is FAIL even when another lookup was refused.
func pullSecretsCheck(o testerDoctorOpts, instance string, nsOK bool) doctor.Check {
	const id, phase = "tester-pullsecrets", "P5"
	skip := func(subject, why string) doctor.Check {
		return tk(id, phase, pullSecretsWhat, subject, doctor.StatusSkip, why, "")
	}
	switch {
	case o.skipCluster:
		return skip("kubectl", "--skip-cluster")
	case instance == "":
		return skip("deployment executor", "no instance was picked, so there is no namespace to read (see tester-namespace)")
	case !nsOK:
		return skip("deployment executor in "+updatecmd.InstanceNamespace(instance), "the namespace could not be confirmed (see tester-cluster and tester-namespace)")
	}
	ns := updatecmd.InstanceNamespace(instance)
	subject := "deployment executor in " + ns
	out, err := testerKubectl(o, "get", "deployment", "executor", "-n", ns, "-o", "jsonpath={.spec.template.spec.imagePullSecrets[*].name}")
	if err != nil {
		return skip(subject, "could not read the executor Deployment, so its pull Secrets are unchecked: "+oneLine(err.Error()))
	}
	names := strings.Fields(out)
	if len(names) == 0 {
		return tk(id, phase, pullSecretsWhat, subject, doctor.StatusOK, "the executor Deployment names no imagePullSecrets, so there is no pull Secret to be missing", "")
	}
	var missing, unknown []string
	var why string
	for _, n := range names {
		if _, err := testerKubectl(o, "get", "secret", n, "-n", ns, "-o", "name"); err != nil {
			if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "not found") {
				missing = append(missing, n)
			} else {
				unknown = append(unknown, n)
				why = oneLine(err.Error())
			}
		}
	}
	switch {
	case len(missing) > 0:
		return tk(id, phase, pullSecretsWhat, subject, doctor.StatusFail,
			"the executor Deployment names imagePullSecrets that do not exist in "+ns+": "+strings.Join(missing, ", ")+
				" — the executor cannot pull its image through a Secret that is not there, and an update the guard allows "+
				"would strand it (the Deployment is maxSurge:0, so no old pod survives a pull failure)",
			"create each missing Secret in "+ns+", or remove it from the executor Deployment's imagePullSecrets")
	case len(unknown) > 0:
		return skip(subject, "could not tell whether "+strings.Join(unknown, ", ")+" exist(s) ("+why+"), so the pull Secrets are unchecked")
	}
	return tk(id, phase, pullSecretsWhat, subject, doctor.StatusOK, "all exist: "+strings.Join(names, ", "), "")
}

func testerKubectl(o testerDoctorOpts, args ...string) (string, error) {
	pre := (&secretsCommonFlags{kubeconfig: o.kubeconfig, kubeContext: o.kubeContext}).kubectlPrefix()
	all := append(append(pre, "--request-timeout=10s"), args...)
	return runKubectl(nil, all...)
}

func kubectlWhere(o testerDoctorOpts) string {
	s := ""
	if o.kubeconfig != "" {
		s += " --kubeconfig " + o.kubeconfig
	}
	if o.kubeContext != "" {
		s += " --context " + o.kubeContext
	}
	return s
}

// fileModeWide returns the file's mode as octal text when group or other can read it, else "".
func fileModeWide(path string) string {
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm()&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf("%o", st.Mode().Perm())
}

// refusalText turns a control-plane error into evidence without echoing anything credential-shaped.
func refusalText(err error) string {
	var sc *onboard.SignInChallengeError
	if errors.As(err, &sc) {
		return fmt.Sprintf("the control plane answered HTTP %d with a sign-in challenge: the token is missing, expired, revoked or not for this control plane", sc.Status)
	}
	if s := onboard.StatusOf(err); s != 0 {
		return fmt.Sprintf("the control plane answered HTTP %d: %s", s, onboardMsg(err))
	}
	return oneLine(err.Error())
}

func orSuffix(v, prefix string) string {
	if v == "" {
		return ""
	}
	return prefix + v
}

// runTesterDoctor prints the report (JSON on stdout, the PASS/WARN/FAIL/SKIP lines on stderr) and returns the exit code.
func runTesterDoctor(o testerDoctorOpts) int {
	checks := testerChecks(o)
	rep := doctor.Summarize(checks)
	for _, c := range checks {
		rep.Lines = append(rep.Lines, c.Line())
	}
	for _, l := range rep.Lines {
		fmt.Fprintln(os.Stderr, l)
	}
	emit(rep)
	if rep.Verdict == doctor.VerdictFail {
		return exitFailed
	}
	return exitOK
}
