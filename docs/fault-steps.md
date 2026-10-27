# Fault steps - break a connection or a deployment during a run

A chain scenario can MAKE the system under test fail while it runs: stop a Deployment, scale it, delay or cut or
truncate a connection. The step reference is in `specs/17-chain-scenarios.md` ("fault step"); the config key is
`fault_allowed_targets` in `argus-config.yaml`. This page is what an OPERATOR sets up: the allowlist, the
Toxiproxy, the Kubernetes Role, and a worked scenario.

Nothing here runs unless you list it. **No `fault_allowed_targets` block = no fault step can run.**

## 1. The allowlist (`argus-config.yaml`, top level, beside `targets:`)

```yaml
fault_allowed_targets:
  deployments:
    trades: {namespace: calm-demo}
    trades-mcp-server: {namespace: calm-demo}
  links:
    mcp-to-trades: {toxiproxy_api: "http://toxiproxy.calm-demo:8474", proxy: trades}
```

- A deployment entry's name is the Deployment's own name; a scenario names it, never a namespace.
- A link entry names a Toxiproxy API address and a proxy in it; a scenario names the link.
- A namespace or address with `prod`, `production`, `prd` or `shared` as a segment is refused when the config
  loads (`argus validate-config`), naming the entry and never the address.
- The key is top level for the reason `load_allowed_targets` is: an executor built before it existed ignores a
  top-level key, but refuses an unknown key under `targets:` and would not start after a rollback.

## 2. A Toxiproxy in the path of the connection

Fault steps on a **link** work through a [Toxiproxy](https://github.com/Shopify/toxiproxy) you place between the
caller and the callee. Argus does not install it and does not rewire your SUT: point the caller at the proxy's
listen address and the proxy at the real upstream. Example for the CALM trades demo (MCP server -> trades API):

```yaml
apiVersion: v1
kind: ConfigMap
metadata: {name: toxiproxy-config, namespace: calm-demo}
data:
  toxiproxy.json: |
    [{"name": "trades", "listen": "0.0.0.0:8081", "upstream": "trades:8080", "enabled": true}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: toxiproxy, namespace: calm-demo}
spec:
  replicas: 1
  selector: {matchLabels: {app: toxiproxy}}
  template:
    metadata: {labels: {app: toxiproxy}}
    spec:
      containers:
        - name: toxiproxy
          image: ghcr.io/shopify/toxiproxy:2.9.0   # pin a digest in your cluster
          args: ["-host=0.0.0.0", "-config=/etc/toxiproxy/toxiproxy.json"]
          ports: [{containerPort: 8474}, {containerPort: 8081}]
          volumeMounts: [{name: cfg, mountPath: /etc/toxiproxy}]
      volumes: [{name: cfg, configMap: {name: toxiproxy-config}}]
---
apiVersion: v1
kind: Service
metadata: {name: toxiproxy, namespace: calm-demo}
spec:
  selector: {app: toxiproxy}
  ports: [{name: api, port: 8474}, {name: trades, port: 8081}]
```

Then the MCP server's `TRADES_API_URL` (or equivalent) points at `http://toxiproxy:8081`. The Toxiproxy admin API
(8474) has no authentication: keep it cluster-internal and reachable only from the executor (a NetworkPolicy).
Every toxic Argus adds is named `argus-<correlation id>-<n>`, and restore deletes exactly those; your own toxics
are never touched. `drop` disables the proxy and re-enables it only if it was enabled before.

A run that used a link fault says so: `toxiproxy: true` on the fault record and in the certificate - the
connection under test was the one through the proxy.

## 3. The Kubernetes Role the executor needs (deployment faults)

Granted in the SUT's namespace to the executor's ServiceAccount, limited by `resourceNames` to the allowlisted
Deployments - nothing broader:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: argus-fault, namespace: calm-demo}
rules:
  - apiGroups: ["apps"]
    resources: ["deployments/scale"]
    resourceNames: ["trades", "trades-mcp-server"]
    verbs: ["get", "patch"]
  - apiGroups: ["apps"]
    resources: ["deployments"]
    resourceNames: ["trades", "trades-mcp-server"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: argus-fault, namespace: calm-demo}
subjects: [{kind: ServiceAccount, name: <the executor's ServiceAccount>, namespace: <the executor's namespace>}]
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: argus-fault}
```

A step the Role does not allow fails by name ("forbidden - the executor's ServiceAccount may not do this"); the
restore needs the same verbs, so a missing grant also shows as a failed restore.

## 4. A worked scenario (the six CALM checks)

```json
{"steps":[
  {"type":"fault","name":"stop-api","op":"stop","deployment":"trades"},
  {"type":"mcp","name":"search-down","tool":"search_trades","args":{"query":"x"}},
  {"type":"fault","name":"api-back","op":"start","deployment":"trades"},
  {"type":"fault","name":"api-ready","op":"wait_ready","deployment":"trades","timeout":"120s"},
  {"type":"fault","name":"slow-link","op":"delay","link":"mcp-to-trades","latency_ms":5000},
  {"type":"mcp","name":"search-slow","tool":"search_trades","args":{"query":"x"}},
  {"type":"fault","name":"heal","op":"restore"}
]}
```

`wait_ready` after `start` waits for the pods; for "the HTTP health URL answers again" use an `http` step with
`poll` (do not use `wait_ready` for that). The claims go on `search-down` and `search-slow` in `## EXPECT`; the fault steps take none.
`replicas: 2` is a `scale` step; "a restart" is `stop` then `start`; "a truncated answer" is `truncate`.

## 5. What happens when it goes wrong

| Event | What Argus does |
|---|---|
| a step names something not listed | the whole chain is refused before its first step (`- preflight`), naming the entry |
| the undo cannot be written to the results volume | the fault is not applied |
| the chain passes, fails, errors, times out, or a step panics | every active fault is undone, newest first, before the row is returned |
| the executor dies mid-run | the next executor start undoes every pending entry before its first poll, logs each, and appends it to `fault-journal/replay.log` |
| two runs would fault the same deployment or proxy | the second is refused (a lock file on the results volume, refreshed every 10 s while its run lives) |
| a restore fails | `restore_outcome: "failed"` on the record, `FAULT NOT RESTORED` on the row's residue and in the log, the undo stays journaled and is retried at the next start. **The verdict is unchanged.** |

Known limits: the Toxiproxy admin API and the Kubernetes API are the executor's to reach; a fault from the
`run-direct` CLI is undone at the end of its chain but a crash there is only replayed when the executor next
starts; the control-plane red-alert sweep (`run_alerts`) judges verdicts, so a failed restore reaches an alert
only through the executor log and the record today.
