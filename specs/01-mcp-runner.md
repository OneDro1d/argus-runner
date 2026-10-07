# Spec 01 — Argus MCP server (single binary, two tool namespaces)

> **Status:** Draft · **Owner:** Aleksander · **Phase:** Phase 0 (consumer-prototype spike) · **Iteration:** 1 · **Last updated:** 2026-05-21 (Day-5 reshape: CC-1, CC-2, smoke-test, MCP-SDK lock-in)

## Purpose

The Argus MCP server is the AI agent's interface to the suite. **Per Day-5 review CC-1**: iter 1 ships a **single Go binary** exposing two tool namespaces — `runner__*` (validate / run / report / sagas) and `author__*` (CRUD scenarios). The dark-factory boundary that separates "product agent reads results only" from "test agent reads + writes scenarios" is enforced via **token-scope-gated tool exposure**, not by deploying two binaries. If iter 1 evidence shows the auth boundary leaks or per-namespace deploy lifecycle matters, iter 2 splits into two binaries (spec 02).

Per the [eight design principles](00-suite-overview.md#eight-design-principles), this MCP embodies: *standardized bootup ritual* (the first tool call per session validates instance + runs preflight; cached for 60s thereafter per spec 00 NFR-9), *contracts not conversations* (responses are structured JSON, not prose), *executable artifacts only* (every output is consumable by the next tool call or by `failure-triage`), and *visibility before execution* (every tool call emits Begin/Completed log pairs + Prometheus metrics).

## Iteration-1 scope (PoC against OrderService on local k3d)

Per Day-5 review iteration plan, iter 1 ships **9 tools** across two namespaces. Bearer-token auth with a role claim; token scope determines which namespace the caller can reach. Deferred Day-2 / iter-5x tools listed below for navigation but **not built in iter 1**.

| Tool | Iter 1 | Notes |
|---|---|---|
| `runner__validate_config` | ✓ | Wraps argus `preflight.py`. Per-call bootup ritual caches for 60s. |
| `runner__run` | ✓ | Wraps argus `run.sh`. Optional `--layer` / `--tag` / `--scenarios=[ids]` filters. |
| `runner__get_report` | ✓ | Returns parsed `results/${INSTANCE_ID}/report.json`. |
| `runner__get_sagas` | ✓ | **Per Day-5 CC-2**: backed by **Loki query** for GoKit native sagas (already in the operator production at `tr-telemetry` exchange → Loki). NOT backed by Filum or chain in iter 1. Returns the saga timeline for a given `correlation_id`. |
| `author__list_scenarios` | ✓ | By instance, by layer, by tag. |
| `author__read_scenario` | ✓ | Returns markdown + parsed fields. |
| `author__validate_scenario` | ✓ | Parses without writing; returns `{valid, errors[]}`. |
| `author__write_scenario` | ✓ | Validate-then-write atomic. Idempotent. |
| `author__propose_scenario` | ✓ | LLM-helper: returns a draft + `needs_user_review[]` flags, does NOT write. |

**Smoke test deliverable (per Day-5 add):** `scripts/smoke-test.sh` runs OUTSIDE Argus (plain curl + jq), validates the MCP server answers a known query correctly. Breaks the circular-bootstrap problem in dogfooding (review issue #5). Iter-1 acceptance includes "smoke test green."

### Deferred to later iterations (NOT in iter 1)

| Tool | Iteration | Reason |
|---|---|---|
| `runner__list_runs` | 2 | Recent run history. Needs persistent run-store; iter 1 stays on disk-only. |
| `runner__tail_logs` | 2 | Continuous-mode pod isn't iter 1. |
| `author__delete_scenario` | 2 | Defensive UX (confirm: true) not needed for iter-1 PoC. |
| `runner__get_events` | 4 | Needs the events schema + chain-writer (iter 5a). |
| `runner__verify_event` | 5a | Chain integrity check; needs chain library online. |
| `runner__get_dashboard_url` | 5d | Argus-branded website not iter 1; the operator Grafana URL surfaced directly by the skill. |
| Split into two binaries (spec 02) | 2 | Evidence-driven. If iter-1 single-MCP topology has problems, iter 2 splits. |

## Functional requirements

| FR | Requirement |
|---|---|
| FR-1 | Tool `validate_config({instance_id, config_path?})` runs argus's `preflight.py` and returns structured pass/fail + a list of errors with line numbers. |
| FR-2 | Tool `run({instance_id, layer?, tag?})` invokes argus's `run.sh` with the corresponding flags and returns the run's overall status + path to the report. Blocks until the run completes (no async for Phase 0). |
| FR-3 | Tool `get_report({instance_id})` returns the parsed `results/${INSTANCE_ID}/report.json` as a structured response. |
| FR-4 | Every tool call begins with the standardized bootup ritual: validate `instance_id` is known → resolve scenario path + config path for that instance → run preflight → only then execute the tool body. Failures in any bootup step are returned as structured errors before the tool body runs. |
| FR-5 | The MCP server is stateless — no in-memory map of `instance_id → state`. All instance metadata resolves from disk on every call. |
| FR-6 | The MCP server is consumable from Claude Code, Codex, and Gemini via standard MCP transport (HTTP+SSE for Phase 0; stdio variant deferred). |
| FR-7 | Every tool emits structured logs (Begin + Completed pair, correlation_id, instance_id, duration_ms) and Prometheus metrics (`argus_mcp_tool_calls_total`, `argus_mcp_tool_duration_seconds`). |

### Day-2 additions (Phase 1.5 / Phase 2)

| FR | Requirement |
|---|---|
| FR-D1 | Tool `list_runs({instance_id, limit?, since?})` returns the most recent N runs for an instance, ordered DESC by timestamp. Each row: `{run_id, started_at, completed_at, status, summary, report_url, chain_id, chain_block}`. Default `limit=10`, max `100`. **DELIVERED 0.3.29 (V28-016), with three corrections — read them, this row's shape is no longer current.** (1) It lives on the AUTHOR plane as `author__list_runs`, not the in-env runner plane: the run ledger is the control plane's, it is cross-machine, and it survives a torn-down executor. (2) `chain_id` / `chain_block` / `report_url` are DROPPED — chain library chain checkpointing is not running and nothing serves a URL per run; the report is fetched by id. (3) It gained `layer?` / `tag?` (already in `store.RunQuery`), each row gained `report_available`, and `limit` defaults to 20. The deferral reason this row was written under is obsolete: the run store EXISTS and `GET /api/runs` already served this query. |
| FR-D2 | Tool `get_events({instance_id, event_type?, since?, limit?})` lists Argus audit events (per spec 24a FR-5..FR-15: `scenario_authored`, `scenario_run_completed`, `oauth_client_created`, etc.). Default returns last 25 across all event types. Filter by `event_type` or `since` (ISO8601). |
| FR-D3 | Tool `get_sagas({instance_id, correlation_id})` returns the Filum saga timeline for the given `correlation_id`: ordered list of `{saga_id, step, step_name?, service, timestamp, duration_ms?, error?, fields, hmac_verified, chain_anchor?}`. Pulled from Loki (primary) + spec 24c `saga_chain_audit` table (for chain anchors). Used by `failure-triage` skill (spec 16). |
| FR-D4 | Tool `verify_event({correlation_id})` resolves the event in Postgres, reads the chain via chain library using its recorded `(chain_id, chain_block)`, compares against the on-disk payload, returns `{verified: bool, reason?, mismatched_fields?[]}`. Per spec 24a FR-20..FR-22. Handles three cases: verified true, not-yet-checkpointed (chain_id NULL), mismatch. |
| FR-D5 | All Day-2 tools follow the same bootup ritual (FR-4) — every call validates the instance first. |
| FR-D6 | All Day-2 tools emit Begin/Completed logs + `argus_mcp_tool_calls_total{tool, status, instance}` metrics per the over-inform principle (spec 20). |
| FR-D7 | All Day-2 tools respect the scope binding from the calling client's API key (spec 22). A `runner`-scoped token can call all Day-2 tools (they're all read-only). |

## Non-functional requirements

| NFR | Requirement |
|---|---|
| NFR-1 | Go 1.24, mario-pattern Go layout (`cmd/argus-mcp-runner/main.go`, `internal/{tools,argus,instance}/`). |
| NFR-2 | HTTP+SSE MCP transport. Port via `MCP_PORT` env var (default 8080). Bearer auth via `MCP_BEARER_TOKEN` env (Phase 0: single token; Phase 1+: per-instance tokens). |
| NFR-3 | Debug-on by default. No `if env == "production"` log gating. Structured `slog` output to stdout in JSON format with required fields: `correlation_id`, `instance_id`, `tool`, `level`, `msg`. |
| NFR-4 | All argus invocations through a single internal package (`internal/argus/`) — the MCP server doesn't `exec` argus scripts directly from `internal/tools/`. Keeps the boundary auditable. |
| NFR-5 | The argus invocation respects `INSTANCE_ID` parameterization: `--config=${ARGUS_HOME}/instances/${INSTANCE_ID}/argus-config.yaml`, `--scenarios=${ARGUS_HOME}/instances/${INSTANCE_ID}/scenarios/`, `--results-dir=${ARGUS_HOME}/results/${INSTANCE_ID}/`. |
| NFR-6 | Errors are never swallowed. Every `if err != nil` emits an ERROR log with the full error chain + the intent that was in flight. CI `lint-observability` job enforces this. |
| NFR-7 | The bootup ritual is a single internal function `instance.Bootstrap(ctx, instanceID)` called as the first line of every tool handler. Unit tests verify this. |
| NFR-8 | `replicas: 3` once the MCP ships as a k8s Deployment (Phase 1+). For Phase 0 spike, local binary only — replica count N/A. |

## Inputs / Outputs

### Tool: `validate_config`

**Request**
```json
{
  "instance_id": "local",
  "config_path": "examples/order-service/argus-config.yaml"
}
```
`config_path` is optional; defaults to the instance's resolved config.

**Response — success**
```json
{
  "valid": true,
  "scenarios_found": 15,
  "layers_configured": ["http-ingestion", "message-flow", "database-state", "external-delivery", "error-path", "rate-limiting", "permissions"],
  "preflight_duration_ms": 142,
  "correlation_id": "abc-123"
}
```

**Response — failure**
```json
{
  "valid": false,
  "errors": [
    {"file": "argus-config.yaml", "line": 12, "message": "scenarios reference ${DB_URL} but database section missing"},
    {"file": "scenarios/http-ingestion/ORD-001.md", "line": 3, "message": "Layer 'HTTP Igestion' is not a valid layer name"}
  ],
  "correlation_id": "abc-123"
}
```

### Tool: `run`

**Request**
```json
{
  "instance_id": "local",
  "layer": "http-ingestion",
  "tag": "critical"
}
```
`layer` and `tag` are optional. Both omitted → run all scenarios on the instance.

**Response**
```json
{
  "status": "passed",
  "summary": {"total": 15, "passed": 15, "failed": 0, "skipped": 0},
  "report_path": "results/local/report.json",
  "duration_ms": 8723,
  "correlation_id": "abc-123"
}
```
`status` is one of `passed | failed | partial`. `partial` only on Phase 1+ when async runs land.

### Tool: `get_report`

**Request**
```json
{
  "instance_id": "local"
}
```

**Response — pass-through of argus's `report.json`**
```json
{
  "project": "order-service",
  "timestamp": "2026-05-19T14:30:00Z",
  "summary": {"total": 15, "passed": 14, "failed": 1, "skipped": 0},
  "layers": [
    {
      "layer": "http-ingestion",
      "scenarios": [
        {"id": "ORD-001", "status": "passed", "duration_ms": 45, "correlation_id": "tr-001",
         "cleanup": [{"form": "sql", "outcome": "ok", "duration_ms": 12,
                      "observed": "ran to completion (a cleanup that found nothing to remove is a success)"}]},
        {"id": "ORD-002", "status": "failed", "duration_ms": 10004, "correlation_id": "tr-002",
         "failure": "Expected status=409 but got status=200"}
      ]
    }
  ],
  "correlation_id": "abc-123"
}
```

⚠ **`cleanup[]` is an EXTENSION added in 0.3.31 (V29-015)** — one entry per declared `## CLEANUP`
block, in the order written: `form` (`sql`/`bash`/`na`), `outcome` (`ok`/`failed`/`timeout`/`not-run`),
`duration_ms` and a reality-only `observed`. ⛔ It NEVER affects the scenario's verdict, and it never
carries the command's output (there is no Go-side secret scrubber, and this file is fetched by agents).
Also extended in the same release: `residue` (VR12-CH1 rule 7) and `unexecuted[]` (VR12-E13).

## Dependencies

| Dependency | Form | Notes |
|---|---|---|
| `argus` submodule | git submodule at `../argus/` (relative to `mcp-runner/`) | Phase 0: SHA `HEAD` of main. Phase 1+: SHA-pinned. |
| `argus/scripts/run.sh` | invoked via `os/exec` from `internal/argus/runner.go` | Pass `--config`, `--scenarios`, `--results-dir`, `--instance-id`, optional `--layer`, `--tag`. |
| `argus/scripts/preflight.py` | invoked via `os/exec` from `internal/argus/preflight.go` | Pass config + scenarios paths. Parse stdout (JSON). |
| MCP Go SDK | **Open — Aleksander to decide.** Three options on the table (see Open questions below). | Mario uses `mark3labs/mcp-go` (community library). **MCP-router uses a native in-house impl** (raw jsonrpc 2.0 in `cmd/mcp-gateway/handlers/` + `internal/router/`, no third-party MCP SDK). Aleksander has prior MCP-server experience from his social-media MCP — his call which path fits Argus best. |
| Structured logging | `log/slog` from stdlib | JSON handler. |
| Prometheus client | `github.com/prometheus/client_golang` | Standard the operator pattern. |

## File layout

```
mcp-runner/
├── cmd/argus-mcp-runner/
│   └── main.go                  # Entry: load config, register tools, start HTTP+SSE server
├── internal/
│   ├── tools/
│   │   ├── validate_config.go
│   │   ├── run.go
│   │   ├── get_report.go
│   │   └── tools_test.go        # Per-tool unit tests w/ fake argus runner
│   ├── argus/
│   │   ├── preflight.go         # Wraps argus/scripts/preflight.py
│   │   ├── runner.go            # Wraps argus/scripts/run.sh
│   │   ├── report.go            # Parses results/${INSTANCE_ID}/report.json
│   │   └── argus_test.go
│   ├── instance/
│   │   ├── bootstrap.go         # Bootup ritual: validate → load → preflight
│   │   ├── resolver.go          # instance_id -> config path, scenarios path, results path
│   │   └── instance_test.go
│   └── obs/
│       ├── logs.go              # Structured slog setup
│       ├── metrics.go           # Prometheus counters + histograms
│       └── obs_test.go
├── deployments/k8s/base/        # Phase 1+: Kustomize base
├── Dockerfile                   # Phase 1+: multi-arch via multi
├── go.mod
└── README.md
```

## Testing strategy

### Unit tests (`internal/*_test.go`)

- Each tool handler: bootup ritual called first (fail if not); structured response shape; structured error on bootup failure.
- `internal/argus/preflight.go`: parses argus's preflight JSON output, surfaces line-level errors.
- `internal/argus/runner.go`: wraps argus invocation, parses exit code → status.
- `internal/instance/bootstrap.go`: rejects unknown instance_id; rejects malformed config; succeeds on a fixture.

### Integration test (Phase 0 acceptance)

1. Spin a local argus checkout in `testdata/argus-fixture/` with an `examples/order-service/` analog.
2. Start `argus-mcp-runner` against `INSTANCE_ID=local`.
3. Issue: `validate_config({instance_id: "local"})` → expect `valid: true`, `scenarios_found > 0`.
4. Issue: `run({instance_id: "local"})` → expect `status: passed` (or `failed` if scenarios intentionally fail).
5. Issue: `get_report({instance_id: "local"})` → expect parsed report matching `run`'s outcome.

### Ergonomics test (Phase 0 gating)

Drive from Claude Code via MCP:

- "Validate the local instance's configuration." → AI picks `validate_config`, instance_id="local", calls it.
- "Run all scenarios for the local instance and show me the report." → AI picks `run` then `get_report`.
- "Re-run the http-ingestion layer only." → AI passes `layer` to `run`.

If the AI gets stuck, names the wrong tool, or chains incorrectly, the design has an ergonomics bug — revise the tool names, descriptions, or parameter shape BEFORE proceeding.

### Lint-observability (CI, deferred to spec 20)

Static checks for new code:
- Every `if err != nil` in tool handlers emits a structured ERROR
- Every tool body is preceded by `instance.Bootstrap(...)`
- Every tool emits `argus_mcp_tool_calls_total{tool, status, instance}`
- No bare-string log lines

## Open questions

| Question | Resolution path |
|---|---|
| **MCP Go SDK / protocol layer — Aleksander to pick (iter-1 day-1 blocker).** Three options: | **(a)** `github.com/mark3labs/mcp-go` (community library; mario-mcp uses it; fastest bootstrap). **(b)** Vendor/share MCP-router's native impl (raw jsonrpc 2.0 in `cmd/mcp-gateway/handlers/` + `internal/router/`; matches the operator house pattern; benefits from MCP-router's auth + permissions code; adds coupling to MCP-router's evolution). **(c)** Native in-house, written fresh (full control; duplicates MCP-router's work; only justifiable if (a) and (b) both block on something). Aleksander brings prior MCP-server experience (social-media MCP) — recommend he scan MCP-router's protocol code + mark3labs/mcp-go and pick within day 1 of iter 1. |
| Stdio transport for local CLI use, in addition to HTTP+SSE? | Defer to Phase 1+ unless ergonomics test reveals it's needed sooner. |
| Where does the MCP server resolve `instance_id` from? Env var, config file, env-substituted scenario root path? | Phase 0: `${ARGUS_HOME}/instances/${INSTANCE_ID}/`. Phase 1+: pluggable resolver. |
| Should `run` be sync or async? Argus runs can take 10+ minutes for full suites. | Phase 0: sync. Phase 1+: `run_async` + `get_run_status` if the AI ergonomics demand it. |
| Per-instance bearer token model | Phase 0: single global token via env. Phase 1+: per-instance issuance — defer to spec for security spec. |
| How does the bootup ritual handle a partially-set-up instance (config present, scenarios missing)? | Bootstrap returns a structured error with which step failed. The AI agent can then call `argus-mcp-author.write_scenario` to fix. |
