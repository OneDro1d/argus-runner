package config

import (
	"sort"
	"strings"
	"testing"
)

// ARGUS-CMP-3: CredentialValues is the resolved-secret list a recorded output is scrubbed with.
func TestCredentialValues_ReturnsSecretsAndNotOrdinaryFields(t *testing.T) {
	c := &Config{}
	c.Targets.HTTP = &HTTPTarget{BaseURL: "http://sut:8080"}
	c.Targets.Auth = &AuthTarget{BearerToken: "bearer-xyz"}
	c.Targets.Database = &DBTarget{Username: "sa", Password: "pw-1", JDBCURL: "jdbc:postgresql://h/db?user=sa&password=pw-2"}
	c.Targets.MessageBroker = &MQTarget{URL: "amqp://guest:pw-3@broker:5672/"}
	got := c.CredentialValues()
	sort.Strings(got)
	joined := strings.Join(got, "|")
	for _, want := range []string{"bearer-xyz", "pw-1", "pw-2", "pw-3", "guest:pw-3", "jdbc:postgresql://h/db?user=sa&password=pw-2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	for _, g := range got {
		if g == "sa" || g == "http://sut:8080" || g == "" {
			t.Errorf("returned a non-secret or empty value %q", g)
		}
	}
	var none *Config
	if none.CredentialValues() != nil {
		t.Error("nil config must give nil")
	}
}
