package k8srender

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
)

func parseCfgWH(t *testing.T, yaml string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseUnresolved(p)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return c
}

func TestWebhookURLFor_mapForm_FQDNRewrite(t *testing.T) {
	c := parseCfgWH(t, "project: {name: order-service}\ntargets:\n  external:\n    webhook_base_url: http://webhook-mock:8081\n")
	got := WebhookURLFor(c, "order-k3d")
	want := "http://webhook-mock.order-k3d.svc.cluster.local:8081"
	if got != want {
		t.Errorf("WebhookURLFor = %q, want %q", got, want)
	}
}

func TestWebhookURLFor_alreadyFQDN_untouched(t *testing.T) {
	c := parseCfgWH(t, "project: {name: s}\ntargets:\n  external:\n    webhook_base_url: http://sink.example.com/hook\n")
	if got := WebhookURLFor(c, "order-k3d"); got != "http://sink.example.com/hook" {
		t.Errorf("an FQDN host must be left untouched, got %q", got)
	}
}

// The LIST form (OrderService's codebase config) has no webhook_base_url — WebhookURLFor returns ""
// and does NOT error. This is the two-shapes safety the untyped block preserves.
func TestWebhookURLFor_listForm_empty(t *testing.T) {
	c := parseCfgWH(t, "project: {name: s}\ntargets:\n  external:\n    - name: webhook-mock\n      verify_url: http://webhook-mock:8081/received\n")
	if got := WebhookURLFor(c, "order-k3d"); got != "" {
		t.Errorf("list-form external has no webhook_base_url; want \"\", got %q", got)
	}
}

func TestWebhookURLFor_absent_empty(t *testing.T) {
	c := parseCfgWH(t, "project: {name: s}\n")
	if got := WebhookURLFor(c, "order-k3d"); got != "" {
		t.Errorf("no external block; want \"\", got %q", got)
	}
}

func TestRenderExecutor_injectsWebhookURLWhenSet(t *testing.T) {
	out, err := RenderExecutor(Instance{
		ID: "ws-probe", SUTNamespace: "order-k3d", Image: "img:t", CPURL: "https://cp",
		WorkspaceID: "ws", SUTName: "order-service", Tier: "k3d", Replicas: 1,
		ArgusConfig: "project:\n  name: order-service\n",
		WebhookURL:  "http://webhook-mock.order-k3d.svc.cluster.local:8081",
	})
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	if !strings.Contains(out, "- name: WEBHOOK_URL") ||
		!strings.Contains(out, `value: "http://webhook-mock.order-k3d.svc.cluster.local:8081"`) {
		t.Errorf("executor should carry the WEBHOOK_URL env; got:\n%s", out)
	}
}

func TestRenderExecutor_noWebhookEnvWhenUnset(t *testing.T) {
	out, err := RenderExecutor(Instance{
		ID: "ws-probe", SUTNamespace: "order-k3d", Image: "img:t", CPURL: "https://cp",
		WorkspaceID: "ws", SUTName: "order-service", Tier: "k3d", Replicas: 1,
		ArgusConfig: "project:\n  name: order-service\n",
	})
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	if strings.Contains(out, "WEBHOOK_URL") {
		t.Errorf("no WEBHOOK_URL env should appear when WebhookURL is empty")
	}
}
