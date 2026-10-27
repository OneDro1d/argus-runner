package argus

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// AC-D67 / #669: an mcp chain step printed its claims with the SAVED value bound in
// (`field email equals "anna@example.com"`), so the author's full report, which the control plane
// stores, carried a value the customer's system returned. The claim as WRITTEN is what a report shows
// (claimbind.go); the bound copy is for the judge only. http, amqp, db and logs steps already did this.
//
// The fake echoes each call's arguments back as the content text: step "write" answers with the
// e-mail it was sent and saves it; step "read" answers with whatever IT is sent.

const acd67Email = "anna@example.com"

func acd67Run(t *testing.T, readArgs, claims string) (status string, enforced []string, expected string) {
	t.Helper()
	ts := httptest.NewServer(fakeMCPTextServer("")) // echoes the args as the content text
	defer ts.Close()
	c := chainCfg(t, ts.URL)
	s := scenario.Parse(chainRunMD(ts.URL, `{"steps":[
  {"type":"mcp","name":"write","tool":"t","args":{"email":"`+acd67Email+`"},"save":{"email":"email"}},
  {"type":"mcp","name":"read","tool":"t","args":`+readArgs+`}
]}`, "### Runnable\n- step write: result.isError == false\n"+claims))
	r := runChainScenario(c, s, "tr-acd67", "testkit/ui")
	if len(r.Steps) != 2 {
		t.Fatalf("want 2 steps, got %+v", r.Steps)
	}
	if r.Failure != nil && r.Failure.Expected != nil {
		expected = *r.Failure.Expected
	}
	return r.Status, r.Steps[1].AssertionsEnforced, expected
}

func TestMCPChain_TextClaimsOnASavedValue_ShowThePlaceholder_OnAPass(t *testing.T) {
	claims := "- step read: body has email equals ${saved.email}\n" +
		"- step read: body contains ${saved.email}\n" +
		"- step read: body has email not equals someone-else@example.com\n"
	status, enforced, _ := acd67Run(t, `{"email":"${saved.email}"}`, claims)
	if status != "passed" {
		t.Fatalf("the read answer names the saved e-mail: must pass, got %s", status)
	}
	got := strings.Join(enforced, "; ")
	if strings.Contains(got, acd67Email) {
		t.Errorf("assertions_enforced prints the saved value: %q", got)
	}
	if strings.Count(got, "${saved.email}") != 2 {
		t.Errorf("assertions_enforced must show both saved claims as written, got %q", got)
	}
}

func TestMCPChain_TextClaimsOnASavedValue_ShowThePlaceholder_OnAContentFailure(t *testing.T) {
	// the read step is sent a DIFFERENT e-mail, so its claim on the saved one does not hold
	status, enforced, expected := acd67Run(t, `{"email":"other@example.org"}`,
		"- step read: body has email equals ${saved.email}\n")
	if status != "failed" {
		t.Fatalf("a different e-mail must fail the claim (the judge still binds), got %s", status)
	}
	for name, got := range map[string]string{"assertions_enforced": strings.Join(enforced, "; "), "failure.expected": expected} {
		if strings.Contains(got, acd67Email) {
			t.Errorf("%s prints the saved value: %q", name, got)
		}
		if !strings.Contains(got, "${saved.email}") {
			t.Errorf("%s must show the claim as written, got %q", name, got)
		}
	}
}
