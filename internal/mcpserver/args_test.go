package mcpserver

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// VR10-S1 — the pure half: which caller keys are unknown (D2: `_`-prefixed keys are another program's
// notes), in the caller's order (S1-b), and the refusal text that carries everything needed to fix the
// call in one step (D7): the unknown named, the accepted listed with the required marked, the nearest
// match suggested when it is within Levenshtein 2 (S1-a) — and never substituted (D6).

func closedArgs(required []string, keys ...string) map[string]any {
	props := map[string]any{}
	for _, k := range keys {
		props[k] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

func TestUnknownArgs_Table(t *testing.T) {
	report := closedArgs([]string{"instance_id"}, "run_id", "instance_id")
	cases := []struct {
		name         string
		schema       map[string]any
		raw          string
		wantUnknown  []string
		wantAccepted []string
		wantErr      bool
	}{
		{"declared keys only", report, `{"instance_id":"local","run_id":"r1"}`, nil, []string{"instance_id", "run_id"}, false},
		{"one unknown key", report, `{"instance_id":"local","scenario_id":"ORD-X"}`, []string{"scenario_id"}, []string{"instance_id", "run_id"}, false},
		{"_-prefixed keys are ignored (D2)", report, `{"instance_id":"local","_meta":{"progressToken":1},"_note":"x"}`, nil, []string{"instance_id", "run_id"}, false},
		{"several unknown keys, caller order kept (S1-b)", report, `{"zeta":1,"alpha":2,"instance_id":"local"}`, []string{"zeta", "alpha"}, []string{"instance_id", "run_id"}, false},
		{"no arguments at all", report, ``, nil, []string{"instance_id", "run_id"}, false},
		{"null arguments", report, `null`, nil, []string{"instance_id", "run_id"}, false},
		{"empty object", report, `{}`, nil, []string{"instance_id", "run_id"}, false},
		{"arguments that are not an object", report, `[1,2]`, nil, nil, true},
		{"closed schema with no properties: every key is unknown", map[string]any{"type": "object", "additionalProperties": false},
			`{"a":1,"_b":2}`, []string{"a"}, nil, false},
		// An OPEN schema (no additionalProperties:false) declares an open object — JSON Schema's default.
		// The check enforces the schema; it does not override it. The local router's proxy schemas stay
		// open on purpose (it forwards the caller's bytes; the upstream plane owns the real contract).
		{"open schema is not policed", map[string]any{"type": "object", "properties": map[string]any{"instance_id": map[string]any{"type": "string"}}},
			`{"instance_id":"local","use_local_scenarios":"true"}`, nil, []string{"instance_id"}, false},
		{"additionalProperties:true is open too", map[string]any{"type": "object", "additionalProperties": true,
			"properties": map[string]any{"instance_id": map[string]any{"type": "string"}}}, `{"x":1}`, nil, []string{"instance_id"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			unknown, accepted, err := UnknownArgs(c.schema, json.RawMessage(c.raw))
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if !reflect.DeepEqual(unknown, c.wantUnknown) {
				t.Errorf("unknown = %v, want %v", unknown, c.wantUnknown)
			}
			if !c.wantErr && !reflect.DeepEqual(accepted, c.wantAccepted) {
				t.Errorf("accepted = %v, want %v (required first, then alphabetical)", accepted, c.wantAccepted)
			}
		})
	}
}

func TestRequiredArgs(t *testing.T) {
	got := RequiredArgs(closedArgs([]string{"instance_id", "content"}, "instance_id", "content", "path"))
	if want := []string{"instance_id", "content"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RequiredArgs = %v, want %v", got, want)
	}
	if got := RequiredArgs(map[string]any{"type": "object"}); len(got) != 0 {
		t.Fatalf("RequiredArgs on a schema without required = %v, want none", got)
	}
}

func TestLevenshtein(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"", "abc", 3}, {"abc", "", 3}, {"tag", "tag", 0}, {"kitten", "sitting", 3},
		{"scenaro_ref", "scenario_ref", 1}, {"layerr", "layer", 1}, {"scenario_id", "scenario_ref", 3}, {"junk", "instance_id", 10},
	} {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestRefuseUnknown_Text(t *testing.T) {
	accepted := []string{"instance_id", "layer", "scenario_ref", "tag", "use_local_scenarios"}
	required := []string{"instance_id"}

	o := RefuseUnknown("runner__run", []string{"scenaro_ref"}, accepted, required)
	if o == nil || !o.IsError || o.RPCErr != nil {
		t.Fatalf("the refusal must be a TOOL error (isError:true, no RPC error); got %+v", o)
	}
	p, _ := o.Payload.(map[string]any)
	text, _ := p["error"].(string)
	for _, want := range []string{
		`unknown argument "scenaro_ref"`, "runner__run", `did you mean "scenario_ref"?`,
		"instance_id (required)", "layer", "tag", "use_local_scenarios", "did not run",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal text must carry %q; got: %s", want, text)
		}
	}
	if !reflect.DeepEqual(p["unknown"], []string{"scenaro_ref"}) || !reflect.DeepEqual(p["accepted"], accepted) || !reflect.DeepEqual(p["required"], required) {
		t.Errorf("structured fields: unknown=%v accepted=%v required=%v", p["unknown"], p["accepted"], p["required"])
	}
	if dym, _ := p["did_you_mean"].(map[string]string); dym["scenaro_ref"] != "scenario_ref" {
		t.Errorf("did_you_mean = %v, want scenaro_ref → scenario_ref", p["did_you_mean"])
	}

	// Distance > 2 → no suggestion at all (a wrong hint is worse than none).
	o = RefuseUnknown("runner__ping", []string{"junk"}, []string{"instance_id"}, required)
	p, _ = o.Payload.(map[string]any)
	if text, _ := p["error"].(string); strings.Contains(text, "did you mean") {
		t.Errorf("no suggestion expected for a far key; got: %s", text)
	}
	if _, has := p["did_you_mean"]; has {
		t.Errorf("did_you_mean must be absent when nothing is near; got %v", p["did_you_mean"])
	}

	// Ties → the first accepted key in order.
	o = RefuseUnknown("t", []string{"tam"}, []string{"instance_id", "tab", "tag"}, required)
	p, _ = o.Payload.(map[string]any)
	if dym, _ := p["did_you_mean"].(map[string]string); dym["tam"] != "tab" {
		t.Errorf("tie must resolve to the first accepted key in order (tab); got %v", p["did_you_mean"])
	}

	// Several unknown keys: ALL named, in the caller's order; each near one gets its own suggestion.
	o = RefuseUnknown("runner__run", []string{"scenario_id", "layerr"}, accepted, required)
	p, _ = o.Payload.(map[string]any)
	text, _ = p["error"].(string)
	if !strings.Contains(text, `unknown arguments "scenario_id", "layerr"`) {
		t.Errorf("all unknown keys must be named in caller order; got: %s", text)
	}
	if !strings.Contains(text, `did you mean "layer" for "layerr"?`) {
		t.Errorf("the near key gets its suggestion, the far one none; got: %s", text)
	}
	if dym, _ := p["did_you_mean"].(map[string]string); len(dym) != 1 || dym["layerr"] != "layer" {
		t.Errorf("did_you_mean = %v, want only layerr → layer", p["did_you_mean"])
	}
}
