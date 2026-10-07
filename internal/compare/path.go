package compare

import (
	"fmt"
	"strconv"
	"strings"
)

// The path grammar is closed (design 2.1): `$`, then up to MaxPathSegments segments, each `.name`
// (letters, digits, `_`, `-`), `[*]` (every element of an array) or `[n]` (one element). No filters,
// no regex, no recursive descent. A header is `header:<Name>`. Anything else is refused by name.

// SegKind is the kind of one path segment.
type SegKind int

const (
	SegKey   SegKind = iota // .name
	SegAny                  // [*]
	SegIndex                // [n]
)

// Seg is one segment of a body path.
type Seg struct {
	Kind  SegKind
	Name  string
	Index int
}

// Path is a parsed body path or header reference.
type Path struct {
	header   string // lower-case name when this is a header path
	isHeader bool
	segs     []Seg
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// ParsePath parses one path. The error names the offending text and what was expected.
func ParsePath(s string) (Path, error) {
	if rest, ok := strings.CutPrefix(s, "header:"); ok {
		if rest == "" {
			return Path{}, fmt.Errorf("%q names no header after `header:`", s)
		}
		for i := 0; i < len(rest); i++ {
			if !isNameByte(rest[i]) {
				return Path{}, fmt.Errorf("%q: a header name has letters, digits, `_` and `-` only", s)
			}
		}
		return Path{header: strings.ToLower(rest), isHeader: true}, nil
	}
	if !strings.HasPrefix(s, "$") {
		return Path{}, fmt.Errorf("%q is not a path: it starts with `$` (a body path) or `header:` (a header)", s)
	}
	var segs []Seg
	i := 1
	for i < len(s) {
		switch s[i] {
		case '.':
			j := i + 1
			for j < len(s) && isNameByte(s[j]) {
				j++
			}
			if j == i+1 {
				return Path{}, fmt.Errorf("%q: `.` must be followed by a name (letters, digits, `_`, `-`)", s)
			}
			segs = append(segs, Seg{Kind: SegKey, Name: s[i+1 : j]})
			i = j
		case '[':
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				return Path{}, fmt.Errorf("%q: `[` is never closed", s)
			}
			inner := s[i+1 : i+end]
			switch {
			case inner == "*":
				segs = append(segs, Seg{Kind: SegAny})
			case inner != "" && len(inner) <= 6 && allDigits(inner):
				n, _ := strconv.Atoi(inner)
				segs = append(segs, Seg{Kind: SegIndex, Index: n})
			default:
				return Path{}, fmt.Errorf("%q: `[%s]` is not allowed, only `[*]` or `[n]` (no filters, no regex)", s, inner)
			}
			i += end + 1
		default:
			return Path{}, fmt.Errorf("%q: unexpected %q at position %d (a path is `$`, `.name`, `[*]`, `[n]`)", s, string(s[i]), i)
		}
		if len(segs) > MaxPathSegments {
			return Path{}, fmt.Errorf("%q has more than %d segments", s, MaxPathSegments)
		}
	}
	return Path{segs: segs}, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// String is the canonical text: header names lower-cased, everything else as written.
func (p Path) String() string {
	if p.isHeader {
		return "header:" + p.header
	}
	var b strings.Builder
	b.WriteByte('$')
	for _, sg := range p.segs {
		switch sg.Kind {
		case SegKey:
			b.WriteByte('.')
			b.WriteString(sg.Name)
		case SegAny:
			b.WriteString("[*]")
		case SegIndex:
			b.WriteString("[" + strconv.Itoa(sg.Index) + "]")
		}
	}
	return b.String()
}

// IsHeader reports whether this is a `header:<Name>` reference.
func (p Path) IsHeader() bool { return p.isHeader }

// Header is the lower-case header name, "" for a body path.
func (p Path) Header() string { return p.header }

// atOrUnder reports whether some location that q addresses lies at or under a location p addresses:
// p's segments are a prefix of q's, each pair of segments able to name the same step ([*] overlaps
// every [n]).
func (p Path) atOrUnder(q Path) bool {
	if p.isHeader || q.isHeader || len(p.segs) > len(q.segs) {
		return false
	}
	for i, a := range p.segs {
		b := q.segs[i]
		switch {
		case a.Kind == SegKey || b.Kind == SegKey:
			if a.Kind != b.Kind || a.Name != b.Name {
				return false
			}
		case a.Kind == SegIndex && b.Kind == SegIndex:
			if a.Index != b.Index {
				return false
			}
		}
	}
	return true
}

// matches reports whether this body path addresses the concrete location loc (only SegKey and
// SegIndex appear in a location).
func (p Path) matches(loc []Seg) bool {
	if p.isHeader || len(p.segs) != len(loc) {
		return false
	}
	for i, sg := range p.segs {
		l := loc[i]
		switch sg.Kind {
		case SegKey:
			if l.Kind != SegKey || l.Name != sg.Name {
				return false
			}
		case SegAny:
			if l.Kind != SegIndex {
				return false
			}
		case SegIndex:
			if l.Kind != SegIndex || l.Index != sg.Index {
				return false
			}
		}
	}
	return true
}
