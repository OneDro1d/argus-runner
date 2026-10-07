# How to write your `argus-config.yaml`

The **one** file you fill to test your service with the Argus MCP. One config = one SUT
(single-tenant). It has two halves: an **argus-owned** half (what to test) and a **argus-owned** half
(where your SUT's logs live). Copy `onboarding/argus-config.template.yaml`, fill it, done.

> **Before you start:** your service must be **compatible** — see [COMPATIBLE-SUT.md](../docs/COMPATIBLE-SUT.md)
> (structured JSON logs with `correlation_id` + `level`, a saga per control action, docker-compose deployable,
> a read-only DB role). And it must be **running in its own docker compose project/network** before you onboard.

## THE ONE RULE (read this twice)
Every URL is written **from the JMeter runner's network vantage** — i.e. your SUT's compose **service name**
and its **in-container port**, NOT `localhost:<published-port>`. The onboarder joins the runner to your SUT's
docker network, so `http://order-api:8080` resolves; `http://localhost:8080` does **not**.

## MANDATORY FOR ONBOARDING — supply these or onboarding stops

Most of this file is "declare what you actually test": **omit any layer you don't test** and nothing
breaks. That advice is true of the **argus-owned** half (the `targets` block) and it does **not**
apply to everything below it, which is what this section exists to say plainly.

These are required, and onboarding **refuses** without them rather than guessing:

| What | Where | Why it cannot be defaulted |
|---|---|---|
| **Grafana URL for the tier you are onboarding to** | `observability.grafana.public_url.<tier>` | The right value differs per environment. A default would be right on compose and produce a dead link on k8s — silently, because a wrong link looks exactly like a right one until somebody clicks it. |
| **The four log-field mappings** | `observability.loki` | Every service names these differently; a guess produces empty Logs and Saga panels that read as "your SUT emitted nothing". |
| **`deploy.compose_project` / `deploy.network`** | `deploy` | Without them the obs stack cannot find a SUT it was never told about. |
| **A value for every `${VAR}` you reference** | your product folder's `.env` | Sending an empty credential is worse than failing. |

Everything else may be omitted when it does not apply to your SUT.

## Step 1 — find your SUT's compose project + network
```bash
docker compose ls          # → your project NAME (e.g. order-service)
docker network ls          # → your network   (usually <project>_default, e.g. order-service_default)
```
These two values go in the `deploy:` block.

## Step 2 — find your service names + in-container ports
```bash
docker compose -p <your-project> ps   # the SERVICE names (left column) = the hostnames you use
```
Use the **container** port (the right side of a `8080:8080` mapping, or the `EXPOSE`/`PORT` your service listens on),
not the published host port. (OrderService: `order-api` listens on `8080`, `webhook-mock` on `8081`.)

## Step 3 — fill the argus-owned targets (only the layers you actually test)
- **`http.base_url`** — your HTTP edge: `http://<service>:<port>` (required if you have HTTP Ingestion / Error Path / Rate Limiting / Permissions scenarios).
- **`database`** — for Database State scenarios. Use a **READ-ONLY** credential (the suite must never write). `jdbc_url: jdbc:postgresql://<db-service>:5432/<db>`; write the `password` as a `${VAR}` (see **Secrets & credentials** below). If your table is in a non-public schema, write VERIFY queries fully-qualified (`SELECT … FROM <schema>.<table> WHERE correlation_id = '${correlation_id}'`).
- **`message_broker`** — for Message Flow scenarios: the AMQP `url` (write embedded creds as `${VAR}`) + `management_url` + your `exchanges`/`queues` (incl. the `dlq` for poison/negative tests) + **`routing_keys: {incoming: <key>}`** — the routing key your service publishes with. ⚠ Since 0.3.32 a scenario that asserts what a message CONTAINS reads it through a tap queue bound with that key; without it every such check is refused before it runs (reported `error`, naming `routing_keys.incoming`), and the broker user needs `configure` + `write` + `read` on `argus-tap-*`, `read` on the incoming exchange and a management-capable tag such as `monitoring` (see *What can be a SUT*, SOFT 16).
- **`external`** — for External Delivery scenarios: a sink that records outbound webhooks; write it as `webhook_base_url: http://<sink-service>:<port>`.
- **`mcp`** — for MCP-server SUTs: `base_url` as `<service-name>:<port>/mcp` from the runner's vantage; `transport`: `streamable-http` | `http-sse` (declared, never guessed); `auth`: `none` | `bearer` (`bearer_token` as a `${VAR}`).
- **`auth`** — a valid test bearer token so the suite can call authenticated endpoints, written as a `${VAR}` (see **Secrets & credentials** below).
- **`scenarios`** — `timeout_default`. ⚠ `cleanup_enabled` was REMOVED in 0.3.31 (V29-015): a
  scenario's `## CLEANUP` now always runs, on every path, and there is no switch to turn it off.
  **If your config still carries the key, delete it.** It is *not* refused — strictness covers
  `targets` (VR10-S3), `rate_limit` (VR10-R1), `package` (AC-36), `money_writes`,
  `message_schemas`, `load_allowed_targets` (AMQP Load; HTTP Load), `test_targets` (UI-6,
), `observability.openshell` (spec 26) and `observability.pushgateway`
, not `scenarios` (a top-level list such as `check_env` has no inner keys to be strict
  about; its entries are checked as names) — so it parses and
  reaches nothing, which is exactly the condition this change was made to end. Measured, not
  assumed: `internal/config/cleanup_enabled_leftover_test.go` (the strict members are
  `strictTargetsDoc` in `internal/config/config.go`). Confluence 691699714 says the same.

Omit any layer you don't test — `validate-config` only checks that every layer your scenarios reference has a usable target.

## AMQP Load (v0.3.52+): which broker, which names, which permissions

An `AMQP Load` scenario (`**Target**: <name>`) loads ONE named broker, and only one the operator has
listed at the top level under `load_allowed_targets` (never under `targets:`). Nothing else allows load.

```yaml
targets:
  message_broker_targets:
    lab-load:
      type: amqp
      url: amqp://${LAB_LOAD_USER}:${LAB_LOAD_PASSWORD}@my-lab-rabbit:5672/%2F   # write the vhost out: a URL ending in "/" is the EMPTY vhost to the Java client
      exchanges:
        load: perf.x          # optional; the exchange each session queue binds to. Default amq.direct. It must EXIST (Argus declares no exchange)
      queues:
        load: perf.argus      # optional; the session queue name prefix. Default argus-load
load_allowed_targets:
  lab-load:
    max_sessions: 1000        # the largest step a scenario may ask for (default 2000)
```

Each session declares and binds its own queue `<queues.load>-<run id>-s<step>-<n>` (with an `x-expires`, and
deletes it at the end), publishes to `exchanges.load`, and consumes from its queue. So the load login needs,
on the vhost: **configure, write and read on queues matching `^<prefix>-`** and **write and read on the
exchange**. A least-privilege load login (one name pattern, say `^perf\..*`) works by choosing names inside
it, `exchanges.load: perf.x` and `queues.load: perf.argus` in the example. Without them every session is
refused at setup with `403 ACCESS_REFUSED`, and the run reports `setup_failed` for the step.

When the broker blocks publishers (a memory or disk alarm), the step reads `blocked` with the broker's reason and
the run fails "blocked by broker … at step N", even if the executor then had to stop a JMeter process the block
kept alive (v0.3.55+; before that the row read as an Argus error). Latencies read about 1 ms higher than
RabbitMQ PerfTest at low load (p50 ~4 ms vs ~3 ms in the msgbus lab): JMeter thread scheduling and timestamping
inside the executor pod, not the broker. Compare throughput directly; compare latency against Argus's own baseline.

### Where the numbers show (control plane with)

The run's own page and drawer show a **Load steps** table (step, sessions, offered and delivered msg/s,
publish-to-deliver p50/p95/p99, errors, status, blocked reason, duration, restarts, not ready); a value the step
did not measure reads "not measured", never 0. The **Capacity** page (`/capacity`, also the Overview's capacity
card) shows the measured limit of the LATEST completed load run for that system and target: the largest step
that is comfortable with every smaller step comfortable too, the date and the version it was measured on, a
chart of delivered throughput by step with the comfortable zone shaded, and "now" when the environment reports
a `summary_metrics` reading in `sessions` or `agents`. The limit goes amber when the version the latest monitor
run recorded differs from the one the load run recorded, and reads "unknown" (also amber) when either run
recorded none. The version is recorded **by the executor itself, per Kubernetes namespace** (decision
): at the start of every run, of every mode, it reads the image digests running in each namespace
the run's checks belong to (a target's `namespace`, else `ARGUS_SUT_NAMESPACE`) and keeps a key over them. Two
targets in one namespace (`lab` and `lab-load` in `msgbus-lab`) are compared with each other; `live` in another
namespace is never compared with them. This needs no key in `argus-config.yaml` and the read Role described in
`docs/runbooks/TEST-TARGETS.md` for each namespace. A `deployment_probe` is the **optional other way**: when a
load run recorded no key for the namespace, the probe's fingerprint is used exactly as before. A load run you asked for as
expected to fail is not a capacity measurement and is never the one shown. When the latest load run found no
limit (no step was comfortable), the page says so and shows the last limit an older run measured beside it,
with its date, never as the current limit. A control plane older than this
shows "not available on this control plane", not "not measured yet".

A system with several `test_targets` can mark one that must never be loaded, such as the real message bus, with
`load_test: never` on its entry (the only accepted value; leave the key out otherwise). It is a guard:
`validate-config` reports an error for a load check (an AMQP Load check, or any check with a `## LOAD` section)
that belongs to it, the executor refuses to fire one, and the Capacity page says "<target> is not load tested, by
declaration." instead of "not measured yet". ⛔ Upgrade the executor to the release that carries this key BEFORE
you add it: `test_targets` is strict, and an executor built from `v0.3.59` or earlier refuses the whole config
with `unknown key "load_test" under test_targets`. Runbook: `docs/runbooks/TEST-TARGETS.md`.

A target whose system does not run in a Kubernetes cluster (a hosted app behind a CDN) can say so with
`outside_cluster: true` on its entry (leave the key out otherwise; refused together with `namespace` on the same
entry). A load run's `environment` then reads `captured: false` with `not applicable: the target of these load
checks is declared outside the cluster`, instead of `forbidden: pods in namespace ...`, which reads like a missing
permission. Nothing is asked of Kubernetes for such a target. Same upgrade rule as `load_test`: upgrade the
executor first, because `test_targets` is strict. Runbook: `docs/runbooks/TEST-TARGETS.md`.

## HTTP Load (v0.3.66+): a stepped HTTP ramp, the same gate

An `HTTP Load` scenario (`**Target**: <name>`) ramps users against ONE named http target, and only
one the operator has listed under the same top-level `load_allowed_targets` the AMQP ramp uses. The entry names a
`targets.http_targets` entry; `max_sessions` caps the users of a step. An entry that names only a broker does not
allow an HTTP ramp at a same-named host, and a `base_url` that looks shared or production (`prod`, `production`,
`prd`, `shared` as a part of the host or path) is refused when the config is loaded.

```yaml
targets:
  http_targets:
    api-lab:
      base_url: http://orders-api.lab.svc:8080   # a lab or dedicated test deployment, never a live one
load_allowed_targets:
  api-lab:
    max_sessions: 200        # the largest step (users) a scenario may ask for (default 2000)
```

Each step is one JMeter run of the `http-ingestion` template with the scenario's own request (its TRIGGER, its
headers, `targets.auth`), `Steps` users looping back to back, each keeping one connection alive, for `Step Duration
Seconds`. The ramp **stops at the first step that is not comfortable** (its response p95 above `Target P95 Ms`, or
more than `Max Error Rate` of its requests answered 400 or more or not at all): nothing is sent after that step's
hold, and the step is named (`stopped_at_step`). The per-step record is the AMQP ramp's (`load_ramp`, driver `http`,
`sessions` = users, `delivered_per_s` = requests served below 400 a second, `response_us` = request-to-response
time, `errors` by class `status:<code>` / `timeout` / `transport` and `error_reasons`), and the Capacity page reads it
exactly as it reads an AMQP ramp, in users. A set holding an HTTP Load scenario is not queued for `final`,
`scheduled` or `rehearsal`, nor for an executor below 0.3.66; an older executor refuses the layer by name. The
authoring reference and a worked example: `skills/scenario-author/SKILL.md`, "HTTP Load".

**Upgrade the executor first.** An executor before 0.3.66 accepts only `targets.message_broker_targets` names
under `load_allowed_targets`, and it checks that block when it loads the config: an entry naming an http target
makes it refuse the WHOLE config (`load_allowed_targets.<name>: no targets.message_broker_targets.<name> is
declared`), not just the HTTP Load scenario. Add the http entry only once every executor reading this config is on
0.3.66 or later.

## Secrets & credentials (REQUIRED reading)

There are **two different kinds of "key"**, and they live in two different places. Getting this right is
what keeps the suite generic and your secrets out of git.

**1. A credential the SUITE needs to reach YOUR SUT** — a bearer token for your HTTP/MCP endpoint, a
read-only DB password, AMQP creds. These belong in `argus-config.yaml`, but **always as a `${VAR}`
placeholder, never a literal**:

- **THE NAMING RULE: the `${VAR}` name must be the variable's name in YOUR product folder's `.env`.**
  The onboarder resolves every referenced `${VAR}` from **`<product-dir>/.env`** — the same env file your
  SUT already uses — so if your `.env` says `TEST_USER_STATIC_TOKEN=…`, write
  `bearer_token: ${TEST_USER_STATIC_TOKEN}`. Zero copying: the value stays in one file, yours.
  (No such variable yet? Pick a name, add `NAME=value` to `<product-dir>/.env`, reference `${NAME}`.)
- Write the placeholder in the config: `bearer_token: ${MY_SUT_BEARER_TOKEN}`, `password: ${MY_SUT_DB_PASSWORD}`,
  `url: amqp://${MY_SUT_BROKER_USER}:${MY_SUT_BROKER_PASSWORD}@my-rabbit:5672/`. `${VAR}` resolves anywhere in a string.
- **How loading works (secrets preflight):** before anything runs, the onboarder scans your config for the
  referenced names, loads **only those** from `<product-dir>/.env` (never the whole file as env vars — other
  keys in your `.env` are not injected into the suite), shows a masked preview of each (length plus the
  first/last few characters — 4 for long values, 2 for shorter ones, length-only under 12 chars; never the
  value, and only when you're there to confirm interactively) and asks you to confirm. A referenced name
  **missing or empty** in your `.env` stops onboarding right there, telling you which names to add. Standard
  dotenv syntax is understood (`export NAME=…`, quoted values, inline comments, CRLF). On a **direct host
  run** (running `argus` yourself) a shell `export MY_SUT_BEARER_TOKEN=…` supplies the value instead.
- **Scan scope (exact):** `${VAR}`s are scanned/resolved in `targets.http.base_url`,
  `targets.auth.bearer_token`, `targets.mcp.base_url` + `targets.mcp.auth.bearer_token`,
  `targets.database.username`/`password`/`jdbc_url`, and `targets.message_broker.url` +
  `management_url` — and, since 0.3.29 (VR10-S3), the same fields inside every NAMED entry under
  `targets.http_targets.<name>`, `targets.mcp_targets.<name>`, `targets.database_targets.<name>` and
  `targets.message_broker_targets.<name>` (a SUT with several endpoints of one kind; a scenario picks one with
  `**Target**: <name>`). Do not put `${VAR}`s in other fields (e.g. `external.webhook_base_url`) — they would
  reach the runtime as literal text.
- **A secret only a check uses — `check_env`.** The scan above reads only the fields just listed. A password
  that appears only inside a check (an HTTP body, a chain step's header) has none of them, so declare its NAME in
  a top-level list, beside `targets:` and never under it:

  ```yaml
  check_env:
    - SOME_PASSWORD        # the check writes ${SOME_PASSWORD}; the value lives in <product-dir>/.env
  ```

  Names only, never values: an entry that is not an environment-variable name (letters A-Z, digits,
  underscore, not starting with a digit) is refused without being printed back, so a pasted `NAME=value` does
  not leak. Names that would change how the executor process itself runs are refused too (the refusal names the
  entry's position and the rule, never the entry): `ARGUS_*`, `LD_*`, `KUBERNETES_*`, `PATH`, `HOME`, `USER`,
  `SHELL`, `PWD`, `HOSTNAME`, `TMPDIR`, `JAVA_TOOL_OPTIONS`, `JAVA_HOME`, `GODEBUG`, `SSL_CERT_FILE`,
  `SSL_CERT_DIR`, `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`. Up to 64; a name listed twice, or already referenced by a
  credential field, counts once. A declared name travels the same way a credential does on every tier:
  `argus secrets-scan` lists it (so the compose onboarder loads it from `<product-dir>/.env`), `render-k8s`
  puts it in the instance Secret on k3d and aks, and `argus secrets set --key SOME_PASSWORD` rotates it later.
  A declared name missing or empty in your `.env` stops onboarding by name, and fails `validate-config`, exactly
  as a missing credential does. `validate-config` echoes the declared names (never values) under `check_env`.
  ⚠ **`check_env` decides what is DELIVERED to the executor, not what a check may use.** Any name present in the
  executor's environment is substituted for a `${NAME}` in a chain step or a plain check, declared or not (and that
  includes `ARGUS_*` names); a name that is unset or empty is not refused in a body or header, it is sent as the
  literal text `${NAME}` (only a chain url refuses it). Open question, not decided.
  ⚠ **A value is data in a chain spec.** It is placed inside the JSON as text, so a quote, a backslash or a
  newline in it arrives unchanged and cannot add a field; a value written where JSON wants a number or a
  boolean must itself be one, or the step fails with an error that does not show the value.
  ⚠ **Upgrade the executor first.** An executor built before this key does not refuse it, it ignores it: the
  name is then never scanned or delivered, and a `${SOME_PASSWORD}` in a check body reaches the SUT as literal
  text. (This is the top-level-key behaviour `load_allowed_targets` relies on.)
- **Mount note (dev/local):** the suite bind-mounts your product folder read-only into its own containers
  (for the config + the holdout guard), so your `.env` file is visible to the suite's containers on your
  machine — the preflight limits what is *injected as environment*, not what the folder mount exposes. This
  is a dev/local tool: never point `--product-dir` at a folder holding production credentials.
- **Never** put the value in the committed config. The committed file holds only the `${VAR}` name, so it is
  safe to commit (and the spec-20 lint blocks any committed secret).
- **A referenced `${VAR}` that is unset — or set to an empty string — FAILS `validate-config` loudly**, naming
  the missing variable; the suite never silently sends an empty credential. (This is stricter than the log-field
  table, which warns; a `${VAR}` placeholder has no sensible default, so a missing/empty value is an error.)
- This is consistent with [COMPATIBLE-SUT.md](../docs/COMPATIBLE-SUT.md) HARD #6 ("all wiring in this one file,
  no out-of-band env vars"): the **wiring** stays declared here; the environment supplies only the **value**.
  #6 forbids code reading hidden endpoints/tokens that bypass the config — not `${VAR}` placeholders.

**2. A key YOUR SUT needs internally, for itself** — e.g. an LLM/embedding API key, a payment-provider secret,
a downstream token your service calls out with. **This is NOT the testing suite's concern and does NOT go in
`argus-config.yaml`.** It belongs to your SUT's own deployment (its `.env` / compose / k8s secret), managed by
you as the SUT owner. The suite only calls your SUT's endpoints; whether your SUT can reach its own downstream
is your service's configuration.

- *Worked example — Memstore's embedding key.* To exercise Memstore's content path (write → search), Memstore needs an
  embedding credential. In its current design you register that credential **through Memstore's own REST API**
  (`POST /api/credentials`, stored encrypted per group) — **not** via `argus-config.yaml` and not as a suite
  env var. If the SUT lacks it, the write→search scenario simply *fails* — which is a correct test outcome
  (the suite surfaces the SUT's misconfiguration; it never injects a key it shouldn't own).

## Rate limits — declare yours (optional, 0.3.29)

If your SUT throttles callers, say so in a **top-level** `rate_limit:` block (never under `targets`). With it declared, a
refusal that carries your signature is scored **"not measured"** (a grey step — never a pass and never a product
failure), the run pauses for the retry-after (or the window), retries that one scenario once, and the run summary
says how often it paused. Without the block nothing changes: Argus never guesses a throttle.

```yaml
rate_limit:
  requests: 120                 # how many requests the SUT accepts per window
  per: minute                   # minute | second — nothing else is accepted
  signature:
    body_contains: "rate limit exceeded"   # the stable text in YOUR refusal (MCP tool error / HTTP 429 body)
    retry_after_field: retry_after         # optional: the JSON field carrying the retry-after SECONDS
  pause:
    max_pauses: 3               # pauses allowed in ONE run (default 3)
    max_total_wait: 180s        # total wall-clock ONE run may spend paused (default 180s)
    retry_once: true            # re-fire a throttled scenario once after the pause (default true)
```

An unknown key under `rate_limit`, `signature` or `pause` is refused at load, by name. `runner__validate_config` reports
`requests_estimated` for the pack and warns when it would exceed your limit — information only; the run still starts.
When a run hits the pause cap it stops pausing and says so: split the pack by layer or tag.

## Step 4 — fill the argus-owned block (`deploy` + `observability`)
This is what makes the bundled obs stack see **your** SUT (it runs in a separate project/network):
- **`deploy.compose_project`** / **`deploy.network`** — from Step 1. The onboarder uses these to scope promtail's log
  capture and to join the suite's runner + JMeter to your network.
- **`observability.loki`** — `url: http://loki:3100` (the bundled Loki — leave as-is); `derive_project_from_container: true`
  (labels each SUT's logs by its project); **and the four-field LOG-FIELD TRANSLATION TABLE (REQUIRED) — see the
  next section.** You must fill all four (`correlation_field`, `level_field`, `saga_event_field`, `saga_event_value`).
- **`observability.grafana.dashboard_template`** — leave as the bundled dashboard.
- **`observability.grafana.public_url`** — the Grafana URL you open in your **browser**, declared **PER TIER**.
  This is the one field in the whole file whose correct value *differs per environment*, and it is
  **MANDATORY for the tier you onboard to** — if it is missing for that tier, onboarding **stops**
  and prints the validator's own reason. **The one exception is `--obs none`**: you chose no Argus
  observability, so no Grafana is wired and the field is not required on any tier. Without it (and
  without a `dashboard_link.template`, below) a run has no dashboard link and the web says so, rather
  than showing a link that does not open. If you declare it anyway it is still used for the link.

  ```yaml
  observability:
    grafana:
      dashboard_template: dashboards/argus-overview.json   # common — the same everywhere
      public_url:
        compose: http://localhost:3000
        k3d:     http://localhost:3000
        managed: https://grafana.example.com           # the cluster ingress — NEVER localhost
  ```

  **Why this one field is different.** Everything else in this file is written from the **runner's**
  vantage — a service name like `http://order-api:8080`, which resolves the same on every tier. This
  field answers a different question: *what does a **human** open in a browser?* On compose and k3d
  that is your own machine, so `http://localhost:3000` is right. **On a k8s tier `localhost` is NOT
  your machine** — it is a container inside the cluster — so a `localhost` link cannot open the
  cluster's Grafana and produces a dead link in every report.

  Onboarding refuses rather than falling back to a default, because a dashboard link that does not
  open is worse than no link at all: nobody re-checks a link that looks fine.

- **`observability.dashboard_link`** *(optional)* — already have your own dashboards (Grafana, Zabbix, Datadog, an
  internal page) and no use for ours? Say where a run's dashboard lives and the control plane renders a link per
  run — for **every past run** too, no re-run needed. Pair it with `--obs none` to deploy no observability of
  Argus's own.

  ```yaml
  observability:
    dashboard_link:
      template: "https://grafana.lab.example/d/msgbus?var-run={run_id}&from={from}&to={to}"
      label: "Open in Grafana"        # optional; default "Open dashboard"
  ```

  Placeholders: `{run_id}` `{correlation_id}` (the run's prefix `tr-<run_id>`) `{instance}` `{target}` `{from}` `{to}`
  (`{from}`/`{to}` are epoch **milliseconds**, the run's start − 5 min and finish + 5 min). Anything else in braces,
  a credential in the URL (`user:pass@host`), `${VAR}`, or a non-`http(s)` URL is refused when the config loads: a
  link is shown to people, never a secret. When a template is declared, a missing per-tier `public_url` is no longer
  a warning. See `docs/ARGUS-GRAFANA-CONTRACT.md` ("Bring your own dashboard").

## The log-field translation table (REQUIRED — fill all four)
The dashboard's **Logs** panel and **Saga timeline** panel are built by reading **your service's own log
lines**. Every service writes JSON logs, but each one *names its fields differently*. So the suite cannot
guess — you must **explicitly** tell it what your service calls four specific things. These live under
`observability.loki`, and **you provide all four every time** (do not rely on hidden defaults):

| # | Field in argus-config | What it is (in YOUR logs) | the operator-standard value | Social's value |
|---|---|---|---|---|
| 1 | `correlation_field` | the field holding the **request / correlation id** the suite injects on every call (this is what ties each log line back to a specific scenario + run — the Logs panel and the per-scenario filtering key on it) | `correlation_id` | `request_id` |
| 2 | `level_field` | the field holding the **log severity** (INFO / WARN / ERROR) | `level` | `level` |
| 3 | `saga_event_field` | the field that **marks a line as a saga** — a control-action / audit event that belongs on the Saga timeline (NOT every log line) | `event_type` | `event` |
| 4 | `saga_event_value` | the **value** that field #3 holds when the line *is* a saga | `saga` | `tool_dispatch` |

**Why these four and no others:** the dashboard only ever needs to find, in your logs, (a) *which scenario a line
belongs to* (#1), (b) *how severe it is* (#2), and (c) *whether it's a control-action worth putting on the Saga
timeline* (#3 + #4). Everything else on the dashboard is fed by the suite's **own** data, not your logs — so there
are no other log-field declarations.

### How to find your values (2 minutes)
1. Start your service and print one structured log line:
   ```bash
   docker compose -p <your-project> logs <your-gateway> | grep '{' | head -1
   ```
2. You'll see a JSON object, e.g.:
   - **A the operator-conformant service (OrderService):**
     `{"level":"INFO","correlation_id":"tr-...","event_type":"saga","what":"...","msg":"..."}`
     → `correlation_field: correlation_id`, `level_field: level`, `saga_event_field: event_type`, `saga_event_value: saga`.
   - **A service that deviates (Social):**
     `{"level":"INFO","request_id":"tr-...","event":"tool_dispatch","tool_name":"...","msg":"..."}`
     → `correlation_field: request_id`, `level_field: level`, `saga_event_field: event`, `saga_event_value: tool_dispatch`.
3. Copy those four names/values into the config. If your service follows the the operator convention, the four are
   `correlation_id / level / event_type / saga` — **still write them out explicitly.**

If your service emits no sagas or exposes no /metrics, still fill the four field names with your best
values and declare the gap in `declared_gaps:` — the affected panel then shows an honest "no data
because…" instead of silently misconfiguring. Correlation propagation is different: it is a HARD
requirement, not a declarable gap. If your service cannot propagate a correlation id, talk to us
before onboarding.

## `observability.betterstack` (optional — an alternative log source)
`observability.loki` (above) is the default: the bundled Loki, queried over LogQL. If your SUT's logs
already live in a BetterStack (Telemetry) account instead, declare `observability.betterstack` and the
suite queries BetterStack's HTTP SQL API for the exact same evidence — saga timeline, tailed logs,
correlation-id- and window-scoped — that the Logs/Saga panels and `get_sagas`/`get_tail_logs` read from
Loki today. **Declare exactly one of `observability.loki` / `observability.betterstack` — never both**;
validation refuses a config declaring both, naming both blocks.

```yaml
observability:
  betterstack:
    query_url: https://eu-nbg-2-connect.betterstackdata.com   # the account's regional Query API host
    credential: ${BETTERSTACK_CREDENTIAL}                     # ${VAR} ONLY — never a literal secret
    team_id: "123456"
    sources:                       # source slug -> the service name it carries (one source per service)
      accounting: accounting-service
      trade_execution: trade-execution-service
    correlation_fields: ["message_json.correlationId", "correlation_id"]   # optional — this is the default
    level_field: level             # optional — default shown
    saga_event_field: event_type   # optional — default shown
    saga_event_values: ["saga"]    # optional — default shown; same meaning as observability.loki's saga_event_value(s)
```

| Key | Required | What it is |
|---|---|---|
| `query_url` | yes | The HTTP SQL query endpoint (your account's Query API host — see BetterStack's "connect remotely" docs). |
| `credential` | yes | A `${VAR}` reference to the query API's `username:password` Basic-Auth pair — **a literal value here is refused at load time**, same rule as every other credential in this file. |
| `team_id` | yes | Your BetterStack team id — it names the table `remote(t<team_id>_<source>_logs)` for each source. |
| `sources` | yes, ≥1 | Source slug/id → the service name it carries. BetterStack has one source per service (unlike Loki's single multi-service stream), so this is also how a row's `service` is derived. Each slug is checked against a safe-identifier pattern; an unsafe slug is refused at load time, by name. |
| `correlation_fields` | no | The JSON field path(s), tried in order, that carry the propagated correlation id inside a log line's body. Default: `message_json.correlationId` (BetterStack nests an ingested JSON message under `message_json`), then `correlation_id` (a SUT that writes it flat instead). |
| `level_field` / `saga_event_field` / `saga_event_values` / `saga_step_fields` | no | Same meaning as their `observability.loki` counterparts — see **The log-field translation table** above. |

A source slug, team id, or field path is never interpolated raw into a query: each is checked against a
safe allow-listed pattern before use, and a correlation id is always sent as a query **parameter**, never
concatenated into the SQL text. A query that times out, errors, or returns nothing surfaces the same
honest "unavailable" outcome the Loki path uses (`available:false` + a `note` explaining why) — never a
silent empty result.

## `observability.pushgateway.group_retention` (optional — how long a finished run's metrics group stays)
Every run pushes its `argus_*` metrics to the Pushgateway as its own group (`run_id`-keyed), and the
Pushgateway never forgets a group by itself — at about a hundred runs a day it grows without bound.
After each successful push the executor deletes **this instance's** older run groups (never another
instance's or another job's, never the run it just pushed). Prometheus keeps what it already scraped.

```yaml
observability:
  pushgateway:
    url: http://pushgateway:9091   # as before (adopt mode)
    group_retention: 15m           # optional — a Go duration, 1m to 720h; 0 keeps every group for ever
```

Absent = `15m`. `0` = the old behaviour (nothing is deleted). A negative, unparseable or out-of-range
value, or a mistyped key under `observability.pushgateway`, is refused at load, by name. Keep it well above
your Prometheus scrape interval. Needs an executor release; an older executor ignores the key and keeps
every group. See `docs/ARGUS-GRAFANA-CONTRACT.md` (Group retention).

## OpenShell sandbox evidence (optional)
Only when the system under test is an **AI agent running in an NVIDIA OpenShell sandbox** (spec 26, A1).
OpenShell logs network and HTTP allow/deny decisions, process lifecycle events and findings as OCSF JSON;
filesystem (Landlock) and seccomp refusals are not logged (spec 26 C2, C3), so this block never shows
them, and a block with nothing denied says nothing about file access. With this block declared, every
scenario row in `report.json` carries a `sandbox_policy` block: what the sandbox **denied** during that
scenario. **Observe only in this release: it never changes a verdict.** Absent = off, and `report.json`
says nothing about it (no query, no wait, the same bytes and evidence hashes as before).

```yaml
observability:
  openshell:
    source: loki                    # the only value in v1
    selector: '{job="openshell-gateway",source="sandbox-jsonl"}'
    sandbox: ${SUT_SANDBOX_UID}     # the sandbox ID, not its name
    window_pad: 5s
```

`sandbox-jsonl` (the sandbox-local OCSF file, shipped by the host) is trustworthy only if the effective
policy does not grant read-write on `/var/log`, because the agent could then forge or erase it (spec 26
C10). Spec 26 G2, **not yet measured**, decides whether this source may be used as evidence.

**This source is unproven.** No real OpenShell line has yet been shipped into Loki and read through
this block: the shipping path, the label and the fields come from OpenShell's documentation, not from a
capture. Spec 26 §9 G1 proves it or not; until then treat every block as a reading of an unproven
source.

| Key | Required | What it is |
|---|---|---|
| `source` | yes | `loki`. The events are read from **this executor's own Loki** (`--loki`, with its `--loki-tenant`), not from `observability.loki.url`. Anything else is refused by name. |
| `selector` | yes | A bare LogQL stream selector (`{label="value", ...}`) for the stream that carries the sandbox's OCSF JSONL. A line filter or pipeline stage after the braces (such as `!= "Denied"`, or a `json` or `line_format` stage) is refused by name: it could hide denials or rewrite lines. Getting the lines there is the host's job (spec 26 §9, G1). |
| `sandbox` | yes | The sandbox **ID** — OCSF `container.uid`, what `openshell sandbox get` prints — never its name. Re-creating a sandbox changes the ID. Expanded from the environment like a credential field, and listed by `secrets-scan`; an unset variable fails the load naming it. |
| `window_pad` | yes | A positive Go duration (`5s`), at most `2m`. Each scenario's window is padded by it on both sides, and the run waits one more pad after the last scenario before reading, so the wait is up to two pads. A larger value is refused by name. |

Unknown keys inside this block are **refused by name** at load, with the line and the accepted keys.
The block is **refused beside `observability.betterstack`**: a BetterStack instance has no Loki to read.

**What a block says.** `coverage` and `coverage_reason` are always present:
- `complete` — lines from the sandbox were read for the window, and every one parsed. Loss inside
  OpenShell before a line reached Loki is not visible to Argus, and the reason says so.
- `lossy` — the read hit its limit of 2000 lines, or some lines were not OCSF JSON. The listed events
  are real; others may be missing. Only the line limit is detected as a partial read: Loki's own
  `warnings` or partial-response signals are not read in this release.
- `unavailable` — nothing can be said, and the reason names why: Loki could not be read (the HTTP status
  or transport error), `--loki` is empty, the sandbox id is not a safe identifier (it is then not echoed: the block's `sandbox` is empty), the lines carry a
  downgraded schema (OpenShell's `ocsf_schema_version` set to 1.1 or 1.3 strips `container`; the reason
  names the `metadata.version` seen, cut to 32 characters, with token-shaped runs redacted), the lines
  are OpenShell's shorthand text rather than OCSF JSON, or **no line from the sandbox at all** fell in the
  window. That last one is the liveness rule: Loki answers a wrong selector with an empty success, so an
  empty window is never read as "no denials". An idle sandbox therefore also reads `unavailable`.
  An `unavailable` block always carries `"denied_count": null` and `"events": []`, even when some lines
  did parse: nothing was measured. **An empty `events` list with `coverage: unavailable` never means
  "nothing was denied".** On `complete` and `lossy`, `denied_count` is a number, and `0` is a measured zero.

Kept events are `Denied` or `Blocked` decisions and detection findings (class 2004), each as
`{time, class, action, target, process, rule, reason}`, every field at most 256 characters. No payload,
header or query string is carried. HTTP targets are `METHOD scheme://host:port/path`, with no
userinfo, no fragment and at most the first 8 path segments (`/…` marks the rest); OpenShell logs
TLS-terminated traffic as `http://host:443/...`. A URL that does not parse and still holds an `@`
after its userinfo is dropped (the text cannot tell userinfo from a path), and the target is then
`METHOD host:port` from the event's destination. A finding's target is its title, cut to 128
characters. A title and a reason lose query strings and `scheme://user:password@` userinfo; free
text has no URL end to go by, so userinfo holding a `/` or a space is not recognised there. A path
segment, or a run of a title or a reason, that looks like a token is replaced by `[redacted]`: a known
prefix (`ghp_`, `github_pat_`, `sk-`, `syn_`, `xoxb-`, `eyJ` and a few more) followed by 16 or
more characters; 24 or more hex digits; 24 or more letters and digits mixed; 24 or more base64
characters (letters, digits, `+ / - _`) with a piece of 8 or more that mixes upper case, lower case
and digits; or the value after `Bearer` or `Basic`, unless it is a plain word (`Basic auth`).
**That is a check on shape, not a guarantee**: a secret that does not look like a token (a short
password in a path segment, a value of letters only) is carried as OpenShell logged it, and so are
host names and the method, class, action, process and rule names. Measured on random secrets of 24
to 32 bytes in base64 or base64url, under 1% pass it (about 3% at 18 bytes). A git SHA, a digest,
or a name with such a mixed piece (`ProviderV2-...`) also looks like a token and is redacted. Keep
credentials out of URL paths. At most 50 events are listed (`events_omitted` counts the rest;
`denied_count` counts all). A DNS failure is logged by OpenShell as a denial and is listed too.

**Attribution.** No OpenShell event carries a correlation id, so events are tied to a scenario by the
event's own time and the sandbox id. A scenario's window runs from just before it starts to just after
its throttle re-fire, **without** its `## CLEANUP` (a cleanup talks to the database, not to the agent),
padded by `window_pad`; a rate-limit pause inside a re-fire stays inside the window. Scenarios run one
after another, so padded windows overlap at their edges: an event that falls inside another scenario's
padded window lists that scenario in `shared_with`. A run on **another** Argus instance against the
same sandbox is not known to this one: its events land in this run's windows as if they were this
run's, with no mark. The run lock fences runs on one instance only. **Give a certification instance
its own sandbox, or never run an ordinary run against a sandbox while another instance certifies
against it**: otherwise the certification run's denials appear in the ordinary run's block, which the
builder reads.

**Who sees it.** The block follows the run's custody. On a build run, or a local run you start
yourself, both hats read it in `get_report`. On a certification run (final, scheduled, rehearsal) a
builder gets the verdict and the tallies only, as for every other per-scenario field: no block, no
event, no count, no coverage word. The author reads the whole block on every run. The relayed
`get_report` and the results push carry none of it. It leaves the executor only in
`argus.build_record` on a build run (written to the hub's Memstore, in the product-hat shape), and
whole in the author's `author_get_report` answer (relay verb `get_full_report`, which the control
plane keeps in `relay_commands`). What leaves is the block as described under the kept events above:
credentials are removed by shape only (#448), so on a build run a secret that does not look like a
token reaches the builder's report and `argus.build_record` as OpenShell logged it.

This covers what Argus hands out. The raw OCSF lines sit in the same Loki that
`runner__get_tail_logs` and `runner__get_sagas` read. Those two take only a whole correlation id of
the shape Argus mints (`tr-<run_id>-<scenario_id>-<8 hex>`) and refuse a fragment, and an OCSF line
carries no correlation id, so a builder cannot reach a certification run's denials through them by
searching for the sandbox id or `Denied` (before #449 they accepted a fragment, and that was the route). Still
**ship the OCSF stream with no `service` label** (the selector above has none): those tools read only
streams that have one (other than loki, promtail, grafana, prometheus, pushgateway or jmeter), so the
stream stays out of their reach altogether. Grafana Explore on that Loki has no such guard: whoever
can open it can read the stream.

Every `lossy` or `unavailable` row of a build or local run is also logged as a WARN line with its
`correlation_id`. A certification run logs no such line, because a builder can read the executor's
log through `get_tail_logs`.

## Step 5 — validate
The onboarder runs `validate-config` for you (and fails fast if a layer has no target). A green result looks like:
`{"valid": true, "errors": null, "scenarios_found": N, "targets_configured": [...], "scenario_layers": [...]}`.

**What this config can test.** The same output carries `capability_matrix` — one row for each of the eight
layers, whether or not a scenario uses it yet — and a one-line `capability_summary`, e.g.
`"5 of 8 layers testable with this config; not testable: Message Flow, External Delivery; decided outside the config: Web UI"`.
Each row says `available` / `unavailable`, which target it `needs`, and the `reason`. It is information only:
a layer your SUT does not have (no bus → no Message Flow) never makes the config invalid. A scenario that
*uses* an unavailable layer is still refused, as before. An MCP SUT with only `targets.mcp` gets HTTP
Ingestion, Error Path, Rate Limiting and Permissions for its mcp/chain scenarios — the row says "mcp/chain
scenarios only", because a plain HTTP scenario on those layers still needs its own target. Web UI is always `outside-config`: its page comes
from the scenario's `app_url` or the runner's `APP_URL`, never from this file.
If a credential `${VAR}` is referenced but unset — or set to an empty value — (missing from your **product
folder's `.env`** / your shell on direct runs), validation **fails** naming the variable — add it, then re-run.
This is the guardrail against an empty credential (the secrets preflight normally catches it even earlier).

## Worked example — OrderService
A complete, validated example for the OrderService SUT (project `order-service`, services `order-api:8080` /
`rabbitmq` / `postgres` / `webhook-mock:8081`, read-only role `sut_ro`, table in schema `order_service`) is the
bundled `examples/order-service/demo-packaging/argus-config.yaml` (it ships in the image and is what Path A's demo uses). Use it as a model for your own service.

For MCP-server SUTs, see `examples/social/argus-config.yaml` (bearer token via a `${VAR}` named after the
variable in the SUT's own `.env`, `correlation_field: request_id`, `saga_event_field: event`,
`saga_event_value: tool_dispatch`) and `examples/memstore/argus-config.yaml` (`auth: none`, a minimal SUT).

## Common mistakes
- ❌ `http://localhost:8080` (your laptop's vantage) → ✅ `http://order-api:8080` (the runner's vantage).
- ❌ `public_url: { managed: http://localhost:3000 }` → ✅ `managed: https://grafana.example.com`.
  **This is the mirror image of the entry above, and it catches people precisely because of it.**
  Everywhere else "never localhost" is the rule; `public_url` is the one field where the *laptop's*
  vantage is correct — on compose and k3d. On a k8s tier `localhost` is a container inside the
  cluster, not your machine, so the link resolves to nothing. It fails silently: the run passes, the
  report carries a link, and the link 404s when somebody finally clicks it.
- ❌ `public_url: http://localhost:3000` as a single value → ✅ a per-tier map. The single-value form
  is rejected at parse time with the shape to copy; it was removed because one value cannot be
  correct for three tiers.
- ❌ a read-write DB user → ✅ a read-only role (the suite must never write to your SUT).
- ❌ a literal secret in the config (`bearer_token: smcp_abc123`) → ✅ a `${VAR}` named after the variable in your product folder's `.env` (see **Secrets & credentials**).
- ❌ a `${VAR}` name that doesn't match any key in `<product-dir>/.env` → the preflight stops onboarding, listing the missing names — fix the name (or add the key) and re-run.
- ❌ putting your SUT's OWN downstream key (LLM/embedding/payment) in `argus-config.yaml` → ✅ that lives in your SUT's own env; the config only holds keys the SUITE needs to reach your SUT.
- ❌ forgetting `deploy.compose_project`/`network` → your logs never reach the dashboards, and the runner/JMeter
  can't join your SUT (the obs stack can't find a SUT it wasn't told about).
- ❌ a VERIFY query without the schema when your table isn't in `public`.
- ❌ a prose-only Database State VERIFY (no ```sql block) → rejected at validation (it would verify nothing).

## SUT compatibility: one SUT namespace, one Argus executor (UC007)

On the Kubernetes tiers (`--tier k3d` and `--tier aks`) your SUT must run in a namespace that **no other
Argus executor is already watching**, and that namespace should hold **only that one SUT**.

Onboarding enforces this: if another instance already watches the namespace you passed to
`--sut-namespace`, it is REFUSED, and the message names the conflicting instance and the exact teardown
command. Re-onboarding the SAME instance is always allowed — the check excludes it.

**Why it is a hard rule rather than advice.** Each instance collects the SUT's logs with a static
namespace glob (`/var/log/pods/<sut-namespace>_*/*/*.log`) and stamps every line it reads with its OWN
instance label. Two instances watching one namespace therefore both ingest the SAME pod logs and each
claim them: the second instance's log store fills with the first instance's test runs, and vice versa.
Nothing looks broken while it happens — every pod is Ready and every dashboard renders — and the damage
is MUTUAL, so adding a second watcher corrupts the instance that was already running.

If the conflicting instance is stale (a teardown that did not finish), tear it down and re-run:

    bash onboarding/teardown.sh --instance-id <conflicting-instance> --tier <tier> [--kube-context <ctx>] [--control-plane <url>]

Note this applies to k3d exactly as it does to a managed cluster — both render the same glob. On the
compose tier it does not arise, because each compose instance brings up its own copy of the SUT.
