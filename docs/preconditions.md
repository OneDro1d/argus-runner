# Preconditions: a check that depends on a mechanism the environment may not have

Decision (issue #671). Spec: `specs/17-chain-scenarios.md` (section "Preconditions").

## The problem

A check whose enforcement mechanism is absent cannot fail on the system's merits; it can only report the instrument.
On a cluster that does not enforce NetworkPolicy (AKS with `networkPolicy: none`), "the denied client must not connect"
fails because the cluster does nothing with the policy, not because the system under test is wrong. Filing that under
`failed` is a false statement about the system. Filing it under `errored` is a false statement about the instrument.
A precondition is how a check says what it needs, and how Argus says the need was not met.

## Declaring one

In the check's `## Metadata`:

```
- **Preconditions**: np-in=networkpolicy.ingress_enforced
- **Depends on**: np-in
```

- `Preconditions` is a comma-separated list of `id=kind`. The id matches `^[a-z0-9][a-z0-9_-]{0,62}$`. The kind is one of
  the BUILT-IN kinds below. An unknown kind is a validation error (the nearest built-in is offered). There is no way to
  supply probe code.
- `Depends on` is a comma-separated list of ids. An id that this file's `Preconditions` does not declare is a validation
  error.
- A set declares its preconditions as the union over its files. Two files that declare one id with different kinds are a
  conflict the executor refuses: those checks end `errored`.
- Both lines are in the check's bytes, so the set hash and the holdout (Merkle) root change when either changes, and a set
  that declares none hashes exactly as before.

| kind | the mechanism | positive control | negative control |
| --- | --- | --- | --- |
| `networkpolicy.ingress_enforced` | the cluster enforces ingress NetworkPolicy | a client connects to a listener the probe's allow policy admits | after the allow is removed and a deny policy selects the listener, the client is refused |
| `networkpolicy.egress_enforced` | the cluster enforces egress NetworkPolicy | a client connects to the listener with no egress policy | after a deny policy selects the client's egress, the client is refused |

## What the probe does

It runs on the executor side, in-cluster, with the executor's own ServiceAccount, in the executor's OWN instance
namespace (read from the pod, never from a scenario or a config value) and never in the system-under-test namespace. It
creates a listener pod, a one-shot client pod and the allow/deny NetworkPolicies, reads the client's exit code, and deletes
everything it created: by name, then by label sweep, on a fresh context so a cancelled run still cleans up. A leftover from
an earlier crashed probe is swept at the start. Anything it could not remove is reported as `residue`.

Each client retries (`attempts` of `connect_timeout` each), so a listener that is slow to bind is never read as "refused".
Between a policy change and the next client the probe waits `settle`, for the policy engine to program the change.

**An ambient policy guard.** NetworkPolicies are additive allow-lists. If another policy in the namespace already admits the
probe's traffic, a deny cannot close it, and a connection that still works would say nothing about enforcement. Reading
that as `absent` would excuse checks on a cluster that DOES enforce. Before the negative control the probe lists the
namespace's policies; if one admits the client it ends `broken`, naming the policy. `argus render-k8s` renders
`allow-same-namespace` so that it excludes the probe's listener (label `argus.onedroid.ai/probe-isolated`) for this reason;
an instance rendered before that change reads `broken` for ingress until it is re-rendered, never `absent`.

## The three results

| result | meaning | dependents |
| --- | --- | --- |
| `holds` | positive control alive, negative control refused | run normally |
| `absent` | positive control alive, negative control NOT refused: the mechanism does nothing here | NOT executed: `not_applicable`, carrying the precondition id and both observations |
| `broken` | the positive control failed, the probe could not be set up, or the negative control could not be trusted | NOT executed: `errored`. The instrument is broken. Never `not_applicable` |

`broken` never excuses a check: a probe that could be made to say "not applicable" by breaking it would be a way to skip any
check. Preconditions run before any dependent check, once each, and their results are in the in-env report
(`preconditions`).

## Outcome, tallies and the run verdict

`not_applicable` is its own outcome in the report, the federation push, the control-plane store and every view. It is
never folded into `errored` and never counted as a pass or a fail.

`passed + failed + errored + degraded + not_applicable == total`.

| the run | verdict |
| --- | --- |
| any failure or error | `failed`, as before |
| passed checks and `not_applicable` checks | `passed`, with the `not_applicable` count shown beside it |
| EVERY check `not_applicable` | `not_applicable` (never `passed`: nothing was verified) |

The lifecycle status of an all-N/A run stays `completed` (it did finish); the verdict is the word that differs. The
notifier reads such a run as `no_result`, not green.

## Evidence and the certificate

Each precondition's result (id, kind, verdict, both observations) is part of the artifact-measurement record the
`evidence_bundle_hash` commits to, so editing one changes the hash the chain holds. Executor-attested, like fault steps:
the executor ran the probe, and the chain proves only that the report was not edited afterwards.

A run that carries preconditions or `not_applicable` scenarios is issued **certificate v5**; every other run keeps its v1-v4
certificate byte for byte. v5 adds a `not_applicable` bucket to the (anchored) tallies, the `not_applicable` verdict, and the
`preconditions` list. `argus certificate verify` checks that the buckets add up, that an all-N/A run is not a pass, that an
N/A count is explained by a recorded `absent` precondition, and that the list reproduces the anchored hash; it prints under
NOT ANCHORED that the results are what the executor reported. An older verifier does not know v5 and refuses it by name: it
never reads a v5 certificate with the N/A count dropped.

## Reports

The author's full report and the web run view say `not applicable: <precondition> absent` with both controls'
observations, distinct from `failed` and `errored`. A builder reading a certification run sees only the verdict and tallies.

## Operating it

- Re-render the executor's manifests (`argus render-k8s`) so its Role carries pods create/list and networkpolicies
  create/get/list/delete in its own namespace, and `allow-same-namespace` carries the probe-listener exclusion.
- The image defaults to `busybox:1.36.1` pinned by digest (`docker.io/library/busybox:1.36.1@sha256:73aaf090…`, the full value is `DefaultImage` in `internal/precondition/probe.go`): the probe's verdict reaches the certificate, so it never runs from a moving tag. Set `precondition_probe.image` if the executor namespace cannot pull it (for example a mirror in your own registry); pin your own by digest too.
- The instance ResourceQuota counts the probe's two pods (tiny requests and limits).
- `forbidden` in a `broken` reason means the Role was not re-rendered.
