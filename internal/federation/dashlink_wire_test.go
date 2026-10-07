package federation

import (
	"encoding/json"
	"strings"
	"testing"
)

// (UI-2): the dashboard link template rides register and every poll, THREE-VALUED:
// absent = not reported, "" = reported none, "x" = reported.

func sp(s string) *string { return &s }

func TestDashboardLinkWire_ThreeValues(t *testing.T) {
	b, _ := json.Marshal(PollRequest{RunnerVersion: "0.3.60"})
	if strings.Contains(string(b), "dashboard_link") {
		t.Errorf("not reported must be ABSENT on the wire: %s", b)
	}
	b, _ = json.Marshal(PollRequest{RunnerVersion: "0.3.60", DashboardLinkTemplate: sp(""), DashboardLinkLabel: sp("")})
	if !strings.Contains(string(b), `"dashboard_link_template":""`) || !strings.Contains(string(b), `"dashboard_link_label":""`) {
		t.Errorf("reported-none must be PRESENT as \"\": %s", b)
	}
	var back PollRequest
	if err := json.Unmarshal(b, &back); err != nil || back.DashboardLinkTemplate == nil || *back.DashboardLinkTemplate != "" {
		t.Fatalf("reported none did not survive the round trip: %v %+v", err, back)
	}
	b, _ = json.Marshal(RegisterRequest{InstanceID: "i", DashboardLinkTemplate: sp("https://g.example/{run_id}"), DashboardLinkLabel: sp("Open in Grafana")})
	var reg RegisterRequest
	if err := json.Unmarshal(b, &reg); err != nil || reg.DashboardLinkTemplate == nil || *reg.DashboardLinkTemplate != "https://g.example/{run_id}" || *reg.DashboardLinkLabel != "Open in Grafana" {
		t.Fatalf("register round trip: %v %+v", err, reg)
	}
}

func TestDashboardLinkWire_OldPeers(t *testing.T) {
	var req PollRequest
	if err := json.Unmarshal([]byte(`{"runner_version":"0.3.39","protocol_version":"`+ProtocolVersion+`"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.DashboardLinkTemplate != nil || req.DashboardLinkLabel != nil {
		t.Fatalf("an old executor's poll must decode to NOT REPORTED: %+v", req)
	}
	// a new executor's payload decodes into the old shape (no DisallowUnknownFields)
	raw, _ := json.Marshal(PollRequest{RunnerVersion: "0.3.60", DashboardLinkTemplate: sp("https://g.example/")})
	var older struct {
		RunnerVersion string `json:"runner_version"`
	}
	if err := json.Unmarshal(raw, &older); err != nil || older.RunnerVersion != "0.3.60" {
		t.Fatalf("old shape failed: %v %+v", err, older)
	}
}
