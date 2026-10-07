package onboard

// seed_reason_test.go — VR10-S4-6 (V28-006): THE REASON REACHES THE OPERATOR AS A SENTENCE.
//
// The control plane already refuses with a reason. Until now the seed copied that refusal into
// SeedFailure.Error VERBATIM — as the tool payload, JSON braces and all. Printed on a console under a
// file name, that is not a reason, it is a dump; and D3 asks for the NAMES and the REASON so the
// operator can fix the file. So the payload is rendered here, at the one place that knows its shape,
// into the sentence the CLI and onboard.sh then print unchanged.
//
// An UNRECOGNISED payload is still surfaced verbatim — losing an unexpected refusal would be the very
// silence this requirement exists to remove.

import (
	"encoding/json"
	"strings"
	"testing"
)

// refusalBody wraps a control-plane tool payload in the JSON-RPC envelope SeedScenarios receives.
func refusalBody(t *testing.T, payload map[string]any) []byte {
	t.Helper()
	inner, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2,
		"result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": string(inner)}},
			"isError": true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestParseWriteResult_RendersTheRefusalAsASentence(t *testing.T) {
	for _, c := range []struct {
		name       string
		payload    map[string]any
		want       []string
		wantAbsent []string
	}{
		{
			name: "a validation refusal becomes line + message",
			payload: map[string]any{"written": false, "valid": false, "errors": []any{
				map[string]any{"line": 4, "message": `ID "my scenario" is not allowed: an id may contain only letters, digits`},
			}},
			want: []string{"line 4", `ID "my scenario" is not allowed`},
			// the JSON scaffolding is noise on a console — the operator needs the sentence
			wantAbsent: []string{`"errors"`, `"written"`, "{"},
		},
		{
			name: "several validation errors are joined, none dropped",
			payload: map[string]any{"written": false, "errors": []any{
				map[string]any{"line": 4, "message": "bad id"},
				map[string]any{"line": 9, "message": "missing TRIGGER"},
			}},
			want:       []string{"line 4: bad id", "line 9: missing TRIGGER"},
			wantAbsent: []string{`"errors"`},
		},
		{
			name: "a refusal BY NAME (a duplicate id) comes through as its own sentence",
			payload: map[string]any{"written": false,
				"error": `ID "race-001" is already used by scenarios/graph/other.md on this instance`},
			want:       []string{`ID "race-001" is already used by scenarios/graph/other.md on this instance`},
			wantAbsent: []string{`"written"`, "{"},
		},
		{
			name:    "errors given as bare strings still render",
			payload: map[string]any{"written": false, "errors": []any{"missing ID"}},
			want:    []string{"missing ID"},
		},
		{
			name:    "an UNRECOGNISED payload is surfaced verbatim rather than lost",
			payload: map[string]any{"written": false, "something_new": "who knows"},
			want:    []string{"something_new", "who knows"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			written, msg := parseWriteResult(200, refusalBody(t, c.payload))
			if written {
				t.Fatalf("a refusal was read as a successful write: %v", c.payload)
			}
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("the rendered reason lost %q\n  got: %s", w, msg)
				}
			}
			for _, a := range c.wantAbsent {
				if strings.Contains(msg, a) {
					t.Errorf("the rendered reason still carries the raw payload token %q — that is a dump,\n"+
						"  not a reason an operator can act on\n  got: %s", a, msg)
				}
			}
		})
	}
}
