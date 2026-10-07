package compare

// records.go -- WHICH CHECKS RECORD AN OUTPUT TODAY (ARGUS-CMP-5 fix, PR #500).
//
// The control plane must not count a check as "compared" when no executor records an output for it: such a
// check can only ever read `not_measured`. RecordsOutput is the ONE named predicate, and it MIRRORS the
// executor's own dispatch in internal/argus (runOneScenario in argus.go and attachChainOutputs /
// beginJMeterCapture in outputs_capture.go):
//
//   - an `AMQP Load` check: records nothing (runAMQPLoad, a numbers-only load run);
//   - a chain (tag `chain`): records when the chain has at least one `http` step (attachChainOutputs);
//   - an `mcp` or `ui` check: `not_recorded / layer_not_supported` (withNotSupported);
//   - any other check that declares `## LOAD`: records nothing (beginJMeterCapture / applyCaptureProp), an
//     `HTTP Load` ramp included (runHTTPLoad records nothing; the executor sets Load for it);
//   - any other check: records only through the JMeter templates whose JSR223 block writes the capture side
//     files, CaptureTemplateBases (the template the executor RUNS: argus.RuntimeTemplateBase).
//
// internal/argus builds the facts from a parsed scenario (argus.RecordsComparedOutput) and its own
// jmeterCaptureTemplates IS CaptureTemplateBases, so the two cannot drift on the template list.

// CaptureTemplateBases are the JMeter template bases whose check records an output in run mode `compare`.
var CaptureTemplateBases = map[string]bool{"http-ingestion": true, "http-idempotency": true}

// RecorderFacts is what the predicate needs to know about one check.
type RecorderFacts struct {
	AMQPLoad         bool   // the primary layer is `AMQP Load`
	Chain            bool   // the check carries the `chain` tag
	ChainHasHTTPStep bool   // ... and its steps include an `http` step
	MCP              bool   // the check carries the `mcp` tag
	UI               bool   // the check carries the `ui` tag
	Load             bool   // the check declares `## LOAD`
	TemplateBase     string // the JMeter template the executor runs (argus.RuntimeTemplateBase)
}

// RecordsOutput says whether an executor in run mode `compare` records an output for a check with these facts.
func RecordsOutput(f RecorderFacts) bool {
	switch {
	case f.AMQPLoad:
		return false
	case f.Chain:
		return f.ChainHasHTTPStep
	case f.MCP, f.UI, f.Load:
		return false
	}
	return CaptureTemplateBases[f.TemplateBase]
}
