# Spec 17 — Chain scenarios (the multi-step contract)

> **Status:** Implemented (0.3.29, VR10-S2 / V28-014) · **Phase:** Phase 1 · **Iteration:** 1 · **Last updated:** 2026-09-05

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
| `type` | all | `"mcp"`, `"ui"`, `"http"`, `"amqp"`, `"fault"`, `"db"` or `"logs"` — the seven step types. Anything else is refused naming the type, when the scenario is written and again at preflight (one sentence, one list: `scenario.ChainStepTypes`). A `db` or `logs` step needs executor 0.3.70 (see "db step" and "logs step" below). |
| `name` | all | The step's name in the report and in every failure message. |
| `transport` | mcp | `streamable-http` (default from `targets.mcp.transport`) or the legacy SSE transport. |
| `server_url` | mcp | An explicit, RESOLVED endpoint for this step. Falls back to `targets.mcp.base_url` when empty or when a `${VAR}` in it is not set. |
| `target` | mcp | The NAME of an entry under `targets.mcp_targets` in `argus-config.yaml` (VR10-S3, same release). `target` and a RESOLVED `server_url` both set → refused at preflight (a `${VAR}` placeholder left in `server_url` beside `target` is not an answer and is ignored, so shipped `"server_url":"${MCP_URL}"` steps keep working when a target is added); a name the config does not declare → refused by name; never a silent fall-back to the plain slot. A Metadata `**Target**` on a CHAIN scenario is refused, pointing at this step field. A host never lives in a scenario. |
| `tool` | mcp | The MCP tool to call. |
| `args` | mcp | The tool's arguments, as JSON. Kept RAW until run time so `${saved.<var>}` can be bound then. |
| `expect` | — | ⛔ **REMOVED (V31-002).** Refused by name wherever it appears; the claim lives in `## EXPECT` as `- step <name>: …`. |
| `save` | mcp, http | `{"<var>": "<path>"}` — what this step publishes for LATER steps (below). For `http`, the path reads the plain JSON response body (no MCP envelope to unwrap). `{"<var>": {"regex": "<one capture group>"}}` saves the first capture group of the first match in a text body instead (see "save from a text body"). `{"<var>": {"json": "<path>"}}` saves the value at the path AS JSON (an object, a list, a long number...), to be placed in a later `body` or `args` (see "save a JSON value as JSON"); `$` is the whole response. |
| `spec` | ui | The vendored Playwright spec path (`${VAR}` resolved). |
| `app_url` | ui | The app URL; `APP_URL` from the environment when empty. |
| `method` | http | The HTTP verb (`GET`, `POST`, `DELETE`, …). Required — a missing `method` or `url` is refused by name at validate time. |
| `url` | http | The request URL. `${cid}` / `${cid8}` / `${correlation_id}` and an environment `${VAR}` resolve when the chain is parsed, exactly as an mcp step's `args` do; `${saved.<var>}` binds at RUN time, same as everywhere else. A url that STARTS with `${INGESTION_URL}` takes the scheme, host and port of `targets.http.base_url` in its place (its path is not added; a chain cannot select a named `http_targets` entry, so write the full url for another host). A `${NAME}` still unresolved fails the step at preflight, naming every such variable. |
| `headers` | http | `{"<name>": "<value>"}` — layered ON TOP of the scenario's own TRIGGER headers (which are the default for every step, mcp included); a step's own header wins on a name clash. A header value is never written into any report field — it cannot leak, by construction, not by redaction. An `Authorization` value of the form `Basic ${basic_auth:<username>:<PASSWORD_ENV_VAR>}` is the basic-auth helper (below) — never pre-encode a password yourself. |
| `body` | http | The request body, raw JSON. `${saved.<var>}` binds at run time here too. The SUT receives the body's JSON text (`{"a":1}` is those seven bytes; the string `"hello"` is `"hello"`, quotes included) under `Content-Type: application/json`, which is sent by default when a body is present and the step's own headers (or the scenario's TRIGGER headers) set none; a header you set overrides that default. `null` (or no `body`) sends no body. A JSON STRING under a step header that declares a non-JSON Content-Type is sent as its TEXT, see "A text body" below. |
| `poll` | http, db, logs, mcp | (An `mcp` step also needs `"idempotent": true` and re-sends the tool call while the answer is `unavailable` or its claims miss; see "mcp step — answers a gateway wraps".) Optional `{"timeout":"<duration>","interval":"<duration>"}` (Go duration syntax, e.g. `"180s"`, `"5s"`). Re-sends the request, on `interval`, until the step's own claims pass or `timeout` elapses; the failure names the last observed status. A `poll` with no claim on the step is refused (V-AC-D20-2) — nothing would decide "pass" on any attempt. `timeout` over 10 minutes is refused (V-AC-D20-3). **A polled step is given room for its own poll:** the chain's whole-chain backstop is `## TIMEOUT` x step count PLUS the `poll.timeout` of every polled step (`http`, `db`, `logs` or `mcp`), so a 10m poll in a 3-step chain with `## TIMEOUT` 120s is not abandoned at 6 minutes and ends with the poll's own verdict (the last observed status). `## TIMEOUT` itself is still capped at 120s. |
| `extract` | mcp | Optional `{"path":"<dot path>","regex":"<one capture group>"}` (`path` optional). The first capture group of the first match in the answer's content text (or, with `path`, in the decoded string at that path) becomes THE DOCUMENT that the step's `body extracted …` claims and its `save` entries with `"from":"extracted"` read (JSON when it parses as JSON, else text). For a gateway that wraps data in a sentence; see "mcp step — answers a gateway wraps" below. Needs executor 0.3.70; an older executor refuses the check. |
| `unavailable` | mcp | Optional `{"matching":"<regex>"}`. An answer whose content text or JSON-RPC error text matches means "could not check": the step is `error`, never passed and never failed. No default pattern; you declare it. Needs executor 0.3.70: an older executor ignores `unavailable` and judges the claims on the outage text, so a step whose body claims are all negative can PASS there; a step that carries a `body extracted …` claim or a `from: extracted` save is refused by an older executor before any step fires, so pair `unavailable` with one of those, or with a positive data claim. Match only the gateway's outage markers (e.g. `internal server error`, `RPC request timed out`), not a generic `error`. |
| `idempotent` | mcp | Optional bool. Your statement that sending this tool call again is harmless; `poll` on an mcp step is refused without it. |
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

**A text body.** A JSON-STRING `body` on a step whose OWN `headers` declare a Content-Type
that is not JSON (not `application/json`, not `text/json`, not `…+json`; compared case-insensitively, parameters such as
`; charset=utf-8` ignored) is sent as the string's TEXT: unquoted, unescaped, under the header you set.
`"body":"a=1&b=two words"` with `"headers":{"Content-Type":"application/x-www-form-urlencoded"}` puts those
15 bytes on the wire. Every other combination is byte-identical to before: a JSON-string body with no
Content-Type header is still the quoted string under `application/json`; an object, array, number or boolean
body is its JSON text whatever the header says; a Content-Type set only in the scenario's TRIGGER headers
does not count (it is the default for every step and says nothing about this body). The empty string `""`
sends an empty body.
- **Placeholders.** In a text body `${cid}` / `${correlation_id}`, an environment `${VAR}` and
  `${saved.<var>}` (string form) are spliced as RAW text, not JSON-escaped: a saved `x"y\z` arrives as
  `x"y\z`. A variable saved with the `json` form is spliced as its JSON text. A `${saved.<var>}` no earlier
  step saved is refused by name and nothing is sent.
- **Money.** The money guard is shown a text body as a JSON string (as it always was for a string body), so a
  spending route listed under `money_writes` is REFUSED a text body, never sent, even text that reads as
  `{"amount": 5}`. A route that is not a spending one is sent the text.
- **Compare mode** records the RESPONSE of an http step; a request body, text or JSON, is never recorded.
- **Executor version.** An executor older than 0.3.70 does not know the text form and sends the quoted JSON
  string; that is today's behaviour, not a false green by itself, but the SUT will see a different body.

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
and the value itself is never printed. A polled step binds again on every
attempt. The report's enforced-assertions list shows the claim as written (`${saved.n}`), not the
value. `${saved.<var>}` in any other claim value of an `http` step or an `amqp` consume step
(`body has id containing ${saved.x}`) is bound the same way an `mcp` step's is.

**`save` from a text body.** `"save":{"n":{"regex":"msgbus_deduped_total (\\d+)"}}` saves the FIRST
capture group of the FIRST match — for text such as Prometheus exposition, where there is no JSON
path. The regex must compile (Go RE2) and carry exactly one capture group, else it is refused when the
scenario is written, by name; no match fails the step ("save failed: the regex for `n` matched
nothing"). The string form (`"save":{"n":"messages"}`, a JSON path) is unchanged. On an `mcp` step the
regex reads the result's content text.

**Save a JSON value as JSON** (needs executor 0.3.70 or newer, an older one refuses the
step by name before any step runs). The string form saves a scalar as TEXT; an object or a list is refused
and a number goes through a float64, so a long id can change. `"save":{"res":{"json":"resource"}}` saves
the value at the path as the JSON text it was written as, compact: an object, a list, a string WITH its
quotes, a number with every digit (`12345678901234567890` and `0.10` come out as written), `true`, `false`
or `null`. `{"json":"$"}` saves the whole response (for `mcp`, the document the string form's path is read
through). A path that does not resolve fails the step, as before; a saved value over 65536 bytes fails the
step, naming the variable, the size and the limit. The string form on a path that resolves to an object, a
list or `null` now says so, and names the `json` form.

A later step places it as a whole JSON value in its `body` (http) or `args` (mcp). The TRIGGER payload has to
parse before anything is bound, so write the reference as the string value it replaces:
`"body":{"update":"${saved.res}","ids":"${saved.ids}"}`. At run time the quotes go with the reference and the
value arrives unquoted, as a nested object or list. A string-form variable beside it is still escaped text.
When the body or args are a JSON document and are no longer one after binding (the reference sat inside a
longer string), the step is NOT fired and reads `not-measured`, naming the variable.

A JSON-saved variable is refused when you write the scenario, by name, in a `url`, in a header and in any claim
(`- step x: …`); the sentence says it can only be placed in a body or args and to save a scalar with the string
form for the other places. One variable has one kind: saved as text in one step and with the `json` form in
another is refused. The saved value is masked like any saved value (the placeholder `${saved.<var>}` in a failed
claim and in a recorded output, whatever layout the SUT echoed it in), is never written into a report, and a
body carrying it is judged by the money guard as it is really sent.

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

**The config's default auth is not sent by a chain step**. `targets.auth` — a static
`bearer_token` or `type: clerk_session` — and a named http target's `auth:` apply to a plain HTTP
TRIGGER (the JMeter path) only. A chain `http` step sends exactly the headers it and its TRIGGER
declare; a request that must carry the Clerk session identity is a plain-HTTP scenario. A scenario or step that sets its own `Authorization` always
wins over the config's default, and then no Clerk session is minted for it.

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
| `op` | all | `publish`, `consume`, `listen`, `declare_queue`, `declare_exchange`, `queue_bind`, `queue_delete`, `queue_unbind` or `exchange_delete`. An older executor (before 0.3.70) refuses a chain holding `listen` or `queue_bind` at preflight, naming the op (`unknown amqp op "listen"`), before its first step fires. |
| `url_env` | all | REQUIRED. The NAME of the environment variable holding the broker URL — it must match `^[A-Z_][A-Z0-9_]*$`. A URL or any literal is refused (and never quoted back). The URL is read at run time, one connection per step, closed when the step ends, and never appears in any output, report, log or error (credentials are scrubbed from every broker text). A step field that references `${<its url_env>}` is refused. |
| `exchange` | publish, listen, declare_exchange, queue_bind, queue_unbind, exchange_delete | The exchange (publish: `""` is the default exchange; `listen` and `queue_bind` refuse `""`, the default exchange cannot be bound). |
| `routing_key` | publish, listen, queue_bind, queue_unbind | Required on all four: the key published with, listened on, bound with, or the binding to remove. |
| `user_id` | publish | Sets the AMQP `user-id` property — ONLY when given. A value other than the connection's own identity is what RabbitMQ refuses with 406. |
| `envelope` | publish | A msgbus v2 envelope: `to_inbox`, `from_agent_id`, `prompt` (required), `intent` (`INFORM` default, `ASK`, `DECISION_REQUEST`), `correlation_id` (optional; defaults to the run's correlation id). The runner fixes the rest: a fresh `messageId`, kind `MESSAGE`, source `MCP`, originTrust `AGENT`, reply `NoReply`, no refs, `enqueuedAt` = now (UTC). Sent Avro-encoded, `content-type: application/avro`, persistent, with the `x-envelope-schema` header, and the AMQP `correlation-id` = the envelope's. |
| `body` | publish | The alternative to `envelope`/`schema`+`record` — exactly one of the three. A JSON string is sent unquoted as `text/plain`; any other JSON value as its JSON text, `application/json`. |
| `schema` | publish, consume | A reference to an entry under the app's `message_schemas` (argus-config.yaml, below) — any app's wire format, not just msgbus's. On `publish` it is REQUIRED together with `record` (exactly one of `envelope`/`body`/`schema`+`record`, and `envelope` may itself carry a `record` too — an override, see below). On `consume` it is OPTIONAL: when present, the delivered body is decoded and the body grammar (`body has <field> …`) runs on its DECODED fields; without it, today's raw-bytes behaviour is unchanged. |
| `record` | publish (with `schema`, or with `envelope` as an override) | A JSON object in the schema's shape (the ONE JSON record rule below). Checked against the schema TWICE — when the chain is parsed (a placeholder stands for a valid value of its field's type) and again at run time after every placeholder resolves — refused BY FIELD PATH on a misfit, e.g. `record.enqueuedAt: missing (required, type long)`. Encoded to the schema's `format` (Avro only, this release); `content-type` and any declaration `headers` (message_schemas, below) are set automatically. |
| `headers` (P3 #24b) | publish | Custom AMQP message headers, layered onto any payload form. String values only — a non-string value is refused BY THE KEY'S NAME (e.g. `step "pub": publish header "x-retry-count" must be a string value`), never coerced. A schema declaration's own headers (e.g. `x-envelope-schema` for the `envelope` preset) are RESERVED and refused if named here. Resolves `${cid}`/`${correlation_id}`/env and `${saved.<var>}` exactly like every other string field. |
| `queue` | consume, declare_queue, queue_bind, queue_delete, queue_unbind | The queue. Not on `listen`: Argus names that queue. A consume, queue_delete, queue_unbind or queue_bind may write `"${saved.q}"` (exactly that, nothing around it) for the queue a `listen` step of the same chain saved. |
| `wait` | consume | Go duration, default `5s`, max `30s`: how long a consume waits for one message. **Up to `120s` on a queue a `listen` step of this chain made** (`"queue": "${saved.q}"`); the 30 s bound stays for any other queue. A received message is acked; its body is read ONLY for a body assertion or `queue is empty` (below) — never consume from a queue whose messages matter otherwise. |
| `save` | listen | The only amqp op that saves: the answer is `{"queue": "<name>"}`, so `"save": {"q": "queue"}`. A listen that saves nothing is refused (the name is Argus's; nothing else could read the queue). |
| `decode` | consume | `"gzip"` gunzips the delivered body before `schema` and the body claims read it. Also AUTOMATIC when the delivery says `content-encoding: gzip`. Decompressed size is capped at 1 MiB (a larger body ends the step as `error`, named); a body that does not gunzip is a `failed` step saying so. `queue is empty` is judged before any decode. Only the exact value `gzip` (case and surrounding spaces ignored) triggers the automatic decode: a delivery that says `x-gzip` or lists several encodings is judged as delivered, so write `"decode": "gzip"` for it. ⚠ **Needs executor 0.3.70.** An older executor does not decode: it ignores `decode` and the `content-encoding` header and judges the compressed bytes. A positive body claim then fails, but a NEGATIVE one (`body not contains …`, `body not matching …`) holds for the wrong reason. Do not rely on a negative claim over a compressed message until the executor is 0.3.70 or newer. |
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

**Listen without taking — `listen`.** A `consume` on a queue that already exists takes the
message: it acks it and discards it, so on a live system it removes a message another consumer was owed. To
see the event a request caused without stealing it, `listen` first:

```json
{"type":"amqp","name":"tap","op":"listen","url_env":"BROKER_URL","exchange":"orders.events","routing_key":"order.created","save":{"q":"queue"}}
{"type":"amqp","name":"go","op":"publish", "...": "..."}
{"type":"amqp","name":"take","op":"consume","url_env":"BROKER_URL","queue":"${saved.q}","wait":"90s"}
```

`listen` declares a PRIVATE queue named by Argus (`argus-tap-<correlation id>-<step index>`, the same prefix as the
Message Flow tap, so one broker grant on `argus-tap-*` covers both), not durable, with `x-expires` and
`x-message-ttl` of 120000 ms, and binds it to the exchange and routing key in the same call. The broker then
delivers a COPY of every matching publish to it; the original consumers are untouched. The queue is deleted when the
chain ends (passed, failed, errored or abandoned); a delete that fails never changes the verdict, and the 120 s
expiry reaps the queue anyway. The wait arithmetic: the chain backstop is the `## TIMEOUT` (ceiling 120 s) times the
step count, so listen + http + consume(90 s) has 360 s, and each step is capped at the time the chain has left plus
5 s; a consume's own engine watchdog is its wait plus 30 s. A `consume` on a queue the chain did NOT create still
takes the message it reads, still waits at most 30 s, and is never made easier by a `listen`.

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
  on the queue itself) is a DIFFERENT failure from silence and still fails `queue is empty`. It
  passes ONLY on a genuine timeout on a healthy consumer: a consume that ended with
  no reply code for any other reason (consumer channel closed, watchdog teardown, connection error)
  is an `error` ("`queue is empty` could not be judged: …"), never a pass.

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

### fault step — break a connection or a deployment during the run

A `fault` step MAKES something fail so a later step can observe what the system does: a Deployment stopped, a
link slowed, cut or truncated. It changes the system under test, so what it may touch is the OPERATOR's
decision, in `argus-config.yaml` (top level, beside `targets:`), never the scenario's:

```yaml
fault_allowed_targets:
  deployments:
    trades: {namespace: calm-demo}            # allowlist name = the Deployment's own name
  links:
    mcp-to-trades: {toxiproxy_api: "http://toxiproxy.calm-demo:8474", proxy: trades}
```

Absent or empty = **no fault may run**. A namespace or address that looks shared or production (`prod`,
`production`, `prd`, `shared` as a segment) is refused when the config loads. A step naming anything not listed
refuses the WHOLE chain at preflight, before its first step fires, naming the entry — never the address.

| `op` | fields | what it does |
|---|---|---|
| `stop` | `deployment` | Scales the Deployment to 0. Records the replica count it found. |
| `start` | `deployment`, optional `replicas` 1..10 | Scales it back to the count the earlier `stop` recorded (or `replicas`). With no earlier `stop` in the chain, `replicas` is required and `start` is a `scale`. |
| `scale` | `deployment`, `replicas` 1..10 | Sets an explicit count. |
| `wait_ready` | `deployment`, optional `timeout` (default 120s, max 600s) | Waits until `readyReplicas == spec.replicas` and the controller has observed the latest spec. Changes nothing, journals nothing. For an HTTP health URL use an `http` step with `poll` instead — do not duplicate it here. |
| `delay` | `link`, `latency_ms` 1..60000, optional `jitter_ms` | A Toxiproxy `latency` toxic (downstream). |
| `truncate` | `link`, `bytes` 1..1048576 | A Toxiproxy `limit_data` toxic (downstream): the answer is cut after `bytes`. |
| `drop` | `link` | Disables the Toxiproxy proxy (`enabled:false`): connections are refused, like a stopped service. (A `timeout` toxic would hold connections open and hang instead; that is a different failure.) Re-enabled at restore ONLY if it was enabled before. |
| `restore` | — | Undoes every fault this chain applied so far, newest first. |

A fault step takes **no claim** (a `- step <name>:` bullet on it is refused at validate time and at run time):
it passes when the fault was applied and fails, by name, when it was refused or could not be applied. Put the
claim on the step that observes the effect. It may carry `always` (a `restore` that must fire after the SUT
stopped answering); it refuses `poll` and `save`.

**The restore always runs.** Every fault writes its undo to the executor's results volume
(`<results>/<instance>/fault-journal/`, fsync'd) BEFORE it is applied; if that write fails nothing is applied. At
the end of the chain, on EVERY path (passed, failed, errored, timed out, abandoned, panicked) whatever is still
active is undone newest first. If the executor dies mid-run, the next executor start undoes every pending entry
BEFORE its first poll (entries of a run still alive on another replica are left to it). The restore never
changes the verdict, tolerates "already restored", has a 30 s budget per undo and no off switch, and captures no
output. A restore that FAILS is loud: `restore_outcome: "failed"` on the run record, a `FAULT NOT RESTORED`
line on the row's `residue` and in the executor log, the undo stays journaled and is retried at the next start.
Argus-owned toxics are named `argus-<correlation id>-<n>`; restore deletes exactly those, never an operator's.

**On the record.** `report.json` gets `faults: [{step, op, target, params, applied_at, restored_at,
restore_outcome, toxiproxy}]` (also per scenario) - no URL, a link named by its allowlist name. A certifying run's
list (without `step`) is bound into the anchored `evidence_bundle_hash` and carried on a **v4** certificate; a run
with no fault keeps its v3/v2 certificate byte for byte. `toxiproxy: true` says a Toxiproxy was in the path.
Operator manifests, the Role to grant and a worked scenario: `docs/fault-steps.md`.

**On an instance that declares `money_handling`: refused unless its owner lists it.** With the switch on and no
entry, a chain with a fault step is refused (the guard's "only GET http steps may run" rule, plus a clause naming the entry), at
`validate-config`, at the run and at the control plane's write door; nothing is applied. The owner allows fault steps in the
system's own `money_writes.allow` with `{engine: fault}` (no other field). What a fault may touch is unchanged: the top-level
`fault_allowed_targets` list is still required and still checked before any step fires.

**The same switch and a `ui` step or scenario.** A `ui` chain step, and a `ui` scenario, is refused on a `money_handling` instance
unless the owner lists `{engine: ui, spec: <path>}` for exactly the spec it names (the payload `spec`, else the TRIGGER url;
exact string match; a spec built at run time never matches). The entry allows whatever that spec does in the browser: Argus
cannot bound a browser drive. The generic read-only spec is `tests/live/argus-declared.spec.ts`. A runnable `## CLEANUP` block is
refused there unless the owner lists `{engine: cleanup, form: sql|bash}`; the entry allows whatever a block of that form does.
An executor without these kinds refuses the config that holds them when it loads.

### Preconditions — a check that needs a mechanism the environment may not have

Not a step: two lines of `## Metadata`, on any scenario (a chain, an mcp or an http check). Reference: `docs/preconditions.md`.

```
- **Preconditions**: np-in=networkpolicy.ingress_enforced, np-out=networkpolicy.egress_enforced
- **Depends on**: np-in
```

| key | rule |
| --- | --- |
| `Preconditions` | `id=kind` pairs, comma-separated. `id` matches `^[a-z0-9][a-z0-9_-]{0,62}$` and is unique in the file. `kind` is a BUILT-IN from the closed list `scenario.PreconditionKinds` (`networkpolicy.ingress_enforced`, `networkpolicy.egress_enforced`); an unknown kind is refused by name with the nearest built-in. There is no author-supplied probe code. |
| `Depends on` | precondition ids, comma-separated, each declared in THIS file's `Preconditions`; an undeclared id is a validation error (and a second door at run time, `scenario.PreconditionProblems`). |

Both keys are in the file's bytes, so the set hash and the holdout root move when either does; a file with neither is
byte-identical to one written before the tier existed. The probe runs once per precondition, before any dependent check,
in the executor's own namespace. `holds` runs the dependents; `absent` ends each dependent `not_applicable` (not executed,
carrying the precondition id and both controls' observations); `broken` ends each `errored`. The run verdict: any failure or
error is `failed`; passed plus `not_applicable` is `passed`; every check `not_applicable` is `not_applicable`, never `passed`.
The results are bound by `evidence_bundle_hash` and carried by certificate v5. An executor older than the release that added
this ignores both keys and runs the check.

### db step — one read-only SELECT, its rows judged by `body …` claims

A `db` step lets a chain look at the system's database between steps: POST, then see the row. It needs executor
**0.3.70**; an older executor refuses the whole chain by name at preflight, before its first step fires.

```json
{"steps":[
  {"type":"http","name":"place","method":"POST","url":"${INGESTION_URL}/orders","body":{"sku":"A-1"},"save":{"oid":"id"}},
  {"type":"db","name":"row","query":"SELECT status, total FROM orders WHERE id = $1","params":["${saved.oid}"],
   "timeout":"5s","poll":{"timeout":"30s","interval":"2s"}}
]}
```

```
- step place: status=201
- step row: body has row_count == 1
- step row: body has rows.0.status equals PAID
```

| field | meaning |
|---|---|
| `query` | Required. ONE statement that begins with `SELECT` or `WITH`: no `;`-separated second statement, no `INSERT`/`UPDATE`/`DELETE`/`MERGE`/`TRUNCATE`/`DROP`/`ALTER`/`CREATE`/`GRANT` outside a literal or comment, a `FROM`, balanced parentheses and quotes — the same rules as a `## VERIFY` (one implementation), worded "the db step's query". No `${correlation_id}` and no time window are demanded. `${correlation_id}`, `${cid}` and `${cid8}` are filled in; **`${saved.<var>}` in the query text is refused** (a saved value is the system's own data and is never spliced into SQL: pass it through `params`), and so is any other `${…}`. The text reaches the server byte for byte. |
| `params` | Optional JSON array of strings, sent as bind parameters `$1…$n` (text; the server casts: `"42"` for an integer column). The query must use exactly `$1…$n`, each at least once. A param may carry `${saved.<var>}` (a text-saved variable; a `{"json": …}`-saved one is refused) and `${correlation_id}`. |
| `target` | Optional name under `targets.database_targets`; absent = `targets.database`. No database declared, or an unknown name, refuses the chain at preflight naming what is missing. |
| `timeout` | Optional Go duration, default `5s`, max `30s`: the server-side statement timeout AND the client deadline (also bounded by the chain's `## TIMEOUT`). |
| `max_rows` | Optional, default 100, 1..1000. The step reads at most `max_rows + 1` rows; **more than `max_rows` is an `error`** (not a pass on part of the answer): add a `LIMIT`, narrow the `WHERE`, or select `count(*)`. |
| `save`, `poll`, `always` | As on an `http` step (`poll` needs a claim and a `timeout` ≤ 10m); a db step's `poll.interval` under `1s` is refused (a check must not hammer a database). Each attempt re-runs the query in a fresh read-only transaction on the step's one connection. |

**The claims** are the existing `body …` forms, judged against ONE document:
`{"row_count": <n>, "rows": [ {"<column>": <value>, …}, … ]}` — a dotted path indexes an array element
(`rows.0.status`), `body every rows has status not equals FAILED` judges every row. Column names are as the server
returns them (a duplicate name in the result is an `error` naming it: alias it). Values: integers and `numeric` keep
EVERY digit (`==` and `body contains` read the digits as written; the older text forms `equals`/`containing`
compare a number as a float, as they do on any answer), NULL is `null`, text/varchar/uuid are strings,
timestamp/timestamptz/date are RFC 3339 strings in UTC, json/jsonb are the JSON value itself, and bytea and every
other type are the server's text form. `status=…`, `elapsed`, `unreachable` and `broker …` are refused on a db step
by name. A db step with no claim is refused (the minimum is `body has row_count >= 1`). The whole document is capped
at 1 MiB (over it is an `error`).

**Outcomes.** Claims hold → `passed` (`db query returned <n> row(s)`). Claims miss → `failed` with `failed_claims`
(author-only); the builder-visible text carries the row count only — never a claimed value, a row value or the
query. Cannot connect, authentication refused, or the deadline passed → `error` (never retried by a poll; the chain
stops there, the step keeps its own sentence). The server refuses the statement (unknown table, permission denied,
statement timeout, a write refused by the read-only transaction) → `failed`, `database query did not run
(SQLSTATE <code>)`, never "no rows" and never retried by a poll. The builder-visible text carries the SQLSTATE
only: the server's own message quotes the query and can quote a bound parameter, so it goes to the author alone, in
`body_excerpt` (credentials and saved values redacted, cut to 512 bytes).

**Read-only, three times.** The authoring validator and the executor's preflight refuse a query that could write;
the executor then runs it inside `BEGIN READ ONLY` with the statement timeout set for that transaction and always
rolls back, so PostgreSQL itself refuses a write that was smuggled past the first two. **The real guarantee is the
role:** point the target at a READ-ONLY role (see `onboarding/HOW-TO-ARGUS-CONFIG.md`). A read-only transaction
stops writes to tables and sequences (`SELECT … INTO`, `nextval()` are refused by the server, measured); it does
NOT stop a function with a side effect outside the data (`pg_terminate_backend`, `pg_cancel_backend`, an advisory
lock, a session `set_config`). Whether the role may call those is a grant on the database, not something Argus can
see in a SELECT.

**PostgreSQL only.** The target's `jdbc_url` must be `jdbc:postgresql://host[:port]/db[?params]`; another dialect is
refused at preflight (`## VERIFY` on a plain scenario covers the JDBC dialects). Of the URL parameters only
`sslmode`, `ssl`, `currentSchema` and `ApplicationName` are read; any other is refused by name rather than dropped.
Credentials come from the target's `username`/`password`, never from the URL.

**On an instance that declares `money_handling`: refused unless its owner lists it.** With the switch on and no
entry, a chain with a db step is refused (the guard's "only GET http steps may run" rule), at `validate-config`, at
the run and at the control plane's write door; no connection is opened. The owner allows it in the system's own
`money_writes.allow`: `{engine: db}` for `targets.database`, `{engine: db, target: <name>}` for a named target
. Nothing else about the step changes.

**Compare mode.** Only `http` steps record an output. A chain with a db step still runs and its claims still judge,
but the db step's rows are not recorded or compared, and a `**Steps**` entry naming a db step is refused.

### logs step — does a log line exist, not exist, or how many, in the instance's own Loki

A `logs` step lets a chain look at the system's logs between steps: "after this request the service logged X"
(`body has count >= 1`) and "nothing logged an ERROR for this run" (`body has count == 0`). It needs executor
**0.3.70**; an older executor refuses the whole chain by name at preflight, before its first step fires.

```json
{"steps":[
  {"type":"http","name":"place","method":"POST","url":"${INGESTION_URL}/orders","body":{"sku":"A-1"},"save":{"oid":"id"}},
  {"type":"logs","name":"logged","scope":"correlation","contains":"order ${saved.oid} accepted","poll":{"timeout":"30s","interval":"2s"}},
  {"type":"logs","name":"quiet","scope":"correlation","matching":"level=(error|fatal)"}
]}
```

```
- step place: status=201
- step logged: body has count >= 1
- step quiet: body has count == 0
```

| field | meaning |
|---|---|
| `scope` | Required. `correlation`: the lines carrying THIS scenario's correlation id, in every stream but the observability plane's own (the query the failure triage uses). `source`: the lines of the streams `labels` select. |
| `labels` | `source` only (refused on `correlation`; at least one). A JSON object of label name to a literal string value, matched by equality: no regex matcher, no negation. A name matches `[A-Za-z_][A-Za-z0-9_]*`; a value is not empty and carries no `${…}`. Rendered as `name="value"` with the values quoted, names sorted. |
| `contains` | Optional literal line filter. `${cid}`/`${correlation_id}` (the run's own id) and `${saved.<var>}` (a text-saved variable; a `{"json": …}`-saved one is refused) are filled in, always as a quoted literal, never spliced raw. A value that binds to nothing is not queried. |
| `matching` | Optional RE2 line filter (a literal pattern: no `${…}`), compiled when the scenario is written. |
| | A `source` step needs at least one of `contains` / `matching` (a step may not count a whole stream). Both may be set; a line must satisfy both. |
| `window` | Optional Go duration, max `1h`: how far back from the moment of the query to look. Default: back to one minute before the chain started. The end is always "now". |
| `settle` | Optional Go duration, default `2s`, max `60s`: waited before the FIRST query. **A log line is shipped after the request that caused it returns, so `settle` is what stands between "the line does not exist" and "the line has not arrived yet".** For a line that may take longer, add a `poll` (a `count == 0` claim passes on the first attempt that sees none). |
| `save`, `poll`, `always` | As on an `http` step; a logs step's `poll.interval` under `1s` is refused. |

**The claims** are the existing `body …` forms, judged against ONE document:
`{"count": <n>, "lines": ["<line>", …]}` — `count` is the number of matching lines, `lines` holds the first 20 in
time order, each cut at 2048 bytes. `body has count >= 1` (a line exists), `body has count == 0` (none exists),
`body has count <= 3`, `body has lines.0 containing …`. `status=…`, `elapsed`, `unreachable` and `broker …` are
refused on a logs step by name; a logs step with no claim is refused (the minimum is `body has count >= 1`).

**A count is never a silent lower bound.** The query asks for at most 2000 lines. A read that returns 2000 may have
been cut by Loki, so the step is an `error` (never retried by a poll) telling the author to narrow `contains` /
`matching` or `window`.

**Outcomes.** Claims hold → `passed` (`log query matched <n> line(s)`). Claims miss → `failed` with `failed_claims`
(author-only); the builder-visible text carries the count only — never a line, a claimed value, `contains` or
`matching`. Loki unreachable, a non-200 answer, or an answer that cannot be read → `error`: **an unreachable log
store never makes `count == 0` hold**, and a poll does not retry it. The lines are the system's own words: each
kept line passes the credential scrub and the token redaction (the run's own id is held out of it) before it is kept,
and reaches an author only.

**Only the executor's own Loki.** A scenario cannot name a URL: the step reads the Loki the executor was started
with (`--loki`, `--loki-tenant`, and the `observability.loki.credential` when it belongs to that Loki). The chain is
refused at preflight, by name, when the executor has none (`--obs none`, an empty `--loki`) and when the instance's
config selects BetterStack ("a logs step reads Loki only for now"). Which `--obs` modes carry logs, and what must be
switched on for the system's own pod logs to be there: `docs/DEPLOY-ARGUS.md` §2.3a.

**On an instance that declares `money_handling`: refused unless its owner lists it.** With the switch on and no
entry, a chain with a logs step is refused (the guard's "only GET http steps may run" rule), at `validate-config`,
at the run and at the control plane's write door; no query is sent. The owner allows it in the system's own
`money_writes.allow` with `{engine: logs}`.

**Compare mode.** Only `http` steps record an output. A chain with a logs step still runs and its claims still
judge, but the logs step is not recorded or compared, and a `**Steps**` entry naming one is refused.

### mcp step — answers a gateway wraps, "could not check", and polling

Needs executor 0.3.70. **A claim on the extracted document is written `body extracted …`, and an executor older than
0.3.70 refuses a check that has one instead of running it.** Such an executor reads a line that opens with `body` as a
claim about the answer, has no form for `body extracted …`, and refuses the whole chain by name before it sends anything
(`error`, never a pass). So the check can never be judged on the raw answer, where the data's words could also stand in
the gateway's own sentences. The same is true of a `save` entry with `"from": "extracted"` (an older executor accepts a save
entry with exactly one key). `unavailable` and `poll` need no such door, but an executor that ignores them is not harmless: an older executor ignores `unavailable` and judges the claims on the outage text, so a step whose body claims are all negative can PASS there; a step that carries a `body extracted …` claim or a `from: extracted` save is refused by an older executor before any step fires, so pair `unavailable` with one of those, or with a positive data claim. An older executor also does not retry a `poll`.

**Writing the `extract` regex.** When the answer can hold more than one block, a greedy `(.*)` spans all of them and gives a mixed document on which `contains` claims can both hold. Use a bracket-anchored or lazy form such as `(.*?)`. A capture that is empty or only whitespace counts as an answer without the shape: the step is `failed`, and nothing is judged or saved.

**Writing `unavailable.matching`.** Match only the gateway's outage markers (e.g. `internal server error`, `RPC request timed out`), not a generic `"error"`. A generic pattern turns a real SQL mistake (a missing table) into "could not check" and also matches a successful row whose value is the word error.

A database gateway answers with a sentence, the rows as a JSON array between `<untrusted-data-<uuid>>` and
`</untrusted-data-<uuid>>` (the uuid changes on every call), and another sentence. A tester reports the real hub puts all of
it in ONE string: the content text is a JSON object whose `result` field is that string. A claim on a row cannot read
either. `extract` cuts the rows out; claims written `body extracted …` then judge them.

```json
{"type":"mcp","name":"paid-orders","target":"hub","tool":"execute_sql",
 "args":{"query":"SELECT id, status FROM orders WHERE note = '${correlation_id}'"},
 "extract":{"path":"result","regex":"(?s)<untrusted-data-[0-9a-f-]+>\\s*(\\[.*\\])\\s*</untrusted-data-"},
 "unavailable":{"matching":"database_unavailable|internal server error"},
 "poll":{"timeout":"2m","interval":"5s"}, "idempotent":true,
 "save":{"firstId":{"json":"0.id","from":"extracted"},"rows":{"json":"$","from":"extracted"}}}
```
```
- step paid-orders: result.isError == false
- step paid-orders: body extracted has 0.status equals PAID
- step paid-orders: body extracted every $ has status equals PAID
```

(When the answer's text is plain text, not a JSON object, leave `path` out: `examples/gateway-answers/GW-002-…`.)

- **`extract`.** `{"regex":"…"}` or `{"path":"…","regex":"…"}`. The regex is Go RE2, no flags added (write `(?s)` when the
  data spans lines, as the example does), with exactly ONE capture group (refused otherwise, as a `save` regex is).
  Without `path`: the first group of the first match in the answer's content text (every `content[].text`, joined by
  newlines) is THE DOCUMENT. **With `path`** (the dot-path syntax claims and `save` use; non-empty, no `${saved.`): the
  content text is parsed as JSON, the value at `path` must be a JSON STRING, and the regex runs on that string's
  DECODED value. This is the case where the rows sit inside a JSON string: in the raw text their quotes are escaped
  (`\"`) and a regex over the raw text captures something that is not JSON. Content that is not JSON, a missing path, or a
  value that is not a string is `failed` with its own sentence (the answer did not have the shape the check expected).
  The document is parsed as JSON when it is valid JSON (so `body extracted has 0.status …`, `body extracted every $ has
  …` and `{"json":"$","from":"extracted"}` work on it), else judged as text. The plane claims (`result.isError == false` …)
  are judged on the real envelope first, as always. No match → `failed` with its own sentence; the first 512 bytes of the
  answer (of the decoded string, when the regex found nothing in it) go in the step's `body_excerpt` (author only). A
  capture over 1 MiB is an `error`.
- **Which text a claim reads.** `body extracted <form>` is any `body <form>` (`contains`, `not contains`, `matches`,
  `has 0.status equals PAID`, `has no <field>`, `has n == 3`, `has total within 2% of …`, `every $ has …`, …) judged on the
  extracted document. A plain `body <form>` on the same step judges the answer's own text, exactly as on a step without
  `extract`. A `save` entry reads the extracted document only with `"from": "extracted"` (`{"json":"<path>","from":"extracted"}`
  or `{"regex":"<one group>","from":"extracted"}`); without `from` it reads the answer, as on any step. `from` accepts only
  `extracted`.
- **Declared and read together.** A `body extracted …` claim or a `from: extracted` save on a step WITHOUT `extract` is
  refused, and a step WITH `extract` that has neither is refused ("extract is declared but nothing reads it"). `body
  extracted …` is a claim of an mcp chain step only.
- **`unavailable`.** `{"matching":"<regex>"}`. If the content text, or the JSON-RPC error object (code, message, data),
  matches, the step is `error` ("could not check"): it is judged BEFORE the plane and the claims, so a claim cannot turn
  it into a pass or a fail, and the chain stops there as it does for an unreachable system. The sentence says that the
  declared pattern matched; it carries neither the pattern, nor the answer, nor a claim. The answer's start is in the
  step's `body_excerpt` (author only). There is NO default pattern: nothing is "unavailable" unless you wrote it.
- **`poll`.** Same block and bounds as the db step (a claim is required, `timeout` ≤ 10m, `interval` ≥ 1s). The call is
  sent again while the answer is `unavailable` or the claims (plane or body, or the `extract` shape) miss; it stops at the
  first pass, or at the deadline, where the last outcome stands (`error` if it was still unavailable, `failed` if a claim
  missed). An unreachable system, a transport failure and a declared rate limit are not retried by it. **Every attempt SENDS
  THE TOOL CALL AGAIN, so `poll` needs `"idempotent": true`** (your statement that calling the tool again is harmless);
  without it the step is refused when the scenario is written and again at preflight, by name. On a `money_handling` instance
  the `money_writes.allow` entry for the tool is what permits the call at all, as it does for any mcp step.
- **Stray fields** on an mcp step are refused by name (an mcp step takes: type, name, transport, target, server_url, tool,
  args, save, always, extract, unavailable, poll, idempotent).
- **Redirects.** On a run of a `money_handling` instance the mcp client does not follow an HTTP redirect (a 3xx is
  the transport failure it is); any other run is unchanged.
- **Reports.** The builder sees the sentences above and the step's status. The answer, the pattern and the claims go only
  into the author's `body_excerpt` and `failed_claims`.

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
| `body has <field> == <number>` | numeric equality: `1`, `1.0` and `1e0` are equal; never text. `<number>` is a plain decimal or one whole `${saved.<var>}`. `=>` and `=<` are refused by name |
| `body has <field> within <tol> of <number>` | `\|observed - number\| <= <tol>`, `<tol>` a non-negative number (absolute) or `N%` (relative: `\|number\| * N / 100`); a reference of 0 with `%` holds only when observed is exactly 0. Compared exactly (decimal arithmetic, not float64). Absent or non-numeric FAILS |
| `body not contains <value>` / `body not matching <regex>` | the whole answer text does not contain the value / the regex does not match |
| `body has <field> not containing <value>` / `not matching <regex>` / `not equals <value>` | the field is present and its value does not contain / match / equal. ⛔ An answer that is not JSON, or a field that is ABSENT or null, FAILS these: "the field does not contain X" never passes because the field is not there |
| `body has no <field>` | the answer IS JSON and the path does not resolve, or resolves to JSON null. An answer that is not JSON FAILS (it cannot be evaluated) |
| `body every <array-path> has <field> <rest>` | every element of the JSON array at `<array-path>` (`$` = the whole answer) satisfies `<field> <rest>`, where `<rest>` is any field form above or existing (nothing = exists; `containing\|matching\|equals`; the `not` forms; the numeric ops; `==`; `within`; `body every <array-path> has no <field>`). `<field>` is read inside each element; `.` alone is the element itself (arrays of scalars). A path that does not resolve to an array FAILS. **An EMPTY array HOLDS** (no element breaks the claim): pair it with a claim that the list is not empty when that matters. A miss names the index of the FIRST breaking element and its value (author-only) |
| prose | a bullet with no assertion operator (`==`, `must`, `contains`, `matching`, `>`, `>=`, `<`, `<=`) next to a field name is documentation and changes nothing |

The forms (`==`, `within`, the negatives, `has no`, `every`) are judged in Go only, like the other numeric comparisons: a JMeter-executed (plain HTTP) scenario refuses every one of them at authoring time, naming them. They apply to `http` and `mcp` chain steps, `mcp` scenarios and an `amqp` consume step's `body` claims.

**`elapsed` (`http` steps only).** `- step <name>: elapsed < <N>ms` or `elapsed <= <N>ms` (also `<N> ms` and `<N>s`; N a positive whole number, at most 600000 ms; anything else is refused by name). It measures the step's ONE HTTP request: from just before the request is sent to the moment the whole response body has been read. With `poll` it judges the LAST attempt (the one whose claims are judged), never the poll's total. Refused by name beside `unreachable` on the same step (no response, nothing to time), on any step that is not `http`, and with two bounds on one step. A miss is `failed`, never `error`, and is not reported as a timeout. Every `http` step that got a response records `elapsed_ms` in the report, pass or fail; it is the system's own behaviour and both hats read it. The bound is an expected value: the builder-visible text says only `the response took <N> ms and a timing claim did not hold`; `took <N> ms, bound <M> ms` is in the author-only `failed_claims`.

**A response body over 1 MiB is not judged (`http` steps).** An `http` step reads at most 1 MiB (1048576 bytes) of a response body. When the body is longer and the step has a `body …` claim of any form or a `save`, the step ends `error` ("could not check") with a sentence naming the limit, on the first attempt that sees it (a `poll` does not go on), and nothing is saved. A step whose claims are `status=…` and `elapsed` only still passes on a large download, and a missed `status=…` claim is still `failed`. A body of exactly 1 MiB is whole.

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
the answer. `{"json": "<path>"}` is the form that keeps an object, a list or an exact number as JSON
(above): it is placed only in a body or args.

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
| `assertions_enforced` | steps whose content assertions were EVALUATED — success expected and both planes passed, or an error expected and its error plane matched (V31-005) — each claim AS WRITTEN: `${saved.<var>}` is shown unbound, never as the saved value, on every step type; `${cid}` / `${correlation_id}` are this run's id and are shown resolved | **test hat only** (it carries the expected value) |
| `assertions_enforced_count` | same steps | both — the ENFORCED marker a product-hat reader sees |
| `failed_claims` | a step that failed on its claims: one `{claim, observed}` per claim that did NOT hold — the claim as written (`${saved.<var>}` left unbound, never the saved value) and the value the SUT showed for that claim's field on the LAST attempt (a polled step: the last poll; a claim not scoped to a field: the whole answer). EVERY claim that did not hold is listed, in the order the claims were written, up to a backstop of 100 entries (`failed_claims_omitted` counts any beyond it); each `observed` is cut to 200 bytes and ends `... (truncated)` when cut. A claim that held is not listed | **test hat only** (it carries the SUT's observed values) |
| `body_excerpt` | an `http` step that ended `failed` because its STATUS claim missed: at most 512 bytes of the response body (a polled step: the last attempt's), cut on a UTF-8 boundary and ending `... (truncated)` when cut; `(<N> bytes, not text)` for a body that is not UTF-8 text, `(empty body)` for none. The instance's credential scrub, then every saved value, are applied BEFORE the cut, so a secret straddling the cut is still redacted. Absent on a pass, on a body-claim miss (the failed claim shows the observed value) and on a transport error (no body). The step's `observed` for the same miss is `http responded status <got>; the status claim did not hold`: the wanted status is an expected value and is only in `failed_claims` (`status = <want>`). Plain (JMeter-run) http scenarios are out of this: the Go side never sees their body, and the `.jmx` templates are unchanged | **test hat only** (it carries the SUT's own words) |
| `failed_claims_omitted` | how many claims did NOT hold on the same attempt `failed_claims` came from but are not listed because the list reached its backstop of 100; absent when the list is complete. Never part of the step's `observed` text | **test hat only** (how many hidden claims failed is holdout material) |
| `observed_status` (AC-F2.1) | an `http` step: the HTTP status code of the response it judged, on a pass and on a fail (also a save that failed, a body over the limit, and the response that made an `unreachable` step fail). With `poll`, the status of the attempt whose claims were judged (the last one). Never the expected status. Only a three-digit code, 100-999; anything else an app answers (`000`, or any integer over HTTP/2) is not written. Absent when the step judged no response: a transport error (a redirect chain that does not end is one), the chain's `## TIMEOUT`, a money-path refusal (a refused redirect included), a binding error | both — the system's own behaviour; `author_get_report_limited` sends it |
| `observed_sqlstate` (AC-F2.1) | a `db` step whose query ended with a database error (refused, or stopped while it ran, e.g. `57014` statement timeout): its SQLSTATE (e.g. `42P01`, no such table), only when it is exactly 5 characters of `0-9` / `A-Z`. Never the database's message (that is `body_excerpt`). Absent on a pass, a claim failure, and when the database could not be reached | both — the system's own behaviour; `author_get_report_limited` sends it |

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
- a `db` step: no `query`; a query that is not one read-only statement (a `;` second statement,
  not beginning with `SELECT`/`WITH`, a mutating keyword, no `FROM`); a `${saved.<var>}` or any other `${…}` in the
  query text; a `params` that is not an array of strings, or a count that does not match `$1…$n`; a JSON-saved
  variable in a param; a `timeout` over 30s; a `max_rows` outside 1..1000; a `poll.interval` under 1s; a field a db
  step does not carry; a claim that is not a `body …` form; no claim; no `targets.database` (or an unknown `target`
  name); a target whose `jdbc_url` is not PostgreSQL or carries a URL parameter the step does not map. The refusal
  leaves the whole chain unrun: no earlier step has fired and no connection was opened.
- a `logs` step: no `scope`, or one that is neither `correlation` nor `source`; `labels` on a
  `correlation` step, or none on a `source` step; a label name outside `[A-Za-z_][A-Za-z0-9_]*`, a label value that
  is not a string, is empty or carries `${…}`; a `source` step with neither `contains` nor `matching`; an empty
  `contains` or `matching`; a `matching` that does not compile as RE2 or carries `${…}`; a `${…}` in `contains` other
  than `${cid}`, `${correlation_id}` and `${saved.<var>}` (a JSON-saved one is refused too); a `window` over `1h`; a
  `settle` over `60s`; a `poll.interval` under 1s; a field a logs step does not carry (a `url` included); a claim
  that is not a `body …` form; no claim; an executor with no Loki (`--obs none`, an empty `--loki`); an instance whose
  config selects BetterStack. The refusal leaves the whole chain unrun: no earlier step has fired and Loki was not asked.
- an `mcp` step: a field it does not carry (named, with the list of those it takes); an `extract`
  without a `regex`, one that does not compile or does not have exactly one capture group, one with an empty `path` or a
  `path` naming `${saved.…}`; an `extract` that nothing reads (no `body extracted …` claim, no `from: extracted` save), or a
  `body extracted …` claim or `from: extracted` save on a step with no `extract`; a `from` other than `extracted`, or on a step
  that is not an mcp step; an `unavailable` without a
  `matching` or one that does not compile; a `poll` without `"idempotent": true`, with no claim, with a timeout over 10m
  or an interval under 1s. The refusal leaves the whole chain unrun and nothing is sent.
- (V31-002) a scenario with no `### Runnable` bullet — refused when it is written and reported `error` when it runs,
  because nothing could be compared; a `ui` scenario satisfies the rule by naming its own spec file instead.

Each refusal names the step and quotes the offending value — except an amqp step's `url_env`, whose
value is never quoted (it may be a credentialed URL).
