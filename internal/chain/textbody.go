package chain

import "encoding/json"

// ── A TEXT BODY (the rule itself is scenario.TextBodyOf) ────────────────────────────
//
// For an http step flagged WithTextBody, bodyTmpl is the string's TEXT with the run's own ${cid} / ${VAR}
// values already filled in RAW (argus.chainTextBody), not a JSON document. This file is what the run does
// with it: bind ${saved.<var>} as raw text, and show the money guard the only form of it that guard has ever
// been shown for a string body.

// WithTextBody marks the step's body template as TEXT, sent as it stands (no JSON quoting, no escaping).
// false is no option at all: the step is byte-identical to one built before this existed.
func WithTextBody(on bool) HTTPOption {
	return func(o *httpOpts) { o.textBody = on }
}

// bindSavedText binds ${saved.<var>} into a text body. Every value is spliced RAW: a quote or a backslash in
// a saved string stays one quote or one backslash (bindSavedBody escapes them, because there the result is
// a JSON document). A variable saved with the `json` form is spliced as its JSON text, which is already how
// that kind is stored. A reference no earlier step saved is refused by name, as everywhere else.
func bindSavedText(s string, vars map[string]string) (string, error) {
	var missing []string
	out := savedRefRe.ReplaceAllStringFunc(s, func(m string) string {
		name := savedRefRe.FindStringSubmatch(m)[1]
		v, ok := vars[name]
		if !ok {
			missing = append(missing, name)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return "", unresolvedSavedError(missing)
	}
	return out, nil
}

// guardViewOfText is the body the money guard (scenario.EvaluateSpendAmount) is shown for a text body: the
// text as a JSON string, which is what the guard was shown for a string body before text bodies existed.
// A JSON string has no object to read an amount_field from, so an allowlisted spending route sent a text
// body is REFUSED (fail-closed), exactly as it was. Showing the guard the raw text instead would let text
// that happens to read as JSON (`{"amount":5}`) through, which was never sendable.
func guardViewOfText(text string) string {
	b, err := json.Marshal(text)
	if err != nil { // a string always marshals; this keeps the guard fail-closed if that ever changed
		return "\x00"
	}
	return string(b)
}
