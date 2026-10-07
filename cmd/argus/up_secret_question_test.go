package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestUp_Yes_DoesNotAutoAnswerSecretShapedQuestion — --yes may accept a NON-secret default; it must
// never silently answer a secret-shaped one. This pass wires exactly one real question
// (product-folder, never secret-shaped), so the call site itself is exercised directly through
// askOrDefault — the ONE seam every question in this file goes through (upjson.go) — with an
// injected fake secret-shaped question, rather than skipping the requirement for lack of a second
// real trigger.
func TestUp_Yes_DoesNotAutoAnswerSecretShapedQuestion(t *testing.T) {
	if yesCanAnswer(jsonQuestion{Secret: true}) {
		t.Fatal("yesCanAnswer must refuse a secret-shaped question")
	}
	if !yesCanAnswer(jsonQuestion{Secret: false}) {
		t.Fatal("yesCanAnswer must allow a non-secret question")
	}

	// The REAL call site (upFirstRun -> resolveProductDir -> askOrDefault), driven directly with a
	// fake secret-shaped question and --yes set. stdin is empty on purpose: if askOrDefault ever
	// fell through to readAnswer here, it would either hang reading from an empty pipe or return an
	// error from the wrong place — either way this assertion would catch it, but the real point is
	// the RETURNED error and the fact that nothing was written to stdout.
	var out bytes.Buffer
	answer, err := askOrDefault(&out, strings.NewReader(""), true,
		jsonQuestion{Key: "fake-secret", Prompt: "a fake secret-shaped question", Secret: true},
		true, "should-never-be-used")
	if err == nil {
		t.Fatalf("askOrDefault answered a secret-shaped question under --yes instead of refusing (got %q)", answer)
	}
	if out.Len() != 0 {
		t.Fatalf("a refused secret-shaped question must not emit anything: got %q", out.String())
	}
}
