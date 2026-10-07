package amqpengine

import (
	"testing"
	"time"

	envelope "github.com/OneDro1d/argus-runner/internal/msgenvelope"
	"github.com/google/uuid"
)

// TestEnvelopeRoundTrip proves EncodeEnvelope/DecodeEnvelope survive the
// msgbus v2 Avro wire format: encode a message, decode it back using the
// headers EncodeEnvelope returned, and compare every field the v2 schema
// carries (including the v2-only Intent field, which only decodes correctly
// when DecodeEnvelope picks schemaV2 off the x-envelope-schema header).
func TestEnvelopeRoundTrip(t *testing.T) {
	want := envelope.Message{
		MessageID:     uuid.NewString(),
		Kind:          envelope.KindMessage,
		Source:        envelope.SourceSystem,
		OriginTrust:   envelope.TrustAgent,
		FromAgentID:   "argus-test",
		ToInbox:       "agent.hop",
		Prompt:        "AC-D18 amqpengine round-trip probe",
		Reply:         envelope.Reply{Mode: envelope.ReplyExpects, ResponseID: "resp-ac-d18"},
		Refs:          []string{"ref-1", "ref-2"},
		CorrelationID: "corr-ac-d18",
		EnqueuedAt:    time.Now().UTC().Truncate(time.Millisecond),
		Intent:        envelope.IntentAsk,
	}

	body, headers, err := EncodeEnvelope(want)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("EncodeEnvelope returned an empty body")
	}
	if headers[envelope.HeaderSchema] != envelope.SchemaVersion2 {
		t.Fatalf("headers[%s] = %v, want %s", envelope.HeaderSchema, headers[envelope.HeaderSchema], envelope.SchemaVersion2)
	}

	got, err := DecodeEnvelope(body, headers)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}

	if got.MessageID != want.MessageID {
		t.Errorf("MessageID = %q, want %q", got.MessageID, want.MessageID)
	}
	if got.Kind != want.Kind {
		t.Errorf("Kind = %q, want %q", got.Kind, want.Kind)
	}
	if got.Source != want.Source {
		t.Errorf("Source = %q, want %q", got.Source, want.Source)
	}
	if got.OriginTrust != want.OriginTrust {
		t.Errorf("OriginTrust = %q, want %q", got.OriginTrust, want.OriginTrust)
	}
	if got.FromAgentID != want.FromAgentID {
		t.Errorf("FromAgentID = %q, want %q", got.FromAgentID, want.FromAgentID)
	}
	if got.ToInbox != want.ToInbox {
		t.Errorf("ToInbox = %q, want %q", got.ToInbox, want.ToInbox)
	}
	if got.Prompt != want.Prompt {
		t.Errorf("Prompt = %q, want %q", got.Prompt, want.Prompt)
	}
	if got.Reply != want.Reply {
		t.Errorf("Reply = %+v, want %+v", got.Reply, want.Reply)
	}
	if len(got.Refs) != len(want.Refs) || got.Refs[0] != want.Refs[0] || got.Refs[1] != want.Refs[1] {
		t.Errorf("Refs = %v, want %v", got.Refs, want.Refs)
	}
	if got.CorrelationID != want.CorrelationID {
		t.Errorf("CorrelationID = %q, want %q", got.CorrelationID, want.CorrelationID)
	}
	if !got.EnqueuedAt.Equal(want.EnqueuedAt) {
		t.Errorf("EnqueuedAt = %v, want %v", got.EnqueuedAt, want.EnqueuedAt)
	}
	if got.Intent != want.Intent {
		t.Errorf("Intent = %q, want %q (v2 field — a wrong schema pick decodes this as INFORM)", got.Intent, want.Intent)
	}
}

// TestEnvelopeRoundTrip_DefaultIntent proves the documented default: an
// unset Intent encodes and decodes as IntentInform.
func TestEnvelopeRoundTrip_DefaultIntent(t *testing.T) {
	m := envelope.Message{
		MessageID:     uuid.NewString(),
		Kind:          envelope.KindMessage,
		Source:        envelope.SourceMCP,
		OriginTrust:   envelope.TrustAgent,
		FromAgentID:   "argus-test",
		ToInbox:       "agent.hop",
		Prompt:        "no intent set",
		Reply:         envelope.Reply{Mode: envelope.ReplyNone},
		CorrelationID: "corr-default-intent",
		EnqueuedAt:    time.Now().UTC().Truncate(time.Millisecond),
	}
	body, headers, err := EncodeEnvelope(m)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	got, err := DecodeEnvelope(body, headers)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if got.Intent != envelope.IntentInform {
		t.Fatalf("Intent = %q, want default %q", got.Intent, envelope.IntentInform)
	}
}
