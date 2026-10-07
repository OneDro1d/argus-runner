package dashlink

import (
	"strings"
	"testing"
	"time"
)

// dashlink_test.go -- (UI-2, A'1): the pure renderer of an operator-set dashboard link
// template. Every value is query-escaped, {from}/{to} are epoch MILLISECONDS, the six placeholders are
// the only braces allowed, and a template that could carry a credential is refused by name.

var (
	t0 = time.Date(2026, 9, 30, 12, 11, 3, 768_000_000, time.UTC)
	t1 = time.Date(2026, 9, 30, 12, 20, 0, 0, time.UTC)
)

func TestRender_Fixture(t *testing.T) {
	tmpl := "https://grafana.lab.example/d/msgbus?var-run={run_id}&var-inst={instance}&corr={correlation_id}&t={target}&from={from}&to={to}"
	v := Vars{
		RunID: "20260930T121103768", CorrelationID: "tr-20260930T121103768", Instance: "msgbus-homelab",
		From: t0.Add(-5 * time.Minute), To: t1.Add(5 * time.Minute),
	}
	got, err := Render(tmpl, v)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "https://grafana.lab.example/d/msgbus?var-run=20260930T121103768&var-inst=msgbus-homelab&corr=tr-20260930T121103768&t=&from=1790769963768&to=1790771100000"
	if got != want {
		t.Fatalf("Render =\n  %s\nwant\n  %s", got, want)
	}
	t.Logf("RENDERED template=%q -> %s", tmpl, got)
}

func TestRender_EveryValueIsQueryEscaped(t *testing.T) {
	got, err := Render("https://g.example/d/x?a={instance}&b={run_id}&c={target}&d={correlation_id}", Vars{
		Instance: "my inst&x=1", RunID: "r/1?#", Target: "a b", CorrelationID: "tr-é",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://g.example/d/x?a=my+inst%26x%3D1&b=r%2F1%3F%23&c=a+b&d=tr-%C3%A9"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if strings.Contains(got, "&x=1") {
		t.Errorf("a value smuggled a query parameter: %s", got)
	}
}

func TestRender_FromToAreEpochMilliseconds(t *testing.T) {
	got, err := Render("https://g.example/?f={from}&t={to}", Vars{From: time.UnixMilli(1790770263768), To: time.UnixMilli(1790771100000)})
	if err != nil || got != "https://g.example/?f=1790770263768&t=1790771100000" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestRender_RepeatedPlaceholderAndNoPlaceholders(t *testing.T) {
	got, err := Render("https://g.example/{run_id}/{run_id}", Vars{RunID: "r1"})
	if err != nil || got != "https://g.example/r1/r1" {
		t.Fatalf("got %q err %v", got, err)
	}
	got, err = Render("https://zabbix.example/zabbix.php?action=dashboard.view", Vars{})
	if err != nil || got != "https://zabbix.example/zabbix.php?action=dashboard.view" {
		t.Fatalf("a template with no placeholders must render as itself: %q %v", got, err)
	}
}

func TestRender_RefusesWhatValidateRefuses(t *testing.T) {
	if _, err := Render("https://u:p@g.example/{run_id}", Vars{}); err == nil {
		t.Error("Render accepted a template with userinfo")
	}
	if _, err := Render("https://g.example/{nope}", Vars{}); err == nil {
		t.Error("Render accepted an unknown placeholder")
	}
}

func TestValidate_Accepts(t *testing.T) {
	for _, ok := range []string{
		"https://grafana.lab.example/d/msgbus?var-run={run_id}&from={from}&to={to}",
		"http://zabbix.internal:8080/zabbix.php?action=dashboard.view&dashboardid=3",
		"https://app.datadoghq.eu/dashboard/abc?from_ts={from}&to_ts={to}&tpl_var_run={run_id}",
		"https://{instance}.example.com/x",
		"HTTPS://G.EXAMPLE/{target}",
	} {
		if err := Validate(ok); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", ok, err)
		}
	}
}

// The refusal texts are the contract an operator reads in `argus validate-config`: they name the key,
// the problem, and the way out.
func TestValidate_RefusalTexts(t *testing.T) {
	cases := []struct {
		name, tmpl string
		want       []string
	}{
		{"userinfo", "https://admin:hunter2@grafana.lab.example/d/x?run={run_id}",
			[]string{"credential (userinfo)"}},
		{"userinfo with only a user", "https://token@grafana.lab.example/d/x", []string{"credential (userinfo)"}},
		{"unknown placeholder", "https://g.example/d/x?run={runid}",
			[]string{"unknown placeholder {runid}", "{run_id} {correlation_id} {instance} {target} {from} {to}"}},
		{"empty braces", "https://g.example/d/{}", []string{"unknown placeholder {}"}},
		{"unbalanced open", "https://g.example/d/{run_id", []string{"unmatched"}},
		{"unbalanced close", "https://g.example/d/run_id}", []string{"unmatched"}},
		{"env var", "https://g.example/d/x?token=${GRAFANA_TOKEN}", []string{"${VAR}"}},
		{"env var shaped like a placeholder", "https://g.example/d/${run_id}", []string{"${VAR}"}},
		{"not absolute", "/d/x?run={run_id}", []string{"absolute http(s) URL"}},
		{"scheme", "ftp://g.example/x", []string{"absolute http(s) URL"}},
		{"javascript", "javascript:alert(1)", []string{"absolute http(s) URL"}},
		{"no host", "https:///d/x", []string{"absolute http(s) URL"}},
		{"empty", "   ", []string{"required"}},
		{"too long", "https://g.example/" + strings.Repeat("a", MaxLen), []string{"2048"}},
		{"placeholder in userinfo slot", "https://{run_id}@g.example/", []string{"credential (userinfo)"}},
		{"control char", "https://g.example/x\ny", []string{"control character"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.tmpl)
			if err == nil {
				t.Fatalf("Validate(%q) = nil, want a refusal", c.tmpl)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q does not contain %q", err.Error(), w)
				}
			}
			// the refusal must never echo a credential back
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the refusal echoes the password: %q", err.Error())
			}
		})
	}
}

func TestValidate_ExactlyAtTheLimitIsAccepted(t *testing.T) {
	base := "https://g.example/"
	if err := Validate(base + strings.Repeat("a", MaxLen-len(base))); err != nil {
		t.Errorf("a %d-byte template was refused: %v", MaxLen, err)
	}
	if err := Validate(base + strings.Repeat("a", MaxLen-len(base)+1)); err == nil {
		t.Errorf("a %d-byte template was accepted", MaxLen+1)
	}
}

func TestValidateLabel(t *testing.T) {
	if err := ValidateLabel("Open in Grafana"); err != nil {
		t.Errorf("a normal label was refused: %v", err)
	}
	if err := ValidateLabel(""); err != nil {
		t.Errorf("an empty label (use the default) was refused: %v", err)
	}
	if err := ValidateLabel(strings.Repeat("x", MaxLabelLen+1)); err == nil {
		t.Error("an over-long label was accepted")
	}
	if err := ValidateLabel("a\nb"); err == nil {
		t.Error("a label with a control character was accepted")
	}
}

func TestRunVars_WindowIsStartMinusFiveToFinishPlusFive(t *testing.T) {
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	started, finished := t0, t1
	v := RunVars("inst-a", "20260930T121103768", t0, &started, &finished, now)
	if !v.From.Equal(t0.Add(-5*time.Minute)) || !v.To.Equal(t1.Add(5*time.Minute)) {
		t.Fatalf("window = %s .. %s", v.From, v.To)
	}
	if v.CorrelationID != "tr-20260930T121103768" || v.RunID != "20260930T121103768" || v.Instance != "inst-a" || v.Target != "" {
		t.Errorf("vars = %+v", v)
	}
}

func TestRunVars_RunningUsesNow_AndMissingStartFallsBackToCreated(t *testing.T) {
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	started := t0
	v := RunVars("i", "r", t0, &started, nil, now)
	if !v.To.Equal(now) {
		t.Errorf("a running run's window must end at now: %s", v.To)
	}
	v = RunVars("i", "r", t0, nil, nil, now)
	if !v.From.Equal(t0.Add(-5 * time.Minute)) {
		t.Errorf("no started_at: window must start at created_at-5m, got %s", v.From)
	}
}

func TestGeneralVars_IsRunlessAndLast24Hours(t *testing.T) {
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	v := GeneralVars("inst-a", now)
	if v.RunID != "" || v.CorrelationID != "" || v.Target != "" || v.Instance != "inst-a" {
		t.Errorf("vars = %+v", v)
	}
	if !v.To.Equal(now) || !v.From.Equal(now.Add(-24*time.Hour)) {
		t.Errorf("window = %s .. %s", v.From, v.To)
	}
}
