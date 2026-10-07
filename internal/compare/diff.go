package compare

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"unicode/utf8"
)

// diff.go -- ARGUS-CMP-9: the structured difference of two canonical outputs.
//
// PURE and DETERMINISTIC: the same two inputs give the same bytes. It reads two Canonical values (built by
// Reconstruct from the stored files) and nothing else.
//
// THE RULE THAT MAKES IT TRUSTWORTHY: Diff reports a difference in a part EXACTLY when the two outputs' hashes
// differ on that part. It can, because it compares the same canonical trees the hashes are taken over: sorted
// keys, canonical numbers, masked leaves, an unordered array already sorted. Where it could not (a body the
// executor cut, a number JSONB cannot spell back) it does not compare, and says so (Body.State "not_comparable").
//
// A MASKED value is never shown. The canonical tree holds the token, not the value, so the values were never on
// the control plane; Diff additionally never carries a value at a masked location, on either side, even when only
// one side is masked (rules that disagree): such a path is Masked, with no A and no B. An element present on one
// side only that merely HOLDS a masked value is shown with its keys and the token where the value was.

// Bounds of a Diff, one constant each.
const (
	// MaxDiffEntries is the most entries a Diff lists. Total says how many there were.
	MaxDiffEntries = 200
	// MaxDiffValueBytes is the most bytes of one value a DiffEntry carries.
	MaxDiffValueBytes = 1024
	// MaxDiffPathBytes is the most bytes of one path a DiffEntry carries. A longer path is shortened by ShortenPath:
	// a prefix of it, then "~sha256:" and 16 hex digits of the sha256 of the WHOLE path, so two different paths never
	// render the same (barring a 64-bit hash collision) and the entry says PathCut.
	MaxDiffPathBytes = 256
	// MaxDiffEncodedBytes is the most bytes the entries of one Diff add up to, as JSON. Entries are listed in order
	// until the next one would pass it; the rest only count (Total stays true, Truncated is set).
	MaxDiffEncodedBytes = 128 * 1024
)

const pathCutMark = "~sha256:"

// ShortenPath bounds a path to MaxDiffPathBytes. A path within the bound is returned as it is. A longer one keeps its
// first bytes (cut on a rune start) and ends with pathCutMark and 16 hex digits of the sha256 of the whole path, so
// the result is at most MaxDiffPathBytes bytes and distinct paths stay distinct.
func ShortenPath(p string) (short string, cut bool) {
	if len(p) <= MaxDiffPathBytes {
		return p, false
	}
	sum := sha256.Sum256([]byte(p))
	tail := pathCutMark + hex.EncodeToString(sum[:8])
	c := MaxDiffPathBytes - len(tail)
	for c > 0 && !utf8.RuneStart(p[c]) {
		c--
	}
	return p[:c] + tail, true
}

// The parts of an output, as Diff names them.
const (
	DiffPartStatus  = "status"
	DiffPartHeaders = "headers"
	DiffPartBody    = "body"
)

// The changes of a DiffEntry.
const (
	ChangeChanged = "changed"
	ChangeOnlyA   = "only_a"
	ChangeOnlyB   = "only_b"
)

// The states of a BodyDiff.
const (
	BodyEqual         = "equal"
	BodyDiffers       = "differs"
	BodyNotComparable = "not_comparable"
	BodyNotCompared   = "not_compared"
)

// DiffEntry is one difference. Part is status | headers | body. Path is "status", "header:<name>" or a JSON path
// of the body. Change is changed | only_a | only_b. A and B are the canonical JSON text of the value on each side
// (nil where there is none). A masked value is NEVER carried: Masked is true and A and B are nil, except for a
// one-sided element that only HOLDS a masked value: its A or B is its canonical text with the mask token where the
// value was, and Masked is true. Cut says a value was cut at MaxDiffValueBytes.
type DiffEntry struct {
	Part   string  `json:"part"`
	Path   string  `json:"path"`
	Change string  `json:"change"`
	A      *string `json:"a,omitempty"`
	B      *string `json:"b,omitempty"`
	Masked bool    `json:"masked,omitempty"`
	Cut    bool    `json:"cut,omitempty"`
	// PathCut says Path was shortened by ShortenPath: it is a prefix of the real path and a hash of the whole.
	PathCut bool `json:"path_cut,omitempty"`
}

// BodyDiff states the kind of each body and whether they are equal, without inventing a path for text.
type BodyDiff struct {
	AKind string `json:"a_kind"`
	BKind string `json:"b_kind"`
	State string `json:"state"` // equal | differs | not_comparable | not_compared
}

// DiffResult is the structured difference of two canonical outputs. Entries holds at most MaxDiffEntries;
// Total counts every difference found and Truncated says Entries is shorter than Total. Parts lists the parts
// that differ (status, headers, body).
type DiffResult struct {
	Equal     bool        `json:"equal"`
	Parts     []string    `json:"parts_differ"`
	Entries   []DiffEntry `json:"entries"`
	Total     int         `json:"total"`
	Truncated bool        `json:"truncated"`
	Body      BodyDiff    `json:"body"`
	// Approximate says a side's body holds a number of 1e21 or more, whose hash cannot be recomputed after
	// storage: the trees were compared, the "exactly when the hashes differ" claim is not proven for that body.
	Approximate bool `json:"approximate,omitempty"`
}

type differ struct {
	res DiffResult
	// both sides declared this array path unordered: only then is it a set, as only then are both sorted
	unordered []Path
	// encoded is the JSON size of the entries listed so far. full is set at the first entry that is not listed, and
	// from then on none is: the list is a prefix of the order the differences were found in.
	encoded int
	full    bool
}

func (d *differ) add(e DiffEntry) {
	d.res.Total++
	if d.full || len(d.res.Entries) >= MaxDiffEntries {
		d.full, d.res.Truncated = true, true
		return
	}
	e.Path, e.PathCut = ShortenPath(e.Path)
	raw, _ := json.Marshal(e)
	if d.encoded+len(raw)+1 > MaxDiffEncodedBytes {
		d.full, d.res.Truncated = true, true
		return
	}
	d.encoded += len(raw) + 1
	d.res.Entries = append(d.res.Entries, e)
}

// valueText is the canonical JSON text of a node, cut at MaxDiffValueBytes on a rune start.
func valueText(n *node) (s *string, cut bool) {
	t := string(n.bytes())
	if len(t) > MaxDiffValueBytes {
		c := MaxDiffValueBytes
		for c > 0 && !utf8.RuneStart(t[c]) {
			c--
		}
		t, cut = t[:c], true
	}
	return &t, cut
}

func containsMask(n *node) bool {
	if n.k == kMasked {
		return true
	}
	found := false
	children(n, nil, func(c *node, _ []Seg) {
		if !found && containsMask(c) {
			found = true
		}
	})
	return found
}

// entryFor builds an entry for a value present on one side (A) or both (changed).
//
// A masked node is shown as the token and never as a value: the tree holds the token, not the value. So an element
// present on ONE side whose own location is not masked but which holds a masked descendant is shown with its keys and
// the token in place of each masked value, and the entry keeps Masked so a consumer can tell. Nothing
// real can sit under such an element on the other side, as the other side has no element there; a Canonical keeps the
// masked LOCATIONS and not the rules, so there is no other mask to apply. Where both sides hold the value (changed) and
// either holds a mask, and where the entry's own location is masked, A and B stay nil.
func entryFor(part, path, change string, a, b *node) DiffEntry {
	e := DiffEntry{Part: part, Path: path, Change: change}
	if (a != nil && containsMask(a)) || (b != nil && containsMask(b)) {
		e.Masked = true
		if change == ChangeChanged || (a != nil && a.k == kMasked) || (b != nil && b.k == kMasked) {
			return e
		}
	}
	if a != nil {
		e.A, e.Cut = valueText(a)
	}
	if b != nil {
		var cut bool
		e.B, cut = valueText(b)
		e.Cut = e.Cut || cut
	}
	return e
}

func (d *differ) isUnordered(loc []Seg) bool {
	for _, p := range d.unordered {
		if p.matches(loc) {
			return true
		}
	}
	return false
}

func sameNode(a, b *node) bool { return string(a.bytes()) == string(b.bytes()) }

// walk compares two nodes at loc. Equal nodes produce nothing.
func (d *differ) walk(a, b *node, loc []Seg) {
	switch {
	case a.k == kMasked && b.k == kMasked:
		return // both hid the value: equal by rule, and nothing to show
	case a.k == kMasked || b.k == kMasked:
		d.add(DiffEntry{Part: DiffPartBody, Path: locString(loc), Change: ChangeChanged, Masked: true})
		return
	case a.k == kTol && b.k == kTol:
		return // the numbers travel beside the hash (OutputRecord.Values); the body hashes the token
	case a.k != b.k:
		d.add(entryFor(DiffPartBody, locString(loc), ChangeChanged, a, b))
		return
	}
	switch a.k {
	case kObj:
		i, j := 0, 0
		for i < len(a.obj) || j < len(b.obj) {
			switch {
			case j >= len(b.obj) || (i < len(a.obj) && a.obj[i].key < b.obj[j].key):
				d.add(entryFor(DiffPartBody, locString(child(loc, a.obj[i].key, -1)), ChangeOnlyA, a.obj[i].val, nil))
				i++
			case i >= len(a.obj) || b.obj[j].key < a.obj[i].key:
				d.add(entryFor(DiffPartBody, locString(child(loc, b.obj[j].key, -1)), ChangeOnlyB, nil, b.obj[j].val))
				j++
			default:
				d.walk(a.obj[i].val, b.obj[j].val, child(loc, a.obj[i].key, -1))
				i++
				j++
			}
		}
	case kArr:
		if d.isUnordered(loc) {
			d.set(a, b, loc)
			return
		}
		n := len(a.arr)
		if len(b.arr) < n {
			n = len(b.arr)
		}
		for i := 0; i < n; i++ {
			d.walk(a.arr[i], b.arr[i], child(loc, "", i))
		}
		for i := n; i < len(a.arr); i++ {
			d.add(entryFor(DiffPartBody, locString(child(loc, "", i)), ChangeOnlyA, a.arr[i], nil))
		}
		for i := n; i < len(b.arr); i++ {
			d.add(entryFor(DiffPartBody, locString(child(loc, "", i)), ChangeOnlyB, nil, b.arr[i]))
		}
	default: // a scalar of the same kind: equal when its canonical text is
		if !sameNode(a, b) {
			d.add(entryFor(DiffPartBody, locString(loc), ChangeChanged, a, b))
		}
	}
}

// set compares two unordered arrays as MULTISETS of their canonical elements: an element is named, never an index.
func (d *differ) set(a, b *node, loc []Seg) {
	count := map[string]int{}
	elem := map[string]*node{}
	for _, e := range a.arr {
		k := string(e.bytes())
		count[k]++
		elem[k] = e
	}
	for _, e := range b.arr {
		k := string(e.bytes())
		count[k]--
		elem[k] = e
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	path := locString(loc) + "[*]"
	for _, k := range keys {
		switch n := count[k]; {
		case n > 0:
			for ; n > 0; n-- {
				d.add(entryFor(DiffPartBody, path, ChangeOnlyA, elem[k], nil))
			}
		case n < 0:
			for ; n < 0; n++ {
				d.add(entryFor(DiffPartBody, path, ChangeOnlyB, nil, elem[k]))
			}
		}
	}
}

func child(loc []Seg, key string, idx int) []Seg {
	out := append(loc[:len(loc):len(loc)], Seg{})
	if idx >= 0 {
		out[len(loc)] = Seg{Kind: SegIndex, Index: idx}
	} else {
		out[len(loc)] = Seg{Kind: SegKey, Name: key}
	}
	return out
}

func intersectPaths(a, b []Path) []Path {
	var out []Path
	for _, p := range a {
		for _, q := range b {
			if p.String() == q.String() {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// Diff is the structured difference of two canonical outputs. A nil side is nothing to compare: the result is
// never Equal.
func Diff(a, b *Canonical) DiffResult {
	d := &differ{res: DiffResult{Parts: []string{}, Entries: []DiffEntry{}}}
	if a == nil || b == nil {
		d.res.Body = BodyDiff{State: BodyNotComparable}
		return d.res
	}
	d.unordered = intersectPaths(a.unordered, b.unordered)
	d.res.Approximate = a.approx || b.approx
	parts := map[string]bool{}
	mark := func(part string, before int) {
		if d.res.Total > before {
			parts[part] = true
		}
	}

	// status
	before := d.res.Total
	switch {
	case a.hasStatus && b.hasStatus:
		if a.status != b.status {
			d.add(DiffEntry{Part: DiffPartStatus, Path: "status", Change: ChangeChanged, A: strp(strconv.Itoa(a.status)), B: strp(strconv.Itoa(b.status))})
		}
	case a.hasStatus:
		d.add(DiffEntry{Part: DiffPartStatus, Path: "status", Change: ChangeOnlyA, A: strp(strconv.Itoa(a.status))})
	case b.hasStatus:
		d.add(DiffEntry{Part: DiffPartStatus, Path: "status", Change: ChangeOnlyB, B: strp(strconv.Itoa(b.status))})
	}
	mark(DiffPartStatus, before)

	// headers
	before = d.res.Total
	d.headers(a, b)
	mark(DiffPartHeaders, before)

	// body
	before = d.res.Total
	d.body(a, b)
	mark(DiffPartBody, before)
	if d.res.Body.State == BodyDiffers || d.res.Body.State == BodyNotComparable {
		parts[DiffPartBody] = d.res.Body.State == BodyDiffers
	}
	for _, p := range []string{DiffPartStatus, DiffPartHeaders, DiffPartBody} {
		if parts[p] {
			d.res.Parts = append(d.res.Parts, p)
		}
	}
	d.res.Equal = len(d.res.Parts) == 0 && d.res.Body.State != BodyNotComparable
	return d.res
}

func strp(s string) *string { return &s }

func (d *differ) headers(a, b *Canonical) {
	switch {
	case !a.hasHeaders && !b.hasHeaders:
		return
	case a.hasHeaders != b.hasHeaders:
		d.add(DiffEntry{Part: DiffPartHeaders, Path: "headers", Change: map[bool]string{true: ChangeOnlyA, false: ChangeOnlyB}[a.hasHeaders]})
		return
	case a.hasOpaque || b.hasOpaque:
		if a.hasOpaque != b.hasOpaque || a.opaqueHdrs != b.opaqueHdrs {
			d.add(DiffEntry{Part: DiffPartHeaders, Path: "headers", Change: ChangeChanged})
		}
		return
	}
	i, j := 0, 0
	for i < len(a.headers) || j < len(b.headers) {
		switch {
		case j >= len(b.headers) || (i < len(a.headers) && a.headers[i].name < b.headers[j].name):
			h := a.headers[i]
			e := DiffEntry{Part: DiffPartHeaders, Path: "header:" + h.name, Change: ChangeOnlyA, Masked: h.masked}
			if !h.masked {
				e.A = strp(string(jsonString(h.value)))
			}
			d.add(e)
			i++
		case i >= len(a.headers) || b.headers[j].name < a.headers[i].name:
			h := b.headers[j]
			e := DiffEntry{Part: DiffPartHeaders, Path: "header:" + h.name, Change: ChangeOnlyB, Masked: h.masked}
			if !h.masked {
				e.B = strp(string(jsonString(h.value)))
			}
			d.add(e)
			j++
		default:
			x, y := a.headers[i], b.headers[j]
			switch {
			case x.masked && y.masked:
			case x.masked || y.masked:
				d.add(DiffEntry{Part: DiffPartHeaders, Path: "header:" + x.name, Change: ChangeChanged, Masked: true})
			case x.value != y.value:
				d.add(DiffEntry{Part: DiffPartHeaders, Path: "header:" + x.name, Change: ChangeChanged, A: strp(string(jsonString(x.value))), B: strp(string(jsonString(y.value)))})
			}
			i++
			j++
		}
	}
}

func jsonString(s string) []byte {
	var n = node{k: kStr, s: s}
	return n.bytes()
}

func (d *differ) body(a, b *Canonical) {
	d.res.Body = BodyDiff{AKind: a.bodyKind, BKind: b.bodyKind}
	switch {
	case !a.hasBody && !b.hasBody:
		d.res.Body.State = BodyNotCompared
		return
	case a.unchecked || b.unchecked:
		d.res.Body.State = BodyNotComparable
		return
	case a.hasBody != b.hasBody:
		d.res.Body.State = BodyDiffers
		return
	case a.bodyKind != b.bodyKind:
		d.res.Body.State = BodyDiffers
		return
	}
	switch a.bodyKind {
	case KindJSON:
		if a.tree == nil || b.tree == nil {
			d.res.Body.State = BodyNotComparable
			return
		}
		before := d.res.Total
		d.walk(a.tree, b.tree, nil)
		d.res.Body.State = BodyEqual
		if d.res.Total > before {
			d.res.Body.State = BodyDiffers
		}
	default: // text and empty: exactly equal or not, with no path to invent
		d.res.Body.State = BodyEqual
		if a.text != b.text {
			d.res.Body.State = BodyDiffers
		}
	}
}
