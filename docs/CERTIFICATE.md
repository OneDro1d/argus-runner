# The certificate (T6.3)

> **Downstream of [`docs/DEPLOY-ARGUS.md`](DEPLOY-ARGUS.md) — the single door.** This document covers one
> narrow artefact: the shareable, third-party-verifiable certificate a certified run produces. For
> everything else (deploying, onboarding, running scenarios) start at the door.

## What a certificate proves

A certificate is **proof without the tests**. It is a small, typed JSON document a run produces once it
is fully certified — a `final` run whose certification set has its own sealed (version-2) ledger anchor
and whose verdict has an anchored (version-3) ledger entry — and it proves exactly two things to anyone
holding it, with no Argus credential and no access to the scenario catalog:

1. **A scenario set existed, sealed, before the run happened.** The certificate carries that set's own
   commitment (its root hash, the acceptance-criteria hash it was authored against, its content hash and
   how many scenarios it held) and the chain record that anchor was written to.
2. **This verdict was anchored against that set.** The certificate carries the run's verdict, its tallies,
   the two content digests the run bound (the artefact digest and the evidence-bundle hash), and the
   chain record the verdict itself was anchored to.
3. **If the holdout has been revealed, the certificate says so.** Once a run's reveal has been assembled
   (`author_get_reveal` — see below), that fact is ALSO anchored on chain, as the next ledger version
   after the certifying commitment's CURRENT HEAD at the moment the reveal is assembled — version 4 for
   a commitment's first certifying run when no scheduled re-run has landed since, but not fixed: a
   commitment's version counter is shared by every run against it (AC-12), so a reveal for an earlier
   run can land at 5, 6, or higher once a later run's verdict has advanced the head first. A revealed
   run's certificate carries `revealed_at` and `reveal_document_sha256` — the digest of the exact reveal
   document the control plane persisted and handed out — plus that anchor, whatever version it landed
   at. A run whose reveal has not been anchored carries neither field: the seal reads as intact, never
   fabricated as broken.

**Several sets per instance.** An instance can hold several draft and several sealed certification sets
at once, each owned by ONE commitment (migration 063), so several tester sessions can certify on one
instance at the same time. A run is made against one of them and its verdict binds to that one
(`run_ledger.commitment_id`), so a certificate always describes the set its run certified, never a newer
or older one on the same instance. Revealing one set leaves the others untouched, and a revealed set's
scenarios do not roll into the next draft. To choose a set, name it with `commitment_id`
(`author_list_commitments` lists them); the certificate itself still carries no commitment id.

Both (2) and (3) are independently checkable: `argus certificate verify` (below) reads each anchor back
from its own chain and confirms the read-back payload matches what the certificate declares, under the
anchor's own recorded signer. It classifies each anchor's role (set / verdict / reveal) by its position
relative to the others, never by comparing its version number to a constant — see
`internal/certificate/verify.go`'s `roleOfAnchors`. A tampered `reveal_document_sha256` — declaring a
digest the chain's own record does not carry — fails the reveal-anchor check.

## Formats: what is anchored and what is not

| | `argus-certificate/v1` (issued before 2026-09-30) | `argus-certificate/v2` | `argus-certificate/v3` (current) |
|---|---|---|---|
| verdict anchor holds | run_id, artifact digest, evidence-bundle hash, verdict | the same, plus **tallies** (passed/failed/errored/degraded) and **finished_at** | the same as v2 |
| tallies, finished_at | **NOT anchored** — `verify` checks the tallies for consistency only and prints `NOT ANCHORED:` lines | anchored: changing either fails the verdict anchor | anchored |
| which build was running | not shown | not shown | `artifact_measurement`, **required**, bound by the evidence-bundle hash |

`verify` chooses the payload to rebuild from the certificate's **declared** format, so relabelling a v2
certificate as v1 (or the reverse) does not verify. A v1 or v2 certificate keeps verifying if it is honest.
An older `argus` binary does not know v3 and refuses it by name; update the CLI.

### Which build was running (`artifact_measurement`, v3)

`verdict_record.artifact_digest` is what the tester **declared**. Before a `final` or `scheduled` run
starts, the executor reads the image digests the system under test is actually running (Kubernetes: the
pods' `imageID` in the SUT namespace; compose: the project's images' repo digests). The result is on the
certificate:

| `state` | meaning |
|---|---|
| `matched` | the declared digest is one of the `running` digests |
| `not_measured` | the executor could not tell, and `reason` says why (no permission to list pods, no SUT found, tag-only images, unknown tier, docker not available, no digest declared). **Never a pass.** |

A run whose declared digest is provably **not** running is refused before any scenario runs; it has no
verdict and no certificate. A run from an executor that predates measurement reports none, and its
certificate is issued as `argus-certificate/v2`: v3 would have nothing to add, and older verifiers can check
a v2 in full. `verify` says of every v2 certificate that it shows nothing about which build was running.

`verify` rebuilds the measurement from the certificate and checks that it hashes, together with
`scenario_evidence_root`, to the anchored `evidence_bundle_hash`. Editing the state, a digest, the reason, the
source, the commitment lists or the declared digest, or deleting the block, is a `MISMATCH` (exit non-zero).
`verify` prints `NOT BOUND: artifact measurement — NOT MEASURED — <reason>` for an unmeasured run and never
`VERIFIED`.

What this does **not** establish: the running digests are what the **executor reported** reading from the SUT's
cluster or docker daemon. They are bound to the verdict, but neither the chain nor the control plane observed
them. `commitment_images_found` / `commitment_images_not_found` say which of the sealed commitment's
`image_digests` were seen running; they are recorded, not enforced. A holder can drop the block (relabel the
certificate v2) or claim `bound: false`; that claims less than is true and `verify` says nothing is bound.

The SUT owner must let the executor list pods in the SUT namespace (the read-only Role from
`--emit-sut-access-role`); without it every certifying run reads `not measured: forbidden`.

Every certificate, either format, is also refused (`MISMATCH: certificate tallies`, exit non-zero) when its
counts contradict what IS anchored or sealed: the four counts must not add up to more than
`set.scenario_count`; verdict `passed` needs every scenario passed; `failed` needs at least one failed or
errored; `degraded` needs none failed or errored. A degraded scenario is counted in none of
passed/failed/errored, which is why v2 carries a `degraded` count and why a v1 certificate for a degraded or
partly-degraded run legitimately has passed+failed+errored below `scenario_count`.

`issued_at`, `anchoring` and `anchors[].chain_id` describe the document, not the run, and are never anchored.

### Is anchoring finished? (`anchoring`)

A certificate is issued as soon as ANY certification chain holds the verdict. `anchoring.status` is
`in_progress` while a chain of the commitment has no verdict anchor yet — those chains are named in
`chains_awaiting_verdict`; **fetch the certificate again** until it says `complete`. Hash-only (OTS) chains
that are anchored but not yet attested by a Bitcoin block are named in `ots_awaiting_bitcoin` (expected,
hours) and do not make the status `in_progress`.

A verdict that failed to anchor on a chain is **retried automatically**: about every 10 minutes, for 72 hours
after the run finished. So is a run whose results push **stopped part-way** (the control plane restarted after
recording the terminal outcome and before the verdict was written): the outcome is recorded together with a
"bind started" marker, and the sweep retries such a run once 10 minutes have passed. While a retry is coming,
`anchoring.note` says which chain and until when. A retry only ends the wait once the run really holds a
verdict row on **every** chain of its commitment's snapshot; a chain that has no ledger head row for the
commitment, or is not configured on this control plane, keeps the run retryable (and its failure text names the
chain and why) rather than being reported as done. A run whose verdict is already on every chain never carries
a failure, even if a late duplicate results push is refused. When a retry is not coming
(the 72 hours passed, the commitment was revealed or burnt, the run's reveal was already anchored, the run
has no `artifact_digest` to anchor, or no failure was ever recorded for it and no bind was marked started, as for a
run recorded by a control plane older than this behaviour) the status is `incomplete`: the
note says plainly that the verdict was **not** anchored on that chain and is no longer retried, and why.
Fetching again will not change it, and the certificate verifies only what it carries. A client that predates
`incomplete` ignores `anchoring.status`; nothing in `certificate verify` reads it.

A chain's verdict may sit at a different ledger version from the same run's verdict on another chain (a
back-filled verdict lands at that chain's head + 1, which a later scheduled run may already have advanced).
`certificate verify` and `argus anchor verify` classify verdict and reveal per chain, so this verifies.

### The Ledger tab and `author_list_ledger_anchors`

`GET /api/ledger/anchors` and the MCP tool `author_list_ledger_anchors` list anchors newest first. **When the
workspace anchors every run (the `run` setting), most of the newest anchors are `run` anchors**, and a call
without `kind` fills its page with them: filter with `kind` (for the certification record, `verdict`,
`reveal`, `set`, `commitment`) or page on with `next_before` / `before`. Both also return
`verdict_anchor_failures` (newest 20, this workspace only, RPC URLs stripped, as in every error the control
plane stores or answers with): each run whose verdict failed
to anchor, with `retrying` and `retry_until`, and, once it says `retrying: false`, the reason in `error`.
The web response also carries `run_anchor_failures` (a run's own result-hash anchor, a different record).

### Reading a chain through the wrong `--rpc`

`--rpc` is one client for every Ethereum-style anchor. If an anchor cannot be read, the status is
`unreachable` (never `mismatch`) and the detail names the anchor's chain and the chain id the `--rpc`
endpoint serves, then the node's own error. v2 certificates record each anchor's chain id, so a mismatch is
stated outright; a v1 certificate does not, so it only names the anchor's chain and says so.

### `argus anchor verify` judges a reveal document the same way

`argus anchor verify <reveal.json> --rpc <url> --btc-headers <url-or-file>` prints one line per check,
with the same four statuses: `VERIFIED`, `MISMATCH`, `UNREACHABLE`, `PENDING`.

- **Exit 0** only when no line is a `MISMATCH` and at least one of this run's verdict anchors (version 3
  or later) is `VERIFIED`, on any chain. The last line says `OVERALL: verified` or `OVERALL: NOT verified`.
- An anchor on a chain other than the one `--rpc` serves, no `--rpc`, or an OTS receipt with no
  `--btc-headers` is `UNREACHABLE`; an OTS receipt not yet in a Bitcoin block is `PENDING`. Neither fails the
  run by itself, but a run whose verdict anchors are all unreachable or pending is not verified either.
- **`--btc-headers` takes the merkle root in display order.** Give it an `http(s)://` base URL of a block
  explorer API (`https://blockstream.info/api`, `https://mempool.space/api`), or a local JSON file
  `{"<height>": "<merkle root>"}`. In both, the root is written exactly as a block explorer or
  `bitcoin-cli getblockheader` prints it (display order, the byte-reverse of the order inside the block
  header, which is what an OTS receipt folds to); `argus` reverses it before comparing. For block 969288:
  `{"969288": "b8100af53f7ae4f30d4fb59da3c0a016ae2f1c8e29f27e3e2d011b9e0d37b8d9"}`. A root in the wrong
  order is a different block's root, so the receipt is a `MISMATCH`. A file written for v0.3.46, which read
  the root in the receipt's own order, must be re-written in display order.
- The holdout root, each inclusion proof and the evidence bundle hash are computed from the document
  itself, so a failure there is always a `MISMATCH`. So is a payload that differs from the chain's, a
  signer the chain refuses, and an OTS receipt the `--btc-headers` source contradicts (a wrong merkle root
  at its block height, or a receipt that does not parse). A header source that merely cannot answer
  (unreachable, unknown height) stays `UNREACHABLE`: that is "could not check", not a contradiction.
  `certificate verify` judges OTS anchors the same way.
- A reveal document does not record an anchor's chain id, so the `UNREACHABLE` line names the anchor's chain
  and the chain id `--rpc` serves (the wording of `certificate verify`'s v1 case).
- There is no flag that turns `UNREACHABLE` into a failure; `certificate verify` has none either.

## What it does NOT prove or reveal

A certificate is not a reveal document (`argus anchor verify` / `author_get_reveal`, AC-7) and does not
carry what one carries:

- **No scenario content.** No scenario id, path, title or body; no `TRIGGER`/`VERIFY`/`EXPECT` text. Only
  `set.scenario_count` — how many scenarios were sealed, never what they were.
- **No observed values.** No evidence, no log lines, no response bodies — only the verdict (`passed` /
  `failed` / ...) and the pass/fail/error tallies.
- **No commitment id, workspace id, organisation id or instance id/name.** Nothing that identifies which
  Argus account or SUT produced it beyond what the anchor's own chain record already carries.
- **No host, dashboard link, or Grafana URL.**
- **No version-1 (commit-level) anchor.** Only the set anchor (always version 2), this run's own verdict
  anchor and — once revealed — this run's reveal anchor appear; the LATTER TWO are not fixed at 3/4 (see
  above) and are told apart by relative position, not by their version number.
- **No commitment_id inside the reveal-anchor check either.** The real on-chain reveal payload
  (`ledger.RevealV4` — `internal/ledger/ledger.go`) carries `commitment_id`; a certificate never does, so
  `certificate verify`'s reveal-anchor check decodes the read-back payload and compares kind/run_id/
  document-digest rather than doing a byte-exact comparison the way it does for the set/verdict anchors.
  An OTS/hash-only reveal anchor with no Ethereum-style sibling cannot be checked this way at all (a
  certificate has nothing to reconstruct the receipt's digest from) — `certificate verify` reports it
  `unreachable` and points at `argus anchor verify` against the full reveal document instead.

If you need the scenario set itself, its Merkle proofs, or the declared per-scenario verdicts, that is
`author_get_reveal` — an **author-scoped** call, never a certificate's job.

## The two commands

### Get a certificate

```
argus certificate get --instance-id <id> --run-id <id> --control-plane <url> [--token <tok>] [--out cert.json]
```

Calls `author_get_certificate` with the same author session credential the other `cloud-*`/`runner-id`
commands use (`--token`, or `ARGUS_CP_AUTHOR_TOKEN`, formerly `ARGUS_CP_TOKEN`). Refused by name — never a generic failure — for an unknown
run, a non-final run, a certification set with no version-2 anchor yet, or a verdict not yet anchored.

A set's version 2 is written to every certification chain when it is sealed. `author_seal_set` answers a RESULT, never an
error, once the set is sealed, **even when every chain failed**: read `anchored_everywhere` first. `false` means version 2
is NOT on every chain (the commitment is sealed, but no certifying run may be requested yet), and `next` is one sentence
saying what to call (`author_get_commitment` until `anchoring.status` is `complete`, or `author_repair_set_version` for a
chain that is `in_doubt`). `anchoring` carries the detail per chain. A chain whose write ended before any
transaction was sent (`not_sent`) is retried by the control plane by itself, about every 10 minutes for up to 72 hours (a
hash-only chain too: stamping the same bytes twice cannot fork anything). A
chain whose write MAY have sent a transaction (`in_doubt`; the control plane stores that BEFORE it sends, so a process that dies
mid-write leaves the same state) is not written again by anything the control plane runs, because a
second write could fork the record: `author_repair_set_version` reads the chain from version 1's own block, and records a
version that landed, or writes it once when nothing landed and the signer has nothing in flight, or refuses and says why
(including when version 1's block is not recorded or more than 20000 blocks have passed since it). A verdict is never written on a
chain ahead of the set's version 2 there: until it is, that chain is awaiting its verdict, and `author_get_commitment` shows
`anchoring` with `chains_awaiting_set_version` and each chain's outcome. A verdict write that may have been sent is likewise not
written again (the certificate then reads `incomplete` for that chain, with the reason).

### Verify a certificate

```
argus certificate verify <certificate.json> [--chains <chains.json>] [--rpc <url>] [--btc-headers <url-or-file>] [--json]
```

**Pre-auth, offline and independent** — this never contacts the Argus control plane; it needs no
`--control-plane` and no token. For each anchor it reads the version back from its own chain (an
Ethereum-style anchor via `--rpc`, a hash-only/OTS anchor via `--btc-headers`) and checks the read-back
payload against the certificate's own declared `set`/`verdict_record` fields, under the anchor's own
recorded signer. Passing `--chains` additionally cross-checks that signer against a **third party's own**
copy of `chains.json`, rather than trusting the certificate's self-declared signer alone.

`--btc-headers` is a block explorer API base URL (`https://blockstream.info/api`,
`https://mempool.space/api`) or a local JSON file `{"<height>": "<merkle root>"}`. Both take the merkle root
in **display order**, exactly as a block explorer or `bitcoin-cli getblockheader` prints it; `argus`
reverses it to the block header's internal order, which is what an OTS receipt folds to. Example, block
969288: `{"969288": "b8100af53f7ae4f30d4fb59da3c0a016ae2f1c8e29f27e3e2d011b9e0d37b8d9"}`. A file written
for v0.3.46, which read the root in the receipt's own order, must be re-written in display order; a root
in the wrong order reads as a different block's root and the receipt is a `mismatch`.

Each anchor reports one of `verified | mismatch | unreachable | pending`. The command exits `0` only when
this run's own verdict anchor verified and no anchor mismatched — a reveal anchor, when present, is
checked the same way and a mismatch there also fails the command, but its absence never does (an
unrevealed run's certificate simply carries no reveal entry to check).
