package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// V31-003 (VR13-CID) — AN ENVIRONMENT VARIABLE IS NEVER FILLED INTO A CHECK.
//
// This is the half of the row that is a REFUSAL rather than a substitution, and it exists for one
// reason: a check's value is PRINTED IN THE REPORT — `assertions_enforced` carries it, and so does
// `failure.expected`. Filling `${TOKEN}` into a check would publish the token to every reader of the
// run, including the product hat. So the only placeholders a check may carry are the two names of
// the run's own id and, in a chain, `${saved.<var>}`; anything else is refused when the scenario is
// WRITTEN, where the author can still fix it.
func TestCheck_EnvironmentVariableIsNeverFilledIn(t *testing.T) {
	md := func(tag, bullet string) string {
		return strings.Join([]string{
			"# Scenario: CID", "",
			"## Metadata",
			"- **ID**: CID-001",
			"- **Layer**: HTTP Ingestion",
			"- **Tags**: " + tag, "",
			"## TRIGGER",
			"POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
			"## EXPECT",
			"### Runnable",
			"- status=202",
			"- " + bullet, "",
			"### Non-runnable",
			"- a fixture for the placeholder rule", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
	}
	refused := func(t *testing.T, text string) string {
		t.Helper()
		_, errs := scenario.Validate(text)
		for _, e := range errs {
			if strings.Contains(e.Message, "never filled in") {
				return e.Message
			}
		}
		return ""
	}

	t.Run("an environment variable in a check is REFUSED", func(t *testing.T) {
		msg := refused(t, md("http", "body contains ${TOKEN}"))
		if msg == "" {
			t.Fatal("`body contains ${TOKEN}` was accepted — its value would be printed in the report")
		}
		if !strings.Contains(msg, "${TOKEN}") {
			t.Errorf("the refusal does not name the placeholder: %q", msg)
		}
		// it must TEACH what is allowed, not merely refuse
		for _, want := range []string{"${cid}", "${correlation_id}", "${saved.<var>}"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the refusal does not offer %s: %q", want, msg)
			}
		}
	})

	t.Run("the two names of the run's own id are ACCEPTED", func(t *testing.T) {
		for _, b := range []string{"body contains m-${cid}", "body contains m-${correlation_id}"} {
			if msg := refused(t, md("http", b)); msg != "" {
				t.Errorf("%q was refused: %s", b, msg)
			}
		}
	})

	t.Run("an unclosed placeholder is caught too", func(t *testing.T) {
		if refused(t, md("http", "body contains m-${cid")) == "" {
			t.Error("`${cid` (unclosed) was accepted — it is filled in by nothing")
		}
	})

	t.Run("a `### Non-runnable` line is NOT refused", func(t *testing.T) {
		// It is documentation. Nothing fills it in because nothing runs it.
		text := strings.Join([]string{
			"# Scenario: CID", "",
			"## Metadata", "- **ID**: CID-002", "- **Layer**: HTTP Ingestion", "- **Tags**: http", "",
			"## TRIGGER", "POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
			"## EXPECT",
			"### Runnable", "- status=202", "",
			"### Non-runnable", "- the token is read from ${TOKEN}", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
		if msg := refused(t, text); msg != "" {
			t.Errorf("a `### Non-runnable` line was refused: %s", msg)
		}
	})

	t.Run("a chain may name a saved variable", func(t *testing.T) {
		// ${saved.<var>} is bound at run time by the chain runtime, so it IS filled in.
		text := strings.Join([]string{
			"# Scenario: CID", "",
			"## Metadata", "- **ID**: CID-003", "- **Layer**: Permissions", "- **Tags**: chain", "",
			"## TRIGGER", "POST \x60chain\x60", "",
			"```json",
			`{"steps":[{"type":"mcp","name":"read","tool":"t","args":{}}]}`,
			"```", "",
			"## EXPECT",
			"### Runnable", "- step read: body contains ${saved.docId}", "",
			"### Non-runnable", "- the id was captured by an earlier step", "",
			"## TIMEOUT", "60s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
		if msg := refused(t, text); msg != "" {
			t.Errorf("a chain's ${saved.docId} was refused: %s", msg)
		}
	})
}

// GUARD — the helper itself. Three properties, and the third is the one that is easy to get wrong.
func TestFillCorrelationID(t *testing.T) {
	t.Run("both names, and NOTHING else", func(t *testing.T) {
		const want = "a-tr-1/b-tr-1/c-${VAR}"
		if got := fillCorrelationID("a-${cid}/b-${correlation_id}/c-${VAR}", "tr-1"); got != want {
			t.Errorf("fillCorrelationID = %q, want %q — an environment value must never enter a check", got, want)
		}
	})

	t.Run("the checks are COPIED, never mutated", func(t *testing.T) {
		// A chain can be re-run, and one run's id must never leak into the next (VR10-S2-11).
		in := mcp.Expect{Body: []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "m-${cid}"}}}
		out := fillCorrelationIDInChecks(in, "tr-1")
		if in.Body[0].Value != "m-${cid}" {
			t.Errorf("the INPUT was mutated: %q", in.Body[0].Value)
		}
		if out.Body[0].Value != "m-tr-1" {
			t.Errorf("the copy = %q, want %q", out.Body[0].Value, "m-tr-1")
		}
	})

	t.Run("inside a `matching` pattern the id is QUOTED", func(t *testing.T) {
		// ⛔ The run path never applies the scenario-id rule — parse.go accepts any **ID**, and only
		// `argus validate-config` calls CheckID — so an id like `ord.v2` or `ord(1)` reaches the
		// correlation id. Unquoted, its `.` and `(` would change what the author's pattern matches.
		const corr = "tr-1-a.b(c)-x"
		if got := checkValueWithCorrelationID(mcp.BodyMatchesOp, "^m-${cid}$", corr); got != `^m-tr-1-a\.b\(c\)-x$` {
			t.Errorf("a regex value = %q, want the id regex-quoted", got)
		}
		if got := checkValueWithCorrelationID(mcp.BodyContainsOp, "m-${cid}", corr); got != "m-"+corr {
			t.Errorf("a substring value = %q, want the id verbatim", got)
		}
	})
}
