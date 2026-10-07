package main

// AC-D25 `argus cloud-enroll` — drives the actual command against a fake control plane that combines
// the refresh grant (/oauth/token) with the enrollment mint/revoke endpoints, exercising the AC-D24
// session mechanism (sessionForCommand/Session.Do) exactly as a real multi-instance onboard would.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/onboard"
)

// enrollFakeJWT is a bare unsigned JWT carrying only exp — enough for onboard's jwtExp/nearExpiry.
func enrollFakeJWT(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(exp.Unix(), 10) + `}`))
	return header + "." + payload + ".sig"
}

// enrollCP is a fake control plane serving the refresh grant plus AC-D25's enrollment endpoints. It can
// be told (expireAfterOK) to auto-invalidate the currently valid access token the moment the Nth mint
// succeeds — standing in for "the access token expires between two mints" without needing to fake
// Session's use of time.Now(): the very next /api/enrollments call with the old token gets a 401, and
// the reactive refresh-and-retry path (Session.Do) is what has to recover it. All fields are guarded by
// mu; every handler takes it, so counts() taking it too is enough — no separate atomics needed.
type enrollCP struct {
	*httptest.Server
	mu             sync.Mutex
	validAccess    string
	currentRefresh string
	refreshCalls   int
	mintAttempts   int
	mintOK         int
	revokeCalls    []string
	nextMintID     int
	expireAfterOK  int
}

func newEnrollCP(t *testing.T, initialAccess, initialRefresh string) *enrollCP {
	t.Helper()
	f := &enrollCP{validAccess: initialAccess, currentRefresh: initialRefresh}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.refreshCalls++
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != f.currentRefresh {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		newAccess := enrollFakeJWT(time.Now().Add(time.Hour))
		f.validAccess = newAccess
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": newAccess, "token_type": "Bearer", "expires_in": 3600,
			// no refresh_token in the response — endpoints.go:315's "no rotation in M3"; the OLD
			// refresh token must keep working, which is exactly what f.currentRefresh unchanged tests.
		})
	})
	mux.HandleFunc("/api/enrollments/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/enrollments/")
		f.mu.Lock()
		f.revokeCalls = append(f.revokeCalls, id)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"revoked": true})
	})
	mux.HandleFunc("/api/enrollments", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.mintAttempts++
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" || got != f.validAccess {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.nextMintID++
		f.mintOK++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enrollment_token": "enrt_secret_" + strconv.Itoa(f.nextMintID),
			"enrollment_id":    "enr_" + strconv.Itoa(f.nextMintID),
			"workspace":        "ws_test",
			"expires_in":       900,
		})
		if f.expireAfterOK > 0 && f.mintOK == f.expireAfterOK {
			f.validAccess = "__expired__"
		}
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

func (f *enrollCP) counts() (refresh, mintAttempts, mintOK int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls, f.mintAttempts, f.mintOK
}

// seedEnrollSession points ARGUS_SESSION_FILE at a fresh file outside any git tree and writes a
// session the enroll command will load through sessionForCommand → onboard.LoadSession(client, "").
func seedEnrollSession(t *testing.T, cpURL, access, refresh string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.json")
	t.Setenv(onboard.SessionEnvOverride, path)
	if err := onboard.SaveSession(path, onboard.SessionData{
		ControlPlane: cpURL, AccessToken: access, RefreshToken: refresh,
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return path
}

// 🚩 THE REQUIRED AC-D25 SCENARIO, driven through the ACTUAL CLI COMMAND: two instances, the access
// token expires BETWEEN them (the fake CP invalidates it the moment the first mint succeeds), and
// cloud-enroll must still mint both — with EXACTLY ONE refresh in between, never a batch refresh up
// front and never one per instance. Also proves: files land at 0600, the dir is tightened to 0700, and
// the minted tokens never reach stdout.
func TestCloudEnroll_TwoInstances_AccessTokenExpiresBetween_OneRefresh(t *testing.T) {
	cp := newEnrollCP(t, "", "rt_1")
	access := enrollFakeJWT(time.Now().Add(time.Hour)) // far from the 60s proactive margin — only
	cp.validAccess = access                            // the REACTIVE 401 path can catch this expiry
	cp.expireAfterOK = 1                               // expires right after alpha's mint succeeds
	seedEnrollSession(t, cp.URL, access, "rt_1")

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { // pre-existing 0755: proves ensureTokenDir0700 tightens it
		t.Fatal(err)
	}

	cf := &commonFlags{controlPlane: cp.URL, instance: "alpha,beta", tokenDir: dir}
	var rc int
	rawOut := captureEmitRaw(t, func() { rc = cmdCloudEnroll(cf) })
	if rc != exitOK {
		t.Fatalf("cloud-enroll exit=%d: %s", rc, rawOut)
	}
	if strings.Contains(string(rawOut), "enrt_secret") {
		t.Fatalf("an enrollment token leaked to stdout: %s", rawOut)
	}

	var doc struct {
		Enrolled []map[string]any `json:"enrolled"`
	}
	if err := json.Unmarshal(rawOut, &doc); err != nil {
		t.Fatalf("cloud-enroll did not emit parseable JSON: %v\n%s", err, rawOut)
	}
	if len(doc.Enrolled) != 2 {
		t.Fatalf("enrolled %d instances, want 2: %s", len(doc.Enrolled), rawOut)
	}
	for i, id := range []string{"alpha", "beta"} {
		if doc.Enrolled[i]["instance_id"] != id {
			t.Errorf("enrolled[%d].instance_id = %v, want %q", i, doc.Enrolled[i]["instance_id"], id)
		}
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat token dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("token dir mode = %o, want 0700 (a pre-existing 0755 dir must be tightened)", di.Mode().Perm())
	}
	for _, id := range []string{"alpha", "beta"} {
		fp := filepath.Join(dir, id+".enrollment")
		fi, err := os.Stat(fp)
		if err != nil {
			t.Fatalf("stat %s: %v", fp, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 0600", fp, fi.Mode().Perm())
		}
		blob, err := os.ReadFile(fp)
		if err != nil {
			t.Fatalf("read %s: %v", fp, err)
		}
		if !strings.HasPrefix(string(blob), "enrt_secret_") {
			t.Errorf("%s does not contain the minted enrollment token", fp)
		}
	}
	refreshN, _, mintOK := cp.counts()
	if mintOK != 2 {
		t.Errorf("the control plane accepted %d mints, want 2", mintOK)
	}
	if refreshN != 1 {
		t.Errorf("the control plane saw %d refresh calls, want exactly 1 (one refresh BETWEEN the two mints, not per-instance)", refreshN)
	}
}

// `cloud-enroll --revoke <id>` — DELETE /api/enrollments/{id}, no --token-dir needed, nothing minted.
func TestCloudEnroll_Revoke(t *testing.T) {
	cp := newEnrollCP(t, "", "rt_1")
	access := enrollFakeJWT(time.Now().Add(time.Hour))
	cp.validAccess = access
	seedEnrollSession(t, cp.URL, access, "rt_1")

	cf := &commonFlags{controlPlane: cp.URL, revokeID: "enr_7"}
	var rc int
	out := captureEmit(t, func() { rc = cmdCloudEnroll(cf) })
	if rc != exitOK {
		t.Fatalf("cloud-enroll --revoke exit=%d: %v", rc, out)
	}
	if out["revoked"] != true || out["enrollment_id"] != "enr_7" {
		t.Errorf("unexpected result: %v", out)
	}
	cp.mu.Lock()
	calls := append([]string(nil), cp.revokeCalls...)
	cp.mu.Unlock()
	if len(calls) != 1 || calls[0] != "enr_7" {
		t.Errorf("the control plane saw revoke calls %v, want exactly [enr_7]", calls)
	}
	_, mintAttempts, _ := cp.counts()
	if mintAttempts != 0 {
		t.Errorf("--revoke must not mint anything, saw %d mint attempts", mintAttempts)
	}
}

// No --token-dir and no --revoke is a usage error, not a silent no-op.
func TestCloudEnroll_NoTokenDirNoRevoke_IsUsageError(t *testing.T) {
	cp := newEnrollCP(t, "", "rt_1")
	access := enrollFakeJWT(time.Now().Add(time.Hour))
	cp.validAccess = access
	seedEnrollSession(t, cp.URL, access, "rt_1")

	cf := &commonFlags{controlPlane: cp.URL, instance: "alpha"}
	var rc int
	out := captureEmit(t, func() { rc = cmdCloudEnroll(cf) })
	if rc != exitUsage {
		t.Fatalf("exit=%d, want exitUsage: %v", rc, out)
	}
}
