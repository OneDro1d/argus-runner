package chain

import (
	"bytes"
	"fmt"
	"unicode/utf8"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// bodyExcerpt is StepResult.BodyExcerpt: what an author reads of the response body when a
// chain http step's status claim missed. capped says the engine read only maxHTTPBodyBytes of a longer body.
//
// ⛔ THE ORDER IS THE POINT: the instance's credential scrub first, then every saved value (no exemption),
// and only THEN the cut. A cut made first could leave half a secret that no longer matches what is scrubbed.
// A body that is not text is never shown, only its size.
func bodyExcerpt(raw []byte, capped bool, vars map[string]string, scrub func(string) string) string {
	if len(raw) == 0 {
		return "(empty body)"
	}
	text := raw
	if capped {
		// the read cut may have split a multi-byte character; that alone does not make the body binary
		for i := 0; i < utf8.UTFMax-1 && len(text) > 0 && !utf8.Valid(text); i++ {
			text = text[:len(text)-1]
		}
	}
	if !utf8.Valid(text) || bytes.IndexByte(text, 0) >= 0 {
		return fmt.Sprintf("(%d bytes, not text)", len(raw))
	}
	s := string(text)
	if scrub != nil {
		s = scrub(s)
	}
	s = redactEverySaved(s, vars, "")
	if len(s) <= report.MaxBodyExcerpt {
		return s
	}
	cut := report.MaxBodyExcerpt
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + report.FailedClaimTruncatedSuffix
}
