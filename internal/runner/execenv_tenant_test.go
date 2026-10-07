package runner

import "testing"

// T3.3: a federated run is the production path of every k8s executor, and it reaches Loki only through
// the Env execEnv builds. Under --obs=shared the Loki runs auth_enabled: true, so an Env without the
// tenant is refused on every query and every push. The run still completes, but its saga and logs
// come back "unavailable" and nothing says why. Found by mutation: dropping this field turned nothing
// red.
func TestExecEnv_ThreadsTheLokiTenant(t *testing.T) {
	env := execEnv(ExecConfig{InstanceID: "memstore-k3d", Loki: "http://loki.argus-obs.svc.cluster.local:3100", LokiTenant: "memstore-k3d"}, "/results/materialized/r1")
	if env.LokiTenant != "memstore-k3d" {
		t.Errorf("execEnv must carry ExecConfig.LokiTenant into toolcore.Env, got %q", env.LokiTenant)
	}
	if got := execEnv(ExecConfig{InstanceID: "x"}, "/r").LokiTenant; got != "" {
		t.Errorf("no tenant configured must stay no tenant (bundled/adopt/export), got %q", got)
	}
}
