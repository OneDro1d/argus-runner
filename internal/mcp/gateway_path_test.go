package mcp

import "testing"

// extract.path reads a JSON string at a dot path, decoded.
func TestJSONStringAt(t *testing.T) {
	for _, c := range []struct {
		name, text, path, want string
		ok                     bool
	}{
		{"top-level string, decoded", `{"result":"a \"q\" b"}`, "result", `a "q" b`, true},
		{"nested path", `{"a":{"b":"x"}}`, "a.b", "x", true},
		{"array index in the path", `{"a":["x","y"]}`, "a.1", "y", true},
		{"not JSON", `plain`, "result", "", false},
		{"missing", `{"a":"x"}`, "result", "", false},
		{"an object is not a string", `{"result":{"k":"v"}}`, "result", "", false},
		{"a number is not a string", `{"result":5}`, "result", "", false},
		{"null is not a string", `{"result":null}`, "result", "", false},
		{"empty string is a string", `{"result":""}`, "result", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := JSONStringAt(c.text, c.path)
			if ok != c.ok || got != c.want {
				t.Fatalf("got %q, %v; want %q, %v", got, ok, c.want, c.ok)
			}
		})
	}
}
