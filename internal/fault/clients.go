package fault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
)

// Kube is the slice of the Kubernetes API a fault step needs: read and set a Deployment's replica count, and
// read whether it is ready. Production is KubeAPI over the in-cluster ServiceAccount; tests use fakes.
type Kube interface {
	GetScale(ctx context.Context, ns, name string) (int, error)
	SetScale(ctx context.Context, ns, name string, replicas int) error
	// Ready reports spec.replicas and status.readyReplicas, and whether the controller has observed the
	// latest spec (settled). Right after a scale the status still describes the OLD generation; a caller must
	// not read "ready == spec" off it.
	Ready(ctx context.Context, ns, name string) (spec, ready int, settled bool, err error)
}

// KubeAPI is Kube over envcapture's raw in-cluster client (no client-go, as for every other caller).
//
// RBAC it needs, and nothing more: deployments/scale get+patch and deployments get, resourceNames limited to
// the allowlisted deployment names (docs/fault-steps.md carries the Role).
type KubeAPI struct{ C *envcapture.Client }

func depPath(ns, name string) string {
	return "/apis/apps/v1/namespaces/" + url.PathEscape(ns) + "/deployments/" + url.PathEscape(name)
}

func kubeStatusErr(op string, code int) error {
	switch code {
	case http.StatusForbidden:
		return fmt.Errorf("%s: forbidden - the executor's ServiceAccount may not do this (grant the Role in docs/fault-steps.md)", op)
	case http.StatusNotFound:
		return fmt.Errorf("%s: deployment not found", op)
	case http.StatusUnauthorized:
		return fmt.Errorf("%s: unauthorized", op)
	}
	return fmt.Errorf("%s: kubernetes answered HTTP %d", op, code)
}

func (k *KubeAPI) GetScale(ctx context.Context, ns, name string) (int, error) {
	b, code, err := k.C.Do(ctx, http.MethodGet, depPath(ns, name)+"/scale", "", nil)
	if err != nil {
		return 0, fmt.Errorf("read scale: %w", sanitize(err))
	}
	if code/100 != 2 {
		return 0, kubeStatusErr("read scale", code)
	}
	var s struct {
		Spec struct {
			Replicas int `json:"replicas"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return 0, errors.New("read scale: unreadable answer")
	}
	return s.Spec.Replicas, nil
}

func (k *KubeAPI) SetScale(ctx context.Context, ns, name string, replicas int) error {
	body := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	_, code, err := k.C.Do(ctx, http.MethodPatch, depPath(ns, name)+"/scale", "application/merge-patch+json", body)
	if err != nil {
		return fmt.Errorf("set scale: %w", sanitize(err))
	}
	if code/100 != 2 {
		return kubeStatusErr("set scale", code)
	}
	return nil
}

func (k *KubeAPI) Ready(ctx context.Context, ns, name string) (int, int, bool, error) {
	b, code, err := k.C.Do(ctx, http.MethodGet, depPath(ns, name), "", nil)
	if err != nil {
		return 0, 0, false, fmt.Errorf("read deployment: %w", sanitize(err))
	}
	if code/100 != 2 {
		return 0, 0, false, kubeStatusErr("read deployment", code)
	}
	var d struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Spec struct {
			Replicas int `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration int64 `json:"observedGeneration"`
			ReadyReplicas      int   `json:"readyReplicas"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return 0, 0, false, errors.New("read deployment: unreadable answer")
	}
	return d.Spec.Replicas, d.Status.ReadyReplicas, d.Status.ObservedGeneration >= d.Metadata.Generation, nil
}

// sanitize drops the URL a transport error carries ("Patch \"https://host/...\": dial tcp ..."): only the
// cause is kept. An address never reaches a report, a log line or a refusal.
func sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if errors.Is(ue.Err, context.DeadlineExceeded) {
			return errors.New("timed out")
		}
		return errors.New("the request failed (" + shortCause(ue.Err) + ")")
	}
	return err
}

func shortCause(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return "transport error"
}

// Toxic is one Toxiproxy toxic. Attributes are integers (latency, jitter, bytes).
type Toxic struct {
	Name       string
	Type       string // latency | limit_data
	Stream     string // downstream
	Attributes map[string]int
}

// Toxi is the slice of the Toxiproxy v2 HTTP API a fault step needs.
type Toxi interface {
	ProxyEnabled(ctx context.Context, proxy string) (bool, error)
	SetEnabled(ctx context.Context, proxy string, enabled bool) error
	AddToxic(ctx context.Context, proxy string, t Toxic) error
	// DeleteToxic reports whether the toxic existed. Absent is (false, nil): "already gone" is success.
	DeleteToxic(ctx context.Context, proxy, name string) (bool, error)
}

// Toxiproxy is Toxi over HTTP. Its errors never contain the API address.
type Toxiproxy struct {
	base string
	hc   *http.Client
}

func NewToxiproxy(api string, hc *http.Client) *Toxiproxy {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Toxiproxy{base: api, hc: hc}
}

func (x *Toxiproxy) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, x.base+path, rd)
	if err != nil {
		return nil, 0, errors.New("toxiproxy: bad request")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := x.hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("toxiproxy: %w", sanitize(err))
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(http.MaxBytesReader(nil, resp.Body, 1<<20))
	return buf.Bytes(), resp.StatusCode, nil
}

func (x *Toxiproxy) ProxyEnabled(ctx context.Context, proxy string) (bool, error) {
	b, code, err := x.do(ctx, http.MethodGet, "/proxies/"+url.PathEscape(proxy), nil)
	if err != nil {
		return false, err
	}
	if code == http.StatusNotFound {
		return false, errors.New("toxiproxy: the proxy named in fault_allowed_targets does not exist")
	}
	if code/100 != 2 {
		return false, fmt.Errorf("toxiproxy answered HTTP %d reading the proxy", code)
	}
	var p struct {
		Enabled bool `json:"enabled"`
	}
	if json.Unmarshal(b, &p) != nil {
		return false, errors.New("toxiproxy: unreadable answer")
	}
	return p.Enabled, nil
}

func (x *Toxiproxy) SetEnabled(ctx context.Context, proxy string, enabled bool) error {
	_, code, err := x.do(ctx, http.MethodPost, "/proxies/"+url.PathEscape(proxy), map[string]any{"enabled": enabled})
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return fmt.Errorf("toxiproxy answered HTTP %d setting the proxy", code)
	}
	return nil
}

func (x *Toxiproxy) AddToxic(ctx context.Context, proxy string, t Toxic) error {
	_, code, err := x.do(ctx, http.MethodPost, "/proxies/"+url.PathEscape(proxy)+"/toxics", map[string]any{
		"name": t.Name, "type": t.Type, "stream": t.Stream, "toxicity": 1.0, "attributes": t.Attributes,
	})
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return fmt.Errorf("toxiproxy answered HTTP %d adding a toxic", code)
	}
	return nil
}

func (x *Toxiproxy) DeleteToxic(ctx context.Context, proxy, name string) (bool, error) {
	_, code, err := x.do(ctx, http.MethodDelete, "/proxies/"+url.PathEscape(proxy)+"/toxics/"+url.PathEscape(name), nil)
	if err != nil {
		return false, err
	}
	switch {
	case code == http.StatusNotFound:
		return false, nil
	case code/100 != 2:
		return false, fmt.Errorf("toxiproxy answered HTTP %d removing a toxic", code)
	}
	return true, nil
}

// DefaultKube is the production Kube factory: the executor's in-cluster ServiceAccount. Outside a pod it
// returns an error, and the fault step fails by name rather than pretending.
func DefaultKube() (Kube, error) {
	c, err := envcapture.NewInClusterClient()
	if err != nil {
		return nil, errors.New("no in-cluster ServiceAccount: a deployment fault can only run from an executor pod")
	}
	return &KubeAPI{C: c}, nil
}

// DefaultToxi is the production Toxi factory.
func DefaultToxi(api string) Toxi { return NewToxiproxy(api, nil) }
