package scenario

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// PER-CALL Ed25519 SIGNING OF AN `mcp` CHAIN STEP.
//
// A system under test that identifies a caller only by a signature computed per call (msgbus' public MCP door) can
// be driven only if the executor signs. A step may carry a `sign` block:
//
//	"sign": {
//	  "key": "MEMBER_A_KEY",                    a NAME declared under check_env - never the key
//	  "key_id": "k-123", "key_id_header": "x-msgbus-key-id",     (both or neither; key_id may be ${saved.x})
//	  "fields": [ {"literal":"v4"}, {"path":"messageId"}, {"path":"prompt","sha256":"hex"} ],
//	  "separator": "|",                         joins the fields (required with two or more)
//	  "arg": "/signatures/agent.inbox",         where the signature goes in the request arguments, and/or
//	  "header": "X-Signature"                   the header that carries it
//	}
//
// or, instead of `fields`, a value an earlier step saved, signed as it is: "value": "${saved.canonical}".
//
// This file is the config-free half (door A, the validator, and door B, the executor's preflight, ask the same
// functions). The half that needs argus-config - is the key NAME declared under check_env - is argus.CheckSigningKeys.
// The private key is read from the executor's environment at run time and appears in no check, report or log.

// MCPSign is an mcp step's `sign` block.
type MCPSign struct {
	Key         string      `json:"key"`
	KeyID       string      `json:"key_id"`
	KeyIDHeader string      `json:"key_id_header"`
	Fields      []SignField `json:"fields"`
	// Separator is a pointer so an explicitly empty separator is told apart from none.
	Separator *string `json:"separator"`
	Value     string  `json:"value"`
	Arg       string  `json:"arg"`
	Header    string  `json:"header"`
}

// SignField is one element of the signed template: a literal, or the value at a path in the request arguments,
// optionally sha256-hashed (hex: lower-case hexadecimal; base64: standard encoding with padding).
type SignField struct {
	Literal *string `json:"literal"`
	Path    string  `json:"path"`
	Sha256  string  `json:"sha256"`
}

var signKeyAllowed = map[string]bool{"key": true, "key_id": true, "key_id_header": true, "fields": true,
	"separator": true, "value": true, "arg": true, "header": true}

var signFieldAllowed = map[string]bool{"literal": true, "path": true, "sha256": true}

var headerNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// signReservedHeaders are the headers a sign block may never write: the runner's and the protocol's own.
func signReservedHeader(h string) bool {
	switch http.CanonicalHeaderKey(h) {
	case "X-Correlation-Id", "Mcp-Session-Id", "Content-Type", "Content-Length", "Host", "Accept":
		return true
	}
	return false
}

// SignPathSegments splits a path into segments. A path that starts with "/" is a JSON pointer (RFC 6901: "~1" is
// "/", "~0" is "~"), the form for a key that contains a dot; anything else is the dot path `save` uses.
func SignPathSegments(p string) ([]string, bool) {
	if p == "" {
		return nil, false
	}
	if strings.HasPrefix(p, "/") {
		parts := strings.Split(p[1:], "/")
		for i, s := range parts {
			if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(s, "~0", ""), "~1", ""), "~") {
				return nil, false
			}
			s = strings.ReplaceAll(s, "~1", "/")
			parts[i] = strings.ReplaceAll(s, "~0", "~")
			if parts[i] == "" {
				return nil, false
			}
		}
		return parts, true
	}
	parts := strings.Split(p, ".")
	for _, s := range parts {
		if s == "" {
			return nil, false
		}
	}
	return parts, true
}

func signPathProblem(what, p string) string {
	if _, ok := SignPathSegments(p); ok {
		return ""
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Sprintf("`%s` %q is not a valid JSON pointer (segments are non-empty; \"~\" is written \"~0\" and \"/\" \"~1\")", what, p)
	}
	return fmt.Sprintf("`%s` %q is not a valid path (dot-separated, no empty segment; start with \"/\" for a JSON pointer)", what, p)
}

// MCPSignRawProblems refuses a key the block does not take, by name, from the block's raw JSON.
func MCPSignRawProblems(raw json.RawMessage) []string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	var out []string
	var extra []string
	for k := range m {
		if !signKeyAllowed[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		out = append(out, fmt.Sprintf("`sign` carries the field %q, which it does not accept (it takes: key, key_id, key_id_header, fields, separator, value, arg, header)", k))
	}
	var fs []map[string]json.RawMessage
	if f, ok := m["fields"]; ok && json.Unmarshal(f, &fs) == nil {
		for i, f := range fs {
			for k := range f {
				if !signFieldAllowed[k] {
					out = append(out, fmt.Sprintf("`sign.fields[%d]` carries the field %q, which it does not accept (it takes: literal, path, sha256)", i, k))
				}
			}
		}
	}
	return out
}

// MCPSignProblems is the ONE judgement of a `sign` block: short sentences, "" list when it is usable.
func MCPSignProblems(s *MCPSign) []string {
	if s == nil {
		return nil
	}
	var out []string
	switch {
	case strings.TrimSpace(s.Key) == "":
		out = append(out, "`sign` needs a `key`: the NAME of an environment variable declared under check_env that holds the signing key (never the key itself)")
	case !ValidURLEnv(s.Key):
		out = append(out, "`sign.key` is not an environment variable NAME - use letters A-Z, digits and underscores, not starting with a digit (a name, never the key)")
	}
	hasFields, hasValue := len(s.Fields) > 0, s.Value != ""
	if hasFields == hasValue {
		out = append(out, "`sign` needs exactly one of `fields` (a template of request fields) or `value` (a ${saved.<var>} to sign as it is)")
	}
	if hasValue {
		if !savedWholeRefRe.MatchString(s.Value) {
			out = append(out, "`sign.value` must be a whole ${saved.<var>} reference (the value an earlier step saved, signed as it is)")
		}
		if s.Separator != nil {
			out = append(out, "`sign.separator` belongs to `fields`; a `value` is signed as it is")
		}
	}
	for i, f := range s.Fields {
		where := fmt.Sprintf("`sign.fields[%d]`", i)
		if (f.Literal != nil) == (f.Path != "") {
			out = append(out, where+" needs exactly one of `literal` or `path`")
		}
		if f.Path != "" {
			if why := signPathProblem(fmt.Sprintf("sign.fields[%d].path", i), f.Path); why != "" {
				out = append(out, why)
			}
		}
		if f.Sha256 != "" && f.Sha256 != "hex" && f.Sha256 != "base64" {
			out = append(out, fmt.Sprintf("%s `sha256` must be \"hex\" or \"base64\" (the encoding of the digest), not %q", where, f.Sha256))
		}
	}
	if len(s.Fields) > 1 && s.Separator == nil {
		out = append(out, "`sign.separator` is required with two or more `fields` (it may be \"\" to join them with nothing)")
	}
	if s.Arg == "" && s.Header == "" {
		out = append(out, "`sign` needs `arg` and/or `header`: where the signature (base64, standard encoding) is put")
	}
	if s.Arg != "" {
		if why := signPathProblem("sign.arg", s.Arg); why != "" {
			out = append(out, why)
		}
	}
	for _, h := range [][2]string{{"header", s.Header}, {"key_id_header", s.KeyIDHeader}} {
		if h[1] == "" {
			continue
		}
		switch {
		case !headerNameRe.MatchString(h[1]):
			out = append(out, fmt.Sprintf("`sign.%s` %q is not a valid HTTP header name", h[0], h[1]))
		case signReservedHeader(h[1]):
			out = append(out, fmt.Sprintf("`sign.%s` %q is reserved: the runner or the protocol owns that header", h[0], h[1]))
		}
	}
	if (s.KeyID == "") != (s.KeyIDHeader == "") {
		out = append(out, "`key_id` and `key_id_header` go together: the id of the key, and the header that carries it")
	}
	if s.Header != "" && s.KeyIDHeader != "" && strings.EqualFold(s.Header, s.KeyIDHeader) {
		out = append(out, "`sign.header` and `sign.key_id_header` name the same header")
	}
	return out
}
