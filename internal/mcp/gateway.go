package mcp

import "regexp"

// JSONStringAt parses text as a JSON document and returns the value at the dot path (the path syntax the claims and
// `save` use) when that value is a JSON STRING, decoded. ok is false when text is not JSON, the path does not resolve,
// or the value is anything but a string. It is how an `extract` with a `path` reads a gateway answer whose text is
// a JSON object holding the data inside a string.
func JSONStringAt(text, path string) (string, bool) {
	doc, ok := decodeJSON(text)
	if !ok {
		return "", false
	}
	v, found := lookupPath(doc, path, false)
	if !found {
		return "", false
	}
	s, isStr := v.(string)
	return s, isStr
}

// AnswerMatches reports whether re matches what the system said: the text of result.content (every block, joined as
// the claims read it) or, when it answered with a JSON-RPC error, that error object as the claims read it. It is how
// a step's declared `unavailable` pattern is applied. There is no default pattern anywhere.
func AnswerMatches(r CallResult, re *regexp.Regexp) bool {
	if re == nil {
		return false
	}
	if re.MatchString(ContentText(r)) {
		return true
	}
	return r.JSONRPCError != nil && re.MatchString(errorObjectText(r.JSONRPCError))
}
