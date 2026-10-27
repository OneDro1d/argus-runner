package reveal

// replay_test.go — AC-7: `argus replay`'s pure logic against a fake target, matching and
// mismatching digests — no real container, no real SUT.

import (
	"context"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

func replayDocFixture() *Document {
	return &Document{
		RunID: "run-1", ArtifactDigest: "sha256:" + strings.Repeat("aa", 32),
		SealedFiles: []SealedFile{{ID: "S1", Path: "a/S1.md", Body: "# S1 body"}},
		Verdicts:    []federation.ScenarioResult{{ID: "S1", Outcome: "passed"}},
	}
}

// fakeExecutor reports a fixed observed outcome — a fake target, standing in for a real SUT.
type fakeExecutor struct {
	outcome string
	err     error
}

func (f fakeExecutor) Execute(ctx context.Context, path, body string) (string, string, error) {
	return f.outcome, "fake target executed " + path, f.err
}

func TestReplay_MismatchedDigest_Refused_ExitTwo(t *testing.T) {
	doc := replayDocFixture()
	res, err := Replay(context.Background(), doc, "S1", "sha256:"+strings.Repeat("bb", 32), fakeExecutor{outcome: "passed"})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !res.Refused || res.ExitCode() != 2 {
		t.Fatalf("Replay with a mismatching --against = %+v, want Refused with ExitCode 2", res)
	}
	t.Logf("REFUSAL: %s", res.RefusalReason)
}

func TestReplay_MatchingDigestAndVerdict_ExitZero(t *testing.T) {
	doc := replayDocFixture()
	res, err := Replay(context.Background(), doc, "S1", doc.ArtifactDigest, fakeExecutor{outcome: "passed"})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if res.Refused || !res.Match || res.ExitCode() != 0 {
		t.Fatalf("Replay with a matching digest and verdict = %+v, want Match with ExitCode 0", res)
	}
}

func TestReplay_MatchingDigestMismatchingVerdict_ExitOne(t *testing.T) {
	doc := replayDocFixture()
	res, err := Replay(context.Background(), doc, "S1", doc.ArtifactDigest, fakeExecutor{outcome: "failed"})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if res.Refused || res.Match || res.ExitCode() != 1 {
		t.Fatalf("Replay with a matching digest but a DIFFERENT observed outcome = %+v, want a non-match with ExitCode 1", res)
	}
	t.Logf("MISMATCH: declared=%s observed=%s (%s)", res.DeclaredVerdict, res.ObservedOutcome, res.ObservedDetail)
}

func TestReplay_UnknownScenario_Errors(t *testing.T) {
	doc := replayDocFixture()
	if _, err := Replay(context.Background(), doc, "NOPE", doc.ArtifactDigest, fakeExecutor{outcome: "passed"}); err == nil {
		t.Fatal("Replay of a scenario absent from the reveal's verdicts succeeded; want an error")
	}
}
