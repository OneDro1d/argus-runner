// Package compare is the pure core of the compare mission (ARGUS-CMP-2/-03): the
// canonical form and hash of a recorded output, the `## COMPARE` rules, the wire records, the judge
// and the result function that turns recorded runs into a table and a roll-up verdict.
//
// It imports nothing from this module and nothing outside the standard library, so the executor and
// the control plane can link the same bytes and cannot drift. It does no I/O, reads no clock and
// holds no state.
package compare

// The vocabulary of OutputRecord.Reason is CLOSED: a constant here, never free text and never built
// from a response. A reason is only ever set when State is StateNotRecorded.
const (
	// ReasonMaskNeedsJSON: a field mask is declared and the body is not JSON. A mask is never
	// silently skipped, so the output is not recorded instead.
	ReasonMaskNeedsJSON = "mask_needs_json"
	// ReasonPathRuleNeedsJSON: an Unordered or Tolerance path is declared and the body is not JSON.
	ReasonPathRuleNeedsJSON = "path_rule_needs_json"
	// ReasonBodyTooLarge: the body is over MaxBodyBytes; a prefix is never hashed as the whole.
	ReasonBodyTooLarge = "body_too_large"
	// ReasonLayerNotSupported: this layer or engine records nothing yet. The cell says "not
	// measured", never "identical".
	ReasonLayerNotSupported = "layer_not_supported"
	// ReasonHeaderNotAllowed: a declared header is on the refused list (credentials).
	ReasonHeaderNotAllowed = "header_not_allowed"
	// ReasonNoResponse: no response was received (transport failure); there is nothing to record.
	ReasonNoResponse = "no_response"
	// ReasonTooManySamples: more than MaxSamples samples of one check.
	ReasonTooManySamples = "too_many_samples"
	// ReasonTooManyValues: more than MaxToleranceValues numeric leaves matched Tolerance paths.
	ReasonTooManyValues = "too_many_values"
	// ReasonLoadNumbersOnly: the record of a `## LOAD` check that declares `Not Worse Than`. It carries the
	// load numbers (OutputRecord.Load) and NO output: no status, no part hash, no total hash, so the
	// measured-output judge can only read it as "nothing recorded to compare", never as identical or
	// differs (ARGUS-CMP-11).
	ReasonLoadNumbersOnly = "load_numbers_only"
)

var reasonText = map[string]string{
	ReasonMaskNeedsJSON:     "a field mask needs a JSON body",
	ReasonPathRuleNeedsJSON: "an unordered or tolerance path needs a JSON body",
	ReasonBodyTooLarge:      "larger than 1 MiB",
	ReasonLayerNotSupported: "layer not supported for comparison yet",
	ReasonHeaderNotAllowed:  "a declared header may carry credentials and is never recorded",
	ReasonNoResponse:        "no response was received",
	ReasonTooManySamples:    "more samples than a check may record",
	ReasonTooManyValues:     "more tolerant values than a check may record",
	ReasonLoadNumbersOnly:   "this check records load numbers, not an output",
}

// AllReasons lists the closed vocabulary in a fixed order.
func AllReasons() []string {
	return []string{
		ReasonMaskNeedsJSON, ReasonPathRuleNeedsJSON, ReasonBodyTooLarge, ReasonLayerNotSupported,
		ReasonHeaderNotAllowed, ReasonNoResponse, ReasonTooManySamples, ReasonTooManyValues, ReasonLoadNumbersOnly,
	}
}

// ValidReason reports whether r is one of the constants above. The empty string is not a reason.
func ValidReason(r string) bool {
	_, ok := reasonText[r]
	return ok
}

// ReasonText is the fixed sentence for a reason, or "" for anything that is not in the vocabulary.
func ReasonText(r string) string { return reasonText[r] }
