package scenario

import (
	"fmt"
	"strconv"
	"strings"
)

// httpload.go -- the `HTTP Load` layer and its `## LOAD` fields.
//
// It is the AMQP Load ramp's shape for an HTTP target: the same `## LOAD` field names where they mean the same
// thing (Steps, Step Duration Seconds, Ramp Seconds, Settle Seconds, Target P95 Ms, Max Error Rate, Must Sustain),
// the same closed-vocabulary `### Runnable`, the same required `**Target**`, the same S9 gate
// (load_allowed_targets) and the same per-step record (report.LoadStep). What differs is the generator (the
// http-ingestion template, one JMeter run per step, keep-alive users looping back to back) and the stop rule:
// an HTTP ramp STOPS at the first step that is not comfortable, so a target that has broken is not pushed harder.

// HTTPLoadLayer is the layer name. NEW on purpose, exactly like AMQPLoadLayer: an executor built before it
// does not list it, so it refuses the scenario by name before anything is sent. A `## LOAD` with `**Steps**`
// on `HTTP Ingestion` would have been read by an old executor as an unknown key or as the legacy profile.
const HTTPLoadLayer = "HTTP Load"

// HTTPLoadProfile is the validated `## LOAD` block of an `HTTP Load` scenario. Its own type, for the reason
// AMQPLoadProfile is: LoadProfile keeps its meaning on every other layer and is compared with `!=` by tests.
type HTTPLoadProfile struct {
	Steps               []int // users (JMeter threads) per step, strictly rising; one JMeter run each
	StepDurationSeconds int   // per step, INCLUDING the ramp: the hold
	RampSeconds         int   // excluded from the statistics
	SettleSeconds       int   // Go sleeps this between steps
	TargetP95Ms         int
	MaxErrorRate        float64
	MustSustain         int // 0 = none declared; else one of Steps
}

// The hard caps of the layer. They are the AMQP Load layer's own bounds (amqpload.go), so one rule reads the
// same on both: at most HTTPLoadMaxSteps steps, each of 1..HTTPLoadMaxUsers users, each held
// HTTPLoadMinStepSeconds..HTTPLoadMaxStepSeconds. Enforced when the scenario is written (buildHTTPLoad) and
// again when it runs (argus.httpLoadCapRefusal), and the operator's load_allowed_targets max_sessions caps
// the users of a target below that.
const (
	HTTPLoadMaxSteps       = 12
	HTTPLoadMaxUsers       = 2000
	HTTPLoadMinStepSeconds = 10
	HTTPLoadMaxStepSeconds = 3600
	HTTPLoadMaxSettle      = 600
)

const (
	httpDefaultRamp   = 10
	httpDefaultSettle = 30
)

// httpLoadLabels is the closed list of `## LOAD` keys of this layer, key -> the `**Bold Name**`.
var httpLoadLabels = []struct{ key, label string }{
	{"steps", "Steps"},
	{"step_duration", "Step Duration Seconds"},
	{"ramp", "Ramp Seconds"},
	{"settle", "Settle Seconds"},
	{"p95", "Target P95 Ms"},
	{"error_rate", "Max Error Rate"},
	{"sustain", "Must Sustain"},
}

// httpAMQPOnlyLabels are AMQP Load keys that have no meaning for an HTTP request: refused by name with that reason.
var httpAMQPOnlyLabels = []string{"Rate Per Session", "Message Size", "Queue Type", "Confirm", "Ack", "Prefetch", "Min Delivered Ratio"}

// HTTPLoadVocabulary is the closed list of `### Runnable` bullets of this layer. Both are ALWAYS enforced by
// the ramp's verdict (internal/httpload.Verdict): a step that recorded no request fails the ramp, and so does a
// ramp whose smallest step was not comfortable.
var HTTPLoadVocabulary = []string{"every step is measured", "the smallest step is comfortable"}

// IsHTTPLoad reports whether the scenario's primary layer is `HTTP Load`.
func IsHTTPLoad(s *Scenario) bool { return s != nil && PrimaryLayer(s) == HTTPLoadLayer }

func parseHTTPLoadRaw(body string) map[string]string {
	out := map[string]string{}
	for _, f := range httpLoadLabels {
		if m := loadFieldRe(f.label).FindStringSubmatch(body); m != nil {
			out[f.key] = strings.TrimSpace(m[1])
		}
	}
	return out
}

// buildHTTPLoad applies the defaults and the bounds. The ONE reader: Parse keeps the profile only when it
// returns no problem, Validate turns every problem into an error, so the two never disagree.
func buildHTTPLoad(raw map[string]string) (*HTTPLoadProfile, []amqpProblem) {
	var probs []amqpProblem
	bad := func(label, format string, a ...any) {
		probs = append(probs, amqpProblem{label, fmt.Sprintf(format, a...)})
	}
	p := &HTTPLoadProfile{}
	intField := func(key, label string, def, lo, hi int, required bool) int {
		v, ok := raw[key]
		if !ok {
			if required {
				bad(label, "is missing **%s**", label)
			}
			return def
		}
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
			bad(label, "**%s** is not a whole number: %q", label, v)
			return def
		case n < lo || n > hi:
			bad(label, "**%s** = %d is out of bounds [%d, %d]", label, n, lo, hi)
			return def
		}
		return n
	}

	if v, ok := raw["steps"]; !ok {
		bad("Steps", "is missing **Steps**")
	} else {
		parts := strings.Split(v, ",")
		if len(parts) > HTTPLoadMaxSteps {
			bad("Steps", "**Steps** has %d entries, at most %d are allowed", len(parts), HTTPLoadMaxSteps)
		}
		prev := 0
		for _, part := range parts {
			part = strings.TrimSpace(part)
			n, err := strconv.Atoi(part)
			switch {
			case err != nil:
				bad("Steps", "**Steps** entry %q is not a whole number of users", part)
			case n < 1 || n > HTTPLoadMaxUsers:
				bad("Steps", "**Steps** entry %d is out of bounds [1, %d] users", n, HTTPLoadMaxUsers)
			case n <= prev:
				// the ramp stops at the first step that is not comfortable, so the steps must RISE: a smaller step
				// after a larger one would be skipped by a stop that was meant for the larger load.
				bad("Steps", "**Steps** must rise: %d comes after %d", n, prev)
			default:
				p.Steps = append(p.Steps, n)
				prev = n
			}
		}
	}

	p.StepDurationSeconds = intField("step_duration", "Step Duration Seconds", 0, HTTPLoadMinStepSeconds, HTTPLoadMaxStepSeconds, true)
	rampDef, rampMax := httpDefaultRamp, HTTPLoadMaxStepSeconds
	if p.StepDurationSeconds > 0 {
		rampMax = p.StepDurationSeconds / 2
		if rampMax < rampDef {
			rampDef = rampMax // the default never trips its own bound
		}
	}
	p.RampSeconds = intField("ramp", "Ramp Seconds", rampDef, 0, rampMax, false)
	p.SettleSeconds = intField("settle", "Settle Seconds", httpDefaultSettle, 0, HTTPLoadMaxSettle, false)
	p.TargetP95Ms = intField("p95", "Target P95 Ms", 0, 1, 600000, true)

	if v, ok := raw["error_rate"]; !ok {
		bad("Max Error Rate", "is missing **Max Error Rate**")
	} else if f, err := strconv.ParseFloat(v, 64); err != nil {
		bad("Max Error Rate", "**Max Error Rate** is not a number: %q", v)
	} else if f < 0.000001 || f > 1 {
		bad("Max Error Rate", "**Max Error Rate** = %v is out of bounds [%v, %v]", f, 0.000001, 1)
	} else {
		p.MaxErrorRate = f
	}

	if v, ok := raw["sustain"]; ok {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
			bad("Must Sustain", "**Must Sustain** is not a whole number: %q", v)
		default:
			found := false
			for _, s := range p.Steps {
				found = found || s == n
			}
			if !found {
				bad("Must Sustain", "**Must Sustain** = %d is not one of **Steps** %v", n, p.Steps)
			} else {
				p.MustSustain = n
			}
		}
	}
	return p, probs
}

// HTTPLoadTopStep is the largest step of the profile (0 for nil).
func HTTPLoadTopStep(p *HTTPLoadProfile) int {
	top := 0
	if p != nil {
		for _, n := range p.Steps {
			if n > top {
				top = n
			}
		}
	}
	return top
}

// httpLoadErrors is every rule of the layer that is not a bound on one field.
func httpLoadErrors(s *Scenario, text string, metaLine int) []Error {
	if !IsHTTPLoad(s) && !contains(s.Layers, HTTPLoadLayer) {
		return nil
	}
	var errs []Error
	add := func(line int, format string, a ...any) {
		errs = append(errs, Error{Line: line, Message: fmt.Sprintf(format, a...)})
	}
	layerLine := lineContaining(text, "**Layer**", metaLine)
	if len(s.Layers) != 1 {
		add(layerLine, "`HTTP Load` must be the only layer of its scenario: it has its own `## LOAD` and its own verdict, so it cannot be chained with another layer")
		return errs
	}
	for _, t := range []string{ChainTag, MCPTag, UITag} {
		if contains(s.Tags, t) {
			add(lineContaining(text, "**Tags**", metaLine), "`HTTP Load` runs on the JMeter path, so its dispatch tag is `http`, not `%s`", t)
		}
	}
	if s.Target == "" {
		add(metaLine, "`**Target**` is required on an `HTTP Load` scenario: name the targets.http_targets entry to load. "+
			"The plain targets.http slot is never a load target")
	}
	if s.Trigger.Method == "" {
		add(s.sectionStart("TRIGGER", metaLine), "an `HTTP Load` scenario needs a `## TRIGGER` request line (METHOD `/path`): it is the request every user sends")
	}
	for _, b := range s.RunnableExpect() {
		if f, sec := DeclaredStatuses([]string{b}); f > 0 || sec > 0 {
			add(lineContaining(text, strings.TrimSpace(b), s.sectionStart("EXPECT", metaLine)),
				"`%s`: a `status=` bullet is not allowed on an `HTTP Load` scenario: a request counts as answered when its status is below 400, and the ramp judges the error rate and p95 per step. "+
					"The claims of this layer are: %s", bulletText(b), strings.Join(HTTPLoadVocabulary, " | "))
			continue
		}
		in := false
		for _, v := range HTTPLoadVocabulary {
			in = in || strings.EqualFold(strings.TrimSpace(bulletText(b)), v)
		}
		if !in {
			add(lineContaining(text, strings.TrimSpace(b), s.sectionStart("EXPECT", metaLine)),
				"`%s` is not in the HTTP Load vocabulary (%s) — a bullet nothing executes is refused rather than ignored",
				bulletText(b), strings.Join(HTTPLoadVocabulary, " | "))
		}
	}
	if !s.LoadDeclared {
		add(metaLine, "an `HTTP Load` scenario needs a `## LOAD` section: the profile IS the scenario")
		return errs
	}
	loadLine := s.sectionStart("LOAD", metaLine)
	body := sectionBodyOf(text, "LOAD")
	known := map[string]bool{}
	labels := make([]string, 0, len(httpLoadLabels))
	for _, f := range httpLoadLabels {
		known[f.label] = true
		labels = append(labels, f.label)
	}
	for _, m := range amqpBoldKeyRe.FindAllStringSubmatch(body, -1) {
		k := strings.TrimSpace(m[1])
		switch {
		case known[k]:
		case contains(amqpLegacyLabels, k):
			add(lineContaining(text, "**"+k+"**", loadLine), "## LOAD **%s** is a key of the other layers — the HTTP Load layer does not use Users or Duration Seconds: declare **Steps** (users per step) and **Step Duration Seconds**", k)
		case contains(httpAMQPOnlyLabels, k):
			add(lineContaining(text, "**"+k+"**", loadLine), "## LOAD **%s** is a key of the AMQP Load layer and has no meaning for an HTTP request — accepted: %s", k, strings.Join(labels, ", "))
		default:
			add(lineContaining(text, "**"+k+"**", loadLine), "## LOAD has an unknown key **%s** — accepted: %s", k, strings.Join(labels, ", "))
		}
	}
	_, probs := buildHTTPLoad(s.httpLoadRaw)
	for _, p := range probs {
		add(loadLine, "%s", p.text())
	}
	return errs
}
