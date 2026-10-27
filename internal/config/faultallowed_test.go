package config

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// a fault step runs only against a deployment or link the OPERATOR listed.

const faultBase = "project:\n  name: p\ntargets:\n  http:\n    base_url: http://sut.invalid\n"

const faultBlock = `fault_allowed_targets:
  deployments:
    trades: {namespace: calm-demo}
  links:
    mcp-to-trades: {toxiproxy_api: "http://toxiproxy.calm-demo:8474", proxy: trades}
`

func TestFaultAllowed_AbsentMeansNoFaultMayRun(t *testing.T) {
	c, err := loadFrom(t, faultBase)
	if err != nil {
		t.Fatal(err)
	}
	if c.FaultEnabled() {
		t.Fatal("fault steps are ON with no fault_allowed_targets block: the default must be OFF")
	}
	if _, ok := c.FaultDeployment("trades"); ok {
		t.Fatal("a deployment resolved with no block")
	}
	if _, _, ok := c.FaultLink("mcp-to-trades"); ok {
		t.Fatal("a link resolved with no block")
	}
}

func TestFaultAllowed_ListedResolves_UnlistedDoesNot(t *testing.T) {
	c, err := loadFrom(t, faultBase+faultBlock)
	if err != nil {
		t.Fatal(err)
	}
	if ns, ok := c.FaultDeployment("trades"); !ok || ns != "calm-demo" {
		t.Fatalf("trades -> %q %v", ns, ok)
	}
	if api, proxy, ok := c.FaultLink("mcp-to-trades"); !ok || proxy != "trades" || api != "http://toxiproxy.calm-demo:8474" {
		t.Fatalf("link -> %q %q %v", api, proxy, ok)
	}
	if _, ok := c.FaultDeployment("payments"); ok {
		t.Fatal("an unlisted deployment resolved")
	}
	if _, _, ok := c.FaultLink("other"); ok {
		t.Fatal("an unlisted link resolved")
	}
}

func TestFaultAllowed_RefusesProductionLookingNamespaceAndURL(t *testing.T) {
	cases := map[string]string{
		"namespace prod":       "fault_allowed_targets:\n  deployments:\n    api: {namespace: prod}\n",
		"namespace production": "fault_allowed_targets:\n  deployments:\n    api: {namespace: shop-production}\n",
		"namespace shared":     "fault_allowed_targets:\n  deployments:\n    api: {namespace: shared}\n",
		"toxiproxy prod url":   "fault_allowed_targets:\n  links:\n    l: {toxiproxy_api: \"http://toxiproxy.prod.internal:8474\", proxy: p}\n",
	}
	for name, body := range cases {
		_, err := loadFrom(t, faultBase+body)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "toxiproxy.prod") || strings.Contains(err.Error(), "8474") {
			t.Errorf("%s: the refusal echoes the URL: %v", name, err)
		}
		if !strings.Contains(err.Error(), "fault_allowed_targets.") {
			t.Errorf("%s: the refusal does not name the entry: %v", name, err)
		}
	}
}

func TestFaultAllowed_RefusesMalformedEntries(t *testing.T) {
	for name, body := range map[string]string{
		"no namespace":        "fault_allowed_targets:\n  deployments:\n    api: {}\n",
		"bad deployment name": "fault_allowed_targets:\n  deployments:\n    'Bad Name': {namespace: lab}\n",
		"bad namespace":       "fault_allowed_targets:\n  deployments:\n    api: {namespace: 'Lab_1'}\n",
		"link no proxy":       "fault_allowed_targets:\n  links:\n    l: {toxiproxy_api: 'http://t:8474'}\n",
		"link no api":         "fault_allowed_targets:\n  links:\n    l: {proxy: p}\n",
		"api not http":        "fault_allowed_targets:\n  links:\n    l: {toxiproxy_api: 'ftp://t:8474', proxy: p}\n",
		"api userinfo":        "fault_allowed_targets:\n  links:\n    l: {toxiproxy_api: 'http://u:pw@t:8474', proxy: p}\n",
		"unknown key":         "fault_allowed_targets:\n  deployments:\n    api: {namespace: lab, replicas: 3}\n",
	} {
		if _, err := loadFrom(t, faultBase+body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFaultAllowed_OldExecutorDecodesIgnoring(t *testing.T) {
	dec := yaml.NewDecoder(bytes.NewReader([]byte(faultBase + faultBlock)))
	dec.KnownFields(true)
	var d oldStrictTargetsDoc
	if err := dec.Decode(&d); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("an old executor refused the new top-level key (a rollback would take it down): %v", err)
	}
	if _, ok := d.Rest["fault_allowed_targets"]; !ok {
		t.Error("the key did not land in the old executor's lenient inline map")
	}
}
