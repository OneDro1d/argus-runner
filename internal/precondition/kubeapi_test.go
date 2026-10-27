package precondition

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
)

// The production Kube over a fake API server: the paths, the verbs and the shapes the executor's Role must allow,
// and the parse of the namespace's own policies (the ambient guard's input).

type apiCall struct{ method, path, query, body string }

func fakeAPI(t *testing.T, answer func(c apiCall) (int, string)) (*KubeAPI, *[]apiCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []apiCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c := apiCall{r.Method, r.URL.Path, r.URL.RawQuery, string(b)}
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		code, body := answer(c)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &KubeAPI{C: envcapture.NewClientWith(srv.URL, "tok", srv.Client())}, &calls
}

func TestKubeAPI_EveryCallStaysInTheGivenNamespaceAndUsesOnlyTheRoleVerbs(t *testing.T) {
	k, calls := fakeAPI(t, func(c apiCall) (int, string) {
		switch {
		case c.method == "GET" && strings.HasSuffix(c.path, "/pods"):
			return 200, `{"items":[{"metadata":{"name":"p1"}}]}`
		case c.method == "GET" && strings.Contains(c.path, "/pods/"):
			return 200, `{"status":{"phase":"Running","podIP":"10.1.2.3"}}`
		case c.method == "GET":
			return 200, `{"items":[]}`
		}
		return 201, `{}`
	})
	ctx := context.Background()
	_ = k.CreatePod(ctx, "argus-inst-x", Pod{Name: "p1", Labels: map[string]string{"a": "b"}, Image: "busybox:1", Command: []string{"sh"}})
	_, _ = k.PodStatus(ctx, "argus-inst-x", "p1")
	_, _ = k.ListPods(ctx, "argus-inst-x", LabelProbe)
	_ = k.DeletePod(ctx, "argus-inst-x", "p1")
	_ = k.CreatePolicy(ctx, "argus-inst-x", Policy{Name: "n1", PodSelector: LabelSelector{MatchLabels: map[string]string{"a": "b"}}, Types: []string{"Ingress"}})
	_, _ = k.ListPolicies(ctx, "argus-inst-x", "")
	_ = k.DeletePolicy(ctx, "argus-inst-x", "n1")

	allowed := map[string]bool{
		"POST /api/v1/namespaces/argus-inst-x/pods":                                    true, // create pods
		"GET /api/v1/namespaces/argus-inst-x/pods/p1":                                  true, // get pods
		"GET /api/v1/namespaces/argus-inst-x/pods":                                     true, // list pods
		"DELETE /api/v1/namespaces/argus-inst-x/pods/p1":                               true, // delete pods
		"POST /apis/networking.k8s.io/v1/namespaces/argus-inst-x/networkpolicies":      true,
		"GET /apis/networking.k8s.io/v1/namespaces/argus-inst-x/networkpolicies":       true,
		"DELETE /apis/networking.k8s.io/v1/namespaces/argus-inst-x/networkpolicies/n1": true,
	}
	if len(*calls) != 7 {
		t.Fatalf("calls = %d: %+v", len(*calls), *calls)
	}
	for _, c := range *calls {
		if !allowed[c.method+" "+c.path] {
			t.Errorf("a call outside the Role's rights or outside the namespace: %s %s", c.method, c.path)
		}
	}
}

func TestKubeAPI_PodIsTinyUnprivilegedAndHasLimits(t *testing.T) {
	k, calls := fakeAPI(t, func(apiCall) (int, string) { return 201, `{}` })
	if err := k.CreatePod(context.Background(), "ns", Pod{Name: "p", Labels: map[string]string{LabelProbe: "true"}, Image: "busybox:1.36.1", Command: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatal(err)
	}
	var pod struct {
		Spec struct {
			AutomountServiceAccountToken bool   `json:"automountServiceAccountToken"`
			RestartPolicy                string `json:"restartPolicy"`
			SecurityContext              struct {
				RunAsNonRoot bool `json:"runAsNonRoot"`
			} `json:"securityContext"`
			Containers []struct {
				Image           string `json:"image"`
				SecurityContext struct {
					AllowPrivilegeEscalation bool `json:"allowPrivilegeEscalation"`
					ReadOnlyRootFilesystem   bool `json:"readOnlyRootFilesystem"`
					Capabilities             struct {
						Drop []string `json:"drop"`
					} `json:"capabilities"`
				} `json:"securityContext"`
				Resources struct {
					Limits map[string]string `json:"limits"`
				} `json:"resources"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte((*calls)[0].body), &pod); err != nil {
		t.Fatal(err)
	}
	c := pod.Spec.Containers[0]
	if pod.Spec.AutomountServiceAccountToken || pod.Spec.RestartPolicy != "Never" || !pod.Spec.SecurityContext.RunAsNonRoot ||
		c.SecurityContext.AllowPrivilegeEscalation || !c.SecurityContext.ReadOnlyRootFilesystem || len(c.SecurityContext.Capabilities.Drop) != 1 ||
		c.Resources.Limits["cpu"] == "" || c.Resources.Limits["memory"] == "" || c.Image != "busybox:1.36.1" {
		t.Fatalf("the probe pod is not tiny and unprivileged: %s", (*calls)[0].body)
	}
}

func TestKubeAPI_PodStatusParsesPhaseIPWaitingAndExitCode(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want PodStatus
	}{
		"running":    {`{"status":{"phase":"Running","podIP":"10.0.0.7"}}`, PodStatus{Phase: "Running", IP: "10.0.0.7"}},
		"pull error": {`{"status":{"phase":"Pending","containerStatuses":[{"state":{"waiting":{"reason":"ErrImagePull"}}}]}}`, PodStatus{Phase: "Pending", WaitingReason: "ErrImagePull"}},
		"exited 1":   {`{"status":{"phase":"Failed","containerStatuses":[{"state":{"terminated":{"exitCode":1}}}]}}`, PodStatus{Phase: "Failed", Terminated: true, ExitCode: 1}},
		"exited 0":   {`{"status":{"phase":"Succeeded","containerStatuses":[{"state":{"terminated":{"exitCode":0}}}]}}`, PodStatus{Phase: "Succeeded", Terminated: true}},
	} {
		k, _ := fakeAPI(t, func(apiCall) (int, string) { return 200, tc.body })
		got, err := k.PodStatus(context.Background(), "ns", "p")
		if err != nil || got.Phase != tc.want.Phase || got.IP != tc.want.IP || got.Terminated != tc.want.Terminated || got.ExitCode != tc.want.ExitCode || got.WaitingReason != tc.want.WaitingReason {
			t.Errorf("%s: %+v %v, want %+v", name, got, err, tc.want)
		}
	}
}

func TestKubeAPI_ForbiddenIsNamedNotSwallowed_AndAbsentDeletesAreSuccess(t *testing.T) {
	k, _ := fakeAPI(t, func(c apiCall) (int, string) {
		if c.method == "DELETE" {
			return 404, `{}`
		}
		return 403, `{}`
	})
	if err := k.CreatePolicy(context.Background(), "ns", Policy{Name: "n", Types: []string{"Ingress"}}); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("a 403 must be named: %v", err)
	}
	if _, err := k.ListPolicies(context.Background(), "ns", ""); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("a 403 must be named: %v", err)
	}
	if err := k.DeletePod(context.Background(), "ns", "gone"); err != nil {
		t.Fatalf("deleting what is already gone is success: %v", err)
	}
	if err := k.DeletePolicy(context.Background(), "ns", "gone"); err != nil {
		t.Fatalf("deleting what is already gone is success: %v", err)
	}
}

// The namespace's own policies as k8srender renders them parse into what the guard evaluates.
func TestKubeAPI_ListPoliciesParsesTheRenderedNamespacePolicies(t *testing.T) {
	k, _ := fakeAPI(t, func(apiCall) (int, string) {
		return 200, `{"items":[
 {"metadata":{"name":"default-deny-ingress"},"spec":{"podSelector":{},"policyTypes":["Ingress"]}},
 {"metadata":{"name":"allow-same-namespace"},"spec":{"podSelector":{"matchExpressions":[{"key":"` + LabelIsolated + `","operator":"DoesNotExist"}]},
   "policyTypes":["Ingress"],"ingress":[{"from":[{"podSelector":{}}]}]}},
 {"metadata":{"name":"to-world"},"spec":{"podSelector":{},"egress":[{"to":[{"ipBlock":{"cidr":"0.0.0.0/0"}}],"ports":[{"port":443}]}]}}]}`
	})
	pols, err := k.ListPolicies(context.Background(), "ns", "")
	if err != nil || len(pols) != 3 {
		t.Fatalf("%v %v", pols, err)
	}
	if len(pols[0].Ingress) != 0 || !contains(pols[0].Types, "Ingress") {
		t.Errorf("default-deny: %+v", pols[0])
	}
	allow := pols[1]
	if allow.PodSelector.Matches(map[string]string{LabelIsolated: "true"}) || !allow.PodSelector.Matches(map[string]string{"app": "x"}) {
		t.Errorf("allow-same-namespace must select everything BUT the isolated listener: %+v", allow.PodSelector)
	}
	if len(allow.Ingress) != 1 || len(allow.Ingress[0].Peers) != 1 || allow.Ingress[0].Peers[0].PodSelector == nil {
		t.Errorf("allow-same-namespace rule: %+v", allow.Ingress)
	}
	if !contains(pols[2].Types, "Egress") || len(pols[2].Egress) != 1 || !pols[2].Egress[0].Peers[0].IPBlock || pols[2].Egress[0].Ports[0] != 443 {
		t.Errorf("an egress-only policy defaults to Ingress+Egress types with its rule: %+v", pols[2])
	}
}
