package argus

// The run's half of a chain http step's TEXT body. The rule (which bodies are text) is
// scenario.TextBodyOf and the sending is chain.WithTextBody; this is the part that knows the run: which
// ${...} are the run's to fill in before the step is built.

import "github.com/OneDro1d/argus-runner/internal/scenario"

// chainTextBody returns the body template of an http step whose `body` is sent as text, and whether it is.
// The text arrives with the run's ${cid} / ${correlation_id} and environment values ALREADY filled in raw:
// parseChainSpec substituted the whole payload as JSON text, and decoding the JSON string undid the escaping,
// so a value holding `"` or `\` is one `"` or one `\` here. What is left to do is the same second pass the
// JSON form gets (substituteVars), without the JSON escaping that pass applies to a JSON document, so a
// value is never escaped twice. `${saved.<var>}` is left standing for the run (chain.bindSavedText).
func chainTextBody(st chainStepSpec, stepHeaders map[string]string, corr string) (string, bool) {
	own := make(map[string]string, len(stepHeaders))
	for k, v := range stepHeaders {
		own[k] = resolveVars(v, corr)
	}
	text, ok := scenario.TextBodyOf(st.Body, own)
	if !ok {
		return "", false
	}
	text, _ = substituteVars(text, corr, false, false)
	return text, true
}
