package k8srender

import "testing"

// VR10-S3-10 / acceptance 6 + 11 (V28-012): every NAMED entry's URL host is aliased exactly like the
// plain slot's — otherwise a named target resolves on compose (the runner joins the SUT network) and
// fails on k3d/AKS, where only an ExternalName Service makes a bare compose host resolve. The PO
// flagged this as the failure a compose-only test would miss.
func TestDeriveAliases_namedTargetsAreAliasedLikeThePlainSlot(t *testing.T) {
	c := cfgFrom(t, `
project: {name: memstore}
targets:
  http: {base_url: "http://memstore-gateway:8090"}
  http_targets:
    graph: {base_url: "http://memstore-graph:8096"}
  mcp_targets:
    admin: {base_url: "http://memstore-admin:8091/mcp"}
  database_targets:
    neo4j: {jdbc_url: "jdbc:neo4j://memstore-neo4j:7687"}
  message_broker_targets:
    audit:
      url: "amqp://a:b@audit-rabbit:5672/"
      management_url: "http://audit-mgmt:15672"
`)
	got := DeriveAliases(c, "memstore-k3d")
	want := map[string]string{
		"memstore-gateway": "memstore-gateway.memstore-k3d.svc.cluster.local",
		"memstore-graph":   "memstore-graph.memstore-k3d.svc.cluster.local",
		"memstore-admin":   "memstore-admin.memstore-k3d.svc.cluster.local",
		"memstore-neo4j":   "memstore-neo4j.memstore-k3d.svc.cluster.local",
		"audit-rabbit":     "audit-rabbit.memstore-k3d.svc.cluster.local",
		"audit-mgmt":       "audit-mgmt.memstore-k3d.svc.cluster.local",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("alias %q = %q, want %q (a named target's bare host must resolve in-cluster like the plain slot's)", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("derived %d aliases, want %d: %v", len(got), len(want), got)
	}
}
