package main

import (
	"fmt"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/k8srender"
)

// sut_logs_via.go — the stderr notices of --sut-logs-via. The flag itself, its refusals
// and the `argus collect-logs` command are in main.go / collect_logs_cmd.go.

// collectSUTLogsNotice is what render-k8s says on stderr, at render time, whenever --collect-sut-logs is
// on: which namespace's pod logs will be read, and by what. The node text is the line this command has
// always printed, byte for byte.
func collectSUTLogsNotice(in k8srender.Instance, emittedRole bool) string {
	if !strings.EqualFold(in.SUTLogsVia, k8srender.SUTLogsViaAPI) {
		return fmt.Sprintf("collect-sut-logs: ON — promtail will read every pod log under namespace %q (SUT_NAMESPACE)\n", in.SUTNamespace)
	}
	if !in.SUTLogsCollectorRendered() {
		// adopt / none / BetterStack: the renderer emits no collector (promtail is silent there too), so
		// there is no Deployment that reads anything and no Role to apply.
		mode := strings.ToLower(strings.TrimSpace(in.ObsMode))
		what := "nothing collects the SUT's pod logs"
		switch {
		case mode == "adopt":
			what = "your own pipeline ships the SUT's pod logs"
		case mode == "export" && in.ObsUseBetterStack:
			mode = "export to BetterStack"
		}
		return fmt.Sprintf("collect-sut-logs: --sut-logs-via api renders nothing: no log collector is rendered in --obs %s, so %s\n", mode, what)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "collect-sut-logs: ON (--sut-logs-via api) — deployment/%s will read every pod log in namespace %q (SUT_NAMESPACE) through the Kubernetes API; no hostPath, no DaemonSet, nothing cluster-scoped\n",
		k8srender.SUTLogsName, in.SUTNamespace)
	if emittedRole {
		fmt.Fprintf(&b, "collect-sut-logs: the owner of namespace %q must apply sut-access-role.yaml, which now holds a SECOND Role (argus-sut-logs-read: pods get/list and pods/log get) for serviceaccount %s in %s; until it is applied the collector logs a 403 once a minute and collects nothing\n",
			in.SUTNamespace, k8srender.SUTLogsServiceAccount, in.Namespace())
	} else {
		fmt.Fprintf(&b, "collect-sut-logs: the owner of namespace %q must apply the Role argus-sut-logs-read (pods get/list and pods/log get) for serviceaccount %s in %s: render it with --emit-sut-access-role; until it is applied the collector logs a 403 once a minute and collects nothing\n",
			in.SUTNamespace, k8srender.SUTLogsServiceAccount, in.Namespace())
	}
	return b.String()
}

// promtailLeftoverNotice is what `argus upgrade` says when it moves an instance to --sut-logs-via api
// and the namespace still holds the promtail objects of `node`. upgrade never removes, so they stay, and
// until they are deleted every line is shipped twice (once by each collector).
func promtailLeftoverNotice(ns string) string {
	return fmt.Sprintf("collect-sut-logs: this instance still runs the promtail of --sut-logs-via node, and upgrade never removes anything: until you delete it, EVERY log line is shipped TWICE (once by promtail, once by deployment/%s). Delete it:\n"+
		"  kubectl -n %s delete daemonset/promtail\n"+
		"  kubectl -n %s delete configmap/promtail-config\n"+
		"  kubectl -n %s delete serviceaccount/promtail\n",
		k8srender.SUTLogsName, ns, ns, ns)
}
