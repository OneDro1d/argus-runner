package compare

// diff_bound_test.go -- ARGUS-CMP-9 review fix, item 3: the ENCODED difference is bounded, path included.
//
// The numbers are written as literals on purpose: the test states the bound, the code must meet it.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const (
	boundPathBytes    = 256
	boundEncodedBytes = 128 * 1024
)

func bigKeyBody(key string, n, v int) string {
	var sb strings.Builder
	sb.WriteString(`{"` + key + `":{`)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"k%03d":%d`, i, v)
	}
	sb.WriteString("}}")
	return sb.String()
}

func TestDiff_AOneKeyOf200KBWith300DifferingLeavesIsBoundedInPathAndInEncodedSize(t *testing.T) {
	key := strings.Repeat("K", 200*1024)
	ca, _ := canonOf(t, nil, 200, nil, bigKeyBody(key, 300, 1))
	cb, _ := canonOf(t, nil, 200, nil, bigKeyBody(key, 300, 2))
	d := Diff(ca, cb)
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > boundEncodedBytes+4096 {
		t.Errorf("the encoded difference is %d bytes, want at most %d (+4096 for the envelope)", len(raw), boundEncodedBytes)
	}
	if d.Total != 300 {
		t.Errorf("Total = %d, want the true count 300", d.Total)
	}
	if !d.Truncated {
		t.Errorf("Truncated = false: the bound cut nothing and said nothing")
	}
	if len(d.Entries) != 200 {
		t.Errorf("%d entries listed, want 200: shortened paths must leave room for the entries the count bound allows", len(d.Entries))
	}
	for i, e := range d.Entries {
		if len(e.Path) > boundPathBytes {
			t.Errorf("entry %d: a path of %d bytes, want at most %d", i, len(e.Path), boundPathBytes)
			break
		}
	}
}

func TestDiff_ManyLargeValuesAreCutAtTheEncodedBoundAndTheTotalStaysTrue(t *testing.T) {
	leaves := func(c byte) string {
		var sb strings.Builder
		sb.WriteByte('{')
		// 75 leaves of 3000 bytes stay under the executor's own 256 KiB stored-body cut, so the body is whole
		for i := 0; i < 75; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `"k%03d":"%s"`, i, strings.Repeat(string(c), 3000))
		}
		sb.WriteByte('}')
		return sb.String()
	}
	ca, _ := canonOf(t, nil, 200, nil, leaves('a'))
	cb, _ := canonOf(t, nil, 200, nil, leaves('b'))
	d := Diff(ca, cb)
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > boundEncodedBytes+4096 {
		t.Errorf("the encoded difference is %d bytes, want at most %d (+4096 for the envelope)", len(raw), boundEncodedBytes)
	}
	if d.Total != 75 || !d.Truncated {
		t.Errorf("Total = %d Truncated = %v, want the true count 75 and the cut said", d.Total, d.Truncated)
	}
	if len(d.Entries) == 0 || len(d.Entries) >= 75 {
		t.Errorf("%d entries listed, want some but fewer than 75 (the byte bound cut first)", len(d.Entries))
	}
}

func TestDiff_TwoLongKeysThatShareALongPrefixNeverRenderAsTheSamePath(t *testing.T) {
	prefix := strings.Repeat("P", 1000)
	body := func(v int) string {
		return fmt.Sprintf(`{"%sA":%d,"%sB":%d}`, prefix, v, prefix, v)
	}
	ca, _ := canonOf(t, nil, 200, nil, body(1))
	cb, _ := canonOf(t, nil, 200, nil, body(2))
	d := Diff(ca, cb)
	if d.Total != 2 || len(d.Entries) != 2 {
		t.Fatalf("Total %d, entries %d, want 2 and 2", d.Total, len(d.Entries))
	}
	if d.Entries[0].Path == d.Entries[1].Path {
		t.Errorf("two different keys render as the same path %q", d.Entries[0].Path)
	}
	for _, e := range d.Entries {
		if len(e.Path) > boundPathBytes {
			t.Errorf("a path of %d bytes, want at most %d", len(e.Path), boundPathBytes)
		}
	}
}
