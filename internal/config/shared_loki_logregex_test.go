package config

import (
	"strings"
	"testing"
)

// observability.loki.log_regex: a PLAIN-TEXT line format, read at query time in shared-Loki mode only.

const sharedLokiBlock = "observability:\n  loki:\n    url: http://loki.observability.svc:3100\n    shared_cluster: cluster-a\n"

func TestLogRegex_ParsesUnderASharedLoki(t *testing.T) {
	c, err := loadCfgText(t, sharedBase+sharedLokiBlock+`    log_regex: '^(?P<time>\S+) (?P<service>\S+) (?P<level>[A-Z]+): '`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.SharedLokiLogRegex(), `^(?P<time>\S+) (?P<service>\S+) (?P<level>[A-Z]+): `; got != want {
		t.Fatalf("SharedLokiLogRegex() = %q, want %q", got, want)
	}
	c, err = loadCfgText(t, sharedBase+sharedLokiBlock)
	if err != nil || c.SharedLokiLogRegex() != "" {
		t.Fatalf("unset = %q %v", c.SharedLokiLogRegex(), err)
	}
}

func TestLogRegex_Refusals(t *testing.T) {
	cases := []struct{ name, block, want string }{
		{"not without a shared Loki: the ingest pipelines would ignore it", "observability:\n  loki:\n    url: http://l\n    log_regex: '(?P<level>[A-Z]+)'\n", "only read in shared-Loki mode"},
		{"not with logfmt", sharedLokiBlock + "    log_format: logfmt\n    log_regex: '(?P<level>[A-Z]+)'\n", "give one"},
		{"must be RE2", sharedLokiBlock + "    log_regex: '(?P<level>[A-Z'\n", "valid RE2"},
		{"needs a named group", sharedLokiBlock + "    log_regex: '([A-Z]+): '\n", "named group"},
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
