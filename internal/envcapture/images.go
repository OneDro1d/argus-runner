package envcapture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// NewClient builds a Client against an explicit API base — for a caller that is not in-cluster and for
// tests, which point it at an httptest server.
func NewClient(baseURL, token string, hc *http.Client) *Client {
	return &Client{baseURL: baseURL, token: token, hc: hc}
}

// RunningContainer is one live regular container of the SUT namespace and the imageID its runtime
// reported (status.containerStatuses[].imageID) — what is ACTUALLY running, as opposed to the image
// string in the pod spec, which is a tag someone can re-push.
type RunningContainer struct {
	Pod       string
	Container string
	ImageID   string
}

type liveMeta struct {
	Name              string `json:"name"`
	DeletionTimestamp string `json:"deletionTimestamp"`
}

type livePodList struct {
	Items []struct {
		Metadata liveMeta `json:"metadata"`
		Status   struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				Name    string `json:"name"`
				ImageID string `json:"imageID"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// ReadRunningContainers lists the pods of ns and returns the regular containers of every pod that is
// Running and not terminating. It reads pods ONLY (get/list on pods — the narrowest grant, the same Role
// SUTAccessRoleManifest already emits) and keeps nothing from a pod spec: no env, no args, no volumes.
//
// A non-empty reason means nothing was read — forbidden, unreachable, unparsable, or no live pod. It is
// never an empty reading dressed as success.
func ReadRunningContainers(ctx context.Context, cl *Client, ns string) ([]RunningContainer, string) {
	if cl == nil {
		return nil, "no in-cluster Kubernetes credentials available"
	}
	if ns == "" {
		return nil, "no SUT namespace declared"
	}
	body, status, err := cl.get(ctx, "/api/v1/namespaces/"+url.PathEscape(ns)+"/pods")
	if err != nil {
		return nil, "unreachable: " + err.Error()
	}
	if status == http.StatusForbidden {
		return nil, fmt.Sprintf("forbidden: this executor may not list pods in namespace %s (the SUT owner grants it: see --emit-sut-access-role)", ns)
	}
	if status != http.StatusOK {
		return nil, fmt.Sprintf("k8s API GET pods in namespace %s returned status %d", ns, status)
	}
	var list livePodList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, "pods response: " + err.Error()
	}
	var out []RunningContainer
	for _, p := range list.Items {
		if p.Status.Phase != "Running" || p.Metadata.DeletionTimestamp != "" {
			continue
		}
		for _, c := range p.Status.ContainerStatuses {
			out = append(out, RunningContainer{Pod: p.Metadata.Name, Container: c.Name, ImageID: c.ImageID})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Sprintf("no running pod found in namespace %s", ns)
	}
	return out, ""
}
