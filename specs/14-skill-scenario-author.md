# Spec 14 — `skills/scenario-author/SKILL.md`

> **Status:** Draft · **Owner:** a tester · **Phase:** Phase 1 · **Iteration:** 1 · **Last updated:** 2026-05-21 (Day-5: CC-1 tool-name rewrite — `onedroid_argus_author.*` → `argus_mcp.author__*`)

> ⚠️ **CC-1 tool-name rewrite (Day-5):** Tool references in this spec say `onedroid_argus_author.<tool>` (e.g. `onedroid_argus_author.write_scenario`). Per Day-5 single-MCP topology, the actual tool calls become `argus_mcp.author__<tool>` (e.g. `argus_mcp.author__write_scenario`). When implementing the SKILL.md body, use the `author__*` namespace form. Spec text below uses the older `onedroid_argus_author.*` form for readability — read as the namespaced form.

## Purpose

`scenario-author` is the AI test-agent skill that turns plain-English testing intent into a structured argus scenario `.md` file. It is the **initializer** in the initializer/worker pattern: it bootstraps a structured contract from a user prompt, then exits. It does not run scenarios, does not triage failures, does not modify product code.

This skill is the load-bearing piece of *contracts, not conversations* (design principle #5). The user describes intent in English; the skill emits a markdown contract that `parse_scenario.py` accepts; the runner executes the contract; `report.json` says whether the system met the contract. There is no chat history retained as state — only the `.md` file is the artifact.

## Functional requirements

| FR | Requirement |
|---|---|
| FR-1 | Triggers on prompts like "write a scenario that…", "test that…", "verify…", "add a scenario for…", "we need a test for…". The skill description includes these as activation keywords. |
| FR-2 | Performs the **standardized bootup ritual** (validate instance → load config + existing scenarios → run preflight) by calling `onedroid_argus_runner.validate_config({instance_id})` BEFORE drafting anything. If preflight fails, the skill reports the error and stops — it does not draft a scenario against a broken config. |
| FR-3 | Lists existing scenarios in the relevant layer via `onedroid_argus_author.list_scenarios({instance_id, layer})` to understand the conventions (ID prefix, tag patterns, payload variable names) before drafting. |
| FR-4 | Reads 1-3 reference scenarios from the same layer via `onedroid_argus_author.read_scenario` — uses them as style anchors for the draft, not as content to copy. |
| FR-5 | Calls `onedroid_argus_author.propose_scenario({instance_id, intent, target_service, layer, examples})` to get the structured draft + the `needs_user_review` flags. |
| FR-6 | Validates the draft via `onedroid_argus_author.validate_scenario({instance_id, markdown})` BEFORE writing. If validation fails, the skill iterates (up to 3 times) — each iteration tightens specific fields the validator flagged. After 3 failed iterations, the skill returns the validator errors verbatim and asks the human for guidance. |
| FR-7 | Surfaces `needs_user_review` items to the human for explicit confirmation BEFORE writing. These are typically: cleanup queries (test-isolation choice), unique-ID assignment (when the proposed ID collides), tag choice, timeout selection. The skill never silently picks for the human on items the proposer flagged as ambiguous. |
| FR-8 | On human approval, calls `onedroid_argus_author.write_scenario({instance_id, path, markdown})`. The skill never writes without explicit human approval — *contracts not conversations* extends to *human sign-off before contract lands*. |
| FR-9 | After write, returns the path + SHA256 + a one-paragraph progress note. The progress note goes into the PR description or session log. |

## Non-functional requirements

| NFR | Requirement |
|---|---|
| NFR-1 | The skill is **stateless** — it bootstraps an artifact, then exits. No long-term memory beyond the on-disk scenario file. Subsequent edits to the same scenario invoke the skill again with the existing file as input. |
| NFR-2 | The skill operates as the **test agent**, NOT the product agent. The product agent never invokes `scenario-author` (they don't have the author MCP token; spec 02 enforces). |
| NFR-3 | **Context engineering, not prompt engineering** (principle 5). The skill assembles config + schema + intent + examples into a structured input for the proposer — it does not concatenate user prompts. |
| NFR-4 | **Bounded reasoning** (anti-pattern: open-ended loops). The skill picks among the **8** fixed argus layers (NFR-7 names them; `Web UI` is the eighth) + the known service config + a finite ID space. Iterations cap at 3; if the validator can't be satisfied in 3 tries, escalate to human. |
| NFR-5 | Every tool call emits Begin/Completed log pairs with `correlation_id`, `instance_id`, `tool`, `duration_ms`, `result` (per over-inform principle, spec 20). |
| NFR-6 | The skill is described in a single `SKILL.md` file (~200-400 lines). No additional code — it's a prompt with structured tool-call instructions. |
| NFR-7 | Domain-specific schemas, never generalized (principle: opinionated schemas). The skill only knows the 8 argus layers + the TRIGGER/VERIFY/EXPECT/CLEANUP/TIMEOUT/References sections and the ID/Layer/Tags/Target Metadata keys (⚠ `Priority` was REMOVED in 0.3.31 — V30-003; `Web UI` is the eighth layer). It does not invent new fields. |

## Inputs / Outputs

### Inputs

The skill receives:
- **Free-form English intent** from the user (e.g. "verify a duplicate order with the same idempotency-key returns 409 Conflict")
- **`instance_id`** (required; user passes explicitly or the skill asks)
- **Optional hints:** target service name, target layer, reference scenario IDs

### Outputs

- A committed `.md` file at `scenarios/<INSTANCE_ID>/<layer>/<ID>-<slug>.md`
- A one-paragraph progress note (to the user, the PR, the session log)
- Begin/Completed log pairs + `argus_mcp_tool_calls_total{tool, status, instance}` metrics

### Failure modes (explicit)

| Condition | Skill behavior |
|---|---|
| Bootup ritual fails (preflight error) | Stop. Report the preflight error verbatim. Do not draft. |
| Proposer returns a draft that fails validation | Iterate up to 3 times; on 4th failure, return validator errors + ask the human. |
| Proposed ID collides with an existing scenario | Surface the collision as a `needs_user_review` item. Propose 1-2 alternatives. Wait for human pick. |
| User asks for a scenario in a layer not configured for the instance | Refuse. Report which layers ARE configured. Do not silently draft. |
| User asks for a UI-layer scenario but argus-config.yaml has no UI target | Refuse — and say WHY: the LAYER is shipped (`Web UI`, the eighth; V29-021 gave it an executed `## EXPECT` grammar), but this SUT declares no UI target for it to run against. ⛔ Not "deferred to Phase 2" — that answer predates 0.3.32 and sends an author away from a layer the product now supports. |

## SKILL.md frontmatter

```yaml
---
name: scenario-author
description: |
  Use this skill when the user (or another agent) describes a testing scenario in plain English
  and wants it turned into an argus markdown contract that the suite can execute.
  Triggers on: "write a scenario that...", "test that...", "verify...", "add a scenario for...",
  "we need a test for...", "spec out a check for...".

  DO NOT use this skill to:
    - Run scenarios (use scenario-runner)
    - Triage failures (use failure-triage)
    - Modify product code (out of scope)

  This skill operates as the test agent. It requires argus-mcp-author access.
---
```

## Skill body — required sections

The `SKILL.md` body MUST include these sections in order:

1. **Operating philosophy** — three paragraphs: initializer pattern, contracts not conversations, bounded reasoning.
2. **Bootup ritual** — exact sequence: `validate_config` → `list_scenarios` → `read_scenario` for examples → `propose_scenario`. Show the tool calls verbatim.
3. **Iteration loop** — pseudocode: propose → validate → if invalid, refine using validator output → cap at 3 iterations → escalate.
4. **Human sign-off contract** — when to ask; what `needs_user_review` items always trigger a question; how to phrase the question.
5. **Writing the file** — `write_scenario` call shape, expected response handling, what to do if write fails (path collision, validation drift between propose-time and write-time).
6. **Progress note format** — exact template, where it goes.
7. **Examples (3-5)** — full prompt → tool-call sequence → output. Each example exercises a different layer.
8. **Anti-patterns** — list with one-line explanations.

## Anti-patterns (must be listed in SKILL.md)

- **"Just write the markdown directly."** No — use `propose_scenario` then `validate_scenario`. Direct authorship skips the parser's invariants and creates contracts the runner can't execute.
- **"Make up an ID."** No — propose IDs that fit the existing prefix scheme. If proposer returns an ID, defer to it; if proposer asks, surface to human.
- **"Skip CLEANUP because the test data is small."** No — `## CLEANUP` is REQUIRED and it is ENFORCED: `scenario.Validate` (Go — ⚠ not `parse_scenario.py`, which is the upstream argus script and never checked this) refuses a scenario whose CLEANUP is neither a runnable ```sql / ```bash block nor `N/A` + a justification, on both author paths. And since 0.3.31 the block is actually EXECUTED after every scenario, on every path (V29-015) — before that it was parsed and read by nothing, while four documents including this one said otherwise.
- **"Batch-create three scenarios in one tool call."** No — one scenario per skill invocation. This preserves the atomic-progress principle and keeps the progress note actionable.
- **"Hardcode `localhost` or specific URLs."** No — use `${VAR}` references, filled in from the environment of the process that runs the scenario (a leading `${VAR}` in an HTTP TRIGGER stands for the base URL in `argus-config.yaml`) — never inside a check (V31-003). Hardcoded values break the multi-instance model.
- **"Write to the product repo's test directory."** No — scenarios are the holdout set. They live in Argus's `scenarios/` tree, never in the product repo (dark-factory boundary).

## Examples (must appear in SKILL.md)

### Example 1 — Happy-path HTTP ingestion

```
User: write a scenario that verifies a valid order with one item gets accepted

Skill:
  1. validate_config({instance_id: "local"}) → valid: true, 12 scenarios, 7 layers
  2. list_scenarios({instance_id: "local", layer: "http-ingestion"}) → [ORD-001, ORD-002, ORD-003]
  3. read_scenario({instance_id: "local", scenario_id: "ORD-001"}) → returns markdown + parsed fields
  4. propose_scenario({instance_id: "local", intent: "verify a valid order with one item gets accepted",
                      target_service: "order-api", layer: "http-ingestion", examples: ["ORD-001"]})
     → draft markdown, suggested_id: "ORD-005", needs_user_review: ["Cleanup query: DELETE by customer_id?"]
  5. ASK: "Confirm cleanup is DELETE FROM orders WHERE customer_id = '${test_customer_id}' — OK?"
  6. validate_scenario({instance_id: "local", markdown: <draft>}) → valid: true
  7. write_scenario({instance_id: "local",
                     path: "scenarios/local/http-ingestion/ORD-005-single-item.md",
                     markdown: <draft>}) → written: true, sha256: abc...
  8. Return progress note: "Wrote ORD-005 (HTTP Ingestion, one-item happy path) to scenarios/local/http-ingestion/."
```

### Example 2 — Validation iteration

```
User: add a scenario for the 401 case

Skill: (bootup ritual ok)
  - propose_scenario(...) → draft
  - validate_scenario(...) → invalid: "Layer 'HTTP Igestion' is not a recognized layer name" (typo)
  - Refine layer name → re-validate → valid
  - ASK human about cleanup
  - write_scenario(...) → written
  - Return progress note.
```

### Example 3 — Escalate after 3 failures

```
User: write a scenario for the saga timeout edge case

Skill: (bootup ritual ok)
  - propose_scenario(...) → draft 1 → invalid (TIMEOUT missing)
  - Refine → draft 2 → invalid (CLEANUP missing)
  - Refine → draft 3 → invalid (`${SAGA_TIMEOUT_THRESHOLD}` in a check is never filled in)

  Skill: "I drafted three versions; each failed validation on a different field. Final error:
          `${SAGA_TIMEOUT_THRESHOLD}` in a check is never filled in — a check may carry only `${cid}` /
          `${correlation_id}` (and, in a chain, `${saved.<var>}`). Tell me the threshold's value and I
          will write it into the check. Drafts attached."
```

## Dependencies

| Dependency | Form |
|---|---|
| `argus-mcp-author` (spec 02) | MCP server with `list_scenarios`, `read_scenario`, `propose_scenario`, `validate_scenario`, `write_scenario` tools |
| `argus-mcp-runner` (spec 01) | MCP server with `validate_config` tool (for bootup ritual) |
| Test-agent API key | Scoped to author MCP (per spec 02 dark-factory enforcement) |

## File layout

```
skills/scenario-author/
├── SKILL.md                    # The skill file (this spec's primary artifact)
└── examples/                   # Optional: longer example transcripts for the skill's regression test
    ├── 01-happy-path-http.md
    ├── 02-validation-iteration.md
    └── 03-escalation.md
```

## Testing strategy

### SKILL.md self-test (manual)

1. Open Claude Code with `argus-mcp-author` connected.
2. Issue: "Write a scenario that verifies a duplicate order returns 409." (real prompt against a real instance).
3. Verify: skill calls bootup ritual → list/read existing scenarios → propose → ask human about cleanup → validate → write.
4. Confirm the resulting `.md` file passes argus `preflight.py`.

### Regression test (in CI)

A regression harness drives the skill against a fixture instance with:
- 5 plain-English prompts (one per layer in scope: HTTP ingestion, message flow, database state, error path, permissions)
- For each: expected layer + expected ID prefix + expected scenario file produced
- The test asserts the file exists, passes preflight, and is byte-identical to a golden reference (modulo correlation_ids)

Goldens regenerate when the proposer's prompt template legitimately changes (manual review).

### Ergonomics test (Phase 1 gating)

A human (a tester/Bartek) drives the skill through 10 free-form requests. If 8+ ship to disk without escalation, the skill is ready. If <8, iterate the skill's prompt structure.

## Open questions

| Question | Resolution path |
|---|---|
| Should the skill be allowed to author scenarios that span multiple layers (`HTTP Ingestion -> Database State`)? | Yes — argus supports this. The proposer can chain layer names with `->`. Examples include chained scenarios. |
| Does the skill auto-add Day-2 chain checkpointing? | No — chain checkpointing is per-event (spec 24a), not per-scenario-author-call. The author writes the contract; the runner emits the chain entry when the scenario is run. |
| Should the skill propose scenarios that test the website itself (Phase 2.5)? | Phase 2+; defer until the website is deployed and argus has a Playwright/Clerk layer wired up. |
| Should the skill be a Claude Code "slash command" or a regular skill? | Regular skill — triggers on natural-language intent, not an explicit command. |
