package scenario

import (
	"strings"
	"testing"
)

// amqpLoadSample is the scenario the docs show: the new layer, its own `## LOAD` fields, a TRIGGER that
// VR12-T1 requires and the layer never sends, and the closed load vocabulary under ### Runnable.
const amqpLoadSample = `# Scenario: AMQP load ramp, 10 to 1000 sessions

## Metadata
- **ID**: AMQPLOAD-001
- **Layer**: AMQP Load
- **Tags**: http, load
- **Target**: load-lab

## TRIGGER
POST ` + "`/amqp-load`" + `

## EXPECT
### Runnable
- broker is not blocked
- every step is measured

### Non-runnable
- the ramp reports the largest comfortable step as the tested limit

## LOAD
- **Steps**: 10, 100, 500, 1000
- **Step Duration Seconds**: 90
- **Ramp Seconds**: 15
- **Settle Seconds**: 30
- **Rate Per Session**: 1
- **Message Size**: 1000
- **Queue Type**: quorum
- **Confirm**: each
- **Ack**: manual
- **Prefetch**: 100
- **Target P95 Ms**: 250
- **Max Error Rate**: 0.01
- **Min Delivered Ratio**: 0.95
- **Must Sustain**: 500

## TIMEOUT
30s

## CLEANUP
N/A — the sampler deletes its per-session queues itself and x-expires reaps a killed run.
`

func amqpLoadWith(t *testing.T, oldLine, newLine string) string {
	t.Helper()
	if !strings.Contains(amqpLoadSample, oldLine) {
		t.Fatalf("fixture drifted: %q is not in the sample", oldLine)
	}
	return strings.Replace(amqpLoadSample, oldLine, newLine, 1)
}

func errsText(errs []Error) string {
	var b strings.Builder
	for _, e := range errs {
		b.WriteString(e.String() + "\n")
	}
	return b.String()
}

func TestAMQPLoad_ParsesItsOwnLoadFields_AndLeavesTheLegacyLoadNil(t *testing.T) {
	s := Parse(amqpLoadSample)
	if s.Load != nil {
		t.Fatalf("an AMQP Load scenario must leave Scenario.Load nil (every legacy consumer reads it), got %+v", s.Load)
	}
	p := s.AMQPLoad
	if p == nil {
		t.Fatal("AMQPLoad is nil for a fully declared profile")
	}
	if got := p.Steps; len(got) != 4 || got[0] != 10 || got[3] != 1000 {
		t.Errorf("Steps = %v", got)
	}
	if p.StepDurationSeconds != 90 || p.RampSeconds != 15 || p.SettleSeconds != 30 ||
		p.RatePerSession != 1 || p.MessageSize != 1000 || p.QueueType != "quorum" || p.Confirm != "each" ||
		p.Ack != "manual" || p.Prefetch != 100 || p.TargetP95Ms != 250 || p.MaxErrorRate != 0.01 ||
		p.MinDeliveredRatio != 0.95 || p.MustSustain != 500 {
		t.Errorf("profile = %+v", *p)
	}
}

func TestAMQPLoad_DefaultsApplyWhenOptionalFieldsAreAbsent(t *testing.T) {
	text := amqpLoadWith(t, "- **Ramp Seconds**: 15\n- **Settle Seconds**: 30\n- **Rate Per Session**: 1\n- **Message Size**: 1000\n- **Queue Type**: quorum\n- **Confirm**: each\n- **Ack**: manual\n- **Prefetch**: 100\n", "")
	text = strings.Replace(text, "- **Min Delivered Ratio**: 0.95\n- **Must Sustain**: 500\n", "", 1)
	s, errs := Validate(text)
	if len(errs) != 0 {
		t.Fatalf("a profile with only the required fields must validate: %s", errsText(errs))
	}
	p := s.AMQPLoad
	if p == nil {
		t.Fatal("AMQPLoad nil")
	}
	if p.RampSeconds != 10 || p.SettleSeconds != 30 || p.RatePerSession != 1 || p.MessageSize != 1000 ||
		p.QueueType != "classic" || p.Confirm != "each" || p.Ack != "manual" || p.Prefetch != 100 ||
		p.MinDeliveredRatio != 0.95 || p.MustSustain != 0 {
		t.Errorf("defaults D-1..D-13 not applied: %+v", *p)
	}
}

func TestAMQPLoad_TheSampleValidatesClean(t *testing.T) {
	if _, errs := Validate(amqpLoadSample); len(errs) != 0 {
		t.Fatalf("the documented sample is refused:\n%s", errsText(errs))
	}
}

func TestAMQPLoad_RefusedByName(t *testing.T) {
	for _, c := range []struct {
		name, old, new, want string
	}{
		{"no target", "- **Target**: load-lab\n", "", "**Target**"},
		{"a status bullet", "- broker is not blocked\n", "- status=200\n", "status="},
		{"a bullet outside the vocabulary", "- every step is measured\n", "- p99 under 5 ms\n", "vocabulary"},
		{"no steps", "- **Steps**: 10, 100, 500, 1000\n", "", "**Steps**"},
		{"13 steps", "- **Steps**: 10, 100, 500, 1000\n", "- **Steps**: 1,2,3,4,5,6,7,8,9,10,11,12,13\n", "**Steps**"},
		{"a step of 2001 sessions", "- **Steps**: 10, 100, 500, 1000\n", "- **Steps**: 10, 2001\n", "2001"},
		{"a step that is not a number", "- **Steps**: 10, 100, 500, 1000\n", "- **Steps**: 10, many\n", "many"},
		{"step duration 9", "- **Step Duration Seconds**: 90\n", "- **Step Duration Seconds**: 9\n", "**Step Duration Seconds**"},
		{"ramp over half the step", "- **Ramp Seconds**: 15\n", "- **Ramp Seconds**: 50\n", "**Ramp Seconds**"},
		{"rate below 0.1", "- **Rate Per Session**: 1\n", "- **Rate Per Session**: 0.05\n", "**Rate Per Session**"},
		{"message size 0", "- **Message Size**: 1000\n", "- **Message Size**: 0\n", "**Message Size**"},
		{"queue type fast", "- **Queue Type**: quorum\n", "- **Queue Type**: fast\n", "**Queue Type**"},
		{"confirm sometimes", "- **Confirm**: each\n", "- **Confirm**: sometimes\n", "**Confirm**"},
		{"confirm batch:1", "- **Confirm**: each\n", "- **Confirm**: batch:1\n", "**Confirm**"},
		{"ack maybe", "- **Ack**: manual\n", "- **Ack**: maybe\n", "**Ack**"},
		{"prefetch 0", "- **Prefetch**: 100\n", "- **Prefetch**: 0\n", "**Prefetch**"},
		{"no p95 target", "- **Target P95 Ms**: 250\n", "", "**Target P95 Ms**"},
		{"no error rate", "- **Max Error Rate**: 0.01\n", "", "**Max Error Rate**"},
		{"delivered ratio 0.4", "- **Min Delivered Ratio**: 0.95\n", "- **Min Delivered Ratio**: 0.4\n", "**Min Delivered Ratio**"},
		{"must sustain not a step", "- **Must Sustain**: 500\n", "- **Must Sustain**: 501\n", "**Must Sustain**"},
		{"a legacy Users label", "- **Steps**: 10, 100, 500, 1000\n", "- **Steps**: 10, 100, 500, 1000\n- **Users**: 5\n", "does not use"},
		{"a legacy Duration Seconds label", "- **Steps**: 10, 100, 500, 1000\n", "- **Steps**: 10, 100, 500, 1000\n- **Duration Seconds**: 60\n", "does not use"},
		{"a mistyped label", "- **Settle Seconds**: 30\n", "- **Settle Second**: 30\n", "Settle Second"},
		{"a chain tag", "- **Tags**: http, load", "- **Tags**: chain, load", "http"},
		{"another layer chained in", "- **Layer**: AMQP Load", "- **Layer**: Message Flow -> AMQP Load", "only layer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, errs := Validate(amqpLoadWith(t, c.old, c.new))
			if len(errs) == 0 {
				t.Fatalf("accepted")
			}
			if !strings.Contains(errsText(errs), c.want) {
				t.Errorf("no error names %q:\n%s", c.want, errsText(errs))
			}
		})
	}
}

func TestAMQPLoad_NoLoadSectionIsRefused(t *testing.T) {
	i := strings.Index(amqpLoadSample, "## LOAD")
	j := strings.Index(amqpLoadSample, "## TIMEOUT")
	_, errs := Validate(amqpLoadSample[:i] + amqpLoadSample[j:])
	if !strings.Contains(errsText(errs), "## LOAD") {
		t.Fatalf("a layer that is only a load profile accepted no ## LOAD:\n%s", errsText(errs))
	}
}

// The rule the legacy layers keep: LoadProfile is unchanged and a Message Flow scenario's ## LOAD is the
// five fields. The two parsers must not leak into each other.
func TestAMQPLoad_DoesNotChangeTheLegacyLoadBlock(t *testing.T) {
	legacy := strings.Replace(amqpLoadSample, "AMQP Load", "Message Flow", 1)
	i := strings.Index(legacy, "## LOAD")
	j := strings.Index(legacy, "## TIMEOUT")
	legacy = legacy[:i] + "## LOAD\n- **Users**: 5\n- **Ramp Seconds**: 1\n- **Duration Seconds**: 10\n- **Target P95 Ms**: 100\n- **Max Error Rate**: 0.1\n\n" + legacy[j:]
	s := Parse(legacy)
	if s.Load == nil || s.Load.Users != 5 {
		t.Fatalf("legacy load lost: %+v", s.Load)
	}
	if s.AMQPLoad != nil {
		t.Errorf("a Message Flow scenario must not grow an AMQPLoad: %+v", s.AMQPLoad)
	}
}

func TestAMQPLoad_TargetKindIsTheMessageBroker(t *testing.T) {
	k, err := TargetKind(Parse(amqpLoadSample))
	if err != nil || k != "message_broker" {
		t.Fatalf("TargetKind = %q, %v", k, err)
	}
}

// §3.4 refusal 2: an executor that does not know the layer judges it by response code and finds no
// `status=` bullet. That barrier only holds if THIS build never lets a well-formed AMQP Load scenario
// carry one, and if the layer is not response-code judged here.
func TestAMQPLoad_IsAContentLayer_SoNoStatusIsDemanded(t *testing.T) {
	if JudgedByResponseCode(Parse(amqpLoadSample)) {
		t.Fatal("AMQP Load must not be judged by response code")
	}
	if !IsContentLayer(AMQPLoadLayer) {
		t.Fatal("AMQP Load must be a content layer (its verdict is the ramp's)")
	}
}

// Old executors: the layer name must not be one an older build lists. This is the in-tree half of the
// proof; scripts/amqp-load-old-executor-check.sh runs the real v0.3.50 tree.
func TestAMQPLoad_LayerNameIsNewToEveryOldExecutor(t *testing.T) {
	old := []string{"HTTP Ingestion", "Message Flow", "Database State", "External Delivery", "Error Path", "Rate Limiting", "Permissions", "Web UI"}
	for _, l := range old {
		if l == AMQPLoadLayer {
			t.Fatalf("%q is already a layer of the pinned old executor", l)
		}
	}
	found := false
	for _, l := range CanonicalLayers {
		if l == AMQPLoadLayer {
			found = true
		}
	}
	if !found {
		t.Fatal("this build does not list the layer")
	}
}

func TestAMQPLoad_CapacityCeilingIsAWarningNotAnError(t *testing.T) {
	text := amqpLoadWith(t, "- **Steps**: 10, 100, 500, 1000\n", "- **Steps**: 10, 2000\n")
	text = strings.Replace(text, "- **Must Sustain**: 500\n", "", 1)
	text = strings.Replace(text, "- **Rate Per Session**: 1\n", "- **Rate Per Session**: 5\n", 1)
	if _, errs := Validate(text); len(errs) != 0 {
		t.Fatalf("a capacity ceiling must never refuse: %s", errsText(errs))
	}
	var hit bool
	for _, w := range Warnings(text) {
		if strings.Contains(w, "generator") {
			hit = true
		}
	}
	if !hit {
		t.Errorf("no generator-capacity warning: %v", Warnings(text))
	}
}
