package fault

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
)

// fakeAPIServer is an httptest Kubernetes API: it records every request and answers from a table.
type fakeAPIServer struct {
	mu   sync.Mutex
	reqs []string
	body map[string]string
	ct   map[string]string
	resp map[string]string
	code map[string]int
}

func (f *fakeAPIServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		b, _ := io.ReadAll(r.Body)
		f.reqs = append(f.reqs, key)
		if f.body == nil {
			f.body, f.ct = map[string]string{}, map[string]string{}
		}
		f.body[key], f.ct[key] = string(b), r.Header.Get("Content-Type")
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		if c, ok := f.code[key]; ok {
			w.WriteHeader(c)
		}
		_, _ = io.WriteString(w, f.resp[key])
	})
}

func TestKubeAPI_UsesTheScaleSubresource_WithMergePatch(t *testing.T) {
	f := &fakeAPIServer{resp: map[string]string{
		"GET /apis/apps/v1/namespaces/calm-demo/deployments/trades/scale":   `{"spec":{"replicas":3}}`,
		"PATCH /apis/apps/v1/namespaces/calm-demo/deployments/trades/scale": `{"spec":{"replicas":0}}`,
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	k := &KubeAPI{C: envcapture.NewClient(srv.URL, "tok", srv.Client())}
	n, err := k.GetScale(ctx(), "calm-demo", "trades")
	if err != nil || n != 3 {
		t.Fatalf("GetScale = %d, %v", n, err)
	}
	if err := k.SetScale(ctx(), "calm-demo", "trades", 0); err != nil {
		t.Fatal(err)
	}
	key := "PATCH /apis/apps/v1/namespaces/calm-demo/deployments/trades/scale"
	if f.ct[key] != "application/merge-patch+json" || f.body[key] != `{"spec":{"replicas":0}}` {
		t.Fatalf("patch sent %q / %q", f.ct[key], f.body[key])
	}
}

func TestKubeAPI_ForbiddenAndNotFoundAreNamedNotSwallowed(t *testing.T) {
	f := &fakeAPIServer{code: map[string]int{
		"GET /apis/apps/v1/namespaces/calm-demo/deployments/trades/scale": 403,
		"GET /apis/apps/v1/namespaces/calm-demo/deployments/gone/scale":   404,
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	k := &KubeAPI{C: envcapture.NewClient(srv.URL, "tok", srv.Client())}
	if _, err := k.GetScale(ctx(), "calm-demo", "trades"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("403: %v", err)
	}
	if _, err := k.GetScale(ctx(), "calm-demo", "gone"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("404: %v", err)
	}
}

func TestKubeAPI_ReadyIsUnsettledUntilObservedGenerationCatchesUp(t *testing.T) {
	path := "GET /apis/apps/v1/namespaces/calm-demo/deployments/trades"
	f := &fakeAPIServer{resp: map[string]string{path: `{"metadata":{"generation":5},"spec":{"replicas":2},"status":{"observedGeneration":4,"readyReplicas":2}}`}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	k := &KubeAPI{C: envcapture.NewClient(srv.URL, "tok", srv.Client())}
	spec, ready, settled, err := k.Ready(ctx(), "calm-demo", "trades")
	if err != nil || spec != 2 || ready != 2 || settled {
		t.Fatalf("stale status read as settled: %d %d %v %v", spec, ready, settled, err)
	}
	f.mu.Lock()
	f.resp[path] = `{"metadata":{"generation":5},"spec":{"replicas":2},"status":{"observedGeneration":5}}`
	f.mu.Unlock()
	spec, ready, settled, _ = k.Ready(ctx(), "calm-demo", "trades")
	if spec != 2 || ready != 0 || !settled {
		t.Fatalf("absent readyReplicas must read 0: %d %d %v", spec, ready, settled)
	}
}

// fakeToxiproxy is an httptest Toxiproxy v2 API.
type fakeToxiproxy struct {
	mu      sync.Mutex
	reqs    []string
	bodies  map[string]string
	toxics  map[string]bool
	enabled bool
}

func (f *fakeToxiproxy) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		f.reqs = append(f.reqs, key)
		if f.bodies == nil {
			f.bodies = map[string]string{}
		}
		f.bodies[key] = string(b)
		switch {
		case r.Method == "POST" && r.URL.Path == "/proxies/trades/toxics":
			var t struct{ Name string }
			_ = json.Unmarshal(b, &t)
			if f.toxics[t.Name] {
				w.WriteHeader(409)
				return
			}
			if f.toxics == nil {
				f.toxics = map[string]bool{}
			}
			f.toxics[t.Name] = true
			_, _ = io.WriteString(w, `{}`)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/proxies/trades/toxics/"):
			name := strings.TrimPrefix(r.URL.Path, "/proxies/trades/toxics/")
			if !f.toxics[name] {
				w.WriteHeader(404)
				return
			}
			delete(f.toxics, name)
			w.WriteHeader(204)
		case r.Method == "GET" && r.URL.Path == "/proxies/trades":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "trades", "enabled": f.enabled})
		case r.Method == "POST" && r.URL.Path == "/proxies/trades":
			var p struct{ Enabled bool }
			_ = json.Unmarshal(b, &p)
			f.enabled = p.Enabled
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(404)
		}
	})
}

func TestToxiproxy_SpeaksTheV2API(t *testing.T) {
	f := &fakeToxiproxy{enabled: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	x := NewToxiproxy(srv.URL, srv.Client())
	err := x.AddToxic(ctx(), "trades", Toxic{Name: "argus-r-1", Type: "latency", Stream: "downstream", Attributes: map[string]int{"latency": 5000, "jitter": 100}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Name, Type, Stream string
		Toxicity           float64
		Attributes         map[string]int
	}
	_ = json.Unmarshal([]byte(f.bodies["POST /proxies/trades/toxics"]), &got)
	if got.Name != "argus-r-1" || got.Type != "latency" || got.Stream != "downstream" || got.Toxicity != 1 || got.Attributes["latency"] != 5000 {
		t.Fatalf("toxic body %+v", got)
	}
	if found, err := x.DeleteToxic(ctx(), "trades", "argus-r-1"); err != nil || !found {
		t.Fatalf("delete: %v %v", found, err)
	}
	if found, err := x.DeleteToxic(ctx(), "trades", "argus-r-1"); err != nil || found {
		t.Fatalf("a toxic already gone is not an error: %v %v", found, err)
	}
	on, err := x.ProxyEnabled(ctx(), "trades")
	if err != nil || !on {
		t.Fatal(on, err)
	}
	if err := x.SetEnabled(ctx(), "trades", false); err != nil || f.enabled {
		t.Fatal("proxy not disabled", err)
	}
}

func TestToxiproxy_TransportErrorNamesNoAddress(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	x := NewToxiproxy(url, &http.Client{Timeout: time.Second})
	_, err := x.ProxyEnabled(ctx(), "trades")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), url) {
		t.Fatalf("error carries the address: %v", err)
	}
}
