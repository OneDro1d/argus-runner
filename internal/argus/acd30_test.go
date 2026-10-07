package argus

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// AC-D30 (#171) — A DECLARED SECOND STATUS HAS TO BE ABLE TO ARRIVE.
//
// ── THE DEFECT ───────────────────────────────────────────────────────────────────────────────────
//
// ORDE-013 declares the two-request idempotency flow — the same order POSTed twice under one
// Idempotency-Key, answered 202 then 409, exactly one row — as `HTTP Ingestion -> Database State`
// with `status=202`, `status2=409` and a row check. The template is chosen from the LAST layer
// (scenario.PrimaryLayer), so it ran database-state.jmx, which sent ONE request, while the judge
// honours a declared second status on every layer. The scenario was red against any SUT — "responder
// returned 1 response(s) [202]; a request did not fire" — and its row check never ran. Measured on run
// 20260922T170520156 (orderservice-compose): the demo itself answers 202 then 409 with one row.
//
// ── WHY THE FIX IS NOT "SELECT http-idempotency" ─────────────────────────────────────────────────
//
// http-idempotency.jmx sends two requests and has no JDBC sampler; per TemplateReads it reads no
// expect.* property at all. Selecting it for ORDE-013 would judge 202/409 and never evaluate the row
// check: a false red would become a pass that never looked at the database, the shape #171 itself
// warns about. database-state.jmx already sends Idempotency-Key at thread-group scope (fed from
// idempotency.key, which DeriveProps sets exactly when status2= is declared). What it lacked was the
// second request: it now sends `<id>-trigger2`, inside an IfController on trigger.second, and the JDBC
// verify runs after both.
//
// ── AND AN IMPOSSIBLE DECLARATION IS REFUSED WHEN WRITTEN ────────────────────────────────────────
//
// A second status on a template that still sends one request cannot be satisfied by any SUT, so it is
// refused at write time and the message names the layer and the template (#171, option 2).
// RuntimeTemplateBase's comment promised that refusal; it did not exist.

func acd30Props(t *testing.T, s *scenario.Scenario) map[string]string {
	t.Helper()
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	c.Targets.Database = &config.DBTarget{JDBCURL: "jdbc:postgresql://db.invalid:5432/t", Username: "u"}
	p, err := DeriveProps(c, s, "tr-test-0001")
	if err != nil {
		t.Fatalf("DeriveProps: %v", err)
	}
	return p
}

const acd30Aggregate = "SELECT count(*) AS n FROM order_service.orders WHERE correlation_id = '${correlation_id}';"

func TestACD30_ADeclaredSecondStatusAsksTheTemplateForASecondRequest(t *testing.T) {
	two := acd30Props(t, dbScenario(acd30Aggregate, "- status2=409\n- n == 1"))
	if two["trigger.second"] != "true" {
		t.Errorf("status2= is declared but trigger.second = %q: database-state.jmx sends one request, the "+
			"second response can never arrive, and the scenario is red against a healthy SUT (AC-D30)", two["trigger.second"])
	}
	if two["idempotency.key"] == "" {
		t.Errorf("the second request must share the first one's Idempotency-Key, and idempotency.key is empty")
	}

	one := acd30Props(t, dbScenario(acd30Aggregate, "- n == 1"))
	if v, ok := one["trigger.second"]; ok {
		t.Errorf("no status2= is declared, yet trigger.second = %q: a one-request scenario would start "+
			"POSTing twice (VR12-E7: a second request is sent ONLY when it is declared)", v)
	}
}

// ── the template ─────────────────────────────────────────────────────────────────────────────────

type jmxNode struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Content string     `xml:",chardata"`
	Nodes   []jmxNode  `xml:",any"`
}

func (n jmxNode) attr(name string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// prop is the value of a direct stringProp/boolProp child called name.
func (n jmxNode) prop(name string) string {
	for _, c := range n.Nodes {
		if (c.XMLName.Local == "stringProp" || c.XMLName.Local == "boolProp") && c.attr("name") == name {
			return strings.TrimSpace(c.Content)
		}
	}
	return ""
}

// flatProps is every stringProp/boolProp below n as sorted "name=value" lines — what a sampler sends.
func (n jmxNode) flatProps() string {
	var out []string
	var walk func(jmxNode)
	walk = func(x jmxNode) {
		for _, c := range x.Nodes {
			if c.XMLName.Local == "stringProp" || c.XMLName.Local == "boolProp" {
				out = append(out, c.attr("name")+"="+strings.TrimSpace(c.Content))
			}
			walk(c)
		}
	}
	walk(n)
	sort.Strings(out)
	return strings.Join(out, "\n")
}

type jmxPair struct{ el, kids jmxNode }

// pairs reads a hashTree the way JMeter does: every element is followed by the hashTree of its children.
func pairs(tree jmxNode) []jmxPair {
	var out []jmxPair
	for i := 0; i < len(tree.Nodes); i++ {
		el := tree.Nodes[i]
		if el.XMLName.Local == "hashTree" {
			continue
		}
		var kids jmxNode
		if i+1 < len(tree.Nodes) && tree.Nodes[i+1].XMLName.Local == "hashTree" {
			kids = tree.Nodes[i+1]
			i++
		}
		out = append(out, jmxPair{el, kids})
	}
	return out
}

func loadJMX(t *testing.T, base string) jmxNode {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "templates", base+".jmx"))
	if err != nil {
		t.Fatal(err)
	}
	var root jmxNode
	if err := xml.Unmarshal(b, &root); err != nil {
		t.Fatalf("%s.jmx is not well-formed XML: %v", base, err)
	}
	return root
}

// threadGroup returns the children of the template's single ThreadGroup.
func threadGroup(t *testing.T, root jmxNode) []jmxPair {
	t.Helper()
	var found []jmxPair
	var walk func(jmxNode)
	walk = func(tree jmxNode) {
		for _, p := range pairs(tree) {
			if p.el.XMLName.Local == "ThreadGroup" {
				found = pairs(p.kids)
				return
			}
			walk(p.kids)
		}
	}
	for _, top := range root.Nodes {
		if top.XMLName.Local == "hashTree" {
			walk(top)
		}
	}
	if found == nil {
		t.Fatal("no ThreadGroup in the template")
	}
	return found
}

func processor(kids jmxNode, class, name string) (jmxNode, bool) {
	for _, p := range pairs(kids) {
		if p.el.XMLName.Local == class && p.el.attr("testname") == name {
			return p.el, true
		}
	}
	return jmxNode{}, false
}

func TestACD30_DatabaseStateSendsTheSecondRequestOnlyWhenAsked(t *testing.T) {
	const (
		trigger  = "${__P(scenario.id,unknown)}-trigger"
		trigger2 = "${__P(scenario.id,unknown)}-trigger2"
		verify   = "${__P(scenario.id,unknown)}-verify"
		headers  = "apply author headers (VR12-T3)"
		stimulus = "trigger is stimulus-only (DF-14)"
	)
	tg := threadGroup(t, loadJMX(t, "database-state"))

	iTrigger, iIf, iVerify := -1, -1, -1
	var first, ifc jmxPair
	ifs := 0
	for i, p := range tg {
		switch {
		case p.el.XMLName.Local == "HTTPSamplerProxy" && p.el.attr("testname") == trigger:
			iTrigger, first = i, p
		case p.el.XMLName.Local == "IfController":
			iIf, ifc = i, p
			ifs++
		case p.el.XMLName.Local == "JDBCSampler" && p.el.attr("testname") == verify:
			iVerify = i
		}
	}
	if iTrigger < 0 || iVerify < 0 {
		t.Fatalf("the first trigger (%d) and the JDBC verify (%d) must both stay in the thread group", iTrigger, iVerify)
	}
	if ifs != 1 {
		t.Fatalf("want exactly one IfController in the thread group, found %d — without it database-state.jmx "+
			"sends ONE request and a declared second status can never arrive (AC-D30)", ifs)
	}
	if !(iTrigger < iIf && iIf < iVerify) {
		t.Errorf("order must be trigger < second request < verify (got %d, %d, %d): the row check has to see "+
			"the database AFTER the duplicate, or `n == 1` proves nothing about idempotency", iTrigger, iIf, iVerify)
	}
	if got := ifc.el.prop("IfController.condition"); got != "${__P(trigger.second,false)}" {
		t.Errorf("the second request must fire only under trigger.second, condition = %q", got)
	}
	if ifc.el.prop("IfController.useExpression") != "true" {
		t.Errorf("IfController.useExpression must be true, or the condition string is never evaluated as a value")
	}

	inner := pairs(ifc.kids)
	if len(inner) != 1 || inner[0].el.XMLName.Local != "HTTPSamplerProxy" || inner[0].el.attr("testname") != trigger2 {
		t.Fatalf("the IfController must hold exactly one HTTP sampler named %q, got %d element(s)", trigger2, len(inner))
	}
	second := inner[0]

	// The second request is the SAME request: same host, port, path, method and body. Only the label differs.
	if a, b := first.el.flatProps(), second.el.flatProps(); a != b {
		t.Errorf("the second request differs from the first — an idempotency replay must resend the same "+
			"request.\nfirst:\n%s\nsecond:\n%s", a, b)
	}
	for _, c := range []struct{ class, name string }{
		{"JSR223PreProcessor", headers},   // author headers reach the second request too
		{"JSR223PostProcessor", stimulus}, // a 409 on the replay is a stimulus, not a JMeter failure
	} {
		want, ok1 := processor(first.kids, c.class, c.name)
		got, ok2 := processor(second.kids, c.class, c.name)
		if !ok1 || !ok2 {
			t.Errorf("%q: on the first trigger = %v, on the second = %v — both requests need it", c.name, ok1, ok2)
			continue
		}
		if want.prop("script") != got.prop("script") {
			t.Errorf("%q: the second request's script differs from the first one's", c.name)
		}
	}
}

// A template "sends a second request" when it has two HTTP samplers that talk to the SUT. Samplers
// labelled `-verify…` are the runner's own reads, and the judge drops them (sutTriggerCodes). This keeps
// templateSendsASecondRequest tied to templates/*.jmx, the way TemplateReads is.
func TestACD30_SecondRequestTemplatesAreGroundedInTheFiles(t *testing.T) {
	for base := range TemplateReads {
		n := 0
		var walk func(jmxNode)
		walk = func(x jmxNode) {
			for _, c := range x.Nodes {
				if c.XMLName.Local == "HTTPSamplerProxy" && !strings.Contains(c.attr("testname"), "-verify") {
					n++
				}
				walk(c)
			}
		}
		walk(loadJMX(t, base))
		if got, want := templateSendsASecondRequest(base), n >= 2; got != want {
			t.Errorf("%s.jmx has %d SUT request sampler(s), but templateSendsASecondRequest = %v", base, n, got)
		}
	}
}

func TestACD30_TheJudgeCountsBothRequestsAndNotTheVerify(t *testing.T) {
	codes := sutTriggerCodes([]int{202, 409, 200}, []string{"ORDE-013-trigger", "ORDE-013-trigger2", "ORDE-013-verify"}, "ORDE-013")
	if fmt.Sprint(codes) != "[202 409]" {
		t.Fatalf("the judge must see both requests and never the JDBC verify, got %v", codes)
	}
	if pass, observed := judge(codes, []int{202, 409}, nil); !pass {
		t.Errorf("202 then 409 against a declared 202 / 409 must hold, got %q", observed)
	}
	// the defect's own evidence: one request against two declared statuses
	if pass, observed := judge([]int{202}, []int{202, 409}, nil); pass || !strings.Contains(observed, "did not fire") {
		t.Errorf("one response against two declared statuses must be red and say a request did not fire, got %v %q", pass, observed)
	}
}

func TestACD30_ASecondStatusNoTemplateCanSendIsRefusedWhenWritten(t *testing.T) {
	sc := func(layer, tags, expect string) *scenario.Scenario {
		return scenario.Parse(strings.Join([]string{
			"# Scenario: t", "",
			"## Metadata", "- **ID**: T-030", "- **Layer**: " + layer, "- **Tags**: " + tags, "",
			"## TRIGGER", "POST `${INGESTION_URL}/api/v1/orders`", "",
			"## EXPECT", "### Runnable", expect, "",
			"### Non-runnable", "- a fixture", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n"))
	}
	acd30 := func(s *scenario.Scenario) (ExpectProblem, bool) {
		for _, p := range ExpectProblems(s) {
			if strings.Contains(p.Message, "AC-D30") {
				return p, true
			}
		}
		return ExpectProblem{}, false
	}

	for _, c := range []struct {
		name, layer, tags, template string
		refused                     bool
	}{
		{"Message Flow sends one request", "Message Flow", "http", "message-flow", true},
		{"External Delivery sends one request", "External Delivery", "http", "external-delivery", true},
		{"a layer without its own template falls back to one request", "Permissions", "http", "http-ingestion", true},
		{"the saga tag sends one request, even on HTTP Ingestion", "HTTP Ingestion", "http, saga-presence", "saga-presence", true},
		{"HTTP Ingestion switches to http-idempotency", "HTTP Ingestion", "http", "http-idempotency", false},
		{"Database State now sends the second request itself", "Database State", "http", "database-state", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := sc(c.layer, c.tags, "- status=202\n- status2=409")
			if got := RuntimeTemplateBase(s); got != c.template {
				t.Fatalf("fixture drift: RuntimeTemplateBase = %q, want %q", got, c.template)
			}
			p, refused := acd30(s)
			if refused != c.refused {
				t.Fatalf("refused = %v, want %v (%+v)", refused, c.refused, p)
			}
			if !refused {
				return
			}
			if p.Bullet != "status2=409" {
				t.Errorf("the refusal must quote the declaring bullet verbatim, got %q", p.Bullet)
			}
			for _, want := range []string{"`" + c.layer + "`", "`" + c.template + ".jmx`"} {
				if !strings.Contains(p.Message, want) {
					t.Errorf("the refusal must name %s, got %q", want, p.Message)
				}
			}
		})
	}

	t.Run("no second status, no refusal", func(t *testing.T) {
		if p, refused := acd30(sc("Message Flow", "http", "- status=202")); refused {
			t.Errorf("a scenario with one declared status was refused: %+v", p)
		}
	})
}
