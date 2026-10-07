package argus

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// jmeterBinForTest is the JMeter this test drives for real: ARGUS_TEST_JMETER (a path), else `jmeter` on
// PATH. "" = none, and the test SKIPS (a skipped run proves nothing and says so).
func jmeterBinForTest() string {
	if p := os.Getenv("ARGUS_TEST_JMETER"); p != "" {
		return p
	}
	if p, err := exec.LookPath("jmeter"); err == nil {
		return p
	}
	return ""
}

// against REAL JMeter, a `## LOAD` check (5 users, 2 s) opens at most one connection per
// user, and the same template without the keepalive property opens about one per request.
func TestRealJMeter_LoadUsersKeepTheirConnection(t *testing.T) {
	bin := jmeterBinForTest()
	if bin == "" {
		t.Skip("no JMeter: set ARGUS_TEST_JMETER or put jmeter on PATH")
	}
	var conns, reqs int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&reqs, 1)
		_, _ = w.Write([]byte("{}"))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt64(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: srv.URL}
	body := "- **Users**: 5\n- **Ramp Seconds**: 0\n- **Duration Seconds**: 2\n- **Target P95 Ms**: 5000\n- **Max Error Rate**: 0.5"
	props, err := DeriveProps(c, minimalLoadScenario(t, body), "tr-t")
	if err != nil {
		t.Fatal(err)
	}
	tpl, _ := filepath.Abs("../../templates")
	r := &LocalJMeterRunner{TemplatesDir: tpl, JMeterBin: bin}
	run := func(p map[string]string) (int64, int64) {
		atomic.StoreInt64(&conns, 0)
		atomic.StoreInt64(&reqs, 0)
		jtl := filepath.Join(t.TempDir(), "run.jtl")
		if err := r.Run("http-ingestion", jtl, p, 2*time.Minute); err != nil {
			t.Fatalf("jmeter: %v", err)
		}
		return atomic.LoadInt64(&conns), atomic.LoadInt64(&reqs)
	}

	n, q := run(props)
	t.Logf("with http.keepalive=%s: %d connections for %d requests", props["http.keepalive"], n, q)
	if q < 20 || n > 5 {
		t.Errorf("%d connections for %d requests, want <= 5 connections (one per user)", n, q)
	}

	delete(props, "http.keepalive")
	n, q = run(props)
	t.Logf("without it: %d connections for %d requests", n, q)
	if q < 20 || n < q/2 {
		t.Errorf("control: %d connections for %d requests, want about one per request", n, q)
	}
}

// The reviewer measured external-delivery at 11,437 connections for 11,437 requests while it did not
// read http.keepalive. Its trigger loops locally, so it is proven here with real JMeter. The other
// templates (database-state, message-flow, saga-presence) stop after one loop locally when their
// JDBC/broker sampler fails: they are proven by TestHTTPTemplatesReadTheKeepAliveProperty alone.
func TestRealJMeter_ExternalDeliveryLoadUsersKeepTheirConnection(t *testing.T) {
	bin := jmeterBinForTest()
	if bin == "" {
		t.Skip("no JMeter: set ARGUS_TEST_JMETER or put jmeter on PATH")
	}
	const users = 5
	var conns, reqs int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&reqs, 1)
		_, _ = w.Write([]byte("{}"))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt64(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: srv.URL}
	body := "- **Users**: 5\n- **Ramp Seconds**: 0\n- **Duration Seconds**: 3\n- **Target P95 Ms**: 5000\n- **Max Error Rate**: 0.5"
	props, err := DeriveProps(c, minimalLoadScenario(t, body), "tr-t")
	if err != nil {
		t.Fatal(err)
	}
	tpl, _ := filepath.Abs("../../templates")
	r := &LocalJMeterRunner{TemplatesDir: tpl, JMeterBin: bin}
	jtl := filepath.Join(t.TempDir(), "run.jtl")
	if err := r.Run("external-delivery", jtl, props, 2*time.Minute); err != nil {
		t.Logf("jmeter: %v", err)
	}
	n, q := atomic.LoadInt64(&conns), atomic.LoadInt64(&reqs)
	t.Logf("external-delivery with http.keepalive=%s: %d connections for %d requests", props["http.keepalive"], n, q)
	if q <= users {
		t.Fatalf("only %d requests reached the server: the run did not loop, so it proves nothing", q)
	}
	if n > users {
		t.Errorf("%d connections for %d requests, want <= %d (one per user)", n, q, users)
	}
}
