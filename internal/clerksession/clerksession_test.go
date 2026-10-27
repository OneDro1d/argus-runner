package clerksession

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every secret in this file is an obvious fake. The mock below plays Clerk's Backend API; nothing here
// ever dials the real one.
const (
	fakeSecret = "sk_test_FAKEFAKEFAKEFAKEFAKE"
	fakeUser   = "user_FAKE0000000001"
)

// fakeJWT builds an unsigned three-part token whose payload carries iat/exp. The client never checks a
// signature (the SUT does), so the third part is a marker.
func fakeJWT(iat, exp time.Time, tag string) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." +
		enc(map[string]any{"iat": iat.Unix(), "exp": exp.Unix(), "tag": tag}) + ".FAKESIG" + tag
}

type rec struct {
	Method, Path, Auth, Body string
}

// mockClerk records every request and answers like the Backend API. mint decides the JWT of the Nth
// token request (1-based).
type mockClerk struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []rec

	sessionStatus int // 0 = 200
	tokenStatus   int
	revokeStatus  int
	noSessionID   bool
	mint          func(n int) string
	tokenCalls    int32
}

func newMock(t *testing.T) *mockClerk {
	m := &mockClerk{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.reqs = append(m.reqs, rec{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)})
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/sessions":
			if m.sessionStatus != 0 {
				w.WriteHeader(m.sessionStatus)
				fmt.Fprint(w, `{"errors":[{"code":"nope"}]}`)
				return
			}
			if m.noSessionID {
				fmt.Fprint(w, `{"object":"session"}`)
				return
			}
			fmt.Fprint(w, `{"id":"sess_FAKE1","object":"session","status":"active"}`)
		case strings.HasSuffix(r.URL.Path, "/revoke"):
			if m.revokeStatus != 0 {
				w.WriteHeader(m.revokeStatus)
				return
			}
			fmt.Fprint(w, `{"id":"sess_FAKE1","status":"revoked"}`)
		case strings.Contains(r.URL.Path, "/tokens"):
			n := int(atomic.AddInt32(&m.tokenCalls, 1))
			if m.tokenStatus != 0 {
				w.WriteHeader(m.tokenStatus)
				return
			}
			fmt.Fprintf(w, `{"object":"token","jwt":%q}`, m.mint(n))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockClerk) calls() []rec {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]rec(nil), m.reqs...)
}

// clock is a controllable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var t0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func newMgr(m *mockClerk, ck *clock, tmpl string) *Manager {
	return New(Params{APIURL: m.srv.URL, SecretKey: fakeSecret, UserID: fakeUser, Template: tmpl}, WithClock(ck.now))
}

func TestMint_PathsBodyAndSecretHeader(t *testing.T) {
	ck := &clock{t: t0}
	m := newMock(t)
	m.mint = func(n int) string { return fakeJWT(ck.now(), ck.now().Add(60*time.Second), "A") }
	tok, err := newMgr(m, ck, "").Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok != fakeJWT(t0, t0.Add(60*time.Second), "A") {
		t.Fatalf("token not returned verbatim")
	}
	got := m.calls()
	if len(got) != 2 {
		t.Fatalf("want 2 Clerk calls (create session, mint token), got %d: %+v", len(got), got)
	}
	if got[0].Method != "POST" || got[0].Path != "/v1/sessions" {
		t.Errorf("first call = %s %s", got[0].Method, got[0].Path)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(got[0].Body), &body); err != nil || body["user_id"] != fakeUser || len(body) != 1 {
		t.Errorf("create-session body = %q", got[0].Body)
	}
	if got[1].Method != "POST" || got[1].Path != "/v1/sessions/sess_FAKE1/tokens" {
		t.Errorf("second call = %s %s", got[1].Method, got[1].Path)
	}
	for i, c := range got {
		if c.Auth != "Bearer "+fakeSecret {
			t.Errorf("call %d did not carry the secret key as Bearer", i)
		}
	}
}

func TestMint_TemplatePath(t *testing.T) {
	ck := &clock{t: t0}
	m := newMock(t)
	m.mint = func(n int) string { return fakeJWT(ck.now(), ck.now().Add(time.Hour), "T") }
	if _, err := newMgr(m, ck, "argus-test").Token(); err != nil {
		t.Fatal(err)
	}
	got := m.calls()
	if got[1].Path != "/v1/sessions/sess_FAKE1/tokens/argus-test" {
		t.Errorf("template path = %q", got[1].Path)
	}
}

func TestMint_ReusesTokenWhileFresh(t *testing.T) {
	ck := &clock{t: t0}
	m := newMock(t)
	m.mint = func(n int) string { return fakeJWT(ck.now(), ck.now().Add(60*time.Second), fmt.Sprint(n)) }
	mg := newMgr(m, ck, "")
	a, _ := mg.Token()
	ck.advance(20 * time.Second) // 40 s of 60 left: more than 15 s and more than 25 %
	b, _ := mg.Token()
	if a != b || len(m.calls()) != 2 {
		t.Fatalf("a fresh token must be reused: %d calls", len(m.calls()))
	}
}

func TestRefresh_BeforeExpiry_ShortLived(t *testing.T) {
	ck := &clock{t: t0}
	m := newMock(t)
	m.mint = func(n int) string { return fakeJWT(ck.now(), ck.now().Add(60*time.Second), fmt.Sprint(n)) }
	mg := newMgr(m, ck, "")
	a, _ := mg.Token()
	ck.advance(46 * time.Second) // 14 s left: under the 15 s floor
	b, err := mg.Token()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("a token with 14 s left must be re-minted")
	}
	// the session is reused: one create, two token calls
	var creates int
	for _, c := range m.calls() {
		if c.Path == "/v1/sessions" {
			creates++
		}
	}
	if creates != 1 || atomic.LoadInt32(&m.tokenCalls) != 2 {
		t.Fatalf("creates=%d tokenCalls=%d", creates, m.tokenCalls)
	}
}

func TestRefresh_BeforeExpiry_QuarterOfLife(t *testing.T) {
	ck := &clock{t: t0}
	m := newMock(t)
	m.mint = func(n int) string { return fakeJWT(ck.now(), ck.now().Add(time.Hour), fmt.Sprint(n)) }
	mg := newMgr(m, ck, "tpl")
	a, _ := mg.Token()
	ck.advance(44 * time.Minute) // 16 min left: > 15 s, > 25 % (15 min)
	if b, _ := mg.Token(); b != a {
		t.Fatal("16 min of 60 left must not re-mint")
	}
	ck.advance(2 * time.Minute) // 14 min left: < 25 %
	if b, _ := mg.Token(); b == a {
		t.Fatal("under a quarter of life left must re-mint")
	}
}

func TestRefresh_ConcurrentCallersTriggerOneRefresh(t *testing.T) {
	ck := &clock{t: t0}
	m := newMock(t)
	m.mint = func(n int) string {
		if n > 1 {
			time.Sleep(50 * time.Millisecond) // widen the race window
		}
		return fakeJWT(ck.now(), ck.now().Add(60*time.Second), fmt.Sprint(n))
	}
	mg := newMgr(m, ck, "")
	if _, err := mg.Token(); err != nil {
		t.Fatal(err)
	}
	ck.advance(50 * time.Second) // stale for everyone
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mg.Token(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&m.tokenCalls); got != 2 {
		t.Fatalf("32 concurrent callers on a stale token must trigger exactly ONE refresh (2 token calls in all), got %d", got)
	}
}

func TestRevoke_PostsRevokeAndIsNoopWithoutSession(t *testing.T) {
	ck := &clock{t: t0}
	m := newMock(t)
	m.mint = func(n int) string { return fakeJWT(ck.now(), ck.now().Add(time.Minute), "R") }
	mg := newMgr(m, ck, "")
	if err := mg.Revoke(); err != nil || len(m.calls()) != 0 {
		t.Fatalf("revoke before any mint must be a no-op: %v %v", err, m.calls())
	}
	mg.Token()
	if err := mg.Revoke(); err != nil {
		t.Fatal(err)
	}
	last := m.calls()[len(m.calls())-1]
	if last.Method != "POST" || last.Path != "/v1/sessions/sess_FAKE1/revoke" || last.Auth != "Bearer "+fakeSecret {
		t.Errorf("revoke call = %+v", last)
	}
	n := len(m.calls())
	mg.Revoke() // a second revoke must not re-POST
	if len(m.calls()) != n {
		t.Error("a revoked session must not be revoked twice")
	}
}

func TestRevoke_FailureIsAnErrorWithoutSecrets(t *testing.T) {
	ck := &clock{t: t0}
	m := newMock(t)
	m.revokeStatus = 500
	m.mint = func(n int) string { return fakeJWT(ck.now(), ck.now().Add(time.Minute), "R") }
	mg := newMgr(m, ck, "")
	tok, _ := mg.Token()
	err := mg.Revoke()
	if err == nil {
		t.Fatal("a 500 on revoke must be reported to the caller (which logs it)")
	}
	if strings.Contains(err.Error(), fakeSecret) || strings.Contains(err.Error(), tok) {
		t.Fatalf("revoke error leaks a secret: %v", err)
	}
}

func TestRevoke_IsBounded(t *testing.T) {
	ck := &clock{t: t0}
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/sessions":
			fmt.Fprint(w, `{"id":"sess_FAKE1"}`)
		case strings.HasSuffix(r.URL.Path, "/revoke"):
			<-block
		default:
			fmt.Fprintf(w, `{"jwt":%q}`, fakeJWT(ck.now(), ck.now().Add(time.Minute), "B"))
		}
	}))
	defer srv.Close()
	defer close(block)
	mg := New(Params{APIURL: srv.URL, SecretKey: fakeSecret, UserID: fakeUser}, WithClock(ck.now), WithRevokeTimeout(150*time.Millisecond))
	mg.Token()
	start := time.Now()
	if err := mg.Revoke(); err == nil {
		t.Fatal("a hung revoke must time out with an error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("revoke was not bounded: %v", time.Since(start))
	}
}

func TestMintFailure_NamesTheStage(t *testing.T) {
	ck := &clock{t: t0}
	m := newMock(t)
	m.sessionStatus = 401
	_, err := newMgr(m, ck, "").Token()
	if err == nil || err.Error() != "clerk_session: could not create a session: HTTP 401" {
		t.Fatalf("got %v", err)
	}
	m2 := newMock(t)
	m2.tokenStatus = 404
	m2.mint = func(int) string { return "" }
	_, err = newMgr(m2, ck, "").Token()
	if err == nil || err.Error() != "clerk_session: could not mint a session token: HTTP 404" {
		t.Fatalf("got %v", err)
	}
	m3 := newMock(t)
	m3.noSessionID = true
	_, err = newMgr(m3, ck, "").Token()
	if err == nil || !strings.HasPrefix(err.Error(), "clerk_session: could not create a session:") {
		t.Fatalf("got %v", err)
	}
}

func TestMintFailure_NeverCarriesSecrets(t *testing.T) {
	ck := &clock{t: t0}
	// an unreachable API: the transport error must not echo the Authorization header or the key
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := New(Params{APIURL: url, SecretKey: fakeSecret, UserID: fakeUser}, WithClock(ck.now)).Token()
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), fakeSecret) || !strings.HasPrefix(err.Error(), "clerk_session: could not create a session:") {
		t.Fatalf("got %v", err)
	}
}

func TestMintFailure_SessionCreateIsStickyForTheRun(t *testing.T) {
	ck := &clock{t: t0}
	m := newMock(t)
	m.sessionStatus = 401
	mg := newMgr(m, ck, "")
	mg.Token()
	mg.Token()
	mg.Token()
	if n := len(m.calls()); n != 1 {
		t.Fatalf("a refused session create must not be hammered once per scenario: %d calls", n)
	}
}

func TestToken_WithoutExpIsRefused(t *testing.T) {
	ck := &clock{t: t0}
	m := newMock(t)
	m.mint = func(int) string { return "not-a-jwt" }
	_, err := newMgr(m, ck, "").Token()
	if err == nil || !strings.HasPrefix(err.Error(), "clerk_session: could not mint a session token:") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "not-a-jwt") {
		t.Fatal("the error must not echo the token")
	}
}

func TestValuesAndPrefix(t *testing.T) {
	ck := &clock{t: t0}
	m := newMock(t)
	m.mint = func(int) string { return fakeJWT(ck.now(), ck.now().Add(time.Minute), "V") }
	mg := newMgr(m, ck, "")
	tok, _ := mg.Token()
	vals := mg.Values()
	if len(vals) != 2 || vals[0] != fakeSecret || vals[1] != tok {
		t.Fatalf("Values() must hold the key and the live token for scrubbers: %d values", len(vals))
	}
	if p := Prefix(tok); len(p) != 6 || !strings.HasPrefix(tok, p) {
		t.Errorf("Prefix = %q", p)
	}
}
