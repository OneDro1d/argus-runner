package scenario

import (
	"encoding/json"
	"testing"
)

func TestContentTypeIsJSON(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/json":                  true,
		"Application/JSON":                  true,
		"application/json; charset=utf-8":   true,
		"  application/json  ":              true,
		"application/merge-patch+json":      true,
		"application/vnd.api+JSON; v=1":     true,
		"text/plain":                        false,
		"text/plain; charset=utf-8":         false,
		"application/x-www-form-urlencoded": false,
		"application/jsonp":                 false,
		"text/json":                         true, // amended 2026-10-11: text/json counts as JSON
		"Text/JSON; charset=utf-8":          true,
		"  text/json  ":                     true,
		"application/x-ndjson":              false,
		"":                                  false,
	} {
		if got := ContentTypeIsJSON(ct); got != want {
			t.Errorf("ContentTypeIsJSON(%q) = %v, want %v", ct, got, want)
		}
	}
}

func TestTextBodyOf(t *testing.T) {
	str := json.RawMessage(`"line one\nline \"two\" \\ end"`)
	plain := map[string]string{"Content-Type": "text/plain"}
	cases := []struct {
		name string
		body json.RawMessage
		hdr  map[string]string
		text string
		ok   bool
	}{
		{"string + text/plain: the unescaped text", str, plain, "line one\nline \"two\" \\ end", true},
		{"header name in any case", str, map[string]string{"content-TYPE": "text/plain"}, "line one\nline \"two\" \\ end", true},
		{"string + form type", json.RawMessage(`"a=1&b=2"`), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, "a=1&b=2", true},
		{"empty string is the empty text", json.RawMessage(`""`), plain, "", true},
		{"no header", str, nil, "", false},
		{"an empty header declares nothing", str, map[string]string{"Content-Type": "  "}, "", false},
		{"json type", str, map[string]string{"Content-Type": "application/json"}, "", false},
		{"text/json is JSON, so the string stays quoted", str, map[string]string{"Content-Type": "text/json"}, "", false},
		{"+json type", str, map[string]string{"Content-Type": "application/merge-patch+json"}, "", false},
		{"a placeholder not yet known is not guessed at", str, map[string]string{"Content-Type": "${saved.ct}"}, "", false},
		{"object body", json.RawMessage(`{"a":1}`), plain, "", false},
		{"array body", json.RawMessage(`[1]`), plain, "", false},
		{"number body", json.RawMessage(`5`), plain, "", false},
		{"boolean body", json.RawMessage(`true`), plain, "", false},
		{"null body", json.RawMessage(`null`), plain, "", false},
		{"no body", nil, plain, "", false},
	}
	for _, c := range cases {
		text, ok := TextBodyOf(c.body, c.hdr)
		if ok != c.ok || text != c.text {
			t.Errorf("%s: (%q, %v), want (%q, %v)", c.name, text, ok, c.text, c.ok)
		}
	}
}
