package argus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// #140 follow-up — the cause #139 quotes must not carry a secret into the report. The spec's
// app_url is ${VAR}-expanded before Playwright sees it, and Playwright quotes the URL in full
// ("page.goto: net::ERR_CONNECTION_REFUSED at <url>"), user:password and query included. The row
// keeps where it tried to go (scheme, host, port, path — the #139 diagnosis) and loses the rest.
// (Found by Aleksander in his review of #140, reproduced on dev 5cb13b0 with real Playwright.)
func TestUIScenario_CauseRedactsTheURL(t *testing.T) {
	orig := uiRun
	defer func() { uiRun = orig }()

	const secret = "zz-fake-secret-140"
	t.Setenv("ZZ_FAKE_TOKEN", secret)
	sc := scenario.Parse(strings.Join([]string{
		"# Scenario: u", "",
		"## Metadata", "- **ID**: WEBUI-001", "- **Layer**: Web UI", "- **Tags**: ui", "",
		"## TRIGGER", "POST \x60tests/live/x.spec.ts\x60", "",
		"```json",
		`{"spec":"tests/live/x.spec.ts","app_url":"http://fakeuser:${ZZ_FAKE_TOKEN}@127.0.0.1:59997/app?token=${ZZ_FAKE_TOKEN}#k=${ZZ_FAKE_TOKEN}"}`,
		"```", "",
		"## EXPECT", "### Runnable", "- dom has .banner", "",
		"### Non-runnable", "- the flow is described in the spec file", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n"))

	check := func(t *testing.T, res report.ScenarioResult, sawURL string) {
		t.Helper()
		if !strings.Contains(sawURL, secret) {
			t.Fatalf("fixture: the runner must have been handed the expanded app_url, got %q", sawURL)
		}
		if res.Status != report.StatusError {
			t.Fatalf("status = %q, want %q", res.Status, report.StatusError)
		}
		obs := res.Failure.Observed
		if strings.Contains(obs, secret) || strings.Contains(obs, "fakeuser") {
			t.Errorf("the row leaks the app_url's credentials: %q", obs)
		}
		if !strings.Contains(obs, "ERR_CONNECTION_REFUSED at http://") || !strings.Contains(obs, "127.0.0.1:59997/app") {
			t.Errorf("the cause and where it tried to go must stay: %q", obs)
		}
	}

	t.Run("from the JSON report", func(t *testing.T) {
		var saw string
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			saw = appURL
			dir := filepath.Join(vendorDir, "argus-out")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			body := `{"errors":[{"message":"Error: page.goto: net::ERR_CONNECTION_REFUSED at ` + appURL + `\nCall log:"}],"suites":[]}`
			if err := os.WriteFile(filepath.Join(dir, "results.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			return 1, true, ""
		}
		check(t, runUIScenario(sc, "tr-140-1", t.TempDir()), saw)
	})

	t.Run("from stderr", func(t *testing.T) {
		var saw string
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			saw = appURL
			return 1, true, "Error: page.goto: net::ERR_CONNECTION_REFUSED at " + appURL + "\nCall log:"
		}
		check(t, runUIScenario(sc, "tr-140-2", t.TempDir()), saw)
	})
}

// redactURLs is the whole rule: userinfo, query and fragment go; scheme, host, port and path stay.
func TestRedactURLs(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"at http://u:p@h:1/x?token=s#f", "at http://REDACTED@h:1/x?REDACTED"},
		{"at http://u:pa/ss@h/x", "at http://REDACTED@h/x"}, // an unencoded '/' in the password
		{"at https://h/x?a=1 and ws://u@h2", "at https://h/x?REDACTED and ws://REDACTED@h2"},
		{`navigating to "http://h:8090/"`, `navigating to "http://h:8090/"`},
		{"no url here", "no url here"},
		// #152 review (Aleksander): each of these leaked on b3e9b15.
		{"at http://fakeuser:ab'cd@127.0.0.1:59997/app?token=T", "at http://REDACTED@127.0.0.1:59997/app?REDACTED"}, // apostrophe in the password
		{"at http://fakeuser:ab#cd@127.0.0.1:59997/app?token=T", "at http://REDACTED?REDACTED"},                     // '#' in the password: host lost, nothing leaks
		{"at http://fakeuser:ab?cd@127.0.0.1:59997/app?token=T", "at http://REDACTED?REDACTED"},                     // '?' in the password: host lost, nothing leaks
		{"at http://u:a@b@h/x", "at http://REDACTED@h/x"},                                                           // '@' in the password
		{"at http://h/x#access_token=s", "at http://h/x?REDACTED"},                                                  // fragment only
		{"at http://h/x?email=a@b.com&t=s", "at http://h/x?REDACTED"},                                               // '@' in a query value is not userinfo
		{"at http://[::1]:8080/x?t=s", "at http://[::1]:8080/x?REDACTED"},                                           // IPv6 literal
		{"at http://u:p@[::1]:8080/x", "at http://REDACTED@[::1]:8080/x"},
		// The pinned trade-off: an '@' at the start of a path segment reads as a userinfo end. The host is
		// lost; nothing leaks.
		{"at http://h:1/@scope/x", "at http://REDACTED@scope/x"},
		// #152 second review (Aleksander): each of these kept a credential on 3163f40.
		{"GET http://h/api/me?next=/@alice/home&token=S → 401", "GET http://REDACTED?REDACTED → 401"},                                                                             // '@' + host-like text in the query
		{`{"error":"failed to connect to amqp://svc:S@rabbitmq:5672: connection refused"}`, `{"error":"failed to connect to amqp://REDACTED@rabbitmq:5672: connection refused"}`}, // no path, ends in ':'
		{"(see http://svc:S@h:1)", "(see http://REDACTED@h:1)"},                                                                                                                   // ends in ')'
		{"at 'http://svc:S@h:1', retrying", "at 'http://REDACTED@h:1', retrying"},                                                                                                 // closing apostrophe
		{"at http://svc:pw12345@${SUT_HOST}:8080/app", "at http://REDACTED@${SUT_HOST}:8080/app"},                                                                                 // host scrubbed back to ${VAR}
		{"at http://u:a?b@h:5672:", "at http://REDACTED?REDACTED"},                                                                                                                // '?' in the password, no readable host
	} {
		if got := redactURLs(c.in); got != c.want {
			t.Errorf("redactURLs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A stderr that opens with colour codes on a line of their own used to leave an empty "first line",
// so the real error was dropped for Argus's fallback (ui_scenario.go:252 took the line, then stripped).
func TestUIFailureCause_ColourCodesBeforeTheError(t *testing.T) {
	stderr := "\x1b[2m\x1b[22m\n\x1b[31mError: browserType.launch: Executable doesn't exist\x1b[39m\n    at x"
	got, fromPW := uiFailureCause(filepath.Join(t.TempDir(), "results.json"), stderr, "argus fallback")
	if !fromPW || got != "Error: browserType.launch: Executable doesn't exist" {
		t.Errorf("uiFailureCause = %q, %v; want Playwright's error line", got, fromPW)
	}
}

// uiScenario builds a one-bullet Web UI scenario whose payload app_url is appURL.
func uiScenario(t *testing.T, appURL, bullet string) *scenario.Scenario {
	t.Helper()
	return scenario.Parse(strings.Join([]string{
		"# Scenario: u", "",
		"## Metadata", "- **ID**: WEBUI-001", "- **Layer**: Web UI", "- **Tags**: ui", "",
		"## TRIGGER", "POST \x60tests/live/x.spec.ts\x60", "",
		"```json",
		`{"spec":"tests/live/x.spec.ts","app_url":"` + appURL + `"}`,
		"```", "",
		"## EXPECT", "### Runnable", "- " + bullet, "",
		"### Non-runnable", "- the flow is described in the spec file", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n"))
}

// #152 review point 3 — every branch that can put Playwright's or the spec's text into the row is
// redacted, not only the #139 cause: Failure.Observed is set in one place, and that is where it goes.
func TestUIScenario_EveryObservedBranchIsRedacted(t *testing.T) {
	orig := uiRun
	defer func() { uiRun = orig }()

	const secret = "zz-fake-secret-152"
	t.Setenv("ZZ_FAKE_TOKEN", secret)
	t.Setenv("ZZ_APP_CODE", secret) // a name the spec's own env-var mask (KEY|PASSWORD|SECRET|TOKEN) misses

	noLeak := func(t *testing.T, res report.ScenarioResult, mustKeep string) {
		t.Helper()
		if res.Failure == nil {
			t.Fatalf("status %q with no failure row", res.Status)
		}
		obs := res.Failure.Observed
		if strings.Contains(obs, secret) || strings.Contains(obs, "fakeuser") {
			t.Errorf("the row leaks: %q", obs)
		}
		if !strings.Contains(obs, mustKeep) {
			t.Errorf("the row lost %q: %q", mustKeep, obs)
		}
	}

	t.Run("no Playwright artifact: Classify's first stderr line, behind colour codes", func(t *testing.T) {
		sc := uiScenario(t, "http://fakeuser:${ZZ_FAKE_TOKEN}@127.0.0.1:59997/app?token=${ZZ_FAKE_TOKEN}", "dom has .banner")
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			return 1, false, "\x1b[2m\x1b[22m\n\x1b[31mError: page.goto: net::ERR_CONNECTION_REFUSED at " + appURL + "\x1b[39m\nCall log:"
		}
		res := runUIScenario(sc, "tr-152-1", t.TempDir())
		if res.Status != report.StatusError {
			t.Fatalf("status = %q, want %q (the no-artifact branch)", res.Status, report.StatusError)
		}
		noLeak(t, res, "ERR_CONNECTION_REFUSED at http://REDACTED@127.0.0.1:59997/app?REDACTED")
	})

	t.Run("a failed backend-errors bullet quoting the response URL", func(t *testing.T) {
		sc := uiScenario(t, "http://127.0.0.1:59997/app?code=${ZZ_APP_CODE}", "no backend 4xx/5xx")
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			if len(asserts) != 1 {
				t.Fatalf("fixture: want 1 declared bullet, got %d", len(asserts))
			}
			if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
				t.Fatal(err)
			}
			obs := "backend errors: GET http://127.0.0.1:59997/api/me?code=" + secret + " → 500 " + `{"error":"bad code ` + secret + `"}`
			b, _ := json.Marshal([]uiOutcomeEntry{{Bullet: asserts[0].Bullet, OK: false, Observed: obs}})
			if err := os.WriteFile(outPath, b, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(vendorDir, "test-results", "x"), 0o755); err != nil {
				t.Fatal(err)
			}
			return 1, true, ""
		}
		res := runUIScenario(sc, "tr-152-2", t.TempDir())
		if res.Status != "failed" {
			t.Fatalf("status = %q, want failed (the failed-bullet branch)", res.Status)
		}
		noLeak(t, res, "GET http://127.0.0.1:59997/api/me?REDACTED → 500")
	})

	t.Run("a secret in the URL path (review point 4)", func(t *testing.T) {
		sc := uiScenario(t, "http://127.0.0.1:59997/${ZZ_FAKE_TOKEN}/app", "dom has .banner")
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			return 1, false, "Error: page.goto: net::ERR_CONNECTION_REFUSED at " + appURL
		}
		noLeak(t, runUIScenario(sc, "tr-152-3", t.TempDir()), "at http://127.0.0.1:59997/${ZZ_FAKE_TOKEN}/app")
	})
}

func TestScrubVarValues(t *testing.T) {
	t.Setenv("ZZ_LONG", "s3cr3t-value")
	t.Setenv("ZZ_SHORT", "dev")
	t.Setenv("ZZ_URL", "http://h:1/app")
	payload := `{"app_url":"${ZZ_URL}/${ZZ_LONG}?q=${ZZ_SHORT}&c=${cid}"}`
	for _, c := range []struct{ in, want string }{
		{"at http://h:1/app/s3cr3t-value?q=dev", "at http://h:1/app/${ZZ_LONG}?q=dev"}, // short value + whole-URL value kept
		{"body 's3cr3t-value' quoted", "body '${ZZ_LONG}' quoted"},                     // outside any URL
		{"escaped s3cr3t%2Dvalue", "escaped s3cr3t%2Dvalue"},                           // '-' is not escaped by either form
		{"no value here", "no value here"},
	} {
		if got := scrubVarValues(c.in, payload); got != c.want {
			t.Errorf("scrubVarValues(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A value that is a prefix of a longer one must not split it (longest first).
	t.Setenv("ZZ_TENANT", "acme-prod-01")
	t.Setenv("ZZ_KEY", "acme-prod-01-9f8e7d6c")
	if got, want := scrubVarValues("at /k/acme-prod-01-9f8e7d6c/x", `{"a":"${ZZ_TENANT}","b":"${ZZ_KEY}"}`), "at /k/${ZZ_KEY}/x"; got != want {
		t.Errorf("prefix value: got %q, want %q", got, want)
	}
	t.Setenv("ZZ_LONG", "a b+c/d=secret")
	if got := scrubVarValues("q=a+b%2Bc%2Fd%3Dsecret p=a%20b+c%2Fd=secret", payload); strings.Contains(got, "secret") {
		t.Errorf("a URL-escaped value survived: %q", got)
	}
}
