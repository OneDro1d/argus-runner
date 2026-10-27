package scenario

import (
	"strings"
	"testing"
)

// The mcp step's new parts at the AUTHORING door. Each refusal is asserted by its sentence.

func mcpTrig(fields string) string {
	if fields != "" {
		fields = "," + fields
	}
	return `{"steps":[{"type":"mcp","name":"m","tool":"execute_sql","args":{}` + fields + `}]}`
}

const mcpOKExpect = "### Runnable\n- step m: result.isError == false\n- step m: body has 0.status equals PAID\n"
const mcpExtractedExpect = "### Runnable\n- step m: result.isError == false\n- step m: body extracted has 0.status equals PAID\n"

// mcpErrs validates a step with the given fields; a step that declares `extract` gets a claim that reads it, so the
// "declared but nothing reads it" rule is not what these cases are about.
func mcpErrs(fields string) []Error {
	exp := mcpOKExpect
	if strings.Contains(fields, `"extract"`) {
		exp = mcpExtractedExpect
	}
	_, errs := Validate(chainMD(mcpTrig(fields), exp))
	return errs
}

func mcpErrsExpect(fields, expect string) []Error {
	_, errs := Validate(chainMD(mcpTrig(fields), expect))
	return errs
}

// (second build): the two doors of the extracted document, both directions, by name.
func TestValidate_MCPStepExtractedDocumentNeedsBothHalves(t *testing.T) {
	ext := `"extract":{"regex":"(a)"}`
	for _, c := range []struct{ name, fields, expect, want string }{
		{"extract with a body extracted claim", ext, mcpExtractedExpect, ""},
		{"extract read by a save from extracted", ext + `,"save":{"a":{"json":"$","from":"extracted"}}`, mcpOKExpect, ""},
		{"extract read by a regex save from extracted", ext + `,"save":{"a":{"regex":"(x)","from":"extracted"}}`, mcpOKExpect, ""},
		{"extract and a path", `"extract":{"path":"result","regex":"(a)"}`, mcpExtractedExpect, ""},
		{"extract nothing reads", ext, mcpOKExpect, "declares `extract` but nothing reads it"},
		{"extract read only by a plain save", ext + `,"save":{"a":"0.id"}`, mcpOKExpect, "declares `extract` but nothing reads it"},
		{"extracted claim without extract", ``, mcpExtractedExpect, "declares no `extract`"},
		{"save from extracted without extract", `"save":{"a":{"json":"$","from":"extracted"}}`, mcpOKExpect, "declares no `extract`"},
		{"from with another value", ext + `,"save":{"a":{"json":"$","from":"raw"}}`, mcpExtractedExpect, `declares "from" with a value other than "extracted"`},
		{"from alone", ext + `,"save":{"a":{"from":"extracted"}}`, mcpExtractedExpect, "must be a JSON path string"},
		{"empty path", `"extract":{"path":"","regex":"(a)"}`, mcpExtractedExpect, "empty `path`"},
		{"path naming a saved variable", `"extract":{"path":"${saved.p}","regex":"(a)"}`, mcpExtractedExpect, "${saved.…}"},
	} {
		t.Run(c.name, func(t *testing.T) {
			errs := mcpErrsExpect(c.fields, c.expect)
			if c.want == "" {
				if len(errs) != 0 {
					t.Fatalf("must be accepted, got %v", errs)
				}
				return
			}
			if !find(errs, c.want) {
				t.Fatalf("want a refusal saying %q, got %v", c.want, errs)
			}
		})
	}
}

func TestValidate_FromOnAStepThatIsNotMCPIsRefused(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"h","method":"GET","url":"http://x","save":{"a":{"json":"$","from":"extracted"}}}]}`
	_, errs := Validate(chainMD(trig, "### Runnable\n- step h: status=200\n"))
	if !find(errs, `declares "from"`) {
		t.Fatalf("only an mcp step has an extracted document: %v", errs)
	}
}

func TestValidate_BodyExtractedOnAStepThatIsNotMCPIsRefused(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"h","method":"GET","url":"http://x"}]}`
	_, errs := Validate(chainMD(trig, "### Runnable\n- step h: status=200\n- body extracted contains x\n"))
	if len(errs) == 0 {
		t.Fatalf("a `body extracted` bullet outside an mcp step is refused by name")
	}
}

func TestValidate_MCPStepWellFormedAccepted(t *testing.T) {
	for name, fields := range map[string]string{
		"nothing new":      ``,
		"extract":          `"extract":{"regex":"(?s)<d>(.*)</d>"}`,
		"unavailable":      `"unavailable":{"matching":"database_unavailable|internal server error"}`,
		"every new key":    `"extract":{"regex":"(\\[.*\\])"},"unavailable":{"matching":"x"},"poll":{"timeout":"10m","interval":"1s"},"idempotent":true,"always":true,"save":{"a":"0.id"}`,
		"the old fields":   `"transport":"streamable-http","target":"hub"`,
		"idempotent alone": `"idempotent":true`,
	} {
		t.Run(name, func(t *testing.T) {
			if errs := mcpErrs(fields); len(errs) != 0 {
				t.Fatalf("must be accepted, got %v", errs)
			}
		})
	}
}

func TestValidate_MCPStepRefusals(t *testing.T) {
	for _, c := range []struct{ name, fields, want string }{
		{"poll without idempotent", `"poll":{"timeout":"5s","interval":"1s"}`, "set \"idempotent\": true"},
		{"poll names the re-send", `"poll":{"timeout":"5s","interval":"1s"}`, "SENDS THE TOOL CALL AGAIN"},
		{"poll interval under a second", `"poll":{"timeout":"5s","interval":"500ms"},"idempotent":true`, "which is under 1s"},
		{"poll timeout over 10m", `"poll":{"timeout":"11m"},"idempotent":true`, "10m bound"},
		{"poll timeout not a duration", `"poll":{"timeout":"soon"},"idempotent":true`, "not a valid duration"},
		{"stray method", `"method":"GET"`, `carries the field "method"`},
		{"stray url", `"url":"http://x"`, `carries the field "url"`},
		{"stray query", `"query":"select 1"`, `carries the field "query"`},
		{"stray lists what is allowed", `"timeout":"5s"`, "an mcp step takes: type, name, transport, target, server_url, tool, args, save, always, extract, unavailable, poll, idempotent"},
		{"extract two groups", `"extract":{"regex":"(a)(b)"}`, "2 capture group(s)"},
		{"extract no group", `"extract":{"regex":"abc"}`, "0 capture group(s)"},
		{"extract broken", `"extract":{"regex":"(["}`, "does not compile"},
		{"extract empty", `"extract":{"regex":""}`, "without a `regex`"},
		{"unavailable empty", `"unavailable":{"matching":""}`, "there is no default pattern"},
		{"unavailable broken", `"unavailable":{"matching":"(["}`, "`unavailable` regex that does not compile"},
	} {
		t.Run(c.name, func(t *testing.T) {
			errs := mcpErrs(c.fields)
			if !find(errs, c.want) {
				t.Fatalf("want a refusal saying %q, got %v", c.want, errs)
			}
			if !find(errs, `step "m"`) {
				t.Fatalf("the refusal names the step: %v", errs)
			}
		})
	}
}

func TestValidate_MCPStepStrayFieldsAreNamedAllAtOnce(t *testing.T) {
	errs := mcpErrs(`"url":"x","method":"y"`)
	if !find(errs, `"method"`) || !find(errs, `"url"`) {
		t.Fatalf("every stray field is named: %v", errs)
	}
}

// `expect` is refused by name by W1, with the way out; the stray rule does not say a second, worse thing about it.
func TestValidate_MCPStepExpectKeyIsOnlyW1sBusiness(t *testing.T) {
	errs := mcpErrs(`"expect":"x"`)
	if !find(errs, "carries an `expect` key") {
		t.Fatalf("W1 still refuses it: %v", errs)
	}
	if find(errs, "does not accept") {
		t.Fatalf("not also as a stray field: %v", errs)
	}
}

func TestMCPStepProblems_PollNeedsAClaim(t *testing.T) {
	st := ChainStep{Type: "mcp", Name: "m", Idempotent: true, Poll: &ChainPoll{Timeout: "5s", Interval: "1s"}}
	if got := MCPStepProblems(st, nil, nil); len(got) != 1 || !strings.Contains(got[0], "no claim") {
		t.Fatalf("a poll with nothing to decide on is refused: %v", got)
	}
	if got := MCPStepProblems(st, nil, []string{"result.isError == false"}); len(got) != 0 {
		t.Fatalf("with a claim it is fine: %v", got)
	}
}

// a db or http step carrying the new keys is not the mcp rule's business (they have their own stray rules)
func TestValidate_NewKeysOnOtherStepTypesAreStillTheirOwnRules(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"h","method":"GET","url":"http://x","extract":{"regex":"(a)"}}]}`
	_, errs := Validate(chainMD(trig, "### Runnable\n- step h: status=200\n"))
	if !find(errs, `carries the field "extract"`) {
		t.Fatalf("an http step still refuses the field: %v", errs)
	}
}

func TestValidate_MCPStepPollIntervalSentenceIsSaidOnce(t *testing.T) {
	errs := mcpErrs(`"poll":{"timeout":"5s","interval":"500ms"},"idempotent":true`)
	for _, e := range errs {
		if strings.Contains(e.Message, "which is under 1s") {
			if n := strings.Count(e.Message, "poll no faster than once a second"); n != 1 {
				t.Fatalf("the poll-interval rule is stated %d times, want once: %s", n, e.Message)
			}
			return
		}
	}
	t.Fatalf("no poll-interval refusal: %v", errs)
}
