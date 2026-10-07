// Package msgenvelope is Argus's own copy of the message format a publish step uses when it
// names no schema of its own.
//
// The format was first defined by the msgbus message bus (its pkg/envelope package, taken at
// github.com/example/msgbus@v0.0.0-20260918185505-f985dc760377). Argus used to import that
// private module. It now carries the small part it needs, so the build needs no access to msgbus.
//
// Rules for this package:
//   - What goes on the wire must not change by accident. testdata/golden holds the exact bytes the
//     original package produced, and golden_test.go checks this copy against them.
//   - When msgbus changes its envelope, this copy is updated on purpose, with new golden bytes.
//   - A scenario never has to use this format. It can bring its own schema.
//
// Encode always writes the v2 shape. A publisher must also send PublishHeaders so the reader picks
// the v2 schema. DecodeWithHeaders picks v2 when that header says "2" and falls back to the v1
// shape otherwise.
package msgenvelope

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hamba/avro/v2"
)

type Kind string

const (
	KindMessage       Kind = "MESSAGE"
	KindExpiredNotice Kind = "EXPIRED_NOTICE"
)

type Source string

const (
	SourceMCP     Source = "MCP"
	SourceWebhook Source = "WEBHOOK"
	SourceSlack   Source = "SLACK"
	SourceSystem  Source = "SYSTEM"
)

type Trust string

const (
	TrustAgent    Trust = "AGENT"
	TrustExternal Trust = "EXTERNAL"
	TrustSystem   Trust = "SYSTEM"
)

// Intent is the purpose of a message. This is the Avro enum symbol spelling.
type Intent string

const (
	IntentInform          Intent = "INFORM"
	IntentAsk             Intent = "ASK"
	IntentDecisionRequest Intent = "DECISION_REQUEST"
	IntentApproval        Intent = "APPROVAL"
)

type ReplyMode int

const (
	ReplyNone ReplyMode = iota
	ReplyExpects
	ReplyTo
)

// Reply is the Go view of the Avro reply union (NoReply, ExpectsReply, ReplyTo).
type Reply struct {
	Mode       ReplyMode
	ResponseID string
}

// ExpiredRef points at the original message inside an EXPIRED_NOTICE.
type ExpiredRef struct {
	MessageID         string
	ToInbox           string
	ExpiredForAgentID string
	EnqueuedAt        time.Time
	TTLMs             int64
}

// Message is the EnvelopeMessage record.
type Message struct {
	MessageID       string
	Kind            Kind
	Source          Source
	OriginTrust     Trust
	FromAgentID     string
	ToInbox         string
	Prompt          string
	Reply           Reply
	Refs            []string
	CorrelationID   string
	EnqueuedAt      time.Time
	ExpiredOriginal *ExpiredRef
	Intent          Intent // "" encodes as IntentInform
	// Supersedes travels in an AMQP header, not in the Avro body.
	Supersedes Supersede
}

// SchemaJSON is the v1 Avro schema. Only the v1 reader uses it.
const SchemaJSON = `{
  "type": "record", "name": "EnvelopeMessage", "namespace": "argus.envelope.v1",
  "fields": [
    {"name": "messageId",     "type": {"type": "string", "logicalType": "uuid"}},
    {"name": "kind",          "type": {"type": "enum", "name": "Kind", "symbols": ["MESSAGE", "EXPIRED_NOTICE"]}},
    {"name": "source",        "type": {"type": "enum", "name": "Source", "symbols": ["MCP", "WEBHOOK", "SLACK", "SYSTEM"]}},
    {"name": "originTrust",   "type": {"type": "enum", "name": "Trust", "symbols": ["AGENT", "EXTERNAL", "SYSTEM"]}},
    {"name": "fromAgentId",   "type": "string"},
    {"name": "toInbox",       "type": "string"},
    {"name": "prompt",        "type": "string"},
    {"name": "reply",         "type": [
        {"type": "record", "name": "NoReply",      "fields": []},
        {"type": "record", "name": "ExpectsReply", "fields": [{"name": "responseId", "type": "string"}]},
        {"type": "record", "name": "ReplyTo",      "fields": [{"name": "responseId", "type": "string"}]}
    ]},
    {"name": "refs",          "type": {"type": "array", "items": "string"}, "default": []},
    {"name": "correlationId", "type": "string"},
    {"name": "enqueuedAt",    "type": {"type": "long", "logicalType": "timestamp-millis"}},
    {"name": "expiredOriginal", "type": ["null", {"type": "record", "name": "ExpiredRef", "fields": [
        {"name": "messageId", "type": "string"},
        {"name": "toInbox", "type": "string"},
        {"name": "expiredForAgentId", "type": "string"},
        {"name": "enqueuedAt", "type": {"type": "long", "logicalType": "timestamp-millis"}},
        {"name": "ttlMs", "type": "long"}
    ]}], "default": null}
  ]
}`

// SchemaJSONV2 is the v2 Avro schema: the v1 shape plus a trailing intent field.
const SchemaJSONV2 = `{
  "type": "record", "name": "EnvelopeMessage", "namespace": "argus.envelope.v2",
  "fields": [
    {"name": "messageId",     "type": {"type": "string", "logicalType": "uuid"}},
    {"name": "kind",          "type": {"type": "enum", "name": "Kind", "symbols": ["MESSAGE", "EXPIRED_NOTICE"]}},
    {"name": "source",        "type": {"type": "enum", "name": "Source", "symbols": ["MCP", "WEBHOOK", "SLACK", "SYSTEM"]}},
    {"name": "originTrust",   "type": {"type": "enum", "name": "Trust", "symbols": ["AGENT", "EXTERNAL", "SYSTEM"]}},
    {"name": "fromAgentId",   "type": "string"},
    {"name": "toInbox",       "type": "string"},
    {"name": "prompt",        "type": "string"},
    {"name": "reply",         "type": [
        {"type": "record", "name": "NoReply",      "fields": []},
        {"type": "record", "name": "ExpectsReply", "fields": [{"name": "responseId", "type": "string"}]},
        {"type": "record", "name": "ReplyTo",      "fields": [{"name": "responseId", "type": "string"}]}
    ]},
    {"name": "refs",          "type": {"type": "array", "items": "string"}, "default": []},
    {"name": "correlationId", "type": "string"},
    {"name": "enqueuedAt",    "type": {"type": "long", "logicalType": "timestamp-millis"}},
    {"name": "expiredOriginal", "type": ["null", {"type": "record", "name": "ExpiredRef", "fields": [
        {"name": "messageId", "type": "string"},
        {"name": "toInbox", "type": "string"},
        {"name": "expiredForAgentId", "type": "string"},
        {"name": "enqueuedAt", "type": {"type": "long", "logicalType": "timestamp-millis"}},
        {"name": "ttlMs", "type": "long"}
    ]}], "default": null},
    {"name": "intent", "type": {"type": "enum", "name": "Intent", "symbols": ["INFORM", "ASK", "DECISION_REQUEST", "APPROVAL"]}, "default": "INFORM"}
  ]
}`

var schemaV1 = avro.MustParse(SchemaJSON)
var schemaV2 = avro.MustParse(SchemaJSONV2)

const nsV1 = "argus.envelope.v1"
const nsV2 = "argus.envelope.v2"

// HeaderSchema is the AMQP header a v2 publisher sets so the reader decodes with the v2 schema.
const HeaderSchema = "x-envelope-schema"

// SchemaVersion2 is the HeaderSchema value a v2 publisher sets.
const SchemaVersion2 = "2"

// PublishHeaders returns the AMQP headers a v2 publisher sets next to the Avro body. The type is
// map[string]any so it assigns straight into an amqp.Table.
func PublishHeaders() map[string]any {
	return map[string]any{HeaderSchema: SchemaVersion2}
}

// HeaderSupersedes carries a Supersede value. It is a header, not an Avro field.
const HeaderSupersedes = "x-envelope-supersedes"

type SupersedeScope string

const (
	SupersedeNone   SupersedeScope = ""
	SupersedeThread SupersedeScope = "thread"
	SupersedeAll    SupersedeScope = "all"
	SupersedeIDs    SupersedeScope = "ids"
)

// Supersede is the parsed header. The zero value supersedes nothing.
type Supersede struct {
	Scope SupersedeScope
	IDs   []string // only for SupersedeIDs
}

var supersedeIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

const maxSupersedeIDs = 50

// ParseSupersedes reads a header value. Anything it does not understand supersedes nothing.
func ParseSupersedes(v string) Supersede {
	v = strings.TrimSpace(v)
	switch {
	case v == string(SupersedeThread):
		return Supersede{Scope: SupersedeThread}
	case v == string(SupersedeAll):
		return Supersede{Scope: SupersedeAll}
	case strings.HasPrefix(v, "ids:"):
		var ids []string
		for _, id := range strings.Split(strings.TrimPrefix(v, "ids:"), ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if !supersedeIDRe.MatchString(id) || len(ids) == maxSupersedeIDs {
				return Supersede{}
			}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return Supersede{}
		}
		return Supersede{Scope: SupersedeIDs, IDs: ids}
	}
	return Supersede{}
}

// buildRecord builds the raw Avro record for m. The reply and expiredOriginal unions are keyed
// under the namespace ns.
func buildRecord(m Message, ns string) (map[string]any, error) {
	var reply map[string]any
	switch m.Reply.Mode {
	case ReplyNone:
		reply = map[string]any{ns + ".NoReply": map[string]any{}}
	case ReplyExpects:
		reply = map[string]any{ns + ".ExpectsReply": map[string]any{"responseId": m.Reply.ResponseID}}
	case ReplyTo:
		reply = map[string]any{ns + ".ReplyTo": map[string]any{"responseId": m.Reply.ResponseID}}
	default:
		return nil, fmt.Errorf("invalid reply mode %d", m.Reply.Mode)
	}

	refs := m.Refs
	if refs == nil {
		refs = []string{}
	}

	rec := map[string]any{
		"messageId":       m.MessageID,
		"kind":            string(m.Kind),
		"source":          string(m.Source),
		"originTrust":     string(m.OriginTrust),
		"fromAgentId":     m.FromAgentID,
		"toInbox":         m.ToInbox,
		"prompt":          m.Prompt,
		"reply":           reply,
		"refs":            refs,
		"correlationId":   m.CorrelationID,
		"enqueuedAt":      m.EnqueuedAt.UTC(),
		"expiredOriginal": nil,
	}
	if m.ExpiredOriginal != nil {
		rec["expiredOriginal"] = map[string]any{ns + ".ExpiredRef": map[string]any{
			"messageId":         m.ExpiredOriginal.MessageID,
			"toInbox":           m.ExpiredOriginal.ToInbox,
			"expiredForAgentId": m.ExpiredOriginal.ExpiredForAgentID,
			"enqueuedAt":        m.ExpiredOriginal.EnqueuedAt.UTC(),
			"ttlMs":             m.ExpiredOriginal.TTLMs,
		}}
	}
	return rec, nil
}

// decodeRecord is the inverse of buildRecord.
func decodeRecord(rec map[string]any, ns string) Message {
	m := Message{
		MessageID:     str(rec["messageId"]),
		Kind:          Kind(str(rec["kind"])),
		Source:        Source(str(rec["source"])),
		OriginTrust:   Trust(str(rec["originTrust"])),
		FromAgentID:   str(rec["fromAgentId"]),
		ToInbox:       str(rec["toInbox"]),
		Prompt:        str(rec["prompt"]),
		CorrelationID: str(rec["correlationId"]),
	}
	if t, ok := rec["enqueuedAt"].(time.Time); ok {
		m.EnqueuedAt = t
	}
	if rs, ok := rec["refs"].([]any); ok {
		for _, r := range rs {
			m.Refs = append(m.Refs, str(r))
		}
	}
	switch ru := rec["reply"].(type) {
	case map[string]any:
		if v, ok := ru[ns+".ExpectsReply"]; ok {
			m.Reply = Reply{Mode: ReplyExpects, ResponseID: fieldStr(v, "responseId")}
		} else if v, ok := ru[ns+".ReplyTo"]; ok {
			m.Reply = Reply{Mode: ReplyTo, ResponseID: fieldStr(v, "responseId")}
		} else {
			m.Reply = Reply{Mode: ReplyNone}
		}
	default:
		m.Reply = Reply{Mode: ReplyNone}
	}
	if eo, ok := rec["expiredOriginal"].(map[string]any); ok {
		// A nullable union: the reader yields a map keyed by type name, or the record map itself.
		inner := eo
		if v, ok := eo[ns+".ExpiredRef"].(map[string]any); ok {
			inner = v
		}
		ref := &ExpiredRef{
			MessageID:         fieldStr(inner, "messageId"),
			ToInbox:           fieldStr(inner, "toInbox"),
			ExpiredForAgentID: fieldStr(inner, "expiredForAgentId"),
		}
		if t, ok := inner["enqueuedAt"].(time.Time); ok {
			ref.EnqueuedAt = t
		}
		switch v := inner["ttlMs"].(type) {
		case int64:
			ref.TTLMs = v
		case int:
			ref.TTLMs = int64(v)
		}
		m.ExpiredOriginal = ref
	}
	return m
}

// Encode serializes a Message to v2 Avro binary. An unset Intent encodes as IntentInform. The
// publisher must also send PublishHeaders.
func Encode(m Message) ([]byte, error) {
	rec, err := buildRecord(m, nsV2)
	if err != nil {
		return nil, err
	}
	intent := m.Intent
	if intent == "" {
		intent = IntentInform
	}
	rec["intent"] = string(intent)
	return avro.Marshal(schemaV2, rec)
}

// Decode parses Avro binary as the v1 shape. It always reports Intent as IntentInform, because v1
// has no intent and the trailing intent bytes of a v2 body are never read. Prefer
// DecodeWithHeaders.
func Decode(b []byte) (Message, error) {
	if len(b) == 0 {
		return Message{}, errors.New("empty envelope")
	}
	var rec map[string]any
	if err := avro.Unmarshal(schemaV1, b, &rec); err != nil {
		return Message{}, fmt.Errorf("avro decode: %w", err)
	}
	m := decodeRecord(rec, nsV1)
	m.Intent = IntentInform
	return m, nil
}

// DecodeWithHeaders decodes with the v2 schema when headers carries HeaderSchema == SchemaVersion2,
// and with the v1 shape otherwise.
func DecodeWithHeaders(body []byte, headers map[string]any) (Message, error) {
	if headerString(headers, HeaderSchema) != SchemaVersion2 {
		return Decode(body)
	}
	if len(body) == 0 {
		return Message{}, errors.New("empty envelope")
	}
	var rec map[string]any
	if err := avro.Unmarshal(schemaV2, body, &rec); err != nil {
		return Message{}, fmt.Errorf("avro decode: %w", err)
	}
	m := decodeRecord(rec, nsV2)
	m.Intent = Intent(str(rec["intent"]))
	if m.Intent == "" {
		m.Intent = IntentInform
	}
	m.Supersedes = ParseSupersedes(headerString(headers, HeaderSupersedes))
	return m, nil
}

func headerString(headers map[string]any, key string) string {
	v, ok := headers[key]
	if !ok {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return fmt.Sprintf("%v", s)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func fieldStr(v any, key string) string {
	if m, ok := v.(map[string]any); ok {
		return str(m[key])
	}
	return ""
}
