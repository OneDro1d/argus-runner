package argus

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/testtargets"
)

// envCaptureTimeout bounds how long a load run's environment capture may take. A slow or wedged
// Kubernetes API must never hang the run itself — this is a best-effort read, never a gate.
const envCaptureTimeout = 20 * time.Second

// sutNamespaceEnvVar is where the executor learns the SUT's Kubernetes namespace — rendered by
// k8srender.RenderExecutor from --sut-namespace (internal/k8srender/k8srender.go's ARGUS_SUT_NAMESPACE
// env entry), the SAME value the executor's NetworkPolicy is already scoped to
// (k8srender.Instance.SUTNamespace). Never guessed or derived from the SUT's own argus-config.yaml,
// which describes ENDPOINTS, not a Kubernetes namespace.
const sutNamespaceEnvVar = "ARGUS_SUT_NAMESPACE"

// anyLoadDeclared reports whether ANY scenario in this run declared a `## LOAD` profile (AC-11).
// Environment capture (P3 #23) is scoped to load runs ONLY — a plain functional run pays no
// Kubernetes-API cost and gets no environment field at all (nil, omitempty on report.Report), the
// same convention report.ScenarioResult.Load already follows per scenario.
func anyLoadDeclared(scns []scenarioFile) bool {
	for _, sf := range scns {
		if sf.s.Load != nil || sf.s.AMQPLoad != nil || sf.s.HTTPLoad != nil { // / a ramp is a load run too
			return true
		}
	}
	return false
}

// captureEnvironmentIfNeeded reads the SUT namespace's pods/workloads/nodes for a load run — the
// operator's principle: "we can't say 'it breaks under load'; we must say 'this environment, with
// these resources, breaks under this load'". nil for a run with no declared LOAD profile: never
// attempted, never reported absent, because it was never asked for.
//
// Read access is granted ENTIRELY by the SUT: the executor presents its OWN ServiceAccount token
// (already mounted in-cluster for UC069 self-delete, internal/runner/autoscale.go) against the
// SUT's namespace, and Kubernetes RBAC alone decides whether that succeeds — Argus never grants
// itself a right in a namespace it does not own. See internal/k8srender.SUTAccessRoleManifest for
// the Role/RoleBinding the SUT owner applies in THEIR OWN namespace.
func captureEnvironmentIfNeeded(scns []scenarioFile) *envcapture.Capture {
	return captureEnvironmentForMode(scns, "")
}

// loadCheckRef is what the namespace choice needs of one LOAD check: its id and tags, the two inputs of
// testtargets.List.Map (the ONE check-to-target mapping).
type loadCheckRef struct {
	ID   string
	Tags []string
}

// loadCheckRefs lists the run's LOAD checks: the ones anyLoadDeclared looks for, plus a `## LOAD` section
// that was declared but did not parse (config/neverload.go's isLoadCheck counts it as a load check too).
func loadCheckRefs(scns []scenarioFile) []loadCheckRef {
	var out []loadCheckRef
	for _, sf := range scns {
		if sf.s == nil {
			continue
		}
		if sf.s.Load != nil || sf.s.AMQPLoad != nil || sf.s.LoadDeclared {
			out = append(out, loadCheckRef{ID: sf.s.ID, Tags: sf.s.Tags})
		}
	}
	return out
}

// allCheckRefs lists every check of the run, for a compare run that declares no `## LOAD`.
func allCheckRefs(scns []scenarioFile) []loadCheckRef {
	var out []loadCheckRef
	for _, sf := range scns {
		if sf.s != nil {
			out = append(out, loadCheckRef{ID: sf.s.ID, Tags: sf.s.Tags})
		}
	}
	return out
}

// chooseCaptureNamespace (ARGUS-TA-5) picks the Kubernetes namespace a load run's
// environment capture reads. PURE: no Kubernetes, no environment access (envNS is ARGUS_SUT_NAMESPACE,
// passed in).
//
//	exactly one distinct namespace  -> (that namespace, "")
//	none declared anywhere          -> (envNS, "")   today's behaviour, ARGUS_SUT_NAMESPACE
//	more than one                   -> ("", reason)  capture nothing; never guess, never fall back
//	every check's target outside    -> ("", reason) `outside_cluster: true`; envNS is NOT a fallback
//
// A check of an outside-cluster target is left out of the count above; the others decide as before.
//
// Each load check is mapped to its target; a target's non-empty namespace is that check's namespace. A
// check with NO namespace of its own (no test_targets, unassigned, or a target that declares none) counts
// as the env-var namespace for the distinct count (when the env var is set), so a run mixing a
// namespaced target with such checks is "several" unless the two names are equal.
func chooseCaptureNamespace(targets testtargets.List, loads []loadCheckRef, envNS string) (ns, reason string) {
	return chooseNamespaceFor(targets, loads, envNS, loadCheckWords)
}

// checkWords is how a refusal names the checks that decided the namespace: the load checks of a load run, or
// every check of a compare run that declares no `## LOAD` (the run's checks are what made the capture happen).
type checkWords struct {
	checks string // "load checks" | "checks"
	run    string // "this load run's checks" | "this run's checks"
}

var (
	loadCheckWords    = checkWords{checks: "load checks", run: "this load run's checks"}
	compareCheckWords = checkWords{checks: "checks", run: "this run's checks"}
)

// chooseNamespaceFor is chooseCaptureNamespace for any set of checks, said in the given words.
func chooseNamespaceFor(targets testtargets.List, loads []loadCheckRef, envNS string, w checkWords) (ns, reason string) {
	byNS := map[string][]string{} // namespace -> the target names (or the env var) that name it
	declared := false
	add := func(n, who string) {
		for _, w := range byNS[n] {
			if w == who {
				return
			}
		}
		byNS[n] = append(byNS[n], who)
	}
	outside, inCluster := 0, 0 // load checks of an outside-cluster target / of every other target
	for _, lc := range loads {
		if t, ok := targets.ByName(targets.Map(lc.ID, lc.Tags)); ok && t.OutsideCluster {
			outside++ // nothing of it is in a cluster: it names no namespace and never falls back to envNS
			continue
		}
		inCluster++
		if t, ok := targets.ByName(targets.Map(lc.ID, lc.Tags)); ok && t.Namespace != "" {
			declared = true
			add(t.Namespace, fmt.Sprintf("target %q", t.Name))
			continue
		}
		if envNS != "" {
			add(envNS, sutNamespaceEnvVar)
		}
	}
	if outside > 0 && inCluster == 0 {
		return "", "not applicable: the target of these " + w.checks + " is declared outside the cluster (outside_cluster: true on test target(s) " +
			strings.Join(outsideTargetNames(targets, loads), ", ") + "), so there is no Kubernetes namespace to read and nothing was asked of Kubernetes"
	}
	if !declared {
		return envNS, ""
	}
	if len(byNS) == 1 {
		for n := range byNS {
			return n, ""
		}
	}
	names := make([]string, 0, len(byNS))
	for n := range byNS {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		sort.Strings(byNS[n])
		parts = append(parts, fmt.Sprintf("%q (%s)", n, strings.Join(byNS[n], ", ")))
	}
	return "", w.run + " belong to more than one Kubernetes namespace: " + strings.Join(parts, "; ") +
		". No namespace was read, because choosing one would describe the wrong system for the others. Run the " + w.checks + " of one test target per run (--scenario or --tag) to capture its environment."
}

// outsideTargetNames lists, sorted and unique, the names of the targets the run's load checks belong to that are
// declared `outside_cluster: true`.
func outsideTargetNames(targets testtargets.List, loads []loadCheckRef) []string {
	seen := map[string]bool{}
	var names []string
	for _, lc := range loads {
		if t, ok := targets.ByName(targets.Map(lc.ID, lc.Tags)); ok && t.OutsideCluster && !seen[t.Name] {
			seen[t.Name] = true
			names = append(names, t.Name)
		}
	}
	sort.Strings(names)
	return names
}

// outsideClusterNotes is the Capture warning for a run whose load checks span an in-cluster namespace and
// targets declared outside the cluster: the namespace is read, and these targets were not.
func outsideClusterNotes(targets testtargets.List, loads []loadCheckRef) []string {
	return outsideClusterNotesFor(targets, loads, loadCheckWords)
}

// outsideClusterNotesFor is outsideClusterNotes in the words of the given set of checks.
func outsideClusterNotesFor(targets testtargets.List, loads []loadCheckRef, w checkWords) []string {
	names := outsideTargetNames(targets, loads)
	if len(names) == 0 {
		return nil
	}
	return []string{"not read: the " + w.checks + " of test target(s) " + strings.Join(names, ", ") + " are declared outside the cluster (outside_cluster: true), so nothing was asked of Kubernetes for them"}
}

// captureEnvironmentForMode is captureEnvironmentIfNeeded plus ARGUS-CMP-3's rule: a run in mode
// `compare` captures the environment fingerprint even when no scenario declares `## LOAD`, because the
// fingerprint (with the running image digests) is what names the version a comparison cell was measured
// at. Every other mode is exactly as before.
func captureEnvironmentForMode(scns []scenarioFile, mode string) *envcapture.Capture {
	return captureEnvironmentFor(scns, mode, nil)
}

// captureEnvironmentFor is captureEnvironmentForMode with the instance's test targets: the namespace read
// is chosen by chooseCaptureNamespace from the run's load checks (ARGUS-TA-5). The Capture's Namespace is
// the one actually read; its Reason says why nothing was.
func captureEnvironmentFor(scns []scenarioFile, mode string, targets testtargets.List) *envcapture.Capture {
	if !anyLoadDeclared(scns) && mode != report.ModeCompare {
		return nil
	}
	// The checks whose targets decide the namespace are the ones that made the capture happen: the load
	// checks of a load run, else (mode compare, no `## LOAD`) every check of the run.
	loads, words := loadCheckRefs(scns), loadCheckWords
	if !anyLoadDeclared(scns) {
		loads, words = allCheckRefs(scns), compareCheckWords
	}
	ns, why := chooseNamespaceFor(targets, loads, os.Getenv(sutNamespaceEnvVar), words)
	if why != "" {
		return &envcapture.Capture{Reason: why}
	}
	if ns == "" {
		return &envcapture.Capture{Reason: "no SUT namespace declared (" + sutNamespaceEnvVar + " unset and no test target of the " + words.checks + " declares a namespace — render-k8s with --sut-namespace)"}
	}
	captured := envCaptureRead(ns)
	// a namespace was read for the in-cluster checks; say which targets of the run were not.
	captured.Warnings = append(captured.Warnings, outsideClusterNotesFor(targets, loads, words)...)
	return captured
}

// envCaptureRead reads one namespace through the executor's own ServiceAccount; a test swaps it so no test
// reaches a cluster the machine it runs on happens to have credentials for.
var envCaptureRead = func(ns string) *envcapture.Capture {
	cl, err := envcapture.NewInClusterClient()
	if err != nil {
		return &envcapture.Capture{Namespace: ns, Reason: "no in-cluster Kubernetes credentials: " + err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), envCaptureTimeout)
	defer cancel()
	captured := envcapture.CaptureNamespace(ctx, cl, ns)
	return &captured
}
