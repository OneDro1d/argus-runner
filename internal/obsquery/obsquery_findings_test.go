package obsquery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Patch #4: the deep link scopes the dashboard to THIS SUT's project (var-project) so the
// Loki panels can't bleed a prior SUT's logs; an empty project omits it (back-compat).
func TestDashboardURL_ScopesProject(t *testing.T) {
	u := DashboardURL("http://localhost:3000", "local", "social", "tr-20260902T134305716-FX-X-00000001", "")
	for _, want := range []string{"var-argus_instance=local", "var-project=social", "var-correlation_id=tr-20260902T134305716-FX-X-00000001"} {
		if !strings.Contains(u, want) {
			t.Errorf("DashboardURL missing %q: %s", want, u)
		}
	}
	if strings.Contains(DashboardURL("http://x", "local", "", "", ""), "var-project") {
		t.Error("an empty project must omit var-project (back-compat)")
	}
}

// E1 (R12): the deep link scopes the dashboard to the RUN via var-current_run (the dashboard's "Run ID"
// variable has NO default, so without it the run-scoped panels render empty). Empty runID omits it.
func TestDashboardURL_ScopesRun(t *testing.T) {
	u := DashboardURL("http://g:3000", "local", "social", "tr-20260715T120000-A-1-x", "20260715T120000")
	if !strings.Contains(u, "var-current_run=20260715T120000") {
		t.Errorf("DashboardURL must set var-current_run: %s", u)
	}
	if strings.Contains(DashboardURL("http://g", "local", "", "", ""), "var-current_run") {
		t.Error("an empty runID must omit var-current_run")
	}
}

// DF-15: a log line whose body carries no `ts` must fall back to Loki's authoritative
// entry timestamp (v[0]); a saga-step line with no `msg` must surface its step_name.
func TestParseLogLine_TSFallbackAndMsg(t *testing.T) {
	dl := &Loki{} // canonical defaults (correlation_id / event_type=saga)
	// human-readable line: body has msg, no ts → TS must come from the Loki entry ts.
	hr := dl.parseLogLine(lokiEntry{TS: "1700000000000000000", Line: `{"msg":"order accepted","level":"info","service":"order-api","correlation_id":"tr-20260902T134305716-FX-X-00000001"}`})
	if hr.Msg != "order accepted" {
		t.Fatalf("msg = %q; want 'order accepted'", hr.Msg)
	}
	if hr.TS == "" {
		t.Fatalf("TS must fall back to Loki entry ts when body has none")
	}
	if want := time.Unix(0, 1700000000000000000).UTC().Format(time.RFC3339); hr.TS != want {
		t.Fatalf("TS = %q; want entry-ts %q", hr.TS, want)
	}
	// saga-step line: body has ts, no msg → Msg surfaces step_name; TS keeps the body ts.
	sg := dl.parseLogLine(lokiEntry{TS: "1700000001000000000", Line: `{"ts":"2026-06-22T10:00:00Z","event_type":"saga","step_name":"persist","service":"order-processor"}`})
	if sg.TS != "2026-06-22T10:00:00Z" {
		t.Fatalf("body ts must win: %q", sg.TS)
	}
	if sg.Msg != "persist" {
		t.Fatalf("saga line msg should surface step_name, got %q", sg.Msg)
	}
}

// CHANGE-2: a SUT that logs its correlation id under a non-canonical field name (Social →
// request_id) and tags sagas under a non-canonical field must thread to the bundle when the
// fields are DECLARED — and must NOT when they are not (proving the name is really read).
func TestParseLogLine_ConfiguredFields(t *testing.T) {
	socialLine := lokiEntry{TS: "1700000000000000000", Line: `{"request_id":"tr-20260902T134305716-FX-SOC-00000006","level":"info","service":"social-gateway","kind":"audit","step_name":"post"}`}

	// declared → the correlation value normalizes under the canonical key; the saga tag matches.
	declared := &Loki{CorrelationField: "request_id", SagaEventField: "kind", SagaEventValue: "audit"}
	got := declared.parseLogLine(socialLine)
	if got.Fields["correlation_id"] != "tr-20260902T134305716-FX-SOC-00000006" {
		t.Errorf("declared correlation_field=request_id must surface the value, got Fields=%v", got.Fields)
	}
	if got.Msg != "post" { // no msg → saga tag matched (kind=audit) → step_name surfaces
		t.Errorf("declared saga_event_field=kind/value=audit must surface step_name, got Msg=%q", got.Msg)
	}

	// default fields → request_id is NOT correlation_id, kind=audit is NOT event_type=saga.
	def := &Loki{}
	g2 := def.parseLogLine(socialLine)
	if g2.Fields["correlation_id"] != "" {
		t.Errorf("with default correlation_field, request_id must NOT be read as correlation_id, got %v", g2.Fields)
	}
	if g2.Msg != "" {
		t.Errorf("with default saga field, a kind=audit line is not a saga → no step_name msg, got %q", g2.Msg)
	}
}

// CHANGE-2: Sagas() must select saga lines by the DECLARED saga field/value (httptest Loki).
func TestSagas_HonorsConfiguredSagaField(t *testing.T) {
	line := `{"kind":"saga_event","saga_id":"s1","step":1,"step_name":"persist","step_status":"ok","ts":"2026-06-22T10:00:00Z","request_id":"tr-20260902T134305716-FX-X-00000001"}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"result": []map[string]any{
			{"values": [][2]string{{strconv.FormatInt(time.Now().UnixNano(), 10), line}}},
		}}})
	}))
	defer ts.Close()

	// declared saga field/value → the line is recognized as a saga step.
	declared := &Loki{BaseURL: ts.URL, SagaEventField: "kind", SagaEventValue: "saga_event"}
	sg := declared.Sagas("tr-20260902T134305716-FX-X-00000001", "30m", time.Time{})
	if !sg.Available || len(sg.Timeline) != 1 || sg.Timeline[0].StepName != "persist" {
		t.Fatalf("declared saga field must select the saga line: available=%v timeline=%+v note=%q", sg.Available, sg.Timeline, sg.Note)
	}

	// default fields → kind=saga_event is NOT event_type=saga → no saga steps (honest empty).
	def := &Loki{BaseURL: ts.URL}
	sg2 := def.Sagas("tr-20260902T134305716-FX-X-00000001", "30m", time.Time{})
	if len(sg2.Timeline) != 0 {
		t.Errorf("with default saga field, a kind=saga_event line must NOT be read as a saga, got %+v", sg2.Timeline)
	}
}

// DF-09: the query window must be anchorable to the run. With a zero anchor it is
// [now-window, now]; with an anchor it is [anchor-window, anchor+window] so a run older
// than the window still returns its saga/logs. Default window is large (1h).
func TestComputeWindow_AnchorVsNow(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	// zero anchor → relative to now
	s, e := computeWindow("10m", time.Time{}, now)
	if !e.Equal(now) || !s.Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("zero-anchor window = [%v,%v]; want [now-10m, now]", s, e)
	}
	// anchored → band around the run timestamp (so a 40-min-old run is still covered)
	run := now.Add(-40 * time.Minute)
	s, e = computeWindow("30m", run, now)
	if s.After(run) || e.Before(run) {
		t.Fatalf("anchored window [%v,%v] must contain the run time %v", s, e, run)
	}
	// invalid/empty window → large default (1h), not 10m
	s, e = computeWindow("", time.Time{}, now)
	if d := e.Sub(s); d < time.Hour {
		t.Fatalf("default window = %v; want >= 1h", d)
	}
}

// R5-1 (2026-07-22, owner remark "no data in Grafana for saga and loki"): in MULTI-INSTANCE mode the
// shared Grafana holds ONE dashboard PER INSTANCE (argus-overview-<id>), each wired to THAT instance's
// OWN Loki datasource (loki-<id> -> argus-inst-<id>-loki-1). The GENERIC `argus-overview` dashboard
// resolves datasource uid `loki` -> http://loki:3100, and every per-instance Loki joins argus-obs-net
// under the SAME alias `loki` -> docker DNS ROUND-ROBINS across all of them, so its Loki panels showed a
// random instance's logs or none. A registered instance MUST deep-link to its OWN dashboard.
func TestDashboardURL_PerInstanceDashboardUID(t *testing.T) {
	u := DashboardURL("http://g:3000", "orderservice-compose-v1", "order-service", "", "20260722T110438")
	if !strings.Contains(u, "/d/argus-overview-orderservice-compose-v1?") {
		t.Errorf("a registered instance must deep-link to ITS OWN dashboard (argus-overview-<id>): %s", u)
	}
	// the standalone/direct path has no per-instance dashboard — it keeps the generic one.
	for _, inst := range []string{"local", ""} {
		g := DashboardURL("http://g:3000", inst, "", "", "")
		if !strings.Contains(g, "/d/argus-overview?") {
			t.Errorf("instance %q must keep the generic dashboard: %s", inst, g)
		}
	}
}
