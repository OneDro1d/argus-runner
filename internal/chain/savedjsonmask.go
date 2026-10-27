package chain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// MASKING A JSON-SAVED VALUE.
//
// redactEverySaved replaces a saved value's exact text with ${saved.<var>}. For a scalar that is enough: the
// SUT echoes the text. A JSON value is a STRUCTURE, and the SUT echoes it in whatever layout it likes (indented,
// keys in another order, a number written 1.0 where it was saved 1), so the text of the saved object is rarely
// a substring of the answer. A recorded output is worse: compare scrubs string LEAVES only, and a saved object
// is never a leaf. So a JSON-saved value is also masked as a SUBTREE: wherever a JSON document holds a value
// equal to the saved one, that value is replaced by the string "${saved.<var>}". Equal means the same JSON
// value, numbers compared as numbers: the text a judge renders (sorted keys, float64) must still match.
//
// Held to the same floor as every saved value (minRedactedSavedLen): a saved `1` or `true` is not a credential,
// and replacing every 1 in an answer would garble it.

type jsonSavedValue struct {
	name string
	text string // the saved JSON text, compact
	tree any    // the same value, decoded (numbers as float64)
	str  string // a JSON string's content, without quotes ("" otherwise)
}

// jsonSavedValues lists, longest first, every JSON-saved variable of at least minRedactedSavedLen bytes.
func jsonSavedValues(vars map[string]string) []jsonSavedValue {
	var out []jsonSavedValue
	for k := range vars {
		if !strings.HasPrefix(k, jsonKindPrefix) {
			continue
		}
		name := strings.TrimPrefix(k, jsonKindPrefix)
		text, ok := vars[name]
		if !ok || len(text) < minRedactedSavedLen {
			continue
		}
		var tree any
		if json.Unmarshal([]byte(text), &tree) != nil {
			continue
		}
		v := jsonSavedValue{name: name, text: text, tree: tree}
		if s, isStr := tree.(string); isStr && len(s) >= minRedactedSavedLen {
			v.str = s
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].text) != len(out[j].text) {
			return len(out[i].text) > len(out[j].text)
		}
		return out[i].name < out[j].name
	})
	return out
}

// maskJSONSavedText returns text with every subtree equal to a JSON-saved value replaced, when text is a JSON
// document; text that is not JSON, or holds no such subtree, comes back unchanged (the same string). A JSON
// string's content (an unquoted fragment of an echo) is replaced too, as a text-saved value's would be.
func maskJSONSavedText(text string, vars map[string]string) string {
	vals := jsonSavedValues(vars)
	if len(vals) == 0 {
		return text
	}
	if out, changed := maskTree(json.RawMessage(text), vals); changed {
		text = string(out)
	}
	for _, v := range vals {
		if v.str != "" {
			text = strings.ReplaceAll(text, v.str, "${saved."+v.name+"}")
		}
	}
	return text
}

// maskJSONSavedBody is maskJSONSavedText for a response body about to be recorded. A body that is not JSON, or
// holds nothing to mask, is returned as the very same bytes, so a chain that saves no JSON records exactly what
// it recorded before.
func maskJSONSavedBody(body []byte, vars map[string]string) []byte {
	vals := jsonSavedValues(vars)
	if len(vals) == 0 || len(body) == 0 || len(body) > maxHTTPBodyBytes {
		return body
	}
	if out, changed := maskTree(json.RawMessage(body), vals); changed {
		return out
	}
	return body
}

// maskTree walks raw and replaces every subtree equal to one of vals by the JSON string "${saved.<name>}".
// changed is false (and raw is returned as it came) when nothing matched.
func maskTree(raw json.RawMessage, vals []jsonSavedValue) (json.RawMessage, bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return raw, false
	}
	var cur any
	if json.Unmarshal(t, &cur) != nil {
		return raw, false
	}
	for _, v := range vals {
		if reflect.DeepEqual(cur, v.tree) {
			return json.RawMessage(strconv.Quote("${saved." + v.name + "}")), true
		}
	}
	switch t[0] {
	case '{':
		var m map[string]json.RawMessage
		if json.Unmarshal(t, &m) != nil {
			return raw, false
		}
		changed := false
		for k, child := range m {
			if c, ok := maskTree(child, vals); ok {
				m[k], changed = c, true
			}
		}
		if !changed {
			return raw, false
		}
		out, err := json.Marshal(m)
		return out, err == nil
	case '[':
		var a []json.RawMessage
		if json.Unmarshal(t, &a) != nil {
			return raw, false
		}
		changed := false
		for i, child := range a {
			if c, ok := maskTree(child, vals); ok {
				a[i], changed = c, true
			}
		}
		if !changed {
			return raw, false
		}
		out, err := json.Marshal(a)
		return out, err == nil
	}
	return raw, false
}
