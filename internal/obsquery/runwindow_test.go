package obsquery

import (
	"strings"
	"testing"
)

// The deep link's time range has now been wrong in three distinct ways, so all three are pinned
// here. Each was reported by the owner from a screenshot of an empty or nonsensical dashboard.
//
//  1. NO range at all — the link set var-current_run only, and the dashboard's own default is
//     now-1h, so any run older than an hour opened with empty Saga and Logs panels while Loki held
//     464 lines and 113 saga lines for it.
//  2. An ABSOLUTE right edge (runStart+2h) — a `to` in the FUTURE. Every Prometheus panel here is an
//     INSTANT query evaluated at `to`, and two hours past the last sample is far outside
//     Prometheus's 5-minute staleness horizon, so they all read "No data" while the Loki RANGE panel
//     beside them rendered fine.
//  3. An ABSOLUTE left edge — correct, but it displayed as "2026-07-23 17:25:00 to a few seconds
//     ago", an arbitrary-looking period rather than a window anyone recognises. Owner call: use the
//     familiar relative range.

func TestDashboardURL_usesTheRelativeLastHourWindow(t *testing.T) {
	u := DashboardURL("http://localhost:3000", "social-k3d", "social", "", "20260723T153000822")
	if !strings.Contains(u, "&from=now-1h") {
		t.Errorf("url %q must use the relative left edge now-1h", u)
	}
	if !strings.Contains(u, "&to=now") {
		t.Errorf("url %q must pin the right edge to now", u)
	}
}

// Regression guard for defect 2 AND 3 at once: neither edge may be an absolute epoch stamp. Digits
// after from=/to= are exactly what a reintroduced absolute range would look like.
func TestDashboardURL_neitherEdgeIsAnAbsoluteStamp(t *testing.T) {
	u := DashboardURL("http://localhost:3000", "social-k3d", "social", "", "20260723T153000822")
	for _, key := range []string{"&from=", "&to="} {
		i := strings.Index(u, key)
		if i < 0 {
			t.Fatalf("url %q missing %s", u, key)
		}
		val := u[i+len(key):]
		if end := strings.IndexByte(val, '&'); end >= 0 {
			val = val[:end]
		}
		if val == "" || (val[0] >= '0' && val[0] <= '9') {
			t.Errorf("%s%s is an absolute stamp; the range must stay relative", key, val)
		}
	}
}

// The range belongs to a RUN. Without a run there is nothing to scope, and forcing a window would
// override whatever the operator had set on the dashboard.
func TestDashboardURL_omitsTimeRangeWhenNoRunID(t *testing.T) {
	u := DashboardURL("http://localhost:3000", "orderservice-compose", "order-service", "tr-x-Y-1", "")
	if strings.Contains(u, "&from=") || strings.Contains(u, "&to=") {
		t.Errorf("url %q should carry no time range without a run id", u)
	}
}

// An unparseable run id is still a run id: it is passed through to the variable, and the window
// still applies. (The previous cut derived the window BY PARSING the id, so a malformed id silently
// dropped the range; nothing parses it any more.)
func TestDashboardURL_windowSurvivesAnUnparseableRunID(t *testing.T) {
	u := DashboardURL("http://localhost:3000", "orderservice-compose", "order-service", "", "not-a-run-id")
	if !strings.Contains(u, "var-current_run=not-a-run-id") {
		t.Errorf("url %q dropped the run variable", u)
	}
	if !strings.Contains(u, "&from=now-1h&to=now") {
		t.Errorf("url %q should still carry the relative window", u)
	}
}
