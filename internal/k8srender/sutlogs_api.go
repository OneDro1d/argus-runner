package k8srender

import (
	"fmt"
	"strings"
)

// sutlogs_api.go — render-k8s --collect-sut-logs --sut-logs-via api.
//
// `node` (the default, and the only way before this) is the promtail DaemonSet: three hostPath volumes
// and, on a local tier, a ClusterRole. A cluster whose Pod Security level is `baseline` refuses it, and
// an account that is namespace-admin only cannot create the ClusterRole. `api` renders a one-replica
// Deployment of the SAME image running `argus collect-logs`, which reads the SUT namespace's pod logs
// over the Kubernetes API. Nothing here is a hostPath, a DaemonSet, or cluster-scoped.

// The two --sut-logs-via values. "" is node.
const (
	SUTLogsViaNode = "node"
	SUTLogsViaAPI  = "api"
)

const (
	// SUTLogsName names the collector's Deployment (and its pod label); SUTLogsServiceAccount its identity.
	SUTLogsName           = "sut-logs"
	SUTLogsServiceAccount = "argus-sut-logs"
	// sutLogsReadRoleName is the Role + RoleBinding the SUT's owner applies in the SUT namespace.
	sutLogsReadRoleName = "argus-sut-logs-read"
)

// SUTLogsCollectorRendered is THE answer to "does render-k8s emit a pod-log collector (promtail or the
// sut-logs Deployment) for this instance": --collect-sut-logs is on AND the obs mode renders one. bundled
// and shared always do; export does unless the target is BetterStack; adopt and none never do (the
// operator's own pipeline ships the logs, or nothing does). The renderer gates the sut-logs Deployment and
// the second Role on it (sutLogsViaAPI) and cmd/argus words its notice from it: one list of modes.
func (in Instance) SUTLogsCollectorRendered() bool {
	if !in.CollectSUTLogs {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(in.ObsMode)) {
	case "", obsModeBundled, obsModeShared:
		return true
	case obsModeExport:
		return !in.ObsUseBetterStack
	}
	return false
}

// sutLogsViaAPI is true when the sut-logs Deployment (and the owner's second Role) are rendered.
func (in Instance) sutLogsViaAPI() bool {
	return in.SUTLogsCollectorRendered() && strings.EqualFold(strings.TrimSpace(in.SUTLogsVia), SUTLogsViaAPI)
}

// ValidSUTLogsVia reports whether v is an accepted --sut-logs-via value ("" counts as node).
func ValidSUTLogsVia(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", SUTLogsViaNode, SUTLogsViaAPI:
		return true
	}
	return false
}

// sutLogsLokiArgs is the collector's `args:` tail after `--config /config/argus-config.yaml`: the same
// Loki the executor of this obs mode is given (obsExecArgsBlock): bundled the instance Loki, shared the
// environment's Loki as tenant <instance id>, export the operator's own Loki (its push endpoint and
// credential come from the config and the credential env).
func sutLogsLokiArgs(in Instance) string {
	switch {
	case strings.EqualFold(in.ObsMode, obsModeShared):
		return fmt.Sprintf(", --obs, shared, --loki, %q, --loki-tenant, %q", in.ObsSharedURL, in.ID)
	case strings.EqualFold(in.ObsMode, obsModeExport):
		return fmt.Sprintf(", --loki, %q", in.ObsLokiURL)
	default:
		return ", --loki, http://loki:3100"
	}
}

// renderSUTLogsCollector renders the collector's ServiceAccount and Deployment as YAML documents, each
// preceded by `---`. The credential, for an export run, is a secretKeyRef to the same Secret the
// executor reads; the value is never rendered.
func renderSUTLogsCollector(in Instance, ns string) string {
	return fmt.Sprintf(`---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    app: %[3]s
    argus.onedroid.ai/instance: %[4]s
automountServiceAccountToken: true
---
# --sut-logs-via api: reads the pod logs of namespace %[5]s over the Kubernetes API and ships them to Loki
# with promtail's labels. no host mounts, no host networking, no per-node pod, nothing cluster-scoped. What it may
# read is whatever the SUT namespace's owner grants %[1]s there (the argus-sut-logs-read Role in
# sut-access-role.yaml, rendered with --emit-sut-access-role).
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[3]s
  namespace: %[2]s
  labels:
    app: %[3]s
    argus.onedroid.ai/instance: %[4]s
spec:
  replicas: 1
%[6]s  selector: {matchLabels: {app: %[3]s}}
  template:
    metadata:
      labels:
        app: %[3]s
        argus.onedroid.ai/instance: %[4]s
    spec:
      serviceAccountName: %[1]s
      automountServiceAccountToken: true
%[7]s      containers:
        - name: %[3]s
          image: %[8]s
          imagePullPolicy: %[9]s
          # the image ENTRYPOINT is ["argus"], so args: (a k8s command: would replace it).
          args: [collect-logs, --config, /config/argus-config.yaml%[10]s]
          env:
            - name: ARGUS_INSTANCE_ID
              value: %[4]q
            - name: ARGUS_SUT_NAMESPACE
              value: %[5]q
            - name: ARGUS_CLUSTER
              value: %[11]q
            - name: POD_NAME
              valueFrom:
                fieldRef: {fieldPath: metadata.name}
            - name: POD_NAMESPACE
              valueFrom:
                fieldRef: {fieldPath: metadata.namespace}
%[12]s          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
          resources:                                    # mandatory under the instance ResourceQuota
            requests: {cpu: "25m", memory: "64Mi"}
            limits:   {cpu: "200m", memory: "256Mi"}
          volumeMounts:
            - {name: config, mountPath: /config, readOnly: true}
      volumes:
        - name: config
          %[13]s
`,
		SUTLogsServiceAccount, ns, SUTLogsName, in.ID, in.SUTNamespace,
		obsRecreateStrategy,
		imagePullSecretBlock(in.ImagePullSecret),
		in.Image, in.imagePullPolicy(),
		sutLogsLokiArgs(in),
		in.Cluster,
		obsCredentialEnvBlock("            ", in.ObsCredentialVarName),
		messageSchemaConfigVolumeSource(in.MessageSchemaFiles),
	)
}

// sutLogsReadRoleManifest is the SECOND grant in sut-access-role.yaml, rendered only with --sut-logs-via
// api: pods get+list and pods/log get, in the SUT namespace, to the collector's ServiceAccount in the
// instance namespace. A separate Role beside argus-load-environment-read, which stays unchanged and
// still does not grant pods/log.
func sutLogsReadRoleManifest(in Instance) string {
	return fmt.Sprintf(`---
# SECOND GRANT (--sut-logs-via api). Apply this too, in YOUR OWN namespace (%[1]s), only if you want
# Argus to collect your pods' logs. What it lets Argus read: EVERY container's log lines of EVERY pod in
# namespace %[1]s (pods/log), and the pod list that finds them (pods get, list). Nothing else: no
# secrets, no configmaps, no exec, no other namespace. The collector only reads; it cannot change a pod.
# Delete the Role and RoleBinding below to withdraw the access; collection then stops (it logs the 403
# once a minute and keeps trying).
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: %[2]s, namespace: %[1]s}
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [get, list]
  - apiGroups: [""]
    resources: [pods/log]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: %[2]s, namespace: %[1]s}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: %[2]s}
subjects:
  - {kind: ServiceAccount, name: %[3]s, namespace: %[4]s}
`, in.SUTNamespace, sutLogsReadRoleName, SUTLogsServiceAccount, in.Namespace())
}
