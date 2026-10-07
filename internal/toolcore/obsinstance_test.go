package toolcore

import (
	"strings"
	"testing"
)

// R4-2: the argus_instance telemetry label (obsInstance) is distinct from the tool identity (Instance).
// A direct run whose tool identity stays "local" must still emit + deep-link under the REAL instance id
// so direct + cloud runs share one Grafana instance drawer.
func TestObsInstance_DeepLinkUsesTelemetryLabel(t *testing.T) {
	if got := (Env{Instance: "local"}).obsInstance(); got != "local" {
		t.Errorf("obsInstance fallback = %q, want local", got)
	}
	if got := (Env{Instance: "local", ObsInstance: "orderservice-compose"}).obsInstance(); got != "orderservice-compose" {
		t.Errorf("obsInstance override = %q, want orderservice-compose", got)
	}
	u := RunDashboardURL(Env{Instance: "local", ObsInstance: "orderservice-compose", Grafana: "http://g:3000"}, "20260101T000001")
	if !strings.Contains(u, "var-argus_instance=orderservice-compose") {
		t.Errorf("deep-link must carry the obs instance label, not the tool identity, got %q", u)
	}
	if !strings.Contains(u, "var-current_run=20260101T000001") {
		t.Errorf("deep-link must scope to the run, got %q", u)
	}
	if u2 := RunDashboardURL(Env{Instance: "local", Grafana: "http://g:3000"}, "R2"); !strings.Contains(u2, "var-argus_instance=local") {
		t.Errorf("deep-link falls back to Instance when ObsInstance empty, got %q", u2)
	}
}
