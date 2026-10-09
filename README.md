# OneDroid Argus runner

This repository is the open part of OneDroid Argus: the **runner** that executes checks against a system you
own, and the **`argus` command line** a tester uses, including `argus certificate verify`, which lets anyone
who holds a OneDroid Argus certificate check it against the chain without trusting OneDroid.

OneDroid Argus is an end-to-end test and certification service. You describe checks for your system as
scenario files. The runner executes them next to your system (HTTP, database state, message flow, load, web UI)
and reports to a control plane. The control plane keeps the registry, signs, and anchors results on a public
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

You need an account on the hosted control plane at `https://argus-dev.onedroid.ai`, and a Kubernetes cluster
that can reach both your system and the control plane (the runner connects out only; it needs no inbound route).

1. **Get the runner.**
   Pull the published image (`docker pull ghcr.io/onedro1d/argus-runner:main`; each build is also tagged
   `sha-<commit>`, and step 4 pins it by digest), or build it (`docker build -t argus-runner .`, see
   `BUILDING.md`). To get only the CLI: `go build -o argus ./cmd/argus`.

2. **Sign in to the hosted control plane.**

   ```sh
   argus cloud-login --control-plane https://argus-dev.onedroid.ai --scope author
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

Not in this repository: the onboarding kit (`argus up`, `argus init`, `argus update` and the compose/k3d scripts
they drive) is not shipped here, so those commands will not work from this build. Use the steps above.

## Verify a certificate

```sh
argus certificate get    --control-plane https://argus-dev.onedroid.ai --instance-id <id> --run-id <run> --out cert.json   # needs your author session
argus certificate verify cert.json --rpc <ethereum json-rpc url> [--chains chains.json] [--btc-headers <url-or-file>]
```

`verify` never contacts the control plane. `--rpc` is your own chain client; `--chains` is your own copy of the
chains' allowed signers (without it, each anchor is only checked against the signer it declares); `--btc-headers`
is a block-explorer URL or a local file for OpenTimestamps receipts. Exit code 0 means every anchor read back and
matched; see `docs/CERTIFICATE.md` for the format and the exact checks.

## What is here

| Path | What |
|---|---|
| `cmd/argus` | the CLI: scenario and config validation, cloud sign-in and enrollment, `render-k8s`, `doctor`, `certificate`, the runner (`serve --mode runner`) |
| `internal/runner`, `internal/argus`, `internal/toolcore` | the executor: federation poll loop, scenario execution, results |
| `internal/certificate`, `internal/chainread` | the certificate format and the read-only chain reader behind `certificate verify` |
| `templates/` | the JMeter templates the runner drives |
| `jmeter-plugins/amqp` | the AMQP JMeter sampler (Java) |
| `testkit/ui` | the Playwright harness for Web UI scenarios |
| `schemas/`, `specs/`, `onboarding/HOW-TO-ARGUS-CONFIG.md`, `docs/` | the scenario and config format |
| `examples/http-hello` | a minimal kit |

## What is deliberately not here

The control plane and its store, the web UI, anything that signs, writes to a chain or holds a key, reveal
document assembly and replay, deployment overlays and secrets for any hosted environment, and the onboarding kit.
`argus serve --mode control` is refused in this build.

## Build and test

See `BUILDING.md`. In short: `go build ./... && go test ./...`. No token, no private module and no database are
needed.
