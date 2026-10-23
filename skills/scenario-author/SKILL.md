---
name: scenario-author
description: Turn a plain-text description / requirements text into argus-valid OneDroid Argus scenarios (propose → validate → review → write) using the cloud onedroid_argus_author MCP tools. Use when authoring E2E test scenarios for a service under the OneDroid Argus MCP (test-agent hat only).
---

# scenario-author

You are the **initializer**: turn plain-text intent into one or more argus-valid scenario `.md`
files via the MCP `author_*` tools, then exit. You do NOT run scenarios (that's `scenario-runner`)
and you do NOT fix product code (that's the product agent). One testable unit per run — bootstrap
the artifact, then stop (the *atomic, observable progress* rule).

> **Holdout note.** Scenarios are the dark-factory holdout. They are authored ONLY in the test-agent
> hat, on the CLOUD control plane — the `onedroid_argus_author` MCP connection (the `author_*` tools
> the control plane lists). Writes land in the cloud scenario CATALOG, never on disk. The in-env
> `onedroid_argus_runner` connection (the `runner_*` tools) — the product agent's only connection — has
> no `author_*` tools and its folder has no scenarios; never paste scenario text into a product-agent window.

> **About this copy.** This is the skill OneDroid Argus installs in the tester's folder, published here to read. It
> describes the **released** executor (v0.3.74) and the hosted control plane, so it can be ahead of the code in
> this repository. Executor versions named below are the release that first carries a feature. Install OneDroid Argus and
> read the guides at https://docs.onedroid.ai/argus-quickstart and https://docs.onedroid.ai/argus-tester-guide.

## Your instance id — the operator gives it to you; you never read or guess it
Every `author_*` and `runner_*` tool takes `instance_id`, and it is REQUIRED. `<instance>` in every example
below is your **registered instance id** (e.g. `orderservice-compose`) — the SAME value on both connections;
the router routes on it and rewrites it for the executor. **If you do not have it, STOP and ASK the operator
before making ANY tool call** — say that `instance_id` is required on every tool. Never read it from a file,
never infer it from the folder, never pick one: one agent folder can be wired to several instances, so a
guess authors into the wrong instance's catalog. The operator can read it off the ALL SET banner of their
onboarding run, the Environments page, or `argus router folders`. `local` is the executor's own name, not
a value you pass.

**Who resolves what (D9):** a wrong or misspelled argument NAME you correct yourself — `tools/list` describes
every tool and every argument (what it is, required or optional, an example of its shape, what an omission
means), and the plane refuses an unknown argument naming the accepted set. A VALUE you cannot know — which
instance, which run — you ask the operator for.

## Bootup ritual (every run)
1. Confirm BOTH test-agent MCP connections are present:
   - `onedroid_argus_author` (cloud control plane) — the `author_*` tools you author with. Writes land in the
     cloud CATALOG, never on disk.
   - `onedroid_argus_runner` (in-env) — the `runner_*` tools.
   Through the local router both sets arrive on one `argus` connection. If the `author_*` tools are missing, read
   the text OneDroid Argus gave at the start of this session: when the control plane could not be read, it says why and
   asks you to reconnect.
2. `runner_validate_config({instance_id:"<instance>"})` (the in-env `onedroid_argus_runner` connection) — confirm the
   SUT config is valid and see the configured targets/layers before you author against them. **Never pass a
   config path** — the server owns exactly one config (single-tenant).

## Starting from a real repo (T5.1+T5.3) — run this FIRST when a SUT source tree is available

When you have (or can get) a checkout of the SUT's own repo, run `argus propose-from-repo` **before**
hand-authoring anything — it turns real HTTP routes into reviewable scenario DRAFTS instead of you
guessing endpoints from a description:

```
argus propose-from-repo --repo <path-to-sut-repo> --out <local-scratch-dir> [--config <argus-config.yaml>] [--json]
```

- It needs **no token** (like `package-check`) and touches `--repo` **READ-ONLY** — it never writes,
  executes, or installs anything there. Everything it writes lands under `--out` only, and it REFUSES
  outright (nothing written) if `--out` resolves inside `--repo`.
- It extracts routes from OpenAPI/Swagger documents and from common Fastify/Express/Go
  (net/http, gorilla mux, chi, gin) route-registration shapes, and proposes one `HTTP Ingestion`
  scenario draft per route — GET routes get `status=200` as the runnable claim. A route with a path
  parameter (`/orders/:id`) is written under `<out>/.needs-values/` instead of `<out>/`, because no
  value is invented for it: `validate-config` and seeding skip dot-directories, so it cannot run
  until you fill the value in and move it out. Test files (`*.test.*`, `*.spec.*`, `__tests__/`) are
  never read, and outbound client calls (`axios.create(...)`, `clients/`) are not taken for routes.
- Every draft is gated through the SAME two doors a hand-authored scenario must pass: it runs
  `validate-scenario` and, when `--config` declares `money_handling: true`, the money-path guard
  (T5.4) — a draft that fails either is **refused and named**, never written. Pass `--config` for any
  money-handling SUT: only probe-style GETs are then proposed (`health`, `healthz`, `ready`, `live`,
  `ping`, `metrics`, a root `/status`, `/info`, `/version`, `/.well-known/…`). Every other route is
  listed as not proposed, or refused with the guard's reason. (`money_handling: true` is the system
  owner's switch: it means "refuse what my `money_writes.allow` does not name". This command proposes
  against the switch alone and never consults the owner's list; a check the owner has listed — an HTTP
  route, a bus op, a tool — is written by hand, and the money guard (`validate-config`, the control plane
  and the run) accepts it exactly when an entry names it.) A GET can still create state
  (`GET …/deposit-address/:chainId` can mint an address), so this rule does not trust the method.
- Without `--config`, a non-GET route is still proposed — clearly tagged `proposed`, with a
  `### Non-runnable` note that it WRITES to the SUT and needs a human decision before it runs.
- The summary (and `--json` report) also lists migration/schema files and README/ARCHITECTURE/
  REQUIREMENTS docs it found but did **not** turn into scenarios — read those yourself before
  authoring the layers propose-from-repo cannot reach (Database State, Message Flow, …).

**Your job after running it:** read the printed drafts and refusals, **review every accepted draft
against the SUT source** (a proposed draft is an honest starting point, not a certified one — it
never invents a value it was not given), fix up anything a human review flags, then continue with
the normal propose → validate → review → write flow below for each one — through the CLOUD
`author_*` tools, since `propose-from-repo`'s own `--out` is a local scratch directory, not the
scenario catalog.

## The flow (propose → validate → review → write) — on the cloud `onedroid_argus_author` connection
Below, `<instance>` is your registered instance id (the operator gave it to you — see above).
1. **Propose.** `author_propose_scenario({instance_id:"<instance>", description:"<plain text>", target_service:"<svc>", layer:"<layer>"})`
   returns a skeleton draft + `needs_user_review[]`. The skeleton is an HONEST initializer — it does
   NOT know the real endpoint/method/EXPECT; fill those from the SUT source/config. Never silently
   invent a value — surface every guess in `needs_user_review[]`.
   > **The corrected order: anchor the commit, then propose, then seal, then request a final run.**
   > When the control plane has a ledger configured, the commit under certification is anchored FIRST
   > — `author_anchor_commit` creates a draft commitment and writes its version-1 ledger record —
   > before any certification content exists. Only then does `author_propose_scenario` write into the
   > instance's **certification set**: accepted while that draft still carries its version-1 anchor,
   > refused by name before the anchor exists and again once the commitment is sealed (the
   > certification set is frozen from that point on). `author_seal_set` then seals the certification
   > set itself — draft becomes sealed, and the set's root is anchored as version 2 of the SAME record,
   > refused if the certification set is empty. Only a commitment sealed with both versions present
   > accepts a `final` run request. If you get the before-the-anchor refusal, call
   > `author_anchor_commit` first; if you get the after-the-seal refusal, anchor a new commitment to
   > propose again.
   >
   > **An instance holds several sets at once — several drafts and several sealed ones — each owned
   > by ONE commitment** (several tester sessions may certify on one instance at the same time). An
   > `author_anchor_commit` with NEW content (commit digest, runner digest, image digests or criteria)
   > creates a NEW draft with its own `commitment_id` and never touches the others; the SAME content as an
   > existing open draft returns THAT draft with `already_anchored: true` and anchors nothing new, so a
   > retry after a dropped response is safe. A draft you do not want is removed with
   > `author_discard_draft({instance_id, commitment_id})` (drafts only; the control plane's own rows of
   > its version-1 anchor go with it, so `author_list_ledger_anchors` stops listing an anchor that still
   > exists on chain; the discard response reports it once, under `on_chain_v1`). Two sessions that anchor
   > IDENTICAL inputs (commit digest, runner digest, image digests, criteria) share ONE draft
   > (`already_anchored: true`); a second, separate set for the same commit needs a differing input. A scenario belongs to the set of the commitment it was written into, and a revealed set's
   > scenarios never roll into the next draft. `author_list_commitments` lists them (state,
   > `scenario_count`, pinned schedules). With exactly one commitment in the state a call needs, you
   > name nothing and it behaves as before. With several, pass `commitment_id` to `author_propose_scenario`,
   > `author_write_scenario`, `author_seal_set` (drafts), and to `author_request_run` (`rehearsal`:
   > a draft; `final` / `scheduled`: a sealed one) or `author_set_schedule` (a sealed one); without
   > it the call is refused by name and lists every candidate. `author_read_scenario` and
   > `author_delete_scenario` do the same for an id or path that exists in several sets (name `set_kind`
   > `"build"` or a `commitment_id`). No write, seal, run, schedule, read or delete ever picks "the most
   > recent" for you. The one exception is a read kept for compatibility: `author_get_commitment` with no
   > `commitment_id` returns the most recent commitment AND lists every commitment of the instance
   > (`commitments`) beside it, with a note when there are several.
2. **Validate.** `author_validate_scenario({instance_id:"<instance>", content:"<draft>"})` → fix until
   `{"valid": true}`. Heed `warnings[]` (an EXPECT the layer can't execute — or one the instance's declared
   targets can't cover — is flagged, not silently dropped).
3. **Review.** Show the developer the draft(s) + `needs_user_review[]`; let them correct/approve.
4. **Write.** `author_write_scenario({instance_id:"<instance>", path:"<layer>/<ID>-<slug>.md", content:"<approved>"})`
   — writes to the cloud scenario CATALOG (the registry), NOT to disk; `path` is the registry key (absolute
   paths and `..` escapes are rejected). It validates-then-writes atomically and returns the resolved
   `scenario_id` + `layer` (parsed from the content). (C9) **First run `author_list_scenarios` and mirror the
   layout it reports** — the catalog usually keys scenarios under a `<layer>/` prefix, so write to that SAME
   prefix (e.g. `database/<ID>.md`), not one level off. Use `author_read_scenario` to avoid ID clashes.

## Checking your scenario actually ran
`author_list_runs({instance_id:"<instance>"})` lists the instance's runs newest first — `run_id`,
`started_at`, `completed_at`, `status` and the `summary` tallies, optionally narrowed by `limit`
(default 20, max 100) / `since` (RFC3339) / `layer` / `tag`. ⛔ Never assume the newest run contains the
scenario you just authored: the lister does not know which run holds which scenario id, so narrow by
`layer`/`tag`, pick a `run_id`, and read THAT report.

## The argus scenario format (produce exactly this)
```markdown
# Scenario: <human-readable name>

## Metadata
- **ID**: <id>                    # ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$ — letters, digits, _ and -;
                                  # max 64, no spaces; any case (e.g. race-001, neo4j_health,
                                  # ORD-001). Keep it unique — but the SCOPE at which uniqueness
                                  # is enforced (per instance / per folder / per repo) is an
                                  # OPEN question, so today only the FORMAT is checked.
- **Layer**: <Layer> [-> <Layer>] # one or more of the 8 layers (below)
- **Tags**: <dispatch>, <others>  # ⛔ EXACTLY ONE dispatch tag, REQUIRED: chain | mcp | ui | http
                                  # It selects which ENGINE runs this scenario. `http` is the
                                  # JMeter path — it used to be signalled by ABSENCE, which is why
                                  # 35 scenarios had no tag at all. Two dispatch tags = REFUSED.
- **Target**: <name>              # OPTIONAL — picks a NAMED target (http_targets.<name> etc.)
                                  # instead of the plain slot. Omit it to use the plain slot.
                                  # ⚠ The TRIGGER URL's HOST is never used — only its path. A second
                                  # host needs this line; validate-config warns when it is missing (AC-D21).

## TRIGGER
<METHOD> `${VAR}/path`            # e.g. POST `${INGESTION_URL}/api/v1/orders`
Authorization: Bearer wrong       # OPTIONAL header lines override the default token (4.8) —
                                  # use for permission tests: `Authorization: Bearer bad` (expect 401),
                                  # or `Authorization:` (empty = no token). One auth variant per scenario
                                  # (split multi-variant tests into PERM-001/002/003).
                                  # For HTTP Basic auth use the helper, never a pre-encoded value:
                                  # `Authorization: Basic ${basic_auth:<username>:<PASSWORD_ENV_VAR>}`
                                  # — the password env var NAME, never its value or a base64'd literal.
\`\`\`json
{ ...request body... }
\`\`\`

## VERIFY
<an executable ```sql block, OR "N/A — <reason>">
                                  # ⛔ Those are the ONLY two forms. Prose alone is REFUSED (it
                                  # would verify nothing), and there is no ``` http ``` form.
                                  # ⛔ On a `chain`, `mcp` or `ui` scenario ## VERIFY has no
                                  # meaning and is REFUSED — delete the section.
                                  # In a query, `${correlation_id}` is the ONLY legal placeholder;
                                  # the query must be READ-ONLY and a single statement.

## EXPECT
### Runnable
- <assertion>                     # the claims the product EXECUTES; a code-judged scenario MUST
                                  # carry `status=<code>` here (VR12-E6 refuses it otherwise)

### Non-runnable
- <expected result the product cannot check automatically>

## TIMEOUT
<duration, e.g. 15s>              # ⛔ REQUIRED. Format `^[0-9]+(ms|s)$` — `15s`, `800ms`. And
                                  # ENFORCED AT RUN TIME (VR12-TO-RUN), not only validated at
                                  # authoring time. Ceiling: 30s (60s if the Layer includes Rate Limiting
                                  # or is chained across layers, e.g. `A -> B`; 120s for a `chain`-
                                  # tagged scenario; 300s for a `ui`-tagged one — internal/scenario/sections.go's
                                  # timeoutCeiling). It is NOT optional and there is no inherited
                                  # default: a scenario without one used to get a silent 10s, so it
                                  # failed for a reason its author never chose. Give it the value
                                  # THIS scenario actually needs — how long IT may wait for the SUT
                                  # to answer ONE request:
                                  #   - http/database/message-flow/external-delivery/error-path/
                                  #     rate-limiting/permissions/saga-presence (the JMeter-driven
                                  #     layers): TIMEOUT bounds EACH REQUEST inside JMeter itself
                                  #     (every HTTPSamplerProxy's connect_timeout/response_timeout,
                                  #     every JDBCSampler's queryTimeout — DeriveProps'
                                  #     trigger.timeout_ms/trigger.timeout_s, templates/*.jmx). A
                                  #     timed-out sampler is a `failed` verdict naming the timeout,
                                  #     never `error` (that word stays reserved for a genuinely
                                  #     unreachable SUT — connection refused, unknown host). SEPARATELY,
                                  #     a GENEROUS PROCESS-LEVEL BACKSTOP (TIMEOUT × this template's
                                  #     sampler count, + a `## LOAD` profile's own ramp+duration, +
                                  #     >=60s JVM start-up grace) kills the whole jmeter process if it
                                  #     never returns at all — a hung executor, not a slow SUT; it is
                                  #     sized to never fire on a healthy run, including a full-length
                                  #     `## LOAD` profile (ramp+duration, up to 86400s).
                                  #   - mcp (single-call AND each `chain` mcp step): the call's own
                                  #     deadline is capped at this value (it can only make the
                                  #     target's configured timeout TIGHTER, never looser).
                                  #   - chain: TIMEOUT bounds EACH STEP's own call (same rule as the
                                  #     mcp bullet above) — NOT the whole chain summed. The whole-chain
                                  #     wall clock is instead a GENEROUS backstop (TIMEOUT × step
                                  #     count) that stops firing FURTHER steps once it elapses; a step
                                  #     already in flight when it passes still finishes. (Before this,
                                  #     the flat per-chain TIMEOUT — capped at 120s regardless of step
                                  #     count — bounded the WHOLE chain directly, so several steps each
                                  #     legitimately taking close to their own budget could sum past it
                                  #     and be cut off mid-chain for a reason no single step earned.)
                                  # A per-request/per-step timeout is `failed`, naming the elapsed
                                  # time and the declared budget — the SAME word a chain step's own
                                  # `poll` timeout already uses, never `error` (that stays reserved for
                                  # a dead/refusing SUT, not one that simply answered too slowly).
                                  #   - ui (a `ui`-tagged Playwright scenario): ceiling 300s (a browser
                                  #     flow is many round trips). The value LENGTHENS the vendored
                                  #     limits and never shortens them: the per-test timeout is
                                  #     max(60s, TIMEOUT) and the navigation timeout max(30s, TIMEOUT)
                                  #     (action and expect timeouts, 15s each, stay). A `10s` or `30s`
                                  #     TIMEOUT runs exactly as before. NO process-level kill is added:
                                  #     a hung browser process is still bounded only by Playwright.
                                  #     A `ui` STEP inside a `chain` is unchanged (the chain's own
                                  #     TIMEOUT and backstop bound it).

## CLEANUP
<sql / bash / "N/A — reason">     # ⛔ REQUIRED on EVERY scenario — not only data-writing ones.
                                  # A scenario that needs no cleanup says so: `N/A — <why>`.

## References
- <ticket key / URL / doc id / repo path>       # optional; POINTERS + caveats — not expected results
                                                 # ⚠ It does NOT flow to the failure-context bundle:
                                                 # internal/failcontext/ never mentions references.
                                                 # It is read by author_read_scenario and by a human.
```

**Optional: `## LOAD`** (mode 2, "under load"). It is NOT part of the template above: leave it out
and the scenario runs the default one-thread, one-loop run. Append it only when you want a load run.
It declares a JMeter thread group. Once the section is present, all five fields are REQUIRED. The
runtime authority is `internal/scenario/parse.go`'s `LoadProfile`, and the bounds are in
`internal/scenario/validate.go`. An example with real values:

```markdown
## LOAD
**Users**: 50
**Ramp Seconds**: 30
**Duration Seconds**: 120
**Target P95 Ms**: 800
**Max Error Rate**: 0.01
```

- **Users**: concurrent virtual users, 1–10000.
- **Ramp Seconds**: seconds to bring all users online, 0–86400.
- **Duration Seconds**: seconds to hold the load after ramp-up, 0–86400. 0 means "loop count only,
  no scheduler cutoff".
- **Target P95 Ms**: the p95 latency this load must stay under, in milliseconds, 1–600000.
- **Max Error Rate**: the highest acceptable error fraction, from 0.000001 to 1 (a fraction, not a
  percentage).

A user is a persistent client: every template's HTTP requests ask for keep-alive on a `## LOAD` check,
so each simulated user keeps ONE connection alive per host across all its loops (a load run on the SUT
opens about `Users` connections, not one per request), as a real client would. A step may run
longer than 60 s (`Duration Seconds` goes up to 86400); a request's two timeouts (connect and response)
are each the check's own `## TIMEOUT`, so a request to an unreachable target can take twice that.

The load record in `report.json` (`load`) carries, besides `samples`, the percentiles and `error_rate`,
two bounded fields that never change the verdict: `errors`, the transport failures (requests that got
no HTTP status) grouped by reason and word, e.g. `{"reason": "java.net.SocketException: Network is unreachable",
"kind": "other", "count": 180}` (at most 8 reasons, the rest summed under `other` with one entry per word, each reason at most 160 characters,
scrubbed: every URL in the text is cut to `scheme://host[:port]` (no userinfo, path, query or fragment),
then declared credentials and token-shaped strings are removed; absent when there were none; `kind` is one word
from `refused`, `timeout`, `reset`, `unknown_host`, `tls`, `protocol`, `other`, chosen from the error type;
`author_get_report_limited` carries `kind` and `count` only, never the reason, while `argus get-report` and
`author_get_report` show the whole entry), and `timeline`, buckets of `timeline_bucket_s`
seconds (at most 120) of `{at_s, started, answered, failed}`: `started` counts requests by start time,
`answered` those with an HTTP status and `failed` those without, both by end time (absent below two
samples). The top reason is also named in the `load target breached` sentence.

A run with ANY `## LOAD` section also captures the SUT namespace's environment (P3 #23): pods (owner,
node, image digest, per-container requests/limits), workload replicas (desired/ready) and, best-effort,
node allocatable capacity, into the run's `report.json` under `environment`. It is READ-ONLY and needs
the SUT owner's grant — the Role/RoleBinding they apply in THEIR OWN namespace
(`argus render-k8s --emit-sut-access-role` renders it as `argus-load-environment-read`; see the
tester guide, https://docs.onedroid.ai/argus-tester-guide). With no grant the run still reports
`environment.captured: false` with a named reason, never a silently empty field. Two runs whose
`environment.fingerprint` differ are NOT directly comparable: a pass/fail difference between them may
reflect the environment, not the code under test.

**HTTP Load: a STEPPED HTTP ramp that measures a limit** (layer `HTTP Load`, executor 0.3.66+).
A plain `## LOAD` holds ONE load for a duration and judges it. An `HTTP Load` scenario steps the load UP (for example
10, 20, 40, 80 users, each step held for `Step Duration Seconds`), records each step, and **stops at the first step
that is not comfortable**, so a target that has broken is not pushed harder. The largest comfortable step is the
tested limit the Capacity page shows, with its date and version, exactly as for an `AMQP Load` ramp. The fields are
the AMQP Load layer's where they mean the same thing; the runtime authority is `internal/scenario/httpload.go`.

````markdown
# Scenario: HTTP load ramp on the orders API, 10 to 80 users

## Metadata
- **ID**: HTTPLOAD-001
- **Layer**: HTTP Load
- **Tags**: http, load
- **Target**: api-lab

## TRIGGER
POST `/api/orders`
Content-Type: application/json
```json
{"sku": "A-1", "qty": 1}
```

## EXPECT
### Runnable
- every step is measured
- the smallest step is comfortable

### Non-runnable
- the ramp reports the largest comfortable step as the tested limit

## LOAD
- **Steps**: 10, 20, 40, 80
- **Step Duration Seconds**: 60
- **Ramp Seconds**: 10
- **Settle Seconds**: 20
- **Target P95 Ms**: 500
- **Max Error Rate**: 0.01
- **Must Sustain**: 20

## TIMEOUT
10s

## CLEANUP
N/A — every order the ramp creates is in the lab deployment, which is reset after each load run.
````

| Key | Meaning | Default | Bounds |
|---|---|---|---|
| **Steps** | users per step, one JMeter run each; they must RISE | required | 1 to 12 steps, each 1 to 2000 users, and at most the target's `max_sessions` |
| **Step Duration Seconds** | how long each step is held, ramp included | required | 10 to 3600 |
| **Ramp Seconds** | users start over this; excluded from the step's numbers | 10 (half the step if shorter) | 0 to half the step |
| **Settle Seconds** | pause between steps | 30 | 0 to 600 |
| **Target P95 Ms** | a step whose response p95 is above this is not comfortable | required | 1 to 600000 |
| **Max Error Rate** | a step with more of its requests answered 400 or more, or not at all, is not comfortable | required | 0.000001 to 1 |
| **Must Sustain** | the check fails unless this step was reached and comfortable | none | one of **Steps** |

- `**Target**` is required: a `targets.http_targets` entry that the operator listed under the top-level
  `load_allowed_targets`. Anything else is refused before any request is sent (`onboarding/HOW-TO-ARGUS-CONFIG.md`,
  "HTTP Load"). `Users`, `Duration Seconds` and the AMQP-only keys (`Rate Per Session`, `Message Size`, …) are refused
  by name; so is a `status=` bullet: a request counts as served when it is answered below 400.
- `### Runnable` takes only `every step is measured` and `the smallest step is comfortable`; the ramp's verdict
  enforces both every time. The check PASSES when a limit was measured (the ramp broke above its first step, or never
  broke), FAILS when a step recorded no request, when even the smallest step was not comfortable, or when the
  `Must Sustain` step was not comfortable or never reached, and is `degraded` when the target restarted during a step.
- Each user sends the TRIGGER request back to back on one kept-alive connection (no think time), so `delivered_per_s`
  of a step is what the target served at that concurrency. The run records per step: users, requests started, answered
  and served per second, the response-time quantiles (`response_us`), errors by class (`status:<code>`, `timeout`,
  `transport`) with the transport reasons and their words, and whether it was comfortable; the run's `load_ramp` carries
  it with driver `http` and the step that broke (`stopped_at_step`), each error reason as its word and count only.
- It cannot be written into a certification set, and is not queued for a `final`, `scheduled` or `rehearsal` run.

The whole compare flow (seal, create, run, read, approve) is at https://docs.onedroid.ai/argus-compare.

**Optional: `## COMPARE`** (the ninth section). It says, before any run, what "the same output" means
for this check, so two systems (or two versions of one system) can be compared on the SAME sealed
checks. Leave it out and the check is not part of any comparison. It is not read by a build, final or
scheduled run: those judge the check by its `## EXPECT`, exactly as before. The reader is
`internal/compare` (`BuildRules`); an invalid section is refused by key and line when you write the
scenario, and a run in compare mode refuses the check instead of running with a rule ignored.

```markdown
## COMPARE
- **Reference**: measured
- **Output**: status, body, header:Content-Type
- **Mask**: $.id; $.items[*].createdAt
- **Unordered**: $.items
- **Repeats**: 5
- **Agreement**: 100%
```

| Key | What it means | Default | Bounds |
|---|---|---|---|
| **Reference** | `measured`: compare with another system's recorded output. `fixed`: this check's own `### Runnable` claims are the expected output. `property`: the claims are a property that must hold. | required | one of the three |
| **Output** | Which parts of the response are compared: `status`, `body`, `header:<Name>`. | `status, body` | up to 8 headers; never `Set-Cookie`, `Authorization`, `Proxy-Authorization`, `WWW-Authenticate` or `Cookie` |
| **Mask** | Fields whose VALUE is ignored: `$.id`, `$.items[*].createdAt`, `header:Date`. A mask hides a value, never the presence of a field: a field that exists on one side only is still a difference. | none | up to 32 |
| **Unordered** | Arrays whose element order does not matter. | order matters | up to 8 |
| **Tolerance** | Numbers that may differ a little: `$.total abs 0.01` or `$.rate rel 0.001` (`rel` is relative to the smaller of the two values, so it is the same whichever run is called the reference). The executor records each such number (at most 64 per check) and sends it to the control plane as a number; two runs whose numbers are all within the tolerance compare as the same, one outside it is a difference and the cell names the declared path (the author's read with a Full token also shows the two numbers). With several reference runs, the reference is stable only if every pair of its runs agree, and a candidate run agrees only if it agrees with every reference run. Needs an executor of release 0.3.58 or later. Needs a Full author token: with a Limited token, a check with a Tolerance rule is refused when a comparison is created or a set is sealed, and such a comparison is refused when it is run (instead, make the numbers match exactly, or mask the field). | exact | up to 16 rules |
| **Repeats** | How many runs of each system the check needs. | 1 | 1 to 20 |
| **Agreement** | The share of a system's runs that must agree, for example `100%` or `95%`. `property` must declare it. | 100% | 1% to 100% |
| **Not Worse Than** | A band for load numbers against the reference: `p95 20%`, `error_rate 0.5pp`. Needs `## LOAD`. The run records its load numbers (samples, p50, p95, p99 in ms, the error rate); each system's MEDIAN over its runs (with an even number of runs, the mean of the two middle values; a run in which the load check failed still counts its numbers, a run that errored on the check counts none) must stay within the band of the reference's median (`%`: at most reference x (1 + band); `pp`: at most reference + band points). A performance result has no output to pin, so it cannot be approved. Declare `Reference: property` (or `fixed`) on such a check: it records load numbers, not an output. Needs an executor of release 0.3.58 or later. | none | `p50`, `p95`, `p99` with `%`; `error_rate` with `pp` |
| **Steps** | For a chain: which `http` steps are compared. | every `http` step | names must exist |

- **A member's target.** When a comparison is created, a member may name a `target`: a named connection target of that
  executor's `argus-config.yaml` (`targets.<kind>_targets.<name>`). The run uses it for every check that has no
  `**Target**` of its own (a check's own word always wins). It does not reach a chain (a chain selects its target per step), a
  Web UI check or an External Delivery check. If the executor's environment declares no target of that name the run fails by
  name before any check fires; it never falls back to the default target. Two members may share one instance when their
  targets differ. Needs an executor of release 0.3.58 or later.
- A path is `$`, then up to 8 of `.name`, `[*]` (every element) and `[n]` (one element). No filters and no
  regular expressions. A header is `header:<Name>`; its name is not case sensitive.
- Numbers in `Repeats`, `Agreement`, `Tolerance` and `Not Worse Than` are written plainly (`5`, `95.5%`,
  `0.01`): no sign, exponent, underscore or hex. A `Tolerance` path may not lie at or under an
  `Unordered` path; it is refused by name.
- A body that is not exactly one JSON value (a byte-order mark, bytes after the value, a repeated key
  in an object, bytes that are not valid UTF-8, an unpaired `\ud800`-style escape) is compared as text. A mask, `Unordered` or `Tolerance` on a text body means the output
  is not recorded, and the cell says so: it is never compared with the rule left out.
- `Reference: measured` needs an output that is recorded. Today that is the http engine on HTTP
  Ingestion, Error Path, Rate Limiting and Permissions, and the `http` steps of a chain. Other layers are
  refused by name. `fixed` and `property` need no recording: they use the check's own outcome.
- `fixed` and `property` judge each system alone, so `Output`, `Mask`, `Unordered`, `Tolerance` and
  `Steps` have no meaning there and are refused.
- With `Agreement` below 100% the check warns when `Repeats` is too small to support it. For example, 5
  clean runs cannot support 99%; that needs 299 runs with no disagreement. A result never claims a
  rate that its number of runs cannot support.
- Only responses are recorded, never requests. Personal data cannot be detected in general: keep it
  out with `Mask`, and note that the recorded body stays on the executor that ran the check.

**Claim kinds and seed checks in one comparison.** One sealed set may mix `measured`, `fixed` and
`property` checks. A `measured` check is compared with the reference member, so a set that has one needs
exactly one reference member; a set with none needs no reference (a reference member given anyway is judged
like every other member). A `fixed` or `property` check is judged on every member alone, from the check's
own outcome over that member's runs (it passed in at least `Agreement` percent of them; `fixed` means every
run unless you declare otherwise); a run in which the check errored is left out of the count and reported,
never counted as a failure, and the table never shows what the claim expected. A check whose `**Tags**`
include the exact word `seed` (for example `**Tags**: seed, setup`) puts every system in the same starting
state: the control plane refuses to create the comparison unless every seed check's ID sorts BEFORE every
other check ID of the set in byte order (the executor runs a set in ID order; name them `00-SEED-...`), and a
run whose seed check did not pass establishes nothing: every other cell of that run reads "could not run:
starting state not established" and the run is not counted. A seed check that declares `## COMPARE` is shown
but is not part of the roll-up.

### The 8 layers (exact strings — the runtime authority is `internal/scenario/parse.go`)
`HTTP Ingestion` · `Message Flow` · `Database State` · `External Delivery` · `Error Path` ·
`Rate Limiting` · `Permissions` · `Web UI`. Chain with `->` (e.g. `HTTP Ingestion -> Database State`).
Two load ramp layers stand alone and are never chained: `AMQP Load` (a broker, "Load testing" in https://docs.onedroid.ai/argus-tester-guide)
and `HTTP Load` (an http target, "HTTP Load" above).

⛔ **`## Metadata` is a CLOSED list of four keys** — `ID`, `Layer`, `Tags`, `Target`. Any other key
is REFUSED by name. So is any `## SECTION` that is not one of the nine (`Metadata`, `TRIGGER`,
`VERIFY`, `EXPECT`, `TIMEOUT`, `CLEANUP`, `References`, `LOAD`, `COMPARE` — `LOAD` and `COMPARE` are the
OPTIONAL ones, shown below the template), and any `### ` sub-section under `## EXPECT` that is not `Runnable` or `Non-runnable`.
Nothing is silently ignored any more: if you misspell a key you are told, rather than having it
quietly read as empty.

### Rules

- ⭐ **PROSE HAS TWO HOMES, AND THE TEST IS NOT "IS IT PROSE?" — IT IS *IS THIS AN EXPECTED
  RESULT OF THE RUN?***
  - **YES ⇒ `### Non-runnable`**, beside the runnable expectations it belongs with. "The newly
    created group then appears by name in the `docs_list_groups` result" is an expected result
    that cannot be automated.
  - **NO ⇒ `## References`** — things you can go and LOOK AT: spec docs, Jira tickets, Confluence
    pages, plain-text caveats. "Verified during authoring: `docs_list_groups` returns the same
    collection as `docs_list_libraries`" is a note about how it was authored, not a result.
  ⛔ **References is NOT a neutral prose bin.** The `failure-triage` skill that ships with OneDroid Argus makes FOLLOWING
  References a MANDATORY step before any verdict. Pour unfollowable descriptions into it and that
  mandatory read degrades from *follow these refs* to *sift prose for refs*.
- ⭐ **NEVER WRITE THAT A TOOL OR BEHAVIOUR DOES NOT EXIST WITHOUT THE DATE YOU PROBED IT.**
  Absence is the claim that expires fastest.
  - A positive claim gets contradicted by the next run that touches it. **A negative one is never
    contradicted by anything** — nobody runs a probe for a tool the file says is not there. A file
    that says "X exists" is under continuous test; a file that says "X does not exist" has REMOVED
    ITSELF from testing.
  - It is not hypothetical: one real check's CLEANUP said *"no `docs_delete_access_group` tool was
    discovered during probing"*. The tool EXISTS. Seven access groups accumulated over five days,
    and the file did not merely fail to record the change — **it authorised the accumulation, in
    writing**, and every reviewer since read it as settled.
  - So: carry **the date and what would settle it, in the sentence itself**. A convention, not a
    parsed field:
    ```
    - 2026-09-04: no access-group delete tool found during probing. Recheck when one
      appears in the tool list.
    - ⏳ OPEN — raised 2026-08-25 · closes when: a second, unprivileged token exists for this instance
    ```

- ⭐ **A step that means "this was deleted" must assert the DELETION SUCCEEDED, not that the thing is
  absent.** *"No workspace"* is not the same as *"workspace deleted"* — **an absence assertion also
  passes when the object was never created**, so it is a false green waiting for the first run where
  the setup step failed. Assert on the delete call's own answer; keep any absence check as the second
  half of the claim, never the whole of it. (The runner will not report such a step green after a
  break — see `specs/17-chain-scenarios.md` — but a scenario should not need that safety net.)
- ⭐ **Every `Name: value` line you write in `## TRIGGER` IS SENT.** Put them between the METHOD line
  and the payload fence. An author header REPLACES the runner's for the same name, `${VAR}`, `${cid}`
  and `${correlation_id}` resolve in the value (the HTTP `Authorization` header too), and `author_validate_scenario` lists the header NAMES back to you
  so you can see what will go on the wire.
  - ⛔ **Two names are REFUSED:** `X-Correlation-Id` (the runner's alone — the report, the logs, the
    dashboard and the SUT's own records are stitched together by it) and `Mcp-Session-Id` (minted by
    the MCP handshake). You are told by name, not ignored.
  - ⚠ A header value is the likeliest place in a scenario to hold a credential: write it as a
    `${VAR}`, never a literal. Values are never logged and never reach `report.json`.
  (Until 0.3.31 the parser read every header and the runner sent exactly ONE of them. Two shipped
  scenarios declare `Accept: application/json, text/event-stream` and neither ever sent it.)
- ⭐ **`## CLEANUP` is REQUIRED, and it RUNS.** It must be a ```sql or ```bash block that removes
  what this scenario creates, or `N/A — <why nothing needs cleaning up>`. Prose describing what
  someone ought to delete is **refused** by `author_validate_scenario` AND by
  `author_write_scenario`. Every declared block executes after the scenario, **on every path**
  (passed, failed or errored), and the result is in `report.json`.
  - A ```bash cleanup block runs under POSIX `sh -c` (dash on the executor image), not bash: write
    POSIX sh. Bash-only syntax (`<<<`, `[[`, arrays, `${v//…}`, `pipefail`) fails at once, and
    `author_validate_scenario` warns about it.
  - **It never changes the verdict**, in either direction — write it freely.
  - **Finding nothing to delete is a SUCCESS.** A cleanup that went red for that would turn
    every green run red.
  - Use `${correlation_id}` to scope it to this run: `DELETE FROM <t> WHERE correlation_id =
    '${correlation_id}';`
  - ⛔ **Before you write a `sql` cleanup, check that your suite credential may WRITE.** The
    product's own onboarding tells operators to grant a **read-only** database role (*"the suite
    must never write to your SUT"*), and a cleanup runs on exactly that connection — no privilege
    escalation. On a SUT configured as instructed, a `DELETE` is denied, and because a cleanup never
    changes the verdict and its output is not captured, **the denial is invisible**. Prefer a `bash`
    block, or a delete STEP inside a chain, or say `N/A — <why>` honestly. This tension is a known
    open design question; do not paper over it with a cleanup that cannot run.
  - On a `money_handling` instance a runnable cleanup is refused unless the owner's `money_writes.allow` lists
    `engine: cleanup` with that block's `form` (`sql` or `bash`; a scenario with both needs both). The entry allows
    whatever a block of that form does. `N/A` needs no entry. A scenario the run refused before firing runs no cleanup
    block: each is reported `not-run`. The same instance refuses a `ui` scenario or `ui` chain step unless the owner lists
    `engine: ui` with exactly that `spec` (the generic read-only spec is `tests/live/argus-declared.spec.ts`), and a
    `fault` chain step unless the owner lists `engine: fault`; the refusal sentence names the entry to add.
  - ⚠ Its output is **not captured** — the outcome and the duration are reported, nothing else,
    because a command line can carry a resolved secret and `report.json` is pasted into chat.
  (Until 0.3.31 this section was parsed and read by NOTHING, while four documents said it was
  enforced. 32 example scenarios were creating data on a real system that nothing deleted.)
- **A content-layer VERIFY must be EXECUTABLE.** A `Database State` scenario VERIFY must contain a
  ```sql``` block (a real query, scoped by `correlation_id = '${correlation_id}'`). A prose-only VERIFY
  is **rejected** by `author_validate_scenario` — it would silently verify nothing (a false green). (RO-09)
- ⛔ **A count is a VALUE, not a number of rows (VF14).** `row_count == N` and `no rows` count the ROWS
  the query returns. `SELECT count(*) …` (any ungrouped aggregate) always returns exactly ONE row, so
  `row_count == 1` could never fail and `no rows` could never pass — both are refused. Alias the
  aggregate and assert its value: `SELECT count(*) AS n FROM … WHERE correlation_id = '${correlation_id}'`
  with `- n == 1` (or `- n == 0`). ⚠ Do not alias it `row_count` — that name is a grammar keyword, never
  read as a column. (AC-D31)
- ⭐ **`${correlation_id}` scoping (VF5) has ONE narrow, explicit opt-out: tag `standing-state`.**
  VF5 exists because an unscoped query can go green on ANOTHER RUN's rows. Some checks measure a
  STANDING invariant of the SUT instead — a computed aggregate, not a row this run created — and
  another run's rows cannot make an aggregate falsely green: *"no order stuck in a non-final
  state for more than an hour"* (`SELECT count(*)::text AS stuck FROM orders WHERE …`), *"the
  feed wrote in the last 90 minutes"* (`SELECT (max(fetched_at) > now() - interval '90
  minutes')::text AS fresh FROM feed_readings`). Add `standing-state` to `- **Tags**:` and the query
  no longer needs `${correlation_id}` at all.
  - ⛔ **The tag is not a free pass (VF5b).** It is refused unless `## EXPECT` commits to an actual
    asserted COLUMN VALUE: `no rows` / any exact `row_count == N` is refused (that is still judging
    by row existence, which is exactly what `standing-state` opts out of scoping against), and an
    EXPECT with no column assertion at all is refused too — with nothing else to judge it by, the
    verdict would fall back to "a row exists", the same false-green shape VF5 exists to close. Write
    `stuck == 0` / `fresh == true`, not `row_count == 1`.
  - ⚠ If the query still contains `${correlation_id}` alongside the tag, `author_validate_scenario`
    WARNS (not refuses) that the tag is probably unnecessary — the query is already run-scoped.
  - Every other VERIFY rule (VF4 read-only, VF6 columns, VF7 no tautology, VF8 one statement, VF9
    structural grammar, VF11 no other placeholder) still applies unchanged; only VF5 is skipped.
  - **Do not paste an unused `${correlation_id}` into a query to dodge VF5.** That silently defeats
    the guard it exists to enforce — that is exactly the hack `standing-state` + VF5b replaces with
    something the validator can see and check.
- Use `${VAR}` for anything environment-specific (URLs, tokens, ids). It is filled in from the environment of the
  process that runs the scenario — in MCP and chain request fields (`args`, the TRIGGER payload, `server_url`, a ui
  step's `spec` / `app_url`) and in request headers. At the start of an HTTP TRIGGER (`${INGESTION_URL}/…`) it stands
  for the base URL, which comes from `argus-config.yaml`. It is NOT filled in later in an HTTP path (the path takes the
  correlation id only); an HTTP body and header values DO fill it in; and it is NEVER filled in a check (see
  *Placeholders — which one is filled in where* at the end of this skill).
  In a **chain `http` step**, `${INGESTION_URL}` at the start of the `url` also stands for the base URL: the host and
  port of `targets.http.base_url` (its path is not added, exactly as for a plain HTTP check). A chain cannot select a
  named `http_targets` entry, so for another host write the full url. Any other `${NAME}` in a chain step is an
  environment variable, and one that is not set is named in the failure (in a url only; in a step body or header it
  is sent as the literal text). To have onboarding DELIVER a secret a check uses (say a password in a step body) to
  the executor, declare its NAME under `check_env:` in `argus-config.yaml`. `check_env` decides delivery, not use:
  any name present in the executor's environment is substituted.
- ⛔ **Idempotency / multi-status scenarios: declare `status2=`.** Write `- status=202` and
  `- status2=409` under `### Runnable`; the SECOND declared status is what makes the runner fire the
  request twice with a shared Idempotency-Key. **A status is never scraped from prose** — writing
  "First: status=202" / "Second: status=409" no longer declares anything, and three prose variants of
  one request used to make the runner send it TWICE against the SUT on every run.
  The second request is sent on two layers only: **HTTP Ingestion** (the run switches to
  `http-idempotency.jmx`) and **Database State** (its template re-sends the trigger as `<id>-trigger2`
  under the same key, and the row check runs after both — for example `status=202`,
  `status2=409`, `n == 1`). On any other layer `status2=` is **refused when you write the scenario**
  (AC-D30): its template sends one request, so the second response could never arrive and the
  scenario would be red against every system.
- A scenario that asserts a **control action** must include the mandated saga assertion (tag `saga-presence`).
- **MCP scenarios (tag `mcp`) assert on the ANSWER, not only on "it did not error".** The runner calls
  the tool itself and judges the two MCP error planes, then — when you ask it to — the CONTENT of the answer:
  `result.content[*].text`, or the JSON-RPC error object when a protocol error is expected. EXPECT bullets it understands (specs/17-chain-scenarios.md is the authority):
  - `result.isError == false` (success) · `result.isError == true` (the tool plane) ·
    `jsonrpc error == -32601` (the protocol plane; the code is optional)
  - `body has <field>` · `body has <field> containing <value>` · `body has <field> matching <regex>` — `<field>` is a
    field of the JSON answer (a dotted path such as `data.id` works): the bare form checks that the field is present
    and not null; `containing` / `matching` check its VALUE (a number, list or object as its JSON text). When the
    answer is not JSON, or has no such field, the check
    FAILS. Write `body has id containing <the id>` / `body has error_code containing PROVIDER_REJECTED_CONTENT`.
  - `body contains <value>` · `body matching <regex>` — checked against the WHOLE answer text, JSON or not.
    `body has <field> …` needs the answer to be JSON with that field; to match text anywhere in the answer, write
    `body contains …`.
  - A step that expects an error (`result.isError == true`, or a protocol error) has its `body …` checks judged too,
    once the error plane matches: on the tool plane against `result.content[*].text`, on the protocol plane against the
    JSON-RPC error object (`code`, `message`, `data`). A content miss is `failed` on a success step and on an error step
    alike (the SUT answered; the answer was wrong). On the protocol plane `body contains …` reads the error object as
    JSON text, so a quote or backslash in it appears escaped, as JSON writes it; check the error's stable fields instead
    (`body has code containing …`, `body has data.<field> …`).
- ⭐ **A `ui` scenario: three forms, and they are the whole grammar (V29-021).** Until 0.3.32 a Web UI
  scenario's `## EXPECT` was read by NOTHING — the real assertions lived inside the Playwright spec
  file and the verdict was that process's exit code, so an author could write three careful bullets,
  the product would compare none of them, and the run went green on whatever the spec happened to
  check. `## EXPECT` looked like the contract and was not. These forms are now EXECUTED, one outcome
  per bullet:
  - `dom has <selector>` — the selector matches at least one element. `dom has <selector> containing
    <text>` and `dom has <selector> matching <regex>` check the matched element's text.
  - `no backend 4xx/5xx` — no request the page made came back 4xx or 5xx.
  - `no console errors` — the browser console logged no error.
  
  ⚠ **`<selector>` is a Playwright LOCATOR, and we invent nothing** (the owner's U-3): `.order-banner`,
  `text=Order created`, `[data-test=total]`. It is the language the spec files already speak, so there
  is no second syntax to learn. **Quote a selector that contains spaces** — `dom has "text=Order
  created" containing 42` — or the value swallows half the locator.
  
  ⛔ **A bullet that OPENS with one of these three words and is then malformed is an ERROR, not prose**
  (`- dom has`, `- no backend`). ⚠ But a MISSPELLED opener (`- no backedn 4xx/5xx`) reads as prose and
  always will — no rule over a single bullet can tell a typo from a sentence. What catches it is the
  rule above: a scenario declaring NO runnable check is refused when you write it and reported
  `error` when it runs. Read your own bullets back.
  - ⛔ **A bullet that looks like an assertion but is none of these forms is REFUSED** by
    `author_validate_scenario`, by `validate-config`, and at run time (preflight `failed`, bullet
    quoted). It is never silently dropped — before 0.3.29 it was, and 19 of 21 green steps in one run
    proved nothing about their claim. Plain prose bullets (no `==`/`must`/`contains`/`matching` next to
    a field name) stay documentation.
- **Assert on stable values, never prose.** OneDroid Argus reads the SUT's own reply, which is
  machine-generated — but its wording is not equally stable everywhere:

  | | assert on | why |
  |---|---|---|
  | ✅ | ids, markers the scenario itself wrote (`marker-${cid}`), field names, error codes, enum values | machine-generated, stable across releases |
  | ⛔ | prose sentences (`"document not found"`) | phrasing changes on upgrade — a green that rots into a red for no product reason |
  | ⚠ | a value with more than one legitimate rendering — a commit sha (full 40 hex or abbreviated 7-8), a timestamp, a number with a unit | the value is stable; its SPELLING is not. `containing` is a substring match, so `14a2149b…` (full) does not match `14a2149b` alone if the answer abbreviates, and vice versa |

  So `body has error_code containing CONTENT_VALIDATION_FAILED`, never
  `body contains "since_days must be an integer"`.

  **This holds for error tests too.** Check an error's stable fields — its code, a rule name, a field name — never its
  message. When the error carries nothing stable (a plain sentence), check only the error plane — `result.isError ==
  true` on MCP, the declared `status=` on HTTP — and record the message under `### Non-runnable`.

  > **Measure the answer before you assert on it.** Run the step once and read `result.content[*].text`, or call the tool
  > directly. Assert the form the ANSWER carries — never the form some other tool (your own runner's evidence file, a UI, a
  > log line) prints for the same value. This is not "avoid shas": a captured sha is a strong assertion when the answer
  > carries it in full — measured 2026-09-08 on a document store whose `docs_history` returns full 40-char shas, so
  > `body contains ${saved.sha2}` is correct there. It is "know which spelling you are asserting".
- **A scenario must declare at least one check under `### Runnable`**: a scenario that asserts nothing proves
  nothing, `author_write_scenario` refuses it, and the run reports it as `error`. ⚠ **A Web UI scenario is NOT
  exempt** — it satisfies the rule one of two ways: by declaring bullets in the `ui` grammar above, or by naming
  its own Playwright spec file in the TRIGGER, where its assertions live. Naming neither is refused like any
  other scenario. (Until 0.3.32 a `ui` scenario WAS exempt and its verdict was the Playwright exit code; that is
  the behaviour V29-021 replaced, because it meant carefully written bullets were compared against nothing.)
- **Chain scenarios** (tag `chain`, multi-step across MCP/UI/HTTP/AMQP/fault/db/logs): the TRIGGER body is a JSON
  `{"steps":[{"type":"mcp"|"ui"|"http"|"amqp"|"fault"|"db"|"logs", "name":…, "tool":…, "args":…}, …]}` — a chain with no
  `steps[]` fails preflight, and a `type` outside those seven is refused when you write the scenario. (C11)
  Contract: `specs/17-chain-scenarios.md`.
- ⭐ **`db` chain step — "POST, then see the row". Needs executor 0.3.70.** One READ-ONLY
  `SELECT` against a PostgreSQL target of `argus-config.yaml` (`targets.database`, or `"target": "<name>"` under
  `database_targets`), its rows judged by the ordinary `body …` claims on ONE document:
  `{"row_count": N, "rows": [{"<column>": <value>}, …]}`.
  ```
  {"type":"db","name":"row","query":"SELECT status FROM orders WHERE id = $1","params":["${saved.oid}"],
   "poll":{"timeout":"30s","interval":"2s"}}
  ...
  - step row: body has row_count == 1
  - step row: body has rows.0.status equals PAID
  ```
  - ⛔ **A saved value goes in `params`, NEVER in the query text.** `${saved.oid}` inside `query` is refused by name.
    `params` is a JSON array of strings sent as bind parameters `$1…$n` (the server casts), and the query must use
    exactly `$1…$n`. Only `${correlation_id}`/`${cid}`/`${cid8}` may stand in the text.
  - One statement that begins with `SELECT` or `WITH`, no write keyword, a `FROM` (the `## VERIFY` rules). It runs in
    a read-only transaction; **the real guarantee is that the target's role is read-only** — ask the operator for one.
  - It reads at most `max_rows` rows (default 100, max 1000); one more is an `error`, not a pass on part of the
    answer: add a `LIMIT` or select `count(*)`. `timeout` default `5s`, max `30s`. A `poll.interval` under `1s` is refused.
  - Needs at least one `body …` claim (minimum `body has row_count >= 1`); `status=`, `elapsed`, `unreachable`
    and `broker …` are refused on it. `==` and `body contains` keep every digit of a big integer; `equals` reads a number as a float.
  - A failed query (unknown table, permission denied, statement timeout) is `failed`, never "no rows"; the builder sees only `database query did not run (SQLSTATE <code>)` and you read the database's own message in the step's `body_excerpt`. The step also records the code alone in `observed_sqlstate`, which `author_get_report_limited` sends (an `http` step records its status the same way, in `observed_status`). Cannot connect is `error`.
  - PostgreSQL only; on a `money_handling` instance it is refused unless the owner's `money_writes.allow` lists `engine: db`. A `**Steps**` entry in `## COMPARE` cannot name it.
- ⭐ **`mcp` step, answers a gateway wraps. Needs executor 0.3.70. A claim on the extracted rows is written
  `body extracted …`, and an executor older than 0.3.70 REFUSES a check that has one (by name, before sending anything) instead of running it.**
  A database gateway answers with a sentence, the rows between `<untrusted-data-<uuid>>` tags (the uuid changes each call), a sentence;
  the real hub puts all of it in ONE string, the `result` field of a JSON object.
  ```
  {"type":"mcp","name":"rows","tool":"execute_sql","args":{"query":"SELECT id, status FROM orders WHERE note = '${correlation_id}'"},
   "extract":{"path":"result","regex":"(?s)<untrusted-data-[0-9a-f-]+>\\s*(\\[.*\\])\\s*</untrusted-data-"},
   "unavailable":{"matching":"database_unavailable|internal server error"},
   "poll":{"timeout":"2m","interval":"5s"},"idempotent":true,"save":{"firstId":{"json":"0.id","from":"extracted"}}}
  ...
  - step rows: body extracted has 0.status equals PAID
  ```
  - `extract`: ONE capture group (write `(?s)` when the data spans lines). Without `path` it runs on the answer's text; with `"path":"result"` the answer's text is parsed as JSON, the value at `result` must be a string, and the regex runs on its DECODED value (use it when the rows sit inside a JSON string: in the raw text their quotes are escaped and a regex over it captures non-JSON). A wrong shape, or a capture that is empty or only whitespace → `failed` (the start of the answer is in `body_excerpt`). When the answer can hold more than one block, a greedy `(.*)` spans all of them and gives a mixed document on which `contains` claims can both hold; use a bracket-anchored or lazy `(.*?)` form. Plain-text answer: leave `path` out.
  - Claims about the rows are `body extracted <any body form>`: `body extracted has 0.status equals PAID`, `body extracted every $ has …`, `body extracted contains …`. A plain `body …` on the same step reads the answer's own text. A `save` entry reads the rows only with `"from":"extracted"` (`{"json":"0.id","from":"extracted"}` or `{"regex":"(…)","from":"extracted"}`); without `from` it reads the answer.
  - `extract` and its readers go together: a `body extracted …` claim or a `from` save on a step without `extract` is refused, and so is an `extract` nothing reads.
  - `unavailable`: an answer (content or JSON-RPC error text) matching your regex is `error` = "could not check", never passed, never failed. There is no default pattern; write the one your gateway's outage uses, matching only its outage markers (e.g. `internal server error`, `RPC request timed out`), not a generic `"error"` (that turns a real SQL mistake such as a missing table into "could not check" and also matches a row whose value is the word error). An older executor than 0.3.70 ignores `unavailable` and judges the claims on the outage text, so a step whose body claims are all negative can pass there; a step with a `body extracted …` claim or a `from: extracted` save is refused by an older executor before any step fires, so pair `unavailable` with one of those, or with a positive data claim.
  - `poll` re-sends the TOOL CALL while the answer is unavailable or the claims miss, so it needs `"idempotent": true` ("calling this tool again is harmless"); same bounds as a db step (a claim, timeout ≤10m, interval ≥1s).
  - An mcp step carrying a field outside `type, name, transport, target, server_url, tool, args, save, always, extract, unavailable, poll, idempotent, sign` is refused by name.
- ⭐ **`mcp` step, signed per call. The system under test knows the caller only by an Ed25519 signature over each call.**
  An older executor refuses a step with `sign` by name before sending anything (an unknown mcp field), so it never sends it unsigned.
  ```
  "sign": {
    "key": "MEMBER_A_KEY",                               a NAME declared under check_env - never the key
    "key_id": "k-123", "key_id_header": "x-key-id",           both or neither; key_id may be ${saved.x}
    "fields": [{"literal":"v4"}, {"path":"messageId"}, {"path":"prompt","sha256":"hex"}], "separator": "|",
    "arg": "/signatures/agent.inbox",                    where the signature goes in the step's args, and/or
    "header": "X-Signature"                              the header that carries it
  }
  ```
  - **Key.** `key` names an environment variable you declare under `check_env` in `argus-config.yaml`; onboarding delivers it to the executor like any
    check secret (`argus secrets set --key MEMBER_A_KEY …`). Its value is ONE of: a PKCS#8 PEM (a `BEGIN PRIVATE KEY` block), or base64 (standard or URL alphabet, padded or not) of
    a raw 32-byte seed, of the 64-byte Ed25519 key (seed then public key) or of a PKCS#8 DER. Another algorithm is refused. Two members = two names; each step picks its own.
    The key is read when the run starts, held in memory only, and is in no check, report, log or error sentence (the value of a `check_env` name is also scrubbed from the texts the executor keeps).
  - **What is signed: either** `fields` (a template) **or** `value` (never both).
    `fields`: each element is `{"literal": "text"}` (may hold `${saved.x}`) or `{"path": "<where>"}`, read in the step's own request arguments after `${saved.x}` is bound (a string as it is, a number or boolean as its text; an object, an array, null or a missing path is a harness error).
    Add `"sha256": "hex"` (lower-case hexadecimal) or `"sha256": "base64"` (standard, padded) to hash that field's text first. The elements are joined with `separator` (required with two or more; `""` joins with nothing).
    `value`: `"value": "${saved.canonical}"` signs, byte for byte, what an earlier step saved (no JSON escaping), for a canonical string the system itself returns.
  - **Paths** (`path`, `arg`) are the dot path `save` uses (`a.b.0.c`), or a JSON pointer when they start with `/` (`/signatures/agent.inbox`; `~1` is `/`, `~0` is `~`), which you need when a key holds a dot, as an inbox name does.
  - **Where it goes.** The signature is base64 (standard encoding). `arg` puts it in the request arguments (objects on the way are created); `header` puts it in a header; either or both. `key_id` + `key_id_header` put the key's id in a header the step names.
    The headers ride on every request of that call. `X-Correlation-Id`, `Mcp-Session-Id`, `Content-Type`, `Content-Length`, `Host` and `Accept` are refused as targets.
  - **Grey, not red.** A key name not declared, an empty or unreadable value, a key of another algorithm, or a `${saved.x}` no earlier step saved makes the scenario `error` ("could not check"), with a sentence naming the reference and never the value; the signed call is not sent. A signature the system rejects is the system's answer, so `failed` as any other answer.
  - **Validation.** `author_validate_scenario` and `validate-config --scenarios` refuse a malformed block (shape), and with the config (`validate-scenario --config`, `validate-config`) a key name not under `check_env`.
  - **Complete example, the two-call flow** (call 1 asks the system for the string to sign, call 2 repeats the fields with the signature):
    ```
    {"steps":[
     {"type":"mcp","name":"dry","tool":"send_message","args":{"messageId":"m-${cid}","toInbox":"agent.b","prompt":"hello","dryRun":true,"keyId":"key-a"},
      "save":{"canonical":"canonical"}},
     {"type":"mcp","name":"send","tool":"send_message","args":{"messageId":"m-${cid}","toInbox":"agent.b","prompt":"hello","signatures":{}},
      "sign":{"key":"MEMBER_A_KEY","key_id":"key-a","key_id_header":"x-key-id","value":"${saved.canonical}","arg":"/signatures/agent.b"}},
     {"type":"mcp","name":"send-as-b","tool":"send_message","args":{"messageId":"n-${cid}","toInbox":"agent.a","prompt":"hi"},
      "sign":{"key":"MEMBER_B_KEY","key_id":"key-b","key_id_header":"x-key-id","separator":"|",
              "fields":[{"literal":"v4"},{"path":"messageId"},{"path":"toInbox"},{"path":"prompt","sha256":"hex"}],"arg":"/signatures/agent.a"}}]}
    ...
    - step dry: body contains canonical
    - step send: body contains accepted
    - step send-as-b: body contains accepted
    ```
    with `check_env: [MEMBER_A_KEY, MEMBER_B_KEY]` in `argus-config.yaml`. (The `dry` step's `save` path and the claims depend on how your system words its answers.)
- ⭐ **`logs` chain step — "after this request the service logged X" / "nothing logged an ERROR".
  Needs executor 0.3.70.** COUNTS the lines of the Loki the executor already reads (you cannot name a URL), judged by
  the ordinary `body …` claims on ONE document: `{"count": N, "lines": ["…", …]}` (the first 20 lines, time order).
  ```
  {"type":"logs","name":"logged","scope":"correlation","contains":"order ${saved.oid} accepted",
   "poll":{"timeout":"30s","interval":"2s"}}
  {"type":"logs","name":"quiet","scope":"correlation","matching":"level=(error|fatal)"}
  ...
  - step logged: body has count >= 1
  - step quiet: body has count == 0
  ```
  - `scope` is required: `correlation` = lines carrying THIS run's correlation id (every stream but the observability
    plane's own); `source` = the streams `labels` select, e.g. `"labels": {"app": "orders"}` (equality only, at least
    one, and then at least one of `contains` / `matching`: a step may not count a whole stream).
  - `contains` is a literal (`${cid}` and `${saved.<var>}` are filled in, quoted); `matching` is an RE2 pattern. Every
    value is a quoted literal in the query, never spliced raw. `window` (default: from a minute before the chain
    started, max `1h`) says how far back to look.
  - ⛔ **`settle` (default `2s`, max `60s`) is what stands between "the line does not exist" and "the line has not
    arrived yet"**: a log line is shipped after the request that caused it returns. A `count == 0` claim is only as
    good as the wait before it; if the line can take longer, add a `poll`.
  - A read that returns 2000 lines is an `error` (a count that might be a lower bound is never reported as a number):
    narrow `contains` / `matching` / `window`. Loki unreachable, an HTTP error or an unreadable answer is an `error`,
    never a pass of `count == 0`.
  - Needs at least one `body …` claim (minimum `body has count >= 1`); `status=`, `elapsed`, `unreachable` and
    `broker …` are refused on it. The lines are the system's own words: they are redacted and reach an author only.
  - Reads Loki only: refused when the executor has none (`--obs none`) or the config selects BetterStack. The system's
    own pod logs are in Loki only if the operator collects them (`--collect-sut-logs`; `--sut-logs-via api` when the cluster refuses promtail's host mounts, and then the SUT owner must apply the second Role; in `--obs adopt`, `none` or BetterStack no collector is rendered at all; see "Collecting pod logs through the Kubernetes API" in https://docs.onedroid.ai/argus-tester-guide).
    On a `money_handling` instance it is refused unless the owner's `money_writes.allow` lists `engine: logs`. A `**Steps**` entry in `## COMPARE` cannot name it.
- ⭐ **`bus` chain step — "an order-created event with this correlation id appears on the bus" /
  "no error-level message for this run". An AMQP subscription as evidence, next to `logs` (Loki stays).
  Needs an executor that knows the step (phase 1 ships it; phase 2 adds the control-plane version floor that
  refuses a set containing a `bus` step for an older executor — until then an older executor refuses the step at
  its preflight with "unknown chain step type").** The executor binds a private queue before the chain's FIRST step
  fires and counts what arrives; the step is judged where it stands, by the ordinary `body …` claims on ONE document:
  `{"count":N,"correlated":N,"seen":N,"uncorrelated":N,"undecodable":N,"messages":[{"exchange","routing_key","properties","headers","matched_by","body"}]}`
  (the first 20 messages that pass `where`, arrival order).
  ```
  {"type":"http","name":"create","method":"POST","url":"${INGESTION_URL}/api/v1/orders","body":{...}}
  {"type":"bus","name":"order-created","target":"sut-bus","exchange":"orders.incoming","routing_key":"orders.created",
   "where":{"status":"pending"},"wait":"10s","save":{"oid":"messages.0.body.order_id"}}
  {"type":"bus","name":"no-errors","target":"sut-bus","stream":"logs","where":{"level":["error","ERROR","fatal","FATAL"]}}
  ...
  - step order-created: body has count >= 1
  - step order-created: body has messages.0.body.currency equals EUR
  - step no-errors: body has count == 0
  - step no-errors: body has correlated >= 1
  ```
  - Binding: `stream` (a name the operator declared under the broker target's `streams`, for platform streams such as a log
    exchange you should not hard-code) **or** `exchange` + `routing_key` (literals: no `${…}`, not the default exchange).
    ⛔ **A bare `#` routing key is refused at write time** — name a narrow binding (the service or event). `target` names
    `targets.message_broker_targets.<name>`; none = `targets.message_broker`. `wait` is `1s`–`60s` (default `10s`), measured
    from when the step starts. `poll` and `always` are refused (the step waits by itself and creates nothing to clean up).
    A bus step may not be the chain's FIRST step (it is judged where it stands; put the step that causes the traffic first).
  - ⛔ **THE ANCHOR RULE (C2).** A claim that holds when NOTHING was heard (`count == 0`, `count < 1`, `undecodable == 0`, …)
    must be paired on the same step with `body has correlated >= 1` (or `> n`, or `== n`, n ≥ 1). `correlated` counts the
    messages that carry THIS run's id under the sources the operator declared, so a wrong routing key, or an app that does
    not publish over the bus, gives `correlated == 0` and the step FAILS instead of passing on silence.
  - **Universal and negative claims over the message list are refused (C1)**: `body every messages …`,
    `body has messages.… not …`, `body not contains …`, `body has no messages.…` would be judged on the first 20 messages
    only. Narrow the question with `where` and claim `count == 0` (+ the anchor).
  - `where`: body path → string or array of strings (any-of, exact), judged on the message record that carries the run's id.
    Values may hold `${cid}` and `${saved.<var>}` (nothing else).
  - A claim set that is all monotone (`count >= n`, `correlated >= n`, a positive claim on `messages.<i>.…`) passes as soon
    as it holds; anything else is judged once, when `wait` ends.
  - **Fail-closed.** A subscription lost at any moment before the judgement (broker cancel, closed channel or connection) is
    `error` and the claims are NOT judged; a binding that delivered more than 20 000 messages, or more than 2 000 for this
    run, is `error` ("binding too wide - narrow `routing_key`"); a broker that refuses the subscription (403 grant, 404
    exchange) is `error` and nothing is fired. Correlation matches a value EQUAL to the run's id or starting with `<id>.` -
    never a substring. A chain with a bus step is NEVER re-fired after a rate limit (a re-fire reuses the correlation id).
  - The operator declares, per broker target in `argus-config.yaml`, `envelope.correlation` (where the id travels: an AMQP
    `property`, a `header`, or a `body` path), `list_payload`, `streams` and an optional `vhost`; **a target without
    `envelope.correlation` makes the step an `error` - there is no default**. The messages are the system's own words:
    redacted, and they reach an author only (`failed_claims`, `save`); Observed carries counts only.
  - Against a `money_handling` instance the step is judged as an `amqp` `listen` on the same exchange and routing key (a
    `stream`-bound step names neither in the file, so it needs the `exchange` + `routing_key` spelling to be listed).
- ⭐ **A CHAIN'S CLAIMS LIVE IN `## EXPECT`, ONE BULLET PER ASSERTION, EACH NAMING ITS STEP.**
  ```
  ## EXPECT
  ### Runnable
  - step write-doc: result.isError == false
  - step read-back: body has id containing ${saved.docId}
  ```
  - `<name>` is a step's `name` from the TRIGGER JSON, matched **exactly and case-sensitively**.
    A bullet naming no step is REFUSED — renaming a step must not silently orphan its claim.
  - `<assertion>` is any form in the grammar above. **Several bullets on one step are ANDed.**
  - **Every `mcp` step needs at least one bullet**, or its answer is never read. The minimum is
    `- step <name>: result.isError == false`. **A `ui` step is the one exception**: it needs no
    bullet, because its assertions live in the Playwright spec file it names and its verdict is
    that spec's result. ⚠ That is the SPEC-FILE shape. A `ui` scenario can also be **declared** —
    no spec file, its claims written as `### Runnable` bullets in the grammar below — and then the
    bullets ARE the verdict. Pick one shape per scenario; the product tells the two apart by
    whether the TRIGGER names a spec.
  - ⛔ **Never join bullets with `;`** (`- step s: result.isError == false; body has …`): refused,
    and pointed at one-bullet-per-assertion, because nothing ever splits on `;` (a `;` inside a
    value or a regex would break it).
  - Prose about the test goes under `### Non-runnable`; a caveat about the REQUIREMENT goes in
    `## References`.
- ⛔ **The step `expect` key is REMOVED (V31-002).** Until 0.3.31 a chain's claims lived inside each
  step while `## EXPECT` was documentation — two places looked like they held the claims and only one
  did (measured: 127 EXPECT bullets across the shipped chains, 38 of them correctly-formed assertions
  that nothing executed). A step that carries `expect` is now REFUSED when you write it, and the
  runner does not read the key at all: old-format scenarios are not supported and must be rewritten.
  Every claim goes in `## EXPECT` → `### Runnable` as `- step <name>: <assertion>`.
- **Chain step CAPTURE — use a value the SUT generated, in args AND in assertions.** A step declares
  `"save": {"<var>": "<path>"}`; any LATER step references it as `${saved.<var>}` in its args and in its
  `body has …` bullets. This is how you write a scenario against a SUT that mints ids server-side, and
  how a read-back proves it read THAT document: (C13)
  ```json
  {"steps":[
    {"type":"mcp","name":"write-doc","tool":"docs_write","args":{"collection":"c-${cid}","title":"t","content":"marker-${cid}"},
     "save":{"docId":"id"}},
    {"type":"mcp","name":"read-back","tool":"docs_read","args":{"document_id":"${saved.docId}"}}
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
  - step read-back: body has content containing marker-${cid}
  ```
  - `<path>` is dot-separated into the response: `id`, `doc.id`, `items.0.id`. It is read through
    `result.content[0].text` (parsed as JSON — the usual MCP shape) and, failing that, against `result`
    itself, so you do not need to know which shape your SUT uses.
  - `${cid}` and `${correlation_id}` (this scenario's correlation id — two names for one value) are filled in in the
    request fields (`args`, `server_url`, a ui step's `spec` / `app_url`) and in every check; an environment `${VAR}`
    is filled in in the request fields only, never in a check. `${saved.<var>}` binds at run time, per step, into a
    fresh copy of the expectation — a re-run never sees the previous run's id. ⛔ **Moving a claim into `## EXPECT`
    does not change WHEN it binds.**
  - ⛔ **"in every check" means every step type, not just `mcp`.** A chain's per-step claim
    (`- step <name>: <assertion>`) resolves `${cid}` / `${cid8}` / `${correlation_id}` identically
    whether `<name>` is an `mcp`, `http` or `amqp` step — e.g. `- step consume: body contains
    "argus-lab NLB-001 ${cid8}"` on an `amqp` `consume` step is checked against THIS run's own
    derived token, exactly like an `mcp` step's `body has … containing marker-${cid}` above. An
    environment `${VAR}` in a claim is refused BY NAME — at authoring time (`author_validate_scenario`)
    and again if a file reaches a run without going through it — never substituted, because a claim's
    text is copied into the report.
  - **Prefer this over a pinned fixture id.** A pinned fixture is un-versioned state that nothing in the
    suite creates: when one was wiped, SEVEN scenarios went red at once and none of them was a real defect.
    Capture also lets a later STEP delete what the chain created — and since 0.3.31 that step actually
    runs even when an earlier step failed (V30-001), which is when the leak used to happen. ⚠ A
    `${saved.…}` value is NOT available to `## CLEANUP`, which runs outside the chain: scope a
    cleanup block on `${correlation_id}`, or clean up in a step.
  - Referencing a `${saved.x}` no earlier step saved FAILS that step with a message naming `x` — it is
    never sent to the SUT as a literal and never compared against the answer as a literal, because a
    literal placeholder comes back as a confusing SUT-side complaint and gets misread as a product bug.
    A `save` path that does not resolve fails the SAVING step, where the mistake actually is. A step
    whose assertion failed saves nothing.
  - The report marks, per step, which content assertions were ENFORCED (`assertions_enforced_count`;
    the text is test-hat only). A green step with a
    count of 0 proved only its error plane — that the call did not error or, on a step that expects an error, that
    the error came back.
  - For a plain http scenario (no chain), `assertions_enforced_count` counts the CONTENT checks only
    (body, columns, row count). The `status=` line IS checked but is never in that count, so `0` means
    "only the status was checked", not "nothing was". The code the SUT actually answered is on the
    scenario row as `observed_status` (and each distinct code once in `observed_status_codes` when the
    answers differed), on a pass and on a fail. When a `body …` check fails, the author's report
    names it in `failure.failed_body_check` (`index` among the body checks, 1-based, and the `bullet`
    as written); test hat only, like `failure.expected`.
- ⭐ **A chain `http` step — a REST lifecycle no MCP tool fronts** (AC-D20). Fields: `method`, `url`,
  `headers` (layered on top of the scenario's own TRIGGER headers, which default for every step —
  a step's own header wins on a name clash), `body` (raw JSON), `save` (the same capture store an
  `mcp` step publishes into, read from the plain JSON response body), `poll` (optional
  `{"timeout":"180s","interval":"5s"}` — re-sends the request until the step's claims pass or the
  timeout elapses; a poll's failure names the last observed status), `always` (optional bool — a
  CLEANUP step that fires even when an earlier step's `${saved.<var>}` was never captured, and even
  after the chain stopped because the SUT stopped answering; an `mcp` step takes it too, so set it on
  every cleanup delete). **`body` is sent as its JSON text** under `Content-Type: application/json`
  (the default when a body is present and no header sets one; a header you set overrides it); `null`
  or no `body` sends no body. **A text body (executor 0.3.70 or later):** a JSON STRING `body` on a
  step whose OWN `headers` declare a Content-Type that is NOT JSON (not `application/json`, not
  `text/json`, not `…+json`; case and parameters ignored) is sent as the string's TEXT, unquoted:
  `"headers":{"Content-Type":"text/plain"},"body":"line one\nline two"`. Without that header a string
  stays the quoted JSON string; a Content-Type set only in the TRIGGER headers does not count. In a
  text body `${cid}`, env `${VAR}` and `${saved.<var>}` are spliced RAW (not JSON-escaped), and a
  `json`-saved variable as its JSON text. An older executor sends the quoted string. On a
  `money_writes` spending route a text body is REFUSED, never sent (the guard cannot read an amount
  from text). `url`,
  `headers` and `body` resolve `${cid}`/`${cid8}`/`${correlation_id}`/env exactly as an `mcp`
  step's `args` do; `${saved.<var>}` binds at RUN time in all three, same as everywhere else. A
  header value is never written into any report field — a secret in it cannot leak, by
  construction.
  - ⭐ **Basic auth: a helper, never a literal secret.** Write
    `"headers":{"Authorization":"Basic ${basic_auth:<username>:<PASSWORD_ENV_VAR>}"}` (in a plain
    TRIGGER's headers too, not only a chain step's) — `<username>` is written in the clear (it is
    not the secret), and `<PASSWORD_ENV_VAR>` NAMES an environment variable; the executor reads it
    and builds the real `Basic <base64>` header itself, AT RUN TIME, on every attempt. Never
    pre-encode the password yourself and paste the result in — that puts it in the file, the diff
    and git history forever. An env var that is not set refuses the step by name (never an empty
    credential); a malformed form (no `:PASSWORD_ENV_VAR`, an empty username, or a name that is not
    a valid environment-variable identifier) is refused when you write the scenario.
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

  An `http` step's claim is `status=<code>` and/or the SAME `body has`/`body contains`/`body
  matching` grammar every other engine uses — no parallel assertion language. Its value resolves
  `${cid}` / `${cid8}` / `${correlation_id}` to THIS run's own id, exactly like an `mcp` step's
  claim; an environment `${VAR}` here is refused by name, never substituted. It also carries a
  numeric comparison: `body has <field> >|>=|<|<= <number>` — e.g. `- step check: body has
  latency_ms < 400` or `- step check: body has price.usd >= 9.99`. `<field>` is a JSON path in the
  response body (always field-scoped — there is no whole-response numeric form); the observed value
  is parsed as a number and compared to the literal threshold. A field that is absent, or holds
  something that is not a number, FAILS the check (never a pass, never a silent skip) — write
  `body has <field> containing <value>` instead if the field is genuinely text. A bad operator
  (`>`, `>=`, `<`, `<=` and `==` are supported; `=>` and `=<` are refused by name) or a threshold that is neither a number nor one whole
  `${saved.<var>}` is refused when you write the scenario, by name — on an `http`, an `mcp` and an
  `amqp` consume step alike (an `http` step's claims are parsed at write time by the same parser the
  executor uses).

  ⭐ **More claim forms** — judged in Go on `http` and `mcp` chain steps, `mcp` scenarios
  and an `amqp` consume step's `body` claims; a plain (JMeter-run) scenario refuses every one at authoring.
  ⚠ They need **executor 0.3.70 or newer**: an older executor refuses the bullet by name before any step
  runs, so the check reads `error` (never a false green) until the executor is updated. `==` and `within`
  compare every digit, also above 2^53 (amounts in a chain's smallest unit):

  | you want to say | write | holds when |
  |---|---|---|
  | this amount IS that number | `body has total == 42` · `== ${saved.n}` | the field, read as a number, equals it (`1`, `1.0`, `1e0` are one value; never text) |
  | within a tolerance | `body has total within 0.5 of 42` · `within 2% of ${saved.n}` | `abs(observed - ref) <= tol`; `N%` is relative to `abs(ref)`; a ref of 0 with `%` holds only at exactly 0; absent or non-numeric fails |
  | the answer lacks a word | `body not contains error` · `body not matching "status":\s*"failed"` | the whole answer text does not contain it / the regex does not match |
  | a field lacks a value | `body has status not containing fail` · `not matching <regex>` · `not equals failed` | the field IS there and does not contain / match / equal it. ⛔ A non-JSON answer or an ABSENT field **fails** these — a negative never passes because the field is missing |
  | a field is not there | `body has no secret` · `body has no data.token` | the answer is JSON and the path does not resolve (or is null). A non-JSON answer fails |
  | every row satisfies a claim | `body every rows has status not equals failed` · `body every rows has no secret` · `body every $ has id` · `body every ids has . matching ^a\d$` | each element of the array at the path (`$` = the whole answer) satisfies the claim; `.` is the element itself. A path that is not an array fails. ⚠ **An EMPTY array holds** (nothing breaks the claim): when the list must not be empty, add `body has rows.0` (or another claim that it has a first element). A miss names the first breaking element's index |
  | the step answered quickly | `elapsed < 500ms` · `elapsed <= 2s` | the step's ONE request took less than the bound (sent → whole body read). With `poll`, the last attempt is judged. `http` step only; refused beside `unreachable`. The step records `elapsed_ms` either way, and both hats read it; the bound itself is never shown to a builder |
  - **Reading a status miss.** When an `http` step expects `200` and gets `422`, the step's `observed`
    says `http responded status 422; the status claim did not hold` (the wanted status is held out from a builder; you read
    it in `failed_claims` as `status = 200`), and your report carries `body_excerpt`: at most 512 bytes of the response
    body (`... (truncated)` when cut; `(<N> bytes, not text)` or `(empty body)` otherwise), with the instance's credentials
    and every saved value redacted BEFORE the cut. With `poll` it is the last attempt's. It is set ONLY when the status
    claim missed (not on a pass, a body-claim miss or a transport error), is **test hat only**, and exists on chain `http`
    steps only: a plain (JMeter-run) http scenario never shows its body to the Go side.
  - ⭐ **`- step <name>: unreachable` — a connection that must FAIL.** The one claim
    whose expected answer is a transport failure: it passes ONLY when no HTTP response arrived
    because of a connect timeout, a refused connection, a reset or no route (the step waits at most
    5 s). ANY HTTP status fails it (403, 404, 503 …: the network let the request through), and so does a
    DNS "no such host" (a typo is not a blocked path). It stands ALONE on its step (never combined with
    another claim), refuses `poll` and `always`, and is an `http`-step claim only — on an `mcp` step it
    is refused, because it would read as prose. **False-green guard:** it is judged only when an
    EARLIER step of the same chain PASSED (put a positive step that proves the system is up before
    it); otherwise the step is `not-measured`, so a dead cluster cannot pass every negative check.
    Expected unreachability does not stop the chain. It runs in the executor, so it needs an
    executor release that carries it.
  - ⭐ **A counter that must go up between two reads.** The threshold may be `${saved.<var>}`, bound
    from the capture store when the step runs: `"save":{"n":"messages"}` on the step that reads it
    first, then `- step after: body has messages > ${saved.n}` on the step that reads it again
    (put a `poll` on that step if the counter takes a moment to move). A variable no EARLIER step
    saves is refused when you write the scenario; a saved value that is not a number FAILS the step
    ("the saved value of `n` is not a number") and is never printed; `==` is not supported. The
    report shows the claim as written (`${saved.n}`). `${saved.<var>}` in any other claim value of an
    `http` step or an `amqp` consume step (`body has id containing ${saved.x}`) binds the same way.
  - ⭐ **`save` from a text body.** `"save":{"n":{"regex":"messages_deduped_total (\\d+)"}}` saves the
    FIRST capture group of the FIRST match — for Prometheus exposition or any text body with no JSON
    path. The regex must compile (Go RE2) and carry exactly one capture group, or it is refused when
    you write the scenario; no match fails the step ("save failed: the regex for `n` matched
    nothing"). The string form (a JSON path) is unchanged. On an `mcp` step it reads the result's
    content text.
  - ⭐ **Save an object, a list or an exact number as JSON** (needs **executor 0.3.70 or newer**: an
    older executor refuses the step by name before any step runs). The string form saves a scalar as
    text, refuses an object or a list, and sends a number through a float64 (a long id can change).
    `"save":{"res":{"json":"resource"}}` saves the value at the path exactly as written, compact: an
    object, a list, a string with its quotes, a number with every digit (`12345678901234567890`,
    `0.10`), `true`, `false` or `null`; `{"json":"$"}` is the whole response. A later step places it
    in its `body` (http) or `args` (mcp) as a whole value, written as the string it replaces:
    `"body":{"update":"${saved.res}"}` — it arrives unquoted, as a nested object or list. A
    string-form variable beside it is still escaped text. If the body is no longer JSON after binding
    (the reference sat inside a longer string) the step is not fired and reads `not-measured`, naming
    the variable. A JSON-saved variable is refused, by name, in a `url`, a header and any claim — save a
    scalar with the string form for those; one variable has one kind. A value over 65536 bytes fails
    the step. Read a resource, change it, send it back: `save` it with `json`, and `"${saved.res}"` in
    the next step's body.

  ⚠ Numeric comparisons are evaluated in Go (this `http` step and an `mcp`
  scenario) — a plain HTTP TRIGGER (the JMeter-executed path, no `chain`/`mcp`/`ui` tag) refuses one
  at authoring time, because the JMeter template's own assertion script does not understand the
  operator. `${cid8}` is an
  8-lowercase-hex-char token derived from the correlation id (its sha256, first 4 bytes, hex),
  short enough for a resource name a SUT bounds tightly (a Coder workspace name: ≤32 chars,
  `[a-z0-9-]`). An `http` step needs at least one claim, exactly like an `mcp` step; `poll` with no
  claim, a missing `method`/`url`, a `poll.timeout over 10 minutes, and a field an
  `http` step does not carry are all refused by name at validate time — see
  `specs/17-chain-scenarios.md`.
- ⭐ **`amqp` chain step (AC-D18b, extended P3 #24) — one broker operation, judged by the broker's
  answer (and, for `consume`, by what it delivered).** Use it to prove a bus permission holds: a
  spoofed `user_id`, a routing key the identity does not own, a read of another agent's queue, a
  queue declare, or that a check cleaned up what it created. Seven ops: `publish` (always
  publisher-confirmed), `consume` (`queue`, `wait` default 5s, max 30s; a received message is ACKED
  and discarded — never consume from a queue whose messages matter), `declare_queue`,
  `declare_exchange` (`exchange_kind` default `topic`), and the cleanup ops `queue_delete`,
  `queue_unbind` (`queue`+`exchange`+`routing_key`), `exchange_delete`.
  **To see an event WITHOUT taking it, use `listen`, `queue_bind`, `decode`:** a `consume` on an existing
  queue acks and discards the message, which on a live system takes one another consumer was owed. `listen`
  (`exchange`+`routing_key`, both required, `"save": {"q": "queue"}`) makes a PRIVATE queue OneDroid Argus names
  (`argus-tap-<correlation id>-<step index>`, non-durable, `x-expires`/`x-message-ttl` 120000 ms), binds it, and answers
  `{"queue": …}`; then `consume` with `"queue": "${saved.q}"` reads the COPY, and that consume may `wait` up to `120s`
  (30 s on any other queue). The chain deletes the queue when it ends. `queue_bind` (`queue`+`exchange`+`routing_key`) binds
  a queue the chain declared (`force` for a foreign one). `consume` takes `"decode": "gzip"` (also automatic for a delivery
  with `content-encoding: gzip`; 1 MiB cap after decompression); a `schema` decode runs after the gunzip. All of this needs
  **executor 0.3.70**: an older executor refuses a chain holding `listen`/`queue_bind` at preflight, naming the op, but it
  IGNORES `decode` and judges the compressed bytes, so a negative claim (`body not contains …`) over a gzip message holds
  for the wrong reason there. `url_env` is REQUIRED and is
  the NAME of the env var holding the broker URL (`^[A-Z_][A-Z0-9_]*$`) — never a URL; the URL never
  appears in any output. A publish carries exactly one of `envelope` (a v2 agent-message envelope:
  `to_inbox`, `from_agent_id`, `prompt`, `intent` INFORM/ASK/DECISION_REQUEST, optional
  `correlation_id` defaulting to the run's), `body`, or **`schema`+`record`** — plus optional custom
  `headers` (string values only — a non-string value is refused BY NAME, and a schema's own
  declaration headers, e.g. the `envelope` preset's schema header, are reserved); `user_id` is
  set only when given. A `body` that is a JSON STRING is sent UNQUOTED as `text/plain`; any other JSON
  value is sent as its JSON text, `application/json` (an `http` step does this only when its own
  headers declare a non-JSON Content-Type: see the `http` step above). Placeholders resolve exactly as
  for an `http` step, including in a header value.

  ⭐ **`schema`+`record` — any app's wire format is
  test data, never OneDroid Argus code.** `schema` names an entry the app declared under `message_schemas` in
  its `argus-config.yaml` (`format: avro`, a `path:` file or `inline:` text); `record` is a JSON
  object in that schema's shape (spec 17's ONE JSON record rule — plain values, a nullable union bare,
  a 2+-branch union tagged `{"<type>": value}`, bytes/fixed base64). Checked against the schema TWICE
  (when the chain is parsed, and again at run time once every placeholder resolves) and refused BY
  FIELD PATH on a misfit. Placeholders include the usual `${cid}`/`${cid8}`/`${correlation_id}`/
  `${saved.<var>}`/env `${VAR}`, plus `${uuid}` (fresh per occurrence) and `${now}`/`${now_ms}` (one
  instant per step); a JSON string that is EXACTLY one placeholder may fill a non-string field
  (`"${now_ms}"` → long). A `${VAR}` whose name looks like a credential
  (`(?i)(token|secret|password|passwd|key|credential|auth)`) is refused in a `record`, by name, its
  value never read — put a credential in a header from the executor's own Secret instead. On
  `consume`, an optional `schema:` decodes the delivered body so `body has <field> …` reads its
  DECODED fields; an undecodable delivery is its own failure text, never a content mismatch. The
  `envelope` form above is this same path as a PRESET (a built-in agent-message schema, a default record from
  its four fields) — pair it with an optional `record` to override any OTHER field the preset
  hard-codes, e.g. `"record":{"refs":["slack:<channel>"]}` for a bridge that must not fall back to a DM.
  See spec 17's amqp section for the full table and the JSON record rule.

  ⭐ **Check a `schema`+`record` step locally before writing it:**
  `argus validate-scenario --file <draft.md> --config <argus-config.yaml>` applies the FULL check
  against the declared schema (the same one a real run applies) and refuses a misfitting record BY
  FIELD PATH, with nothing run. `--config` is what makes this possible — the control plane's
  `author_validate_scenario`/`author_write_scenario` never has the app's `argus-config.yaml` (it
  lives on the executor), so there the check is shape-only: one payload form, `schema` names a
  declared-looking ref, `record` is an object, no secret-looking `${VAR}`. Without `--config`,
  `validate-scenario` is that same shape-only check, unchanged from before this schema support
  existed.

  ```json
  {"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"BUS_TEST_URL",
    "exchange":"bus.inbox","routing_key":"inbox.agent.example","user_id":"example",
    "headers":{"x-retry-count":"0"},
    "envelope":{"to_inbox":"agent.example","from_agent_id":"argus-test","intent":"INFORM",
                "prompt":"argus-test: ${correlation_id}"}}]}
  ```

  ```
  ### Runnable
  - step spoof: broker refuses with 406
  ```

  ⛔ **ONE PUBLISH PER STEP PER RUN** — a real agent's inbox may be on the other end. An `amqp` step
  may carry neither `poll` nor `always`; it is never retried, never re-fired as a cleanup, and a chain
  containing one is never re-fired after a rate-limit.

  **Claims.** Every op takes the two broker forms — `broker accepts` or
  `broker refuses with <3-digit code>` — and anything else is refused at validate time, e.g.
  `- step spoof: status=200` → "claim \"status=200\" is not an amqp-step claim", and
  `broker refuses with 4x6` is refused the same way. Two different broker claims on one step, a
  `broker …` claim on a non-amqp step, and an amqp step with no claim are refused too. **`consume`
  additionally takes:**
  - a **body assertion** on the delivered message — the SAME grammar an `http` step's claim does
    (`body contains <value>`, `body equals <value>`, `body has <field> containing/matching/equals
    <value>`, `body has <field>` for exists); it implies `broker accepts` (a message must arrive to
    be checked) and may be paired with an explicit `broker accepts`, but never with
    `broker refuses with <code>`. The value resolves `${cid}` / `${cid8}` / `${correlation_id}` to
    THIS run's own id, exactly like any other check — `- step consume: body contains "argus-lab
    NLB-001 ${cid8}"` is checked against the run's OWN derived token, never the literal text. An
    environment `${VAR}` here is refused by name, never substituted (the value would be printed in
    the report).
  - **`queue is empty`** — the one claim form an EMPTY consume can PASS: it passes when the wait times
    out with no delivery, and fails (showing what arrived) when a message does. It cannot be combined
    with a body assertion or a `broker …` claim (a genuine broker refusal, e.g. 403 on the queue
    itself, is a DIFFERENT failure from silence, and still fails `queue is empty`). It passes ONLY on
    a genuine timeout on a healthy consumer. If the consume ended any other way with no reply code
    (the consumer channel closed, the executor's watchdog tore the connection down, a connection
    error), the step is an `error` saying so, never a pass: a lost broker is not an empty queue.

  On a failure the report shows the observed reply code / delivered-message summary and the broker's
  redacted text — never the asserted value (the test hat's step carries each missed claim, as written, with what was observed, in `failed_claims`). A pure-amqp
  chain needs no `targets.mcp`/`targets.http`.

  **Cleanup ops (`queue_delete`/`queue_unbind`/`exchange_delete`).** Each refuses, BY DEFAULT, a
  queue/exchange name this SAME chain never declared with its own `declare_queue`/`declare_exchange`
  step — "a check must not be able to delete a live queue by typo." Set `force: true` on the step to
  act on a name the chain never declared. `queue_unbind` checks BOTH the queue and the exchange.

  ```
  {"steps":[
    {"type":"amqp","name":"dq","op":"declare_queue","url_env":"BUS_TEST_URL","queue":"argus-probe-q"},
    {"type":"amqp","name":"cleanup","op":"queue_delete","url_env":"BUS_TEST_URL","queue":"argus-probe-q"}
  ]}
  ```
  ```
  ### Runnable
  - step dq: broker accepts
  - step cleanup: broker accepts
  ```

  See `specs/17-chain-scenarios.md`.

  ⛔ **`## LOAD` does NOT drive an `amqp` step.** A `## LOAD` profile only ever feeds the JMeter
  thread group behind a plain HTTP TRIGGER — a chain (of any step type, `amqp` included) ignores it
  entirely today. There is no way to declare an AMQP connection/session count or a publish rate.
  Given this step's one-publish-per-run safety rule (a publish may land in a real agent's inbox),
  driving load through `publish` is a deliberately separate, larger piece of work — not a small
  addition here.
- **Rate Limiting** scenarios judge by status (e.g. drive traffic over the limit, EXPECT `status=429`). The
  suite can only CONFIRM the limiter engaged if the SUT emits a control saga for the rate-limit decision
  (`saga-presence`); if the SUT does not instrument that, the scenario can only assert the response code. (C12)
- One scenario per file, in the correct `<layer>/` directory. Never hardcode credentials.

### Placeholders — which one is filled in where

| written in | this scenario's correlation id: `${cid}` / `${correlation_id}` (`${cid8}`, its 8-hex-char short form, resolves alongside it everywhere it does) | an environment `${VAR}` |
|---|---|---|
| a step's `args`, the MCP TRIGGER payload, `server_url`, request headers, a ui step's `spec` / `app_url`, an http step's `method` / `url` / `headers` / `body` | filled in | filled in (a chain http `url` that STARTS with `${INGESTION_URL}` gets the `targets.http.base_url` host and port instead; `check_env` makes onboarding deliver a name to the executor, and any name present there is filled in) |
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

**Adding a `${VAR}` credential after the instance is already enrolled** (a new scenario needs one, or the SUT rotated
one): `render-k8s --secrets-env-file` only writes it at RENDER time. The supported path afterward is
`argus secrets set` — value read from stdin only, never a flag, never echoed:
```
echo -n "$VALUE" | argus secrets set --key DB_PASSWORD --namespace argus-inst-<id> --restart
```
`--restart` is needed because the executor reads its Secret only at pod start; without it, `argus secrets set`
prints the exact `kubectl rollout restart` command instead of running it. See the tester guide,
https://docs.onedroid.ai/argus-tester-guide.

⛔ **There is no `secretRef`.** A scenario's credential cannot be read live from a Secret in the SUT's own
namespace at run time, even when the SUT grants the executor a RoleBinding for exactly that — the executor has no
Kubernetes client for reading an arbitrary Secret and no RBAC for `get secrets` anywhere (its one in-cluster grant
is self-delete/self-scale on its OWN pod/Deployment, rendered by `internal/k8srender`). A value
`secrets set` writes is a **copy**: it goes stale **silently** when the SUT rotates the source, and nothing
detects the drift. Re-run `argus secrets set` after every rotation — do not assume a `url_env`/`${VAR}` value
stays current on its own.
