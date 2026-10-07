package argus

import (
	"strconv"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// enforcedContentChecks lists the CONTENT checks an http/JMeter scenario hands to the template
// that runs it — the text behind report.ScenarioResult.AssertionsEnforced on this engine.
//
// It reads the props DeriveProps built, never the scenario file, so it can only name a check the
// template was actually given: the numbered body family (`expect.body.N.*`, filled with the run's
// correlation id) and the column set (`expect.columns`, from ParseDBExpect). Each family counts only
// when the template that runs (base) reads it — per TemplateReads — because a property no template
// reads was not enforced, whatever the file says.
//
// A status line is not a content check and never appears here, which keeps the meaning a chain step
// already has: 0 says only the status / no-error was checked.
func enforcedContentChecks(base string, props map[string]string) []string {
	var out []string
	if templateReadsProperty(base, "expect.body.") {
		n, _ := strconv.Atoi(props["expect.body.count"])
		var want mcp.Expect
		for i := 1; i <= n; i++ {
			k := "expect.body." + strconv.Itoa(i) + "."
			want.Body = append(want.Body, mcp.BodyAssert{
				Field: props[k+"field"], Op: props[k+"op"], Value: props[k+"value"]})
		}
		out = append(out, chain.EnforcedAssertions(want)...)
	}
	// A declared "no rows" and a declared row count are checks the database templates evaluate. The
	// has_rows=true every database expectation carries by default is not a declared check.
	//
	// AC-D31: NEITHER is a check on a query that always returns one row (an ungrouped aggregate) — the
	// template counts rows, so `row_count == N` cannot fail and `no rows` cannot pass. Listing them here
	// reported a check that judges nothing as enforced; tier 3 (unexecuted.go) names them instead.
	if scenario.SingleRowAggregate(props["db.query"]) {
		// fall through to the column families below, which DO read the value
	} else if templateReadsProperty(base, "expect.has_rows") && props["expect.has_rows"] == "false" {
		out = append(out, "no rows")
	} else if templateReadsProperty(base, "expect.row_count") && props["expect.row_count"] != "" {
		out = append(out, "row_count == "+props["expect.row_count"])
	}
	// AC-D16: a declared `- broker refuses with <code>` is a content check like the two above —
	// the AMQP sampler judges it (never here, VR-C8: this file reports what was ENFORCED, not a
	// second verdict), so it is named by the code the scenario declared.
	if templateReadsProperty(base, "expect.refusal_code") && props["expect.refusal_code"] != "" {
		out = append(out, "broker refuses with "+props["expect.refusal_code"])
	}
	if templateReadsProperty(base, "expect.columns") && props["expect.columns"] != "" {
		for _, pair := range strings.Split(props["expect.columns"], ";") {
			name, val, ok := strings.Cut(pair, "=")
			if !ok || name == "" {
				continue
			}
			switch {
			case val == "__NULL__":
				out = append(out, "column "+name+" is null")
			case strings.Contains(val, "|"):
				out = append(out, "column "+name+" is one of "+strings.ReplaceAll(val, "|", " or "))
			default:
				out = append(out, "column "+name+" == "+val)
			}
		}
	}
	return out
}
