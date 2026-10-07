package scenario

import (
	"strings"
	"testing"
)

// /, promise 1: an author writes a STEPPED HTTP load check, declared the way the AMQP
// Load check declares its steps, and a malformed ramp is refused when it is written, naming the field.

// httpLoadSample is the worked example of the docs (skills/scenario-author/SKILL.md).
const httpLoadSample = `# Scenario: HTTP load ramp on the orders API, 10 to 80 users

## Metadata
- **ID**: HTTPLOAD-001
- **Layer**: HTTP Load
- **Tags**: http, load
- **Target**: api-lab

## TRIGGER
POST ` + "`/api/orders`" + `
Content-Type: application/json
` + "```json" + `
{"sku": "A-1", "qty": 1}
` + "```" + `

## EXPECT
### Runnable
- every step is measured
- the smallest step is comfortable

### Non-runnable
- the ramp reports the largest comfortable step as the tested limit

## LOAD
- **Steps**: 10, 20, 40, 80
- **Step Duration Seconds**: 60
- **Ramp Seconds**: 10
- **Settle Seconds**: 20
- **Target P95 Ms**: 500
- **Max Error Rate**: 0.01
- **Must Sustain**: 20

## TIMEOUT
10s

## CLEANUP
N/A — every order the ramp creates is in the lab deployment, which is reset after each load run.
`

func httpLoadWith(t *testing.T, oldLine, newLine string) string {
	t.Helper()
	if !strings.Contains(httpLoadSample, oldLine) {
		t.Fatalf("fixture drifted: %q is not in the sample", oldLine)
	}
	return strings.Replace(httpLoadSample, oldLine, newLine, 1)
}

func TestHTTPLoad_TheWorkedExampleIsValid_AndParsesItsSteps(t *testing.T) {
	s, errs := Validate(httpLoadSample)
	if len(errs) != 0 {
		t.Fatalf("the worked example is refused:\n%s", errsText(errs))
	}
	if s.Load != nil || s.AMQPLoad != nil {
		t.Fatalf("an HTTP Load scenario must leave Load and AMQPLoad nil: %+v %+v", s.Load, s.AMQPLoad)
	}
	p := s.HTTPLoad
	if p == nil {
		t.Fatal("HTTPLoad is nil for a fully declared profile")
	}
	if len(p.Steps) != 4 || p.Steps[0] != 10 || p.Steps[3] != 80 || p.StepDurationSeconds != 60 || p.RampSeconds != 10 ||
		p.SettleSeconds != 20 || p.TargetP95Ms != 500 || p.MaxErrorRate != 0.01 || p.MustSustain != 20 {
		t.Errorf("profile = %+v", p)
	}
	if kind, err := TargetKind(s); err != nil || kind != "http" {
		t.Errorf("TargetKind = %q, %v; want http (the **Target** names a targets.http_targets entry)", kind, err)
	}
	if !IsContentLayer(HTTPLoadLayer) || JudgedByResponseCode(s) {
		t.Error("an HTTP Load scenario is judged by its ramp's verdict, never by one response code")
	}
}

func TestHTTPLoad_Defaults_AreTheAMQPRampDefaults(t *testing.T) {
	md := httpLoadWith(t, "- **Ramp Seconds**: 10\n- **Settle Seconds**: 20\n", "")
	md = strings.Replace(md, "- **Must Sustain**: 20\n", "", 1)
	s, errs := Validate(md)
	if len(errs) != 0 {
		t.Fatalf("refused:\n%s", errsText(errs))
	}
	if p := s.HTTPLoad; p == nil || p.RampSeconds != amqpDefaultRamp || p.SettleSeconds != amqpDefaultSettle || p.MustSustain != 0 {
		t.Fatalf("profile = %+v, want ramp %d, settle %d", p, amqpDefaultRamp, amqpDefaultSettle)
	}
}

func TestHTTPLoad_MalformedRampIsRefusedNamingTheField(t *testing.T) {
	cases := []struct {
		name, old, new, want string
	}{
		{"no Steps", "- **Steps**: 10, 20, 40, 80\n", "", "is missing **Steps**"},
		{"a step that is not a number", "- **Steps**: 10, 20, 40, 80", "- **Steps**: 10, twenty", `**Steps** entry "twenty" is not a whole number of users`},
		{"steps that do not rise", "- **Steps**: 10, 20, 40, 80", "- **Steps**: 10, 40, 20", "**Steps** must rise: 20 comes after 40"},
		{"too many users", "- **Steps**: 10, 20, 40, 80", "- **Steps**: 10, 2001", "**Steps** entry 2001 is out of bounds [1, 2000] users"},
		{"too many steps", "- **Steps**: 10, 20, 40, 80", "- **Steps**: 1,2,3,4,5,6,7,8,9,10,11,12,13", "**Steps** has 13 entries, at most 12 are allowed"},
		{"a step held too briefly", "- **Step Duration Seconds**: 60", "- **Step Duration Seconds**: 5", "**Step Duration Seconds** = 5 is out of bounds [10, 3600]"},
		{"a step held too long", "- **Step Duration Seconds**: 60", "- **Step Duration Seconds**: 3601", "**Step Duration Seconds** = 3601 is out of bounds [10, 3600]"},
		{"a ramp longer than half the step", "- **Ramp Seconds**: 10", "- **Ramp Seconds**: 31", "**Ramp Seconds** = 31 is out of bounds [0, 30]"},
		{"a settle pause too long", "- **Settle Seconds**: 20", "- **Settle Seconds**: 601", "**Settle Seconds** = 601 is out of bounds [0, 600]"},
		{"no p95", "- **Target P95 Ms**: 500\n", "", "is missing **Target P95 Ms**"},
		{"an error rate above 1", "- **Max Error Rate**: 0.01", "- **Max Error Rate**: 2", "**Max Error Rate** = 2 is out of bounds"},
		{"Must Sustain not a step", "- **Must Sustain**: 20", "- **Must Sustain**: 30", "**Must Sustain** = 30 is not one of **Steps**"},
		{"an AMQP-only key", "- **Max Error Rate**: 0.01\n", "- **Max Error Rate**: 0.01\n- **Rate Per Session**: 5\n", "**Rate Per Session** is a key of the AMQP Load layer"},
		{"the legacy Users key", "- **Max Error Rate**: 0.01\n", "- **Max Error Rate**: 0.01\n- **Users**: 5\n", "**Users** is a key of the other layers"},
		{"an unknown key", "- **Max Error Rate**: 0.01\n", "- **Max Error Rate**: 0.01\n- **Think Time**: 5\n", "unknown key **Think Time**"},
		{"no Target", "- **Target**: api-lab\n", "", "`**Target**` is required on an `HTTP Load` scenario"},
		{"a status= bullet", "- every step is measured\n", "- every step is measured\n- status=200\n", "a `status=` bullet is not allowed on an `HTTP Load` scenario"},
		{"a claim nothing executes", "- every step is measured\n", "- every step is measured\n- the API stays fast\n", "is not in the HTTP Load vocabulary"},
		{"a chained layer", "- **Layer**: HTTP Load", "- **Layer**: HTTP Ingestion -> HTTP Load", "`HTTP Load` must be the only layer"},
		{"an mcp dispatch tag", "- **Tags**: http, load", "- **Tags**: mcp, load", "its dispatch tag is `http`, not `mcp`"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, errs := Validate(httpLoadWith(t, c.old, c.new))
			got := errsText(errs)
			if !strings.Contains(got, c.want) {
				t.Fatalf("want an error containing %q, got:\n%s", c.want, got)
			}
			// a bad VALUE leaves no profile; a refused extra KEY (like on AMQP Load) is refused by Validate only
			if (strings.HasPrefix(c.want, "**") || strings.HasPrefix(c.want, "is missing")) && !strings.Contains(c.want, "is a key of") {
				if s.HTTPLoad != nil {
					t.Errorf("a refused profile was kept: %+v", s.HTTPLoad)
				}
			}
		})
	}
}

func TestHTTPLoad_NeedsALoadSection(t *testing.T) {
	md := httpLoadSample[:strings.Index(httpLoadSample, "## LOAD")] + httpLoadSample[strings.Index(httpLoadSample, "## TIMEOUT"):]
	_, errs := Validate(md)
	if got := errsText(errs); !strings.Contains(got, "an `HTTP Load` scenario needs a `## LOAD` section") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestHTTPLoad_IsACanonicalLayer(t *testing.T) {
	if !contains(CanonicalLayers, HTTPLoadLayer) {
		t.Fatalf("CanonicalLayers lacks %q", HTTPLoadLayer)
	}
}
