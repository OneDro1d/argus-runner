package router

import (
	"encoding/json"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcpserver"
)

// the control plane's owner-wide builder token is a Product-hat principal with a Subject and
// no workspace, and this server builds exactly that shape for a product folder (auth.Authenticate: Hat from the
// folder, Subject = the router token). The builder namespace must stay CLOSED to it: a router principal never
// carries the OwnerWide marker, only the control plane's PAT authenticator sets it. Driven through the REAL
// mcpserver filter, with a stand-in NSBuilder tool.
func TestRouterPrincipal_NeverSeesTheBuilderNamespace(t *testing.T) {
	tbl := testTable(t)
	stub := mcpserver.Tool{
		Name: "runner__stub", Namespace: mcpserver.NSBuilder, Description: "d",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		Handler: func(json.RawMessage, mcpserver.Principal) mcpserver.Outcome {
			return mcpserver.Ok(map[string]any{"ok": true})
		},
	}
	srv, err := mcpserver.NewServerWithAuth(Authenticator(tbl), append(Tools(tbl, &fakeFwd{}), stub)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{prodTok, testTok} {
		prin, err := Authenticator(tbl).Authenticate(tok)
		if err != nil {
			t.Fatal(err)
		}
		if prin.Subject == "" || prin.Workspace != "" || prin.OwnerWide {
			t.Fatalf("premise: the router's principal is %+v, want a Subject, no workspace, no OwnerWide marker", prin)
		}
		for _, tl := range srv.VisibleToolsForPrincipal(prin) {
			if tl.Name == "runner__stub" {
				t.Errorf("the router principal for %s sees the builder namespace", tok)
			}
		}
	}
}
