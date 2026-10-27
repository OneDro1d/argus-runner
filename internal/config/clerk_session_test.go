package config

import (
	"strings"
	"testing"
)

// `type: clerk_session` on targets.auth and on targets.http_targets.<name>.auth.
// Every key and value below is an obvious fake; nothing here reaches Clerk.

const (
	fakeClerkKeyVal  = "sk_test_FAKEFAKEFAKEFAKE0001"
	fakeClerkUserVal = "user_FAKE0000000001"
)

func clerkEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CS_SECRET_KEY", fakeClerkKeyVal)
	t.Setenv("CS_USER_ID", fakeClerkUserVal)
}

const clerkDefaultYAML = `project:
  name: cs
targets:
  http:
    base_url: http://sut.lab.svc:8080
  auth:
    type: clerk_session
    secret_key_env: CS_SECRET_KEY
    user_id_env: CS_USER_ID
`

func loadErr(t *testing.T, body string) string {
	t.Helper()
	_, err := Load(writeCfg(t, body))
	if err == nil {
		t.Fatalf("expected a refusal, the config loaded:\n%s", body)
	}
	return err.Error()
}

func TestClerkSession_ValidShapeLoads(t *testing.T) {
	clerkEnv(t)
	body := clerkDefaultYAML + `    jwt_template: argus-test
    api_url: http://127.0.0.1:9999
`
	c, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("a valid clerk_session shape must load: %v", err)
	}
	a := c.Targets.Auth
	if a.Type != "clerk_session" || a.SecretKeyEnv != "CS_SECRET_KEY" || a.UserIDEnv != "CS_USER_ID" || a.JWTTemplate != "argus-test" || a.APIURL != "http://127.0.0.1:9999" {
		t.Fatalf("fields not parsed: %+v", a)
	}
	if got := c.ClerkSessionAuths(); len(got) != 1 || got[""] != a {
		t.Fatalf("ClerkSessionAuths = %v", got)
	}
}

func TestClerkSession_RefusesMissingRefs(t *testing.T) {
	clerkEnv(t)
	for name, tc := range map[string]struct{ yaml, want string }{
		"no secret ref": {`    type: clerk_session
    user_id_env: CS_USER_ID
`, "secret_key_env"},
		"no user ref": {`    type: clerk_session
    secret_key_env: CS_SECRET_KEY
`, "user_id_env"},
		"neither": {`    type: clerk_session
`, "secret_key_env"},
	} {
		t.Run(name, func(t *testing.T) {
			msg := loadErr(t, "project:\n  name: cs\ntargets:\n  http:\n    base_url: http://sut:8080\n  auth:\n"+tc.yaml)
			if !strings.Contains(msg, "clerk_session") || !strings.Contains(msg, tc.want) {
				t.Errorf("the refusal must name clerk_session and %q: %s", tc.want, msg)
			}
		})
	}
}

func TestClerkSession_RefsMustBeNamesNotValues(t *testing.T) {
	clerkEnv(t)
	body := strings.Replace(clerkDefaultYAML, "CS_SECRET_KEY", "sk_test_FAKE-not-a-name!", 1)
	msg := loadErr(t, body)
	if !strings.Contains(msg, "secret_key_env") || strings.Contains(msg, "sk_test_FAKE") {
		t.Errorf("must refuse a value where a NAME belongs, without echoing it: %s", msg)
	}
}

func TestClerkSession_UnsetEnvIsMissingByName(t *testing.T) {
	t.Setenv("CS_SECRET_KEY", fakeClerkKeyVal)
	// CS_USER_ID deliberately unset
	msg := loadErr(t, clerkDefaultYAML)
	if !strings.Contains(msg, "CS_USER_ID") || strings.Contains(msg, fakeClerkKeyVal) {
		t.Errorf("an unset reference is refused by NAME and never prints a value: %s", msg)
	}
}

func TestClerkSession_EnvRefsListTheNames(t *testing.T) {
	c, err := ParseUnresolved(writeCfg(t, clerkDefaultYAML))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range c.EnvRefs() {
		names = append(names, r.Name)
	}
	got := strings.Join(names, ",")
	if !strings.Contains(got, "CS_SECRET_KEY") || !strings.Contains(got, "CS_USER_ID") {
		t.Errorf("secrets-scan / preflight must see both names: %v", names)
	}
}

func TestClerkSession_StrictKeysAcceptedAndUnknownRefused(t *testing.T) {
	clerkEnv(t)
	named := clerkDefaultYAML + `  http_targets:
    admin:
      base_url: http://admin.lab.svc:8080
      auth:
        type: clerk_session
        secret_key_env: CS_SECRET_KEY
        user_id_env: CS_USER_ID
        jwt_template: t
        api_url: http://127.0.0.1:1
`
	if _, err := Load(writeCfg(t, named)); err != nil {
		t.Fatalf("the new keys must be accepted under targets.auth and a named target's auth: %v", err)
	}
	// an unknown key beside them is refused BY NAME with the accepted list, in both places
	for name, body := range map[string]string{
		"default":     clerkDefaultYAML + "    secret_key: x\n",
		"named":       strings.Replace(named, "        jwt_template: t\n", "        jwt_templat: t\n", 1),
		"http.auth":   "project:\n  name: cs\ntargets:\n  http_targets:\n    a:\n      base_url: http://a:1\n      auth:\n        type: bearer\n        bearer_tokn: x\n",
		"bad targets": clerkDefaultYAML + "  clerk:\n    x: 1\n",
	} {
		msg := loadErr(t, body)
		if !strings.Contains(msg, "unknown key") && !strings.Contains(msg, "not found") {
			t.Errorf("%s: want an unknown-key refusal, got: %s", name, msg)
		}
	}
	msg := loadErr(t, clerkDefaultYAML+"    secret_key: x\n")
	for _, want := range []string{`"secret_key"`, "secret_key_env", "user_id_env", "jwt_template", "api_url", "allow_clerk_session_on_prod"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must list the accepted keys; missing %q in: %s", want, msg)
		}
	}
}

func TestClerkSession_PerTargetAuthWinsOverDefault(t *testing.T) {
	clerkEnv(t)
	t.Setenv("CS_OTHER_USER", "user_FAKE0000000002")
	body := clerkDefaultYAML + `  http_targets:
    admin:
      base_url: http://admin.lab.svc:8080
      auth:
        type: clerk_session
        secret_key_env: CS_SECRET_KEY
        user_id_env: CS_OTHER_USER
    plain:
      base_url: http://plain.lab.svc:8080
`
	c, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if k, a := c.HTTPAuthFor(&TargetSelection{Kind: KindHTTP, Name: "admin", HTTP: c.Targets.HTTPTargets["admin"]}); k != "admin" || a.UserIDEnv != "CS_OTHER_USER" {
		t.Errorf("a named target's own auth must win: key=%q auth=%+v", k, a)
	}
	if k, a := c.HTTPAuthFor(&TargetSelection{Kind: KindHTTP, Name: "plain", HTTP: c.Targets.HTTPTargets["plain"]}); k != "" || a != c.Targets.Auth {
		t.Errorf("a named target with no auth falls back to the default: key=%q", k)
	}
	if k, a := c.HTTPAuthFor(nil); k != "" || a != c.Targets.Auth {
		t.Errorf("no selection = the default: key=%q", k)
	}
	if got := c.ClerkSessionAuths(); len(got) != 2 || got["admin"] == nil || got[""] == nil {
		t.Errorf("ClerkSessionAuths = %v", got)
	}
}

func TestClerkSession_PerTargetBearerAlsoWorks(t *testing.T) {
	t.Setenv("CS_TOK", "tok-FAKE-1")
	body := `project:
  name: cs
targets:
  http_targets:
    a:
      base_url: http://a:1
      auth:
        type: bearer
        bearer_token: ${CS_TOK}
`
	c, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if c.Targets.HTTPTargets["a"].Auth.BearerToken != "tok-FAKE-1" {
		t.Error("a per-target bearer_token must be ${VAR}-expanded like targets.auth's")
	}
}

func TestClerkSession_ProdRefusedUnlessOptedIn(t *testing.T) {
	clerkEnv(t)
	prodDefault := strings.Replace(clerkDefaultYAML, "http://sut.lab.svc:8080", "https://api.prod.example.com", 1)
	msg := loadErr(t, prodDefault)
	for _, want := range []string{"clerk_session", "production", "allow_clerk_session_on_prod", "targets.auth"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the prod refusal must name %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "prod.example.com") {
		t.Errorf("a base_url can carry credentials; the refusal must not echo it: %s", msg)
	}
	if _, err := Load(writeCfg(t, prodDefault+"    allow_clerk_session_on_prod: true\n")); err != nil {
		t.Fatalf("the explicit opt-in must let a prod-looking instance load: %v", err)
	}
	// a prod-looking NAMED target taints the default (it is the default for every scenario)
	taint := clerkDefaultYAML + "  http_targets:\n    live:\n      base_url: https://shared.example.com\n"
	if msg := loadErr(t, taint); !strings.Contains(msg, "allow_clerk_session_on_prod") {
		t.Errorf("default auth must be refused when ANY http target looks production: %s", msg)
	}
	// a per-target clerk auth only looks at ITS base_url
	own := "project:\n  name: cs\ntargets:\n  http_targets:\n    ok:\n      base_url: http://ok.lab.svc:1\n      auth:\n        type: clerk_session\n        secret_key_env: CS_SECRET_KEY\n        user_id_env: CS_USER_ID\n    live:\n      base_url: https://live.prod.example.com\n"
	if _, err := Load(writeCfg(t, own)); err != nil {
		t.Fatalf("a per-target auth must judge only its own base_url: %v", err)
	}
	liveOwn := own +
		"      auth:\n        type: clerk_session\n        secret_key_env: CS_SECRET_KEY\n        user_id_env: CS_USER_ID\n"
	if msg := loadErr(t, liveOwn); !strings.Contains(msg, "targets.http_targets.live.auth") {
		t.Errorf("the refusal must name the offending auth block: %s", msg)
	}
}

func TestClerkSession_LiveKeyIsAProdSignal(t *testing.T) {
	t.Setenv("CS_SECRET_KEY", "sk_live_FAKEFAKEFAKEFAKE0001")
	t.Setenv("CS_USER_ID", fakeClerkUserVal)
	msg := loadErr(t, clerkDefaultYAML)
	if !strings.Contains(msg, "allow_clerk_session_on_prod") || !strings.Contains(msg, "sk_live_") {
		t.Errorf("a LIVE Clerk key is a prod signal: %s", msg)
	}
	if strings.Contains(msg, "FAKEFAKE") {
		t.Errorf("the refusal must not echo the key: %s", msg)
	}
	if _, err := Load(writeCfg(t, clerkDefaultYAML+"    allow_clerk_session_on_prod: true\n")); err != nil {
		t.Fatalf("opt-in must allow a live key: %v", err)
	}
}

func TestClerkSession_FieldsWithoutTheTypeAreRefused(t *testing.T) {
	clerkEnv(t)
	for name, auth := range map[string]string{
		"bearer + secret ref": "    type: bearer\n    bearer_token: x\n    secret_key_env: CS_SECRET_KEY\n",
		"no type + opt-in":    "    allow_clerk_session_on_prod: true\n",
		"clerk + bearer":      "    type: clerk_session\n    secret_key_env: CS_SECRET_KEY\n    user_id_env: CS_USER_ID\n    bearer_token: x\n",
		"bad api_url":         "    type: clerk_session\n    secret_key_env: CS_SECRET_KEY\n    user_id_env: CS_USER_ID\n    api_url: ftp://x\n",
		"api_url userinfo":    "    type: clerk_session\n    secret_key_env: CS_SECRET_KEY\n    user_id_env: CS_USER_ID\n    api_url: https://u:p@x\n",
		"bad template":        "    type: clerk_session\n    secret_key_env: CS_SECRET_KEY\n    user_id_env: CS_USER_ID\n    jwt_template: a/b\n",
	} {
		t.Run(name, func(t *testing.T) {
			msg := loadErr(t, "project:\n  name: cs\ntargets:\n  http:\n    base_url: http://sut:8080\n  auth:\n"+auth)
			if !strings.Contains(msg, "targets.auth") {
				t.Errorf("must name the block: %s", msg)
			}
			if strings.Contains(msg, "u:p") {
				t.Errorf("must not echo a URL's userinfo: %s", msg)
			}
		})
	}
}

func TestClerkSession_PlainHTTPSlotMayNotCarryAuth(t *testing.T) {
	clerkEnv(t)
	body := "project:\n  name: cs\ntargets:\n  http:\n    base_url: http://sut:8080\n    auth:\n      type: bearer\n      bearer_token: x\n"
	if msg := loadErr(t, body); !strings.Contains(msg, "targets.auth") {
		t.Errorf("the plain slot's credentials live in targets.auth; a second place is two answers: %s", msg)
	}
}

func TestClerkSession_SecretKeyValueIsACredentialValue(t *testing.T) {
	clerkEnv(t)
	c, err := Load(writeCfg(t, clerkDefaultYAML))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range c.CredentialValues() {
		if v == fakeClerkKeyVal {
			found = true
		}
		if v == fakeClerkUserVal {
			t.Error("a user id is not a secret and a scrubber must not be handed it")
		}
	}
	if !found {
		t.Error("CredentialValues must carry the Clerk secret key so recorded output is scrubbed of it")
	}
}

func TestClerkSession_TypeIsAcceptedByTheTypeCheckForHTTPAuthOnly(t *testing.T) {
	if e := targetTypeError("targets.auth", "clerk_session", httpAuthTypesSupported); e != nil {
		t.Errorf("clerk_session is served on targets.auth: %s", e.Message)
	}
	if e := targetTypeError("targets.mcp.auth", "clerk_session", authTypesSupported); e == nil {
		t.Error("MCP auth has no clerk_session; it must keep refusing it")
	}
}
