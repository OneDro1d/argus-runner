package compare

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The canonical form of an output (design 1.2, decisions in).
//
//	status   the decimal integer
//	headers  only the declared names, lower-cased and sorted, values trimmed: {"name":"value",...}
//	body     JSON: keys in byte order, numbers in their shortest round-trip form, arrays in order unless
//	         declared unordered, null and absent different. Anything that is not exactly one JSON value
//	         is TEXT (CRLF -> LF, nothing else). Empty is its own kind.
const (
	// MaskToken replaces a masked leaf or subtree. A REAL string equal to it is written with its first
	// character escaped (<), so a response can never be mistaken for a mask.
	MaskToken = "<masked>"
	// ToleranceToken replaces a numeric leaf governed by a Tolerance rule; the number travels in
	// OutputRecord.Values. Same escaping rule as MaskToken.
	ToleranceToken = "<tolerance>"

	// MaxDepth is the deepest JSON nesting that is canonicalised. A deeper body is TEXT: it is
	// recorded exactly, and never allowed to exhaust a stack.
	MaxDepth = 100
)

// TolSpec is one Tolerance rule as the recorder needs it.
type TolSpec struct {
	Path  Path
	Kind  string
	Value float64
}

// Spec is everything the recorder reads from the rules: which parts, which masks, which arrays are
// unordered. Rules.Spec builds it; a test may build one by hand.
type Spec struct {
	Status      bool
	Body        bool
	Headers     []string // lower-case names to compare
	Masks       []Path   // body masks
	HeaderMasks []string // lower-case header names whose value is masked
	Unordered   []Path
	Tolerances  []TolSpec
	// Scrub, when set, is applied to every string VALUE (never an object key), header value and text
	// body, after masking and before hashing, so the hash and the stored bytes agree. The executor
	// passes its secret-and-saved-variable scrubber here; this package knows nothing about it.
	Scrub func(string) string
}

type kind int

const (
	kNull kind = iota
	kBool
	kNum
	kStr
	kArr
	kObj
	kMasked
	kTol
)

type member struct {
	key string
	val *node
}

type node struct {
	k    kind
	b    bool
	s    string // string value, or the canonical number text
	arr  []*node
	obj  []member // sorted by key
	tol  float64
	rule int
}

// ── parsing ─────────────────────────────────────────────────────────────────────────────────────

// parseJSON reads exactly one JSON value. ok is false for anything else: a BOM, trailing bytes, a
// second value, a duplicate object key (parsers disagree about which one wins), a nesting deeper
// than MaxDepth, or invalid JSON.
func parseJSON(b []byte) (*node, bool) {
	// Go's decoder substitutes U+FFFD for an invalid byte and for an unpaired surrogate escape, so two
	// different bodies would hash alike. Such a body is TEXT: exact bytes, never repaired.
	if !utf8.Valid(b) || hasLoneSurrogate(b) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	n, ok := readValue(dec, 0)
	if !ok {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return n, true
}

// hasLoneSurrogate reports whether a JSON string (a value or a key) holds a \uD800-\uDFFF escape that
// is not half of a high+low pair.
func hasLoneSurrogate(b []byte) bool {
	hex4 := func(i int) (rune, bool) {
		if i+4 > len(b) {
			return 0, false
		}
		var v rune
		for _, c := range b[i : i+4] {
			switch {
			case c >= '0' && c <= '9':
				v = v<<4 | rune(c-'0')
			case c >= 'a' && c <= 'f':
				v = v<<4 | rune(c-'a'+10)
			case c >= 'A' && c <= 'F':
				v = v<<4 | rune(c-'A'+10)
			default:
				return 0, false
			}
		}
		return v, true
	}
	inStr := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if !inStr {
			inStr = c == '"'
			continue
		}
		switch c {
		case '"':
			inStr = false
		case '\\':
			if i+1 >= len(b) {
				return false
			}
			if b[i+1] != 'u' {
				i++
				continue
			}
			v, ok := hex4(i + 2)
			if !ok {
				return false // invalid JSON anyway; the decoder refuses it
			}
			i += 5
			switch {
			case v >= 0xDC00 && v <= 0xDFFF:
				return true
			case v >= 0xD800 && v <= 0xDBFF:
				if i+6 < len(b) && b[i+1] == '\\' && b[i+2] == 'u' {
					if lo, ok := hex4(i + 3); ok && lo >= 0xDC00 && lo <= 0xDFFF {
						i += 6
						continue
					}
				}
				return true
			}
		}
	}
	return false
}

func readValue(dec *json.Decoder, depth int) (*node, bool) {
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	switch t := tok.(type) {
	case json.Delim:
		if depth+1 > MaxDepth {
			return nil, false
		}
		switch t {
		case '[':
			n := &node{k: kArr, arr: []*node{}}
			for dec.More() {
				v, ok := readValue(dec, depth+1)
				if !ok {
					return nil, false
				}
				n.arr = append(n.arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, false
			}
			return n, true
		case '{':
			n := &node{k: kObj, obj: []member{}}
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				key, isStr := kt.(string)
				if err != nil || !isStr || seen[key] {
					return nil, false
				}
				seen[key] = true
				v, ok := readValue(dec, depth+1)
				if !ok {
					return nil, false
				}
				n.obj = append(n.obj, member{key, v})
			}
			if _, err := dec.Token(); err != nil {
				return nil, false
			}
			sort.Slice(n.obj, func(i, j int) bool { return n.obj[i].key < n.obj[j].key })
			return n, true
		}
		return nil, false
	case string:
		return &node{k: kStr, s: t}, true
	case json.Number:
		return &node{k: kNum, s: canonNumber(t.String())}, true
	case bool:
		return &node{k: kBool, b: t}, true
	case nil:
		return &node{k: kNull}, true
	}
	return nil, false
}

const maxSafeInt = 1 << 53

// canonNumber rewrites a JSON number literal: an integer up to 2^53 in magnitude as that integer,
// a larger integer exactly as written, anything else as the shortest decimal that round-trips a
// float64 (1.0 -> 1, 1e2 -> 100, 0.10 -> 0.1). -0 is 0. A literal float64 cannot hold is kept as
// written.
func canonNumber(lit string) string {
	if !strings.ContainsAny(lit, ".eE") {
		i, err := strconv.ParseInt(lit, 10, 64)
		if err != nil {
			return lit
		}
		if i >= -maxSafeInt && i <= maxSafeInt {
			return strconv.FormatInt(i, 10)
		}
		return lit
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return lit
	}
	return formatFloat(f)
}

// formatFloat is the shortest round-trip decimal, in the notation JavaScript uses: plain below 1e21
// and from 1e-6, exponent form (without zero padding) outside.
func formatFloat(f float64) string {
	if f == 0 {
		return "0"
	}
	a := math.Abs(f)
	if a >= 1e21 || a < 1e-6 {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[:1]
		digits := strings.TrimLeft(exp[1:], "0")
		if digits == "" {
			digits = "0"
		}
		return mant + "e" + sign + digits
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// ── encoding ────────────────────────────────────────────────────────────────────────────────────

const hexDigits = "0123456789abcdef"

// encString writes a JSON string with only the escapes JSON requires; everything else is raw UTF-8.
// With tokens set, a string that is exactly a token is written with its first character escaped.
func encString(buf *bytes.Buffer, s string, tokens bool) {
	buf.WriteByte('"')
	if tokens && (s == MaskToken || s == ToleranceToken) {
		// the escape is built from two pieces on purpose: written whole it can be decoded in transit
		buf.WriteString("\\" + "u003c")
		s = s[1:]
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			buf.WriteString(`\"`)
		case c == '\\':
			buf.WriteString(`\\`)
		case c == '\b':
			buf.WriteString(`\b`)
		case c == '\f':
			buf.WriteString(`\f`)
		case c == '\n':
			buf.WriteString(`\n`)
		case c == '\r':
			buf.WriteString(`\r`)
		case c == '\t':
			buf.WriteString(`\t`)
		case c < 0x20:
			buf.WriteString(`\u00`)
			buf.WriteByte(hexDigits[c>>4])
			buf.WriteByte(hexDigits[c&0xf])
		default:
			buf.WriteByte(c)
		}
	}
	buf.WriteByte('"')
}

func (n *node) encode(buf *bytes.Buffer) {
	switch n.k {
	case kNull:
		buf.WriteString("null")
	case kBool:
		if n.b {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case kNum:
		buf.WriteString(n.s)
	case kStr:
		encString(buf, n.s, true)
	case kMasked:
		buf.WriteString(`"` + MaskToken + `"`)
	case kTol:
		buf.WriteString(`"` + ToleranceToken + `"`)
	case kArr:
		buf.WriteByte('[')
		for i, e := range n.arr {
			if i > 0 {
				buf.WriteByte(',')
			}
			e.encode(buf)
		}
		buf.WriteByte(']')
	case kObj:
		buf.WriteByte('{')
		for i, m := range n.obj {
			if i > 0 {
				buf.WriteByte(',')
			}
			encString(buf, m.key, false)
			buf.WriteByte(':')
			m.val.encode(buf)
		}
		buf.WriteByte('}')
	}
}

func (n *node) bytes() []byte {
	var b bytes.Buffer
	n.encode(&b)
	return b.Bytes()
}

// ── passes over the tree ────────────────────────────────────────────────────────────────────────

func children(n *node, loc []Seg, f func(c *node, loc []Seg)) {
	switch n.k {
	case kArr:
		for i, c := range n.arr {
			f(c, append(loc[:len(loc):len(loc)], Seg{Kind: SegIndex, Index: i}))
		}
	case kObj:
		for _, m := range n.obj {
			f(m.val, append(loc[:len(loc):len(loc)], Seg{Kind: SegKey, Name: m.key}))
		}
	}
}

// maskPass replaces the node at each masked path with the mask token. A mask hides a VALUE, never
// the presence of a field: a path absent on one side stays absent, so a field that exists on one
// side only remains a visible difference.
func maskPass(n *node, loc []Seg, masks []Path) int {
	for _, m := range masks {
		if m.matches(loc) {
			*n = node{k: kMasked}
			return 1
		}
	}
	count := 0
	children(n, loc, func(c *node, l []Seg) { count += maskPass(c, l, masks) })
	return count
}

// tolPass swaps each numeric leaf a Tolerance rule governs for the tolerance token, keeping the
// number on the node. A governed leaf that is not a number stays exact.
func tolPass(n *node, loc []Seg, rules []TolSpec) int {
	count := 0
	if n.k == kNum {
		for i, r := range rules {
			if r.Path.matches(loc) {
				f, err := strconv.ParseFloat(n.s, 64)
				if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
					return 0
				}
				*n = node{k: kTol, tol: f, rule: i}
				return 1
			}
		}
		return 0
	}
	children(n, loc, func(c *node, l []Seg) { count += tolPass(c, l, rules) })
	return count
}

func scrubPass(n *node, scrub func(string) string) {
	switch n.k {
	case kStr:
		n.s = scrub(n.s)
	case kArr:
		for _, c := range n.arr {
			scrubPass(c, scrub)
		}
	case kObj:
		for _, m := range n.obj {
			scrubPass(m.val, scrub)
		}
	}
}

// sortPass sorts each declared array by the canonical bytes of its elements, inside-out, so an inner
// unordered array is already normal when the outer one is sorted. It runs after masking and
// scrubbing: the order comes from what is left to compare, identically on both sides.
func sortPass(n *node, loc []Seg, unordered []Path) {
	children(n, loc, func(c *node, l []Seg) { sortPass(c, l, unordered) })
	if n.k != kArr {
		return
	}
	for _, u := range unordered {
		if u.matches(loc) {
			keys := make([]string, len(n.arr))
			for i, e := range n.arr {
				keys[i] = string(e.bytes())
			}
			idx := make([]int, len(n.arr))
			for i := range idx {
				idx[i] = i
			}
			sort.SliceStable(idx, func(a, b int) bool { return keys[idx[a]] < keys[idx[b]] })
			sorted := make([]*node, len(n.arr))
			for i, j := range idx {
				sorted[i] = n.arr[j]
			}
			n.arr = sorted
			return
		}
	}
}

func locString(loc []Seg) string {
	var b strings.Builder
	b.WriteByte('$')
	for _, s := range loc {
		switch s.Kind {
		case SegIndex:
			b.WriteString("[" + strconv.Itoa(s.Index) + "]")
		default:
			simple := s.Name != ""
			for i := 0; i < len(s.Name); i++ {
				simple = simple && isNameByte(s.Name[i])
			}
			if simple {
				b.WriteString("." + s.Name)
			} else {
				q, _ := json.Marshal(s.Name)
				b.WriteString("[" + string(q) + "]")
			}
		}
	}
	return b.String()
}

// collectValues lists the tolerant leaves with their final locations, in tree order.
func collectValues(n *node, loc []Seg, out *[]ToleranceValue) {
	if n.k == kTol {
		*out = append(*out, ToleranceValue{Path: locString(loc), Rule: n.rule, Value: n.tol})
		return
	}
	children(n, loc, func(c *node, l []Seg) { collectValues(c, l, out) })
}

// ── headers ─────────────────────────────────────────────────────────────────────────────────────

// canonHeaders builds {"name":"value",...} for the declared headers present in the response.
func canonHeaders(h map[string][]string, spec Spec) (string, int) {
	have := map[string][]string{}
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		lk := strings.ToLower(strings.TrimSpace(name))
		for _, v := range h[name] {
			have[lk] = append(have[lk], strings.Trim(v, " \t"))
		}
	}
	declared := append([]string(nil), spec.Headers...)
	sort.Strings(declared)
	var buf bytes.Buffer
	buf.WriteByte('{')
	masked, first := 0, true
	for i, name := range declared {
		if i > 0 && name == declared[i-1] {
			continue
		}
		vals, ok := have[name]
		if !ok {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		encString(&buf, name, false)
		buf.WriteByte(':')
		if hasString(spec.HeaderMasks, name) {
			buf.WriteString(`"` + MaskToken + `"`)
			masked++
			continue
		}
		v := strings.Join(vals, ", ")
		if spec.Scrub != nil {
			v = spec.Scrub(v)
		}
		encString(&buf, v, true)
	}
	buf.WriteByte('}')
	return buf.String(), masked
}

// ── the recorder ────────────────────────────────────────────────────────────────────────────────

// BuildRecord canonicalises one response under spec, hashes it, and returns the record (digests and
// sizes only) with the stored form (nil when nothing was recorded). It never errs: when an output
// cannot be recorded faithfully the record says not_recorded with a reason from the closed
// vocabulary, and carries no hash, so absence can never read as sameness.
func BuildRecord(resp Response, spec Spec, step string, sample int) (OutputRecord, *Stored) {
	rec := OutputRecord{V: RecordVersion, Step: step, Sample: sample, Status: resp.Status}
	fail := func(reason string) (OutputRecord, *Stored) {
		rec.State, rec.Reason = StateNotRecorded, reason
		return rec, nil
	}
	switch {
	case sample > MaxSamples:
		return fail(ReasonTooManySamples)
	case resp.Status <= 0:
		return fail(ReasonNoResponse)
	}
	for _, h := range spec.Headers {
		if RefusedHeader(h) {
			return fail(ReasonHeaderNotAllowed)
		}
	}

	st := &Stored{status: resp.Status, bodySelected: spec.Body}
	var bodyBytes []byte
	if spec.Body {
		rec.BodyBytes = len(resp.Body)
		if len(resp.Body) > MaxBodyBytes {
			return fail(ReasonBodyTooLarge)
		}
		switch tree, isJSON := parseJSON(resp.Body); {
		case len(resp.Body) == 0:
			rec.BodyKind = KindEmpty
		case isJSON:
			rec.BodyKind = KindJSON
			rec.MasksApplied += maskPass(tree, nil, spec.Masks)
			if nTol := tolPass(tree, nil, spec.Tolerances); nTol > MaxToleranceValues {
				return fail(ReasonTooManyValues)
			}
			if spec.Scrub != nil {
				scrubPass(tree, spec.Scrub)
			}
			sortPass(tree, nil, spec.Unordered)
			collectValues(tree, nil, &rec.Values)
			bodyBytes = tree.bytes()
		default:
			rec.BodyKind = KindText
			switch {
			case len(spec.Masks) > 0:
				return fail(ReasonMaskNeedsJSON)
			case len(spec.Unordered) > 0 || len(spec.Tolerances) > 0:
				return fail(ReasonPathRuleNeedsJSON)
			}
			s := strings.ReplaceAll(string(resp.Body), "\r\n", "\n")
			if spec.Scrub != nil {
				s = spec.Scrub(s)
			}
			bodyBytes = []byte(s)
		}
		st.bodyKind = rec.BodyKind
	}

	var statusPart, headersPart, bodyPart string
	if spec.Status {
		statusPart = PartHash(DomainStatus, []byte(strconv.Itoa(resp.Status)))
	}
	if len(spec.Headers) > 0 {
		hs, masked := canonHeaders(resp.Headers, spec)
		rec.MasksApplied += masked
		headersPart = PartHash(DomainHeaders, []byte(hs))
		st.headers = hs
	}
	if spec.Body {
		bodyPart = PartHash(DomainBody, bodyBytes)
	}
	rec.Parts = Parts{Status: statusPart, Headers: headersPart, Body: bodyPart}
	rec.Hash = OutputHash(statusPart, headersPart, bodyPart)
	rec.State = StateRecorded

	body := string(bodyBytes)
	if len(body) > StoredBodyLimit {
		cut := StoredBodyLimit
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body = body[:cut]
		rec.Truncated = true
	}
	st.body = body
	st.truncated = rec.Truncated
	// Bound the ENCODED file, not the raw body: control bytes are written as 6-byte escapes, so a body
	// within StoredBodyLimit can still make a file far over what a relayed answer may carry. Cut the
	// stored body further (on a rune start) until the file fits, and say so. The hashes above are over the
	// whole body and are not touched.
	for {
		n := len(st.File())
		if n <= MaxStoredFileBytes || len(st.body) == 0 {
			break
		}
		cut := len(st.body) - ((n-MaxStoredFileBytes)/4 + 1)
		if cut < 0 {
			cut = 0
		}
		for cut > 0 && !utf8.RuneStart(st.body[cut]) {
			cut--
		}
		st.body = st.body[:cut]
		st.truncated, rec.Truncated = true, true
	}
	return rec, st
}

// CanonicalBody is the stored canonical body text (cut at StoredBodyLimit).
func (s *Stored) CanonicalBody() string { return s.body }

// CanonicalHeaders is the canonical headers object, "" when the check compares no header.
func (s *Stored) CanonicalHeaders() string { return s.headers }

// File is the executor's on-disk form: {status, headers{}, body_kind, body}. The body is embedded as
// JSON when it is JSON and fits; otherwise it is a JSON string of the (possibly cut) canonical text.
//
// LOSSLESS (ARGUS-CMP-3): the hash is over the exact bytes, and a JSON string cannot carry a byte that is
// not valid UTF-8 (a decoder turns it into U+FFFD, so two different bodies would read back alike). A text
// body, or a canonical headers object, that is not valid UTF-8 is therefore written as base64 in its own
// field (`body_b64` with `"body_encoding":"base64"` and `"body":null`; `headers_b64` with `"headers":{}`).
// Anything valid UTF-8 is written exactly as before, so no existing file changes. A body that was cut
// (at StoredBodyLimit, or further so the ENCODED file fits MaxStoredFileBytes) adds a top-level
// `"truncated":true`; a whole body adds nothing.
func (s *Stored) File() []byte {
	var b bytes.Buffer
	b.WriteString(`{"status":` + strconv.Itoa(s.status) + `,"headers":`)
	headersB64, bodyB64 := "", ""
	switch {
	case s.headers == "":
		b.WriteString("{}")
	case !utf8.ValidString(s.headers):
		b.WriteString("{}")
		headersB64 = base64.StdEncoding.EncodeToString([]byte(s.headers))
	default:
		b.WriteString(s.headers)
	}
	b.WriteString(`,"body_kind":`)
	encString(&b, s.bodyKind, false)
	b.WriteString(`,"body":`)
	switch {
	case !s.bodySelected:
		b.WriteString("null")
	case s.bodyKind == KindJSON && !s.truncated:
		b.WriteString(s.body)
	case !utf8.ValidString(s.body):
		b.WriteString("null")
		bodyB64 = base64.StdEncoding.EncodeToString([]byte(s.body))
	default:
		encString(&b, s.body, false)
	}
	if headersB64 != "" {
		b.WriteString(`,"headers_b64":"` + headersB64 + `"`)
	}
	if bodyB64 != "" {
		b.WriteString(`,"body_encoding":"base64","body_b64":"` + bodyB64 + `"`)
	}
	if s.truncated {
		b.WriteString(`,"truncated":true`) // a reader of the file alone can tell a capped body from a whole one
	}
	b.WriteByte('}')
	return b.Bytes()
}
