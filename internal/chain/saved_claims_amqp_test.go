package chain

import (
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// An amqp consume step binds ${saved.<var>} in its body claims, numeric thresholds included; the
// enforced list shows the claim as written.
func TestAMQPStep_ConsumeBodyClaimBindsSavedValues(t *testing.T) {
	spec := AMQPSpec{Op: "consume", URLEnv: "X", Queue: "q", Wait: time.Second}
	run := func(body string, vars map[string]string, asserts ...mcp.BodyAssert) (string, string, []string) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte(body)}
		dial, _ := dialerFor(f)
		st := AMQPStep("take", spec, scenario.AMQPStepWant{Broker: accepts, Body: asserts}, dial)
		if nerr := st.Needs(vars); nerr != nil {
			return "not-measured", nerr.Error(), nil
		}
		sr := st.Run("cid", vars)
		return sr.Status, sr.Observed, sr.AssertionsEnforced
	}
	gt := mcp.BodyAssert{Field: "count", Op: mcp.BodyGTOp, Value: "${saved.n}"}

	if status, obs, enf := run(`{"count": 12}`, map[string]string{"n": "11"}, gt); status != "passed" ||
		!strings.Contains(strings.Join(enf, ";"), "field count > ${saved.n}") {
		t.Errorf("12 > 11 must pass with the claim shown as written: %s %s %v", status, obs, enf)
	}
	if status, _, _ := run(`{"count": 12}`, map[string]string{"n": "12"}, gt); status != "failed" {
		t.Errorf("12 > 12 must fail: %s", status)
	}
	if status, obs, _ := run(`{"count": 12}`, map[string]string{"n": "soon-987654"}, gt); status != "failed" ||
		!strings.Contains(obs, "the saved value of `n` is not a number") || strings.Contains(obs, "987654") {
		t.Errorf("a non-numeric saved value must fail by name without printing it: %s %s", status, obs)
	}
	if status, obs, _ := run(`{"count": 12}`, map[string]string{}, gt); status != "not-measured" || !strings.Contains(obs, "saved.n") {
		t.Errorf("an unsaved variable must be named: %s %s", status, obs)
	}
	eq := mcp.BodyAssert{Field: "tag", Op: mcp.BodyEqualsOp, Value: "${saved.tag}"}
	if status, obs, _ := run(`{"tag": "v7"}`, map[string]string{"tag": "v7"}, eq); status != "passed" {
		t.Errorf("a saved value in an equals claim must be bound: %s %s", status, obs)
	}
}
