package obsquery

import "testing"

func TestLeadingSelector(t *testing.T) {
	for in, want := range map[string]string{
		`{a="b"} |= "x"`:             `{a="b"}`,
		`{a="}",b="\"}"} |= "}"`:     `{a="}",b="\"}"}`,
		`{cluster="p"} | json l="l"`: `{cluster="p"}`,
	} {
		if got, ok := leadingSelector(in); !ok || got != want {
			t.Errorf("%s: got %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{``, `|= "x"`, `{a="b"`} {
		if got, ok := leadingSelector(in); ok {
			t.Errorf("%q: want no selector, got %q", in, got)
		}
	}
}
