# The results directory — what is in it, and what may leave

The execution plane writes one directory per run instance (`<results>/<instance>/`). It is both
the report's home and JMeter's scratch space, and the two must not be confused when anything
bundles it for a reader outside the environment.

| Entry | Written by | Leaves the environment? |
|---|---|---|
| `report.json` | the runner, after the run | **yes** — the product's evidence |
| `runs/<run_id>.json` | the runner, one per run | **yes** |
| `<template>__<scenario_id>.jtl` | JMeter (`-l`), raw samples | no |
| `<template>__<scenario_id>.jtl.log` | JMeter (`-j`), its run log | no |
| `cleanup__<scenario_id>.jtl` | JMeter, the SQL cleanup's samples | no |
| the results outbox | the runner, queued result pushes | no |
| `outputs/<run_id>/<scenario_id>[__<step>].<sample>.json` | the runner, in a `compare` run only | **no** — see "Recorded outputs" |
| `capture/c-*/<n>.{status,headers,body}` | JMeter's capture block, in a `compare` run only | **no** — scratch, deleted after each check |

Bundling code (a reveal package, a publish step, a support archive) takes
`argus.PublishableResults(dir)` / `argus.PublishableResult(rel)` (`internal/argus/results_bundle.go`)
and nothing else. It is an allow-list: a new kind of file stays out until it is added there.

## Credentials never reach the directory

The templates read the app-under-test's credentials as JMeter properties
(`${__P(auth.header)}`, `${__P(db.password)}`, `${__P(mgmt.auth.header)}`, and the author-declared
header values). JMeter echoes every `-J` command-line property into its run log, name and value
(JMeter 5.6.3, `org.apache.jmeter.JMeter#initializeProperties`: `Setting JMeter property: {}={}`),
so those used to sit in cleartext in every `.jtl.log`.

They no longer travel as `-J`. The runners split the property set (`internal/argus/secret_props.go`):

- **local runner** (the execution-plane image): the credential-bearing properties are written to a
  private `0600` file in the temp dir and passed with `-q <file>`; JMeter logs only the file's
  path (`Loading additional properties from: {}`), and the file is removed when JMeter exits. The
  run log is pre-created `0600` and the results dir `0700`.
- **compose runner**: the same properties are piped on stdin to a small `sh` wrapper inside the
  executor container, which writes them to a `0600` file in the container's own temp dir, runs
  JMeter with `-q` on it and removes it — nothing touches the shared results volume or a command
  line.

Every other property keeps travelling as `-J`, so the templates are unchanged. The set of names
treated as credentials is closed and pinned by `internal/argus/secret_props_test.go`; a new
credential-bearing property is added there, never smuggled under a public name.

## Recorded outputs (run mode `compare`)

A `compare` run (one member's run of a sealed set, ARGUS-CMP-3/-05) records the output of
each check that declares `## COMPARE`. It is the ONLY mode that does, and the only kind of check. What it
keeps, and where:

- **The digest**, in `report.json` and `runs/<run_id>.json`: `ScenarioResult.outputs[]`, one record per
  sample (a chain `http` step, a JMeter sampler): state, a closed reason, status, three part hashes, the
  total hash, body kind and size. Never a body, a header value or a claim. It rides the same bytes the
  evidence hash is taken over, and crosses to the control plane on the results push
  (`outputs`, `outputs_root`, `env_fingerprint`).
- **The body**, executor disk only: `<results>/<instance>/outputs/<run_id>/<scenario_id>[__<step>].<sample>.json`
  holding `{status, headers, body_kind, body}` in canonical form, masked and scrubbed. The directories
  are `0700` and the files `0600`: not readable by group or world. Only the `get_output` relay verb
  (`{run_id, scenario_id, step, sample}`, enqueued by an author tool) reads it back; the four inputs are
  checked so nothing outside the directory can be named, and a missing file is a closed error with no path.
  A text body that is not valid UTF-8 is stored losslessly as base64 (`body_encoding: base64`, `body_b64`,
  `body: null`; headers likewise as `headers_b64`), because the hash is over the exact bytes.
  The stored body is at most 256 KiB of canonical text, and the whole file at most 768 KiB encoded (a
  control byte is a 6-byte escape, so the body is cut further until the file fits); a cut file carries a
  top-level `"truncated": true` (absent when whole) and the record says `truncated` too; the hash covers
  everything that was read. A body over 1 MiB is not recorded at all (`body_too_large`).
  In the file name `~` is written `~7e` and an underscore that is first, last or beside another underscore
  `~5f`, so two different (check, step, sample) never share a file; an ordinary id is unchanged. A re-fire
  of a check first removes that check's files of the earlier attempt in the same run directory.
- **The scratch** for the two JMeter http templates: with the property `output.capture.dir` set (mode
  `compare`, a check with `## COMPARE`, never a check with `## LOAD`) the template's capture block writes
  `<dir>/<n>.body`, `.headers` and, last, `.status`, at most 8 samples. The executor reads them, builds the
  record with the same `compare.BuildRecord` the chain uses, and deletes the raw files on every path,
  a failed run included. Nothing of a response is logged: not by the executor, not by the template.

**Numbers in a record (executor release E2, ARGUS-CMP-11).** Two optional keys of a record in `outputs[]` (both
absent otherwise, so every other row and its evidence hash are byte for byte what they were):

- `values`: `[{"path": "$.total", "rule": 0, "value": 100.004}]`, the numeric leaves a `**Tolerance**` rule governs, as
  numbers (`path` the leaf's location, `rule` the index into the check's rules, at most 64 per check). The leaf is
  `"<tolerance>"` in the hashed (and the stored) body, so two outputs within tolerance hash alike. `outputs_root` covers them: for a row that carries `values`
  (or `load`), the root adds one line per tolerant value (rule index, path, canonical number) and one for the load numbers after
  the row's own line; a row that carries neither contributes exactly the bytes it always did, so a root computed by a released
  0.3.57 executor stays valid. A number changed in transit therefore no longer matches the root (the push stores no outputs and
  still lands the run). The evidence hash covers them too, through `report.json`.
- `load`: `{"samples": 120, "p50_ms": 40, "p95_ms": 90, "p99_ms": 130, "error_rate": 0.02}`, on the ONE record of a
  `## LOAD` check that declares `**Not Worse Than**`: `state` `not_recorded`, `reason` `load_numbers_only`, no `status`,
  `parts` or `hash`, and no stored file under `outputs/` (a load check records no body). `error_rate` is a fraction (0..1).

A row whose numbers are not valid (a negative or non-finite number, an error rate over 1) is left out by the control plane,
alone: the push is never refused over it. Numbers reach the author's reads only (the control plane's `run_ledger.outputs`;
a load p95 also in the comparison's `performance`); they are never logged, pushed to Loki or the Pushgateway, or anchored.

**Retention.** The newest 20 run directories of an instance are kept (by directory modification time,
the run just pushed never removed). Older ones are removed only AFTER a results push the control plane
accepted (`runner.OutputRetention`, hung on `Executor.AfterPush`), so a run whose results never arrived keeps
every directory it could still be asked about. The remover refuses any run id that is empty, absolute, holds
a path separator or `..`, is a symlink, or does not resolve directly inside `outputs/` (`argus.RemoveOutputRun`),
and a file or symlink in `outputs/` is never a candidate.

**Not publishable.** `outputs/` and `capture/` are not on the allow-list (`argus.PublishableResult`) and
must not be added: they hold bodies. Personal data in a body cannot be detected generically; the rule is
locality (the body stays here) plus the author's masks.
