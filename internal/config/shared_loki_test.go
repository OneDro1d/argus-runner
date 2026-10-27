package config

import (
	"strings"
	"testing"
)

// Observability.loki.shared_cluster.

const sharedBase = "project:\n  name: t\ntargets:\n  http:\n    base_url: http://sut:8080\n"

func TestSharedLokiCluster_ParsesAndDefaultsEmpty(t *testing.T) {
	c, err := loadCfgText(t, sharedBase+"observability:\n  loki:\n    url: http://loki.observability.svc:3100\n    shared_cluster: cluster-a\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.SharedLokiCluster() != "cluster-a" {
		t.Errorf("SharedLokiCluster() = %q", c.SharedLokiCluster())
	}
	if w := c.SharedLokiWarnings(); len(w) != 2 || !strings.Contains(w[0], "readline_rate") || !strings.Contains(w[1], "batchwait") {
		t.Errorf("the two shared-promtail limits must be named as warnings, got %v", w)
	}
	// every existing config
	c, err = loadCfgText(t, sharedBase+"observability:\n  loki:\n    url: http://loki:3100\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.SharedLokiCluster() != "" || c.SharedLokiWarnings() != nil {
		t.Errorf("an unset shared_cluster must say nothing: %q %v", c.SharedLokiCluster(), c.SharedLokiWarnings())
	}
}

func TestSharedLokiCluster_Refusals(t *testing.T) {
	cases := []struct{ name, block, want string }{
		{"needs the url it describes", "observability:\n  loki:\n    shared_cluster: cluster-a\n", "observability.loki.url is not"},
		{"a quote cannot reach a selector", "observability:\n  loki:\n    url: http://l\n    shared_cluster: 'po\"land'\n", "not a cluster label value"},
		{"a brace cannot either", "observability:\n  loki:\n    url: http://l\n    shared_cluster: 'a}b'\n", "not a cluster label value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadCfgText(t, sharedBase+c.block)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}
