// Package argus is the runner-core's execution layer (NFR-4: the ONLY place that
// drives the JMeter engine). It translates a scenario into JMeter -J props
// (the glue argus run.sh never had — see BLOCKERS.md), drives acme-jmeter:5.6.3a
// directly via a Runner, and parses the .jtl into the normalized report.
//
// The runner GENERATES a correlation_id per scenario and threads it as
// -Jcorrelation.id (the argus templates send it as X-Correlation-Id) — this is
// how the report/saga/log/dashboard all link by correlation_id.
package argus

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/httpload"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
	"github.com/OneDro1d/argus-runner/internal/ui"
)

// layerDir maps a Metadata.Layer string to its scenarios/ subdir + template base.
var layerDir = map[string]string{
	"HTTP Ingestion":       "http-ingestion",
	"Message Flow":         "message-flow",
	"Database State":       "database-state",
	"External Delivery":    "external-delivery",
	"Error Path":           "error-path",
	"Rate Limiting":        "rate-limiting",
	"Permissions":          "permissions",
	"Web UI":               "web-ui",    // D4: ui-tagged scenarios route to the vendored Playwright path (no JMX template)
	scenario.AMQPLoadLayer: "amqp-load", //
	scenario.HTTPLoadLayer: "http-load", //
}

var statusRe = regexp.MustCompile(`\b([1-5][0-9][0-9])\b`)

// bodylessMethod marks HTTP verbs that must not carry a request body.
var bodylessMethod = map[string]bool{"GET": true, "HEAD": true, "DELETE": true}

// dedicatedTemplate maps a layer to its own JMX template. Layers absent here are
// status-only HTTP checks (Error Path / Rate Limiting / Permissions) and share the
// generic http-ingestion template — they all reduce to "fire a request, assert a status".
var dedicatedTemplate = map[string]string{
	"HTTP Ingestion":       "http-ingestion",
	"Database State":       "database-state",
	"Message Flow":         "message-flow",
	"External Delivery":    "external-delivery",
	scenario.AMQPLoadLayer: "amqp-load", //
}

// templateBaseFor selects the JMX template base for a scenario's primary layer.
func templateBaseFor(layer string) string {
	if t, ok := dedicatedTemplate[layer]; ok {
		return t
	}
	return "http-ingestion"
}

// RuntimeTemplateBase answers WHICH JMX TEMPLATE WILL RUN this scenario — the question every
// consumer of a template's capabilities has to ask, and the one the runner answers for itself at
// argus.go's dispatch.
//
// ⛔ V30-004 T-A: IT IS NOT THE LAYER. The runner's answer is three steps and the LAST match wins:
//
//  1. the layer's dedicated template (templateBaseFor)
//  2. `http-idempotency` when a second status is declared — and ONLY on HTTP Ingestion. Database
//     State keeps its own template, which sends the second request itself under trigger.second
//     (AC-D30); on any other template a second status is refused when the scenario is written
//     (secondRequestProblems), because nothing there can send it.
//  3. `saga-presence` when the scenario carries the tag
//
// A refusal or a report computed from the layer alone would refuse a legal saga-presence scenario
// (whose layer says database-state while saga-presence.jmx is what runs) and miss an illegal
// idempotency one. ONE helper, in the runner's own order, called by the write-time refusal, by
// tier 3 and by the gate test — so none of them can disagree with the run.
func RuntimeTemplateBase(s *scenario.Scenario) string {
	if s == nil {
		return "http-ingestion"
	}
	layer := PrimaryLayer(s)
	base := templateBaseFor(layer)
	if _, second := scenario.DeclaredStatuses(s.RunnableExpect()); second > 0 && layer == "HTTP Ingestion" {
		base = "http-idempotency"
	}
	if contains(s.Tags, SagaPresenceTag) {
		base = "saga-presence"
	}
	return base
}

// brokerTapLayers are the layers whose template reads a BROKER for content (V30-004). The tap is a
// queue on the SUT's own broker, so it belongs only where a broker is what gets read — a Database
// State scenario has its own JDBC verify and must never declare one.
var brokerTapLayers = []string{"Message Flow"}

const (
	// tapTTLMillis is x-expires AND x-message-ttl on the tap. It is the GUARANTEE the queue goes
	// away: the tearDown delete is the tidy path, and a killed run is reaped by the broker itself.
	tapTTLMillis = 120000
	// tapReadCap bounds one /get. A tap sees every message published to the exchange while it is
	// bound, not only ours, so the read is capped and the evaluator says when it hit the cap —
	// "no message for this id" and "the cap hid it" are different answers.
	tapReadCap = 20
)

// NoRoutingKeyRefusal refuses, before anything is sent, a Message Flow content check whose broker target declares
// no `routing_keys.incoming` (found in the 0.3.32 Release QA). The tap binds with that key; bound with none it sees
// no message, and the run used to say only "the tap returned 0 message(s)" after declaring a queue on the SUT's
// broker for nothing. It names the key and the fix, and never echoes an asserted value (VR-C8).
const NoRoutingKeyRefusal = "this scenario checks what a published message contains, which is read through a tap " +
	"queue bound with its broker target's routing_keys.incoming — the config declares none, so the tap would bind " +
	"nothing and see no message. Add routing_keys: {incoming: <the routing key your service publishes with>} under " +
	"targets.message_broker (or under the message_broker_targets entry this scenario selects)"

// ErrNoRoutingKey is DeriveProps' error for that case. RunAll reports it as `error` — nothing was measured and the gap
// is the config's — never as `failed`, which blames the SUT (the rule stated at the undeclared-status branch of RunAll).
var ErrNoRoutingKey = errors.New(NoRoutingKeyRefusal)

// contentLayers judge by the JMeter success flag (their JMX embeds the content
// assertion), not by HTTP response code.
//
// ⛔ ALIASED, NOT COPIED (V29-016). internal/scenario owns the dispatch answer because tier 2 has
// to ask the same question the runtime asks: keying validation on anything other than what the
// runtime keys on is how this class of defect is created. A second copy here is how a validator
// ends up refusing the 83 scenarios the runner would have run natively.
func contentLayerJudged(layer string) bool { return scenario.IsContentLayer(layer) }

// SagaPresenceTag marks a scenario whose assertion is "a mandated control-action
// saga was emitted for this correlation_id" (VR-F14/F15, exit-bar #5). The HTTP
// trigger alone cannot decide it — the control endpoint returns the same status
// whether or not the saga fired (e.g. under SAGA_SUPPRESS).
//
// Decision-2 (2026-06-19): the saga JUDGMENT runs IN JMeter (the saga-presence.jmx
// template fires the trigger, then a JSR223 step polls Loki and asserts the mandated
// control_action saga is present), so the verdict flows back via the .jtl success
// flag like the content layers — there is no Go-native saga judgment any more. The
// Loki query runs from the in-network JMeter vantage (loki:3100), and fails CLOSED if
// Loki is unreachable (the JSR223 step marks the sample failed).
const SagaPresenceTag = scenario.SagaPresenceTag

// NewCorrelationID returns a fresh "tr-<16hex>" correlation id (the random primitive;
// per-scenario ids use newScenarioCorrelationID which embeds the run + scenario).
func NewCorrelationID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "tr-" + hex.EncodeToString(b[:])
}

// NewRunID returns a run handle: a sortable UTC timestamp to the MILLISECOND, YYYYMMDDThhmmssSSS
// (18 chars). Filename- and regex-safe with no internal '-'/':' — it stays a clean first segment
// inside the correlation id tr-<run_id>-<scenario_id>-<8hex>. Strictly increasing per process, so
// no two mints can collide even inside one millisecond.
func NewRunID() string {
	// MILLISECOND precision, no random suffix (2026-07-22, owner remark "Run Id is too long").
	// R7 (CP-M3-116) appended 6 random hex because a BARE-SECOND stamp let two same-second runs on
	// one instance silently merge in the cloud ledger via ON CONFLICT (instance_id, run_id). That
	// was the right diagnosis but a heavier cure than needed: it made every id 21 chars of
	// half-noise, and the run_id rides inside every correlation id, the Grafana Run-ID variable and
	// the Runs page. Milliseconds give the same uniqueness BY CONSTRUCTION instead of by chance —
	// the W1 fence (instance_run_lock, instance_id PK) serialises runs per instance, so two runs on
	// one instance cannot overlap and no run completes inside a millisecond. 18 chars, reads
	// end-to-end as a timestamp, still lexicographically == chronologically sortable, still
	// dash-free (run_id is the first '-'-delimited segment of tr-<run_id>-<scenario_id>-<8hex>).
	runIDMu.Lock()
	defer runIDMu.Unlock()
	ms := time.Now().UTC().UnixMilli()
	// Strict monotonicity, locally guaranteed: if the clock has not advanced since the last mint
	// (or has gone BACKWARDS — NTP correction), take the next millisecond instead of repeating an
	// id. Without this the uniqueness of a bare millisecond stamp would depend on the W1 fence in
	// the control plane, i.e. on a guarantee living in another component — and any future caller
	// minting twice inside a millisecond would silently resurrect the same-instant ledger merge
	// that the random suffix was introduced to fix. This keeps the id 18 chars and deterministic.
	if ms <= lastRunIDMs {
		ms = lastRunIDMs + 1
	}
	lastRunIDMs = ms
	t := time.UnixMilli(ms).UTC()
	return t.Format("20060102T150405") + fmt.Sprintf("%03d", t.Nanosecond()/int(time.Millisecond))
}

var (
	runIDMu     sync.Mutex
	lastRunIDMs int64
)

// newScenarioCorrelationID mints the per-scenario correlation id embedding the run + scenario,
// so Loki can be sliced by run and by scenario via a regex on the id — while the id stays the
// SINGLE field propagated to the SUT and is NEVER a Prometheus label (NFR-3). Format:
// tr-<run_id>-<scenario_id>-<8hex>. The 8hex is a per-scenario tiebreaker; run_id+scenario_id
// already identify the scenario uniquely within a run.
func newScenarioCorrelationID(runID, scenarioID string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "tr-" + runID + "-" + scenarioID + "-" + hex.EncodeToString(b[:])
}

// extractDeclaredStatuses reads the declared status forms (VR12-E7) from the RUNNABLE bullets.
//
// ⛔ IT IS A DELEGATION, NOT A COPY. The grammar moved to internal/scenario so that VR12-E6's
// validator reads a declaration with the EXACT reader the runtime uses — see scenario/status.go.
func extractDeclaredStatuses(runnable []string) (first, second int) {
	return scenario.DeclaredStatuses(runnable)
}

// DeriveProps maps a scenario + config into the JMeter -J property set for its
// primary layer. corr is the generated correlation id.
//
// VR10-S3: a scenario's **Target** word selects a NAMED entry of its layer's kind (http host/port,
// db.* or mgmt.*/mq.* come from that entry); a missing or wrong-kind name is an error, and the
// props are NOT built from the plain slot instead — no fallback, ever.
func DeriveProps(c *config.Config, s *scenario.Scenario, corr string) (map[string]string, error) {
	sel, err := c.SelectTarget(s)
	if err != nil {
		return nil, err
	}
	httpT := c.Targets.HTTP
	if sel != nil && sel.Kind == config.KindHTTP {
		httpT = sel.HTTP
	}
	proto, host, port := httpT.HostPort()
	method := orDefault(s.Trigger.Method, "POST")
	// Bodyless verbs send no request body (JMeter rejects a raw body on GET; health/
	// readiness/discovery endpoints are GET). Body-bearing verbs default to "{}".
	payload := orDefault(s.Trigger.Payload, "{}")
	if bodylessMethod[strings.ToUpper(method)] {
		payload = ""
	}
	props := map[string]string{
		"scenario.id":    s.ID,
		"http.protocol":  proto,
		"http.host":      host,
		"http.port":      port,
		"correlation.id": corr,
		"trigger.method": method,
		// V31-003: the request itself carried the placeholder literally — a scenario could not even
		// ADDRESS a run-scoped resource. PathFromURL runs first, so its leading-${VAR} base-URL strip
		// is unchanged. ⛔ The correlation id ONLY: the JMeter request never received an environment
		// value and this does not add one (owner, item b).
		"trigger.path": fillCorrelationID(config.PathFromURL(s.Trigger.URL), corr),
		// SY-8: resolveVars is fillCorrelationID PLUS the environment ${VAR} form, so a
		// provisioning body can name its own environment; an unset ${VAR} stays literal so
		// preflight can name it (UC-82). The Authorization override below already uses it.
		"trigger.payload": resolveVars(payload, corr),
	}
	// VR12-TO-RUN (per-request): the scenario's own declared `## TIMEOUT` bounds EACH request the
	// SUT receives, inside JMeter itself — never the whole process (that is jmeterProcessBackstop's
	// job). Every HTTPSamplerProxy in templates/*.jmx reads trigger.timeout_ms via
	// `${__P(trigger.timeout_ms,)}` for HTTPSampler.connect_timeout/response_timeout (JMeter's own
	// units, ms); database-state.jmx's JDBCSampler reads trigger.timeout_s for queryTimeout (JDBC's
	// unit is seconds, matching cleanup-sql.jmx's existing cleanup.timeout.s convention). Rounded UP
	// so a sub-second declared TIMEOUT (`500ms` is legal, sections.go's timeoutRe) never becomes a
	// JDBC queryTimeout of "0" — which JDBC reads as "no timeout", the opposite of what was declared.
	props["trigger.timeout_ms"] = strconv.FormatInt(s.TimeoutDuration().Milliseconds(), 10)
	props["trigger.timeout_s"] = strconv.FormatInt(ceilSeconds(s.TimeoutDuration()), 10)
	// AC-11 (mode 2 — "under load"): a declared `## LOAD` profile drives the thread group beyond
	// the product's one-thread/one-loop default. Set ONLY when the scenario carries one, so a
	// scenario without a profile emits NOTHING here and the template's own `${__P(load.users,1)}`
	// default applies — byte-identical to today.
	if s.Load != nil {
		props["load.users"] = strconv.Itoa(s.Load.Users)
		props["load.ramp"] = strconv.Itoa(s.Load.RampSeconds)
		// a load user is a persistent client. Without this each loop
		// sent `Connection: close` and opened a new TCP (+TLS) connection, which used the node's outbound
		// ports up. Set ONLY with a profile: a check without one keeps its wire as it was.
		props["http.keepalive"] = "true"
		if s.Load.DurationSeconds > 0 {
			// scheduler=true + duration=N is what makes JMeter stop on ELAPSED TIME rather than on
			// the loop count; load.loops=-1 (below) is what lets the load run that long.
			props["load.scheduler"] = "true"
			props["load.duration"] = strconv.Itoa(s.Load.DurationSeconds)
			// with the loop count left at 1 each thread leaves after ONE pass whatever the
			// scheduler says, so the duration could not sustain anything. loops=-1 lets the scheduler be
			// what ends the run. (The templates ALSO had to read the scheduler as a stringProp: JMeter
			// evaluates no function inside a boolProp, so it was never on.) Absent = the template's one pass.
			props["load.loops"] = "-1"
		}
	}
	// VR12-E7: the DECLARED forms, read from the RUNNABLE bullets only. A status mentioned in
	// `### Non-runnable` prose is documentation and never reaches the runner.
	// ⛔ V29-016, SITE 1 OF TWO. `props["expect.status"] = "202"` used to stand here for every
	// scenario that declared nothing. `202` appeared nowhere in the scenario file; the real response
	// was 200; the scenario went red and the report blamed the body assertion that had PASSED. Two
	// operators spent two days on it.
	//
	// There is no default. A scenario that declares no status is refused at authoring time
	// (VR12-E6) and, if it predates the rule, is reported rather than guessed at — never judged
	// against a number nobody wrote.
	status1, status2 := extractDeclaredStatuses(s.RunnableExpect())
	if status1 > 0 {
		props["expect.status"] = strconv.Itoa(status1)
	}
	// VR12-E8 (V29-017 R1): EVERY body assertion is wired, as NUMBERED properties, and the template
	// ANDs them. The old code set one scalar `expect.body_contains` and one `expect.body_matches`,
	// so a scenario declaring two body bullets had exactly one of them enforced and no warning.
	//
	// Parse errors are NOT swallowed here: on the author path R3 refuses them, and anything that
	// still reaches a run is named by tier 3 (VR12-E13). This function's job is only to wire what
	// parsed.
	if asserts, _ := ParseBodyAsserts(s.RunnableExpect()); len(asserts) > 0 {
		props["expect.body.count"] = strconv.Itoa(len(asserts))
		for i, a := range asserts {
			n := strconv.Itoa(i + 1)
			props["expect.body."+n+".field"] = a.Field // "" = the whole response
			props["expect.body."+n+".op"] = a.Op
			props["expect.body."+n+".value"] = checkValueWithCorrelationID(a.Op, a.Value, corr)
		}
		// ⚠ TRANSITIONAL, DELIBERATE: the legacy scalars are still set from the FIRST assertion of
		// each kind. An executor still running a pre-0.3.31 image reads only these, and without them
		// it would enforce NOTHING at all — a silent regression strictly worse than the defect being
		// fixed. With them, a stale image degrades to exactly today's behaviour (the first bullet)
		// while a current image enforces all of them.
		// ⛔ REMOVE once every executor in the estate is on 0.3.31 or later.
		for _, a := range asserts {
			if a.Op == BodyContains && props["expect.body_contains"] == "" {
				props["expect.body_contains"] = checkValueWithCorrelationID(a.Op, a.Value, corr)
			}
			if a.Op == BodyMatches && props["expect.body_matches"] == "" {
				props["expect.body_matches"] = checkValueWithCorrelationID(a.Op, a.Value, corr)
			}
		}
	}
	// VR12-T3 (V29-018): EVERY author-declared header is propagated, as a numbered set mirroring
	// VR12-E8's body set so the runner has ONE convention. A six-template JSR223 PreProcessor
	// applies them to the sampler (XML cannot render a variable count).
	//
	// ⚠ `Authorization` is NOT in this set — it keeps its own `auth.header` path, including the 4.8
	// present-but-empty = send-no-token override that permission scenarios rely on. Two mechanisms
	// for one header would be two answers to one question.
	// ⚠ A scenario with no author headers emits nothing here, so its bytes on the wire are
	// IDENTICAL to today.
	for k, v := range scenario.HeaderProps(s) {
		props[k] = resolveVars(v, corr)
	}
	// VR12-E7: a SECOND request is sent ONLY when `status2=` is DECLARED. It used to fire whenever a
	// second 3-digit number turned up anywhere in EXPECT, which is why PERM-001's three prose
	// variants of one unauthorized request became a two-request idempotency test nobody asked for.
	if status2 > 0 {
		props["expect.status2"] = strconv.Itoa(status2)
		props["idempotency.key"] = corr
		// AC-D30: and ASK for the second request. http-idempotency.jmx always sends two; database-state.jmx
		// sends its `<id>-trigger2` only under this flag, so its JDBC verify runs after the duplicate and a
		// row check is judged on the real double submit. Every other template sends one request, and a
		// second status on it is refused when the scenario is written (secondRequestProblems).
		props["trigger.second"] = "true"
	}
	// auth header (config-driven; generic). Empty => template sends no usable token.
	if c.Targets.Auth != nil && c.Targets.Auth.BearerToken != "" {
		// V31-003 (the owner's "yes"): the config token gets the correlation id — and ONLY that. Its
		// ${VAR} was already expanded when the config was loaded (config.go:746-752).
		props["auth.header"] = "Bearer " + fillCorrelationID(c.Targets.Auth.BearerToken, corr)
	}
	// 4.8: a scenario can OVERRIDE the auth header from its TRIGGER (a wrong/absent token for a
	// permissions test). Presence of the Authorization header wins over the config default — an
	// empty value means "send no token" — so PERM-style scenarios actually exercise auth instead
	// of silently riding the default valid token (the false "auth bypass").
	// The override is resolved like every other author header (headers.go): a two-identity SUT
	// writes `Authorization: Bearer ${OPERATOR_TOKEN}` on the scenarios that need the second identity
	// (SY-8). Present-but-empty stays empty (send no token), and an unset ${VAR} stays literal.
	if v, ok := s.Trigger.Headers["Authorization"]; ok {
		// ⛔ V31-003: this line sent the value RAW — alone among the headers, which all go through
		// resolveVars. Not ${cid}, not ${correlation_id}, not an environment ${VAR}. It now gets the
		// same fill as every other header; an empty value still means "send no token" (the 4.8
		// override below), and an unset ${VAR} still stays literal.
		//
		// item 25 — the basic-auth helper (`Basic ${basic_auth:<user>:<PASSWORD_ENV_VAR>}`) is
		// resolved HERE, at run time, before the ordinary ${...} resolver ever sees it: resolveVars's
		// ${VAR} grammar cannot match the marker anyway (its colons are not identifier characters),
		// but resolving explicitly is what makes the guarantee obvious rather than incidental. The
		// password is read from the named env var and never printed — `auth.header` is already one of
		// secret_props.go's redacted names, so it never reaches JMeter's own run log either.
		if user, passEnvVar, ok := scenario.BasicAuthMarker(v); ok {
			resolved, berr := scenario.ResolveBasicAuthHeader(user, passEnvVar)
			if berr != nil {
				return nil, berr
			}
			props["auth.header"] = resolved
		} else {
			props["auth.header"] = resolveVars(v, corr)
		}
	}
	// database-state (JDBC) props + the verify query (correlation placeholder filled).
	db := c.Targets.Database
	if sel != nil && sel.Kind == config.KindDatabase {
		db = sel.DB
	}
	if db != nil && db.JDBCURL != "" {
		props["db.url"] = db.JDBCURL
		props["db.user"] = db.Username
		props["db.password"] = db.Password
	}
	if s.Verify.SQL != "" {
		props["db.query"] = strings.ReplaceAll(s.Verify.SQL, "${correlation_id}", corr)
	}
	// message-flow (RabbitMQ management API) props: host/port + basic auth + the queue to check.
	mb := c.Targets.MessageBroker
	if sel != nil && sel.Kind == config.KindMessageBroker {
		mb = sel.MQ
	}
	if mb != nil {
		if mb.ManagementURL != "" {
			if u, err := url.Parse(mb.ManagementURL); err == nil {
				props["mgmt.host"] = u.Hostname()
				if u.Port() != "" {
					props["mgmt.port"] = u.Port()
				} else {
					props["mgmt.port"] = "15672"
				}
			}
		}
		if u, err := url.Parse(mb.URL); err == nil && u.User != nil {
			pw, _ := u.User.Password()
			props["mgmt.auth.header"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw))
		}
		if q, ok := mb.Queues["incoming"]; ok {
			props["mq.queue"] = q
		}
		// ⭐ V30-004 F-3/F-4 — THE TAP, AND THE NEGATIVE CHECK. Exactly one of the two is armed, and
		// only when the scenario asserts something a template can evaluate.
		//
		// ⛔ THE GATE IS NOT "columns or row_count is present". ParseDBExpect sets RowCount = 0 and
		// Columns = {} when has_rows is FALSE (dbexpect.go:193-198), so that gate would tap on
		// precisely the `- no rows` case F-4 says must never tap — and a tap bound before the trigger
		// would hold the very message the scenario asserts was never published.
		if contains(brokerTapLayers, PrimaryLayer(s)) {
			dbx := scenario.ParseDBExpect(s.RunnableExpect())
			switch {
			case !dbx.HasRows:
				// the negative case: no tap, and the SUT's own queue is read before and after.
				if q, ok := mb.Queues["incoming"]; ok {
					props["mq.negative.queue"] = q
				}
			case len(dbx.Columns) > 0 || dbx.RowCount >= 0:
				// per RUN, and prefixed so a SUT's broker grant can be written against
				// `argus-tap-*` rather than a queue name nobody can predict.
				props["mq.tap.queue"] = "argus-tap-" + corr
				props["mq.tap.ttl_ms"] = strconv.Itoa(tapTTLMillis)
				props["mq.tap.read_cap"] = strconv.Itoa(tapReadCap)
				if e, ok := mb.Exchanges["incoming"]; ok {
					props["mq.exchange"] = e
				}
				// ⛔ NO KEY, NO TAP: a tap bound with an empty routing key binds nothing, so the check could only
				// report "0 message(s)". The absence is known here, before anything is sent — refused by name.
				rk := strings.TrimSpace(mb.RoutingKeys["incoming"])
				if rk == "" {
					return nil, ErrNoRoutingKey
				}
				props["mq.routing_key"] = rk
			}
		}
		// AC-D16 — EXPECTED REFUSAL. A `- broker refuses with <code>` bullet is a content check
		// like the tap's columns/row_count, so it rides the same expect.* family (TemplateReads,
		// enforced.go) rather than a request-shaping mq.* name. It is independent of the dbx
		// switch above: a refusal bullet sets none of HasRows/Columns/RowCount, so without this it
		// would arm neither the tap nor the negative check and publish to nowhere.
		if contains(brokerTapLayers, PrimaryLayer(s)) {
			if code, ok := scenario.ParseRefusal(s.RunnableExpect()); ok {
				props["expect.refusal_code"] = strconv.Itoa(code)
				// the SAME message-broker target already selected above (VR10-S3) — never a second
				// credential. uri/username/password are split out of mb.URL exactly the way
				// mgmt.auth.header already is, rather than carried inline in one string.
				if u, err := url.Parse(mb.URL); err == nil {
					// the same Java setUri as AMQP Load — the vhost is normalised so a
					// trailing "/" does not become the EMPTY vhost (530 before any refusal can be seen).
					props["mq.amqp.uri"] = javaSamplerURI(u, mb.URL)
					if u.User != nil {
						props["mq.amqp.username"] = u.User.Username()
						if pw, set := u.User.Password(); set {
							props["mq.amqp.password"] = pw
						}
					}
				}
				if e, ok := mb.Exchanges["incoming"]; ok {
					props["mq.exchange"] = e
				}
				// ⚠ UNLIKE THE TAP: an empty routing key does not silently defeat this check the way
				// it defeats a tap bind (tap_routing_key_test.go) — a broker refusal (ACL /
				// precondition) is decided at the channel/exchange level, before routing is ever
				// considered, so this is best-effort only and never refuses the run.
				if rk := strings.TrimSpace(mb.RoutingKeys["incoming"]); rk != "" {
					props["mq.routing_key"] = rk
				}
				// the TRIGGER may declare the AMQP property needed to provoke the refusal
				// ("User-Id: guest") — read like every other author header (VR12-T3), but excluded
				// from AuthorHeaders (headers.go) so it is never ALSO sent as a literal HTTP header.
				if uid, ok := s.Trigger.Headers["User-Id"]; ok {
					props["mq.user_id"] = uid
				}
			}
		}
	}
	// saga-presence (Decision-2): the saga-presence.jmx JSR223 step queries Loki from
	// the in-network JMeter vantage (service name, NOT the host's localhost:3100 the CLI
	// uses for get-sagas/tail-logs). control_action is the only saga step M2 mandates.
	if contains(s.Tags, SagaPresenceTag) {
		props["loki.url"] = "http://loki:3100"
		props["loki.expected_saga"] = "control_action"
		// #419: the template judges ONE parsed line with the SUT's DECLARED names, resolved by the
		// Go reader's own accessors (obsquery.SagaJudgeFields) — the defaults/fallbacks live there
		// and nowhere else. Lists travel as JSON arrays so a value may contain any character.
		lk := &obsquery.Loki{
			CorrelationField: c.CorrelationField(),
			SagaEventField:   c.SagaEventField(),
			SagaEventValue:   c.SagaEventValue(),
			SagaEventValues:  c.SagaEventValues(),
			SagaStepFields:   c.SagaStepFields(),
			LogFormat:        c.LogFormat(), // #422: this Loki only resolves names, but carries the declaration too
		}
		jf := lk.SagaJudgeFields()
		props["loki.corr_field"] = jf.CorrelationField
		props["loki.saga_field"] = jf.SagaField
		props["loki.saga_values"] = jsonStrings(jf.SagaValues)
		props["loki.step_fields"] = jsonStrings(jf.StepNameFields)
		props["loki.log_format"] = c.LogFormat()
	}
	// FINDING-7: structured EXPECT for content layers so the template EVALUATES the
	// scenario's column/count/negative assertions (not just a correlation_id substring).
	if contentLayerJudged(PrimaryLayer(s)) {
		// VR12-E9: runnable bullets only — a sentence containing "rejected" must not invert this.
		for k, v := range scenario.ParseDBExpect(s.RunnableExpect()).Props() {
			// V31-003: a column check naming the run's own id. Message Flow and External Delivery
			// compute this too; it reaches a template the day V30-004 makes one read the property.
			props[k] = fillCorrelationID(v, corr)
		}
	}
	return props, nil
}

// targetRefused is the preflight result for a scenario whose **Target** word names nothing usable
// (VR10-S3-4/5): failed, by name, before anything is fired — and never re-routed to the plain slot.
func targetRefused(s *scenario.Scenario, corr string, why error) report.ScenarioResult {
	return report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: "failed",
		Failure: &report.Failure{Observed: why.Error() + " — preflight"}}
}

// jsonStrings encodes a list as a JSON array for a JMeter property (saga-presence.jmx parses it).
func jsonStrings(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// Runner executes one JMeter test plan with -J props, writing a .jtl. Abstracted
// so the orchestration is unit-testable (fake) and live-runnable (DockerRunner).
//
// ⛔ REVISED (P1 #12 follow-up): Run's timeout is NOT the scenario's declared `## TIMEOUT` anymore —
// it is jmeterProcessBackstop's GENEROUS, process-level value. The declared TIMEOUT itself now
// bounds EACH REQUEST inside JMeter (trigger.timeout_ms/trigger.timeout_s, DeriveProps + every
// HTTPSamplerProxy/JDBCSampler in templates/*.jmx). The first cut of this (3d91fd0) made Run's
// timeout parameter the raw scenario TimeoutDuration(), which is a HARD DEADLINE on the WHOLE
// JMeter process — including JVM start-up (seconds, more on a CPU-limited executor pod) and every
// sampler a content template fires (trigger, trigger2, JDBC verify). A 10–15s TIMEOUT (every live
// check in production) killed a healthy run before it could finish, and a `## LOAD` scenario's
// thread group runs for ramp+duration BY DESIGN (up to 86400s) while TIMEOUT is capped at 30–120s
// — every load run would have been killed. The runner still needs A deadline (a hung sampler must
// not block forever), so Run keeps taking one — it is just no longer the tight, per-request number.
type Runner interface {
	Run(templateBase, jtlPath string, props map[string]string, timeout time.Duration) error
}

// jmeterProcessGrace is the JVM start-up allowance folded into every process backstop — a
// CPU-limited executor pod can take several seconds just to start the JVM, before any sampler
// fires, and that time is NOT part of any sampler's own trigger.timeout_ms/trigger.timeout_s.
// "at least 60s" per the design note; chosen as a fixed floor rather than measured per-pod, because
// a backstop that occasionally fires on a healthy-but-slow-starting JVM is exactly the defect this
// whole change removes.
const jmeterProcessGrace = 60 * time.Second

// templateSamplerCount is the STATIC number of HTTPSamplerProxy + JDBCSampler elements each JMX
// template carries (counted directly off templates/*.jmx). Used ONLY to size jmeterProcessBackstop
// — never to judge a result. A template's own CONDITIONAL samplers (message-flow.jmx's negative-
// case pair, gated on mq.negative.queue; only fired when the scenario declares `expect.has_rows:
// false`) mean a real run may fire fewer than this — the backstop deliberately counts the WORST
// case rather than what THIS run will actually use: a generous backstop must never be the tight
// number the per-request budget already is.
var templateSamplerCount = map[string]int{
	"http-ingestion":    1, // trigger
	"database-state":    3, // trigger + trigger2 (HTTP) + verify (JDBC)
	"external-delivery": 2, // trigger + verify-delivery (a 15s long-poll, itself inside trigger.timeout_ms)
	"http-idempotency":  2, // -1, -2
	"saga-presence":     1, // trigger
	"message-flow":      8, // trigger + up to 7 RabbitMQ mgmt-API verify/tap samplers (worst case, negative-case included)
	// amqp-load: setup + publish + subscribe. Its backstop is STEP-AWARE (runAMQPLoad: the step's own
	// duration + margin + grace) and never TIMEOUT x n, because a thread loops for the whole step.
	"amqp-load": 3,
}

// jmeterProcessBackstop is VR12-TO-RUN's PROCESS-LEVEL deadline (Runner.Run's timeout argument) —
// a GENEROUS multiple of the scenario's own declared `## TIMEOUT`, meant to catch a hung jmeter
// process (a JVM that never returns at all — the executor itself is broken), never to bound a
// healthy run. Each individual request is already bounded by TIMEOUT itself, inside JMeter
// (DeriveProps' trigger.timeout_ms/trigger.timeout_s + templates/*.jmx); this backstop only has to
// be generous enough that a scenario whose EVERY sampler legitimately takes close to the full
// TIMEOUT (a slow-but-healthy SUT) still completes inside it:
//
//	backstop = TIMEOUT × (this template's sampler count) + (a `## LOAD` profile's own ramp+duration,
//	           which is a SEPARATE wall-clock budget the thread group runs for BY DESIGN, added, not
//	           substituted) + jmeterProcessGrace (JVM start-up, >= 60s, every run)
//
// The multiple is the template's OWN static sampler count (templateSamplerCount), not a fixed
// number for every layer: a status-only scenario (http-ingestion, 1 sampler) needs no more room
// than its own TIMEOUT + grace, while a content scenario chaining several verify samplers
// (message-flow, up to 8) legitimately needs up to 8x that before the LAST one has even started.
func jmeterProcessBackstop(s *scenario.Scenario, base string) time.Duration {
	n := templateSamplerCount[base]
	if n < 1 {
		n = 1
	}
	backstop := s.TimeoutDuration() * time.Duration(n)
	if s.Load != nil {
		backstop += time.Duration(s.Load.RampSeconds)*time.Second + time.Duration(s.Load.DurationSeconds)*time.Second
	}
	return backstop + jmeterProcessGrace
}

// ceilSeconds rounds d UP to whole seconds — never DOWN — so a sub-second declared `## TIMEOUT`
// (`500ms` is legal grammar, sections.go's timeoutRe) never becomes "0" for a unit (JDBCSampler's
// queryTimeout) that reads "0" as "no timeout": the opposite of what was declared.
func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	secs := int64(d / time.Second)
	if d%time.Second != 0 {
		secs++
	}
	return secs
}

// HealthChecker is an OPTIONAL Runner capability (RO-04 runtime preflight): verify the
// executor is reachable ONCE before the run, so a dead rig fails fast as `error` instead
// of N per-scenario `failed`/blind runs. A Runner that doesn't implement it skips preflight.
type HealthChecker interface {
	Healthcheck() error
}

// PrimaryLayer returns the layer that drives execution + template selection.
// Delegates to internal/scenario for the same reason contentLayerJudged does.
func PrimaryLayer(s *scenario.Scenario) string { return scenario.PrimaryLayer(s) }

// scenarioFile is a parsed scenario + its source path.
type scenarioFile struct {
	s    *scenario.Scenario
	path string
	// memberTargetRefusal is set by applyCompareTarget (mode compare, a member target, no **Target** of the check's own)
	// when the check's layer has no named-target form: runOneScenario then refuses it before anything is fired.
	memberTargetRefusal string
}

// memberTargetKindName names the kind of a check for the member-target refusal: "chain", or the check's terminal layer.
func memberTargetKindName(s *scenario.Scenario) string {
	if contains(s.Tags, scenario.ChainTag) {
		return "chain"
	}
	if contains(s.Tags, "ui") {
		return "Web UI"
	}
	if len(s.Layers) > 0 {
		return s.Layers[len(s.Layers)-1]
	}
	return "layerless"
}

// collectScenarios discovers scenariosDir's *.md via the shared scenario.DiscoverFiles
// (skips dot-dirs + placeholder IDs — the M25 phantom never runs), optionally filtered by
// layer/tag/id. DiscoverFiles returns ID-sorted, so the run order stays deterministic.
func collectScenarios(dir, layerFilter, tagFilter, idFilter string) ([]scenarioFile, error) {
	var out []scenarioFile
	for _, d := range scenario.DiscoverFiles(dir) {
		s := d.Scenario
		if idFilter != "" && s.ID != idFilter {
			continue
		}
		if layerFilter != "" && PrimaryLayer(s) != layerFilter {
			continue
		}
		if tagFilter != "" && !contains(s.Tags, tagFilter) {
			continue
		}
		out = append(out, scenarioFile{s: s, path: d.Path})
	}
	return out, nil
}

func contains(set []string, v string) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

// applyCompareTarget is the target of a comparison member (ARGUS-CMP-11, design 4.4): in mode `compare` only, every check
// that carries no **Target** of its own is run against the named connection target `target`, selected by the very selector
// the per-scenario word uses (config.SelectTarget): a check that declares one keeps it, and a check whose layer has no named
// form (a chain, which selects per step, a Web UI check, External Delivery) is REFUSED before firing (fix F1, memberTargetRefusal:
// status error, nothing fired, nothing recorded; it used to keep its own wiring and read as the member's). A name the environment does
// not declare under any kind is refused for the whole run, by name; it never falls back to the default target. A name that
// exists under another kind than a check's layer needs is refused for THAT check by the same selector ("the word means one
// thing per layer"), also never replaced by the plain slot.
//
// The scenarios are copied, never edited in place: the parsed files are the run's own, but nothing else may see a Target
// the author did not write.
func applyCompareTarget(c *config.Config, scns []scenarioFile, mode, target string) ([]scenarioFile, string) {
	if mode != report.ModeCompare || target == "" {
		return scns, ""
	}
	if !c.DeclaresNamedTarget(target) {
		var declared []string
		for _, kind := range []string{config.KindHTTP, config.KindMCP, config.KindDatabase, config.KindMessageBroker} {
			declared = append(declared, c.NamedTargets(kind)...)
		}
		if len(declared) == 0 {
			return scns, fmt.Sprintf("this environment declares no connection target named %q (it declares no named targets: add targets.<kind>_targets.%s to argus-config.yaml)", target, target)
		}
		return scns, fmt.Sprintf("this environment declares no connection target named %q (declared: %s)", target, strings.Join(declared, ", "))
	}
	out := make([]scenarioFile, len(scns))
	copy(out, scns)
	for i := range out {
		if out[i].s.Target != "" {
			continue
		}
		cp := *out[i].s
		cp.Target = target
		if _, err := scenario.TargetKind(&cp); err != nil {
			// ARGUS-CMP-11 fix F1: no named form for this kind of check. It is NOT run with its own wiring (that
			// sent a chain's requests to the steps' own urls while the member claimed to be `target`): it is
			// refused before firing, at the door in runOneScenario.
			out[i].memberTargetRefusal = fmt.Sprintf("this comparison member names the connection target %q, and a %s check cannot be pointed at a named target; give the check its own target or compare this system as its own instance", target, memberTargetKindName(out[i].s))
			continue
		}
		out[i].s = &cp
	}
	return out, ""
}

// RunResult is the report + the scenario_id->correlation_id map (for get-sagas etc.).
type RunResult struct {
	Report        *report.Report
	CorrelationID map[string]string
}

// RunAll executes all matching scenarios via the Runner and builds the report.
// resultsDir receives one .jtl per scenario. project labels the report.
// saga-presence scenarios are judged IN JMeter (saga-presence.jmx fires the trigger
// then polls Loki and asserts the mandated saga) and reported via the .jtl success
// flag, same as the content layers — there is no Go-native saga probe.
func RunAll(c *config.Config, scenariosDir, resultsDir, project, layerFilter, tagFilter, idFilter, runID string, r Runner) (*RunResult, error) {
	return RunAllWithEvidence(c, scenariosDir, resultsDir, project, layerFilter, tagFilter, idFilter, runID, r, nil)
}

// RunAllWithEvidence is RunAll plus the spec 26 A1 sandbox evidence reader (observe only). ev is used
// only when argus-config declares observability.openshell; with the block declared and ev nil, every
// row says no reader was wired. toolcore.Run is the caller that wires one (evidenceFor).
func RunAllWithEvidence(c *config.Config, scenariosDir, resultsDir, project, layerFilter, tagFilter, idFilter, runID string, r Runner, ev obsquery.SandboxEvidence) (*RunResult, error) {
	return RunAllWithMode(c, scenariosDir, resultsDir, project, layerFilter, tagFilter, idFilter, runID, r, ev, "")
}

// RunAllWithMode is RunAllWithEvidence plus the run's REAL mode (build | final | scheduled | rehearsal),
// which only the caller knows (toolcore.Env.RunMode; the report is stamped after this returns). It is used
// for one thing: an executor LOG line must not name a certification scenario. "" is a
// local/direct run, which keeps the line.
func RunAllWithMode(c *config.Config, scenariosDir, resultsDir, project, layerFilter, tagFilter, idFilter, runID string, r Runner, ev obsquery.SandboxEvidence, mode string) (*RunResult, error) {
	return RunAllWithTarget(c, scenariosDir, resultsDir, project, layerFilter, tagFilter, idFilter, runID, r, ev, mode, "")
}

// RunAllWithTarget is RunAllWithMode plus the run-level target of a comparison member (ARGUS-CMP-11).
func RunAllWithTarget(c *config.Config, scenariosDir, resultsDir, project, layerFilter, tagFilter, idFilter, runID string, r Runner, ev obsquery.SandboxEvidence, mode, compareTarget string) (*RunResult, error) {
	scns, err := collectScenarios(scenariosDir, layerFilter, tagFilter, idFilter)
	if err != nil {
		return nil, err
	}
	// 0700: the results dir holds JMeter's run logs and raw samples. The credentials no longer
	// reach them (secret_props.go), but nothing JMeter writes there is meant for another user.
	if err := os.MkdirAll(resultsDir, 0o700); err != nil {
		return nil, err
	}
	// The run handle is embedded in every scenario's correlation id (tr-<run_id>-<scenario_id>-<hex>)
	// so Loki can be sliced by run + scenario. A direct caller may pass ""; generate one then.
	if runID == "" {
		runID = NewRunID()
	}
	// ARGUS-CMP-11: the target a comparison member names, for the checks that carry none of their own. An undeclared
	// name fails the WHOLE run here, by name, before the executor is probed and before any check fires.
	scns, targetRefusal := applyCompareTarget(c, scns, mode, compareTarget)
	if targetRefusal != "" {
		return allErroredWith(scns, project, runID, targetRefusal), nil
	}
	// ARGUS-CMP-3: nil unless mode is `compare`; the one switch behind every recording path below.
	oc := newOutputCtx(mode, resultsDir, runID, r)
	// RO-04 runtime preflight: verify the executor is reachable BEFORE running. A dead rig
	// short-circuits the whole run to `error` (UC-62/VR-L3) — never run blind. BUT only when
	// the WHOLE run actually uses JMeter: MCP/UI/chain scenarios run on their own native
	// runners and never touch `r`, so a JMeter healthcheck must NOT abort an MCP-only/mixed run.
	if allNeedJMeter(scns) {
		if hc, ok := r.(HealthChecker); ok {
			if herr := hc.Healthcheck(); herr != nil {
				rr := allErrored(scns, project, runID, herr)
				markSandboxPolicyShortCircuit(c, rr.Report) // spec 26: block on ⇒ every row carries one
				return rr, nil
			}
		}
	}
	corrByID := map[string]string{}
	byLayer := map[string][]report.ScenarioResult{}
	// VR10-R1 (owner D8): ONE pause budget for the whole run. nil when the SUT declared no
	// rate_limit — then nothing here can fire and the run behaves exactly as it did before.
	p := newPacer(c)
	// money_writes (item 3, "across all scenarios"): ONE allowlist + ONE spend ledger for the whole
	// run, computed once here — exactly the pacer's own "one pause budget for the whole run" shape.
	// allow is nil (no exemptions, today's behaviour exactly) whenever the SUT declares no
	// money_writes block; ledger is always safe to Reserve against even when allow is empty/nil.
	moneyAllow := c.MoneyWriteAllowlist()
	moneyLedger := &scenario.MoneySpendLedger{}

	for _, sf := range scns {
		layer := PrimaryLayer(sf.s)
		corr := newScenarioCorrelationID(runID, sf.s.ID)
		corrByID[sf.s.ID] = corr

		// Spec 26 (A1): the window the sandbox evidence is attributed to opens immediately before the
		// scenario runs and closes after its throttle re-fire and BEFORE its cleanup ( a cleanup
		// talks to the database, not to the agent). A pacer pause inside a re-fire stays inside it.
		windowStart := time.Now()
		res := runOneScenario(c, sf, corr, resultsDir, r, moneyAllow, moneyLedger, mode, oc)
		// The SUT said "slow down". Pause the run HERE — between scenarios, outside every
		// scenario's own timeout clock — and re-fire this one scenario once.
		//
		// ⛔ AC-D18b: never a chain carrying an amqp step (mayRefire) — a re-fire would re-publish into
		// a real agent's inbox. That row keeps the not-measured status it already has.
		if res.RateLimited && mayRefire(sf.s) {
			res = p.handle(res, func() report.ScenarioResult {
				oc.clearCheck(sf.s.ID) // compare mode: the first attempt's stored files go before the second writes (nil-safe, a no-op elsewhere)
				return runOneScenario(c, sf, corr, resultsDir, r, moneyAllow, moneyLedger, mode, oc)
			})
		}
		res.WindowStart, res.WindowEnd = windowStart, time.Now()
		// ── VR12-C2 (V29-015) — CLEANUP RUNS HERE, ON EVERY PATH ─────────────────────────────────
		//
		// After the scenario and its throttle retry, before the row is recorded. It runs whether the
		// scenario passed, failed or errored — a SQL cleanup talks to the DATABASE, not to the SUT,
		// so it can still succeed when the SUT itself is unreachable.
		//
		// ⛔ THE RESULT IS ATTACHED AND NEVER CONSULTED. `res.Status` is already decided above and
		// nothing below reads `res.Cleanup`: a green cleanup cannot rescue a failed scenario and a
		// red one cannot fail a passing one. Guarded by TestCleanup_NeverChangesTheVerdict.
		res.Cleanup = RunCleanup(c, sf.s, corr, r)

		// VR12-E13 — TIER 3. Attached at the ONE point every scenario's row passes through, after the
		// throttle retry so it rides the row that is actually reported.
		//
		// It is deliberately independent of the verdict: what it reports is STRUCTURAL — a claim this
		// product cannot execute for this kind of scenario, whether the run passed, failed or never
		// reached the SUT. That is why it never changes a verdict and why it is attached here rather
		// than inside any executor's success path.
		res.Unexecuted = UnexecutedAfterRun(sf.s, res)
		byLayer[layer] = append(byLayer[layer], res)
	}

	// AC-11: the survival-plane pass runs BEFORE summarize (it can move a scenario from `passed` to
	// `degraded`, and the tallies must reflect the row's FINAL status).
	applySurvivalPlane(c, byLayer)
	// Spec 26 (A1, observe only): attach what the SUT agent's sandbox denied in each window. After the
	// survival plane and before summarize, and it never changes a status, so the tallies are unaffected.
	applySandboxPolicy(c, byLayer, ev)
	rep := summarize(project, byLayer)
	// P3 #23: capture the SUT namespace's environment for a load run — see environment.go. nil on
	// any run with no declared LOAD profile, exactly like the per-scenario Load field it accompanies.
	rep.Environment = captureEnvironmentFor(scns, mode, c.TestTargets)
	p.stamp(&rep.Summary)
	return &RunResult{Report: rep, CorrelationID: corrByID}, nil
}

// runOneScenario executes ONE scenario end to end and returns its row. Split out of RunAll for
// VR10-R1: a throttled scenario is re-fired once, and "re-fire the scenario" has to be something the
// run loop can actually call.
//
// NOTHING INSIDE IT CHANGED with the split except the HTTP-429 branch: each `append(...); continue`
// became a `return`. The body is kept inside a bare block so the extraction shows up as those
// returns and nothing else — a re-indentation would have hidden the one real change in 100 lines of
// whitespace, on the file the whole run passes through.
// MoneyGuardRefusal prefixes the observed text of a scenario the money-path guard (T5.4) refused
// before firing it.
const MoneyGuardRefusal = "refused before firing: this SUT is declared money_handling, and "

func runOneScenario(c *config.Config, sf scenarioFile, corr, resultsDir string, r Runner,
	moneyAllow scenario.MoneyWriteAllowlist, moneyLedger *scenario.MoneySpendLedger, mode string, oc *outputCtx) report.ScenarioResult {
	layer := PrimaryLayer(sf.s)
	{
		// ⛔ T5.4 — THE MONEY-PATH GUARD, AT THE LAST DOOR. validate-config refuses these scenarios
		// already; this refuses them again HERE because a file can reach a run without passing the
		// validator (a catalog seeded before the flag was set, a hand-edited kit). Nothing is fired:
		// no SUT call, no CLEANUP. `error`, never `failed` — it says nothing about the SUT.
		if c.MoneyHandling {
			if v := scenario.MoneyGuardViolations(sf.s, moneyAllow); len(v) > 0 {
				return report.ScenarioResult{
					ID: sf.s.ID, CorrelationID: corr, Status: report.StatusError,
					Failure: &report.Failure{Observed: MoneyGuardRefusal + strings.Join(v, "; ")},
				}
			}
		}
		// VR10-S3: a **Target** word that names nothing usable is refused HERE, for every runner
		// path, before anything is fired — and never replaced by the plain slot (that silent
		// fallback is how NEO-010 quietly hit the gateway). The runners below re-select the entry.
		if sf.s.Target != "" {
			if _, terr := c.SelectTarget(sf.s); terr != nil {
				return targetRefused(sf.s, corr, terr)
			}
		}

		// ⛔ — A LOAD CHECK OF A TARGET DECLARED `load_test: never` IS NOT FIRED, AT THE LAST
		// DOOR. Any check with a `## LOAD` section (AMQP Load or not) whose test target declares the key is
		// refused here, before anything is dialled. `error`, never `failed`: it says nothing about the SUT.
		if why := c.NeverLoadRefusal(sf.s); why != "" {
			return report.ScenarioResult{ID: sf.s.ID, CorrelationID: corr, Status: report.StatusError,
				Failure: &report.Failure{Observed: why}}
		}

		// ⛔ S9 — LOAD ONLY WHERE THE OPERATOR ALLOWED IT, AT THE LAST DOOR.
		//
		// An `AMQP Load` scenario whose **Target** is not under `load_allowed_targets` (or whose Steps
		// exceed that entry's max_sessions) is refused HERE, for every runner path, before R10, before the
		// dispatch below, before DeriveProps, before JMeter and before any environment read: nothing dials
		// the broker. validate-config and the control plane say so earlier; this is the guarantee.
		// `error`, never `failed` (it says nothing about the SUT). Deleting THIS call is the wiring mutation
		// TestAMQPLoad_NotAllowedIsRefusedBeforeFiring is built to catch.
		if refused := amqpLoadRefusal(c, sf.s, corr); refused != nil {
			return *refused
		}
		// — THE SAME DOOR FOR AN `HTTP Load` RAMP: its http target must be under
		// load_allowed_targets, its top step within that entry's max_sessions, and its profile within the layer's
		// hard caps, or nothing is sent to it. Deleting THIS call turns TestHTTPLoad_NotAllowedIsRefusedBeforeAnyRequest red.
		if refused := httpLoadRefusal(c, sf.s, corr); refused != nil {
			return *refused
		}

		// ARGUS-CMP-3 (, design 11.2 item 4) — A `## COMPARE` THIS EXECUTOR CANNOT HONOUR IS
		// NOT RUN, AT THE SAME LAST DOOR. In mode `compare` only: a section with a problem, a key this
		// executor does not know, or a key from a newer release is `error` "refused before firing", and
		// nothing is fired. In every other mode the section is inert and the check runs exactly as before.
		if refused := compareRefusal(oc, sf.s, corr); refused != nil {
			return *refused
		}
		// ARGUS-CMP-11 fix F1: a member target this check cannot take is not run (nothing is fired, nothing recorded).
		if sf.memberTargetRefusal != "" {
			return report.ScenarioResult{ID: sf.s.ID, CorrelationID: corr, Status: report.StatusError,
				Failure: &report.Failure{Observed: CompareRefusal + sf.memberTargetRefusal}}
		}

		// ⛔ R10 (V31-002) — A SCENARIO THAT DECLARES NOTHING IS NOT RUN.
		//
		// It returns BEFORE the dispatch: no SUT call, no CLEANUP, no rate-limit pacing. The twin of
		// W2 at authoring time, and the reason both exist is that R1 stops treating a flat `## EXPECT`
		// as all-runnable — so an old file now declares nothing, and without this it would go quiet
		// instead of loud.
		//
		// ⚠ A `ui` scenario is checked the SAME WAY W2 checks it, through the SAME predicate: it
		// passes with a runnable bullet OR by naming its own spec file, where its assertions live. So
		// runUIScenario still runs and ui.Classify still decides; R10 catches only a ui scenario that
		// declares nothing AND names no spec — which today cannot be saved either.
		//
		// R4 keeps its own message for a file that HAS bullets but no `status=`; R10 is checked first,
		// so a file with neither gets R10's, which is the more basic fault.
		if len(sf.s.RunnableExpect()) == 0 &&
			!(contains(sf.s.Tags, UITag) && scenario.UINamesItsOwnSpec(sf.s)) {
			return report.ScenarioResult{
				ID: sf.s.ID, CorrelationID: corr, Status: report.StatusError,
				Failure: &report.Failure{Observed: AssertsNothingObserved},
			}
		}

		// AMQP Load: one JMeter run per step of the declared ramp. Past S9's door above;
		// it never reaches DeriveProps (an AMQP Load scenario has no HTTP request to derive).
		if layer == scenario.AMQPLoadLayer {
			return runAMQPLoad(c, sf.s, corr, resultsDir, r, mode)
		}
		// HTTP Load: one JMeter run per step of the declared ramp, stopping at the first step
		// that is not comfortable. Past its door above.
		if layer == scenario.HTTPLoadLayer {
			return runHTTPLoad(c, sf.s, corr, resultsDir, r, mode)
		}

		// mcp scenario: runner-NATIVE (no JMeter) — the runner calls the SUT's MCP tool
		// and judges the two error planes in Go (D3 / VR-J9). It rides the SAME pipeline
		// (collected, correlated, reported, tag/layer-selectable) as any scenario.
		// chain scenario: multi-step (MCP→…→UI) sequenced in Go, ONE cid threaded across
		// all steps, per-step status + partial-failure on a mid-chain break (D3.6 / UC-32/63).
		if contains(sf.s.Tags, ChainTag) {
			return runChainScenarioWithOutputs(c, sf.s, corr, ui.VendorDir, moneyAllow, moneyLedger, oc)
		}

		if contains(sf.s.Tags, MCPTag) {
			return oc.withNotSupported(sf.s, runMCPScenario(c, sf.s, corr))
		}

		// ui scenario: runner-driven VENDORED Playwright (D4 / UC-84). The runner spawns
		// the OS process, maps exit+artifact to a verdict (passed / SUT-failed / exec-error),
		// and rides the SAME pipeline (correlated, reported, tag/layer-selectable). NO JMeter.
		if contains(sf.s.Tags, UITag) {
			return oc.withNotSupported(sf.s, runUIScenario(sf.s, corr, ui.VendorDir))
		}

		// ⛔ V29-016 — A SCENARIO THAT CANNOT BE JUDGED IS NOT RUN.
		//
		// Reaching here means the runner will judge this scenario by comparing response codes. With
		// no declared `status=` there is nothing to compare against: the old code invented 202 at two
		// separate sites and let the resulting mismatch look like a SUT defect. The body-assertion
		// overlay cannot rescue it either — it only applies to a scenario that already PASSED the
		// code judge, so without a declared status no assertion in the file can decide anything.
		//
		// VR12-E6 refuses this at authoring time. This branch is for the catalogues written before
		// that rule: they are reported (here as `error`, and by name in the tier-3 list), never
		// guessed at. Firing the request first would cost the SUT a call and still decide nothing.
		//
		// ⚠⚠ THIS IS A BUILD-STAGE DEFAULT ON AN OWNER-RESERVED QUESTION (SA §0.8 Q1). The PO left the
		// run-time behaviour unruled and the SA declined to close it. Two candidates were named:
		//
		//	(A) judge on ANY response that arrived — the scenario passes on the status plane, and the
		//	    tier-3 entry records that the plane was not evaluated.   ← strictly VR12-X5-lenient
		//	(B) judge NOTHING on the status plane; report `error` + the tier-3 entry.   ← BUILT
		//
		// (B) is chosen because (A) renders a missing signal as healthy: a green row for a scenario
		// that was compared against nothing. `error` is this product's existing word for "not
		// measured" (the dead-rig and rate-limited branches below use it for exactly that), so this
		// is NOT a new SUT failure and VR12-X5's "no new rule may FAIL a scenario that runs today"
		// still holds — `error` and `failed` are distinct statuses here, and only `failed` blames
		// the SUT.
		//
		// ⛔ TO FLIP TO (A): delete this whole `if` block. Nothing else changes — DeriveProps already
		// emits no `expect.status`, judge() already refuses to invent one, and the tier-3 entry is
		// attached at the run loop's choke point regardless of which branch runs. Measured on this
		// tree: ZERO of the 119 example scenarios are affected either way (every code-judged scenario
		// already declares its status), so the choice costs nothing today and is reversible for free.
		if scenario.JudgedByResponseCode(sf.s) && !scenario.DeclaresStatus(sf.s) {
			return report.ScenarioResult{
				ID: sf.s.ID, CorrelationID: corr, Status: report.StatusError,
				Failure: &report.Failure{Observed: NoDeclaredStatusObserved},
			}
		}

		props, perr := DeriveProps(c, sf.s, corr)
		if errors.Is(perr, ErrNoRoutingKey) {
			// not measured, and the gap is the config's: `error`, never `failed` (only `failed` blames the SUT)
			return report.ScenarioResult{
				ID: sf.s.ID, CorrelationID: corr, Status: report.StatusError,
				Failure: &report.Failure{Observed: NoRoutingKeyRefusal + " — preflight"},
			}
		}
		if perr != nil {
			return targetRefused(sf.s, corr, perr)
		}

		// money_writes (item 3) — THE SPEND CHECK, AT THE DOOR THAT SEES THE RESOLVED VALUE. props
		// carries the plain TRIGGER's method/path/body exactly as they will be handed to the runner
		// (props["trigger.payload"] is resolveVars'd — the SAME resolution the request itself gets),
		// so this is the one point in a plain-HTTP scenario's path where the amount can be checked
		// against reality rather than against a literal in the file. Nothing is fired on a refusal:
		// `error`, never `failed` — same contract as the structural guard above.
		if c.MoneyHandling {
			if entry := moneyAllow.Match(props["trigger.method"], props["trigger.path"]); entry != nil && entry.Spends {
				if msg := scenario.EvaluateSpendAmount(entry, props["trigger.payload"]); msg != "" {
					return report.ScenarioResult{ID: sf.s.ID, CorrelationID: corr, Status: report.StatusError,
						Failure: &report.Failure{Observed: MoneyGuardRefusal + "its declared money_writes spend limit refused it: " + msg}}
				}
				if ok, _ := moneyLedger.Reserve(entry); !ok {
					return report.ScenarioResult{ID: sf.s.ID, CorrelationID: corr, Status: report.StatusError,
						Failure: &report.Failure{Observed: fmt.Sprintf("%sits declared money_writes spend limit refused it: %s %s already reached its max_per_run (%d) for this run",
							MoneyGuardRefusal, entry.Method, entry.Path, entry.MaxPerRun)}}
				}
			}
		}

		// VR12-E7: the dedicated 2-sampler template is chosen ONLY on a DECLARED `status2=`,
		// and the codes the judge compares against are the declared ones, in order.
		declaredFirst, declaredSecond := extractDeclaredStatuses(sf.s.RunnableExpect())
		var expectedCodes []int
		if declaredFirst > 0 {
			expectedCodes = append(expectedCodes, declaredFirst)
		}
		if declaredSecond > 0 {
			expectedCodes = append(expectedCodes, declaredSecond)
		}
		base := templateBaseFor(layer)
		if declaredSecond > 0 && layer == "HTTP Ingestion" {
			base = "http-idempotency" // dedicated 2-sampler template (same Idempotency-Key)
		}
		isSagaPresence := contains(sf.s.Tags, SagaPresenceTag)
		if isSagaPresence {
			base = "saga-presence" // trigger + in-JMeter Loki saga assertion (Decision-2); judged by the .jtl success flag
		}
		jtl := filepath.Join(resultsDir, base+"__"+sf.s.ID+".jtl")
		_ = os.Remove(jtl) // JMeter -l APPENDS; start each run fresh
		_ = os.Remove(jtl + ".log")
		// ARGUS-CMP-3: in mode `compare`, an http-template check that declares `## COMPARE` (and no
		// `## LOAD`) gets a scratch directory and the `output.capture.dir` property; the template's JSR223
		// block writes the raw response there. nil in every other case, and then nothing below changes.
		capture := oc.beginJMeterCapture(sf.s, base, mode, props)
		defer capture.cleanup() // the raw files are deleted on EVERY path, a failed run and a panic included
		// VR12-TO-RUN: the PROCESS backstop (jmeterProcessBackstop), never the raw declared TIMEOUT —
		// that now bounds each REQUEST, inside JMeter (trigger.timeout_ms/trigger.timeout_s, above).
		cpuBefore, haveCPUBefore := httpLoadCPUStat() // #615: only recorded for a plain ## LOAD check
		runErr := r.Run(base, jtl, props, jmeterProcessBackstop(sf.s, base))
		cpuAfter, haveCPUAfter := httpLoadCPUStat()
		recorded := capture.collect(c, sf.s)
		if capture == nil && !jmeterCaptureTemplates[base] && sf.s.Load == nil {
			recorded = oc.notSupported(sf.s) // this layer records nothing yet: not measured, never "identical"
		}

		// Judging strategy is per-layer: STATUS layers (HTTP Ingestion / Error Path /
		// Rate Limiting / Permissions) judge by response code vs expected (JMeter marks
		// an expected 4xx as failure). CONTENT layers (Database State / Message Flow /
		// External Delivery) judge by the JMeter `success` flag — their JMX embeds the
		// assertion (e.g. the JDBC row exists), so success=false means the content check failed.
		// In EVERY layer a declared `status=` is asserted FIRST, against the SUT-trigger
		// sample(s): a content layer whose trigger answered the wrong code is `failed` before
		// its content assertion is consulted (see the content case below).
		codes, labels, tsMs, elapsedPerRow, durMs, msgs := readJTLCodesElapsed(jtl)
		res := report.ScenarioResult{ID: sf.s.ID, CorrelationID: corr, DurationMs: durMs, Status: "failed", Outputs: recorded}
		// Test-requests panel (r3, 1a′): record each SUT-trigger sample as a timestamped
		// RequestSample by response outcome — EXCLUDING the content/saga templates' verification
		// samplers (a JDBC verify, a RabbitMQ mgmt-API GET, a Loki saga poll), which are the
		// runner's assertions against infra, NOT requests to the SUT. This is the REQUEST's
		// outcome, NOT the scenario's verdict below — an error-path scenario that EXPECTs (and
		// gets) a 4xx records a `failed` response here while still passing as a scenario.
		for _, smp := range classifyRequestSamples(codes, labels, tsMs, msgs, sf.s.ID) {
			res.AddRequest(smp.AtMs, smp.Outcome)
		}
		// AC-11 (mode 2): a declared LOAD profile gets its percentile + error-rate read from this
		// same .jtl, over the same SUT-trigger samples the request-distribution panel counts (a
		// verification probe is not a load sample any more than it is a request).
		if sf.s.Load != nil {
			ls := report.ComputeLoadStats(sutTriggerElapsed(codes, labels, elapsedPerRow, sf.s.ID), res.ReqFailed+res.ReqError)
			ls.TargetP95Ms, ls.MaxErrorRate = sf.s.Load.TargetP95Ms, sf.s.Load.MaxErrorRate
			ls.Breached = ls.Breaches(sf.s.Load.TargetP95Ms, sf.s.Load.MaxErrorRate)
			// the transport-failure reasons and the per-bucket view, from the same rows. The
			// reason text comes from the network, so it takes the credential scrub of an output record.
			samples := readJTLLoadSamples(jtl, sf.s.ID)
			ls.Errors = report.LoadErrorsFrom(samples, credentialScrub(c))
			ls.TimelineBucketS, ls.Timeline = report.LoadTimelineFrom(samples)
			// #615: the executor's own CPU throttling during the run, recorded only (the verdict is unchanged).
			if v, ok := httpload.ThrottledShare(cpuBefore, cpuAfter, haveCPUBefore, haveCPUAfter); ok {
				ls.GeneratorCPUThrottledShare = &v
			} else {
				ls.GeneratorNotMeasured = true
			}
			res.Load = &ls
			// ARGUS-CMP-11: in mode `compare`, a load check whose `## COMPARE` declares `Not Worse Than` records its
			// load numbers under outputs[].load (nil in every other mode and for every other check).
			if recs := oc.loadRecords(sf.s, res.Load); recs != nil {
				res.Outputs = recs
			}
			// The survival-plane window (RunAll's applySurvivalPlane): this scenario's own
			// SUT-trigger fire-time range, min to max.
			for _, smp := range res.Requests {
				if res.LoadWindowStartMs == 0 || smp.AtMs < res.LoadWindowStartMs {
					res.LoadWindowStartMs = smp.AtMs
				}
				if smp.AtMs > res.LoadWindowEndMs {
					res.LoadWindowEndMs = smp.AtMs
				}
			}
		}
		var pass bool
		var observed string
		// failedBody names the body bullet a status-layer body check failed on (test hat only).
		var failedBody *report.FailedBodyCheck
		// contentPlaneFailed: the verdict is `failed` BECAUSE a content check (body / column) was
		// evaluated and missed — so those checks were enforced, as on a chain step whose body plane
		// failed. A status miss, a run error or a dead SUT never sets it.
		var contentPlaneFailed bool
		switch {
		case isSagaPresence || contentLayerJudged(layer):
			// ⛔ THE DECLARED STATUS IS ASSERTED FIRST, HERE TOO. Until this gate existed the content
			// branch never compared the trigger's response code with the scenario's `status=`, and
			// no JMX template reads `expect.status` (zero hits across templates/*.jmx) — so for a
			// content-judged scenario the declared status was asserted NOWHERE. Measured 2026-09-17:
			// MSGF-005 (`Permissions -> Message Flow`, `status=401`) went green while the SUT accepted
			// the order with a wrong token and answered 202; the queue assertion held, and nothing
			// looked at the 202. A declared status is a claim like any other bullet: a mismatch is
			// `failed`, before the content assertion is consulted, with the same judge and the same
			// reality-only observed line the status layers use (VR-C8: the declared value lives in
			// failure.expected, never in observed).
			//
			// Only the SUT-trigger samples are compared: the content templates fire verification
			// probes (a management-API GET, a JDBC verify, a Loki poll) BEFORE and after the trigger,
			// and message-flow.jmx's baseline probe is the FIRST row of the .jtl. The exclusion is the
			// one classifyRequestSamples already applies (isVerifySample).
			//
			// A scenario that declares no status keeps its content-only judgment: nothing is invented
			// (V29-016), and a content layer is not refused for lacking one.
			statusHeld := true
			if len(expectedCodes) > 0 {
				statusHeld, observed = judge(sutTriggerCodes(codes, labels, sf.s.ID), expectedCodes, runErr)
				if !statusHeld && runErr == nil {
					// VR12-TO-RUN: name a per-request JMeter TIMEOUT explicitly, before the generic
					// "declared status not met" wrapping — a reader must be able to tell "the SUT was
					// simply too slow" from any other status mismatch at a glance.
					observed = timeoutAwareObserved(observed, codes, msgs, sf.s.TimeoutDuration())
					observed = "declared status not met — " + observed + "; the content assertion was not evaluated"
				}
			}
			// CONTENT-style judging by the JMeter success flag (the JMX embeds the
			// assertion). saga-presence (Decision-2): the saga-presence.jmx JSR223 step
			// fired the trigger, polled Loki, and set success=false + a reality-only
			// failureMessage when the mandated saga was absent or Loki was unreachable
			// (fail-closed). Content layers: the JDBC/queue assertion sets success.
			ok, failMsg := readJTLSuccess(jtl)
			pass = statusHeld && runErr == nil && ok
			contentPlaneFailed = statusHeld && runErr == nil && !ok
			// When the declared status was not met, observed already names the actual code (or
			// the run error) and the content plane is not consulted; only a scenario past the
			// status gate is described by its content assertion.
			if statusHeld && !pass {
				if runErr != nil {
					observed = "jmeter run error: " + runErr.Error()
				} else if failMsg != "" {
					observed = failMsg
				} else if isSagaPresence {
					// B1 (RO-09): ingestion-neutral wording — "not found in Loki" does NOT claim the
					// SUT failed to EMIT (emission vs ingestion is indistinguishable to triage). The
					// VERDICT stays FAIL (a mandated control_action saga is a CODE_BUG, req 3); the
					// VALIDATE INPUT gate proves promtail ingests this SUT before the run, so an
					// absence here genuinely means not-emitted.
					observed = "control_action saga not found in Loki for correlation_id " + corr + " (saga-presence FAIL)"
				} else {
					observed = "content assertion failed (no matching row/message for the correlation_id)"
				}
			}
		default:
			pass, observed = judge(codes, expectedCodes, runErr)
			if !pass && runErr == nil {
				// VR12-TO-RUN: name a per-request JMeter TIMEOUT explicitly (see the content branch's
				// twin above) — otherwise "responder returned status 0" reads exactly like a dead SUT.
				observed = timeoutAwareObserved(observed, codes, msgs, sf.s.TimeoutDuration())
			}
			// DF-04: enforce a status-layer body assertion. The code-judge can't see the
			// body; the http-ingestion JSR223 sets a distinct BODY-ASSERT-FAIL message on a
			// body mismatch. Combine: a code that matched but a failed body assertion → fail
			// (an expected-4xx with NO body assertion is unaffected — failMsg won't carry the marker).
			// ⛔ V31-006: RUNNABLE ONLY — a `### Non-runnable` body line used to switch this gate on.
			if pass && hasBodyAssert(sf.s.RunnableExpect()) {
				if ok, failMsg := readJTLSuccess(jtl); !ok && strings.Contains(failMsg, "BODY-ASSERT-FAIL") {
					pass = false
					contentPlaneFailed = true
					// VR-C8: observed is REALITY-ONLY for BOTH hats — it must NEVER echo the test's
					// expected body value. The raw JMeter failMsg used to contain the expected
					// substring/regex (a holdout leak to the product hat via observed); set a fixed
					// reality-only message instead (the expected lives only in failure.expected, test-hat).
					observed = bodyAssertObserved
					// WHICH bullet failed, for the test hat only —
					// from the template's structured index, mapped back to the parsed scenario. observed
					// above is untouched; redactExpected nils this for the product hat.
					failedBody = failedBodyCheckFrom(failMsg, sf.s.RunnableExpect())
				}
			}
		}
		if pass {
			res.Status = "passed"
		} else if runErr != nil && report.IsExecutorFailure(runErr.Error()) {
			// RO-04: a harness/executor failure (JMeter down, docker exec error) is `error`,
			// NOT a SUT `failed` — one dead rig must never become N fake test failures
			// (UC-62/VR-L3). No `expected`: it is not a SUT mismatch.
			res.Status = report.StatusError
			res.Failure = &report.Failure{Observed: observed}
			// Test-requests panel: a dead rig fired no countable SUT request — discard any
			// partial/spurious samples so the panel reads 0/0/0 (plan: dead rig ⇒ 0 requests).
			res.Requests = nil
			res.ReqSuccess, res.ReqFailed, res.ReqError = 0, 0, 0
		} else if hit, after := httpRateLimited(c.RateLimit, codes, jtl); hit {
			// VR10-R1-12: the SUT answered 429 — it REFUSED to be measured because we were going too
			// fast. Not measured, so `errored` and no Expected, exactly as on the MCP plane. The run
			// loop above reads RateLimited/RetryAfterMs and pauses BETWEEN scenarios.
			//
			// A scenario that EXPECTS 429 never reaches here: it PASSED, and the branches are ordered
			// so a pass is decided first. Testing a limiter and being refused by one are opposites.
			res.Status = report.StatusError
			res.RateLimited = true
			res.RetryAfterMs = int(after.Milliseconds())
			res.Failure = &report.Failure{Observed: mcp.RateLimitedObserved(after)}
		} else if res.NeverReachedSUT() {
			// THE SUT NEVER ANSWERED. The rig is fine — JMeter ran and exited normally, so the branch
			// above does not fire — but every request the runner fired came back a transport error:
			// connection refused, name not resolved, code 0. Nothing was tested, so there is nothing
			// to have failed.
			//
			// This is the rule the request counts already apply one level down: ReqError is defined as
			// "the test couldn't get a response" and ReqFailed as a real negative RESPONSE. The
			// scenario status simply had no way to say it — StatusError was reachable only from a dead
			// rig, so the HTTP path could report passed or failed and nothing else.
			//
			// MEASURED (run 20260809T172606532, orderservice-compose, 2026-08-09): the SUT had been down
			// 24h and all 33 scenarios scored `failed` with req_error=1, req_success=0, req_failed=0 — a
			// single uniform pattern, and exactly the set this predicate selects. "33 failed" reads as a
			// SUT broken in 33 ways when it was ABSENT, sending a triager into the scenario files
			// instead of to `docker ps`.
			//
			// DELIBERATELY CONSERVATIVE. It demands that NOTHING got through: one real response — even a
			// 500 — means the SUT was reachable and the scenario genuinely failed, so a PARTIAL outage
			// stays `failed` rather than being excused as infrastructure. A passing scenario is never
			// touched either; only a would-be `failed` is reclassified.
			res.Status = report.StatusError
			// No Expected, for the same reason as the dead-rig branch above: this is not a SUT mismatch,
			// and an expectation that nothing was measured against is noise.
			res.Failure = &report.Failure{Observed: observed}
		} else {
			exp := strings.Join(sf.s.Expect, "; ")
			res.Failure = &report.Failure{Observed: observed, Expected: &exp, FailedBodyCheck: failedBody}
		}
		// The content checks this scenario's template was handed and the verdict consulted: on a pass,
		// and on a `failed` whose failing plane was the content. Status lines are not counted, so 0
		// still means only the status / no-error was checked. Text for the test hat, count for both.
		if pass || (contentPlaneFailed && res.Status == "failed") {
			res.AssertionsEnforced = enforcedContentChecks(base, props)
			res.AssertionsEnforcedCount = len(res.AssertionsEnforced)
		}
		// the status the SUT ANSWERED, from the .jtl responseCode of the SUT-trigger
		// samples (the verification probes of the content templates are not the SUT's answer). Never
		// the expected value: a status-only scenario reports assertions_enforced_count 0 by design
		// (enforced.go), and without this a reader could not tell the status had been checked.
		// Code 0 is a transport failure, not a status, and is not reported as one.
		res.ObservedStatus, res.ObservedStatusCodes = observedStatusFrom(sutTriggerCodes(codes, labels, sf.s.ID))
		// AC-11: a load SLA breach (measured p95/error-rate past the scenario's OWN declared
		// targets) is the scenario's own claim being wrong — it surfaces as `failed`, overriding a
		// code-level pass. It never touches an already-`error`/`failed` row: those are already the
		// worst word this run has for this scenario.
		if res.Load != nil && res.Load.Breached && res.Status == "passed" {
			res.Status = "failed"
			observed := fmt.Sprintf(
				"load target breached: p95=%dms (target %dms), error_rate=%.4f (max %.4f)",
				res.Load.P95Ms, res.Load.TargetP95Ms, res.Load.ErrorRate, res.Load.MaxErrorRate)
			if len(res.Load.Errors) > 0 {
				// say WHY requests got no answer, not only how many. Already scrubbed.
				observed += fmt.Sprintf("; top transport failure: %s (%d of %d requests)",
					res.Load.Errors[0].Reason, res.Load.Errors[0].Count, res.Load.Samples)
			}
			res.Failure = &report.Failure{Observed: observed}
		}
		return res
	}
}

// sutTriggerCodes is classifyRequestSamples's verify-exclusion applied to the response codes: the
// codes of the samples the runner fired AT THE SUT, in .jtl order, with the content/saga templates'
// verification probes dropped. It is what the declared `status=` is compared against in a content
// layer, where the trigger is not the first row of the .jtl. Index-aligned inputs, same as
// classifyRequestSamples; a shorter/absent labels slice keeps every code (defensive).
func sutTriggerCodes(codes []int, labels []string, scenarioID string) []int {
	out := make([]int, 0, len(codes))
	for i, c := range codes {
		if i < len(labels) && isVerifySample(labels[i], scenarioID) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// observedStatusFrom turns the SUT-trigger response codes (.jtl order) into the report's observed
// status: the first code that is a real HTTP status (> 0; 0 is a transport failure), and, only when
// the samples answered with more than one DISTINCT code, those codes once each in first-seen order,
// capped at maxObservedStatusCodes. Distinct, not every sample: a load scenario fires thousands of
// requests and the per-sample list belongs in the .jtl, not in report.json (counts are req_success).
// Nothing here sees the scenario's expected value.
func observedStatusFrom(trigger []int) (first int, all []int) {
	for _, c := range trigger {
		if c > 0 {
			first = c
			break
		}
	}
	if first == 0 {
		return 0, nil
	}
	seen := map[int]bool{}
	for _, c := range trigger {
		if seen[c] {
			continue
		}
		seen[c] = true
		if len(all) < maxObservedStatusCodes {
			all = append(all, c)
		}
	}
	if len(seen) < 2 {
		return first, nil
	}
	return first, all
}

// maxObservedStatusCodes bounds observed_status_codes: past a handful of distinct codes the list stops
// helping a reader and the .jtl holds the full record.
const maxObservedStatusCodes = 16

// readJTLLoadSamples reads the SUT-trigger rows of the .jtl with their RAW responseCode and
// responseMessage text (readJTLCodesElapsed turns a non-numeric code into 0 and drops it), for the load
// record's transport-failure reasons and timeline. The verification probes are excluded, as everywhere.
func readJTLLoadSamples(jtlPath, scenarioID string) []report.LoadSample {
	f, err := os.Open(jtlPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1
	rows, err := rd.ReadAll()
	if err != nil || len(rows) < 2 {
		return nil
	}
	head := map[string]int{}
	for i, h := range rows[0] {
		head[h] = i
	}
	get := func(row []string, key string) string {
		if i, ok := head[key]; ok && i < len(row) {
			return row[i]
		}
		return ""
	}
	var out []report.LoadSample
	for _, row := range rows[1:] {
		if isVerifySample(get(row, "label"), scenarioID) {
			continue
		}
		s := report.LoadSample{Code: get(row, "responseCode"), Message: get(row, "responseMessage")}
		// A field that does not parse reads as 0, and the row is KEPT: it still counts in the errors and the
		// percentiles. StartMs 0 means "no start time", which report.LoadTimelineFrom documents by leaving
		// the sample out of the timeline only; a negative elapsed is clamped there too.
		s.ElapsedMs, _ = strconv.Atoi(get(row, "elapsed"))
		s.StartMs, _ = strconv.ParseInt(get(row, "timeStamp"), 10, 64)
		out = append(out, s)
	}
	return out
}

// sutTriggerElapsed is classifyRequestSamples's verify-exclusion (AC-11), applied to elapsed times
// instead of outcomes: the load percentiles are computed over the SAME SUT-trigger samples the
// request-distribution panel counts — a JDBC/broker/Loki verification probe is not a load sample
// any more than it is a request. Index-aligned inputs, same as classifyRequestSamples.
func sutTriggerElapsed(codes []int, labels []string, elapsedPerRow []int, scenarioID string) []int {
	var out []int
	for i := range codes {
		if i < len(labels) && isVerifySample(labels[i], scenarioID) {
			continue
		}
		if i < len(elapsedPerRow) {
			out = append(out, elapsedPerRow[i])
		}
	}
	return out
}

// summarize builds the report (layers + counts) from the per-layer results.
func summarize(project string, byLayer map[string][]report.ScenarioResult) *report.Report {
	rep := &report.Report{Project: project, Mode: "ci"}
	var layerNames []string
	for l := range byLayer {
		layerNames = append(layerNames, l)
	}
	sort.Strings(layerNames)
	for _, l := range layerNames {
		scs := byLayer[l]
		rep.Layers = append(rep.Layers, report.Layer{Layer: layerDir[l], Scenarios: scs})
		for _, s := range scs {
			rep.Summary.Total++
			switch s.Status {
			case "passed":
				rep.Summary.Passed++
			case "failed":
				rep.Summary.Failed++
			case report.StatusError:
				rep.Summary.Errored++ // execution/harness failure — non-green, DISTINCT from failed (UC-62)
				if s.RateLimited {
					// VR10-R1: "rate-limited" is a REASON INSIDE errored (owner D4: no fourth
					// status), so it is counted here and never in Passed/Failed — a pass rate is
					// over MEASURED scenarios only.
					rep.Summary.RateLimitedScenarios++
				}
			case report.StatusDegraded:
				// AC-11: DEGRADED is counted on its own — never in Passed (its assertions did not
				// simply pass unremarked) and never in Failed/Errored (nothing was wrong or
				// unmeasured). A pass rate stays a pass rate; a DEGRADED run is visible separately.
				rep.Summary.Degraded++
			default:
				rep.Summary.Skipped++
			}
		}
	}
	return rep
}

// applySurvivalPlane (AC-11) flips a load-profile scenario that PASSED its own assertions to
// DEGRADED when the Prometheus survival-plane read (internal/obsquery, windowed delta) shows the
// SUT pod in distress during the run window.
//
// ⛔ NEVER TOUCHES ANYTHING ELSE. It only ever looks at a scenario whose Status IS ALREADY
// "passed" — the existing throttled-`errored` case (HTTP 429) and a Rate Limiting scenario's
// legitimate `passed` (ratelimit.go:143) are either a different status already, or (for
// Rate Limiting) never carry a Load profile at all, so neither is ever a candidate here.
//
// Opt-in (PromEnabled + a resolvable pod selector): a SUT that never declared a Prometheus surface
// gets no survival-plane read and no surprise network call on a run that never asked for one.
func applySurvivalPlane(c *config.Config, byLayer map[string][]report.ScenarioResult) {
	if !c.PromEnabled() {
		return
	}
	podSelector := c.SUTProject()
	if podSelector == "" {
		return
	}
	q := obsquery.NewPrometheusClient(c.PrometheusURL())
	for _, scs := range byLayer {
		for i := range scs {
			res := &scs[i]
			if res.Load == nil || res.Status != "passed" || res.LoadWindowEndMs == 0 {
				continue
			}
			runStart := time.UnixMilli(res.LoadWindowStartMs)
			runEnd := time.UnixMilli(res.LoadWindowEndMs)
			baseDur := runEnd.Sub(runStart)
			if baseDur <= 0 {
				baseDur = time.Second
			}
			w := obsquery.SurvivalWindow{
				RunStart: runStart, RunEnd: runEnd,
				BaselineStart: runStart.Add(-baseDur), BaselineEnd: runStart,
			}
			read := obsquery.SurvivalPlaneRead(q, podSelector, w)
			res.Load.SurvivalChecked = read.Available
			if read.Available && read.Degraded {
				res.Status = report.StatusDegraded
				res.Load.Degraded = true
				res.Load.DegradedNote = read.Reason
			}
		}
	}
}

// allNeedJMeter reports whether EVERY collected scenario uses the JMeter runner — i.e. none
// is an mcp/ui/chain scenario (which run on native runners, not `r`). The RO-04 JMeter
// preflight only short-circuits a run when this holds, so an MCP/UI/chain run is never
// false-aborted by a JMeter healthcheck.
func allNeedJMeter(scns []scenarioFile) bool {
	if len(scns) == 0 {
		return false
	}
	for _, sf := range scns {
		if contains(sf.s.Tags, MCPTag) || contains(sf.s.Tags, UITag) || contains(sf.s.Tags, ChainTag) {
			return false
		}
	}
	return true
}

// allErrored short-circuits a run when the executor preflight fails (RO-04): every
// matching scenario is `error` with one actionable message, never a blind pass/fail.
func allErrored(scns []scenarioFile, project, runID string, cause error) *RunResult {
	return allErroredWith(scns, project, runID, "executor unavailable: bring up the stack (docker compose up -d): "+cause.Error())
}

// allErroredWith is allErrored with the observed text given: every scenario `error` with msg, nothing fired.
func allErroredWith(scns []scenarioFile, project, runID, msg string) *RunResult {
	corrByID := map[string]string{}
	byLayer := map[string][]report.ScenarioResult{}
	for _, sf := range scns {
		layer := PrimaryLayer(sf.s)
		corr := newScenarioCorrelationID(runID, sf.s.ID)
		corrByID[sf.s.ID] = corr
		byLayer[layer] = append(byLayer[layer], report.ScenarioResult{
			ID: sf.s.ID, CorrelationID: corr, Status: report.StatusError,
			Failure: &report.Failure{Observed: msg},
		})
	}
	return &RunResult{Report: summarize(project, byLayer), CorrelationID: corrByID}
}

// readJTLCodes parses a JMeter .jtl (CSV) and returns the ordered HTTP response
// codes (one per sample), the matching per-sample labels (so the request counter can
// tell the SUT trigger from a verification sampler), each sample's fire-time (the JMeter
// `timeStamp` column, epoch ms — the real-time x-axis for the request-distribution panel),
// and the max elapsed ms. All slices are index-aligned.
func readJTLCodes(jtlPath string) (codes []int, labels []string, tsMs []int64, elapsedMs int) {
	codes, labels, tsMs, _, elapsedMs, _ = readJTLCodesElapsed(jtlPath)
	return
}

// readJTLCodesElapsed is readJTLCodes plus the PER-SAMPLE elapsed time (AC-11: the percentile
// calculation needs every sample's elapsed time, not just the run's max) and the PER-SAMPLE
// responseMessage (VR12-TO-RUN: distinguishing a JMeter socket TIMEOUT from a genuine connection
// failure needs the message — both carry a non-numeric responseCode, parsed as 0 below). All
// slices are index-aligned.
func readJTLCodesElapsed(jtlPath string) (codes []int, labels []string, tsMs []int64, elapsedPerRow []int, elapsedMs int, msgs []string) {
	f, err := os.Open(jtlPath)
	if err != nil {
		return nil, nil, nil, nil, 0, nil
	}
	defer f.Close()
	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1
	rows, err := rd.ReadAll()
	if err != nil || len(rows) < 2 {
		return nil, nil, nil, nil, 0, nil
	}
	head := map[string]int{}
	for i, h := range rows[0] {
		head[h] = i
	}
	get := func(row []string, key string) string {
		if i, ok := head[key]; ok && i < len(row) {
			return row[i]
		}
		return ""
	}
	for _, row := range rows[1:] {
		e, eerr := strconv.Atoi(get(row, "elapsed"))
		if eerr != nil {
			e = 0
		}
		if e > elapsedMs {
			elapsedMs = e
		}
		elapsedPerRow = append(elapsedPerRow, e)
		labels = append(labels, get(row, "label"))
		msgs = append(msgs, get(row, "responseMessage"))
		if n, err := strconv.Atoi(get(row, "responseCode")); err == nil {
			codes = append(codes, n)
		} else {
			codes = append(codes, 0) // non-numeric (e.g. connection error, or a socket TIMEOUT) => 0
		}
		if ts, err := strconv.ParseInt(get(row, "timeStamp"), 10, 64); err == nil {
			tsMs = append(tsMs, ts)
		} else {
			tsMs = append(tsMs, 0) // absent/non-numeric => fall back to now at classify time
		}
	}
	return codes, labels, tsMs, elapsedPerRow, elapsedMs, msgs
}

// isSamplerTimeoutMessage reports whether a JTL row's responseMessage names a JMeter socket-level
// TIMEOUT (HTTPSampler.connect_timeout / response_timeout firing, or a JDBCSampler queryTimeout) —
// VR12-TO-RUN's per-request enforcement (trigger.timeout_ms/trigger.timeout_s). JMeter's own text
// for this is "Non HTTP response message: Read timed out" / "...Connect timed out" / a JDBC driver's
// own "...query timeout"; a genuinely UNREACHABLE SUT (connection refused, unknown host) never
// contains "timed out" and is left to the existing NeverReachedSUT() classification. Case-
// insensitive: JMeter/JDBC driver capitalisation is not a contract worth depending on.
func isSamplerTimeoutMessage(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "timed out") || strings.Contains(strings.ToLower(msg), "timeout")
}

// timeoutAwareObserved rewrites a generic "responder returned status 0"/no-answer observed string
// into an EXPLICIT reason when the row responsible is a JMeter sampler TIMEOUT rather than a
// genuine connection failure — otherwise the reader cannot tell "SUT too slow" (a real, if
// unwelcome, measurement) from "SUT absent" (nothing was measured) apart. codes/msgs are
// index-aligned (readJTLCodesElapsed); the first matching row decides the message, same convention
// as judge()'s own "first mismatch" reporting.
func timeoutAwareObserved(observed string, codes []int, msgs []string, timeout time.Duration) string {
	for i, c := range codes {
		if c == 0 && i < len(msgs) && isSamplerTimeoutMessage(msgs[i]) {
			return fmt.Sprintf("the SUT did not answer within its declared ## TIMEOUT of %s (JMeter sampler: %s)",
				timeout, strings.TrimSpace(msgs[i]))
		}
	}
	return observed
}

// classifyRequestSamples turns each request the runner FIRED AT THE SUT into a timestamped
// RequestSample, by RESPONSE outcome, for the per-request distribution panel (r3, 1a′). It
// counts ONLY the SUT-trigger sample(s): the content/saga JMX templates ALSO emit a verification
// sampler (a JDBC verify query, a RabbitMQ management-API GET, a Loki saga poll) that is the
// runner's ASSERTION against infra — NOT a request to the SUT — and a code-less saga-verify
// sample would otherwise be mis-counted as a response. Those carry a "verify" label and are
// skipped. Outcome (r3, 3-way): 2xx/3xx = success; 4xx/5xx = failed (a negative RESPONSE);
// 0 (connection failure) / any other code = error (no usable response) — EXCEPT a 0 whose
// responseMessage is a JMeter sampler TIMEOUT (isSamplerTimeoutMessage, VR12-TO-RUN): the SUT DID
// receive the request and simply did not answer inside the declared budget, which is a MEASURED
// negative response, not an absent rig — classified `failed`, same as a 4xx/5xx. This is the
// REQUEST's outcome, NOT the scenario's verdict: an error-path/permissions scenario that EXPECTs
// (and gets) a 4xx yields a `failed` sample here while the scenario itself PASSES (judged
// separately). Each sample is stamped with its .jtl fire-time (tsMs), falling back to now if
// absent. labels/tsMs/msgs are index-aligned with codes; a shorter/absent slice degrades
// gracefully (defensive) — msgs may be nil (callers that never had a timeout to distinguish).
func classifyRequestSamples(codes []int, labels []string, tsMs []int64, msgs []string, scenarioID string) []report.RequestSample {
	var out []report.RequestSample
	for i, c := range codes {
		if i < len(labels) && isVerifySample(labels[i], scenarioID) {
			continue // a verification probe against infra (DB / broker / Loki), not a SUT request
		}
		var outcome string
		switch {
		case c >= 100 && c < 400:
			outcome = report.OutcomeSuccess // any real 1xx/2xx/3xx response
		case c >= 400:
			outcome = report.OutcomeFailed // a negative RESPONSE (4xx/5xx and beyond)
		case i < len(msgs) && isSamplerTimeoutMessage(msgs[i]):
			outcome = report.OutcomeFailed // VR12-TO-RUN: a measured non-answer, not an absent rig
		default:
			outcome = report.OutcomeError // 0 / connection failure / non-numeric: no usable response
		}
		at := int64(0)
		if i < len(tsMs) {
			at = tsMs[i]
		}
		if at == 0 {
			at = time.Now().UnixMilli()
		}
		out = append(out, report.RequestSample{AtMs: at, Outcome: outcome})
	}
	return out
}

// isVerifySample reports whether a .jtl sample is a content/saga template's VERIFICATION probe
// (a JDBC verify / RabbitMQ mgmt-API GET / Loki saga poll) rather than a SUT request. The
// verify samplers are labelled "<scenario.id>-verify[-queue|-saga]" (templates/*.jmx), so the
// exclusion is anchored to that exact "<id>-verify" prefix — NOT a bare "verify" substring,
// which would mis-drop the real SUT trigger of a scenario whose id itself contains "verify".
// When the id is unknown (defensive/direct call) it falls back to a "-verify" substring.
func isVerifySample(label, scenarioID string) bool {
	if scenarioID != "" {
		return strings.HasPrefix(label, scenarioID+"-verify")
	}
	return strings.Contains(strings.ToLower(label), "-verify")
}

// readJTLSuccess returns whether ALL samples succeeded (the JMeter success flag,
// which reflects assertion results) + the first failure's message. Used for
// content layers (Database State / Message Flow / External Delivery).
func readJTLSuccess(jtlPath string) (allOK bool, firstFailMsg string) {
	f, err := os.Open(jtlPath)
	if err != nil {
		return false, ""
	}
	defer f.Close()
	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1
	rows, err := rd.ReadAll()
	if err != nil || len(rows) < 2 {
		return false, ""
	}
	head := map[string]int{}
	for i, h := range rows[0] {
		head[h] = i
	}
	get := func(row []string, key string) string {
		if i, ok := head[key]; ok && i < len(row) {
			return row[i]
		}
		return ""
	}
	allOK = true
	for _, row := range rows[1:] {
		if !strings.EqualFold(get(row, "success"), "true") {
			allOK = false
			if firstFailMsg == "" {
				firstFailMsg = strings.TrimSpace(get(row, "label") + ": " + get(row, "responseCode") + " " + get(row, "responseMessage") + " " + get(row, "failureMessage"))
			}
		}
	}
	return allOK, firstFailMsg
}

// judge compares the actual response codes to the expected status codes (in order).
// Pass iff the run launched AND each expected code matches the corresponding sample.
func judge(codes, expected []int, runErr error) (pass bool, observed string) {
	if runErr != nil {
		return false, "jmeter run error: " + runErr.Error()
	}
	if len(codes) == 0 {
		return false, "no result row in .jtl (request did not reach the SUT)"
	}
	// ⛔ V29-016, SITE 2 OF TWO — the one the row did NOT name until 2026-09-10.
	//
	// This used to read `expected = []int{202} // default contract`. There is no contract that says
	// an undeclared status is 202; the comment asserted a rule nobody wrote down, and that is why
	// the site survived a whole round of review. VR12-E7 made it MORE reachable, not less: an
	// undeclared scenario now yields an empty slice and lands squarely here.
	//
	// A verdict cannot be decided from an expectation that does not exist. Say so.
	if len(expected) == 0 {
		return false, NoDeclaredStatusObserved
	}
	// observed must describe the responder's ACTUAL behaviour only — never the test's
	// expected value (VR-C8 / FINDING-1). The expectation lives in the report's `expected`
	// field (test hat); embedding it here would leak the holdout into the product bundle.
	if len(codes) < len(expected) {
		return false, fmt.Sprintf("responder returned %d response(s) %v; a request did not fire", len(codes), codes)
	}
	for i, want := range expected {
		if codes[i] != want {
			return false, fmt.Sprintf("responder returned status %d (codes=%v)", codes[i], codes)
		}
	}
	return true, ""
}

// FormatDuration is a tiny helper for human output.
func FormatDuration(ms int) string { return fmt.Sprintf("%dms", ms) }
