package scenario

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// amqpload.go -- the `AMQP Load` layer and its `## LOAD` fields.

// AMQPLoadLayer is the layer name. It is NEW on purpose (design §3.4): an executor built before this
// layer existed does not list it in TargetKind, so it refuses the scenario by name before anything is
// sent; a tag on `Message Flow` would have run on the old template.
const AMQPLoadLayer = "AMQP Load"

// AMQPLoadProfile is the validated `## LOAD` block of an `AMQP Load` scenario. It is a NEW type on
// Scenario and NOT more fields on LoadProfile: LoadProfile is compared with `!=` by tests (a slice
// would not compile) and the five legacy fields keep their meaning on every other layer.
type AMQPLoadProfile struct {
	Steps               []int // session counts, one JMeter run each
	StepDurationSeconds int   // per step, INCLUDING the ramp
	RampSeconds         int   // excluded from the statistics
	SettleSeconds       int   // Go sleeps this between steps
	RatePerSession      float64
	MessageSize         int
	QueueType           string // classic | quorum
	Confirm             string // off | each | batch:<n>
	Ack                 string // manual | auto
	Prefetch            int
	TargetP95Ms         int
	MaxErrorRate        float64
	MinDeliveredRatio   float64
	MustSustain         int // 0 = none declared; else one of Steps
}

// The defaults of design §5.2 (D-1..D-13). One constant each so a product answer flips ONE line.
const (
	amqpDefaultRamp         = 10
	amqpDefaultSettle       = 30
	amqpDefaultRate         = 1.0
	amqpDefaultSize         = 1000
	amqpDefaultQueueType    = "classic"
	amqpDefaultConfirm      = "each" // D-7: the sampler's own default stays `off`, the PROFILE's is `each`
	amqpDefaultAck          = "manual"
	amqpDefaultPrefetch     = 100
	amqpDefaultMinDelivered = 0.95
)

// AMQPLoadGeneratorCeilingPerS is the offered rate (max Steps x Rate Per Session, msg/s) above which the
// validator WARNS that the executor pod, not the broker, may be what gets measured (Q-M3: the pod is
// 1 CPU / 1.5 GiB). It is a warning and never an error: no hard cap beyond `max_sessions`.
const AMQPLoadGeneratorCeilingPerS = 1000.0

// amqpLoadLabels is the closed list of `## LOAD` keys of this layer, key -> the `**Bold Name**`.
var amqpLoadLabels = []struct{ key, label string }{
	{"steps", "Steps"},
	{"step_duration", "Step Duration Seconds"},
	{"ramp", "Ramp Seconds"},
	{"settle", "Settle Seconds"},
	{"rate", "Rate Per Session"},
	{"size", "Message Size"},
	{"queue_type", "Queue Type"},
	{"confirm", "Confirm"},
	{"ack", "Ack"},
	{"prefetch", "Prefetch"},
	{"p95", "Target P95 Ms"},
	{"error_rate", "Max Error Rate"},
	{"delivered", "Min Delivered Ratio"},
	{"sustain", "Must Sustain"},
}

// amqpLegacyLabels are the legacy `## LOAD` keys this layer does not use (the other three legacy keys
// are shared by name and meaning).
var amqpLegacyLabels = []string{"Users", "Duration Seconds"}

// AMQPLoadVocabulary is the closed list of `### Runnable` bullets of this layer. Both are ALWAYS
// enforced by the verdict rule (a blocked broker or a step that could not be read fails the ramp), so
// declaring them states the claim and costs nothing; anything else would be a claim nothing executes.
var AMQPLoadVocabulary = []string{"broker is not blocked", "every step is measured"}

func isAMQPLoad(s *Scenario) bool { return s != nil && PrimaryLayer(s) == AMQPLoadLayer }

func parseAMQPLoadRaw(body string) map[string]string {
	out := map[string]string{}
	for _, f := range amqpLoadLabels {
		if m := loadFieldRe(f.label).FindStringSubmatch(body); m != nil {
			out[f.key] = strings.TrimSpace(m[1])
		}
	}
	return out
}

type amqpProblem struct{ label, msg string }

func (p amqpProblem) text() string { return "## LOAD " + p.msg }

// buildAMQPLoad applies the defaults and the bounds. It is the ONE reader: Parse keeps the profile only
// when it returns no problem, Validate turns every problem into a line-level error, so the two can never
// disagree about what the block means.
func buildAMQPLoad(raw map[string]string) (*AMQPLoadProfile, []amqpProblem) {
	var probs []amqpProblem
	bad := func(label, format string, a ...any) {
		probs = append(probs, amqpProblem{label, fmt.Sprintf(format, a...)})
	}
	p := &AMQPLoadProfile{}

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
	floatField := func(key, label string, def, lo, hi float64, required bool) float64 {
		v, ok := raw[key]
		if !ok {
			if required {
				bad(label, "is missing **%s**", label)
			}
			return def
		}
		f, err := strconv.ParseFloat(v, 64)
		switch {
		case err != nil:
			bad(label, "**%s** is not a number: %q", label, v)
			return def
		case f < lo || f > hi:
			bad(label, "**%s** = %v is out of bounds [%v, %v]", label, f, lo, hi)
			return def
		}
		return f
	}

	// Steps
	if v, ok := raw["steps"]; !ok {
		bad("Steps", "is missing **Steps**")
	} else {
		parts := strings.Split(v, ",")
		if len(parts) > 12 {
			bad("Steps", "**Steps** has %d entries, at most 12 are allowed", len(parts))
		}
		for _, part := range parts {
			part = strings.TrimSpace(part)
			n, err := strconv.Atoi(part)
			switch {
			case err != nil:
				bad("Steps", "**Steps** entry %q is not a whole number of sessions", part)
			case n < 1 || n > 2000:
				bad("Steps", "**Steps** entry %d is out of bounds [1, 2000] sessions", n)
			default:
				p.Steps = append(p.Steps, n)
			}
		}
	}

	p.StepDurationSeconds = intField("step_duration", "Step Duration Seconds", 0, 10, 3600, true)
	rampDef := amqpDefaultRamp
	if p.StepDurationSeconds > 0 && p.StepDurationSeconds/2 < rampDef {
		rampDef = p.StepDurationSeconds / 2 // the default never trips its own bound
	}
	rampMax := 3600
	if p.StepDurationSeconds > 0 {
		rampMax = p.StepDurationSeconds / 2
	}
	p.RampSeconds = intField("ramp", "Ramp Seconds", rampDef, 0, rampMax, false)
	p.SettleSeconds = intField("settle", "Settle Seconds", amqpDefaultSettle, 0, 600, false)
	p.RatePerSession = floatField("rate", "Rate Per Session", amqpDefaultRate, 0.1, 100, false)
	p.MessageSize = intField("size", "Message Size", amqpDefaultSize, 1, 1048576, false)

	p.QueueType = amqpDefaultQueueType
	if v, ok := raw["queue_type"]; ok {
		if v = strings.ToLower(v); v == "classic" || v == "quorum" {
			p.QueueType = v
		} else {
			bad("Queue Type", "**Queue Type** must be classic or quorum, got %q", v)
		}
	}
	p.Confirm = amqpDefaultConfirm
	if v, ok := raw["confirm"]; ok {
		v = strings.ToLower(v)
		if n, isBatch := strings.CutPrefix(v, "batch:"); isBatch {
			if k, err := strconv.Atoi(n); err == nil && k >= 2 && k <= 1000 {
				p.Confirm = v
			} else {
				bad("Confirm", "**Confirm** batch:<n> needs n between 2 and 1000, got %q", v)
			}
		} else if v == "off" || v == "each" {
			p.Confirm = v
		} else {
			bad("Confirm", "**Confirm** must be off, each or batch:<n>, got %q", v)
		}
	}
	p.Ack = amqpDefaultAck
	if v, ok := raw["ack"]; ok {
		if v = strings.ToLower(v); v == "manual" || v == "auto" {
			p.Ack = v
		} else {
			bad("Ack", "**Ack** must be manual or auto, got %q", v)
		}
	}
	p.Prefetch = intField("prefetch", "Prefetch", amqpDefaultPrefetch, 1, 10000, false)
	p.TargetP95Ms = intField("p95", "Target P95 Ms", 0, 1, 600000, true)
	p.MaxErrorRate = floatField("error_rate", "Max Error Rate", 0, 0.000001, 1, true)
	p.MinDeliveredRatio = floatField("delivered", "Min Delivered Ratio", amqpDefaultMinDelivered, 0.5, 1, false)

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

var amqpBoldKeyRe = regexp.MustCompile(`(?m)^\s*[-*]?\s*\*\*([^*]+)\*\*\s*:`)

// amqpLoadErrors is every rule of the layer that is not a bound on one field.
func amqpLoadErrors(s *Scenario, text string, metaLine int) []Error {
	if !isAMQPLoad(s) && !contains(s.Layers, AMQPLoadLayer) {
		return nil
	}
	var errs []Error
	add := func(line int, format string, a ...any) {
		errs = append(errs, Error{Line: line, Message: fmt.Sprintf(format, a...)})
	}
	layerLine := lineContaining(text, "**Layer**", metaLine)

	if len(s.Layers) != 1 {
		add(layerLine, "`AMQP Load` must be the only layer of its scenario: it has its own template, its own `## LOAD` and its own verdict, so it cannot be chained with another layer")
		return errs
	}
	for _, t := range []string{ChainTag, MCPTag, UITag} {
		if contains(s.Tags, t) {
			add(lineContaining(text, "**Tags**", metaLine), "`AMQP Load` runs on the JMeter path, so its dispatch tag is `http`, not `%s`", t)
		}
	}
	if s.Target == "" {
		add(metaLine, "`**Target**` is required on an `AMQP Load` scenario: name the targets.message_broker_targets entry to load. "+
			"The plain targets.message_broker slot is never a load target")
	}
	for _, b := range s.RunnableExpect() {
		if f, sec := DeclaredStatuses([]string{b}); f > 0 || sec > 0 {
			add(lineContaining(text, strings.TrimSpace(b), s.sectionStart("EXPECT", metaLine)),
				"`%s`: a `status=` bullet is not allowed on an `AMQP Load` scenario — there is no HTTP response to compare. "+
					"The claims of this layer are: %s", bulletText(b), strings.Join(AMQPLoadVocabulary, " | "))
			continue
		}
		in := false
		for _, v := range AMQPLoadVocabulary {
			in = in || strings.EqualFold(strings.TrimSpace(bulletText(b)), v)
		}
		if !in {
			add(lineContaining(text, strings.TrimSpace(b), s.sectionStart("EXPECT", metaLine)),
				"`%s` is not in the AMQP Load vocabulary (%s) — a bullet nothing executes is refused rather than ignored",
				bulletText(b), strings.Join(AMQPLoadVocabulary, " | "))
		}
	}

	if !s.LoadDeclared {
		add(metaLine, "an `AMQP Load` scenario needs a `## LOAD` section: the profile IS the scenario")
		return errs
	}
	loadLine := s.sectionStart("LOAD", metaLine)
	body := sectionBodyOf(text, "LOAD")
	known := map[string]bool{}
	for _, f := range amqpLoadLabels {
		known[f.label] = true
	}
	for _, m := range amqpBoldKeyRe.FindAllStringSubmatch(body, -1) {
		k := strings.TrimSpace(m[1])
		switch {
		case known[k]:
		case contains(amqpLegacyLabels, k):
			add(lineContaining(text, "**"+k+"**", loadLine), "## LOAD **%s** is a key of the other layers — the AMQP Load layer does not use Users or Duration Seconds: declare **Steps** and **Step Duration Seconds**", k)
		default:
			labels := make([]string, 0, len(amqpLoadLabels))
			for _, f := range amqpLoadLabels {
				labels = append(labels, f.label)
			}
			add(lineContaining(text, "**"+k+"**", loadLine), "## LOAD has an unknown key **%s** — accepted: %s", k, strings.Join(labels, ", "))
		}
	}
	_, probs := buildAMQPLoad(s.amqpLoadRaw)
	for _, p := range probs {
		add(loadLine, "%s", p.text())
	}
	return errs
}

// amqpLoadWarnings are the advisory lints of the layer.
func amqpLoadWarnings(s *Scenario) []string {
	if !isAMQPLoad(s) {
		return nil
	}
	p, _ := buildAMQPLoad(s.amqpLoadRaw)
	if p == nil || len(p.Steps) == 0 {
		return nil
	}
	top := 0
	for _, n := range p.Steps {
		if n > top {
			top = n
		}
	}
	if offered := float64(top) * p.RatePerSession; offered > AMQPLoadGeneratorCeilingPerS {
		return []string{fmt.Sprintf("## LOAD asks for %.0f msg/s at its top step (%d sessions x %v msg/s): above %.0f msg/s the executor pod, not the broker, may be what is measured. "+
			"The run flags such a step `generator_limited` and never calls it comfortable; raise the executor's resources or lower Rate Per Session",
			offered, top, p.RatePerSession, AMQPLoadGeneratorCeilingPerS)}
	}
	return nil
}
