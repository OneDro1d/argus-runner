package mcp

import (
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// THE JUDGEMENT OF ONE BODY CLAIM, ALL FORMS, IN ONE PLACE.
//
// bodyAssertMiss, BodyAssertsMissReason and BodyAssertObservation used to each re-walk the answer and each
// re-decide "is this a miss". With nine more ops and the `every` form that is three copies of a rule that
// must agree (a claim is LISTED as failed exactly when the judge COUNTED it as a miss), so all three now read
// ONE result, evalAssert.
//
// Two rules decide most of the new cases, and both are the R4 honesty the grammar already keeps:
//
//   - A claim that is SCOPED TO A FIELD and cannot be evaluated is a MISS, never a pass. That includes the
//     negatives: "the field does not contain X" must not pass because the field is not there, or because the
//     answer is not JSON at all — that is what `body has no <field>` is for.
//   - `body has no <field>` is the one claim that LOOKS for the absence, so it holds when the answer IS JSON
//     and the path does not resolve (or resolves to JSON null). An answer that is not JSON is a miss: it
//     cannot be evaluated, and "no such field" would be a guess.

// NotJSONObserved is the observed text of a claim that needs a JSON answer (`body has no <field>`) and
// was given something else.
const NotJSONObserved = "(the answer is not JSON)"

// EveryNotArrayObserved is the observed text of an `every` claim whose array path did not resolve to a JSON
// array (or whose answer is not JSON).
const EveryNotArrayObserved = "(array path absent, not an array, or the answer is not JSON)"

type evalResult struct {
	miss bool
	// nonNumeric: the miss was a numeric claim finding an observed value that is not a number. Distinct
	// from an ordinary miss so a caller can name WHAT went wrong without ever echoing the reference.
	nonNumeric bool
	// observed is, for a miss, what the answer showed for the claim (see BodyAssertObservation). Empty when
	// the claim held.
	observed string
}

// decodeJSON reports whether text is a JSON document and returns it.
func decodeJSON(text string) (any, bool) {
	var doc any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &doc); err != nil {
		return nil, false
	}
	return doc, true
}

// decodeJSONExact is decodeJSON keeping every number AS WRITTEN (json.Number) instead of as a float64.
//
// `==` and `within` compare exactly (big.Rat), and an exact comparison of a value that already went through
// a float64 is not exact: above 2^53 two different integers decode to the same float, so
// `== 1000000000000000000` HELD on an answer of 1000000000000000001, and `== 12345678901234567890` MISSED on
// that very number (it had become …567000). Amounts in a chain's smallest unit are exactly that size. Only
// the two exact ops read through this decoder: every older op keeps the float64 decoding it always had, so
// what `equals` and `containing` see for a number (`1.0` reads `1`) does not move.
func decodeJSONExact(text string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(text)))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, false
	}
	if dec.More() { // trailing content: json.Unmarshal refuses it, so this must too
		return nil, false
	}
	return doc, true
}

// lookupPath navigates a dotted path through a decoded JSON document. found is false when a segment does not
// resolve; the value found may be nil (a JSON null). self lets "." (alone) name the document itself, which
// is how an `every` claim reaches the element of an array of scalars.
func lookupPath(doc any, path string, self bool) (any, bool) {
	if self && path == "." {
		return doc, true
	}
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		switch v := cur.(type) {
		case map[string]any:
			nxt, ok := v[seg]
			if !ok {
				return nil, false
			}
			cur = nxt
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(v) {
				return nil, false
			}
			cur = v[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// scalarText is a decoded value as the text the string ops compare: a string as itself, anything else as
// its JSON. ok is false for a JSON null (absent for every claim but `has no`).
func scalarText(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		return t, true
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
}

// evalAssert judges ONE claim against the answer text.
func evalAssert(text string, a BodyAssert) evalResult {
	doc, jsonOK := decodeJSON(text)
	if a.Op == BodyEQOp || a.Op == BodyWithinOp {
		doc, jsonOK = decodeJSONExact(text) // every digit kept: see decodeJSONExact
	}
	if a.Every != "" {
		return evalEvery(doc, jsonOK, a)
	}
	return evalOn(doc, jsonOK, text, a, false)
}

// evalEvery judges a claim on every element of an array. An EMPTY array holds: no element breaks the
// claim (the authoring skill says so, and advises pairing it with a claim that the list is not empty). A
// path that does not resolve to an array is a miss. The first breaking element is named by index, with its
// own observed value.
func evalEvery(doc any, jsonOK bool, a BodyAssert) evalResult {
	if !jsonOK {
		return evalResult{miss: true, observed: EveryNotArrayObserved}
	}
	arrV := doc
	if a.Every != "$" {
		v, found := lookupPath(doc, a.Every, false)
		if !found {
			return evalResult{miss: true, observed: EveryNotArrayObserved}
		}
		arrV = v
	}
	arr, isArr := arrV.([]any)
	if !isArr {
		return evalResult{miss: true, observed: EveryNotArrayObserved}
	}
	inner := a
	inner.Every = ""
	if inner.Field == "" {
		inner.Field = "."
	}
	for i, el := range arr {
		if r := evalOn(el, true, "", inner, true); r.miss {
			r.observed = fmt.Sprintf("element %d: %s", i, r.observed)
			return r
		}
	}
	return evalResult{}
}

// evalOn judges a claim against one decoded document (the whole answer, or one element of an `every`).
// whole is the answer's text, what an unscoped claim is judged against.
func evalOn(doc any, jsonOK bool, whole string, a BodyAssert, inEvery bool) evalResult {
	if a.Op == BodyAbsentOp {
		if !jsonOK {
			return evalResult{miss: true, observed: NotJSONObserved}
		}
		v, found := lookupPath(doc, a.Field, inEvery)
		if !found || v == nil {
			return evalResult{}
		}
		b, _ := json.Marshal(v)
		return evalResult{miss: true, observed: string(b)}
	}
	target := whole
	if a.Scoped() {
		if !jsonOK {
			return evalResult{miss: true, observed: FieldAbsentObserved}
		}
		v, found := lookupPath(doc, a.Field, inEvery)
		t, ok := scalarText(v)
		if !found || !ok {
			return evalResult{miss: true, observed: FieldAbsentObserved}
		}
		target = t
	}
	miss, nonNumeric := judgeText(target, a)
	if !miss {
		return evalResult{}
	}
	return evalResult{miss: true, nonNumeric: nonNumeric, observed: target}
}

// judgeText applies a claim's operator to the text it targets.
func judgeText(target string, a BodyAssert) (miss, nonNumeric bool) {
	switch a.Op {
	case BodyExistsOp:
		return false, false // reaching here means the field resolved, which IS the assertion
	case BodyMatchesOp:
		re, err := regexp.Compile(a.Value)
		// A regex that does not compile is refused at validate/preflight before any call is made,
		// so here it can only be a miss — never a silent pass.
		return err != nil || !re.MatchString(target), false
	case BodyNotMatchesOp:
		re, err := regexp.Compile(a.Value)
		// The mirror: an uncompilable regex cannot be shown NOT to match, so it is a miss here too.
		return err != nil || re.MatchString(target), false
	case BodyGTOp, BodyGTEOp, BodyLTOp, BodyLTEOp:
		return numericAssertMiss(target, a.Value, a.Op)
	case BodyEQOp, BodyWithinOp:
		return exactNumericMiss(target, a)
	case BodyEqualsOp:
		return target != a.Value, false // EXACT match — unlike BodyContainsOp, extra text either side is a miss
	case BodyNotEqualsOp:
		return target == a.Value, false
	case BodyNotContainsOp:
		return strings.Contains(target, a.Value), false
	default:
		return !strings.Contains(target, a.Value), false
	}
}

// exactNumRe is the number spelling `==` and `within` accept, on the observed value and the reference alike:
// a decimal with an optional exponent of at most three digits. It is deliberately NOT strconv.ParseFloat's
// grammar, which also reads `inf`, `nan` and hex floats, and not big.Rat's, which also reads fractions and
// base prefixes: an amount that is not a plain decimal is a non-numeric miss, never silently a number.
var exactNumRe = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]{1,3})?$`)

// tolRe is a `within` tolerance: a non-negative number, optionally followed by `%`.
var tolRe = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]{1,3})?%?$`)

// ExactNumber reports whether s is a number in the spelling `==` and `within` accept.
func ExactNumber(s string) bool { _, ok := exactNum(s); return ok }

// ValidTolerance reports whether s is a usable `within` tolerance (a non-negative number, or one followed by `%`).
func ValidTolerance(s string) bool {
	s = strings.TrimSpace(s)
	if !tolRe.MatchString(s) {
		return false
	}
	_, ok := exactNum(strings.TrimSuffix(s, "%"))
	return ok
}

func exactNum(s string) (*big.Rat, bool) {
	s = strings.TrimSpace(s)
	if !exactNumRe.MatchString(s) {
		return nil, false
	}
	return new(big.Rat).SetString(s)
}

// exactNumericMiss judges `==` and `within`. The comparison is EXACT (big.Rat), not float64: with floats,
// 1.3 within 0.3 of 1.0 is a miss by one unit in the last place, and a tolerance claim is exactly the claim
// whose boundary an author tests. nonNumeric reports an observed value that is not a number; an unusable
// reference or tolerance is refused at authoring (and bound-checked for `${saved.x}`), so here it is a miss.
func exactNumericMiss(observed string, a BodyAssert) (miss, nonNumeric bool) {
	ov, ok := exactNum(observed)
	if !ok {
		return true, true
	}
	ref, ok := exactNum(a.Value)
	if !ok {
		return true, false
	}
	diff := new(big.Rat).Sub(ov, ref)
	if a.Op == BodyEQOp {
		return diff.Sign() != 0, false
	}
	tol := strings.TrimSpace(a.Tol)
	if !ValidTolerance(tol) {
		return true, false
	}
	pct := strings.HasSuffix(tol, "%")
	tv, _ := exactNum(strings.TrimSuffix(tol, "%"))
	limit := tv
	if pct {
		// |observed - ref| <= |ref| * pct / 100. A reference of 0 gives a limit of 0, so such a claim holds
		// only when the observed value is exactly 0.
		limit = new(big.Rat).Mul(new(big.Rat).Abs(ref), tv)
		limit.Quo(limit, big.NewRat(100, 1))
	}
	return new(big.Rat).Abs(diff).Cmp(limit) > 0, false
}
