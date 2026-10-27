package reveal

import (
	"context"
	"fmt"
)

// replay.go — `argus replay`'s pure logic (AC-7): from a clean container, with the pinned runner
// release, re-run one revealed scenario against an artefact the caller deploys themselves, and
// compare the caller's OWN evidence (their own environment, their own observation — never the
// reveal's) to the reveal's DECLARED verdict.

// ScenarioExecutor runs one sealed scenario and reports the caller's OWN observed outcome — never a
// value this package inspects or stores; it exists only to be compared to the reveal's declared
// verdict.
type ScenarioExecutor interface {
	Execute(ctx context.Context, path, body string) (outcome, detail string, err error)
}

// ReplayResult is one `argus replay` invocation. ExitCode: 0 when the caller's own evidence matches
// the reveal's declared verdict, 1 when it does not, 2 on a refusal (the artefact digest the caller
// named with --against does not match the reveal's own declared artifact_digest).
type ReplayResult struct {
	ScenarioID      string
	Refused         bool
	RefusalReason   string
	DeclaredDigest  string
	AgainstDigest   string
	DeclaredVerdict string
	ObservedOutcome string
	ObservedDetail  string
	Match           bool
}

// ExitCode is the process exit code `argus replay` reports for this result.
func (r ReplayResult) ExitCode() int {
	switch {
	case r.Refused:
		return 2
	case r.Match:
		return 0
	default:
		return 1
	}
}

// Replay refuses (exit 2) when against does not equal doc's own declared ArtifactDigest — the
// caller's assertion of what they deployed must name the SAME artefact this reveal certified, or
// there is nothing to replay against. Otherwise it runs scenarioID through exec and compares the
// caller's own observed outcome to the reveal's declared verdict for that scenario.
func Replay(ctx context.Context, doc *Document, scenarioID, against string, exec ScenarioExecutor) (ReplayResult, error) {
	res := ReplayResult{ScenarioID: scenarioID, DeclaredDigest: doc.ArtifactDigest, AgainstDigest: against}
	if against != doc.ArtifactDigest {
		res.Refused = true
		res.RefusalReason = fmt.Sprintf("--against %s does not match the reveal's declared artifact_digest %s", against, doc.ArtifactDigest)
		return res, nil
	}

	var declared string
	var haveVerdict bool
	for _, v := range doc.Verdicts {
		if v.ID == scenarioID {
			declared, haveVerdict = v.Outcome, true
			break
		}
	}
	if !haveVerdict {
		return ReplayResult{}, fmt.Errorf("reveal: scenario %s carries no declared verdict in this reveal", scenarioID)
	}
	res.DeclaredVerdict = declared

	sf, ok := findSealedFile(doc, scenarioID)
	if !ok {
		return ReplayResult{}, fmt.Errorf("reveal: scenario %s is not among this reveal's sealed files", scenarioID)
	}
	outcome, detail, err := exec.Execute(ctx, sf.Path, sf.Body)
	if err != nil {
		return ReplayResult{}, fmt.Errorf("reveal: replay execution: %w", err)
	}
	res.ObservedOutcome, res.ObservedDetail = outcome, detail
	res.Match = outcome == declared
	return res, nil
}

func findSealedFile(doc *Document, scenarioID string) (SealedFile, bool) {
	for _, f := range doc.SealedFiles {
		if f.ID == scenarioID {
			return f, true
		}
	}
	return SealedFile{}, false
}
