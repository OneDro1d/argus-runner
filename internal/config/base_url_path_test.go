package config

import (
	"strings"
	"testing"
)

// — an http target's base_url path is DROPPED: the runner keeps only host and port
// (HTTPTarget.HostPort) and takes the path from each scenario's trigger URL (PathFromURL). A config
// written `base_url: http://documenso:3100/api/v2` therefore sent every request to
// `http://documenso:3100/<scenario path>` — a 404 on the first run — while validate-config said
// "valid, no warnings". The warning names the path and the cure; request building is NOT changed.
func TestBaseURLPathWarnings_NamesTheDroppedPathAndTheCure(t *testing.T) {
	c := loadYAML(t, "project:\n  name: p\ntargets:\n  http:\n    base_url: http://documenso:3100/api/v2\n")
	w := c.BaseURLPathWarnings()
	if len(w) != 1 {
		t.Fatalf("want one warning for the dropped /api/v2, got %q", w)
	}
	for _, want := range []string{"targets.http.base_url", "/api/v2", "${INGESTION_URL}/api/v2/", "trigger"} {
		if !strings.Contains(w[0], want) {
			t.Errorf("the warning must carry %q so the operator can act on it: %q", want, w[0])
		}
	}
}

func TestBaseURLPathWarnings_NamesANamedTargetToo(t *testing.T) {
	c := loadYAML(t, "project:\n  name: p\ntargets:\n  http:\n    base_url: http://a:8080\n"+
		"  http_targets:\n    utils:\n      base_url: https://utils.example/v1/\n")
	w := c.BaseURLPathWarnings()
	if len(w) != 1 || !strings.Contains(w[0], "targets.http_targets.utils.base_url") || !strings.Contains(w[0], "/v1/") {
		t.Fatalf("want one warning naming the utils target and its /v1/ path; got %q", w)
	}
}

func TestBaseURLPathWarnings_QuietForAPathlessBase(t *testing.T) {
	for _, base := range []string{"http://x:8080", "http://x:8080/", "https://x.example"} {
		body := "project:\n  name: p\ntargets:\n  http:\n    base_url: " + base + "\n"
		if w := loadYAML(t, body).BaseURLPathWarnings(); len(w) != 0 {
			t.Errorf("base_url %q has no path to drop, but warned: %q", base, w)
		}
	}
}

// The warning is advisory and must not move a request: HostPort and PathFromURL are untouched.
func TestBaseURLPathWarnings_DoesNotChangeRequestBuilding(t *testing.T) {
	c := loadYAML(t, "project:\n  name: p\ntargets:\n  http:\n    base_url: http://documenso:3100/api/v2\n")
	if p, h, port := c.Targets.HTTP.HostPort(); p != "http" || h != "documenso" || port != "3100" {
		t.Errorf("HostPort = %s %s %s; the warning must not alter how a request is addressed", p, h, port)
	}
	if got := PathFromURL("${INGESTION_URL}/document"); got != "/document" {
		t.Errorf("PathFromURL = %q; the base_url path must still NOT be prepended", got)
	}
}
