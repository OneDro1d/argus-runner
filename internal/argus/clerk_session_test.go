package argus

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/clerksession"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// Auth.type clerk_session, wired into the run. The Clerk API below is an httptest mock;
// every key, id and token is an obvious fake and nothing here dials the real one.

const (
	csFakeSecret = "sk_test_FAKEFAKEFAKEFAKE9999"
	csFakeUser   = "user_FAKE0000000009"
)

func csJWT(exp time.Time, tag string) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	return enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." +
		enc(map[string]any{"iat": time.Now().Unix(), "exp": exp.Unix(), "tag": tag}) + ".FAKESIG" + tag
}

type csReq struct{ Method, Path, Auth, Body string }

type csClerk struct {
	srv       *httptest.Server
	mu        sync.Mutex
	reqs      []csReq
	refuse    int // status for POST /v1/sessions; 0 = ok
	nSessions int
	nTokens   int
	life      time.Duration
}

func newCSClerk(t *testing.T) *csClerk {
	m := &csClerk{life: 90 * time.Second}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.reqs = append(m.reqs, csReq{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)})
		switch {
		case r.URL.Path == "/v1/sessions":
			if m.refuse != 0 {
				w.WriteHeader(m.refuse)
				return
			}
			m.nSessions++
			fmt.Fprintf(w, `{"id":"sess_FAKE%d"}`, m.nSessions)
		case strings.HasSuffix(r.URL.Path, "/revoke"):
			fmt.Fprint(w, `{"status":"revoked"}`)
		default:
			m.nTokens++
			fmt.Fprintf(w, `{"jwt":%q}`, csJWT(time.Now().Add(m.life), fmt.Sprint(m.nTokens)))
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *csClerk) count(method, pathSuffix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.reqs {
		if r.Method == method && strings.HasSuffix(r.Path, pathSuffix) {
			n++
		}
	}
	return n
}

func (m *csClerk) bodies(path string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.reqs {
		if r.Path == path {
			out = append(out, r.Body)
		}
	}
	return out
}

func csConfig(t *testing.T, m *csClerk) *config.Config {
	t.Helper()
	t.Setenv("ARGUS_TEST_CLERK_SK", csFakeSecret)
	t.Setenv("ARGUS_TEST_CLERK_UID", csFakeUser)
	c := httpConfig()
	c.Targets.Auth = &config.AuthTarget{Type: "clerk_session", SecretKeyEnv: "ARGUS_TEST_CLERK_SK", UserIDEnv: "ARGUS_TEST_CLERK_UID", APIURL: m.srv.URL}
	return c
}

func bearerOf(h string) string { return strings.TrimPrefix(h, "Bearer ") }

func TestDeriveProps_ClerkSessionSendsTheMintedTokenWhereTheStaticBearerGoes(t *testing.T) {
	m := newCSClerk(t)
	c, done := attachSessions(csConfig(t, m))
	defer done()
	s := &scenario.Scenario{ID: "CS-1", Layers: []string{"HTTP Ingestion"}, Trigger: scenario.Trigger{Method: "GET", URL: "${U}/x"}, Expect: []string{"status=200"}}
	got := mustProps(t, c, s, "tr-1")["auth.header"]
	if !strings.HasPrefix(got, "Bearer eyJ") || strings.Count(got, ".") != 2 {
		t.Fatalf("auth.header should be a Bearer JWT, got a %d-byte value", len(got))
	}
	if !isSecretProp("auth.header") {
		t.Fatal("auth.header must stay a secret prop (kept out of JMeter's -J run log)")
	}
}

func TestDeriveProps_ClerkSession_OwnAuthorizationWinsAndMintsNothing(t *testing.T) {
	m := newCSClerk(t)
	c, done := attachSessions(csConfig(t, m))
	defer done()
	s := &scenario.Scenario{ID: "CS-2", Layers: []string{"HTTP Ingestion"},
		Trigger: scenario.Trigger{Method: "GET", URL: "${U}/x", Headers: map[string]string{"Authorization": "Bearer step-own-token"}}, Expect: []string{"status=200"}}
	if got := mustProps(t, c, s, "tr-1")["auth.header"]; got != "Bearer step-own-token" {
		t.Fatalf("a TRIGGER's own Authorization must win, got %q", got)
	}
	s.Trigger.Headers["Authorization"] = ""
	if got, ok := mustProps(t, c, s, "tr-2")["auth.header"]; !ok || got != "" {
		t.Fatalf("the empty Authorization line must still drop the token, got ok=%v %q", ok, got)
	}
	if n := len(m.reqs); n != 0 {
		t.Fatalf("a scenario that sets its own Authorization needs no session; Clerk saw %d calls", n)
	}
}

func TestDeriveProps_ClerkSession_PerTargetAuthWinsOverTheDefault(t *testing.T) {
	m := newCSClerk(t)
	t.Setenv("ARGUS_TEST_CLERK_UID2", "user_FAKE0000000002")
	c := csConfig(t, m)
	c.Targets.HTTPTargets = map[string]*config.HTTPTarget{
		"admin": {BaseURL: "http://admin:8080", Auth: &config.AuthTarget{Type: "clerk_session", SecretKeyEnv: "ARGUS_TEST_CLERK_SK", UserIDEnv: "ARGUS_TEST_CLERK_UID2", APIURL: m.srv.URL}},
		"plain": {BaseURL: "http://plain:8080"},
		"stat":  {BaseURL: "http://stat:8080", Auth: &config.AuthTarget{Type: "bearer", BearerToken: "static-tok"}},
	}
	c, done := attachSessions(c)
	defer done()
	mk := func(target string) *scenario.Scenario {
		return &scenario.Scenario{ID: "CS-3", Target: target, Layers: []string{"HTTP Ingestion"}, Trigger: scenario.Trigger{Method: "GET", URL: "${U}/x"}, Expect: []string{"status=200"}}
	}
	def := mustProps(t, c, mk(""), "tr")["auth.header"]
	adm := mustProps(t, c, mk("admin"), "tr")["auth.header"]
	pl := mustProps(t, c, mk("plain"), "tr")["auth.header"]
	st := mustProps(t, c, mk("stat"), "tr")["auth.header"]
	if def == adm {
		t.Error("the named target's own identity must mint its own session")
	}
	if pl != def {
		t.Error("a named target without auth falls back to the default session")
	}
	if st != "Bearer static-tok" {
		t.Errorf("a per-target static bearer must win too, got %q", st)
	}
	users := m.bodies("/v1/sessions")
	if len(users) != 2 || !strings.Contains(users[0]+users[1], csFakeUser) || !strings.Contains(users[0]+users[1], "user_FAKE0000000002") {
		t.Errorf("want one session per identity (2), got bodies %v", users)
	}
}

func TestDeriveProps_ClerkSession_NoSourceAttachedIsAnError(t *testing.T) {
	m := newCSClerk(t)
	s := &scenario.Scenario{ID: "CS-4", Layers: []string{"HTTP Ingestion"}, Trigger: scenario.Trigger{Method: "GET", URL: "${U}/x"}, Expect: []string{"status=200"}}
	_, err := DeriveProps(csConfig(t, m), s, "tr")
	var se *SessionError
	if err == nil || !errorsAs(err, &se) {
		t.Fatalf("a clerk_session scenario with no session source must be a SessionError, got %v", err)
	}
}

func errorsAs(err error, target **SessionError) bool {
	for err != nil {
		if se, ok := err.(*SessionError); ok {
			*target = se
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func csRun(t *testing.T, c *config.Config, ids ...string) (*RunResult, string) {
	t.Helper()
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	for _, id := range ids {
		writeScenario(t, scDir, "http-ingestion", id, "status=202")
	}
	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", &recordingRunner{})
	if err != nil {
		t.Fatal(err)
	}
	return rr, dir
}

func TestRunAll_ClerkSession_MintsOncePerRunSendsToEveryScenarioAndRevokesAtTheEnd(t *testing.T) {
	m := newCSClerk(t)
	c := csConfig(t, m)
	rr := &recordingRunner{}
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	for _, id := range []string{"CS-A", "CS-B", "CS-C"} {
		writeScenario(t, scDir, "http-ingestion", id, "status=202")
	}
	res, err := RunAll(c, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", rr)
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Summary.Passed != 3 {
		t.Fatalf("summary = %+v", res.Report.Summary)
	}
	if len(rr.props) != 3 {
		t.Fatalf("runner saw %d runs", len(rr.props))
	}
	tok := rr.props[0]["auth.header"]
	for i, p := range rr.props {
		if p["auth.header"] != tok || !strings.HasPrefix(tok, "Bearer eyJ") {
			t.Errorf("run %d: auth.header differs or is not a minted Bearer", i)
		}
	}
	if m.count("POST", "/v1/sessions") != 1 || m.nTokens != 1 {
		t.Errorf("one session and one token for the whole run: sessions=%d tokens=%d", m.nSessions, m.nTokens)
	}
	if m.count("POST", "/revoke") != 1 {
		t.Errorf("the session must be revoked once the run ends, saw %d revokes", m.count("POST", "/revoke"))
	}
	if c.Sessions != nil {
		t.Error("RunAll must not leave its session source on the caller's config")
	}
}

func TestRunAll_ClerkSession_RevokesWhenTheRunBlowsUp(t *testing.T) {
	m := newCSClerk(t)
	c := csConfig(t, m)
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "CS-P", "status=202")
	func() {
		defer func() { _ = recover() }()
		RunAll(c, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", panicRunner{})
	}()
	if m.count("POST", "/revoke") != 1 {
		t.Fatalf("an aborted run (panic) must still revoke: %d revokes", m.count("POST", "/revoke"))
	}
}

type panicRunner struct{}

func (panicRunner) Run(string, string, map[string]string, time.Duration) error {
	panic("the rig fell over")
}

func TestRunAll_ClerkSession_RevokesWhenEveryScenarioFails(t *testing.T) {
	m := newCSClerk(t)
	c := csConfig(t, m)
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "CS-F", "status=202")
	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", execErrorRunner{msg: "jmeter exec failed"})
	if err != nil {
		t.Fatal(err)
	}
	if rr.Report.Summary.Passed != 0 {
		t.Fatalf("setup: the run should not pass: %+v", rr.Report.Summary)
	}
	if m.count("POST", "/revoke") != 1 {
		t.Fatalf("a failing run must still revoke: %d", m.count("POST", "/revoke"))
	}
}

func TestRunAll_ClerkSession_RevokeFailureIsLoggedWithoutSecretsAndKeepsTheVerdict(t *testing.T) {
	m := newCSClerk(t)
	c := csConfig(t, m)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	// kill the API before the run ends: the revoke fails, the verdict stands
	rr := &killClerkRunner{recordingRunner: recordingRunner{}, kill: m.srv.Close}
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "CS-R", "status=202")
	res, err := RunAll(c, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", rr)
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Summary.Passed != 1 {
		t.Fatalf("a revoke failure must not change the verdict: %+v", res.Report.Summary)
	}
	if !strings.Contains(buf.String(), "clerk_session: could not revoke the session") {
		t.Errorf("the failure must be logged: %q", buf.String())
	}
	if strings.Contains(buf.String(), csFakeSecret) {
		t.Error("the log leaked the secret key")
	}
}

type killClerkRunner struct {
	recordingRunner
	kill func()
}

func (k *killClerkRunner) Run(tb, jtl string, props map[string]string, d time.Duration) error {
	k.kill()
	return k.recordingRunner.Run(tb, jtl, props, d)
}

func TestRunAll_ClerkSession_MintFailureIsErrorNotFailedAndNamesTheStage(t *testing.T) {
	m := newCSClerk(t)
	m.refuse = 401
	c := csConfig(t, m)
	rr := &recordingRunner{}
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "CS-M1", "status=202")
	writeScenario(t, scDir, "http-ingestion", "CS-M2", "status=202")
	res, err := RunAll(c, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", rr)
	if err != nil {
		t.Fatal(err)
	}
	if len(rr.props) != 0 {
		t.Errorf("nothing may be fired without a token: runner ran %d times", len(rr.props))
	}
	if res.Report.Summary.Failed != 0 || res.Report.Summary.Errored != 2 {
		t.Fatalf("a mint failure says nothing about the SUT: it is errored, never failed: %+v", res.Report.Summary)
	}
	for _, id := range []string{"CS-M1", "CS-M2"} {
		row, _ := res.Report.Find(id)
		if row == nil || row.Status != report.StatusError || row.Failure == nil ||
			!strings.Contains(row.Failure.Observed, "clerk_session: could not create a session: HTTP 401") {
			t.Errorf("%s: want an error row naming the mint failure, got %+v", id, row)
		}
	}
	if m.count("POST", "/v1/sessions") != 1 {
		t.Errorf("a refused create must not be hammered per scenario: %d calls", m.count("POST", "/v1/sessions"))
	}
}

// The refresh check must run before EACH request: a token with less than its floor left is replaced between two
// scenarios of the same run. The clock is the manager's own, moved by the runner.
func TestRunAll_ClerkSession_RefreshesBetweenScenariosWhenTheTokenIsNearExpiry(t *testing.T) {
	m := newCSClerk(t)
	m.life = 60 * time.Second
	var cmu sync.Mutex
	now := time.Now()
	clock := func() time.Time { cmu.Lock(); defer cmu.Unlock(); return now }
	prev := sessionOptions
	sessionOptions = []clerksession.Option{clerksession.WithClock(clock)}
	defer func() { sessionOptions = prev }()

	c := csConfig(t, m)
	rr := &advancingRunner{advance: func() { cmu.Lock(); now = now.Add(50 * time.Second); cmu.Unlock() }}
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "CS-T1", "status=202")
	writeScenario(t, scDir, "http-ingestion", "CS-T2", "status=202")
	if _, err := RunAll(c, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", rr); err != nil {
		t.Fatal(err)
	}
	if len(rr.props) != 2 || rr.props[0]["auth.header"] == rr.props[1]["auth.header"] {
		t.Fatal("the second scenario ran 50 s later on a 60 s token: it must carry a freshly minted one")
	}
	if m.nSessions != 1 || m.nTokens != 2 {
		t.Errorf("the session is re-used, the token is re-minted: sessions=%d tokens=%d", m.nSessions, m.nTokens)
	}
}

type advancingRunner struct {
	recordingRunner
	advance func()
}

func (a *advancingRunner) Run(tb, jtl string, props map[string]string, d time.Duration) error {
	err := a.recordingRunner.Run(tb, jtl, props, d)
	a.advance()
	return err
}

// ── redaction: a whole fake run, read back every way it leaves the process ────────────────────────────

func TestRunAll_ClerkSession_NeitherTheKeyNorTheTokenLeavesTheProcess(t *testing.T) {
	m := newCSClerk(t)
	c := csConfig(t, m)

	dir := t.TempDir()
	bin := writeFakeJMeter(t, filepath.Join(dir, "bin"))
	seen := filepath.Join(dir, "seen-by-jmeter.properties")
	t.Setenv("FAKE_JMETER_SEEN", seen)
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "RED-1", "status=202")
	writeScenario(t, scDir, "http-ingestion", "RED-2", "status=500") // a failing row: the failure text is the other leak surface
	results := filepath.Join(dir, "results")
	r := &LocalJMeterRunner{TemplatesDir: filepath.Join("..", "..", "templates"), JMeterBin: bin}
	rr, err := RunAll(c, scDir, results, "p", "", "", "", "", r)
	if err != nil {
		t.Fatal(err)
	}
	reportJSON, _ := json.Marshal(rr.Report)
	corrJSON, _ := json.Marshal(rr.CorrelationID)

	// the token JMeter really received — read from its side channel, which is the only legitimate place
	seenBytes, err := os.ReadFile(seen)
	if err != nil || !strings.Contains(string(seenBytes), "auth.header=Bearer eyJ") {
		t.Fatalf("JMeter never received a minted token (err=%v) — the run did not exercise clerk_session", err)
	}
	var jwt string
	for _, line := range strings.Split(string(seenBytes), "\n") {
		if strings.HasPrefix(line, "auth.header=Bearer ") {
			jwt = strings.TrimPrefix(line, "auth.header=Bearer ")
		}
	}
	if strings.Count(jwt, ".") != 2 {
		t.Fatalf("could not recover the minted token from the side channel")
	}

	needles := map[string]string{"secret key": csFakeSecret, "full JWT": jwt, "JWT signature part": strings.Split(jwt, ".")[2]}
	// leaks is the ONE read used for the run AND for the positive control below.
	leaks := func(hay string) []string {
		var hit []string
		for name, n := range needles {
			if strings.Contains(hay, n) {
				hit = append(hit, name)
			}
		}
		return hit
	}
	treeLeaks := func() []string {
		var hit []string
		for name, n := range needles {
			for _, p := range grepTree(t, dir, n) {
				if p != seen { // the test's own side channel is the one allowed copy
					hit = append(hit, name+" in "+filepath.Base(p))
				}
			}
		}
		return hit
	}

	// POSITIVE CONTROL — the same reads, against the same sources, must FIND a planted string. Without this a
	// green result here could be an empty haystack (an unwired log, a report that did not serialise).
	if len(logs.Bytes()) == 0 {
		t.Fatal("the log capture is empty: the run logged nothing, so 'no leak' proves nothing")
	}
	if len(reportJSON) < 50 || !strings.Contains(string(reportJSON), "RED-1") {
		t.Fatal("the report JSON does not carry the run's rows")
	}
	planted := "PLANTED " + csFakeSecret + " " + jwt
	if len(leaks(string(reportJSON)+planted)) != 3 || len(leaks(logs.String()+planted)) != 3 || len(leaks(string(corrJSON)+planted)) != 3 {
		t.Fatal("positive control: the leak read does not detect a planted secret in the report/log/relay haystacks")
	}
	plantFile := filepath.Join(results, "planted.txt")
	if err := os.WriteFile(plantFile, []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := treeLeaks(); len(got) != 3 {
		t.Fatalf("positive control: the tree read found %v, want all three needles in planted.txt", got)
	}
	_ = os.Remove(plantFile)

	// THE PROMISE.
	if h := leaks(logs.String()); len(h) > 0 {
		t.Errorf("the executor log leaked: %v", h)
	}
	if h := leaks(string(reportJSON)); len(h) > 0 {
		t.Errorf("the report JSON leaked: %v", h)
	}
	if h := leaks(string(corrJSON)); len(h) > 0 {
		t.Errorf("the relay-visible correlation map leaked: %v", h)
	}
	if h := treeLeaks(); len(h) > 0 {
		t.Errorf("a file under the run directory leaked: %v", h)
	}
}

// credentialScrub (the compare-mode output scrub and the HTTP-load failure text) must also remove the live
// token and the key, which the config alone does not hold.
func TestCredentialScrub_RemovesTheLiveSessionToken(t *testing.T) {
	m := newCSClerk(t)
	c, done := attachSessions(csConfig(t, m))
	defer done()
	s := &scenario.Scenario{ID: "CS-S", Layers: []string{"HTTP Ingestion"}, Trigger: scenario.Trigger{Method: "GET", URL: "${U}/x"}, Expect: []string{"status=200"}}
	tok := bearerOf(mustProps(t, c, s, "tr")["auth.header"])
	// built BEFORE the token existed in a real run: the scrub must read the live value when applied
	scrub := credentialScrub(c)
	out := scrub("body echoed " + tok + " and " + csFakeSecret)
	if strings.Contains(out, tok) || strings.Contains(out, csFakeSecret) {
		t.Fatalf("scrub left a secret: %q", out)
	}
}

type stubSessions struct{ vals []string }

func (stubSessions) Bearer(string) (string, error) { return "", nil }
func (s stubSessions) Values() []string            { return s.vals }

// The JWT shape is also removed by obsquery.RedactTokens, which would hide a scrub that ignored the session's
// values. A value that is NOT token-shaped isolates the Sessions.Values leg.
func TestCredentialScrub_AppliesTheSessionsOwnValuesNotOnlyTokenShape(t *testing.T) {
	c := httpConfig()
	scrub := credentialScrub(c)
	c.Sessions = stubSessions{vals: []string{"plain-live-value"}} // attached AFTER the closure was built
	if out := scrub("echo plain-live-value end"); strings.Contains(out, "plain-live-value") {
		t.Fatalf("the session's value survived the scrub: %q", out)
	}
}
