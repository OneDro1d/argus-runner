package doctor

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/role"
)

var enc = base64.RawURLEncoding.EncodeToString

// jwt builds an unsigned RS256-headed token with the given claims. The signature is garbage on
// purpose: doctor reads claims, it does not verify them, and the tests must not suggest otherwise.
func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "test"})
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return enc(hdr) + "." + enc(body) + "." + enc([]byte("not-a-real-signature"))
}

func TestClassify(t *testing.T) {
	exp := time.Date(2026, 9, 25, 17, 15, 0, 0, time.UTC)
	hdr := enc([]byte(`{"alg":"RS256","typ":"JWT"}`))
	cases := []struct {
		name      string
		tok       string
		kind      TokenKind
		scope     string
		hasExp    bool
		malformed bool
	}{
		{"empty", "", KindNone, "", false, false},
		{"author PAT", "odts_anything-at-all", KindAuthorPAT, "", false, false},
		{"control-plane access token", jwt(t, map[string]any{"iss": "https://argus-dev.onedroid.ai", "scope": "author", "exp": exp.Unix()}), KindJWT, "author", true, false},
		{"runner token without exp", jwt(t, map[string]any{"scope": "runner"}), KindJWT, "runner", false, false},
		{"padded base64 still decodes", hdr + "." + base64.URLEncoding.EncodeToString([]byte(`{"scope":"author"}`)) + ".sig", KindJWT, "author", false, false},
		{"JWT with unreadable claims", hdr + ".!!!not-base64!!!.sig", KindJWT, "", false, true},
		{"JWT whose claims are not an object", hdr + "." + enc([]byte(`"just a string"`)) + ".sig", KindJWT, "", false, true},
		{"three segments but no JOSE header", "a.b.c", KindUnrecognised, "", false, false},
		{"a workspace id pasted as a token", "wksp_01J8ZK", KindUnrecognised, "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := Classify(tc.tok)
			if f.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q", f.Kind, tc.kind)
			}
			if f.Scope != tc.scope {
				t.Errorf("scope = %q, want %q", f.Scope, tc.scope)
			}
			if f.HasExpiry != tc.hasExp {
				t.Errorf("HasExpiry = %v, want %v", f.HasExpiry, tc.hasExp)
			}
			if tc.hasExp && !f.ExpiresAt.Equal(exp) {
				t.Errorf("ExpiresAt = %v, want %v", f.ExpiresAt, exp)
			}
			if (f.Malformed != "") != tc.malformed {
				t.Errorf("Malformed = %q, want malformed=%v", f.Malformed, tc.malformed)
			}
		})
	}
}

// ⛔ The one property every other test rests on: nothing of the token comes back out.
func TestClassifyNeverEchoesTheToken(t *testing.T) {
	marker := "SECRET-MARKER-9f3a"
	tok := jwt(t, map[string]any{"sub": marker, "scope": "author", "exp": time.Now().Add(time.Hour).Unix()})
	blob, _ := json.Marshal(Classify(tok))
	if strings.Contains(string(blob), marker) {
		t.Fatalf("a claim that identifies the holder came back in the facts: %s", blob)
	}
	for _, seg := range strings.Split(tok, ".") {
		if strings.Contains(string(blob), seg) {
			t.Fatalf("a token segment came back in the facts: %s", blob)
		}
	}
}

func TestSummarize(t *testing.T) {
	ok := Check{ID: "a", Status: StatusOK}
	warn := Check{ID: "b", Status: StatusWarn}
	unknown := Check{ID: "c", Status: StatusUnknown}
	fail := Check{ID: "d", Status: StatusFail}

	t.Run("no checks is ok, and checks renders as [] not null", func(t *testing.T) {
		rep := Summarize(nil)
		if rep.Verdict != VerdictOK {
			t.Fatalf("verdict = %q", rep.Verdict)
		}
		blob, _ := json.Marshal(rep)
		if !strings.Contains(string(blob), `"checks":[]`) {
			t.Fatalf("checks did not render as an empty list: %s", blob)
		}
	})
	t.Run("a warn is a warn", func(t *testing.T) {
		rep := Summarize([]Check{ok, warn})
		if rep.Verdict != VerdictWarn || strings.Join(rep.Warning, ",") != "b" || len(rep.Failing) != 0 {
			t.Fatalf("got %+v", rep)
		}
	})
	// THE RULE. Not knowing is not a reason to proceed.
	t.Run("an unknown fails the verdict", func(t *testing.T) {
		rep := Summarize([]Check{ok, warn, unknown})
		if rep.Verdict != VerdictFail || strings.Join(rep.Failing, ",") != "c" {
			t.Fatalf("an unknown check did not fail the verdict: %+v", rep)
		}
	})
	t.Run("failing ids keep report order", func(t *testing.T) {
		rep := Summarize([]Check{fail, unknown, warn})
		if strings.Join(rep.Failing, ",") != "d,c" || strings.Join(rep.Warning, ",") != "b" {
			t.Fatalf("got %+v", rep)
		}
	})
}

func TestCheckCredential(t *testing.T) {
	now := time.Date(2026, 9, 25, 17, 0, 0, 0, time.UTC)
	const cp = "https://argus-dev.onedroid.ai"
	at := func(d time.Duration, scope string) TokenFacts {
		return Classify(jwt(t, map[string]any{"iss": cp, "scope": scope, "exp": now.Add(d).Unix()}))
	}
	fresh, stale, soon := at(10*time.Minute, "author"), at(-3*time.Hour, "author"), at(2*time.Minute, "author")
	env := func(f TokenFacts) CredentialInput {
		return CredentialInput{Source: envname.CPAuthorToken, Present: true, Facts: f, ControlPlaneURL: cp, Now: now}
	}
	session := func(f TokenFacts, refresh bool, obtained time.Time) CredentialInput {
		return CredentialInput{Source: "/home/x/.config/argus/session.json", Present: true, Facts: f, FromSession: true,
			RefreshPresent: refresh, ObtainedAt: obtained, ControlPlaneURL: cp, Now: now}
	}

	cases := []struct {
		name   string
		in     CredentialInput
		want   Status
		detail string // must appear in Detail
		fix    string // must appear in Fix ("" = don't care)
	}{
		{"absent", CredentialInput{Source: envname.CPAuthorToken + " and the session file /x", ControlPlaneURL: cp, Now: now},
			StatusFail, "no control-plane credential", "cloud-login --control-plane " + cp + " --scope author"},
		// An odts_ value is an author PAT OR, since #259, a builder token: the prefix is shared (store/tokens.go).
		{"odts_ PAT", env(Classify("odts_x")), StatusOK, "builder token", ""},
		{"fresh bare token says it will not renew", env(fresh), StatusOK, "will not renew", ""},
		// THE TRAP — 38 of 169 recorded incidents were credential confusions, and this was the commonest.
		{"expired bare token", env(stale), StatusFail, "ARGUS_CP_AUTHOR_TOKEN trap", "unset ARGUS_CP_AUTHOR_TOKEN"},
		{"expired session token with a refresh token is routine", session(stale, true, now.Add(-24*time.Hour)), StatusOK, "routine", ""},
		{"expired session token whose refresh is past 30 days", session(stale, true, now.Add(-31*24*time.Hour)), StatusFail, "30 days", "cloud-login"},
		{"expired session token without a refresh token", session(stale, false, now.Add(-time.Hour)), StatusFail, "nothing can renew it", ""},
		{"bare token expiring in two minutes", env(soon), StatusWarn, "expires in 2m", "unset ARGUS_CP_AUTHOR_TOKEN"},
		{"session token expiring in two minutes renews", session(soon, true, now.Add(-time.Hour)), StatusOK, "", ""},
		// A runner-scope token resolves to role.Product, the builder's hat (control/cloudauth.go), not the executor's.
		{"runner scope in the operator's hands", env(at(10*time.Minute, "runner")), StatusWarn, "a builder presents it to runner__run", "cloud-login"},
		{"unrecognised value", env(Classify("wksp_123")), StatusWarn, "neither", "cloud-login"},
		{"unreadable JWT is unknown, never ok", env(Classify(enc([]byte(`{"alg":"RS256"}`)) + ".!!!.sig")), StatusUnknown, "could not be read", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckCredential(tc.in)
			if got.Status != tc.want {
				t.Fatalf("status = %q, want %q\n  subject: %s\n  detail: %s", got.Status, tc.want, got.Subject, got.Detail)
			}
			if got.Subject == "" {
				t.Error("Subject is empty — a check that does not say what it examined cannot be trusted")
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("detail lacks %q: %s", tc.detail, got.Detail)
			}
			if tc.fix != "" && !strings.Contains(got.Fix, tc.fix) {
				t.Errorf("fix lacks %q: %s", tc.fix, got.Fix)
			}
			if got.Status != StatusOK && got.Fix == "" {
				t.Errorf("a non-ok check has no Fix: %+v", got)
			}
		})
	}
}

// REPLAY L0929:R4 (tester verify/2026-09-29-doctor-pr326-live/R4-wrong-token.out). A synthetic odts_ value in
// ARGUS_CP_AUTHOR_TOKEN: argus-dev answered HTTP 401 "invalid credential", and this check still said ok, because
// the refusal only reached executor-version, whose fix was "re-run argus doctor". The control plane's own
// answer outranks what the value looks like.
func TestCheckCredential_RefusedByTheControlPlane(t *testing.T) {
	now := time.Date(2026, 9, 29, 16, 40, 0, 0, time.UTC)
	const cp = "https://argus-dev.onedroid.ai"
	fresh := Classify(jwt(t, map[string]any{"iss": cp, "scope": "author", "exp": now.Add(10 * time.Minute).Unix()}))
	stale := Classify(jwt(t, map[string]any{"iss": cp, "scope": "author", "exp": now.Add(-3 * time.Hour).Unix()}))
	env := func(f TokenFacts, r CredentialRefusal) CredentialInput {
		return CredentialInput{Source: envname.CPAuthorToken, Present: true, Facts: f, ControlPlaneURL: cp, Now: now, Refused: r}
	}
	session := func(f TokenFacts, r CredentialRefusal) CredentialInput {
		return CredentialInput{Source: "/home/x/.config/argus/session.json", Present: true, Facts: f, FromSession: true,
			RefreshPresent: true, ObtainedAt: now.Add(-time.Hour), ControlPlaneURL: cp, Now: now, Refused: r}
	}
	invalid := CredentialRefusal{Status: 401, Msg: "/api/instances answered HTTP 401: invalid credential"}
	notAuthor := CredentialRefusal{Status: 403, Msg: "/api/instances answered HTTP 403: the web is author-scope only"}
	deadRefresh := CredentialRefusal{Status: 400, Renewal: true, Msg: "refresh: refresh token invalid, expired, or revoked"}

	cases := []struct {
		name        string
		in          CredentialInput
		detail, fix []string
	}{
		{"R4: a PAT in the env var that the control plane does not accept", env(Classify("odts_x"), invalid),
			[]string{"refused it", "invalid credential", "revoked", "another control plane"},
			[]string{"unset ARGUS_CP_AUTHOR_TOKEN", "cloud-login --control-plane " + cp + " --scope author", "Tokens page (" + cp + ")"}},
		{"a session token the control plane does not accept", session(fresh, invalid),
			[]string{"refused it", "invalid credential"}, []string{"cloud-login --control-plane " + cp + " --scope author"}},
		{"a builder token where an author credential goes", env(Classify("odts_x"), notAuthor),
			[]string{"accepted it", "author-scope only", "builder token"}, []string{"cloud-login --control-plane " + cp + " --scope author"}},
		{"a session whose refresh token the control plane refuses", session(stale, deadRefresh),
			[]string{"refresh token", "refused", "invalid, expired, or revoked"}, []string{"cloud-login --control-plane " + cp + " --scope author"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckCredential(tc.in)
			if got.Status != StatusFail {
				t.Fatalf("status = %q, want fail: the control plane refused it\n  detail: %s", got.Status, got.Detail)
			}
			for _, s := range tc.detail {
				if !strings.Contains(got.Detail, s) {
					t.Errorf("detail lacks %q: %s", s, got.Detail)
				}
			}
			for _, s := range tc.fix {
				if !strings.Contains(got.Fix, s) {
					t.Errorf("fix lacks %q: %s", s, got.Fix)
				}
			}
			// No fix prints `argus cloud-mint-token --control-plane <url>`: without --router-state the control
			// plane answers it 400, and it mints the machine's onboarding token, not a PAT (#374).
			if strings.Contains(got.Fix, "cloud-mint-token") {
				t.Errorf("fix sends the reader to cloud-mint-token, which fails as printed: %s", got.Fix)
			}
			if !strings.HasPrefix(got.Subject, tc.in.Source+": ") {
				t.Errorf("subject %q no longer names the source and kind", got.Subject)
			}
		})
	}
	// Negative control: the same inputs, not refused, keep their verdicts.
	if c := CheckCredential(env(Classify("odts_x"), CredentialRefusal{})); c.Status != StatusOK {
		t.Errorf("an odts_ PAT nobody refused = %q, want ok", c.Status)
	}
	if c := CheckCredential(session(stale, CredentialRefusal{})); c.Status != StatusOK {
		t.Errorf("an expired session token that renews = %q, want ok", c.Status)
	}
}

// REPLAY B:16 (incidents-B-tester.tsv; the tester notes "a tester's \"couldn't approve: invalid_request\"
// (09-22) = a stale/used device code; no login pending, session.json valid 12:09"). The approve page's
// invalid_request is "unknown, expired, or already-decided user code" (oauth/endpoints.go handleDeviceApprove),
// and nothing on it says that no sign-in was waiting. A signed-in session must say so.
func TestCheckCredential_ReplayB16(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 9, 0, 0, time.UTC)
	const cp = "https://argus-dev.onedroid.ai"
	at := func(d time.Duration) TokenFacts {
		return Classify(jwt(t, map[string]any{"iss": cp, "scope": "author", "exp": now.Add(d).Unix()}))
	}
	session := func(f TokenFacts, obtained time.Time) CredentialInput {
		return CredentialInput{Source: "/home/user/.config/argus/session.json", Present: true, Facts: f, FromSession: true,
			RefreshPresent: true, ObtainedAt: obtained, ControlPlaneURL: cp, Now: now}
	}
	for name, in := range map[string]CredentialInput{
		"B:16 session, access token valid":         session(at(10*time.Minute), now.Add(-5*time.Minute)),
		"session, access token expired and renews": session(at(-time.Hour), now.Add(-2*time.Hour)),
	} {
		t.Run(name, func(t *testing.T) {
			c := CheckCredential(in)
			if c.Status != StatusOK {
				t.Fatalf("status = %q, want ok; detail: %s", c.Status, c.Detail)
			}
			for _, s := range []string{"no sign-in is waiting", "invalid_request", "15m", "nothing to approve"} {
				if !strings.Contains(c.Detail, s) {
					t.Errorf("detail does not say %q: %s", s, c.Detail)
				}
			}
		})
	}
	// Negative controls: where a sign-in IS needed, or the credential is not a session, the sentence would be false.
	for name, in := range map[string]CredentialInput{
		"session past its refresh life needs a sign-in": session(at(-time.Hour), now.Add(-31*24*time.Hour)),
		"a bare env-var token is not a session": {Source: envname.CPAuthorToken, Present: true, Facts: at(10 * time.Minute),
			ControlPlaneURL: cp, Now: now},
		"a runner-scope session is the wrong kind": {Source: "/home/user/.config/argus/session.json", Present: true, FromSession: true,
			RefreshPresent: true, Facts: Classify(jwt(t, map[string]any{"scope": "runner", "exp": now.Add(10 * time.Minute).Unix()})), ControlPlaneURL: cp, Now: now},
	} {
		t.Run("no sentence: "+name, func(t *testing.T) {
			if c := CheckCredential(in); strings.Contains(c.Detail, "nothing to approve") {
				t.Errorf("says there is nothing to approve where that is not established (status %s): %s", c.Status, c.Detail)
			}
		})
	}
}

func TestCheckLocalHats(t *testing.T) {
	// Distinctive values, so "did a value leak into the prose" is a real question.
	const r, a, x = "RUNNER-HAT-VALUE-7c1", "AUTHOR-HAT-VALUE-7c1", "PRESENTED-VALUE-7c1"
	cases := []struct {
		name                      string
		presented, runner, author string
		want                      Status
		detail                    string
	}{
		// OK for the cloud-* path, but the detail must say that the local verbs behind the CLI's auth gate
		// (validate-scenario among them) refuse with "auth not configured" until both hats are set.
		{"nothing set", "", "", "", StatusOK, "auth not configured"},
		{"only the runner hat", "", r, "", StatusFail, "must be set"},
		{"both hats equal", "", r, r, StatusFail, "distinct"},
		{"hats set, nothing presented", "", r, a, StatusWarn, "supply --token or ARGUS_TOKEN"},
		{"presented, no map", x, "", "", StatusFail, "ROLE MAP"},
		{"presented matches neither", x, r, a, StatusFail, "neither hat"},
		{"author hat", a, r, a, StatusOK, "author hat"},
		{"runner hat", r, r, a, StatusOK, "runner hat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := HatsFromValues(tc.presented, tc.runner, tc.author)
			got := CheckLocalHats(in)
			if got.Status != tc.want {
				t.Fatalf("status = %q, want %q; detail: %s", got.Status, tc.want, got.Detail)
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("detail lacks %q: %s", tc.detail, got.Detail)
			}
			for _, v := range []string{tc.presented, tc.runner, tc.author} {
				if v != "" && (strings.Contains(got.Subject, v) || strings.Contains(got.Detail, v) || strings.Contains(got.Fix, v)) {
					t.Errorf("a hat value %q leaked into the check: %+v", v, got)
				}
			}
		})
	}
	if HatsFromValues(a, r, a).Hat != role.Test || HatsFromValues(r, r, a).Hat != role.Product {
		t.Error("HatsFromValues does not resolve hats the way internal/auth does")
	}
}

// REPLAY A:96 (tester-upstream-notepad handoffs/2026-09-18-en-7-fixes-pushed-vault-grafana-access.md:23
// "`validate-scenario` NOT run here (needs `ARGUS_RUNNER_TOKEN` + `ARGUS_AUTHOR_TOKEN`) = UNVERIFIED"): with
// nothing set, a scenario change went unvalidated because the hats read as credentials nobody had. The whole
// recipe has three steps — two distinct strings, then the author one presented; setting the map alone still
// exits "a token is required", and presenting the runner one exits "not permitted for the product scope"
// (both seen 2026-09-29, validating the A:56 pack) — so the detail gives all three.
func TestCheckLocalHats_ReplayA96(t *testing.T) {
	c := CheckLocalHats(HatsFromValues("", "", ""))
	if c.Status != StatusOK {
		t.Fatalf("status = %q: nothing set is fine for the cloud-* path; detail: %s", c.Status, c.Detail)
	}
	for _, s := range []string{"`validate-scenario`", "any two distinct strings", `export ARGUS_TOKEN="$ARGUS_EXECUTOR_SECRET"`} {
		if !strings.Contains(c.Detail, s) {
			t.Errorf("detail does not say %q: %s", s, c.Detail)
		}
	}
}

// #280 renamed ARGUS_AUTHOR_TOKEN -> ARGUS_EXECUTOR_SECRET for the local (M2.5) author hat. Doctor's
// user-facing text must name the NEW variable — a first-run operator reading "ARGUS_AUTHOR_TOKEN" in
// 2026-10 would export a name the CLI no longer prefers.
func TestCheckLocalHats_NamesTheNewVariable(t *testing.T) {
	const r, a = "RUNNER-HAT-VALUE-7c1", "AUTHOR-HAT-VALUE-7c1"

	t.Run("subject names the new variable, not the old one", func(t *testing.T) {
		got := CheckLocalHats(HatsFromValues("", r, a))
		if !strings.Contains(got.Subject, "ARGUS_EXECUTOR_SECRET") {
			t.Errorf("subject does not name ARGUS_EXECUTOR_SECRET: %s", got.Subject)
		}
		if strings.Contains(got.Subject, "ARGUS_AUTHOR_TOKEN") {
			t.Errorf("subject still names the deprecated ARGUS_AUTHOR_TOKEN: %s", got.Subject)
		}
	})
	t.Run("config-error fix names the new variable", func(t *testing.T) {
		got := CheckLocalHats(HatsFromValues("", r, ""))
		if !strings.Contains(got.Fix, "ARGUS_EXECUTOR_SECRET") {
			t.Errorf("fix does not name ARGUS_EXECUTOR_SECRET: %s", got.Fix)
		}
		if strings.Contains(got.Fix, "ARGUS_AUTHOR_TOKEN") {
			t.Errorf("fix still names the deprecated ARGUS_AUTHOR_TOKEN: %s", got.Fix)
		}
	})
	t.Run("wrong-hat detail points at the new variable", func(t *testing.T) {
		got := CheckLocalHats(HatsFromValues(r, r, a)) // presented the runner hat
		if !strings.Contains(got.Detail, "ARGUS_EXECUTOR_SECRET") {
			t.Errorf("detail does not name ARGUS_EXECUTOR_SECRET: %s", got.Detail)
		}
	})
}

func TestCheckConfig(t *testing.T) {
	cases := []struct {
		name   string
		in     ConfigInput
		want   Status
		subj   string
		detail []string // every one must appear in Detail
		absent []string // none may appear in Detail
	}{
		{"no --config", ConfigInput{}, StatusWarn, "no --config given", nil, nil},
		{"parse error", ConfigInput{Path: "c.yaml", Err: "parse c.yaml: yaml: line 3"}, StatusFail, "c.yaml", nil, nil},
		{"no project name", ConfigInput{Path: "c.yaml"}, StatusWarn, "c.yaml", nil, nil},
		{"ok, not validated: says so", ConfigInput{Path: "c.yaml", ProjectName: "memstore"}, StatusOK, `project "memstore"`,
			[]string{"not checked against scenarios", "--scenarios"}, nil},
		{"ok, validated", ConfigInput{Path: "c.yaml", ProjectName: "memstore", Validated: true, ScenariosChecked: 59, ScenariosDir: "s"},
			StatusOK, `project "memstore"`, []string{"59 scenarios"}, []string{"not checked"}},
		{"unset ${VAR}s warn, by name and field, and matter only here", ConfigInput{Path: "c.yaml", ProjectName: "memstore", Validated: true,
			ScenariosChecked: 1, ScenariosDir: "s", Unset: []string{"MEMSTORE_TOKEN (targets.mcp.auth.bearer_token)"}},
			StatusWarn, "c.yaml", []string{"MEMSTORE_TOKEN (targets.mcp.auth.bearer_token)", "started from this machine", "own environment"}, nil},
		{"a refusal outranks a warning, and both are said", ConfigInput{Path: "c.yaml", ProjectName: "memstore", Validated: true, ScenariosChecked: 2,
			ScenariosDir: "s", Invalid: []string{"X-1: boom"}, Portless: []string{"targets.http.base_url"}},
			StatusFail, "c.yaml", []string{"X-1: boom", "targets.http.base_url"}, nil},
	}
	for _, tc := range cases {
		got := CheckConfig(tc.in)
		if got.Status != tc.want || !strings.Contains(got.Subject, tc.subj) {
			t.Errorf("%s: status %q subject %q, want %q / %q", tc.name, got.Status, got.Subject, tc.want, tc.subj)
		}
		for _, s := range tc.detail {
			if !strings.Contains(got.Detail, s) {
				t.Errorf("%s: detail lacks %q: %s", tc.name, s, got.Detail)
			}
		}
		for _, s := range tc.absent {
			if strings.Contains(got.Detail, s) {
				t.Errorf("%s: detail says %q: %s", tc.name, s, got.Detail)
			}
		}
		if got.Status != StatusOK && got.Fix == "" {
			t.Errorf("%s: a non-ok check has no Fix", tc.name)
		}
	}
}

// REPLAY: one case per recorded incident this check claims (incidents-{A,B}-*.tsv; tagging in
// verify/2026-09-28-doctor-incident-tagging, extend:argus-config). A:95 and B:28 (a quote or backslash in a
// ${VAR} emptied a UI payload) are an EXECUTOR fix, #160 in 0.3.37, and are replayed by
// TestCheckExecutorVersion_Replay. A:90 and B:14 are NOT claimed: the port was a scenario's app_url
// (WEBUI-001: compose :8090, the AKS service :80), and which port a cluster service exposes is not
// visible from this machine.
func TestCheckConfig_Replay(t *testing.T) {
	cases := []struct {
		row, source string
		in          ConfigInput
		want        Status
		detail, fix []string
	}{
		// The layer error onboarding hid: validate-config ran in a container whose stderr onboard.sh drops on failure.
		{"A:8", `tester-upstream-notepad handoffs/2026-08-28-laptop-upstream-hub-onboarding.md:97-100 "6/8 validate → should now PASS (the layer fix) … its errors go to the container's stderr, which onboard.sh swallows on failure"`,
			ConfigInput{Path: "product-agent/argus-config.yaml", ProjectName: "hub", Validated: true, ScenariosChecked: 5, ScenariosDir: "test-agent/scenarios",
				Invalid: []string{`SYN-003: uses layer "HTTP Ingestion" but config has no usable targets.http`}},
			StatusFail, []string{`SYN-003: uses layer "HTTP Ingestion" but config has no usable targets.http`, "1 of 5"},
			[]string{"argus validate-config --config product-agent/argus-config.yaml --scenarios test-agent/scenarios"}},
		{"B:35", `handoffs/2026-09-23-hub-harness-scoped-hop-ask-sent.md:70 "a port-less value makes http fall back to :8080, internal/config/config.go:1159-1161"`,
			ConfigInput{Path: "argus-config.yaml", ProjectName: "hub", Validated: true, ScenariosChecked: 12, ScenariosDir: "scenarios",
				Portless: []string{"targets.http.base_url"}},
			StatusWarn, []string{"targets.http.base_url", "no port", ":8080", ":80"}, []string{"targets.http.base_url", ":80"}},
	}
	for _, tc := range cases {
		t.Run(tc.row, func(t *testing.T) {
			c := CheckConfig(tc.in)
			if c.Status != tc.want {
				t.Fatalf("%s: status = %q, want %q\nsource: %s\ndetail: %s", tc.row, c.Status, tc.want, tc.source, c.Detail)
			}
			for _, s := range tc.detail {
				if !strings.Contains(c.Detail, s) {
					t.Errorf("%s: detail does not say %q\nsource: %s\ndetail: %s", tc.row, s, tc.source, c.Detail)
				}
			}
			for _, s := range tc.fix {
				if !strings.Contains(c.Fix, s) {
					t.Errorf("%s: fix does not say %q\nfix: %s", tc.row, s, c.Fix)
				}
			}
		})
	}
}

func TestCheckScenarios(t *testing.T) {
	const def = "scenarios"
	cases := []struct {
		name   string
		in     ScenariosInput
		want   Status
		detail string
	}{
		{"unreadable is unknown", ScenariosInput{Dir: "s", Exists: true, WalkErr: "permission denied"}, StatusUnknown, "could not be read"},
		{"default absent", ScenariosInput{Dir: def, IsDefault: true}, StatusWarn, "not checked"},
		{"chosen dir absent", ScenariosInput{Dir: "s"}, StatusFail, "does not exist"},
		{"no scenario files", ScenariosInput{Dir: "s", Exists: true}, StatusFail, "no scenario files"},
		// The mistake `run` cannot refuse at flag parsing.
		{"demo scenarios against another product", ScenariosInput{Dir: def, IsDefault: true, Exists: true, Files: 9, ProjectName: "memstore"}, StatusWarn, `project "memstore"`},
		{"demo scenarios against the demo", ScenariosInput{Dir: def, IsDefault: true, Exists: true, Files: 9, ProjectName: "order-service"}, StatusOK, ""},
		{"own scenarios, no config to compare", ScenariosInput{Dir: "s", Exists: true, Files: 12}, StatusOK, "not compared"},
		{"own scenarios", ScenariosInput{Dir: "s", Exists: true, Files: 12, ProjectName: "hub"}, StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckScenarios(tc.in)
			if got.Status != tc.want {
				t.Fatalf("status = %q, want %q; detail: %s", got.Status, tc.want, got.Detail)
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("detail lacks %q: %s", tc.detail, got.Detail)
			}
		})
	}
}

// REPLAY: one case per recorded incident the import preview claims (verify/2026-09-28-doctor-incident-tagging/tags.tsv,
// extend:scenarios-dir; source rows in verify/2026-09-25-friction-investigation/incidents-A-upstream-notepad.tsv).
// The refusals here are what gatherScenarios collects; TestDoctor_ScenariosTheImportWouldRefuseAreNamed (cmd/argus)
// proves the real rules produce them.
func TestCheckScenarios_Replay(t *testing.T) {
	oldFormat := func(n int) []ScenarioRefusal {
		var out []ScenarioRefusal
		for i := 1; i <= n; i++ {
			out = append(out, ScenarioRefusal{Path: fmt.Sprintf("SYN-S0-%03d.md", i), ID: fmt.Sprintf("SYN-S0-%03d", i), Errors: 7,
				First: "line 18: ## EXPECT declares no '### Runnable' bullet"})
		}
		return out
	}
	cases := []struct {
		row, source string
		in          ScenariosInput
		detail      []string
	}{
		// The 08-28 IDs broke the old uppercase grammar, which VR10-S4-1 widened; the drop was silent because
		// nothing named the file before the import counted it. An ID today's rule refuses stands in for them.
		{"A:9", `tester-upstream-notepad handoffs/2026-08-28-laptop-upstream-hub-onboarding.md:106 "a scenario whose ID fails the grammar is dropped SILENTLY; the count is the only signal"`,
			ScenariosInput{Dir: "scenarios", Exists: true, Files: 5, ProjectName: "hub", Imported: true, ImportFiles: 5, Version: "0.3.44",
				Refused: []ScenarioRefusal{{Path: "SYN-MCP-001.md", ID: "syn.mcp.001", Errors: 1, First: `line 4: ID "syn.mcp.001" is not allowed`}}},
			[]string{"would refuse 1 of 5", "SYN-MCP-001.md", `"syn.mcp.001"`, "is not allowed", "0.3.44"}},
		{"A:56", `tester-upstream-notepad handoffs/2026-09-14-pass2-done-0331-format-break-argus-pivot.md:22-24 "0.3.31 … does not accept old-format files; all 16 of ours are old-format"`,
			ScenariosInput{Dir: "scenarios-v2", Exists: true, Files: 16, ProjectName: "hub", Imported: true, ImportFiles: 16, Version: "0.3.44",
				Refused: oldFormat(16)},
			[]string{"would refuse 16 of 16", "SYN-S0-001.md", "7 errors", "### Runnable", "and 11 more"}},
	}
	for _, tc := range cases {
		t.Run(tc.row, func(t *testing.T) {
			c := CheckScenarios(tc.in)
			if c.Status != StatusFail {
				t.Fatalf("%s: status = %q, want fail\nsource: %s\ndetail: %s", tc.row, c.Status, tc.source, c.Detail)
			}
			for _, s := range tc.detail {
				if !strings.Contains(c.Detail, s) {
					t.Errorf("%s: detail does not say %q\nsource: %s\ndetail: %s", tc.row, s, tc.source, c.Detail)
				}
			}
			if !strings.Contains(c.Fix, "validate-scenario --file") {
				t.Errorf("%s: fix does not name the per-file check: %s", tc.row, c.Fix)
			}
			// A whole pack in an old format fails the same way: say that once, not once per file.
			if tc.row == "A:56" {
				if n := strings.Count(c.Detail, "### Runnable"); n != 1 {
					t.Errorf("%s: the shared first refusal appears %d times, want once: %s", tc.row, n, c.Detail)
				}
				if !strings.Contains(c.Detail, "each fails first on") {
					t.Errorf("%s: detail does not say the refusal is shared: %s", tc.row, c.Detail)
				}
			}
		})
	}
}

func TestCountScenarioFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a.md", "sub/b.md", "README.md", "sub/README-notes.md", "notes.txt"} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("# x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	n, err := CountScenarioFiles(dir)
	if err != nil || n != 2 {
		t.Fatalf("n = %d, err = %v; want 2 (a.md, sub/b.md — README* and .txt are not scenarios)", n, err)
	}
}

func TestHuman(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second:               "45s",
		3 * time.Minute:                "3m",
		2 * time.Hour:                  "2h",
		2*time.Hour + 5*time.Minute:    "2h 5m",
		31 * 24 * time.Hour:            "31 days",
		-(2*time.Hour + 5*time.Minute): "2h 5m",
	} {
		if got := human(d); got != want {
			t.Errorf("human(%v) = %q, want %q", d, got, want)
		}
	}
}

// The rules' messages carry "—" (three bytes); a cut inside one would put invalid UTF-8 in the report.
func TestClipKeepsCharactersWhole(t *testing.T) {
	s := strings.Repeat("a", 159) + "— the rest"
	got := clip(s)
	if !utf8.ValidString(got) {
		t.Fatalf("clip cut a character in half: %q", got[150:])
	}
	if !strings.HasSuffix(got, "…") || len(got) > 160+len("…") {
		t.Errorf("clip = %q; want at most 160 bytes, then …", got)
	}
}
