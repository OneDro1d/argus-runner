package scenario

import (
	"encoding/json"
	"strings"
)

// ── A CHAIN http STEP'S TEXT BODY ──────────────────────────────────────────────────
//
// A chain step's `body` is a JSON value in the scenario file, and what the SUT receives is that value's
// JSON TEXT: `{"a":1}` is sent as those seven bytes, and `"hello"` as the SEVEN bytes `"hello"`, quotes
// included, under Content-Type application/json (the default when the body is present and the step set
// no header). A SUT that wants a plain-text request (a webhook that takes `text/plain`, a form post, a
// line protocol) could not be sent one.
//
// THE RULE, exactly: a JSON-STRING `body` on a step whose OWN `headers` declare a Content-Type that is NOT
// JSON (not `application/json`, not `text/json`, not `<type>/<x>+json`, compared case-insensitively, parameters ignored) is
// sent as the string's TEXT: unquoted, unescaped. Every other combination is byte-identical to before:
// a JSON-string body with no Content-Type header is still the quoted JSON string under application/json;
// an object, array, number or boolean body is still its JSON text whatever the header says; `null` is
// still no body.
//
// Only the step's own header counts: a Content-Type that arrives from the scenario's TRIGGER headers is
// the default for every step and says nothing about this body's shape, so a chain that sets one there keeps
// today's bytes.

// ContentTypeIsJSON reports whether a Content-Type header value names JSON: the media type (parameters such
// as `; charset=utf-8` dropped, case ignored) is `application/json`, `text/json` or ends in `+json`.
func ContentTypeIsJSON(contentType string) bool {
	mt, _, _ := strings.Cut(contentType, ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	return mt == "application/json" || mt == "text/json" || strings.HasSuffix(mt, "+json")
}

// declaredContentType returns the step's own Content-Type value (header names compare case-insensitively),
// and whether it declares one. An empty value declares nothing: net/http would not send it either.
func declaredContentType(ownHeaders map[string]string) (string, bool) {
	for k, v := range ownHeaders {
		if strings.EqualFold(strings.TrimSpace(k), "Content-Type") && strings.TrimSpace(v) != "" {
			return v, true
		}
	}
	return "", false
}

// TextBodyOf decides whether an http step's body is sent as text, and returns the text. body is the step's
// `body` as the file wrote it; ownHeaders are the step's OWN headers with their ${...} already filled in.
// ok=false is every other case, and the caller then does exactly what it did before this rule existed.
func TextBodyOf(body json.RawMessage, ownHeaders map[string]string) (text string, ok bool) {
	b := strings.TrimSpace(string(body))
	if len(b) == 0 || b[0] != '"' { // only a JSON string can be text; null, objects, arrays, numbers, booleans are not
		return "", false
	}
	ct, declared := declaredContentType(ownHeaders)
	if !declared || ContentTypeIsJSON(ct) || strings.Contains(ct, "${") {
		// a Content-Type that still holds a placeholder (a ${saved.x}, bound only at run time) is not known
		// here, so it is not guessed at: the body keeps today's form.
		return "", false
	}
	if err := json.Unmarshal([]byte(b), &text); err != nil {
		return "", false
	}
	return text, true
}
