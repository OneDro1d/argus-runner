package amqpengine

// The msgbus v2 Avro envelope encoder. It wraps Argus's own copy of that format,
// internal/msgenvelope (the private msgbus module is no longer imported). See
// envelope_test.go for the round-trip proof the wire format survives Encode/Decode through
// this wrapper, and internal/msgenvelope/testdata/golden for the exact bytes.
import (
	envelope "github.com/OneDro1d/argus-runner/internal/msgenvelope"
)

// EncodeEnvelope Avro-encodes m as a msgbus v2 EnvelopeMessage
// (envelope.SchemaJSONV2) and returns the wire body plus the AMQP headers a
// v2 publisher must set alongside it (envelope.PublishHeaders:
// x-envelope-schema=2), so a caller can drop both straight into an
// amqp.Publishing{Body: body, Headers: amqp.Table(headers)}.
func EncodeEnvelope(m envelope.Message) (body []byte, headers map[string]any, err error) {
	body, err = envelope.Encode(m)
	if err != nil {
		return nil, nil, err
	}
	return body, envelope.PublishHeaders(), nil
}

// DecodeEnvelope is EncodeEnvelope's inverse: it picks the v2 schema when
// headers carries envelope.HeaderSchema == envelope.SchemaVersion2 (falling
// back to v1 otherwise), exactly as envelope.DecodeWithHeaders does.
func DecodeEnvelope(body []byte, headers map[string]any) (envelope.Message, error) {
	return envelope.DecodeWithHeaders(body, headers)
}
