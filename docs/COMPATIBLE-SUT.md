> **Downstream of [`docs/DEPLOY-ARGUS.md`](DEPLOY-ARGUS.md) — the single door.** If you are deploying
> Argus for the first time, start there; it links here at the point you need it. This
> document assumes you arrived from it and does not repeat what it already decided.

# What can be a SUT — the conformance contract

> The single, names-free answer to *"which services can the Argus MCP test?"* A service is a
> valid **System-Under-Test (SUT)** — **compatible** — when it meets the HARD requirements below. The SOFT
> ones degrade gracefully (a reported gap, not a failure); the HARD-STOP list is what the suite **cannot**
> test today, stated up front so onboarding fails *honestly* instead of mysteriously.
>
> **The SUT stays clean.** None of this asks the SUT to know anything about Argus — it asks the SUT to
> follow the **standard platform conventions** it would follow anyway. The obs/harness adapts to the SUT,
> never the reverse. The onboarding command runs a conformance preflight so the SUT owner learns the result
> at onboarding, not at first RED run.
>
> **This document is DERIVED, not invented.** Every requirement below cites its source in the Argus design
> documentation. The design docs are authoritative; this file restates their SUT-facing requirements. The
> governing rule: **requirements are not weakened to fit a SUT — if a SUT fails a HARD requirement, that
> *aspect* is out of scope for it.**

## How requirements are classified
- **HARD** — required. Violation ⇒ that aspect is not testable / a CODE_BUG / a scenario FAIL.
- **SOFT** — expected. Omission ⇒ a reported gap; the SUT's behaviour is still testable.
- **CONDITIONAL** — HARD *only when* a given layer or action is exercised (noted inline).

---

## Recommended protocols

These five integration surfaces are the ones the suite tests best. They are **recommended\*** — not
"the operator standards" and not mandated: a SUT speaks whichever of them it already uses, and any single trigger-able
layer (HARD #2) is enough to onboard.

- **HTTP** — a REST/JSON edge (the most common trigger layer).
- **MCP** — a Model Context Protocol server (Streamable HTTP or legacy HTTP+SSE; see #8–#11).
- **AMQP / RabbitMQ** — a message broker the SUT produces to / consumes from (Message-Flow + DLQ assertions).
- **PostgreSQL** — a relational store the suite reads (Database-State layer; read-only credential, #7).
- **Playwright-driven web UI** — a browser-testable front end (the UI layer).

\* A slim execution-plane image will be used.

---

## HARD — required (any SUT)

1. **Deployable somewhere the suite can reach it**, on any of the **three tiers that ship**:

   | tier | what the SUT is | what the suite needs to find it |
   |---|---|---|
   | `compose` | a docker-compose stack on your machine | a discoverable **compose project + network** to join |
   | `k3d` | a namespace in a local k3d cluster | the namespace, and a kube context that reaches it |
   | `aks` | a namespace in a managed cluster | the namespace, and a kubeconfig + context |

   Choose with `--tier compose|k3d|aks` at onboarding (`onboarding/onboard.sh`). The `argus` CLI's own
   `--tier` (`render-k8s`, `preflight`) lists `k3d | aks | eks | gke | managed` in its help (`managed` for a
   cluster you run yourself, e.g. k3s) and also accepts `k8s-dev` (a `managed` alias); `kind`, `minikube`
   and other names are refused (see DEPLOY-ARGUS.md section 2.1).

   > ⚠ **This line used to say "BYO-SUT is local-compose only today; hosted/k8s is a future model."**
   > That was false while `--tier k3d` and `--tier aks` shipped and three SUTs ran on k3d — and its
   > sibling in the same kit documented those tiers correctly. It is corrected here rather than quietly
   > deleted because of what a stale eligibility statement costs: **a team on AKS reads it, concludes the
   > answer is no, and never onboards. Nobody files a bug for a product they concluded was not for them.**

   **On the k8s tiers (`k3d`, `aks`): ONE SUT namespace, ONE executor.** Your SUT must run in a
   namespace no other executor is already watching, and **onboarding refuses otherwise, naming the
   conflicting instance**. Two executors on one namespace both ingest the same pod logs and each claim
   them, so each one's log store fills with the other's runs — and nothing looks broken while it
   happens. This does not arise on compose.
   — *src: `03-infrastructure.md §1`; M25-RO onboarding gates; the namespace rule from `HOW-TO-ARGUS-CONFIG.md`.*

2. **At least one trigger-able layer** reachable from the runner's vantage — an HTTP edge endpoint, **and/or
   an MCP endpoint**, and/or a message broker it consumes, and/or a readable database, and/or an
   external-delivery sink — each declared in `argus-config.yaml` `targets.*` (HTTP → `targets.http`;
   MCP → `targets.mcp`; etc.). A service with SEVERAL endpoints of one kind declares them as named entries
   under `targets.http_targets` / `targets.mcp_targets` / `targets.database_targets` /
   `targets.message_broker_targets`, and a scenario selects one with `**Target**: <name>` (0.3.29, VR10-S3).
   — *src: `COMPATIBLE-SUT` (this contract); `mcp-server-testability-requirements.md` (MCP as a trigger layer).*

3. **Structured JSON logs to stdout**, carrying a **propagated correlation id** and a **`level`** field
   (the platform ships stdout → Loki; triage, sagas, tail-logs, and the dashboard all key on these).
   The canonical field names are `correlation_id` and `level`; **either may be declared under a different
   field name per-SUT** in `argus-config` (`observability.loki.level_field`, `observability.loki.correlation_field`)
   — see [Correlation field naming](#correlation-field-naming) below. **The behaviour (propagation) is HARD;
   only the field name is declarable.**
   — *src: `COMPATIBLE-SUT §HARD #3`; derived from `04-observability.md` (logs → Loki; the Logs panel shows
   ALL SUT logs — generic, no level filter — and `correlation_id` is promoted to a Loki label for run/scenario
   scoping) and the canonical join key `01-product-owner VR-G5`.*

4. **Correlation propagation** — an inbound correlation identifier (HTTP `X-Correlation-Id` / AMQP header /
   MCP `_meta.request_id`) is carried into the log field → saga, unchanged. It is the canonical join key
   across tile ↔ saga ↔ log ↔ DLQ. **A SUT that does not propagate it at all is a HARD-STOP** (cross-layer
   and saga correlation become impossible).
   — *src: `COMPATIBLE-SUT §HARD #3` + HARD-STOP; MCP handle `DF-DEC-M25-11`; `01-product-owner VR-G5`.*

5. **A saga (`event_type=saga`) for every MANDATED CONTROL ACTION**, carrying the correlation id and
   `{what, why, by_whom}`. **CONDITIONAL:** only *control actions* (operational knob/config/grant/revoke/
   migration/safety-grade — explicitly **not** pure data-flow) owe a saga; the SUT **declares which actions
   are saga-mandated** in `argus-config`. A required-but-absent control-action saga is a **CODE_BUG for both
   hats**; an action that is *not* saga-mandated owes none. The tag `event_type=saga` and the field triple are
   the contract. `event_type` is a **top-level JSON field** — promtail promotes it to a **Loki label**, so the
   Saga panel selects saga lines by an `event_type` label matcher (the value is declarable per-SUT via
   `observability.loki.saga_event_field` / `saga_event_value`, default `event_type` / `saga`).
   — *src: `COMPATIBLE-SUT §HARD #4`; `service-testability-requirements §3` Axiom #25; `01-product-owner
   VR-F14/F15`; `M2.5-01 VR-N9..N12`.*

6. **A complete `argus-config.yaml`** — the **single** per-service onboarding artifact. **All SUT-specific
   test wiring lives here**: `targets.*` per layer (including `targets.mcp` for MCP SUTs, and the named
   `targets.<kind>_targets` maps when a service has several of one kind), the auth/token, and
   the argus-owned obs/deploy block. No out-of-band env vars.
   — *src: `COMPATIBLE-SUT §5`; `onboarding/argus-config.template.yaml`.*

7. **A read-only DB credential** — **CONDITIONAL** on a Database-State layer being used (*the suite must never
   write to the SUT DB*). Supplied via `targets.database`. The role **name** is the SUT's implementation
   choice; the contract is "a read-only credential exists and argus never writes."
   — *src: `COMPATIBLE-SUT §HARD #6`; `02-solution-architect §3` (trust boundary); `03-infrastructure §1`.*

### HARD — additional, for an MCP-server SUT
*(Apply when the trigger layer is an MCP server. N/A for a pure HTTP SUT.)*

8. **Transport — declare ONE of two:** Streamable HTTP (single `POST /mcp`) **or** legacy HTTP+SSE
   (`GET /sse` + `POST /message?sessionId=…`). The SUT declares its transport (`targets.mcp.transport`); the
   suite speaks the matching handshake and **never guesses**. **stdio is out of scope.** Protocol revision
   (`2024-11-05` / `2025-03-26`) is version-tolerant, not gated.
   — *src: `mcp-server-testability-requirements.md Tier-1.1/1.3`; `M2.5-01 VR-J5/J6`.*

9. **The two error planes (the binding verdict):** protocol plane = a top-level JSON-RPC `error`
   (`-32601` unknown method, `-32602` unknown tool / bad params); tool plane = `result.isError: true`. A
   tool failure signalled only by prose inside `result.content` (with `isError` absent) is **non-conformant**
   — it produces an *advisory soft-warning only*, never the binding pass/fail.
   — *src: `mcp-server-testability-requirements.md §intro + Tier-1.5`; `M2.5-01 VR-J1..J4`.*

10. **Reachability** — the MCP endpoint must be reachable from the runner's vantage. In the BYO container
    model the runner + JMeter share the SUT docker network, so the endpoint is the SUT **compose
    service-name:port** (e.g. `memstore-gateway:8090`); preflight reports `unreachable` **distinctly** from a
    tool failure (gate-infra, never a SUT pass/fail).
    — *src: `mcp-server-testability-requirements.md Tier-1.7`; `M2.5-03 §3`; `DF-DEC-M25-14`.*

11. **Declared auth mode + clean rejection** — declare `none` / static-bearer / OAuth-JWT; if bearer, reject
    a missing/invalid token clearly (e.g. HTTP `401` + JSON-RPC `-32001`). The token format is the SUT's
    choice.
    — *src: `mcp-server-testability-requirements.md Tier-2 #4`.*

---

## SOFT — expected (omission ⇒ a reported gap; behaviour still testable)

12. **Prometheus `/metrics`** (conventionally `:9090`) — OPTIONAL and **NOT part of onboarding**. As of the
    runner-sourced "Test requests sent" rework, **no dashboard panel depends on a SUT `/metrics` endpoint**:
    the request/error view is runner-owned (the runner ships one Loki request-event per request it FIRED —
    the "Test requests sent (by outcome)" panel plots their per-bucket count_over_time), so it populates for
    ANY SUT, including one that exposes no `/metrics`. Prometheus scraping is not wired up by
    `onboarding/onboard.sh` and there is no `observability.prometheus.*` block in `argus-config` — a SUT is
    free to expose `/metrics` for its own operability tooling, but the suite neither discovers nor depends on
    it. **A missing `/metrics` is not a reported gap.**
    — *src: `COMPATIBLE-SUT §SOFT #7`; RO-05/07.*

13. **Health endpoints** — `/healthz` (+ `/health/live`, `/health/ready`) on `:8080` — operability only,
    non-gating. — *src: `COMPATIBLE-SUT §SOFT #8`; `DF-DEC-012`.*

14. **Dead-letter / poison-message handling** — **CONDITIONAL** on the SUT being a message consumer. NOTE: the
    dedicated DLQ **dashboard panel was removed** when the dashboard was made fully SUT-agnostic (no fixed
    SUT-metric panels). A poison / dead-lettered message is now exercised by a **Message-Flow scenario's
    assertion** (the runner checks the broker via the management API — e.g. `0 rows` in the main queue / a DLQ
    entry), not by a standing metric panel, so **no SUT-specific DLQ metric name is required**. — *src:
    `COMPATIBLE-SUT §SOFT #9`; `01-product-owner VR-F10`.*

15. **A declared rate limit** (0.3.29) — if your SUT throttles callers, declare the limit and the shape of its
    refusal in a top-level `rate_limit:` block of `argus-config.yaml` (see *How to write your argus-config.yaml*).
    A refusal matching the signature is then scored "not measured" — grey, never a pass and never a CODE_BUG — the
    run pauses and retries once, and the summary counts the pauses. Undeclared, a throttled answer is judged like
    any other answer. — *src: `VR10-R1` (V28-009).*

16. **Message-Flow CONTENT checks — a tap queue the broker lets the suite create** (0.3.32, V30-004). A Message
    Flow scenario that asserts what a published message CONTAINS (`- status == pending`, `- currency == EUR`) is
    judged on the message itself: before the trigger the runner declares a private queue
    `argus-tap-<correlation id>` on vhost `/`, binds it to your incoming exchange, and reads it afterwards
    (it expires by itself after 120 s and is deleted at the end; it never consumes from YOUR queues). What that
    needs from your SUT:
    - **`targets.message_broker.routing_keys`** — `routing_keys: {incoming: <the routing key your service
      publishes with>}` on the broker entry the scenario runs against. Without it every content check is refused
      before it runs and reported `error`, naming `routing_keys.incoming` — no trigger is sent and no tap is
      declared. `argus validate-config --scenarios` names the missing key too.
    - **the broker grant.** The user in `targets.message_broker.url` (its `user:pass` is also the management
      credential) needs **`configure` + `write` + `read` on `argus-tap-*`**, **`read` on the incoming
      exchange**, and a management-capable tag (`monitoring` is enough). Measured with `configure` + `read`
      only: the declare succeeds (201) and the BIND is refused **401** — the tap is created, then refused, and
      left for `x-expires` to reap. ⚠ The `read`-on-exchange half is derived from RabbitMQ's bind rules, not
      measured: a user with `write` on the tap and no `read` on the exchange has not been tried.
    - **credentials in the URL.** A `url` with no `user:pass` sends an EMPTY credential and every management
      call is **401**.
    - **vhost `/` only.** The tap path is `%2F`, and a SUT on a named vhost cannot use content checks yet.
    - A message published as a JSON **list** (GoKit publishes its data list) is read through the object in it
      that carries the run's correlation id.
    The negative and DLQ cases (`- no rows`) declare NO tap: they read your queue's depth through the management
    API before and after the trigger, which needs only `read`. — *src: `V32-03-infrastructure.md §1.1`; V30-004
    F-3/F-4/F-6.*

17. **A declared package for `argus package-check`** (AC-36) — needed for certification, not for a test run.
    `package-check` is the static completeness check of a commit: the manifests the deployment applies pin every
    image by digest (never a tag), its lockfiles are present and consistent, its env schema and secrets example
    agree (secret values are placeholders, never real), and its seed data exists. **Declare the package it
    certifies** in a top-level `package:` block of `argus-config.yaml`; without it the check reads the whole
    repository at Argus's own conventional paths (`k8s/`, `deploy/compose/`, `deploy/env.schema.json`,
    `k8s/base/cp-secrets.example.yaml`, `README.md`) and fails on every overlay you never deploy and every file
    you have no reason to keep there. Every path is relative to the config file's directory, must stay under it,
    and must exist — `validate-config` refuses a missing one by key and path; a typo in a key is refused by name.
    ```yaml
    package:
      manifests:                    # directories are scanned recursively for YAML; a file is read as-is
        - k8s/overlays/prod         # the overlay this deployment applies — not the legacy one beside it
      lockfiles:                    # what your builds pin: go.sum / package-lock.json are checked against
        - go.sum                    # the go.mod / package.json beside them; any other must be present
        - ui/package-lock.json
      env_schema: deploy/env.schema.json          # JSON list of {name, required, secret, description}
      secrets_example: k8s/base/secrets.example.yaml   # the Secret manifest: every key by name, placeholder values
      seed: README.md               # the document carrying the `examples/   # <set>, <set>` tree line
    ```
    Run `argus package-check --config argus-config.yaml` (add `--json` for the report an agent reads; `--root`
    only if the package is not rooted at the config's directory). The report names the certified package — the
    declaration, its origin, and every clause's findings — so a reader of the reveal sees exactly what was
    certified. A declared path missing at check time is a finding on the clause that needed it, never a silent
    skip. `argus package-check --root <dir>` without `--config` is the unchanged whole-root check.
    — *src: AC-9 (the check), AC-36 (the declared package).*

18. **A declared schema for a binary/structured `amqp` wire format** — needed only when a chain's `amqp` step
    uses `schema`+`record` (spec 17) instead of `body`. Any app's message fields are test data in a scenario,
    never Argus code: declare each schema `record` may reference in a top-level `message_schemas:` block of
    `argus-config.yaml`.
    ```yaml
    message_schemas:
      msgbus-envelope-v2:
        format: avro                              # only avro this release; protobuf/json later
        path: schemas/msgbus-envelope-v2.avsc    # relative to argus-config.yaml — or `inline: '{...}'`
        content_type: application/avro            # optional; default application/avro
        headers: { x-envelope-schema: "2" }        # set on every publish with this schema; a step may not override these keys
    ```
    Exactly one of `path` or `inline`; the name matches `[a-z0-9][a-z0-9-]*`. `validate-config` parses every
    declared schema and names the one that fails to parse. `render-k8s` embeds every `path:`-named file into
    the instance ConfigMap next to `argus-config.yaml`, at the same relative path (via a key encoding plus
    `items:`/`subPath` mounts — a ConfigMap key cannot itself carry `/`) — rendering refuses a missing or
    unparseable file rather than deploying an executor that cannot encode. On compose, the same file already
    reaches the executor for free: `docker-compose.byo-m3.yml` bind-mounts the whole product directory read-only
    at `/config`, not just `argus-config.yaml`. Run `argus validate-scenario --config argus-config.yaml` to
    apply the FULL per-record schema check locally, before writing a `schema`+`record` step — the control
    plane's own `author_validate_scenario`/`author_write_scenario` can only check the record's SHAPE, never
    against the schema itself, because the app's `argus-config.yaml` lives on the executor, not there.
    — *src: ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28.*

---

## Correlation field naming
*(Answers: "is it compatible with the design to log the correlation id under a different field name?")*

- The design's **canonical name is `correlation_id`** — the join key across tile ↔ saga ↔ log ↔ DLQ
  (`01-product-owner VR-G5`).
- The design **already parameterizes obs field names** (`observability.loki.level_field` is configurable;
  RO-08). So a conformant SUT **may log its correlation id under a different field name** and **declare that
  name** in `argus-config` via **`observability.loki.correlation_field`** (default `correlation_id`). The
  runner's log/saga extraction reads the declared field; the dashboard's Loki panels filter by **value**, so
  they are field-name-agnostic regardless.
- For **MCP SUTs**, the correlation handle is **`_meta.request_id`** (`DF-DEC-M25-11`): the runner injects a
  `tr-<hex>` value there; the SUT echoes it at `result._meta.request_id`. *(Conformant example: Social MCP
  logs it under `request_id` and declares `correlation_field: request_id`.)*
  ⚠ **Since 2026-09-24 the value is unique per CALL: `<correlation id>.<8 hex>`.** A request id names one
  request, and a SUT may refuse a repeat (Social MCP does: "request_id reused"). Before this, every step of a
  chain — and the one re-fire after a rate limit — sent the same id. The correlation id is the **prefix**, so
  Argus's own log lookups (Loki: a `|= "<correlation id>"` line filter; BetterStack: equal to it, or starting
  with it and a dot) still join every call of a scenario;
  a SUT or a scenario that looks the id up must match the **prefix**, not the whole value. A literal
  `request_id` an author writes without `${cid}` is sent unchanged.
- **The contract is unchanged:** a single correlation id is propagated inbound → log → saga and is the join
  key. Only the *field name* is declarable; the *propagation behaviour* is HARD (requirement #4).

---

## HARD-STOP — what the suite cannot test today (declared up front)

- A service the runner **cannot be placed alongside** — no compose project/network to join, and no
  Kubernetes namespace (local k3d or managed) the executor can be deployed into. A bare public URL with no
  reachable deployment is out of scope: the executor must live inside the SUT's environment to reach
  internal service names, collect logs, and read the database.
- A **closed-box** service that ships no structured logs to Loki → sagas / tail-logs / dashboard are blind and
  saga-presence **fails closed**.
- A service that **does not propagate a correlation id** (under *any* declared field name) → cross-layer and
  saga correlation impossible.
- **Effectful / live** paths that need real provider credentials (e.g. live posting, real embedding) → out of
  scope **unless explicitly authorized by the SUT owner** (a per-SUT *scenario* decision, not a default).
- A **production** target → hard stop; the suite is dev/local only.
- **MCP:** stdio transport; or a content-buried tool error with `isError:false` (non-conformant — the binding
  verdict cannot see it).

---

## Instance naming

Every onboarded SUT gets an **`instance_id`** (its instance name). It is **globally unique per control
plane** — one naming universe across all workspaces — because the name lands in infrastructure identifiers
(k8s namespaces, PostgreSQL schemas, AMQP vhosts) that must be DNS-/schema-safe.

- **Charset:** lowercase letters (`a–z`), digits (`0–9`), and hyphens (`-`) only, and it must not start or
  end with a hyphen. `My_SUT!` is rejected at the name prompt with the rule; `my-sut` is accepted.
  *(Enforced at input time by `ValidInstanceName`; the convention is `<sut>-<tier>`, e.g. `orderservice-compose`.)*
- **Collision assist:** if the chosen name is already registered on the control plane, onboarding says so and
  prompts again rather than failing opaquely, so the clash is resolved before anything is provisioned.

---

## How to adopt
1. Meet the HARD requirements above (the SOFT ones improve the dashboards but are not gates). For an MCP SUT,
   meet #8–#11 too.
2. Copy `onboarding/argus-config.template.yaml` into your product folder and fill it — `targets.*` (incl.
   `targets.mcp` for MCP), the auth/token, and the argus obs/deploy block (compose `project` + `network`,
   metrics discovery, `loki.level_field` / `loki.correlation_field`).
3. Run `onboarding/onboard.sh` — it runs the conformance preflight + the input gates and validates your
   config. A green `validate-config` plus a passing scenario run means the SUT is wired correctly; any SOFT
   omission is reported as a declared gap.

---

## Authoritative sources
- `docs/mcp-server-testability-requirements.md` — the MCP Tier-1/Tier-2 contract.
- `the design notes`, `M2.5-03-infra-observability.md` — the observability + dashboard model.
- `the design notes` (VR-*), `02-solution-architect.md`, `03-infrastructure.md` — the design rules.
- `the design notes` / `DECISIONS-LOG.md` — the `DF-DEC-*` decisions cited above.
