package compare

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Keys is the closed list of `## COMPARE` keys, in the order the docs give them.
func Keys() []string {
	return []string{"Reference", "Output", "Mask", "Unordered", "Tolerance", "Repeats", "Agreement", "Not Worse Than", "Steps"}
}

var refusedHeaders = map[string]bool{
	"set-cookie": true, "authorization": true, "proxy-authorization": true,
	"www-authenticate": true, "cookie": true,
}

// RefusedHeader reports whether a header may carry a credential and is therefore never recorded,
// whatever the check declares. Case-insensitive.
func RefusedHeader(name string) bool { return refusedHeaders[strings.ToLower(strings.TrimSpace(name))] }

type builder struct {
	probs []Problem
}

func (b *builder) bad(kv RawKV, format string, a ...any) {
	b.probs = append(b.probs, Problem{Key: kv.Key, Line: kv.Line, Msg: fmt.Sprintf(format, a...)})
}

// splitList splits on sep, trims every item and refuses an empty one.
func splitList(kv RawKV, sep string, b *builder) ([]string, bool) {
	var out []string
	ok := true
	for _, it := range strings.Split(kv.Value, sep) {
		it = strings.TrimSpace(it)
		if it == "" {
			b.bad(kv, "**%s** has an empty entry (entries are separated by %q)", kv.Key, sep)
			ok = false
			continue
		}
		out = append(out, it)
	}
	return out, ok
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// BuildRules is the ONE reader of the `## COMPARE` section: the scenario parser keeps the result only
// when there is no problem, the validator turns every problem into a line-level error, and the
// executor refuses a check with a problem in compare mode. They cannot disagree about what the
// section means.
func BuildRules(kvs []RawKV) (*Rules, []Problem) {
	b := &builder{}
	known := map[string]bool{}
	for _, k := range Keys() {
		known[k] = true
	}
	vals := map[string]RawKV{}
	for _, kv := range kvs {
		kv.Key, kv.Value = strings.TrimSpace(kv.Key), strings.TrimSpace(kv.Value)
		switch {
		case !known[kv.Key]:
			b.probs = append(b.probs, Problem{Key: kv.Key, Line: kv.Line, UnknownKey: true,
				Msg: fmt.Sprintf("has an unknown key **%s** — accepted: %s", kv.Key, strings.Join(Keys(), ", "))})
		case func() bool { _, dup := vals[kv.Key]; return dup }():
			b.bad(kv, "**%s** is declared twice", kv.Key)
		default:
			vals[kv.Key] = kv
		}
	}

	r := &Rules{
		Output: OutputSel{Status: true, Body: true, Headers: []string{}},
		Mask:   []string{}, Unordered: []string{}, Tolerance: []Tolerance{}, NotWorseThan: []Band{}, Steps: []string{},
		Repeats: 1, Agreement: 100,
	}

	ref, ok := vals["Reference"]
	if !ok {
		b.probs = append(b.probs, Problem{Key: "Reference", Msg: "is missing **Reference** (measured, fixed or property)"})
	} else {
		switch Reference(ref.Value) {
		case RefMeasured, RefFixed, RefProperty:
			r.Reference = Reference(ref.Value)
		default:
			b.bad(ref, "**Reference** must be measured, fixed or property, got %q", ref.Value)
		}
	}
	outputKinds := r.Reference == "" || r.Reference == RefMeasured // keys only a measured check reads

	if kv, ok := vals["Output"]; ok {
		if !outputKinds {
			b.bad(kv, "**Output** is not read when **Reference** is %s (the check's own claims decide); delete the line", r.Reference)
		} else {
			parseOutput(kv, r, b)
		}
	}

	paths := func(key string, max int, bodyOnly bool) []Path {
		kv, ok := vals[key]
		if !ok {
			return nil
		}
		if !outputKinds {
			b.bad(kv, "**%s** is not read when **Reference** is %s (the check's own claims decide); delete the line", key, r.Reference)
			return nil
		}
		items, ok := splitList(kv, ";", b)
		if !ok {
			return nil
		}
		if len(items) > max {
			b.bad(kv, "**%s** lists %d paths, at most %d are allowed", key, len(items), max)
		}
		seen := map[string]bool{}
		var out []Path
		for _, it := range items {
			p, err := ParsePath(it)
			switch {
			case err != nil:
				b.bad(kv, "**%s** %v", key, err)
			case bodyOnly && p.IsHeader():
				b.bad(kv, "**%s** %q: only a body path (`$...`) is allowed here", key, it)
			case seen[p.String()]:
				b.bad(kv, "**%s** lists %s twice", key, p.String())
			default:
				seen[p.String()] = true
				out = append(out, p)
			}
		}
		return out
	}

	for _, p := range paths("Mask", MaxMasks, false) {
		kv := vals["Mask"]
		if p.IsHeader() {
			if !hasString(r.Output.Headers, p.Header()) {
				b.bad(kv, "**Mask** %s masks a header that **Output** does not compare (add header:%s to **Output**)", p.String(), p.Header())
				continue
			}
		} else if !r.Output.Body {
			b.bad(kv, "**Mask** %s needs the body in **Output**", p.String())
			continue
		}
		r.Mask = append(r.Mask, p.String())
	}
	for _, p := range paths("Unordered", MaxUnordered, true) {
		if !r.Output.Body {
			b.bad(vals["Unordered"], "**Unordered** %s needs the body in **Output**", p.String())
			continue
		}
		r.Unordered = append(r.Unordered, p.String())
	}
	if kv, ok := vals["Tolerance"]; ok {
		if !outputKinds {
			b.bad(kv, "**Tolerance** is not read when **Reference** is %s (the check's own claims decide); delete the line", r.Reference)
		} else {
			parseTolerance(kv, r, b)
		}
	}
	// A tolerant value is paired with the reference's by POSITION, which an Unordered array does not
	// keep: the hashes would agree and the values would not. Refused, not papered over.
	if kv, ok := vals["Tolerance"]; ok {
		for _, t := range r.Tolerance {
			tp, _ := ParsePath(t.Path)
			for _, u := range r.Unordered {
				if up, err := ParsePath(u); err == nil && up.atOrUnder(tp) {
					b.bad(kv, "**Tolerance** %s lies at or under the **Unordered** path %s: values in an unordered array are paired by position, so a re-ordered array would read as different; drop the tolerance or the unordered rule", t.Path, u)
				}
			}
		}
	}
	if kv, ok := vals["Steps"]; ok {
		if !outputKinds {
			b.bad(kv, "**Steps** is not read when **Reference** is %s (the check's own claims decide); delete the line", r.Reference)
		} else {
			parseSteps(kv, r, b)
		}
	}
	if kv, ok := vals["Repeats"]; ok {
		if !repeatsRE.MatchString(kv.Value) {
			b.bad(kv, "**Repeats** must be a whole number of runs between 1 and %d written plainly (no sign, no leading zero), got %q", MaxRepeats, kv.Value)
		} else if n, _ := strconv.Atoi(kv.Value); n < 1 || n > MaxRepeats {
			b.bad(kv, "**Repeats** = %d is out of bounds [1, %d]", n, MaxRepeats)
		} else {
			r.Repeats = n
		}
	}
	agreementDeclared := false
	if kv, ok := vals["Agreement"]; ok {
		f, err := 0.0, error(nil)
		if agreementRE.MatchString(kv.Value) {
			f, err = strconv.ParseFloat(strings.TrimSuffix(kv.Value, "%"), 64)
		} else {
			err = fmt.Errorf("not a plain percentage")
		}
		switch {
		case err != nil || !finite(f):
			b.bad(kv, "**Agreement** must be a plain decimal percentage such as 100%% or 95.5%%, got %q", kv.Value)
		case f < MinAgreementPct || f > 100:
			b.bad(kv, "**Agreement** = %v%% is out of bounds [%v%%, 100%%]", f, MinAgreementPct)
		default:
			r.Agreement = f
			agreementDeclared = true
		}
	}
	if r.Reference == RefProperty && !agreementDeclared {
		if _, bad := vals["Agreement"]; !bad {
			b.probs = append(b.probs, Problem{Key: "Agreement", Msg: "**Reference**: property needs **Agreement** (the share of runs in which the property must hold, for example 100%)"})
		}
	}
	if kv, ok := vals["Not Worse Than"]; ok {
		parseBands(kv, r, b)
	}

	sort.Strings(r.Mask)
	sort.Strings(r.Unordered)
	sort.Strings(r.Output.Headers)
	sort.Strings(r.Steps)
	sort.Slice(r.Tolerance, func(i, j int) bool {
		if r.Tolerance[i].Path != r.Tolerance[j].Path {
			return r.Tolerance[i].Path < r.Tolerance[j].Path
		}
		return r.Tolerance[i].Kind < r.Tolerance[j].Kind
	})
	sort.Slice(r.NotWorseThan, func(i, j int) bool { return r.NotWorseThan[i].Metric < r.NotWorseThan[j].Metric })

	if len(b.probs) > 0 {
		return nil, b.probs
	}
	return r, nil
}

var (
	repeatsRE   = regexp.MustCompile(`^[1-9][0-9]?$`)
	agreementRE = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,6})?%$`)
	decimalRE   = regexp.MustCompile(`^(0|[1-9][0-9]{0,14})(\.[0-9]{1,15})?$`)
)

// parsePlainDecimal reads a non-negative decimal written plainly: digits, an optional fraction, no
// sign, exponent, underscore, hex or spaces, no leading zero.
func parsePlainDecimal(s string) (float64, error) {
	if !decimalRE.MatchString(s) {
		return 0, fmt.Errorf("not a plain decimal")
	}
	return strconv.ParseFloat(s, 64)
}

func hasString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func parseOutput(kv RawKV, r *Rules, b *builder) {
	items, ok := splitList(kv, ",", b)
	if !ok {
		return
	}
	sel := OutputSel{Headers: []string{}}
	seen := map[string]bool{}
	for _, it := range items {
		key := it
		if p, err := ParsePath(it); err == nil && p.IsHeader() {
			key = p.String()
		}
		if seen[key] {
			b.bad(kv, "**Output** lists %s twice", key)
			continue
		}
		seen[key] = true
		switch {
		case it == "status":
			sel.Status = true
		case it == "body":
			sel.Body = true
		case strings.HasPrefix(it, "header:"):
			p, err := ParsePath(it)
			switch {
			case err != nil:
				b.bad(kv, "**Output** %v", err)
			case RefusedHeader(p.Header()):
				b.bad(kv, "**Output** header:%s is refused: it may carry a credential (refused: Set-Cookie, Authorization, Proxy-Authorization, WWW-Authenticate, Cookie)", p.Header())
			default:
				sel.Headers = append(sel.Headers, p.Header())
			}
		default:
			b.bad(kv, "**Output** entry %q is not allowed: use status, body or header:<Name>", it)
		}
	}
	if len(sel.Headers) > MaxHeaders {
		b.bad(kv, "**Output** names %d headers, at most %d are allowed", len(sel.Headers), MaxHeaders)
	}
	r.Output = sel
}

func parseTolerance(kv RawKV, r *Rules, b *builder) {
	items, ok := splitList(kv, ";", b)
	if !ok {
		return
	}
	if len(items) > MaxTolerances {
		b.bad(kv, "**Tolerance** has %d rules, at most %d are allowed", len(items), MaxTolerances)
	}
	seen := map[string]bool{}
	for _, it := range items {
		f := strings.Fields(it)
		if len(f) != 3 {
			b.bad(kv, "**Tolerance** entry %q must read `<path> abs <number>` or `<path> rel <number>`", it)
			continue
		}
		p, err := ParsePath(f[0])
		switch {
		case err != nil:
			b.bad(kv, "**Tolerance** %v", err)
			continue
		case p.IsHeader():
			b.bad(kv, "**Tolerance** %q: only a body path (`$...`) can carry a number", f[0])
			continue
		case f[1] != "abs" && f[1] != "rel":
			b.bad(kv, "**Tolerance** entry %q: the kind is abs or rel, got %q", it, f[1])
			continue
		}
		v, err := parsePlainDecimal(f[2])
		if err != nil || !finite(v) || v <= 0 {
			b.bad(kv, "**Tolerance** entry %q: the amount must be a positive number, got %q", it, f[2])
			continue
		}
		if seen[p.String()] {
			b.bad(kv, "**Tolerance** has two rules for %s (one per path)", p.String())
			continue
		}
		seen[p.String()] = true
		if !r.Output.Body {
			b.bad(kv, "**Tolerance** %s needs the body in **Output**", p.String())
			continue
		}
		r.Tolerance = append(r.Tolerance, Tolerance{Path: p.String(), Kind: f[1], Value: v})
	}
}

func parseBands(kv RawKV, r *Rules, b *builder) {
	items, ok := splitList(kv, ";", b)
	if !ok {
		return
	}
	seen := map[string]bool{}
	for _, it := range items {
		f := strings.Fields(it)
		if len(f) != 2 {
			b.bad(kv, "**Not Worse Than** entry %q must read `p95 20%%` or `error_rate 0.5pp`", it)
			continue
		}
		metric := f[0]
		var unit string
		var max float64
		switch metric {
		case "p50", "p95", "p99":
			unit, max = "%", MaxBandPercent
		case "error_rate":
			unit, max = "pp", MaxBandPoints
		default:
			b.bad(kv, "**Not Worse Than** metric %q is not allowed: p50, p95, p99 (with a %%) or error_rate (with pp)", metric)
			continue
		}
		num, has := strings.CutSuffix(f[1], unit)
		v, err := parsePlainDecimal(num)
		switch {
		case !has || err != nil || !finite(v):
			b.bad(kv, "**Not Worse Than** entry %q: %s takes a number followed by %s", it, metric, unit)
			continue
		case v <= 0 || v > max:
			b.bad(kv, "**Not Worse Than** entry %q is out of bounds (0, %v]", it, max)
			continue
		case seen[metric]:
			b.bad(kv, "**Not Worse Than** lists %s twice", metric)
			continue
		}
		seen[metric] = true
		r.NotWorseThan = append(r.NotWorseThan, Band{Metric: metric, Value: v, Unit: unit})
	}
}

func parseSteps(kv RawKV, r *Rules, b *builder) {
	items, ok := splitList(kv, ",", b)
	if !ok {
		return
	}
	if len(items) > MaxSteps {
		b.bad(kv, "**Steps** names %d steps, at most %d are allowed", len(items), MaxSteps)
	}
	seen := map[string]bool{}
	for _, it := range items {
		if seen[it] {
			b.bad(kv, "**Steps** lists %q twice", it)
			continue
		}
		seen[it] = true
		r.Steps = append(r.Steps, it)
	}
}

// normalized returns a copy with every list sorted, de-duplicated and non-nil, so hand-built rules
// encode like parsed ones.
func (r *Rules) normalized() Rules {
	n := *r
	uniq := func(in []string) []string {
		out := append([]string{}, in...)
		sort.Strings(out)
		w := out[:0]
		for i, s := range out {
			if i == 0 || s != out[i-1] {
				w = append(w, s)
			}
		}
		return w
	}
	n.Output.Headers = uniq(r.Output.Headers)
	n.Mask = uniq(r.Mask)
	n.Unordered = uniq(r.Unordered)
	n.Steps = uniq(r.Steps)
	n.Tolerance = append([]Tolerance{}, r.Tolerance...)
	sort.Slice(n.Tolerance, func(i, j int) bool {
		if n.Tolerance[i].Path != n.Tolerance[j].Path {
			return n.Tolerance[i].Path < n.Tolerance[j].Path
		}
		return n.Tolerance[i].Kind < n.Tolerance[j].Kind
	})
	n.NotWorseThan = append([]Band{}, r.NotWorseThan...)
	sort.Slice(n.NotWorseThan, func(i, j int) bool { return n.NotWorseThan[i].Metric < n.NotWorseThan[j].Metric })
	if n.Repeats == 0 {
		n.Repeats = 1
	}
	n.Agreement = EffectiveAgreement(n.Agreement)
	return n
}

// CanonicalJSON is the stable encoding of the rules (defaults applied, lists sorted, no nulls).
func (r *Rules) CanonicalJSON() []byte {
	n := r.normalized()
	b, _ := json.Marshal(n)
	return b
}

// RulesHash is the rules_hash of a set: sha256 over every rules-bearing check's canonical JSON, in
// path order. The path is part of what is hashed.
func RulesHash(entries []PathRules) string {
	cp := append([]PathRules(nil), entries...)
	sort.SliceStable(cp, func(i, j int) bool { return cp[i].Path < cp[j].Path })
	parts := []string{domainRules}
	for _, e := range cp {
		enc := "null"
		if e.Rules != nil {
			enc = string(e.Rules.CanonicalJSON())
		}
		parts = append(parts, strconv.Itoa(len(e.Path)), ":", e.Path, "\n", enc, "\n")
	}
	return sum(parts...)
}

// Warnings are the advisory lints of a valid section (design 2.2): an Agreement below 100% that
// Repeats cannot support statistically, with the number that could.
func Warnings(r *Rules) []string {
	if r == nil || EffectiveAgreement(r.Agreement) >= 100 {
		return nil
	}
	if need := MinRuns(r.Agreement); need > r.Repeats {
		p := strconv.FormatFloat(r.Agreement, 'f', -1, 64)
		return []string{fmt.Sprintf("## COMPARE **Agreement** %s%% cannot be supported by **Repeats** %d: that needs %d runs with no disagreement (95%% confidence). "+
			"The check still runs, and its cell will say the claim is not supported", p, r.Repeats, need)}
	}
	return nil
}

// Spec turns the rules into what the recorder reads. Tolerance rule indexes in the records are
// indexes into Rules.Tolerance as normalized (sorted by path).
func (r *Rules) Spec() Spec {
	n := r.normalized()
	s := Spec{Status: n.Output.Status, Body: n.Output.Body, Headers: n.Output.Headers}
	for _, m := range n.Mask {
		if p, err := ParsePath(m); err == nil {
			if p.IsHeader() {
				s.HeaderMasks = append(s.HeaderMasks, p.Header())
			} else {
				s.Masks = append(s.Masks, p)
			}
		}
	}
	for _, u := range n.Unordered {
		if p, err := ParsePath(u); err == nil {
			s.Unordered = append(s.Unordered, p)
		}
	}
	for _, t := range n.Tolerance {
		if p, err := ParsePath(t.Path); err == nil {
			s.Tolerances = append(s.Tolerances, TolSpec{Path: p, Kind: t.Kind, Value: t.Value})
		}
	}
	return s
}

// Floor is the executor release a check's rules need.
func (r *Rules) Floor() string {
	if len(r.Tolerance) > 0 || len(r.NotWorseThan) > 0 {
		return FloorE2
	}
	return FloorE1
}
