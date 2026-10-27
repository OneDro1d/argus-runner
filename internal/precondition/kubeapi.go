package precondition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
)

// KubeAPI is Kube over envcapture's raw in-cluster client (no client-go, as for every other caller).
type KubeAPI struct{ C *envcapture.Client }

const saNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// DefaultKube is the production factory: the executor's in-cluster ServiceAccount, and the namespace that
// ServiceAccount lives in - the executor's OWN. The namespace is read from the pod, never from a scenario or
// a config value, so no input can point a probe at another namespace. Outside a pod it errors and every
// precondition ends `broken` by name.
func DefaultKube() (Kube, string, error) {
	c, err := envcapture.NewInClusterClient()
	if err != nil {
		return nil, "", errors.New("no in-cluster ServiceAccount: a precondition probe can only run from an executor pod")
	}
	b, err := os.ReadFile(saNamespaceFile)
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return nil, "", errors.New("the executor's own namespace could not be read from its ServiceAccount")
	}
	return &KubeAPI{C: c}, strings.TrimSpace(string(b)), nil
}

func podsPath(ns string) string { return "/api/v1/namespaces/" + url.PathEscape(ns) + "/pods" }
func polsPath(ns string) string {
	return "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(ns) + "/networkpolicies"
}

func statusErr(op string, code int) error {
	switch code {
	case http.StatusForbidden:
		return fmt.Errorf("%s: forbidden - the executor's Role in its own namespace lacks pods/networkpolicies rights (re-render with render-k8s)", op)
	case http.StatusUnauthorized:
		return fmt.Errorf("%s: unauthorized", op)
	case http.StatusConflict:
		return fmt.Errorf("%s: already exists", op)
	}
	return fmt.Errorf("%s: kubernetes answered HTTP %d", op, code)
}

// cause drops the URL a transport error carries; an address never reaches a report.
func cause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if errors.Is(ue.Err, context.DeadlineExceeded) {
			return errors.New("timed out")
		}
		return errors.New("the request failed")
	}
	return err
}

func (k *KubeAPI) do(ctx context.Context, op, method, path, ct string, body []byte) ([]byte, int, error) {
	b, code, err := k.C.Do(ctx, method, path, ct, body)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", op, cause(err))
	}
	return b, code, nil
}

func (k *KubeAPI) CreatePod(ctx context.Context, ns string, p Pod) error {
	one := int64(0)
	user := int64(65534)
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": p.Name, "labels": p.Labels},
		"spec": map[string]any{
			"restartPolicy": "Never", "automountServiceAccountToken": false,
			"terminationGracePeriodSeconds": one, "enableServiceLinks": false,
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": user, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"containers": []any{map[string]any{
				"name": "probe", "image": p.Image, "command": p.Command,
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
				"resources": map[string]any{
					"requests": map[string]any{"cpu": "10m", "memory": "16Mi"},
					"limits":   map[string]any{"cpu": "50m", "memory": "32Mi"},
				},
			}},
		},
	}
	b, _ := json.Marshal(pod)
	_, code, err := k.do(ctx, "create pod", http.MethodPost, podsPath(ns), "application/json", b)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return statusErr("create pod", code)
	}
	return nil
}

func (k *KubeAPI) PodStatus(ctx context.Context, ns, name string) (PodStatus, error) {
	b, code, err := k.do(ctx, "read pod", http.MethodGet, podsPath(ns)+"/"+url.PathEscape(name), "", nil)
	if err != nil {
		return PodStatus{}, err
	}
	if code/100 != 2 {
		return PodStatus{}, statusErr("read pod", code)
	}
	var d struct {
		Status struct {
			Phase             string `json:"phase"`
			PodIP             string `json:"podIP"`
			ContainerStatuses []struct {
				State struct {
					Waiting    *struct{ Reason string } `json:"waiting"`
					Terminated *struct {
						ExitCode int `json:"exitCode"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if json.Unmarshal(b, &d) != nil {
		return PodStatus{}, errors.New("read pod: unreadable answer")
	}
	st := PodStatus{Phase: d.Status.Phase, IP: d.Status.PodIP}
	for _, cs := range d.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			st.WaitingReason = cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil {
			st.Terminated, st.ExitCode = true, cs.State.Terminated.ExitCode
		}
	}
	return st, nil
}

func selectorQuery(sel string) string {
	if sel == "" {
		return ""
	}
	return "?labelSelector=" + url.QueryEscape(sel)
}

func (k *KubeAPI) ListPods(ctx context.Context, ns, selector string) ([]string, error) {
	b, code, err := k.do(ctx, "list pods", http.MethodGet, podsPath(ns)+selectorQuery(selector), "", nil)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return nil, statusErr("list pods", code)
	}
	var l struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal(b, &l) != nil {
		return nil, errors.New("list pods: unreadable answer")
	}
	var out []string
	for _, i := range l.Items {
		out = append(out, i.Metadata.Name)
	}
	return out, nil
}

func (k *KubeAPI) DeletePod(ctx context.Context, ns, name string) error {
	_, code, err := k.do(ctx, "delete pod", http.MethodDelete, podsPath(ns)+"/"+url.PathEscape(name), "", nil)
	if err != nil {
		return err
	}
	if code/100 != 2 && code != http.StatusNotFound {
		return statusErr("delete pod", code)
	}
	return nil
}

func selectorJSON(s LabelSelector) map[string]any {
	m := map[string]any{}
	if len(s.MatchLabels) > 0 {
		m["matchLabels"] = s.MatchLabels
	}
	return m
}

func ruleJSON(key string, r Rule) map[string]any {
	m := map[string]any{}
	if len(r.Peers) > 0 {
		var peers []any
		for _, p := range r.Peers {
			if p.PodSelector != nil {
				peers = append(peers, map[string]any{"podSelector": selectorJSON(*p.PodSelector)})
			}
		}
		m[key] = peers
	}
	if len(r.Ports) > 0 {
		var ports []any
		for _, p := range r.Ports {
			ports = append(ports, map[string]any{"protocol": "TCP", "port": p})
		}
		m["ports"] = ports
	}
	return m
}

func (k *KubeAPI) CreatePolicy(ctx context.Context, ns string, p Policy) error {
	spec := map[string]any{"podSelector": selectorJSON(p.PodSelector), "policyTypes": p.Types}
	if len(p.Ingress) > 0 {
		var rs []any
		for _, r := range p.Ingress {
			rs = append(rs, ruleJSON("from", r))
		}
		spec["ingress"] = rs
	}
	if len(p.Egress) > 0 {
		var rs []any
		for _, r := range p.Egress {
			rs = append(rs, ruleJSON("to", r))
		}
		spec["egress"] = rs
	}
	b, _ := json.Marshal(map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": p.Name, "labels": p.Labels}, "spec": spec,
	})
	_, code, err := k.do(ctx, "create networkpolicy", http.MethodPost, polsPath(ns), "application/json", b)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return statusErr("create networkpolicy", code)
	}
	return nil
}

type apiSelector struct {
	MatchLabels      map[string]string `json:"matchLabels"`
	MatchExpressions []struct {
		Key      string   `json:"key"`
		Operator string   `json:"operator"`
		Values   []string `json:"values"`
	} `json:"matchExpressions"`
}

func (a apiSelector) sel() LabelSelector {
	s := LabelSelector{MatchLabels: a.MatchLabels}
	for _, e := range a.MatchExpressions {
		s.MatchExpressions = append(s.MatchExpressions, Expr{Key: e.Key, Operator: e.Operator, Values: e.Values})
	}
	return s
}

type apiPeer struct {
	PodSelector       *apiSelector    `json:"podSelector"`
	NamespaceSelector *apiSelector    `json:"namespaceSelector"`
	IPBlock           json.RawMessage `json:"ipBlock"`
}

type apiRule struct {
	From  []apiPeer `json:"from"`
	To    []apiPeer `json:"to"`
	Ports []struct {
		Port json.RawMessage `json:"port"`
	} `json:"ports"`
}

func (r apiRule) rule(egress bool) Rule {
	peers := r.From
	if egress {
		peers = r.To
	}
	out := Rule{}
	for _, p := range peers {
		peer := Peer{NamespaceSelector: p.NamespaceSelector != nil, IPBlock: len(p.IPBlock) > 0}
		if p.PodSelector != nil {
			s := p.PodSelector.sel()
			peer.PodSelector = &s
		}
		out.Peers = append(out.Peers, peer)
	}
	for _, p := range r.Ports {
		var n int
		if json.Unmarshal(p.Port, &n) == nil {
			out.Ports = append(out.Ports, n)
		} else {
			// A named or absent port: unknown, so the rule is read as admitting every port (conservative).
			out.Ports = nil
			break
		}
	}
	return out
}

func (k *KubeAPI) ListPolicies(ctx context.Context, ns, selector string) ([]Policy, error) {
	b, code, err := k.do(ctx, "list networkpolicies", http.MethodGet, polsPath(ns)+selectorQuery(selector), "", nil)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return nil, statusErr("list networkpolicies", code)
	}
	var l struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				PodSelector apiSelector `json:"podSelector"`
				PolicyTypes []string    `json:"policyTypes"`
				Ingress     []apiRule   `json:"ingress"`
				Egress      []apiRule   `json:"egress"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal(b, &l) != nil {
		return nil, errors.New("list networkpolicies: unreadable answer")
	}
	var out []Policy
	for _, i := range l.Items {
		p := Policy{Name: i.Metadata.Name, Labels: i.Metadata.Labels, PodSelector: i.Spec.PodSelector.sel(), Types: i.Spec.PolicyTypes}
		if len(p.Types) == 0 { // the API's own default: Ingress always, Egress when it has egress rules
			p.Types = []string{"Ingress"}
			if len(i.Spec.Egress) > 0 {
				p.Types = append(p.Types, "Egress")
			}
		}
		for _, r := range i.Spec.Ingress {
			p.Ingress = append(p.Ingress, r.rule(false))
		}
		for _, r := range i.Spec.Egress {
			p.Egress = append(p.Egress, r.rule(true))
		}
		out = append(out, p)
	}
	return out, nil
}

func (k *KubeAPI) DeletePolicy(ctx context.Context, ns, name string) error {
	_, code, err := k.do(ctx, "delete networkpolicy", http.MethodDelete, polsPath(ns)+"/"+url.PathEscape(name), "", nil)
	if err != nil {
		return err
	}
	if code/100 != 2 && code != http.StatusNotFound {
		return statusErr("delete networkpolicy", code)
	}
	return nil
}
