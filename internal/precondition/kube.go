package precondition

import (
	"context"
	"sort"
)

// Pod is the throwaway pod a probe creates: one container, no ServiceAccount token, no writable root, no
// capabilities, tiny requests and limits (the instance ResourceQuota demands limits).
type Pod struct {
	Name    string
	Labels  map[string]string
	Image   string
	Command []string
}

// PodStatus is the slice of a pod's status a probe reads.
type PodStatus struct {
	Phase         string
	IP            string
	WaitingReason string // e.g. ImagePullBackOff, ErrImagePull
	Terminated    bool   // the container finished
	ExitCode      int
}

// LabelSelector is a Kubernetes label selector, enough of it to evaluate the namespace's own policies.
type LabelSelector struct {
	MatchLabels      map[string]string
	MatchExpressions []Expr
}

// Expr is one matchExpressions entry.
type Expr struct {
	Key      string
	Operator string // In | NotIn | Exists | DoesNotExist
	Values   []string
}

// Matches reports whether labels satisfy the selector. An empty selector matches everything. An operator it
// does not know matches (conservative for the guard: it can only turn an `absent` into a `broken`).
func (s LabelSelector) Matches(labels map[string]string) bool {
	for k, v := range s.MatchLabels {
		if labels[k] != v {
			return false
		}
	}
	for _, e := range s.MatchExpressions {
		v, has := labels[e.Key]
		switch e.Operator {
		case "In":
			if !has || !contains(e.Values, v) {
				return false
			}
		case "NotIn":
			if has && contains(e.Values, v) {
				return false
			}
		case "Exists":
			if !has {
				return false
			}
		case "DoesNotExist":
			if has {
				return false
			}
		}
	}
	return true
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// Peer is one from/to entry of a policy rule. NamespaceSelector and IPBlock are carried only as presence: the
// guard treats either as "may admit" because it cannot evaluate them from inside one namespace.
type Peer struct {
	PodSelector       *LabelSelector
	NamespaceSelector bool
	IPBlock           bool
}

// Rule is one ingress or egress rule. No Peers = every peer. No Ports = every port.
type Rule struct {
	Peers []Peer
	Ports []int
}

// Policy is a NetworkPolicy, as created by a probe and as read back for the ambient guard.
type Policy struct {
	Name        string
	Labels      map[string]string
	PodSelector LabelSelector
	Types       []string // Ingress and/or Egress
	Ingress     []Rule
	Egress      []Rule
}

// Kube is the slice of the Kubernetes API a probe needs, ALL of it namespaced to the executor's own namespace
// (the caller passes ns; the production client is never given another one). Production is KubeAPI over the
// in-cluster ServiceAccount; tests use a fake.
//
// RBAC it needs, and nothing more, in the executor's own namespace only: pods and networkpolicies
// create/get/list/delete (internal/k8srender renders it into the executor's Role).
type Kube interface {
	CreatePod(ctx context.Context, ns string, p Pod) error
	PodStatus(ctx context.Context, ns, name string) (PodStatus, error)
	// ListPods returns the names of the pods matching a label selector.
	ListPods(ctx context.Context, ns, selector string) ([]string, error)
	// DeletePod: absent is success.
	DeletePod(ctx context.Context, ns, name string) error
	CreatePolicy(ctx context.Context, ns string, p Policy) error
	// ListPolicies returns the policies matching a label selector; "" = every policy in the namespace.
	ListPolicies(ctx context.Context, ns, selector string) ([]Policy, error)
	// DeletePolicy: absent is success.
	DeletePolicy(ctx context.Context, ns, name string) error
}

// ambientAdmitting names a NetworkPolicy in the namespace, not created by a probe, that selects `subject` for
// `direction` and has a rule admitting `peer` on `port`. "" = none does.
//
// WHY: policies are additive allow-lists. If one of them admits the probe's traffic, a deny the probe adds
// cannot close it, so a connection that still works says nothing about enforcement. Reading that as `absent`
// would excuse checks on a cluster that does enforce. The guard turns that case into `broken` instead.
func (r *probeRun) ambientAdmitting(ctx context.Context, direction string, subject, peer map[string]string, port int) (string, error) {
	pols, err := r.p.Kube.ListPolicies(ctx, r.p.NS, "")
	if err != nil {
		return "", err
	}
	var names []string
	for _, pol := range pols {
		if pol.Labels[LabelProbe] != "" {
			continue // ours
		}
		if !contains(pol.Types, direction) || !pol.PodSelector.Matches(subject) {
			continue
		}
		rules := pol.Ingress
		if direction == "Egress" {
			rules = pol.Egress
		}
		for _, rule := range rules {
			if ruleAdmits(rule, peer, port) {
				names = append(names, pol.Name)
				break
			}
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	sort.Strings(names)
	return names[0], nil
}

func ruleAdmits(rule Rule, peer map[string]string, port int) bool {
	if len(rule.Ports) > 0 && !containsInt(rule.Ports, port) {
		return false
	}
	if len(rule.Peers) == 0 {
		return true
	}
	for _, p := range rule.Peers {
		switch {
		case p.NamespaceSelector, p.IPBlock:
			return true
		case p.PodSelector == nil || p.PodSelector.Matches(peer):
			return true
		}
	}
	return false
}

func containsInt(set []int, v int) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
