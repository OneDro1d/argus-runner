# Spec 17 — Chain scenarios (the multi-step contract)

> **Status:** Implemented (0.3.29, VR10-S2 / V28-014) · **Owner:** Aleksander · **Phase:** Phase 1 · **Iteration:** 1 · **Last updated:** 2026-09-05

## Purpose

A **chain scenario** is one scenario made of several steps that the runner sequences itself in Go —
native MCP tool calls and vendored Playwright UI specs — threading ONE correlation id across all of
them and reporting a per-step verdict. It exists because a write → read-back-THAT-document → delete
flow against a SUT that mints ids server-side cannot live in one JMeter template.

This contract did not exist before 0.3.29. Four defects came out of the gap (fixture pinning, no
capture, the `;`-joined `expect`, and content assertions silently dropped). This file is the
authority for: the step shape, the two step types, `save` /
`${saved.<var>}` / `${cid}`, `target` vs `server_url`, and the per-step report shape.

The runtime is `internal/argus/chain_scenario.go` (parse + assemble), `internal/chain/chain.go`
(execute), `internal/mcp/judge.go` (judge one MCP step).

## The scenario file

A chain scenario is an ordinary argus scenario `.md` (spec 14 format) with:

- `- **Tags**: chain, …` — the `chain` tag selects this executor;
- a `## TRIGGER` whose payload is a JSON object `{"steps":[ … ]}` — the method/URL line is
  conventionally ``POST `chain` `` and is not used;
- a `## EXPECT` section that holds **the chain's claims** — one bullet per assertion, each naming
  the step it is about:

  ```
  ## EXPECT
  ### Runnable
  - step write-doc: result.isError == false
  - step read-back: body has id containing ${saved.docId}

  ### Non-runnable
  - one correlation id is threaded across every step
  ```

  ⛔ **CHANGED IN 0.3.31 (V30-002).** `## EXPECT` used to be documentation and the claims lived
  inside each step's `expect` key. **There were two places that looked like they held a scenario's
  claims and only one did**, and authors filled the decorative one: measured across the shipped
  catalogue, chain scenarios carried 127 EXPECT bullets, 38 of them correctly-formed
  `result.isError == …` assertions that nothing ever executed. A claim now has exactly one home.

  - `- step <name>: <assertion>` — `<name>` is a step's `name`, matched **exactly and
    case-sensitively**; `<assertion>` is any form in the grammar below.
  - **Several bullets naming the same step are ANDed.**
  - **Every `mcp` step needs at least one bullet.** A step nobody asserts anything about is a call
    whose answer is never read. (A `ui` step needs none — its verdict is the Playwright exit code.)
  - **A bullet naming no step is refused** (the orphan guard): renaming a step must not silently
    orphan its claim.
  - Prose about the test goes under `### Non-runnable`; a caveat about the requirement goes in
    `## References`.

  ⛔ **The `expect` key on a step is REMOVED (V31-002).** It is refused by `author__validate_scenario`,
  by `author__write_scenario` and by the control plane's write; the runner does not read it. A claim
  has one home, `## EXPECT` → `### Runnable`.

A chain with no `steps[]` fails preflight.

## Step shape

```json
{"steps":[
  {"type":"mcp","name":"write-doc","transport":"streamable-http","server_url":"${MCP_URL}",
   "tool":"memstore_write","args":{"collection":"c-${cid}","title":"t","content":"marker-${cid}"},
   "save":{"docId":"id"}},
  {"type":"mcp","name":"read-back","tool":"memstore_read",
   "args":{"document_id":"${saved.docId}"}},
  {"type":"ui","name":"see-it","spec":"tests/live/doc.spec.ts","app_url":"${APP_URL}"}
]}
```

…and its claims, in the same file:

```
## EXPECT
### Runnable
- step write-doc: result.isError == false
- step write-doc: body has id
- step read-back: result.isError == false
- step read-back: body has id containing ${saved.docId}
```

| field | steps | meaning |
|---|---|---|
| `type` | all | `"mcp"`, `"ui"`, `"http"` or `"amqp"` — the four step types. Anything else fails preflight naming the type. |
| `name` | all | The step's name in the report and in every failure message. |
| `transport` | mcp | `streamable-http` (default from `targets.mcp.transport`) or the legacy SSE transport. |
| `server_url` | mcp | An explicit, RESOLVED endpoint for this step. Falls back to `targets.mcp.base_url` when empty or when a `${VAR}` in it is not set. |
| `target` | mcp | The NAME of an entry under `targets.mcp_targets` in `argus-config.yaml` (VR10-S3, same release). `target` and a RESOLVED `server_url` both set → refused at preflight (a `${VAR}` placeholder left in `server_url` beside `target` is not an answer and is ignored, so shipped `"server_url":"${MCP_URL}"` steps keep working when a target is added); a name the config does not declare → refused by name; never a silent fall-back to the plain slot. A Metadata `**Target**` on a CHAIN scenario is refused, pointing at this step field. A host never lives in a scenario. |
| `tool` | mcp | The MCP tool to call. |
| `args` | mcp | The tool's arguments, as JSON. Kept RAW until run time so `${saved.<var>}` can be bound then. |
| `expect` | — | ⛔ **REMOVED (V31-002).** Refused by name wherever it appears; the claim lives in `## EXPECT` as `- step <name>: …`. |
| `save` | mcp, http | `{"<var>": "<path>"}` — what this step publishes for LATER steps (below). For `http`, the path reads the plain JSON response body (no MCP envelope to unwrap). `{"<var>": {"regex": "<one capture group>"}}` saves the first capture group of the first match in a text body instead (see "save from a text body"). |
| `spec` | ui | The vendored Playwright spec path (`${VAR}` resolved). |
| `app_url` | ui | The app URL; `APP_URL` from the environment when empty. |
| `method` | http | The HTTP verb (`GET`, `POST`, `DELETE`, …). Required — a missing `method` or `url` is refused by name at validate time. |
| `url` | http | The request URL. `${cid}` / `${cid8}` / `${correlation_id}` and an environment `${VAR}` resolve when the chain is parsed, exactly as an mcp step's `args` do; `${saved.<var>}` binds at RUN time, same as everywhere else. A url that STARTS with `${INGESTION_URL}` takes the scheme, host and port of `targets.http.base_url` in its place (its path is not added; a chain cannot select a named `http_targets` entry, so write the full url for another host). A `${NAME}` still unresolved fails the step at preflight, naming every such variable. |
| `headers` | http | `{"<name>": "<value>"}` — layered ON TOP of the scenario's own TRIGGER headers (which are the default for every step, mcp included); a step's own header wins on a name clash. A header value is never written into any report field — it cannot leak, by construction, not by redaction. An `Authorization` value of the form `Basic ${basic_auth:<username>:<PASSWORD_ENV_VAR>}` is the basic-auth helper (below) — never pre-encode a password yourself. |
| `body` | http | The request body, raw JSON. `${saved.<var>}` binds at run time here too. |
| `poll` | http | Optional `{"timeout":"<duration>","interval":"<duration>"}` (Go duration syntax, e.g. `"180s"`, `"5s"`). Re-sends the request, on `interval`, until the step's own claims pass or `timeout` elapses; the failure names the last observed status. A `poll` with no claim on the step is refused (V-AC-D20-2) — nothing would decide "pass" on any attempt. `timeout` over 10 minutes is refused (V-AC-D20-3). |
| `always` | http, mcp | Optional bool, default false (an `amqp` step refuses it). Marks a CLEANUP step: it fires even when an earlier step's `${saved.<var>}` was never captured (the `not-measured` gate, below, is waived for it) — the chain already runs every step after a failure regardless of this flag (see "What happens after a step fails"); `always` only changes whether THIS step is exempt from the never-fired gate when its own input was never produced. It ALSO fires after the chain has STOPPED because the SUT did not answer (unreachable or rate-limited), where every other later step is `not-measured` and unfired: a dropped connection mid-lifecycle must not leave a created resource behind. After a stop or a failure it is recorded as `ran-after-failure: ok|failed`, never as the named cause. If the reference really is missing, binding it still fails cleanly, naming the variable — the literal placeholder is never sent. |

### http step — a REST lifecycle no MCP tool fronts

The concrete case: a Coder workspace's create → poll-until-running → stop → delete lifecycle,
where the delete must run even if an earlier step failed, so nothing is left behind. `${cid8}` (an
8-lowercase-hex-char token derived from the correlation id — see the placeholders table below)
keeps the workspace name inside Coder's own `[a-z0-9-]`, ≤32-char bound.

```json
{"steps":[
  {"type":"http","name":"create","method":"POST","url":"${CODER_URL}/api/v2/users/me/workspaces",
   "headers":{"Coder-Session-Token":"${CODER_TOKEN}"},
   "body":{"name":"argus-${cid8}","template_id":"${TEMPLATE_ID}"},
   "save":{"wsId":"id"}},
  {"type":"http","name":"wait-running","method":"GET",
   "url":"${CODER_URL}/api/v2/workspaces/${saved.wsId}",
   "headers":{"Coder-Session-Token":"${CODER_TOKEN}"},
   "poll":{"timeout":"180s","interval":"5s"}},
  {"type":"http","name":"delete","method":"POST",
   "url":"${CODER_URL}/api/v2/workspaces/${saved.wsId}/builds",
   "headers":{"Coder-Session-Token":"${CODER_TOKEN}"},
   "body":{"transition":"delete"},
   "poll":{"timeout":"180s","interval":"5s"},
   "always":true}
]}
```

```
## EXPECT
### Runnable
- step create: status=201
- step create: body has id
- step wait-running: status=200
- step wait-running: body has latest_build.status containing running
- step delete: status=201
```

Coder deletes (and stops) a workspace by queueing a BUILD, not with `DELETE`: `POST …/builds` with
`{"transition":"delete"}` answers 201, and 409 while another build is still running — hence the
poll. A workspace is created under a user (`POST /api/v2/users/me/workspaces`), not at `/workspaces`.

An http step's claim is `status=<code>` and/or the SAME body-assertion grammar every other engine
uses (`body has …`, `body contains …`, `body matching …`, and the numeric comparisons below) — no
parallel assertion language. `status=<code>` alone (no `body …` bullet) means the body is not
judged; a `body …` bullet alone (no `status=`) means any status is accepted and only the body
decides. A body assertion's value resolves `${cid}` / `${cid8}` / `${correlation_id}` to THIS run's
own id, exactly like a check anywhere else (the placeholders table below) — an http step's claim is
not special-cased, and neither is an amqp step's (next section). An environment `${VAR}` is never
filled in here: refused when the scenario is written and, again, when the chain runs.

**`- step <name>: unreachable`.** The one claim whose expected answer is a transport
failure (a connection that must NOT work — a NetworkPolicy deny, a closed port). It passes ONLY when
NO HTTP response arrived because of a transport failure BEFORE any TCP connection existed: connect
timeout, connection refused, a reset at the connect, no route (one request, at most 5 s, never polled).
ANY HTTP status fails it — 403, 404 and 503 all mean the network let the request through — and so does
a DNS `no such host` (a typo must not read as a blocked path), and so does EVERY failure after a TCP
connection was established (no answer, a close, a reset, a reset inside an https target's TLS handshake):
the network let it through. The connection is marked at the dial, not after the TLS handshake. Rules:

- it stands alone on its step: combined with any other claim on the step (`status=…`, `body …`, a
  second `unreachable`) it is refused, with the same reason text at write time and at preflight;
- it refuses `poll` and `always` on its step, and it is an `http`-step claim only — on an `mcp`/`ui`/`amqp`
  step it is refused at write time (without that, the bullet has no assertion operator and would read
  as prose, proving nothing);
- expected unreachability does NOT stop the chain (VR12-CH2 stops it only for an UNexpected failure);
- ⛔ **false-green guard:** the step is judged only if an EARLIER POSITIVE step of the same chain (one
  that does not itself claim `unreachable`) has status `passed`. Otherwise it is `not-measured`, never
  fired, and the scenario does not pass — a dead target would otherwise pass every negative check. A
  chain with no positive step before the claim is refused at write time, naming the step.

Report: a pass lists `unreachable` in `assertions_enforced` and observes `no HTTP response arrived: <why>`;
a fail carries `failed_claims: [{"claim":"unreachable","observed":"http status 403"}]`. Executor-side:
the claim runs in the executor, so it needs an executor release that carries it.

**Numeric comparisons.** `body has <field> >|>=|<|<= <number>` — e.g. `- step check: body has
latency_ms < 400`. Always field-scoped (a JSON path in the body; there is no whole-response numeric
form). The observed value is parsed as a number and compared to the literal threshold; an absent
field, or one that is not a number, FAILS the check (never a pass, never a skip). A bad operator or
a threshold that is neither a number nor ONE whole `${saved.<var>}` is refused by name at validate
time, on every step type that carries the claim (`http`, `mcp`, `amqp` consume) — an `http` step's
claims are parsed at write time by the same parser the executor uses, so a claim the executor would
refuse at preflight is refused when the scenario is written, with the same reason text.

**A counter that must go up between two reads.** The threshold may be `${saved.<var>}`, bound from the
capture store when the step runs:

```json
{"steps":[
  {"type":"http","name":"before","method":"GET","url":"${APP_URL}/stats","save":{"n":"messages"}},
  {"type":"http","name":"act","method":"POST","url":"${APP_URL}/send","body":{"text":"hi"}},
  {"type":"http","name":"after","method":"GET","url":"${APP_URL}/stats",
   "poll":{"timeout":"30s","interval":"1s"}}
]}
```

```
### Runnable
- step before: status=200
- step act: status=202
- step after: body has messages > ${saved.n}
```

A variable that no EARLIER step saves is refused at write time, by name. If the step that should have
saved it did not run, the step is `not-measured` and names the variable. If the saved value is not a
number the step FAILS ("the saved value of `n` is not a number") — never a silent miss, never a pass —
and the value itself is never printed. `==` is not supported. A polled step binds again on every
attempt. The report's enforced-assertions list shows the claim as written (`${saved.n}`), not the
value. `${saved.<var>}` in any other claim value of an `http` step or an `amqp` consume step
(`body has id containing ${saved.x}`) is bound the same way an `mcp` step's is.

**`save` from a text body.** `"save":{"n":{"regex":"msgbus_deduped_total (\\d+)"}}` saves the FIRST
capture group of the FIRST match — for text such as Prometheus exposition, where there is no JSON
path. The regex must compile (Go RE2) and carry exactly one capture group, else it is refused when the
scenario is written, by name; no match fails the step ("save failed: the regex for `n` matched
nothing"). The string form (`"save":{"n":"messages"}`, a JSON path) is unchanged. On an `mcp` step the
regex reads the result's content text.

⚠ Numeric comparisons are evaluated in Go (the chain steps and an `mcp` scenario) — a plain HTTP
TRIGGER (JMeter-executed, no `chain`/`mcp`/`ui` tag) refuses a numeric comparison at authoring time,
because the JMeter template's assertion script does not understand the operator.

**Basic auth helper.** `"headers":{"Authorization":"Basic ${basic_auth:<username>:<PASSWORD_ENV_VAR>}"}`
— `<username>` is written in the clear (it is not the secret); `<PASSWORD_ENV_VAR>` NAMES an
environment variable, and the real `Basic <base64>` header is built by the executor, at run time, on
every attempt — never pre-encoded by the author. An env var that is not set refuses the step by
name, never an empty credential; a malformed form (no `:PASSWORD_ENV_VAR`, an empty username, or a
name that is not a valid environment-variable identifier) is refused at validate time. The same form
works in a plain TRIGGER's headers.

### amqp step — one broker operation, judged by the broker's answer (AC-D18b, extended P3 #24)

An `amqp` step performs ONE native AMQP operation against a RabbitMQ broker (through
`internal/amqpengine`) and is judged by what the broker did with it: accepted it, refused it with an
AMQP reply code, or — for `consume` — what (if anything) it delivered. The concrete case is a
permission probe of the msgbus bus: can the `argus-test` identity spoof another agent's `user_id`,
publish to a routing key it does not own, read another agent's queue, or declare a queue? Each of
those must be REFUSED, and the step proves it. `queue_delete`/`queue_unbind`/`exchange_delete` let a
check clean up what it declared.

> ⛔ **ONE PUBLISH PER STEP PER RUN.** A real agent's inbox can be on the other end of a publish. An
> `amqp` step may carry neither `poll` nor `always` (refused at validate time AND again at
> preflight). The runner has no retry loop for it, never re-fires it as a cleanup, and never
> re-fires a chain that contains one after a rate-limit (the run loop's single re-fire of a
> throttled scenario is skipped for such a chain; its row keeps its not-measured status). A publish
> is always publisher-confirmed (never fire-and-forget), so an asynchronous refusal is observed
> rather than lost.

```json
{"steps":[
  {"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",
   "exchange":"msgbus.inbox","routing_key":"inbox.agent.hop","user_id":"hop",
   "envelope":{"to_inbox":"agent.hop","from_agent_id":"argus-test","intent":"INFORM",
               "prompt":"argus-test: ${correlation_id}"}},
  {"type":"amqp","name":"read-hop","op":"consume","url_env":"MSGBUS_ARGUS_TEST_URL",
   "queue":"msgbus.agent.hop","wait":"5s"},
  {"type":"amqp","name":"declare","op":"declare_queue","url_env":"MSGBUS_ARGUS_TEST_URL",
   "queue":"msgbus.agent.probe"}
]}
```

```
## EXPECT
### Runnable
- step spoof: broker refuses with 406
- step read-hop: broker refuses with 403
- step declare: broker refuses with 403
```

| field | ops | meaning |
|---|---|---|
| `op` | all | `publish`, `consume`, `declare_queue`, `declare_exchange`, `queue_delete`, `queue_unbind` or `exchange_delete`. |
| `url_env` | all | REQUIRED. The NAME of the environment variable holding the broker URL — it must match `^[A-Z_][A-Z0-9_]*$`. A URL or any literal is refused (and never quoted back). The URL is read at run time, one connection per step, closed when the step ends, and never appears in any output, report, log or error (credentials are scrubbed from every broker text). A step field that references `${<its url_env>}` is refused. |
| `exchange` | publish, declare_exchange, queue_unbind, exchange_delete | The exchange (publish: `""` is the default exchange). |
| `routing_key` | publish, queue_unbind | Required on publish; required on queue_unbind (the binding to remove). |
| `user_id` | publish | Sets the AMQP `user-id` property — ONLY when given. A value other than the connection's own identity is what RabbitMQ refuses with 406. |
| `envelope` | publish | A msgbus v2 envelope: `to_inbox`, `from_agent_id`, `prompt` (required), `intent` (`INFORM` default, `ASK`, `DECISION_REQUEST`), `correlation_id` (optional; defaults to the run's correlation id). The runner fixes the rest: a fresh `messageId`, kind `MESSAGE`, source `MCP`, originTrust `AGENT`, reply `NoReply`, no refs, `enqueuedAt` = now (UTC). Sent Avro-encoded, `content-type: application/avro`, persistent, with the `x-envelope-schema` header, and the AMQP `correlation-id` = the envelope's. |
| `body` | publish | The alternative to `envelope`/`schema`+`record` — exactly one of the three. A JSON string is sent unquoted as `text/plain`; any other JSON value as its JSON text, `application/json`. |
| `schema` | publish, consume | A reference to an entry under the app's `message_schemas` (argus-config.yaml, below) — any app's wire format, not just msgbus's. On `publish` it is REQUIRED together with `record` (exactly one of `envelope`/`body`/`schema`+`record`, and `envelope` may itself carry a `record` too — an override, see below). On `consume` it is OPTIONAL: when present, the delivered body is decoded and the body grammar (`body has <field> …`) runs on its DECODED fields; without it, today's raw-bytes behaviour is unchanged. |
| `record` | publish (with `schema`, or with `envelope` as an override) | A JSON object in the schema's shape (the ONE JSON record rule below). Checked against the schema TWICE — when the chain is parsed (a placeholder stands for a valid value of its field's type) and again at run time after every placeholder resolves — refused BY FIELD PATH on a misfit, e.g. `record.enqueuedAt: missing (required, type long)`. Encoded to the schema's `format` (Avro only, this release); `content-type` and any declaration `headers` (message_schemas, below) are set automatically. |
| `headers` (P3 #24b) | publish | Custom AMQP message headers, layered onto any payload form. String values only — a non-string value is refused BY THE KEY'S NAME (e.g. `step "pub": publish header "x-retry-count" must be a string value`), never coerced. A schema declaration's own headers (e.g. `x-envelope-schema` for the `envelope` preset) are RESERVED and refused if named here. Resolves `${cid}`/`${correlation_id}`/env and `${saved.<var>}` exactly like every other string field. |
| `queue` | consume, declare_queue, queue_delete, queue_unbind | The queue. |
| `wait` | consume | Go duration, default `5s`, max `30s`: how long a consume waits for one message. A received message is acked; its body is read ONLY for a body assertion or `queue is empty` (below) — never consume from a queue whose messages matter otherwise. |
| `exchange_kind` | declare_exchange | `topic` (default), `direct`, `fanout` or `headers`. |
| `force` (P3 #24c) | queue_delete, queue_unbind, exchange_delete | Bypasses the declared-name guard below. Boolean, default false. |

Every string field (including a `headers` value) resolves `${cid}` / `${cid8}` / `${correlation_id}`
/ env when the chain is parsed, and `${saved.<var>}` at run time — exactly as for an `http` step. A
field an op does not take is refused by name.

**Schemas are app config, not Argus code (ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28).** The
app's `argus-config.yaml` declares each schema `record`/`schema` may reference:

```yaml
message_schemas:
  msgbus-envelope-v2:
    format: avro                             # only avro this release; protobuf/json later
    path: schemas/msgbus-envelope-v2.avsc   # relative to argus-config.yaml — or `inline: '{...}'`
    content_type: application/avro           # optional; default application/avro
    headers: { x-envelope-schema: "2" }       # set on every publish with this schema; a step may not override these keys
```

Exactly one of `path` or `inline`; the name matches `[a-z0-9][a-z0-9-]*`. `validate-config` parses
every declared schema and names the one that fails. `render-k8s` (and the compose render) embed
every `path:`-named file into the instance ConfigMap next to `argus-config.yaml`, at the same
relative path — rendering fails naming a missing or unparseable file. On compose, the same file
already reaches the executor for free: `docker-compose.byo-m3.yml` bind-mounts the whole product
directory (where `argus-config.yaml` lives) read-only at `/config`, not just the one file.

**`argus validate-scenario --config argus-config.yaml`** applies the FULL per-record schema check —
the same one a real run applies (`schema`+`record` -> the declared schema, checked by field path) —
locally, with nothing run: a record that does not fit its schema is refused by field path before any
scenario executes. Without `--config`, `validate-scenario` is unchanged (shape-only: one payload
form, `schema` names a declared-looking ref, `record` is an object, no secret-looking `${VAR}`) —
this is also all the control plane's `author_validate_scenario`/`author_write_scenario` can ever do,
since the app's `argus-config.yaml` lives only on the executor, never on the control plane. Run
`validate-scenario --config` locally before writing a `schema`+`record` step.

**The ONE JSON record rule** — for both writing a `record` and reading a decoded consume body:

| Avro type | JSON |
|---|---|
| null / boolean / string | `null` / `true` / `"…"` |
| int / long / float / double | a JSON number (or a whole-value placeholder, below) |
| bytes / fixed | a base64 string |
| enum | the symbol as a string |
| array / map / record | a JSON array / object |
| union `["null", T]` | `null`, or the bare `T` value — never tagged |
| union with 2+ non-null branches | tagged `{"<branch's full name>": value}`, e.g. `{"string": "x"}` |
| a field with a schema default | may be omitted from `record` |

**Placeholders in a record.** The usual `${cid}` / `${cid8}` / `${correlation_id}` / `${saved.<var>}`
/ env `${VAR}`, plus **`${uuid}`** (a fresh UUIDv4 per OCCURRENCE — two in one record are two
different ids) and **`${now}`** / **`${now_ms}`** (read ONCE PER STEP, UTC; `${now}` is RFC3339 with
milliseconds, `${now_ms}` is epoch milliseconds). A placeholder inside a longer string stays text and
is valid only in a string field; a JSON string that is EXACTLY one placeholder may stand in a
non-string field — it resolves and is parsed as that field's type (`"${now_ms}"` → long), and a
value that doesn't parse is a run-time refusal naming the field. **Claims are different:** a
`- step <name>: …` claim may use only `${cid}` / `${cid8}` / `${correlation_id}` / `${saved.<var>}` —
`${uuid}`, `${now}`/`${now_ms}` and env `${VAR}` are refused in a claim (its text is printed in the
report, and a fresh uuid there could never be matched).

**Safety.** Reports show the record TEMPLATE, never the resolved values. A `${VAR}` whose name
matches `(?i)(token|secret|password|passwd|key|credential|auth)` is refused in a `record` at both
authoring and run time, by name — its value is never read to make that decision. A credential
belongs in a header from the executor's own Secret, never in a message body.

**The `envelope` preset** keeps its exact syntax and behaviour (unchanged) — internally it is
`schema: msgbus-envelope-v2` plus a default record built from `to_inbox`/`from_agent_id`/`intent`/
`prompt`, with every OTHER field overridable through an optional `record` alongside `envelope`
(merged over the defaults at the top level): the motivating case is `"record":{"refs":["slack:…"]}`
for a bridge that must NOT fall back to a DM.

**Cleanup ops — `queue_delete`, `queue_unbind`, `exchange_delete` (P3 #24c).** A delete/unbind is a
write, so each refuses BY DEFAULT a queue/exchange name this SAME chain never declared with its own
`declare_queue`/`declare_exchange` step (in any position — the whole chain is known at validate
time): "a check must not be able to delete a live queue by typo." `force: true` on the step bypasses
this explicitly. `queue_unbind` checks BOTH the queue and the exchange it names. This guard is
UNCONDITIONAL — unlike the money-writes GET-only rule, it applies even against a SUT that is not
money-handling, because deleting broker topology is irreversible regardless of money.

```json
{"steps":[
  {"type":"amqp","name":"dq","op":"declare_queue","url_env":"MSGBUS_ARGUS_TEST_URL","queue":"argus-probe-q"},
  {"type":"amqp","name":"cleanup","op":"queue_delete","url_env":"MSGBUS_ARGUS_TEST_URL","queue":"argus-probe-q"}
]}
```
```
## EXPECT
### Runnable
- step dq: broker accepts
- step cleanup: broker accepts
```

**Claims.** Every op takes the same two broker forms:

- `- step <name>: broker accepts` — passes iff the broker accepted the operation (a confirmed
  publish, a declared/deleted/unbound queue or exchange, a consume that received a message within
  `wait`);
- `- step <name>: broker refuses with <code>` — `<code>` is a 3-digit AMQP reply code (403
  ACCESS_REFUSED, 404 NOT_FOUND, 406 PRECONDITION_FAILED); passes iff the broker did NOT accept and
  its reply code equals `<code>`.

**`consume` additionally takes two more claim forms (P3 #24a/d), because only consume ever has a
delivery to judge:**

- **a body assertion** — the identical grammar (and judge) an `http` step's `body …` claim uses:
  `body contains <value>`, `body equals <value>` (exact match — new, P3 #24a), `body matching
  <regex>`, or `body has <field> containing/matching/equals <value>` / `body has <field>` (exists),
  `<field>` a dotted JSON path into the delivered message. It implies `broker accepts` (a message
  must arrive before its content can be checked) — write it alone, or pair it explicitly with
  `- step <name>: broker accepts`. Combining it with `broker refuses with <code>` is refused: *"a
  body assertion requires the message to arrive; it cannot be combined with `broker refuses with
  <code>`"*. The value resolves `${cid}` / `${cid8}` / `${correlation_id}` to THIS run's own id —
  e.g. `- step consume: body contains "argus-lab NLB-001 ${cid8}"` is checked against the run's OWN
  derived token, not the literal text `${cid8}`. An environment `${VAR}` here is never filled in
  (a check is printed in the report): refused by name when the scenario is written, and again if a
  file somehow reaches a run without going through that check. With the step's `schema:` set, the
  field path is checked against the DECODED body (the ONE JSON record rule, above) — a body that
  fails to decode is its OWN failure, `delivered message does not decode as <schema>: <reason>`,
  never reported as a content mismatch.
- **`- step <name>: queue is empty`** — the ONE claim an EMPTY consume can PASS. `broker accepts`
  fails on an empty queue (no message arrived) and every `broker refuses with <code>` fails too (a
  timeout carries no reply code) — so before this there was no way to assert "the queue is empty" as
  the scenario's PASSING expectation. It passes when the `wait` times out with nothing delivered,
  and fails — showing what arrived (reality, never the claim's own text) — when a message does. It
  cannot be combined with a body assertion or a `broker …` claim: a genuine broker refusal (say 403
  on the queue itself) is a DIFFERENT failure from silence and still fails `queue is empty`.

Any other assertion on an amqp step is refused when the scenario is written — for example:

```
- step spoof: status=200                  ✗ "claim \"status=200\" is not an amqp-step claim"
- step spoof: broker refuses with 4x6     ✗ same — the code is three digits
- step spoof: broker accepts
- step spoof: broker refuses with 406     ✗ "claims … contradict — an amqp step takes exactly one"
- step pub: queue is empty                ✗ same generic message — `queue is empty`/a body assertion
                                             is consume-only; a publish/declare/delete step keeps the
                                             ORIGINAL two-form grammar unchanged
```

…and a `broker …` claim on an `mcp`/`http`/`ui` step is refused too. An amqp step with no claim is
refused by the empty-step guard. The step records its claim(s) in `assertions_enforced` (the body
assertion's rendered text is holdout material for the product hat, exactly like every other engine's
enforced-assertion text — VR10-S2); on a failure the report shows the observed reply code / what (if
anything) was delivered, and the broker's (redacted) text — never the asserted value (VR-C8). A
broker that cannot be reached at all is an execution error — the chain stops there, exactly as for an
unreachable SUT (VR12-CH2). An amqp step's claims are judged, so they are never listed as unexecuted
(AC-D23), and a chain made only of amqp steps needs neither `targets.mcp` nor `targets.http` in
`argus-config.yaml`.

> ⛔ **`## LOAD` does not drive an `amqp` step (P3 #24e, not implemented).** A `## LOAD` profile only
> ever feeds the JMeter thread group behind a plain HTTP TRIGGER (`internal/argus`'s `DeriveProps` —
> `load.users`/`load.ramp`/`load.duration`); `runChainScenario` never reads `s.Load` at all, so a
> chain (of any step type) ignores it entirely today, and there is no `rate` field to declare an AMQP
> publish/connection rate with. Driving load through an AMQP `publish` also runs straight into the
> one-publish-per-run safety rule above (a real agent's inbox may be on the other end) — so this is
> not a small addition: it needs its own opt-in, safety-reviewed load path (a Go connection/session
> pool in the executor, most likely, since no JMeter AMQP sampler is vendored in this repo), a new
> grammar for it, and new report/metrics plumbing. Rough size: several days, out of scope here.

## The assertion grammar

A claim's `<assertion>` — in a `- step <name>: <assertion>` bullet — is one of:

| bullet | meaning |
|---|---|
| `status=<code>` (http steps only) | the response's HTTP status code must equal `<code>` |
| `result.isError == false` | success expected: no JSON-RPC error and `result.isError:false` (the default) |
| `result.isError == true` | the tool plane: `result.isError:true` |
| `jsonrpc error == -32601` (or `protocol error …`, `error code …`) | the protocol plane; the code is optional (0 = any) |
| `body has <field>` | the answer is JSON and has the field `<field>`, not null (a dotted path such as `data.id` works) |
| `body has <field> containing <value>` | `<value>` appears in that field's value (a number, list or object as its JSON text); when the answer is not JSON, or has no such field, the check FAILS |
| `body has <field> matching <regex>` | the regex (Go syntax) matches that field's value; a regex that does not compile is refused at validate time |
| `body contains <value>` | `<value>` appears anywhere in the answer text, JSON or not |
| `body matching <regex>` | the regex matches anywhere in the answer text |
| `body has <field> >\|>=\|<\|<= <number>` (http steps and mcp scenarios only) | the field's value is parsed as a number and compared to `<number>`; absent or non-numeric FAILS. Refused at authoring time on a JMeter-executed (plain HTTP) scenario — its assertion script does not evaluate this operator |
| prose | a bullet with no assertion operator (`==`, `must`, `contains`, `matching`, `>`, `>=`, `<`, `<=`) next to a field name is documentation and changes nothing |

`body has <field> …` needs the answer to be JSON with that field; to match text anywhere in the answer, write
`body contains …`.

Rules:

- ⛔ **No `;` joining.** `- step s: result.isError == false; body contains X` (and the
  deprecated `"expect": "…; …"`) is refused at preflight and by both validators, and pointed at the
  one-bullet-per-assertion form. Nothing ever splits on `;` — a `;` inside a value or a regex would
  break it.
- **A bullet that looks like an assertion and is none of the forms above is refused** — by
  `author__validate_scenario`, by `argus validate-config`, and at run time as a preflight
  `failed` that quotes the bullet. It is never silently read as "expect success" (CR-1).
- **Content is judged after the plane.** A step that expects an error (`result.isError == true`, or a protocol
  error) has its `body …` checks judged too, once the error plane matches: on the tool plane against
  `result.content[*].text`, on the protocol plane against the JSON-RPC error object (`code`, `message`, `data`). A
  success step's checks are judged once both planes say success. On the protocol plane `body contains …` reads the
  error object as JSON text, so a quote or backslash in it appears escaped, as JSON writes it; check the error's
  stable fields instead (`body has code containing …`, `body has data.<field> …`).
- **The match is against `result.content[*].text`** (concatenated, in order) — or, for an expected protocol error,
  the JSON-RPC error object — and never the raw envelope, which echoes the request's id and `_meta.request_id`.
- **A content miss is `failed`, never `error`**: the SUT answered, the answer was wrong. `error`
  means no measurement was obtained (unreachable, transport failure).

## Variables — what resolves when

| form | resolved | from |
|---|---|---|
| `${VAR}` | when the chain is parsed, everywhere in the payload except inside a check — never in a claim | the environment; an unset `${VAR}` in `server_url`/`spec` means "not provided" |
| `${cid}`, `${correlation_id}` | when the chain is parsed: in the step args and in every check | this scenario's correlation id (`tr-<run>-<scenario>-<hex>`, one per scenario) — two names for one value; also injected as `_meta.request_id` on every call |
| `${cid8}` | when the chain is parsed: same places as `${cid}` | an 8-LOWERCASE-HEX-CHARACTER token derived from the correlation id (first 4 bytes of its sha256, hex-encoded) — deterministic, so a create step and a cleanup step never need to save it. For a resource name a SUT bounds tightly (a Coder workspace name: ≤32 chars, `[a-z0-9-]`), where the full `tr-<run>-<scenario>-<hex>` id routinely runs too long. |
| `${saved.<var>}` | at RUN time, per step, in `args` and in `body has` assertions alike — ⛔ **moving a claim from the step into `## EXPECT` does NOT change when it is bound** | the capture store: what earlier steps declared in `save`; a captured value may have more than one rendering — see the authoring rule below before asserting on it. |

`save`: `{"<var>": "<path>"}`. `<path>` is dot-separated into the step's response — `id`,
`doc.id`, `items.0.id` — read through `result.content[0].text` (parsed as JSON, the usual MCP
shape) and, failing that, against `result` itself. `save` binds **after a passing judge**: a
step whose assertion failed saves nothing. A `save` path that does not resolve fails the SAVING
step. A `${saved.<var>}` no earlier step saved fails THAT step naming the variable — in args and
in assertions alike; the literal placeholder is never sent to the SUT and never compared against
the answer.

The bound `expect` is a per-execution copy: re-running a chain never carries one run's captured
id into the next run's assertion.


### Placeholders — which one is filled in where

| written in | this scenario's correlation id: `${cid}` / `${correlation_id}` (`${cid8}` resolves alongside it everywhere it does) | an environment `${VAR}` |
|---|---|---|
| a step's `args`, the MCP TRIGGER payload, `server_url`, request headers, a ui step's `spec` / `app_url`, an http step's `method` / `url` / `headers` / `body` | filled in | filled in (a chain http `url` that STARTS with `${INGESTION_URL}` gets the host and port of `targets.http.base_url` instead, which is not an environment variable; `check_env` makes onboarding deliver a name to the executor, and any name present there is filled in) |
| the HTTP request path (the JMeter layers) | filled in | not filled in (a leading `${VAR}` stands for the base URL from `argus-config.yaml`) |
| the HTTP request body and header values (the JMeter layers) | filled in | filled in (an unset one stays literal and is reported) |
| a check under `### Runnable` (MCP, chain, HTTP and Database State — the engines that evaluate checks) — in a chain, a per-step claim (`- step <name>: <assertion>`) is a check TOO, for every step type (`mcp`, `http`, `amqp`) alike | filled in | **never** |
| `## VERIFY` SQL | `${correlation_id}` only — `${cid}` is refused | refused |
| `## CLEANUP` | filled in | filled in |

`${cid}` and `${correlation_id}` are two names for the same value — this scenario's correlation id; every scenario in
a run gets its own. In a chain, `${saved.<var>}` is also filled in — in a step's `args` and in its checks — at run
time, from what an earlier step saved. Check a marker the scenario itself wrote (`marker-${cid}`), never the bare
`${cid}`: the runner sends the id with every request (`X-Correlation-Id`, `_meta.request_id`), so an answer that
merely echoes it proves nothing.

An environment `${VAR}` is never filled in in a check: a check is printed in the report, so a secret would be printed
with it. A `${…}` in a check that nothing fills in — a typo such as `${cId}`, or an environment variable — is refused
when the scenario is written.

## Execution

Steps run IN ORDER, one correlation id across all of them. On the first non-passing step the
chain STOPS: that step is named in the scenario's failure, every later step is recorded as
`skipped` (never green), and the scenario is `failed` — or `error` when nothing reached the SUT
at all (the first step was unreachable).

## Per-step report shape

Each step is one entry of the scenario's `steps[]` in `report.json`:

```json
{"name":"read-back","status":"failed","correlation_id":"tr-3f2a…",
 "observed":"responder returned result.isError:false, no JSON-RPC error, but the result.content text did not satisfy the scenario's content assertion (the asserted value is held out; the test hat reads the claims that did not hold, with what was observed, in this step's failed_claims)",
 "mcp_envelope":{"jsonrpc":"2.0","id":"tr-3f2a…","result":{…}},
 "assertions_enforced":["content contains \"7d702c8b-…\""],
 "assertions_enforced_count":1,
 "failed_claims":[{"claim":"content contains \"7d702c8b-…\"","observed":"{\"items\":[]}"}]}
```

| field | when | hat |
|---|---|---|
| `name`, `status`, `observed`, `correlation_id` | always | both — `observed` is reality-only (VR-C8): it never echoes an asserted value |
| `status` | `passed` · `failed` (a negative response, incl. a content miss) · `error` (no response) · `not-measured` · `ran-after-failure: ok` · `ran-after-failure: failed` — see below | both |
| `mcp_envelope` | non-passing mcp steps only — the SUT's own reply, for triage | both (it is the SUT's text, not the scenario's) |
| `assertions_enforced` | steps whose content assertions were EVALUATED — success expected and both planes passed, or an error expected and its error plane matched (V31-005) — with the bound values | **test hat only** (it carries the expected value) |
| `assertions_enforced_count` | same steps | both — the ENFORCED marker a product-hat reader sees |
| `failed_claims` | a step that failed on its claims: one `{claim, observed}` per claim that did NOT hold — the claim as written (`${saved.<var>}` left unbound, never the saved value) and the value the SUT showed for that claim's field on the LAST attempt (a polled step: the last poll; a claim not scoped to a field: the whole answer). At most 10 entries, in the order the claims were written; each `observed` is cut to 200 bytes and ends `... (truncated)` when cut. A claim that held is not listed | **test hat only** (it carries the SUT's observed values) |

The scenario-level `failure.expected` (test hat only) carries the failing step's enforced
assertions; `failure.observed` names the step and its reality-only observed.

A green step with `assertions_enforced_count: 0` proved only its error plane — that the call did not error or, on a
step that expects an error, that the error came back.

### What happens after a step fails — ⛔ CHANGED IN 0.3.31 (V30-001)

The executor used to STOP at the first non-passing step and record every later step as `skipped`,
*"not run (a prior step failed)"*. The rule was **positional, not causal**: a step ran only if every
earlier step PASSED. **The steps that got discarded are usually the ones that undo what the test
created**, and they usually need nothing from the step that failed — `RACE-003`'s
`delete-namespace` names its target `race-s3-${cid}` from the correlation id, known before the
chain starts. Measured on the live pack: of six runs, the only two that left a namespace behind
were the two whose chain broke.

**`skipped` is retired from the chain path.** It named a choice we made; the truth was that we
could not.

| what happened | status | note |
|---|---|---|
| the step ran, before any failure | `passed` / `failed` | unchanged |
| the step ran AFTER the first failure | `ran-after-failure: ok` / `ran-after-failure: failed` | ⛔ **never `passed`** |
| its `${saved.…}` was never captured | `not-measured` | **never fired**; the reason names the variable |
| we stopped: the SUT was unreachable or rate-limited | `not-measured` | that step and every later one |

⛔ **Why a step that runs after a failure may never be green.** Once a step has failed the SUT is in
an unknown state, so a later step's POSITIVE claim is not trustworthy. **44 example steps assert an
ABSENCE** (`result.isError == true`) and would pass **vacuously**: `RACE-001`'s `verify-gone`
expects an error because the namespace should have been deleted — if `create-namespace` had failed,
the namespace never existed, the call errors, and the step would report **passed for a deletion
that never happened.** A false green is worse than a leak, because the leak is visible.

**The cost, stated rather than hidden.** Continuing means one bug can produce several red steps —
`RACE-002` declares no dependencies at all, so a failed `create-namespace` makes the write, the
search and the delete fail too. Four red steps, one bug. The report therefore **names the FIRST
failing step as the cause and counts the rest as echoes of it**, in `failure.observed`.

**And when something may be left behind, the run says so in ONE line** — on the terminal and in
`report.json` as the scenario's `residue`:
`<scenario> failed before its cleanup — step(s) X, Y did not complete, so anything this scenario
created may remain on the SUT`. It says **may** remain: the runner knows which steps did not
complete, not what the SUT holds.

### Assert that a deletion SUCCEEDED, not that the object is absent

A step meaning *"this was deleted"* must assert **the deletion succeeded** — not merely that the
object is now absent. *"No workspace"* is not the same thing as *"workspace deleted"*: **an absence
assertion also passes when the object was never created.** Assert on the delete call's own answer,
and keep any absence check as the second half of the claim, never the whole of it.

## Authoring rule — assert on stable values, never prose

| | assert on | why |
|---|---|---|
| ✅ | ids, markers the scenario itself wrote (`marker-${cid}`), field names, error codes, enum values | machine-generated, stable across releases |
| ⛔ | prose sentences (`"document not found"`) | phrasing changes on upgrade — a green that rots into a red for no product reason |
| ⚠ | a value with more than one legitimate rendering — a commit sha (full 40 hex or abbreviated 7-8), a timestamp, a number with a unit | the value is stable; its SPELLING is not. `containing` is a substring match, so `14a2149b…` (full) does not match `14a2149b` alone if the answer abbreviates, and vice versa |

**This holds for error tests too.** Check an error's stable fields — its code, a rule name, a field name — never its
message. When the error carries nothing stable (a plain sentence), check only the error plane — `result.isError ==
true` on MCP, the declared `status=` on HTTP — and record the message under `### Non-runnable`.

The `scenario-author` skill (spec 14) carries the same rule with a worked example.

> **Measure the answer before you assert on it.** Run the step once and read `result.content[*].text`, or call the tool
> directly. Assert the form the ANSWER carries — never the form some other tool (your own runner's evidence file, a UI, a
> log line) prints for the same value. This is not "avoid shas": a captured sha is a strong assertion when the answer
> carries it in full — measured on Memstore 2026-09-08, `memstore_history` returns full 40-char shas, so
> `body contains ${saved.sha2}` is correct there. It is "know which spelling you are asserting".

## Preflight refusals (the chain never runs)

- no `steps[]`; an unknown step `type`; an mcp step with no endpoint resolved; a ui step with an
  unresolved `spec`;
- a `- step <name>: …` bullet naming a step that does not exist (the orphan guard); an `mcp`,
  `http` or `amqp` step with no claim (the empty-step guard); a scenario declaring its claims in BOTH homes;
- an `expect` that joins bullets with `;`; an `expect` that is neither a string nor a list;
- an `expect` bullet that looks like an assertion and is not a known form; a `matching` regex
  that does not compile;
- (VR10-S3) `target` and `server_url` both set; a `target` name the config does not declare.

- (V31-002) a step carrying the removed `expect` key — refused when the scenario is written, never read at run time;

- (AC-D20) an `http` step with a missing or unresolved `method`/`url`; a `poll` block on a step
  with no claim (it would just spin for the whole timeout every run — nothing decides "pass" on
  any attempt); a `poll.timeout` or `poll.interval` that is not a valid duration; a `poll.timeout`
  over 10 minutes; a field an `http` step does not carry (the step shape is shared across
  `mcp`/`ui`/`http` — a stray `tool` left over from copy-pasting an mcp step is refused by name,
  never silently ignored).
- (AC-D18b, extended P3 #24) an `amqp` step carrying `poll` or `always`; a missing `url_env`, or one
  that is not an environment variable NAME (the value is never quoted back); an unknown `op`; a
  `publish` with both or neither of `envelope`/`body`, or no `routing_key`; an envelope with no
  `to_inbox`/`from_agent_id`/`prompt` or an `intent` outside INFORM/ASK/DECISION_REQUEST; a `publish`
  header whose value is not a string (refused BY THE KEY'S NAME) or one named `x-envelope-schema`
  (reserved); a consume `wait` over 30s; a field the step's op does not take; a claim on an amqp step
  that is not one of its legal forms (every op: the two `broker …` forms; `consume` also: `queue is
  empty` / a `body …` assertion), or two different broker claims on one step, or `queue is empty`/a
  body assertion on a non-consume op, or a `queue is empty` + body assertion / `broker refuses`
  contradiction; a `broker …` claim on a non-amqp step; an amqp step with no claim; a
  `queue_delete`/`queue_unbind`/`exchange_delete` naming a queue/exchange this chain never declared
  with its own `declare_queue`/`declare_exchange` step, without `force: true`.
- (V31-002) a scenario with no `### Runnable` bullet — refused when it is written and reported `error` when it runs,
  because nothing could be compared; a `ui` scenario satisfies the rule by naming its own spec file instead.

Each refusal names the step and quotes the offending value — except an amqp step's `url_env`, whose
value is never quoted (it may be a credentialed URL).
