# OneDroid Argus runner

This repository is the open part of OneDroid Argus: the **runner** that executes checks against a system you
own, and the **`argus` command line** a tester uses, including `argus certificate verify`, which lets anyone
who holds a OneDroid Argus certificate check it against the chain without trusting OneDroid.

OneDroid Argus is an end-to-end test and certification service. You describe checks for your system as
scenario files. The runner executes them next to your system (HTTP, database state, message flow, log lines, load, web UI,
and faults you allow it to inject: a stopped deployment, a slow or cut connection) and reports to a control plane. The control plane keeps the registry, signs, and anchors results on a public
chain. **The control plane is a hosted service and is not in this repository.**

Licence: MIT (see `LICENSE`). The AMQP sampler under `jmeter-plugins/amqp` keeps its own Apache-2.0 licence file.

## What a OneDroid Argus certificate proves today, and what it does not

Read this before you rely on a certificate.

**What it gives you.** A certificate states that a certification set (the scenarios, sealed before the run) was
run against a particular build of a system and produced a verdict. The control plane signs that statement and
anchors it on a public chain. `argus certificate verify` re-reads those anchors from a chain client you choose
and checks that the certificate's declared fields (verdict, tallies, set hash, artifact digest, time) are
exactly what was anchored, and that each anchor was written by a signer you allow. A certificate edited after
the fact fails this check.

**What it does not give you.** The runner is open source and it evaluates the sealed checks itself, on the
machine where it runs. The control plane signs and anchors what the runner reports; it does not re-run the
checks. So **a modified runner could report passes it did not earn**, and the certificate would still be
anchored and would still verify, because verification proves what was reported and anchored, not that the
checks truly ran as written. Decoy checks, whose purpose is to catch a runner that reports results it did not
earn, **are not implemented yet**. Until they are, treat a certificate as a signed, anchored statement by the
runner's operator and the control plane, not as proof that no one tampered with the runner. A OneDroid Argus
certificate makes no promise that the runner was unmodified.

Other limits worth knowing: an anchor proves the statement existed no later than its block; it does not prove
the statement is true. A Bitcoin (OpenTimestamps) receipt is "pending" until the calendar has a block
attestation. `argus certificate verify` reports `NOT ANCHORED` for fields a given certificate format does not
put on chain.

## Quickstart

You need an account on the hosted control plane at `https://argus-dev.onedroid.ai` (create one there: **Sign up**
with e-mail, Google or Microsoft; it is the evaluation environment, with no promise that data is kept, so keep
production data out of it), a workspace in it (step 2), and a Kubernetes cluster that can reach both your system
and the control plane (the runner connects out only; it needs no inbound route). The steps below install on
Kubernetes. Argus can also run under Docker Compose with no cluster, but that path needs the onboarding kit,
which this repository does not ship (see the note after step 5). Stuck at any step? `argus doctor`
is read-only and prints, for each thing a first run would trip over, the command that fixes it.

1. **Get the runner.**
   Pull the published image (`docker pull ghcr.io/onedro1d/argus-runner:main`; each build is also tagged
   `sha-<commit>`, and step 4 pins it by digest), or build it (`docker build -t argus-runner .`, see
   `BUILDING.md`). To get only the CLI: `go build -o argus ./cmd/argus`.

2. **Sign in to the hosted control plane, and make a workspace.** The sign-in opens a page in your browser where
   you approve the CLI with the account you created. A workspace holds everything for one system under test;
   `cloud-create-workspace` prints its id, which step 4 needs (or create it in the app, **Settings → My workspaces**).

   ```sh
   argus cloud-login --control-plane https://argus-dev.onedroid.ai --scope author
   argus cloud-create-workspace --control-plane https://argus-dev.onedroid.ai --name <name>
   argus doctor --control-plane https://argus-dev.onedroid.ai   # read-only: what a first run would trip over
   ```

3. **Check your kit locally.** `examples/http-hello` is the smallest working kit (one config, one scenario).

   ```sh
   argus validate-config --config examples/http-hello/argus-config.yaml --scenarios examples/http-hello/scenarios
   ```

4. **Onboard an instance.** The enrollment token lives 900 seconds, so run these in one go. Read it from its
   file inside a script so it never sits in your shell history.

   ```sh
   argus cloud-enroll --control-plane https://argus-dev.onedroid.ai --instance-id hello --token-dir ./enroll
   export ARGUS_ENROLLMENT_TOKEN="$(cat ./enroll/hello.enrollment)"
   export ARGUS_RUNNER_TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
   export ARGUS_EXECUTOR_SECRET="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
   export ARGUS_CP_URL=https://argus-dev.onedroid.ai
   export ARGUS_WORKSPACE_ID=<your workspace id>
   argus render-k8s --config examples/http-hello/argus-config.yaml --scenarios examples/http-hello/scenarios \
     --instance-id hello --sut-namespace <your namespace> --tier managed --obs none \
     --image ghcr.io/onedro1d/argus-runner@sha256:<digest> --kube-context <ctx> --out ./rendered
   kubectl --context <ctx> create -f ./rendered/executor.yaml   # create, never apply: it holds a Secret
   ```

   Delete `./rendered/executor.yaml` afterwards; it holds credentials in clear.

5. **Confirm it enrolled** (both must be true), **load your scenarios, run.**

   ```sh
   argus cloud-executor-status --control-plane https://argus-dev.onedroid.ai --instance-id hello
   argus cloud-seed-scenarios  --control-plane https://argus-dev.onedroid.ai --instance-id hello --scenarios examples/http-hello/scenarios
   ```

   Runs are requested through the control plane's author tools (for example from an agent connected to its MCP
   endpoint, `https://argus-dev.onedroid.ai/mcp`). Use `argus doctor` when a command is refused: it is read-only
   and tells you what the machine would trip over.

The commands `argus up`, `argus init` and `argus update` are in this build, but the onboarding kit they drive
(`onboarding/onboard.sh` and the compose and k3d scripts) is not shipped here: `argus init` stops with "no
onboarding bundle", and `argus up` with "could not find onboarding/onboard.sh". Use the steps above.

## Verify a certificate

```sh
argus certificate get    --control-plane https://argus-dev.onedroid.ai --instance-id <id> --run-id <run> --out cert.json   # needs your author session
argus certificate verify cert.json --rpc <ethereum json-rpc url> [--chains chains.json] [--btc-headers <url-or-file>]
```

`verify` never contacts the control plane. `--rpc` is your own chain client; `--chains` is your own copy of the
chains' allowed signers (without it, each anchor is only checked against the signer it declares); `--btc-headers`
is a block-explorer URL or a local file for OpenTimestamps receipts. Exit code 0 means no check mismatched and at
least one of the run's verdict anchors read back and matched. An anchor that is unreachable or still pending (an
OpenTimestamps receipt not yet in a block) does not fail it; each anchor's status is printed. See
`docs/CERTIFICATE.md` for the format and the exact checks.

## Check a reveal, and replay a revealed scenario

After a certified run, its author can reveal the sealed scenarios. The reveal document holds every sealed scenario,
the Merkle proofs that tie each one to the set anchored before the run, and the run's anchors. Whoever the author
gives it to can check it, and re-run any scenario in it, without a OneDroid account and without contacting OneDroid:

```sh
argus anchor verify reveal.json --rpc <ethereum json-rpc url> --btc-headers <url-or-file>
argus replay <scenario id> --reveal reveal.json --against sha256:<digest> --config <your argus-config.yaml>
```

`anchor verify` recomputes the sealed set's Merkle root and every scenario's inclusion proof, recomputes the evidence
bundle hash, and reads each anchor back through your own chain client (or checks its OpenTimestamps receipt against
your own Bitcoin header source). Each check prints VERIFIED, MISMATCH, UNREACHABLE or PENDING; exit 0 needs no
MISMATCH and at least one verified verdict anchor. `replay` runs one revealed scenario against a deployment you run
yourself and compares your outcome with the verdict the reveal declares. `--against` must be the artefact digest the
reveal certified (it is refused otherwise), so you replay the same build. Anchors on a private chain (for example
`argus-dev-poa`) read as UNREACHABLE from outside: the public check is Base Sepolia and Bitcoin.

## What is here

| Path | What |
|---|---|
| `cmd/argus` | the CLI: scenario and config validation, cloud sign-in and enrollment, `render-k8s`, `doctor`, `certificate`, the runner (`serve --mode runner`) |
| `internal/runner`, `internal/argus`, `internal/toolcore` | the executor: federation poll loop, scenario execution, results |
| `internal/certificate`, `internal/chainread` | the certificate format and the read-only chain reader behind `certificate verify` |
| `internal/reveal` | the reading side of a reveal document: `anchor verify` and `replay` (it assembles nothing and writes no chain) |
| `templates/` | the JMeter templates the runner drives |
| `jmeter-plugins/amqp` | the AMQP JMeter sampler (Java) |
| `testkit/ui` | the Playwright harness for Web UI scenarios |
| `schemas/`, `specs/`, `onboarding/HOW-TO-ARGUS-CONFIG.md`, `docs/` | the scenario and config format |
| `examples/http-hello` | a minimal kit |

## What is deliberately not here

The control plane and its store, the web UI, anything that writes to a chain or holds a chain key, reveal document
assembly (checking a reveal and replaying it ARE here, see above), deployment overlays and secrets for any hosted
environment, and the onboarding kit. The executor does sign its own requests to the control plane, with its own
instance key.
`argus serve --mode control` is refused in this build.

## Build and test

See `BUILDING.md`. In short: `go build ./... && go test ./...`. No token, no private module and no database are
needed.
