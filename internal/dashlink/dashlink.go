// Package dashlink is the pure renderer of an operator-set dashboard link template (,
// UI-2, A'1). An environment's owner declares, in its argus-config, where a run's dashboard lives in ANY
// tool (Grafana, Zabbix, Datadog, an internal page):
//
//	observability:
//	  dashboard_link:
//	    template: "https://grafana.lab.example/d/msgbus?var-run={run_id}&from={from}&to={to}"
//	    label: "Open in Grafana"
//
// The control plane renders it per run from the ledger row, so the link works for every past run the
// moment a template is declared (no re-run). The package imports nothing from the rest of the module, so
// the executor (load-time validation), the control plane (renderer + validation of what an executor
// reported) and the store can all use it.
//
// Placeholders (the only braces allowed):
//
//	{run_id}          the run's id
//	{correlation_id}  at RUN level the run's correlation PREFIX tr-<run_id> (ids are tr-<run>-<scenario>-<hex>
//	                  and the control plane never holds the per-scenario suffix) -- use it as a prefix / filter
//	{instance}        the Argus instance id
//	{target}          the test target a run exercised ("" until per-target templates, UI-6)
//	{from}, {to}      EPOCH MILLISECONDS: started-5m .. finished+5m (to = now while running)
//
// Every value is url.QueryEscape'd. A template is a link shown to people, so it never carries a
// credential: userinfo and ${VAR} are refused, and a refusal never echoes the template back.
package dashlink

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxLen is the longest template accepted, in bytes (also CHECKed by migration 068).
	MaxLen = 2048
	// MaxLabelLen is the longest button label, in characters.
	MaxLabelLen = 60
	// DefaultLabel is the button text when the operator declares none.
	DefaultLabel = "Open dashboard"
	// Pad is how far either side of a run the {from}/{to} window is widened.
	Pad = 5 * time.Minute
)

// Placeholders are the six names a template may use, in the order the refusal lists them.
var Placeholders = []string{"run_id", "correlation_id", "instance", "target", "from", "to"}

// Vars are the values a template is rendered with.
type Vars struct {
	RunID, CorrelationID, Instance, Target string
	From, To                               time.Time
}

func isPlaceholder(n string) bool {
	for _, p := range Placeholders {
		if p == n {
			return true
		}
	}
	return false
}

func placeholderList() string {
	parts := make([]string, len(Placeholders))
	for i, p := range Placeholders {
		parts[i] = "{" + p + "}"
	}
	return strings.Join(parts, " ")
}

// Validate applies the load-time rules. The returned error starts with a verb phrase so a caller can
// prefix the key it came from ("observability.dashboard_link.template carries a credential ...").
// Nothing in an error repeats the template: a refused template may hold a password.
func Validate(tmpl string) error {
	if strings.TrimSpace(tmpl) == "" {
		return fmt.Errorf("is required")
	}
	if len(tmpl) > MaxLen {
		return fmt.Errorf("is %d bytes; at most %d are allowed", len(tmpl), MaxLen)
	}
	for _, r := range tmpl {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("contains a control character")
		}
	}
	if strings.Contains(tmpl, "${") {
		return fmt.Errorf("uses ${VAR}; a dashboard link is shown to people, never a credential, so environment variables are not expanded in it")
	}
	// the brace scan: every '{' must open a known placeholder that closes before the next '{'
	dummy := make([]byte, 0, len(tmpl))
	for i := 0; i < len(tmpl); i++ {
		switch tmpl[i] {
		case '{':
			end := strings.IndexByte(tmpl[i+1:], '}')
			if end < 0 || strings.IndexByte(tmpl[i+1:i+1+end], '{') >= 0 {
				return fmt.Errorf("has an unmatched brace; write each placeholder as {name} with both braces")
			}
			name := tmpl[i+1 : i+1+end]
			if !isPlaceholder(name) {
				return fmt.Errorf("uses unknown placeholder {%s}; the only placeholders are %s", name, placeholderList())
			}
			dummy = append(dummy, 'x')
			i += end + 1
		case '}':
			return fmt.Errorf("has an unmatched brace; write each placeholder as {name} with both braces")
		default:
			dummy = append(dummy, tmpl[i])
		}
	}
	u, err := url.Parse(string(dummy))
	if err != nil {
		return fmt.Errorf("must be an absolute http(s) URL with a host")
	}
	if u.User != nil {
		return fmt.Errorf("carries a credential (userinfo); remove it -- a dashboard link is shown to people, never a secret")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http(s) URL with a host")
	}
	return nil
}

// ValidateLabel checks the optional button text ("" = use DefaultLabel).
func ValidateLabel(label string) error {
	if n := utf8.RuneCountInString(label); n > MaxLabelLen {
		return fmt.Errorf("is %d characters; at most %d are allowed", n, MaxLabelLen)
	}
	for _, r := range label {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("contains a control character")
		}
	}
	return nil
}

func ms(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return strconv.FormatInt(t.UnixMilli(), 10)
}

// Render substitutes the placeholders of a VALID template. A template that fails Validate is refused
// here too, so a caller that forgets to validate still cannot render a userinfo link.
func Render(tmpl string, v Vars) (string, error) {
	if err := Validate(tmpl); err != nil {
		return "", fmt.Errorf("dashboard link template %w", err)
	}
	vals := map[string]string{
		"run_id": v.RunID, "correlation_id": v.CorrelationID, "instance": v.Instance, "target": v.Target,
	}
	var b strings.Builder
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '{' {
			b.WriteByte(tmpl[i])
			continue
		}
		end := strings.IndexByte(tmpl[i+1:], '}')
		name := tmpl[i+1 : i+1+end]
		switch name {
		case "from":
			b.WriteString(ms(v.From))
		case "to":
			b.WriteString(ms(v.To))
		default:
			b.WriteString(url.QueryEscape(vals[name]))
		}
		i += end + 1
	}
	return b.String(), nil
}

// RunVars are the values for ONE run: {correlation_id} is the run's correlation prefix, the window is
// started-5m .. finished+5m (created_at stands in when the run has no start; now while it is running).
func RunVars(instance, runID string, createdAt time.Time, startedAt, finishedAt *time.Time, now time.Time) Vars {
	from := createdAt
	if startedAt != nil && !startedAt.IsZero() {
		from = *startedAt
	}
	to := now
	if finishedAt != nil && !finishedAt.IsZero() {
		to = finishedAt.Add(Pad)
	}
	v := Vars{RunID: runID, Instance: instance, From: from.Add(-Pad), To: to}
	if runID != "" {
		v.CorrelationID = "tr-" + runID
	}
	return v
}

// GeneralVars are the run-less values for the environment's own link: {run_id} and the rest are empty,
// the window is the last 24 hours.
func GeneralVars(instance string, now time.Time) Vars {
	return Vars{Instance: instance, From: now.Add(-24 * time.Hour), To: now}
}
