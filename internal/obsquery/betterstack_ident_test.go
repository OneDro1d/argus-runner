package obsquery

import (
	"strings"
	"testing"
	"time"
)

// obsquery re-checks every identifier it builds into the SQL text, even though config.Load already
// refused bad ones: a BetterStack value can be constructed without going through config. An unsafe
// team id or source slug must drop that source before any request is made, and an unsafe field
// path must never reach the SQL text.
func TestBetterStack_UnsafeIdentifier_NeverReachesARequest(t *testing.T) {
	hostile := "x') UNION SELECT 1 --"
	cases := []struct {
		name    string
		team    string
		sources map[string]string
	}{
		{"unsafe team id", hostile, map[string]string{"accounting": "svc"}},
		{"unsafe source slug", "123456", map[string]string{hostile: "svc"}},
	}
	for _, tc := range cases {
		f, ts := newBSFakeServer(t, nil)
		b := &BetterStack{QueryURL: ts.URL, TeamID: tc.team, Sources: tc.sources}
		logs := b.Logs("tr-20260902T134305716-FX-X-00000001", "1h", time.Now())
		ts.Close()
		if len(f.mu) != 0 {
			t.Errorf("%s: a request was sent with an unchecked identifier: %s", tc.name, f.mu[0].sql)
		}
		if logs.Available {
			t.Errorf("%s: no source was safe to query, yet Logs reported available", tc.name)
		}
	}

	f, ts := newBSFakeServer(t, nil)
	defer ts.Close()
	b := &BetterStack{QueryURL: ts.URL, TeamID: "123456", Sources: map[string]string{"accounting": "svc"},
		CorrelationFields: []string{hostile}, SagaEventField: hostile}
	b.Sagas("tr-20260902T134305716-FX-X-00000001", "1h", time.Now())
	req := mustFindReq(t, f)
	if strings.Contains(req.sql, "UNION") {
		t.Errorf("an unsafe field path reached the SQL text: %s", req.sql)
	}
}
