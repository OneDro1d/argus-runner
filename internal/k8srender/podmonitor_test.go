package k8srender

// INT-015: the MANAGED tier had no metrics pipeline at all.
//
// Onboarding reasoned that "the cluster's kube-prometheus-stack Prometheus already scrapes
// namespace-wide, so the instance's pushgateway is discovered without a targets.d file". The
// operator's namespaceSelector really is {} — every namespace — but its serviceMonitorSelector and
// podMonitorSelector are matchLabels{release: kube-prometheus-stack}. Discovery is by LABELLED CRD,
// never by namespace membership, so rendering neither object meant nothing ever scraped the
// pushgateway. Measured live on social-aks-v1: the cluster Prometheus answered
// /api/v1/label/argus_instance/values with [] while that instance's pushgateway held 57 argus_*
// series. Every metric panel on the managed dashboard was empty forever, beside a Loki datasource
// that worked — which is why it read as a rendering glitch rather than a missing pipeline.
//
// These tests pin the three things that each independently make the object useless if wrong, all of
// which were established by measurement rather than by reading the docs:
//
//   1. the operator's LABEL, without which the object is invisible;
//   2. PodMonitor rather than ServiceMonitor, because the rendered pushgateway Service carries NO
//      metadata.labels — a ServiceMonitor selecting app=pushgateway is discovered and then drops
//      every target, which is exactly what happened on the first attempt;
//   3. honorLabels, without which Prometheus overwrites instance/job with the pod address and the
//      dashboard's argus_instance filter matches nothing.

import (
	"strings"
	"testing"
)

func obsFor(t *testing.T, tier string, promLabel string) string {
	t.Helper()
	in := sampleInstance()
	in.ID = "social-aks-v1"
	in.Tier = tier
	in.Cluster = tier
	in.PromOperatorLabel = promLabel
	out, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(%s): %v", tier, err)
	}
	return out
}

func TestPodMonitor_RenderedOnAKS_WithEverythingThatMakesItWork(t *testing.T) {
	out := obsFor(t, "aks", "")

	if !strings.Contains(out, "kind: PodMonitor") {
		t.Fatal("no PodMonitor rendered for the aks tier — the managed instance's pushgateway is " +
			"never scraped and EVERY metric panel on its dashboard is empty forever (INT-015)")
	}
	// (1) the operator's selector label — the object is invisible without it.
	if !strings.Contains(out, "release: kube-prometheus-stack") {
		t.Error("PodMonitor is missing the operator's selector label. podMonitorSelector is " +
			"matchLabels{release: kube-prometheus-stack}; an unlabelled PodMonitor is never read, and " +
			"nothing anywhere reports that it was ignored")
	}
	// (2) it must select the POD's label, which exists — not the Service's, which does not.
	if !strings.Contains(out, "matchLabels: {app: pushgateway}") {
		t.Error("PodMonitor must select app=pushgateway — the label the POD actually carries")
	}
	if strings.Contains(out, "kind: ServiceMonitor") {
		t.Error("a ServiceMonitor selects the SERVICE's own metadata.labels, and the rendered " +
			"pushgateway Service has none — measured: the operator discovers it and drops every target")
	}
	// (3) honorLabels — else instance/job become the pod address and the dashboard filter dies.
	if !strings.Contains(out, "honorLabels: true") {
		t.Error("honorLabels missing: Prometheus would overwrite the pushed instance/job labels with " +
			"the pod address, and the dashboard's argus_instance filter would match nothing")
	}
	// the port NAME, not the number — a numeric port silently selects nothing.
	if !strings.Contains(out, "port: http") {
		t.Error("PodMonitor must reference the port NAME 'http'; a numeric port selects nothing silently")
	}
}

func TestPodMonitor_NotRenderedOnLocalTiers(t *testing.T) {
	// k3d is scraped by the off-cluster compose Prometheus via targets.d file_sd. Rendering a
	// PodMonitor there would also fail the apply outright on a cluster with no Prometheus Operator
	// CRDs installed — which is the normal k3d case.
	if out := obsFor(t, "k3d", ""); strings.Contains(out, "PodMonitor") {
		t.Error("a PodMonitor was rendered for k3d: it is redundant (targets.d file_sd already covers " +
			"it) and the apply FAILS where the monitoring.coreos.com CRDs are absent")
	}
}

// TestPodMonitor_OptOutForAClusterWithoutTheOperator: a managed tier on a cluster with no Prometheus
// Operator (a homelab k3s, found by a fresh agent 2026-09-23) cannot create a PodMonitor at all —
// `kubectl create` fails on it with "no matches for kind PodMonitor". render-k8s never talks to the
// cluster, so it cannot detect that; ARGUS_OBS_PROM_LABEL=none is the operator saying so explicitly.
// Everything else in the obs plane must still render: only the scrape wiring is dropped.
func TestPodMonitor_OptOutForAClusterWithoutTheOperator(t *testing.T) {
	for _, v := range []string{"none", "NONE", " none "} {
		out := obsFor(t, "managed", v)
		if strings.Contains(out, "PodMonitor") {
			t.Errorf("ARGUS_OBS_PROM_LABEL=%q still rendered a PodMonitor; on a cluster without the "+
				"monitoring.coreos.com CRDs the create fails on it", v)
		}
		if strings.Contains(out, "none: ") {
			t.Errorf("ARGUS_OBS_PROM_LABEL=%q was read as a label key", v)
		}
		if !strings.Contains(out, "name: pushgateway") || !strings.Contains(out, "name: loki") {
			t.Errorf("ARGUS_OBS_PROM_LABEL=%q dropped more than the PodMonitor; the obs plane itself must still render", v)
		}
	}
	// And the default is unchanged: no opt-out, still rendered.
	if out := obsFor(t, "managed", ""); !strings.Contains(out, "kind: PodMonitor") {
		t.Error("the managed tier stopped rendering its PodMonitor by default — INT-015 regresses")
	}
}

func TestPodMonitor_OperatorLabelIsDiscoverable_NotHardcodedToOneCluster(t *testing.T) {
	// The default matches example-cluster, but the label is a per-cluster convention. Onboarding reads
	// the real one off the Prometheus CR's serviceMonitorSelector; a cluster using a different one
	// must still work, or this fix silently does nothing there — the same invisible failure again.
	out := obsFor(t, "aks", "prometheus=main")
	if !strings.Contains(out, "prometheus: main") {
		t.Error("PromOperatorLabel override was ignored — the fix would be silently inert on any " +
			"cluster whose operator does not use release=kube-prometheus-stack")
	}
	if strings.Contains(out, "release: kube-prometheus-stack") {
		t.Error("the default label was emitted alongside the override; only the discovered one applies")
	}
}
