package chain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/OneDro1d/argus-runner/internal/avroschema"
)

// lookupEnv reads an environment variable by name — a thin wrapper so every read on the record
// path goes through one place (matches scrubBroker's "one seam" style elsewhere in this file's
// package for the SAME reason: nothing here ever needs the URL's env, but a record's non-secret
// ${VAR} legitimately does, exactly as resolveVars does for every other amqp-step field).
func lookupEnv(name string) (string, bool) { return os.LookupEnv(name) }

// ─────────────────────────────────────────────────────────────────────────────────────────
// RUN-TIME RECORD RESOLUTION (ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28, design §4).
//
// internal/argus/chain_scenario.go builds AMQPSpec.RecordTemplate from the step's PRISTINE
// (never textually substituted) `record` JSON: parsed into a generic tree, then
// avroschema.MarkWholeValuePlaceholders wraps every leaf that is EXACTLY one `${…}` token as an
// avroschema.Placeholder carrying that token's own text — so a leaf's "is this whole-value?"
// question is answered once, from the UNRESOLVED template, and survives however many resolution
// passes follow (this is why cid/cid8/correlation_id are NOT pre-resolved into the record text the
// way they are for every other amqp-step field: doing so here would have destroyed exactly the
// information this comment describes).
//
// resolveRecordForRun is the ONE run-time pass: it walks that tree and, for EVERY leaf —
// Placeholder or plain string — resolves ${cid}/${cid8}/${correlation_id}/${saved.<var>}/${uuid}/
// ${now}/${now_ms}/${VAR} (env), refusing a secret-looking ${VAR} BY NAME (second door — the first
// is amqpStepSpec's authoring-time scan of the pristine record, before this ever runs) without
// reading its value. A Placeholder leaf's RESOLVED text is re-wrapped as a Placeholder so
// avroschema.BuildNative(schema, tree, avroschema.Runtime) still parses it into the field's real
// type (design §4's typing rule); a plain leaf's resolved text stays a plain string (substring
// substitution only — never typed, design: "a placeholder inside a longer string stays text").

// recordVarRe matches an env-style `${NAME}` token — the same shape ${uuid}/${now}/${now_ms}/${VAR}
// all take. It is checked AFTER the more specific forms (saved./cid/cid8/correlation_id) so it only
// ever fires on a genuine env reference.
var recordVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// recordSecretVarRe (design §6) is the credential-shaped NAME a record's ${VAR} may never carry —
// duplicated from scenario.SecretLikeRecordVars's pattern (internal/scenario may not be imported
// here without risking an import cycle back through internal/argus's use of internal/chain; this is
// the SAME literal pattern, not a redefinition of the rule).
var recordSecretVarRe = regexp.MustCompile(`(?i)(token|secret|password|passwd|key|credential|auth)`)

// recordRunCtx carries everything resolveRecordForRun needs that is NOT already in the record tree
// itself: the run's ids, the vars a chain has captured so far (${saved.<var>}), and freshNow — the
// ONE instant every ${now}/${now_ms} in this record resolves to (design §4: "read once per step").
type recordRunCtx struct {
	corr    string
	cid8    string
	vars    map[string]string
	uuidGen func() string
	now     time.Time
}

// resolveRecordForRun is AMQPStep.Run's second stage for a schema+record publish (or the envelope
// preset, which is the same path over a synthesized default record, chain_scenario.go). path is
// threaded through purely for its error messages (design: "refused by field path").
func resolveRecordForRun(v any, path string, ctx *recordRunCtx) (any, error) {
	switch t := v.(type) {
	case string:
		resolved, err := substituteRecordText(t, path, ctx)
		if err != nil {
			return nil, err
		}
		return resolved, nil
	case avroPlaceholderCarrier:
		token := t.Token()
		resolved, err := resolveRecordToken(token, path, ctx)
		if err != nil {
			return nil, err
		}
		return avroschema.Placeholder(resolved), nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			nv, err := resolveRecordForRun(val, joinRecordPath(path, k), ctx)
			if err != nil {
				return nil, err
			}
			out[k] = nv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			nv, err := resolveRecordForRun(val, fmt.Sprintf("%s.%d", path, i), ctx)
			if err != nil {
				return nil, err
			}
			out[i] = nv
		}
		return out, nil
	default:
		return v, nil
	}
}

// avroPlaceholderCarrier lets resolveRecordForRun recognise an avroschema.Placeholder leaf without
// internal/avroschema exporting its concrete type — Placeholder values satisfy this via their
// String() method (avroschema.placeholder is a defined string type; see below).
type avroPlaceholderCarrier interface{ Token() string }

func joinRecordPath(base, seg string) string {
	if base == "" {
		return seg
	}
	return base + "." + seg
}

// resolveRecordToken resolves ONE whole-value `${…}` token (a Placeholder leaf) to its final text,
// which BuildNative will then parse into the field's real type.
func resolveRecordToken(token, path string, ctx *recordRunCtx) (string, error) {
	switch token {
	case "${cid}", "${correlation_id}":
		return ctx.corr, nil
	case "${cid8}":
		return ctx.cid8, nil
	case "${uuid}":
		return ctx.uuidGen(), nil
	case "${now}":
		return ctx.now.Format("2006-01-02T15:04:05.000Z07:00"), nil
	case "${now_ms}":
		return strconv.FormatInt(ctx.now.UnixMilli(), 10), nil
	}
	if name, ok := savedVarName(token); ok {
		v, ok := ctx.vars[name]
		if !ok {
			return "", fmt.Errorf("record.%s: unresolved ${saved.%s} — no earlier step saved %q", path, name, name)
		}
		return v, nil
	}
	if m := recordVarRe.FindStringSubmatch(token); m != nil {
		name := m[1]
		if recordSecretVarRe.MatchString(name) {
			return "", fmt.Errorf("record.%s: references ${%s}, which looks like a credential — a record may never carry a secret", path, name)
		}
		v, ok := lookupEnv(name)
		if !ok {
			return "", fmt.Errorf("record.%s: unresolved ${%s} — environment variable not set", path, name)
		}
		return v, nil
	}
	return "", fmt.Errorf("record.%s: %q is not a placeholder this record understands", path, token)
}

// substituteRecordText resolves every occurrence of every placeholder kind INSIDE a plain (not
// whole-value) string leaf — text substitution only, never typed (design: "stays text").
func substituteRecordText(s, path string, ctx *recordRunCtx) (string, error) {
	s = strings.ReplaceAll(s, "${cid}", ctx.corr)
	s = strings.ReplaceAll(s, "${correlation_id}", ctx.corr)
	s = strings.ReplaceAll(s, "${cid8}", ctx.cid8)
	s = uuidTokenRe.ReplaceAllStringFunc(s, func(string) string { return ctx.uuidGen() })
	s = strings.ReplaceAll(s, "${now_ms}", strconv.FormatInt(ctx.now.UnixMilli(), 10))
	s = strings.ReplaceAll(s, "${now}", ctx.now.Format("2006-01-02T15:04:05.000Z07:00"))
	bound, err := bindSaved(s, ctx.vars)
	if err != nil {
		return "", fmt.Errorf("record.%s: %w", path, err)
	}
	s = bound
	var outerErr error
	s = recordVarRe.ReplaceAllStringFunc(s, func(m string) string {
		if outerErr != nil {
			return m
		}
		name := recordVarRe.FindStringSubmatch(m)[1]
		if recordSecretVarRe.MatchString(name) {
			outerErr = fmt.Errorf("record.%s: references ${%s}, which looks like a credential — a record may never carry a secret", path, name)
			return m
		}
		v, ok := lookupEnv(name)
		if !ok {
			outerErr = fmt.Errorf("record.%s: unresolved ${%s} — environment variable not set", path, name)
			return m
		}
		return v
	})
	if outerErr != nil {
		return "", outerErr
	}
	return s, nil
}

var uuidTokenRe = regexp.MustCompile(`\$\{uuid\}`)

var savedTokenRe = regexp.MustCompile(`^\$\{saved\.([A-Za-z0-9_-]+)\}$`)

func savedVarName(token string) (string, bool) {
	m := savedTokenRe.FindStringSubmatch(token)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// newUUID is a package var so tests can pin it (design §9: "two runs of the same step produce
// different ${uuid} messageIds" — production never overrides it).
var newUUID = func() string { return uuid.NewString() }

// cid8 derives ${cid8} from a run's correlation id — the SAME algorithm
// internal/argus/correlationid.go's cid8() uses (sha256, first 4 bytes as 8 lowercase hex chars),
// duplicated rather than imported: internal/chain may not depend on internal/argus (argus depends
// on chain, not the reverse), and a record's ${cid8} must resolve to the IDENTICAL value every
// other amqp-step field's ${cid8} already does.
// buildRecordForRun is the whole run-time pass over a schema+record publish's (or the envelope
// preset's) record template: resolve every placeholder (this file), then build+type-check the
// avro-native tree against the schema (internal/avroschema), which is what avro.Marshal takes.
// Returns a *avroschema.FieldError (as error) naming the field path on a schema mismatch — design
// §2's "refused by field path", the run-time half of the two-times check (the authoring half is
// amqpStepSpec, chain_scenario.go, on the PRISTINE template before any of this runs).
func buildRecordForRun(schema *avroschema.Schema, template any, corr string, vars map[string]string) (any, error) {
	ctx := &recordRunCtx{corr: corr, cid8: recordCid8(corr), vars: vars, uuidGen: newUUID, now: time.Now().UTC()}
	resolved, err := resolveRecordForRun(template, "", ctx)
	if err != nil {
		return nil, err
	}
	native, ferr := avroschema.BuildNative(schema, resolved, avroschema.Runtime)
	if ferr != nil {
		return nil, ferr
	}
	return native, nil
}

func recordCid8(corr string) string {
	sum := sha256.Sum256([]byte(corr))
	return hex.EncodeToString(sum[:4])
}
