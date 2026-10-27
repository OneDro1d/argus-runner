package scenario

import (
	"strings"
	"testing"
)

// The `sign` block of an mcp step: the validator's half (door A). The key itself is never here;
// `key` is a NAME declared under check_env (the config-aware half is argus.CheckSigningKeys).

func signStep(t *testing.T, sign string) ChainStep {
	t.Helper()
	steps, err := ParseChainSteps(`{"steps":[{"type":"mcp","name":"w","tool":"t","args":{},"sign":` + sign + `}]}`)
	if err != nil || len(steps) != 1 {
		t.Fatalf("parse: %v", err)
	}
	return steps[0]
}

func TestMCPSignProblems_AcceptsTheTwoDocumentedForms(t *testing.T) {
	for name, sign := range map[string]string{
		"template": `{"key":"KEY_A","key_id":"abc","key_id_header":"x-key-id","separator":"|",
			"fields":[{"literal":"v4"},{"path":"messageId"},{"path":"prompt","sha256":"hex"}],"arg":"/signatures/agent.in"}`,
		"saved":                  `{"key":"KEY_A","value":"${saved.canonical}","header":"X-Sig"}`,
		"one-field-no-separator": `{"key":"KEY_A","fields":[{"path":"a.b"}],"arg":"sig"}`,
	} {
		if p := MCPSignProblems(signStep(t, sign).Sign); len(p) != 0 {
			t.Errorf("%s: unexpected problems %v", name, p)
		}
	}
}

func TestMCPSignProblems_RefusesMalformedBlocks(t *testing.T) {
	cases := map[string]struct{ sign, want string }{
		"no key":                   {`{"value":"${saved.c}","header":"X-Sig"}`, "`key`"},
		"key not a name":           {`{"key":"a b","value":"${saved.c}","header":"X-Sig"}`, "environment variable NAME"},
		"neither fields nor value": {`{"key":"K","header":"X-Sig"}`, "exactly one of `fields`"},
		"both fields and value":    {`{"key":"K","fields":[{"path":"a"}],"value":"${saved.c}","header":"X-Sig"}`, "exactly one of `fields`"},
		"value not a saved ref":    {`{"key":"K","value":"literal text","header":"X-Sig"}`, "whole ${saved."},
		"no destination":           {`{"key":"K","value":"${saved.c}"}`, "`arg` and/or `header`"},
		"two fields no separator":  {`{"key":"K","fields":[{"path":"a"},{"path":"b"}],"arg":"s"}`, "sign.separator"},
		"field with both":          {`{"key":"K","fields":[{"path":"a","literal":"x"}],"arg":"s"}`, "exactly one of `literal`"},
		"field with neither":       {`{"key":"K","fields":[{"sha256":"hex"}],"arg":"s"}`, "exactly one of `literal`"},
		"bad sha256":               {`{"key":"K","fields":[{"path":"a","sha256":"md5"}],"arg":"s"}`, `"hex" or "base64"`},
		"bad pointer":              {`{"key":"K","fields":[{"path":"/a~2b"}],"arg":"s"}`, "JSON pointer"},
		"key_id without header":    {`{"key":"K","key_id":"x","value":"${saved.c}","header":"X-Sig"}`, "`key_id` and `key_id_header`"},
		"header without key_id":    {`{"key":"K","key_id_header":"X-K","value":"${saved.c}","header":"X-Sig"}`, "`key_id` and `key_id_header`"},
		"reserved header":          {`{"key":"K","value":"${saved.c}","header":"X-Correlation-Id"}`, "reserved"},
		"bad header name":          {`{"key":"K","value":"${saved.c}","header":"X Sig"}`, "not a valid HTTP header name"},
		"unknown field":            {`{"key":"K","value":"${saved.c}","header":"X-Sig","private_key":"x"}`, ""},
	}
	for name, c := range cases {
		steps, err := ParseChainSteps(`{"steps":[{"type":"mcp","name":"w","tool":"t","args":{},"sign":` + c.sign + `}]}`)
		if name == "unknown field" {
			// an unknown key inside the block is refused by name by the step-level check, which sees the raw keys
			if probs := MCPSignRawProblems([]byte(c.sign)); len(probs) == 0 || !strings.Contains(probs[0], "private_key") {
				t.Errorf("%s: want a refusal naming private_key, got %v", name, probs)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		probs := MCPSignProblems(steps[0].Sign)
		if len(probs) == 0 {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(strings.Join(probs, " | "), c.want) {
			t.Errorf("%s: problems %v do not mention %q", name, probs, c.want)
		}
	}
}

func TestMCPStepProblems_SignIsAnAllowedKeyAndIsPoliced(t *testing.T) {
	st := signStep(t, `{"key":"K","header":"X-Sig"}`)
	probs := MCPStepProblems(st, nil, nil)
	if len(probs) == 0 || !strings.Contains(strings.Join(probs, " "), "sign") {
		t.Fatalf("a malformed sign block must surface through MCPStepProblems: %v", probs)
	}
}

func TestValidate_SignOnANonMCPStepIsRefused(t *testing.T) {
	steps, err := ParseChainSteps(`{"steps":[{"type":"http","name":"h","method":"GET","url":"http://x","sign":{"key":"K","value":"${saved.c}","header":"X-Sig"}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	errs := mcpStepErrors(steps, nil, nil, 1)
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "only an mcp step") {
		t.Fatalf("want a refusal that sign is for mcp steps only: %+v", errs)
	}
}
