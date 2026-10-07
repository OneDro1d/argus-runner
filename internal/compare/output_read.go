package compare

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"unicode/utf8"
)

// output_read.go -- ARGUS-CMP-9: reading a RECORDED OUTPUT back.
//
// The executor keeps the stored form of each recorded output (Stored.File) and the control plane relays it on
// demand (verb get_output). What comes back has been through a JSONB column (relay_commands.result), which keeps
// the values and loses the bytes: key order, whitespace, escapes, and the spelling of a number. So the control
// plane cannot hash the answer as received. Reconstruct rebuilds the CANONICAL tree from it, applying the rules
// the check declared, and recomputes the part hashes with the SAME functions the executor used. What it cannot
// rebuild faithfully it says so (VerifyPartial), never guessing.
//
// The one real ambiguity is the mask token: a masked leaf is written "<masked>" and a REAL string equal to it is
// written with its first character escaped; after a JSONB round trip both are the same text. The rules resolve it:
// a masked leaf is always AT a path a declared mask addresses (a mask replaces the whole node, so nothing real can
// be under or at it), and a tolerant leaf is always at a path the record lists in Values.

// The states of a Verification.
const (
	// VerifyVerified: every part the record hashed was recomputed from the answer and equals the record.
	VerifyVerified = "verified"
	// VerifyPartial: what could be recomputed equals the record; the rest cannot be recomputed from the answer
	// (a body the executor cut, a number JSONB cannot keep as written). Unchecked names the parts.
	VerifyPartial = "partial"
	// VerifyMismatch: a recomputed part differs from the recorded one. The answer is NOT the recorded output.
	VerifyMismatch = "mismatch"
	// VerifyMalformed: the answer is not a stored output of the shape the executor writes.
	VerifyMalformed = "malformed"
)

// Closed sentences of a Verification.Note.
const (
	NoteVerified  = "the output was recomputed from what the executor returned and matches the hash the control plane recorded"
	NoteBodyCut   = "the executor keeps only the first part of a large body: the part shown is a prefix and its hash cannot be checked; status and headers were checked"
	NoteBigNumber = "the body holds a whole number of 1e21 or more, which cannot be recomputed after storage; the rest of the output was checked"
	NoteMismatch  = "what the executor returned does not match the hash the control plane recorded for this output, so it is not shown"
	NoteMalformed = "what the executor returned is not a recorded output, so it is not shown"
)

const (
	partStatus  = "status"
	partHeaders = "headers"
	partBody    = "body"
	partTotal   = "hash"
	// a whole number of 1e21 or more has at least this many digits
	bigNumberDigits = 22
)

// Verification is the outcome of checking a relayed stored output against the control plane's record of it.
type Verification struct {
	State      string   `json:"state"`
	Mismatched []string `json:"mismatched,omitempty"`
	Unchecked  []string `json:"unchecked,omitempty"`
	Note       string   `json:"note"`
}

// Shown is what an author is shown of one recorded output: only what the executor kept (canonical, masked,
// scrubbed). Body is a JSON value: the canonical tree for a JSON body, a JSON string for text, null when the
// check does not compare the body. BodyEncoding is "base64" when the text is not valid UTF-8 (Body is then the
// base64 of its bytes).
type Shown struct {
	Status       int               `json:"status"`
	Headers      map[string]string `json:"headers"`
	BodyKind     string            `json:"body_kind"`
	Body         json.RawMessage   `json:"body"`
	BodyEncoding string            `json:"body_encoding,omitempty"`
	Truncated    bool              `json:"truncated"`
}

// Canonical is one recorded output rebuilt from its stored form. It is what Diff compares.
type Canonical struct {
	status     int
	hasStatus  bool
	hasHeaders bool
	headers    []canonHeader
	opaqueHdrs string // the canonical headers text when it could not be read as names and values
	hasOpaque  bool
	hasBody    bool
	bodyKind   string
	tree       *node
	text       string
	unchecked  bool // the body is not the whole canonical body (the executor cut it): never compared
	approx     bool // the body is whole but its hash cannot be recomputed (a number of 1e21 or more): compared, flagged
	truncated  bool
	unordered  []Path
}

type canonHeader struct {
	name, value string
	masked      bool
}

// storedFile is the shape of Stored.File, read loosely (every field present or absent, nothing assumed).
type storedFile struct {
	Status       *int            `json:"status"`
	Headers      json.RawMessage `json:"headers"`
	BodyKind     *string         `json:"body_kind"`
	Body         json.RawMessage `json:"body"`
	HeadersB64   string          `json:"headers_b64"`
	BodyEncoding string          `json:"body_encoding"`
	BodyB64      string          `json:"body_b64"`
	Truncated    bool            `json:"truncated"`
}

func malformed() (*Canonical, Verification) {
	return nil, Verification{State: VerifyMalformed, Note: NoteMalformed}
}

func addOnce(list []string, s string) []string {
	for _, e := range list {
		if e == s {
			return list
		}
	}
	return append(list, s)
}

// Reconstruct rebuilds the canonical output from the stored file the executor returned and verifies it against
// rec, the record the control plane holds. rules are the check's declared rules (nil: none). A nil Canonical is
// returned for VerifyMismatch and VerifyMalformed: such an answer is never shown.
func Reconstruct(file []byte, rec OutputRecord, rules *Rules) (*Canonical, Verification) {
	if rec.State != StateRecorded {
		return malformed()
	}
	var f storedFile
	if err := json.Unmarshal(file, &f); err != nil || f.Status == nil || f.BodyKind == nil || len(f.Body) == 0 || len(f.Headers) == 0 {
		return malformed()
	}
	var spec Spec
	if rules != nil {
		spec = rules.Spec()
	}
	c := &Canonical{status: *f.Status, hasStatus: rec.Parts.Status != "", hasHeaders: rec.Parts.Headers != "",
		hasBody: rec.Parts.Body != "", unordered: spec.Unordered, truncated: f.Truncated}
	var mism, unchecked []string
	note := ""
	skipBodyHash := false

	// the record must be consistent with itself: the total is the hash of its own parts
	if OutputHash(rec.Parts.Status, rec.Parts.Headers, rec.Parts.Body) != rec.Hash {
		mism = addOnce(mism, partTotal)
	}

	// status: always compared with the status the control plane recorded, and hashed when the check compares it
	if c.status != rec.Status {
		mism = addOnce(mism, partStatus)
	}
	if c.hasStatus && PartHash(DomainStatus, []byte(strconv.Itoa(c.status))) != rec.Parts.Status {
		mism = addOnce(mism, partStatus)
	}

	// headers
	hdrs, hdrText, opaque, ok := readHeaders(&f, spec)
	if !ok {
		return malformed()
	}
	switch {
	case c.hasHeaders:
		if PartHash(DomainHeaders, []byte(hdrText)) != rec.Parts.Headers {
			mism = addOnce(mism, partHeaders)
		}
	case hdrText != "{}":
		mism = addOnce(mism, partHeaders) // headers the record never hashed: not what was recorded
	}
	if opaque {
		c.hasOpaque, c.opaqueHdrs = true, hdrText
	} else {
		c.headers = hdrs
	}

	// body
	switch {
	case !c.hasBody:
		if *f.BodyKind != "" || string(bytes.TrimSpace(f.Body)) != "null" || f.BodyEncoding != "" || f.Truncated {
			mism = addOnce(mism, partBody) // a body the record never hashed: not what was recorded
		}
	default:
		kind := *f.BodyKind
		if kind != KindJSON && kind != KindText && kind != KindEmpty {
			return malformed()
		}
		c.bodyKind = kind
		if kind != rec.BodyKind || f.Truncated != rec.Truncated {
			mism = addOnce(mism, partBody)
		}
		var bodyBytes []byte
		switch {
		case f.BodyEncoding == "base64":
			if kind != KindText {
				return malformed()
			}
			raw, err := base64.StdEncoding.DecodeString(f.BodyB64)
			if err != nil {
				return malformed()
			}
			c.text, bodyBytes = string(raw), raw
		case kind == KindJSON && !f.Truncated:
			tree, ok := parseJSON(f.Body)
			if !ok {
				return malformed()
			}
			resolveTokens(tree, nil, spec.Masks, rec.Values)
			c.tree = tree
			bodyBytes = tree.bytes()
			if hasBigNumber(tree) {
				// the tree is faithful (Diff compares it), but its hash cannot be recomputed from a JSONB copy
				c.approx, skipBodyHash = true, true
				unchecked = addOnce(unchecked, partBody)
				note = NoteBigNumber
			}
		default: // text, empty, or a JSON body the executor cut: a JSON string
			var s string
			if err := json.Unmarshal(f.Body, &s); err != nil {
				return malformed()
			}
			if kind == KindEmpty && s != "" {
				return malformed()
			}
			c.text, bodyBytes = s, []byte(s)
		}
		if f.Truncated {
			c.unchecked = true
			unchecked = addOnce(unchecked, partBody)
			note = NoteBodyCut
		}
		if !c.unchecked && !skipBodyHash && PartHash(DomainBody, bodyBytes) != rec.Parts.Body {
			mism = addOnce(mism, partBody)
		}
	}

	if len(mism) > 0 {
		sort.Strings(mism)
		return nil, Verification{State: VerifyMismatch, Mismatched: mism, Note: NoteMismatch}
	}
	if len(unchecked) > 0 {
		return c, Verification{State: VerifyPartial, Unchecked: unchecked, Note: note}
	}
	return c, Verification{State: VerifyVerified, Note: NoteVerified}
}

// readHeaders reads the stored headers. text is the canonical headers text the executor hashed, rebuilt exactly
// the way canonHeaders writes it. opaque is true when the executor kept it as base64 (not valid UTF-8): it is then
// the raw text and there are no names to list.
func readHeaders(f *storedFile, spec Spec) (list []canonHeader, text string, opaque, ok bool) {
	if f.HeadersB64 != "" {
		raw, err := base64.StdEncoding.DecodeString(f.HeadersB64)
		if err != nil {
			return nil, "", false, false
		}
		return nil, string(raw), true, true
	}
	var m map[string]string
	if err := json.Unmarshal(f.Headers, &m); err != nil || m == nil {
		return nil, "", false, false
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			buf.WriteByte(',')
		}
		encString(&buf, n, false)
		buf.WriteByte(':')
		v := m[n]
		masked := v == MaskToken && hasString(spec.HeaderMasks, n)
		if masked {
			buf.WriteString(`"` + MaskToken + `"`)
		} else {
			encString(&buf, v, true)
		}
		list = append(list, canonHeader{name: n, value: v, masked: masked})
	}
	buf.WriteByte('}')
	return list, buf.String(), false, true
}

// resolveTokens turns a string equal to a token back into the node it stood for, where the rules say it was one.
func resolveTokens(n *node, loc []Seg, masks []Path, values []ToleranceValue) {
	if n.k == kStr {
		switch n.s {
		case MaskToken:
			for _, m := range masks {
				if m.matches(loc) {
					*n = node{k: kMasked}
					return
				}
			}
		case ToleranceToken:
			p := locString(loc)
			for _, v := range values {
				if v.Path == p {
					*n = node{k: kTol, tol: v.Value, rule: v.Rule}
					return
				}
			}
		}
		return
	}
	children(n, loc, func(c *node, l []Seg) { resolveTokens(c, l, masks, values) })
}

// hasBigNumber reports whether a number is spelled with 22 digits or more: JSONB prints a float of 1e21 or more
// as all its digits, which cannot be told from a whole number written that way.
func hasBigNumber(n *node) bool {
	if n.k == kNum {
		d := n.s
		if len(d) > 0 && d[0] == '-' {
			d = d[1:]
		}
		if len(d) >= bigNumberDigits {
			for i := 0; i < len(d); i++ {
				if d[i] < '0' || d[i] > '9' {
					return false
				}
			}
			return true
		}
		return false
	}
	found := false
	children(n, nil, func(c *node, _ []Seg) {
		if !found && hasBigNumber(c) {
			found = true
		}
	})
	return found
}

// Shown is what to show of a verified or partial output.
func (c *Canonical) Shown() Shown {
	sh := Shown{Status: c.status, Headers: map[string]string{}, BodyKind: c.bodyKind, Body: json.RawMessage("null"), Truncated: c.truncated}
	for _, h := range c.headers {
		sh.Headers[h.name] = h.value
	}
	if !c.hasBody {
		return sh
	}
	switch {
	case c.tree != nil:
		sh.Body = c.tree.bytes()
	case utf8.ValidString(c.text):
		b, _ := json.Marshal(c.text)
		sh.Body = b
	default:
		b, _ := json.Marshal(base64.StdEncoding.EncodeToString([]byte(c.text)))
		sh.Body, sh.BodyEncoding = b, "base64"
	}
	return sh
}

// MaskedPaths lists the concrete locations whose value the rules hid ("$.items[0].id", "header:date"), sorted.
func (c *Canonical) MaskedPaths() []string {
	out := []string{}
	for _, h := range c.headers {
		if h.masked {
			out = append(out, "header:"+h.name)
		}
	}
	if c.tree != nil {
		var walk func(n *node, loc []Seg)
		walk = func(n *node, loc []Seg) {
			if n.k == kMasked {
				out = append(out, locString(loc))
				return
			}
			children(n, loc, func(ch *node, l []Seg) { walk(ch, l) })
		}
		walk(c.tree, nil)
	}
	sort.Strings(out)
	return out
}
