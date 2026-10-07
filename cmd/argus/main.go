// Command argus is the Argus MCP CLI. The CLI and the MCP server
// (internal/mcpserver) are two entry points over the SAME core (internal/toolcore);
// every command emits structured JSON on stdout with a stable exit code.
//
// M2.5 (D2): the dark-factory boundary is now AUTHORIZATION, not behaviour. A token
// is REQUIRED (--token or ARGUS_TOKEN; the matched token IS the hat). The M2
// self-asserted --role is no longer honoured as an auth mechanism (VR-J9). The two
// configured tokens live in ARGUS_RUNNER_TOKEN (product/runner scope) and
// ARGUS_EXECUTOR_SECRET, formerly ARGUS_AUTHOR_TOKEN (test/runner+author scope).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/OneDro1d/argus-runner/internal/argus"
	"github.com/OneDro1d/argus-runner/internal/auth"
	"github.com/OneDro1d/argus-runner/internal/buildinfo"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/k8srender"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/mcpserver"
	"github.com/OneDro1d/argus-runner/internal/obsconfig"
	"github.com/OneDro1d/argus-runner/internal/onboard"
	"github.com/OneDro1d/argus-runner/internal/packagecheck"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/router"
	"github.com/OneDro1d/argus-runner/internal/runner"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

const (
	exitOK     = 0
	exitErr    = 1
	exitUsage  = 2
	exitDenied = 3 // auth: missing/unknown token, or wrong scope
	exitFailed = 4 // validation/run produced failures
)

const keygenUsage = "usage: argus keygen\n" +
	"  Mints a fresh Ed25519 machine key and prints it, base64, on stdout. Takes no arguments.\n" +
	"  The output IS a secret: pipe it straight into where it is stored, never to a terminal or a log."

func main() {
	// The addresses built into this binary are checked once, before any command runs. A bad one
	// stops the binary here, naming the setting, rather than printing a broken address later.
	if err := buildinfo.ValidateHosts(); err != nil {
		fmt.Fprintln(os.Stderr, "argus: "+err.Error())
		os.Exit(exitErr)
	}
	os.Exit(dispatch(os.Args[1:]))
}

func emit(v any) { b, _ := json.MarshalIndent(v, "", "  "); fmt.Println(string(b)) }

// envOr returns the env var value or a default.
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func emitErr(code int, format string, a ...any) int {
	out := map[string]any{"error": fmt.Sprintf(format, a...)}
	// Every cloud-* command reports failure through here, so a control-plane refusal of the credential
	// is recognised here once, from the error's TYPE — see doctor_pointer.go.
	if refusedCredential(a) {
		out["diagnose"] = doctorPointerCP()
	}
	emit(out)
	return code
}

// commonFlags holds the flags most commands share.
// defaultTokenOut is the SHARED default of --token-out, and it belongs to cloud-login.
//
// It is named rather than repeated because cloud-mint-token has to recognise it in order to IGNORE
// it (VR3-09): that subcommand no longer writes a file by default, and a literal comparison against
// a copy-pasted string is exactly the kind of thing that survives one refactor and not two. If this
// value ever changes, the "treat the shared default as unset" test fails rather than the author
// token silently landing on top of the session token.
const defaultTokenOut = "cp-session.token"

type commonFlags struct {
	instance, roleStr, token, addr, configPath, scenariosDir, resultsRoot, composeFile, grafana, loki, pushgateway string
	scenarioID, corrID, window, file, path, layer, tag, from, target, runID, commit                                string
	controlPlane, cloud, scope, tokenOut, name, sut                                                                string // M3 cloud onboarding (D-ONBOARD.7)
	jmeterMode, templatesDir, mode                                                                                 string
	// render-k8s (U1, Stage II): the per-instance k8s manifest inputs.
	sutNamespace, image, outDir, tier, externalAlias, secretsEnvFile string
	replicas                                                         int
	// T2.2: the shared results volume's storage class + access mode. Empty (the default for both)
	// reproduces normalize()'s tier defaults byte-for-byte — see cmdRenderK8s. Giving --storage-class
	// with no --results-access-mode defaults the mode to ReadWriteMany: naming a class is asking for
	// shared storage, and a class that cannot do RWX leaves the PVC visibly Pending rather than
	// silently pinning replicas to one node.
	storageClass, resultsAccessMode string
	// obsStorageClass: the StorageClass of the observability volumes (Loki's data,
	// the Pushgateway's file; also the shared Loki's via render-obs-shared). Empty takes the tier
	// default (managed-csi on aks, else none: the cluster's default StorageClass). Always ReadWriteOnce, so unlike storageClass it never
	// implies an access mode.
	obsStorageClass string
	// emitSUTAccessRole (P3 #23): render-k8s ALSO writes the OPTIONAL Role/RoleBinding the SUT owner
	// applies in THEIR OWN namespace to grant the executor read access for load-run environment
	// capture. Opt-in and separate from execYAML/obsYAML — see k8srender.SUTAccessRoleManifest.
	emitSUTAccessRole bool
	// obsMode (T3.1, E3): bundled | adopt | export — render-k8s/onboard.sh's --obs. See
	// cmdRenderK8s for the refusal shape (export/unknown -> error; adopt with no configured
	// observability.loki.url -> error, surfaced via k8srender.Instance.validate()).
	obsMode string
	// podmonitor: auto | on | off — render-k8s's control over the managed-tier PodMonitor
	// (k8srender.Instance.PromOperatorLabel). See cmdRenderK8s for the auto-detection logic.
	podmonitor string
	// collectSUTLogs (--collect-sut-logs), default false: whether render-k8s's promtail reads the
	// SUT namespace's own pod logs at all. See k8srender.Instance.CollectSUTLogs.
	collectSUTLogs bool
	workspaceID    string // B2: cloud-switch-workspace target
	summary        bool   // B2: cloud-list-workspaces --summary (flat, shell-parseable lines)
	root           string // AC-9: package-check tree root
	jsonOut        bool   // AC-9: package-check --json
	cloudOnly      bool   // R9 (CP-M3-121): accepted no-op — cloud-teardown is inherently cloud-only
	// VR-F6: PLAN B for a direct run, and only that. When the control plane cannot be reached after
	// VR-F2b's full retry policy, the run REFUSES by default and puts two choices to the user — wait,
	// or run the local set knowing it was never verified. This flag is the second choice, made
	// explicitly, and it does nothing at all while the catalog is reachable.
	useLocalScenarios bool
	// T7.2: render-k8s' kube context, recorded on the instance so `argus update` can target it.
	kubeContext string
	// T3.3 (E3 shared ingest): the environment's shared Loki base URL (render-k8s --obs shared), and
	// the tenant every Loki request carries as X-Scope-OrgID (the executor; else ARGUS_LOKI_TENANT).
	obsSharedURL, lokiTenant string
	// mcp-call (D3): test a SUT's OWN MCP tools.
	serverURL, transport, tool, argsRaw, requestID, mcpToken, expectPlane, expectCodeStr string
	// VR3-08/VR3-10: where THIS MACHINE's router keeps its state. cloud-mint-token reads the
	// account's existing author token from there instead of from a per-kit file, and reads it
	// IN-PROCESS — the alternative shape, where the shell reads it and passes the value, puts a live
	// credential into a process listing.
	routerState string
	// AC-D24/AC-D25: cloud-enroll's output directory (per-instance *.enrollment files) and the
	// enrollment id `cloud-enroll --revoke` retires instead of minting.
	tokenDir, revokeID string
	// T5.1+T5.3: propose-from-repo's source tree — read READ-ONLY, never written to (repo.go).
	repo string
	// tokenOutSet is TRUE only when --token-out was named on this invocation's argv (set via
	// fs.Visit in dispatch, not by binding) — REQUIRED #1's "if --token-out is passed explicitly".
	// cf.tokenOut itself cannot answer this: it always holds a value (the shared default when
	// unset), so a string-equality check could not tell "the user typed the default" from "nothing
	// was typed" the way fs.Visit can.
	tokenOutSet bool
}

func (c *commonFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.instance, "instance-id", "local", "instance id")
	fs.StringVar(&c.roleStr, "role", "", "DEPRECATED: --role is no longer an auth mechanism (use a token)")
	fs.StringVar(&c.token, "token", "", "bearer token (else ARGUS_TOKEN); the matched token is the hat")
	fs.StringVar(&c.addr, "addr", ":8765", "serve: listen address for the MCP HTTP+SSE server")
	fs.StringVar(&c.mode, "mode", "", "serve: M3 deploy profile — control | runner (else ARGUS_MODE); empty = the M2.5 MCP server")
	// VR10-R3 (V28-007): NO default. The demo config used to be the fallback, so a forgotten --config
	// ran a different SUT and printed run_begin: ok. Every subcommand that reads a config refuses an
	// empty value at flag parsing — see configRequired.
	fs.StringVar(&c.configPath, "config", "", "the argus-config.yaml of the SUT under test (required by every config-reading subcommand; there is no default)")
	fs.StringVar(&c.scenariosDir, "scenarios", defaultScenariosDir, "scenarios dir")
	fs.BoolVar(&c.useLocalScenarios, "use-local-scenarios", false,
		"run: ONLY after the control plane proves unreachable — run the local scenarios anyway, accepting they were not verified against the catalog")
	fs.StringVar(&c.resultsRoot, "results", "results", "results root dir")
	fs.StringVar(&c.composeFile, "compose-file", "deploy/compose/docker-compose.yaml", "compose file for the jmeter exec")
	// V31-001 §G-2: the default is LOCAL. `docker` means shelling out to `docker compose exec`,
	// which has no socket to reach through on k3d or managed, and the executor image carries JMeter
	// itself — so a forgotten flag used to produce a run that could not start rather than one that
	// ran differently. The `docker` mode and the compose `jmeter` service both stay; only the way an
	// unset flag falls has changed. ⚠ Every shipped launcher still passes the flag EXPLICITLY, and
	// jmeter_default_test.go enumerates them so dropping one fails loudly rather than silently.
	fs.StringVar(&c.jmeterMode, "jmeter", "local", "jmeter execution: docker (compose exec) | local (subprocess, JMeter-as-a-service in the serve image)")
	fs.StringVar(&c.templatesDir, "templates", "/templates", "jmeter=local: dir holding the <layer>.jmx templates")
	fs.StringVar(&c.grafana, "grafana", "http://localhost:3000", "grafana base url")
	fs.StringVar(&c.loki, "loki", "http://localhost:3100", "loki base url")
	fs.StringVar(&c.pushgateway, "pushgateway", "http://localhost:9091", "prometheus pushgateway url")
	fs.StringVar(&c.scenarioID, "scenario", "", "scenario id (single-id filter / target)")
	fs.StringVar(&c.layer, "layer", "", "run only scenarios in this primary layer (canonical name, e.g. \"Rate Limiting\")")
	fs.StringVar(&c.tag, "tag", "", "run only scenarios carrying this tag")
	fs.StringVar(&c.corrID, "correlation-id", "", "correlation id")
	fs.StringVar(&c.runID, "run-id", "", "run id (run: set/override; get-report: scope to this run — DF-06/07)")
	fs.StringVar(&c.commit, "commit", "", "mark-deployment: optional commit id recorded with the marker")
	fs.StringVar(&c.controlPlane, "control-plane", "", "M3 cloud onboarding: the control-plane URL (else ARGUS_CP_URL)")
	fs.StringVar(&c.cloud, "cloud", "direct", "M3 cloud onboarding: direct | hub")
	// msgbus tester follow-up item 2 (2026-09-28): NO default. A defaulted --scope was the same
	// foot-gun P0-2 closed server-side for an unscoped OAuth sign-in (401 on a bad/absent token) —
	// silently minting an "author" session for a caller who meant "runner" (or vice versa). See
	// cmdCloudLogin's refusal for the required flag's own message.
	fs.StringVar(&c.scope, "scope", "", "cloud-login: the OAuth scope to request — REQUIRED, no default (author = operator/tester tools | runner = builder tools)")
	// AC-D24: cloud-login's real, default output is the private session store (~/.config/argus/session.json,
	// or $ARGUS_SESSION_FILE — mode 0600, dir 0700, never inside a git tree), which every cloud-* command
	// then authenticates through, refreshing itself for the OAuth refresh token's 30-day life. --token-out
	// is BACK-COMPAT ONLY: unset, nothing is written here at all; passed explicitly, the bare access token
	// (no refresh material — it goes stale in 15 minutes) is ALSO written, same 0600/git-tree-refusal.
	fs.StringVar(&c.tokenOut, "token-out", defaultTokenOut, "cloud-login/cloud-mint-token/cloud-switch-workspace: BACK-COMPAT ONLY — also write the bare access token here (mode 600, refused inside a git tree); the session store (~/.config/argus/session.json or $ARGUS_SESSION_FILE) is what every cloud-* command actually authenticates and auto-refreshes through. cloud-switch-workspace without this flag writes <session dir>/cp-session.<workspace>.token (absolute path reported), never the working directory")
	fs.StringVar(&c.routerState, "router-state", "", "cloud-mint-token: this machine's router state dir — the ONLY holder of the account's author token (VR3-08)")
	fs.StringVar(&c.name, "name", "", "cloud-mint-token: a label for the minted author token")
	fs.StringVar(&c.sut, "sut", "", "cloud-list-workspaces: also warn if this SUT name already exists in another of your workspaces (UC120)")
	fs.StringVar(&c.workspaceID, "workspace", "", "cloud-switch-workspace: the workspace id to bind the session to (must be yours)")
	fs.BoolVar(&c.summary, "summary", false, "cloud-list-workspaces: print flat shell-parseable lines (ACTIVE/WS/IN) instead of JSON")
	fs.StringVar(&c.sutNamespace, "sut-namespace", "", "render-k8s: the namespace where the SUT's Services live")
	// T7.2: without it the instance registers NO kube context, and `argus update` then treats it as a
	// legacy instance and prints a <KUBE_CONTEXT> placeholder (internal/updatecmd Legacy()). Onboarding
	// passes the same value by env (ARGUS_KUBE_CONTEXT_HOST); the door's hand-run render had no way to.
	fs.StringVar(&c.kubeContext, "kube-context", "", "render-k8s: the kubectl `context` this instance runs in — recorded on the instance so argus update can target it, and used in the printed next command (else ARGUS_KUBE_CONTEXT_HOST)")
	fs.StringVar(&c.image, "image", "", "render-k8s: the onedroid-testing-suite image the executor runs")
	fs.StringVar(&c.outDir, "out", "", "render-k8s: directory to write executor.yaml + obs.yaml into (default: stdout); propose-from-repo: directory accepted scenario drafts are written into (required, created if absent, refused if inside --repo)")
	fs.StringVar(&c.tier, "tier", "k3d", "render-k8s: the tier label reported to the control plane (k3d | aks | eks | gke | managed; managed for a cluster you run yourself, e.g. k3s)")
	fs.StringVar(&c.externalAlias, "external-alias", "", "render-k8s: comma-separated bare=fqdn ExternalName aliases for compose-era hostnames (e.g. webhook-mock=webhook-mock.order-k3d.svc.cluster.local)")
	fs.StringVar(&c.secretsEnvFile, "secrets-env-file", "", "render-k8s: a KEY=VALUE .env whose entries become Secret env for the pod, so the embedded argus-config's ${VAR}s resolve in-cluster")
	fs.IntVar(&c.replicas, "replicas", 1, "render-k8s: executor replicas (1 at genesis so the identity key is created without a race; then the min-3 axiom)")
	// T2.2: the shared results volume's storage class + access mode, otherwise silently defaulted by
	// normalize() from --tier alone (see k8srender.go). Neither flag given renders exactly what
	// today's render-k8s renders, on every tier — that is the compatibility promise this pair of
	// flags exists under.
	fs.StringVar(&c.storageClass, "storage-class", "", "render-k8s: the results volume's StorageClass (else the tier default — see docs/DEPLOY-ARGUS.md §2.1); given with no --results-access-mode, the access mode defaults to ReadWriteMany")
	fs.StringVar(&c.resultsAccessMode, "results-access-mode", "", "render-k8s: the results volume's access mode — ReadWriteMany | ReadWriteOnce (else the tier default, or ReadWriteMany when --storage-class is given with no explicit mode)")
	fs.StringVar(&c.obsStorageClass, "obs-storage-class", "", "render-k8s / render-obs-shared: the StorageClass of the observability volumes — Loki's data and the Pushgateway's file, both ReadWriteOnce (else managed-csi on --tier aks, the cluster's default StorageClass on every other tier; NOT the results volume's RWX class). A bound claim's class cannot be changed in place")
	// P3 #23: opt-in — a SEPARATE manifest (sut-access-role.yaml) for the SUT owner to review and
	// apply in THEIR OWN namespace, never auto-applied by render-k8s itself.
	fs.BoolVar(&c.emitSUTAccessRole, "emit-sut-access-role", false, "render-k8s: also write the OPTIONAL Role/RoleBinding the SUT owner applies in their OWN namespace to grant load-run environment-capture read access (P3 #23) — requires --sut-namespace")
	// T3.1 (E3): bundled (default, today's behaviour) | adopt (deploy no obs, use the operator's own
	// Loki/Pushgateway from argus-config.yaml) | export (recognised, refused — not built, see
	// MVP2-SPRINT.md E3/T3.4). onboard.sh accepts both --obs <mode> and --obs=<mode> and passes
	// this flag through unchanged.
	fs.StringVar(&c.obsMode, "obs", "bundled", "render-k8s: observability mode — bundled | adopt | export | shared | none (T3.1: export forwards to a hosted Loki (observability.loki.url+push_url+credential) or observability.betterstack; refused with neither declared. T3.3: shared writes to the environment's ONE Loki as tenant <instance-id>; k8s tiers only. none renders no observability objects at all). On any other command, --obs shared refuses to start without --loki-tenant")
	// PodMonitor control (a managed cluster with no Prometheus Operator CRDs installed cannot
	// `kubectl create` the object render-k8s renders by default): auto detects it (read-only
	// `kubectl get crd`) when --kube-context is given, else includes it with a printed note; on/off
	// force it either way. See cmdRenderK8s for the detection and k8srender.Instance.PromOperatorLabel
	// ("none" already meant "omit it" before this flag existed — auto/off both resolve to that).
	fs.StringVar(&c.podmonitor, "podmonitor", "auto", "render-k8s: control the managed-tier PodMonitor object in obs.yaml — `mode` is auto (default: with --kube-context, a read-only kubectl get crd podmonitors.monitoring.coreos.com decides; with no --kube-context, it is included and a note is printed) | on (always include) | off (never include — e.g. a cluster with no Prometheus Operator, where kubectl create would otherwise fail on it)")
	// SUT-namespace log collection (render-k8s's promtail tails /var/log/pods/<sut-namespace>_*,
	// which may carry user/agent content — e.g. a message-bus SUT): OFF by default, so a tester
	// gets it only by asking. onboard.sh passes this explicitly for every tier it drives, so
	// existing onboarding behaviour (and its rendered manifests) is unchanged by this default flip.
	fs.BoolVar(&c.collectSUTLogs, "collect-sut-logs", false, "render-k8s: collect the SUT namespace's own pod logs via the bundled promtail (default OFF — a SUT's logs may carry user/agent content the tester never opted into sharing; prints which namespace will be read when set)")
	// T3.3: a single named default (k8srender.SharedLokiInClusterURL) — the in-cluster address of the
	// Loki `argus render-obs-shared` renders. Both the executor and promtail run in-cluster, so it
	// serves k3d and aks alike.
	fs.StringVar(&c.obsSharedURL, "obs-shared-url", k8srender.SharedLokiInClusterURL, "render-k8s --obs shared: the environment's shared Loki base URL")
	// T3.3: EVERY executor request to Loki carries X-Scope-OrgID when this is set, and NONE when it is
	// empty (bundled/adopt/export see exactly today's requests). Default from the env so a deploy can
	// set it either way; render-k8s --obs shared passes it as a flag.
	fs.StringVar(&c.lokiTenant, "loki-tenant", os.Getenv("ARGUS_LOKI_TENANT"), "the Loki tenant sent as X-Scope-OrgID on every Loki request (else ARGUS_LOKI_TENANT); required with --obs shared, empty sends no header")
	fs.BoolVar(&c.cloudOnly, "cloud-only", false, "cloud-teardown: accepted for teardown.sh symmetry (cloud-teardown is inherently cloud-only). R9/CP-M3-121: rejecting this flag made onboard.sh's ghost-purge a silent no-op")
	fs.StringVar(&c.window, "logs-window", "10m", "log/saga lookback window")
	fs.StringVar(&c.window, "window", "10m", "alias of --logs-window")
	fs.StringVar(&c.file, "file", "", "scenario file path (validate/write)")
	fs.StringVar(&c.path, "path", "", "destination path (write-scenario)")
	fs.StringVar(&c.from, "from", "", "propose-scenario: plain-text description")
	fs.StringVar(&c.target, "target", "", "propose-scenario: target service name")
	fs.StringVar(&c.serverURL, "server-url", "", "mcp-call: the SUT MCP endpoint (use ${VAR})")
	fs.StringVar(&c.transport, "transport", "streamable-http", "mcp-call: streamable-http | http-sse (declared, not guessed)")
	fs.StringVar(&c.tool, "tool", "", "mcp-call: the tool to call")
	fs.StringVar(&c.argsRaw, "args", "", "mcp-call: tool arguments as JSON or @file.json")
	fs.StringVar(&c.requestID, "request-id", "", "mcp-call: correlation id (injected as _meta.request_id)")
	fs.StringVar(&c.mcpToken, "mcp-token", "", "mcp-call: the SUT bearer token (else targets.mcp.auth.bearer_token from the config)")
	fs.StringVar(&c.expectPlane, "expect-plane", "", "mcp-call: none | protocol | tool (judge the verdict)")
	fs.StringVar(&c.expectCodeStr, "expect-code", "", "mcp-call: expected protocol-plane JSON-RPC error code")
	// AC-36: with --config, package-check certifies the package: block that config declares, rooted
	// at the config file's directory unless --root says otherwise; without it, the whole tree at
	// --root (default .) at this repository's conventional paths, as before.
	fs.StringVar(&c.root, "root", "", "package-check: root of the tree to check (default: the --config file's directory, else .)")
	fs.BoolVar(&c.jsonOut, "json", false, "package-check --json / propose-from-repo --json: emit the report as JSON instead of human-readable text")
	fs.StringVar(&c.repo, "repo", "", "propose-from-repo: the repository to scan (READ-ONLY — nothing under it is ever written)")
	fs.StringVar(&c.tokenDir, "token-dir", "", "cloud-enroll: directory to write DIR/<instance>.enrollment files into (mode 600; dir created 0700)")
	fs.StringVar(&c.revokeID, "revoke", "", "cloud-enroll: revoke this enrollment id instead of minting (DELETE /api/enrollments/<id>)")
}

func env(cf *commonFlags) toolcore.Env {
	// R4-2: the argus_instance telemetry label = the REAL registered instance id when the federation env
	// supplies it (ARGUS_INSTANCE_ID), even though the tool identity (cf.instance) stays "local" — so
	// direct + cloud runs share ONE Grafana instance drawer. Empty → Env falls back to Instance,
	// preserving the M2.5 standalone "local" behavior.
	return toolcore.Env{
		Instance: cf.instance, ObsInstance: os.Getenv("ARGUS_INSTANCE_ID"), ConfigPath: cf.configPath, ScenariosDir: cf.scenariosDir,
		ResultsRoot: cf.resultsRoot, ComposeFile: cf.composeFile,
		Grafana: cf.grafana, Loki: cf.loki, LokiTenant: cf.lokiTenant, Pushgateway: cf.pushgateway,
		JMeterLocal: cf.jmeterMode == "local", TemplatesDir: cf.templatesDir,
		Cluster: os.Getenv("ARGUS_CLUSTER"), // R6/ADR-10: deploy-derived; empty → "compose"
		// VR-E8: the tier a per-tier config value must be resolved against. NOT defaulted to
		// "compose" — an unset tier is UNKNOWN, and a consumer must say so rather than resolve
		// against a tier nobody chose.
		Tier: federation.WireTier(os.Getenv("ARGUS_TIER")),
		// the --obs mode onboarding chose; empty (a hand-run command) changes nothing.
		ObsMode: os.Getenv("ARGUS_OBS_MODE"),
	}
}

// resolveTierGrafana sets e.Grafana to the browser-facing base declared for THIS TIER in the SUT's
// argus-config (M3-FX VR-E8/VR-F9). It is called from every entry point that builds a dashboard link,
// so the three of them cannot drift apart — which is how the CLI path came to keep the localhost
// default long after cmdServe stopped using it (FX-1 fixed serve only).
//
// The rules, and why each is what it is:
//
//	config declares this tier      -> use it. The config is the authority; the --grafana flag is the
//	                                  obs stack's INTERNAL address and was never the human's link.
//	config loads, tier has NO value-> EMPTY, and say so. A declared config that cannot answer for
//	                                  this tier must not silently fall back to localhost:3000 —
//	                                  that fallback IS INT-008, and it produced links that 404 for
//	                                  weeks because a wrong link looks exactly like a right one.
//	no config, or tier unknown     -> leave the flag value untouched. Direct/dogfood runs have no
//	                                  SUT config and legitimately want the default; warn so the
//	                                  link's provenance is never a mystery.
//
// ParseUnresolved, NOT Load: Load also expands ${VAR} secrets and fails loudly when one is unset.
// The Grafana base needs no secret, and gating it on one is how the Social SUT — whose config
// references TEST_USER_STATIC_TOKEN — kept resolving to localhost:3000 on the managed tier even
// after the per-tier value was declared. Measured while verifying the two live configs: the
// knowledge-base SUT (no ${VAR}) resolved correctly and Social did not, from the same code path.
func resolveTierGrafana(e *toolcore.Env) {
	c, err := config.ParseUnresolved(e.ConfigPath)
	if err != nil || e.Tier == "" {
		return // no config to consult, or no tier to consult it for: keep the flag value
	}
	base, ev := grafanaResolution(c, e.Tier)
	e.Grafana = base
	if ev != nil {
		emit(ev)
	}
}

// grafanaResolution is the decision resolveTierGrafana makes, pure so it is testable: the Grafana base
// for this tier ("" = none: absence, not a lie) and the event to emit, if any.
//
// UI-2 (, A'2): an environment that declares observability.dashboard_link.template needs
// no Grafana base -- its links are rendered by the control plane from the template -- so for it the
// missing base is an {"info"}, not the {"warn": "no dashboard link will be produced"} that is true only
// of an environment with neither.
func grafanaResolution(c *config.Config, tier string) (string, map[string]any) {
	u, gerr := c.GrafanaPublicURL(tier)
	if gerr == nil {
		return u, nil
	}
	if tmpl, _ := c.DashboardLinkDecl(); tmpl != "" {
		return "", map[string]any{"info": "dashboard links come from observability.dashboard_link.template", "tier": tier}
	}
	return "", map[string]any{"warn": "no dashboard link will be produced", "reason": gerr.Error(), "tier": tier}
}

// tierGrafana resolves the browser-facing Grafana base for THIS process, ONCE.
//
// ── INT-008, FOURTH INCARNATION (TS-F6, found live 2026-08-10) ───────────────────────────────────
// cmdServeMode used to resolve the per-tier value into the env it gave the MCP TOOLS, and build the
// federated runner's ExecConfig fifteen lines earlier straight from `--grafana` — which onboarding
// sets to the compose default http://localhost:3000 on EVERY tier. The managed executor therefore
// answered two different things about itself:
//
//	runner__get_dashboard_url -> https://grafana.example.com/d/argus-overview-social-aks-v1?…
//	the deep_link it STORED   -> http://localhost:3000/d/argus-overview-social-aks-v1?…
//
// The tool told the truth and the durable record lied — and the record is the one an operator opens
// from the Runs page during triage, days later, on a machine where localhost:3000 is something else
// entirely or nothing at all.
//
// The defect was never a wrong computation: resolveTierGrafana was correct throughout. It was a
// SECOND SOURCE for a value that must have exactly one. This function is that one source.
//
// Returns "" when the tier declares no entry — absence, not a lie. obsquery.DashboardURL turns an
// empty base into an empty link rather than a relative path that reads like a URL and opens nothing.
func tierGrafana(cf *commonFlags) string {
	e := env(cf)
	resolveTierGrafana(&e)
	return e.Grafana
}

// recommendedImages reads the per-variant digest-pinned executor images the control plane publishes
// (VR-F18). Only variants that are actually configured appear, so "the control plane has no
// recommendation for full" stays distinguishable from "it recommends the slim one for everything" —
// the second is a guess wearing the first's clothes.
// firstNonEmpty returns the first non-blank value — used for the version-floor cutover, where a new
// name and a legacy one must both be honoured until rollback is no longer possible.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ── VR10-R3 (V28-007): `--config` is required wherever a config is read; there is no default ─────
//
// `argus run` typed inside the executor with --config forgotten used to fall back to the bundled
// OrderService demo — a different SUT — print `run_begin: ok` (the run really had begun), and fail later
// in the background, away from the line that said everything was fine. The owner's ruling: the demo
// default is deleted and a subcommand that needs a config and did not get one refuses at flag parsing,
// naming the flag, before any run id is minted. No fifth `run_begin` value exists for this — the refusal
// happens before any `run_begin` is emitted, so `ok` keeps exactly one meaning.

// missingConfigMsg is the ONE refusal every config-reading subcommand prints, prefixed by its own name
// (SA §0.14 R3-a): `run: --config is required …`, `serve: …`, `validate-config: …`.
func missingConfigMsg(cmd string) string {
	return cmd + ": --config is required (the argus-config.yaml of the SUT under test); there is no default"
}

// configRequired reports whether a subcommand READS the SUT's argus-config.yaml to do its job — the
// set that must refuse an empty --config. Membership is "needs the config", not "touches it":
//
//   - run / run-direct / serve (bare, and --mode runner) run the SUT; validate-config, capabilities,
//     preflight-auth, select-image, secrets-scan, render-obs, render-k8s, onboard-guard each parse it as
//     their whole purpose.
//   - `serve --mode control` is the CONTROL PLANE: it has no SUT and no argus-config, and its image's
//     ENTRYPOINT carries no --config (deploy/control/Dockerfile). Gating it would take the CP down.
//   - `mcp-call --server-url …` is the documented flags-only mode; without --server-url the endpoint
//     has to come from the config, so then the config is required.
//   - get-report / get-sagas / tail-logs / get-dashboard-url read the config OPPORTUNISTICALLY (the
//     log-field translation table, the tier's Grafana base) and fall back by design — byo-smoke.sh calls
//     get-report and get-sagas without --config — so they are not gated. Neither is anything that never
//     reads it (the cloud-* steps, scenario commands, version, init, router, keygen, runner-state).
func configRequired(cmd string, cf *commonFlags) bool {
	switch cmd {
	case "run", "run-direct", "validate-config", "capabilities", "preflight-auth", "select-image",
		"secrets-scan", "render-obs", "render-k8s", "onboard-guard":
		return true
	case "serve":
		return serveMode(cf) != "control"
	case "mcp-call":
		return cf.serverURL == ""
	}
	return false
}

// serveMode is the deploy profile `serve` runs in: --mode, else ARGUS_MODE, else "" (the M2.5 MCP
// server). One reader, so the --config gate and the serve dispatch cannot disagree about it.
func serveMode(cf *commonFlags) string {
	if cf.mode != "" {
		return cf.mode
	}
	return os.Getenv("ARGUS_MODE")
}

func recommendedImages() map[string]string {
	out := map[string]string{}
	for _, v := range []struct{ variant, env string }{
		{"slim", "RECOMMENDED_EXECUTOR_IMAGE_SLIM"},
		{"full", "RECOMMENDED_EXECUTOR_IMAGE_FULL"},
	} {
		if ref := strings.TrimSpace(os.Getenv(v.env)); ref != "" {
			out[v.variant] = ref
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func dispatch(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return usage()
	}
	cmd, rest := args[0], args[1:]
	cf := &commonFlags{}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cf.bind(fs)
	// msgbus tester follow-up item 1 (2026-09-28, help_filter.go): fs carries EVERY common flag
	// (cf.bind above just registered them all), so the stock fs.Usage() would print all of them
	// for every command sharing this flagset — exactly the F-CLI-HELP-1 fix's remaining gap
	// (`render-k8s --help` listing `-addr`, `-args`, `-scope`, ...). Replace it with one that
	// prints only the flags commandFlags[cmd] says this command reads.
	fs.Usage = filteredUsage(fs, cmd)
	// VR4-B3: the common parser is handed ONLY the tokens it declared. Everything else — a subcommand's
	// own flags, its positionals — reaches the subcommand in order. Parsing the whole vector here is
	// what made `cloud-onboarding-state --state` unusable and left `onboarding_state` never stamped.
	// See commonflags.go for why this is a partition and not a re-parse.
	// VR5-D1 (V19-010): the partition applies only to subcommands that READ the common flags. A
	// self-parsing subcommand receives its vector whole — taking a flag it declared itself can only
	// break it, which is how `router register --control-plane --token` shipped broken in V18.
	commonArgs, subArgs := subcommandArgv(cmd, fs, rest)
	if err := fs.Parse(commonArgs); err != nil {
		return emitErr(exitUsage, "flag error: %v", err)
	}
	// A flag this command does not own must not vanish (unknownflags.go): `validate-config
	// --scenarios-dir x` used to drop the flag, check the default path, find 0 and say VALID.
	//
	// F-CLI-HELP-1: `argus <cmd> --help` used to fall all the way through to the GLOBAL `usage()` —
	// every command that reads the common flagset (render-k8s, cloud-list-workspaces, and every
	// other command not in ownsArgv/readsSubArgs) printed the whole command list instead of its own
	// flags. A known command gets ITS OWN usage (fs carries its name and every flag it declared,
	// common or its own — cf.bind already ran above) at exit 0, no token required; an unrecognised
	// command falls back to the global list rather than being dressed up as though it existed.
	if wantsHelp(cmd, subArgs) {
		if isKnownCommand(cmd) {
			fs.Usage()
			return exitOK
		}
		return usage()
	}
	if msg := unknownFlagError(cmd, fs, subArgs); msg != "" {
		return emitErr(exitUsage, "%s", msg)
	}
	// REQUIRED #1: precise "was --token-out actually typed" — see the tokenOutSet field comment.
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "token-out" {
			cf.tokenOutSet = true
		}
	})
	// VR10-R3 (V28-007): a config-reading subcommand with no --config refuses HERE, at flag parsing —
	// above the token gate and before any run id is minted or any `run_begin` is emitted. A bare
	// `docker run <image>` (the image CMD is `serve …` with no --config) lands here too, on purpose.
	if cf.configPath == "" && configRequired(cmd, cf) {
		return emitErr(exitUsage, "%s", missingConfigMsg(cmd))
	}

	// `init` is a PRE-AUTH bootstrap (FX5): it extracts the bundled onboarding kit from the
	// image and needs no tokens — the colleague runs it before any token pair exists.
	if cmd == "init" {
		return cmdInit(subArgs)
	}

	// T2.3: `preflight` is PRE-AUTH, and it has to be. One of the things it REPORTS is that no
	// control-plane token is present — a preflight that refuses to run without a token can never
	// deliver that finding, and would answer "auth not configured" to the exact question it
	// exists to answer. It reads nothing but public cluster metadata and one HTTP GET, presents
	// no credential of its own, and changes nothing.
	if cmd == "preflight" {
		return cmdPreflight(subArgs)
	}

	// `doctor` is PRE-AUTH for the same reason `preflight` is: one of the things it REPORTS is the
	// state of the credential (absent, expired, the wrong kind), so it cannot require a working one.
	// Read-only by construction — see cmd/argus/doctor.go.
	if cmd == "doctor" {
		return cmdDoctor(subArgs)
	}

	// `tester init <app>` / `builder init <app>` (onboarding review items 18, 19) write the credential
	// plumbing for a session (env-file stanza, headersHelper, CLI wrapper, .mcp.json) and, for the
	// builder, verify the runner-scope token with one tools/list. PRE-AUTH: they exist to be run BEFORE
	// any token is in place. cmd/argus/session_init_cmd.go.
	if cmd == "tester" {
		return cmdTester(subArgs)
	}
	if cmd == "builder" {
		return cmdBuilder(subArgs)
	}

	// U1 (Stage II): `render-k8s` is a PURE LOCAL TRANSFORM of files the operator already has
	// (their argus-config + flags) into per-instance k8s manifests. The onboarder runs it BEFORE
	// the instance exists, so there is no token pair to present yet — pre-auth, like `init`.
	// It mints nothing; the tokens it embeds come from the caller's own environment.
	if cmd == "render-k8s" {
		return cmdRenderK8s(env(cf), cf)
	}
	// `upgrade` (onboarding review items 11 and 22) re-renders an instance with render-k8s's own inputs
	// and patches what differs in place. PRE-AUTH like `secrets`: its authority is the caller's
	// kubeconfig, it presents no Argus token. It parses its OWN argv (ownsArgv): --instance, --apply and
	// --kubeconfig are its own, and every render-k8s flag is bound onto its flagset. cmd/argus/upgrade_cmd.go.
	if cmd == "upgrade" {
		return cmdUpgrade(rest)
	}
	// T3.3: the environment's ONE shared Loki — a pure local transform like render-k8s, pre-auth for
	// the same reason (onboarding renders it before any instance exists).
	if cmd == "render-obs-shared" {
		return cmdRenderObsShared(cf)
	}
	// T3.3: every command past here may talk to Loki. Under --obs shared an empty tenant would send
	// unscoped requests — refused HERE, before the token gate and before anything runs. (render-k8s
	// is above on purpose: it derives the tenant from --instance-id and renders it, it sends nothing.)
	if err := sharedTenantGuard(cf.obsMode, cf.lokiTenant); err != nil {
		return emitErr(exitUsage, "%v", err)
	}

	// `router serve` is the LOCAL ROUTER (M3-FX block A). PRE-AUTH like `init` and `render-k8s`:
	// it presents no credential of its own and holds no in-env token pair — its authority comes
	// entirely from its own state file, which onboarding wrote. Requiring the M2.5 token pair here
	// would be asking the router to authenticate to itself.
	if cmd == "router" {
		return cmdRouter(subArgs)
	}

	// `mcpjson` is the agent-folder config editor (VR-A1..A3, VR-A5). PRE-AUTH for the same reason
	// `router` is: it edits a file on this machine and presents no credential. It is the ONLY writer
	// of .mcp.json — bash does not get to edit JSON, because bash has no parser and the file belongs
	// to the user.
	// `skills` prints what onboarding installs for a hat. It exists so internal/skills is the ONE
	// definition. ⚠ It had TWO consumers; teardown's purge is gone (owner ruling 2026-08-21) and
	// onboard.sh names the skills literally, so nothing reads this today. See skills_cmd.go.
	if cmd == "skills" {
		return cmdSkills(subArgs)
	}

	// `calm import` turns a FINOS CALM architecture into Argus check files. A PURE LOCAL
	// read-and-write: no control plane, no SUT, no credential — PRE-AUTH like validate-scenario. It owns
	// its argv (ownargv.go): --out and --bind-style flags must reach it whole.
	if cmd == "calm" {
		return cmdCalm(subArgs)
	}

	if cmd == "mcpjson" {
		return cmdMCPJSON(subArgs)
	}

	// `secrets` (P1 #9, tester 2026-09-27) sets/lists keys in an instance's Secret after enrollment —
	// a PURE LOCAL kubectl invocation, pre-auth like `render-k8s`/`preflight`: its authority is
	// whatever kubeconfig/context the caller's kubectl already has, not this machine's in-env
	// runner/author pair. See cmd/argus/secrets_cmd.go.
	if cmd == "secrets" {
		return cmdSecrets(subArgs)
	}

	// `keygen` (G4/S4) mints a fresh Ed25519 machine key and prints it base64 — a PURE LOCAL op,
	// pre-auth like init/render-k8s. Onboarding calls it to PROVISION the identity into a k8s Secret
	// (k3d) or a compose secret file (compose) so the key lives OFF the results volume, instead of the
	// first pod self-minting it onto the shared results PVC. Prints nothing but the key on stdout.
	if cmd == "keygen" {
		// It takes no arguments. Help must never mint a key: whoever asked only wanted the usage, and
		// a key printed to a terminal counts as exposed. Any other argument is refused the same way.
		// `rest`, not subArgs: a token that happens to be a common flag (e.g. --out) is split off into
		// the common set and would otherwise pass unnoticed.
		if len(rest) > 0 {
			switch rest[0] {
			case "-h", "--help", "help":
				fmt.Println(keygenUsage)
				return 0
			}
			return emitErr(exitUsage, "keygen takes no arguments (got %q)\n%s", rest[0], keygenUsage)
		}
		k, kerr := runner.GenerateKeyB64()
		if kerr != nil {
			return emitErr(exitErr, "keygen: %v", kerr)
		}
		fmt.Println(k)
		return 0
	}

	// M3 (D-EXEC.2.8): `serve --mode control|runner` is a NEW deploy-profile bootstrap with its OWN
	// auth model (control: OAuth 2.1 + Clerk; runner: Ed25519 federation) — NOT the M2.5 CLI token
	// pair. Route it PRE-AUTH, like `init`. Bare `serve` (no mode) falls through to the M2.5 token
	// gate below and the unchanged M2.5 MCP server.
	if cmd == "serve" {
		if mode := serveMode(cf); mode != "" {
			return cmdServeMode(mode, cf)
		}
	}

	// M3 (D-FED.4 / UC074): `mark-deployment` emits a deployment marker to the CP over the machine-
	// identity federation channel — no M2.5 token pair. Pre-auth, like serve --mode / init.
	if cmd == "mark-deployment" {
		return cmdMarkDeployment(cf)
	}

	// M3 (D-FED.3 / UC059-062): `run-direct` triggers a DIRECT local run — resolves the scenario set via
	// the hybrid (fresh CP fetch → cached → refuse), runs it, reports up. Machine identity, pre-auth.
	if cmd == "run-direct" {
		return cmdRunDirect(cf)
	}
	// cloud-deregister (D-ONBOARD.6): non-interactive teardown — the machine identity is the auth, so it
	// sits ABOVE the token gate (like run-direct), NOT behind the Clerk device-login the cloud-* cmds use.
	if cmd == "cloud-deregister" {
		return cmdCloudDeregister(cf)
	}
	if cmd == "cloud-onboarding-state" {
		return cmdCloudOnboardingState(cf, subArgs)
	}

	// M3 (D-ONBOARD.7): the cloud-onboarding steps — pre-auth (they carry their OWN OAuth session, not
	// the M2.5 token pair). onboard.sh orchestrates these when run with --control-plane.
	if cmd == "cloud-login" {
		return cmdCloudLogin(cf)
	}
	if cmd == "cloud-logout" {
		return cmdCloudLogout(cf)
	}
	if cmd == "cloud-enroll" {
		return cmdCloudEnroll(cf)
	}
	if cmd == "cloud-check-instance" {
		return cmdCloudCheckInstance(cf)
	}
	if cmd == "cloud-mint-token" {
		return cmdCloudMintToken(cf)
	}
	if cmd == "cloud-executor-status" {
		return cmdCloudExecutorStatus(cf)
	}
	if cmd == "cloud-verify" {
		return cmdCloudVerify(cf)
	}
	if cmd == "cloud-teardown" {
		return cmdCloudTeardown(cf)
	}
	if cmd == "cloud-clock-check" {
		return cmdCloudClockCheck(cf)
	}
	if cmd == "select-image" {
		return cmdSelectImage(cf)
	}
	if cmd == "cloud-list-workspaces" {
		return cmdCloudListWorkspaces(cf)
	}
	if cmd == "cloud-create-workspace" {
		return cmdCloudCreateWorkspace(cf)
	}
	if cmd == "cloud-switch-workspace" {
		return cmdCloudSwitchWorkspace(cf)
	}
	if cmd == "cloud-seed-scenarios" {
		return cmdCloudSeedScenarios(cf)
	}

	// F12/UC071 (CP-M3-III-68): `version` reports THIS BINARY's identity and reads nothing else — no
	// config, no scenarios, no SUT, no control plane. It sits above the token gate on purpose: the
	// callers that need it most are an operator asking "what am I running?" and update.sh comparing
	// before/after, and neither has credentials to hand at that moment. Demanding a token to be told
	// a build number would make the version check useless exactly where it matters.
	if cmd == "version" {
		emit(map[string]any{
			"version":    buildinfo.Resolve(os.Getenv("ARGUS_VERSION")),
			"full":       buildinfo.Full(os.Getenv("ARGUS_VERSION")),
			"commit":     buildinfo.Commit,
			"built":      buildinfo.Date,
			"overridden": os.Getenv("ARGUS_VERSION") != "",
			// The addresses built into this binary (internal/buildinfo/hosts.go).
			"hosts": map[string]string{
				"control_plane": buildinfo.DefaultControlPlane(),
				"docs":          buildinfo.DocsURL,
				"grafana":       buildinfo.DefaultGrafana(),
			},
		})
		return exitOK
	}

	// VR10-U2 (V28-001): `runner-state` is the read-only fence check the onboarding kit execs into a
	// running executor before an update. It sits ABOVE the token gate for the same reason `version`
	// does: the caller is update.sh reaching in with `docker exec` / `kubectl exec`, and the rendered
	// update command carries no credential on purpose (SEC-4). The executor answers with the machine
	// identity already inside its container — it reads only ARGUS_CP_URL, ARGUS_INSTANCE_ID and
	// ARGUS_IDENTITY_PATH, never the presented token. Below the gate it was denied (rc 3) on
	// compose, whose long-lived executor has no ARGUS_TOKEN, and worked on k3d/managed only by
	// accident. Output contract: cmd/argus/runner_state.go.
	//
	// ⚠ THE `default` ARM OF THE GATED SWITCH BELOW IS PART OF THE CONTRACT: an image that predates
	// this subcommand lands there and exits exitUsage, which is how update.sh tells "this image is too
	// old, PROCEED" from "the check failed, REFUSE" (SA §0.10).
	if cmd == "runner-state" {
		return cmdRunnerState(nil)
	}

	// V31-001 (VR13-UP): `update` is the verb set the pasted update block invokes —
	//   update plan → preflight.sh → apply.sh → update commit → update report
	//
	// ⛔ PRE-AUTH, FOR THE SAME REASON `runner-state` IS (V28-001). The operator pastes this block
	// into their own shell; it carries no M2.5 token pair on purpose (SEC-4), and on compose there
	// is no ARGUS_TOKEN to carry. Below the gate every verb would exit `denied` — which is how
	// the fence check shipped broken on the one tier that most needed it.
	//
	// ⛔ AND IT OWNS ITS ARGV (ownargv.go). Four of its flags — --router-state, --instance-id,
	// --token, --control-plane — are ALSO declared by the common flagset, so the partition would
	// route them away and `update commit` would see an empty router state. That is V19-010 exactly,
	// and the enumerated guard in ownargv_test.go now covers this command too.
	if cmd == "update" {
		return cmdUpdate(subArgs)
	}

	// `up` orchestrates onboard.sh directly for a fresh instance — PRE-AUTH for the same
	// reason `update` is (V28-001/V31-001): the operator runs this BEFORE any instance (and so any
	// M2.5 token pair) exists, and onboard.sh carries its own cloud session, never ARGUS_TOKEN.
	//
	// ⛔ AND IT OWNS ITS ARGV (ownargv.go), for the identical reason `update` does — see up.go.
	if cmd == "up" {
		return cmdUp(subArgs)
	}

	// AC-9: package-check reads only the tree under --root and needs no token pair — it runs
	// PRE-AUTH like runner-state and update, so a CI runner (or a third party rebuilding a commit)
	// can gate the commit with no credentials at all.
	if cmd == "package-check" {
		return cmdPackageCheck(cf)
	}

	// F-CLI-VALIDATE-1: `validate-config` is PURE LOCAL VALIDATION — it reads the SUT's
	// argus-config.yaml + scenarios off disk (toolcore.ValidateConfig: config.Load, c.Validate,
	// ProbeTargets against the SUT — never the Argus control plane) and needs no
	// ARGUS_RUNNER_TOKEN/ARGUS_EXECUTOR_SECRET. It used to sit behind the in-env dark-factory hat
	// gate below, so `argus validate-config --config x.yaml` refused outright without a token pair —
	// exactly the gate package-check/propose-from-repo already sit above, for the same reason.
	//
	// This grants NOTHING a runner-scope caller could not already get: the identical call is exposed
	// on the MCP surface as `runner__validate_config` (internal/mcpserver/tools.go), reachable by
	// runner scope alone — checked by reading ValidateConfig's own payload (toolcore.go): it echoes
	// structure (targets configured, scenario/capability layers, reachability, warnings, the config
	// path itself) and never a resolved secret value, so moving the CLI path above the gate does not
	// widen what a runner-scoped caller can see, only removes a token requirement the tool-level
	// surface never had.
	if cmd == "validate-config" {
		return cmdValidateConfig(env(cf))
	}

	// `validate-scenario` is the same kind of verb: it parses the local --file (and, with --config, the
	// local config, for the per-record schema check) and nothing else — no control plane, no SUT, no
	// secret. It used to go through scenarioSide below and so demanded BOTH hat variables and an author
	// --token to read a file off disk, which stopped a tester validating a scenario before any token
	// existed. PRE-AUTH, like validate-config. A --token that is still passed (every older invocation and
	// doc passes one) is accepted and ignored: the flag is parsed by the common flagset before this point
	// and nothing here reads it. The verdict and exit codes are cmdValidateScenario's, unchanged.
	if cmd == "validate-scenario" {
		return cmdValidateScenario(cf)
	}

	// T5.1+T5.3 (MVP2-SPRINT.md E5): `propose-from-repo` reads a repo tree and toolcore.ValidateAll
	// (plus, with --config, the money guard) READ-ONLY and writes only under --out — no SUT, no
	// control plane, no credential of any kind. PRE-AUTH for the same reason package-check is: it
	// needs no token pair to do its job, and demanding one would gate a purely local, read-only
	// operation on infrastructure it never touches.
	if cmd == "propose-from-repo" {
		return cmdProposeFromRepo(cf)
	}

	// `certificate verify` is a third party's own verb (PRE-AUTH, never calls the control plane);
	// `certificate get` carries its own author session token, same as `runner-id` below.
	if cmd == "certificate" {
		return cmdCertificate(subArgs)
	}

	// AC-17: `runner-id mint|list|revoke` carries its own author session token — PRE-AUTH like the
	// two above, and like every cloud-* verb, since it has nothing to do with the in-env dark-factory
	// hat gate below.
	if cmd == "runner-id" {
		return cmdRunnerID(subArgs, cf)
	}

	// D2 token authorization (replaces the M2 self-asserted --role). A token is REQUIRED.
	authCfg := auth.Config{RunnerToken: os.Getenv("ARGUS_RUNNER_TOKEN"), AuthorToken: envname.Lookup(envname.ExecutorSecret, envname.ExecutorSecretDeprecated)}
	if err := authCfg.Validate(); err != nil {
		emit(map[string]any{"error": fmt.Sprintf("auth not configured: %v", err), "diagnose": doctorPointerLocalHats})
		return exitDenied
	}
	presented := cf.token
	if presented == "" {
		presented = os.Getenv("ARGUS_TOKEN")
	}
	hat, aerr := authCfg.Verify(presented)
	if aerr != nil {
		emit(map[string]any{
			"error":    fmt.Sprintf("denied: %v (supply --token or ARGUS_TOKEN; --role is not an auth mechanism)", aerr),
			"diagnose": doctorPointerLocalHats,
		})
		return exitDenied
	}

	e := env(cf)
	switch cmd {
	case "run":
		return cmdRun(e, cf)
	case "get-report":
		return cmdGetReport(e, hat, cf)
	case "get-dashboard-url":
		resolveTierGrafana(&e) // the CLI path kept the localhost default long after serve stopped using it
		emit(toolcore.GetDashboardURL(e, cf.corrID))
		return exitOK
	case "get-sagas":
		return cmdGetSagas(e, cf)
	case "tail-logs":
		return cmdTailLogs(e, cf)
	case "serve":
		return cmdServe(e, authCfg, cf)
	case "render-obs":
		return cmdRenderObs(e, cf)
	case "render-k8s":
		return cmdRenderK8s(e, cf)
	case "capabilities":
		return cmdCapabilities(e)
	case "onboard-guard":
		return cmdOnboardGuard(e)
	case "secrets-scan":
		return cmdSecretsScan(e)
	case "mcp-call":
		return cmdMCPCall(cf)
	case "preflight-auth":
		return cmdPreflightAuth(cf)
	case "propose-scenario":
		return scenarioSide(hat, func() int { return cmdProposeScenario(cf) })
	case "list-scenarios":
		return scenarioSide(hat, func() int { return cmdListScenarios(e) })
	case "read-scenario":
		return scenarioSide(hat, func() int { return cmdReadScenario(e, cf) })
	case "write-scenario":
		return scenarioSide(hat, func() int { return cmdWriteScenario(cf) })
	case "delete-scenario":
		return scenarioSide(hat, func() int { return cmdDeleteScenario(e, cf) })
	// ⚠ THIS `default` IS PART OF THE runner-state CONTRACT (that subcommand is dispatched above the
	// token gate, VR10-U2): an image that predates it lands here and exits exitUsage, which is how
	// update.sh tells "this image is too old, PROCEED" from "the check failed, REFUSE" (SA §0.10).
	default:
		return emitErr(exitUsage, "unknown command %q", cmd)
	}
}

// scenarioSide enforces the dark-factory boundary (VR-AUTH9): the product hat is
// DENIED the author/scenario-side commands (now token-authorized, not self-asserted).
func scenarioSide(hat role.Role, fn func() int) int {
	if !hat.CanAccessScenarios() {
		emit(map[string]any{
			"error": "not permitted for the product scope",
			"hint":  "scenario-side (author) commands require the test-agent token (ARGUS_EXECUTOR_SECRET, formerly ARGUS_AUTHOR_TOKEN)",
			// No "diagnose" here, on purpose: this refusal means the hat map is FINE and the token presented is
			// the runner hat, so doctor's local-hats check reports ok — see doctor_pointer.go's rule.
		})
		return exitDenied
	}
	return fn()
}

func usage() int {
	emit(map[string]any{
		"tool": "argus", "version": "M2.5",
		// Every command this binary dispatches (usage_commands.go), pinned by
		// TestUsageListsEveryDispatchedCommand. It was a curated subset until 2026-09-23, and the
		// commands it left out were the ones docs/DEPLOY-ARGUS.md tells a first-time operator to run.
		"commands": usageCommands,
		"deploy":   "deploying Argus against a system: docs/DEPLOY-ARGUS.md names the six commands, in order",
		"doctor": "doctor [--control-plane <url>] [--config <argus-config.yaml>] [--scenarios <dir>] reports what a first run " +
			"from this machine would trip over — the control-plane credential, the local hat map, the SUT config, the " +
			"scenarios dir, the executor version — and the fix for each; read-only, never prints a credential, no token " +
			"needed. Start here when a command is refused",
		// "version" above is the fixed generation marker, not this build. Said here because a
		// fresh operator reaches for `argus --version`, which is not a flag: it falls through to
		// the token gate and answers "auth not configured", an auth error for a version question.
		"version-note":      `"version" above is the generation marker; this build is "argus version" (there is no --version flag)`,
		"global_flags":      []string{"--instance-id", "--token", "--config", "--scenarios", "--results"},
		"auth":              "a token is REQUIRED (--token or ARGUS_TOKEN) except for the local checks validate-config, validate-scenario, package-check and propose-from-repo; ARGUS_RUNNER_TOKEN=runner scope, ARGUS_EXECUTOR_SECRET=runner+author scope (formerly ARGUS_AUTHOR_TOKEN)",
		"validate-scenario": "validate-scenario --file <scenario.md> [--config <argus-config.yaml>] checks one scenario file locally (with --config, every record against its declared message schema); no token needed and no ARGUS_RUNNER_TOKEN/ARGUS_EXECUTOR_SECRET — a --token that is passed is accepted and ignored",
		"package-check":     "package-check --config <argus-config.yaml> [--root <dir>] [--json] certifies the package the config's package: block declares (root = the config's directory unless --root); package-check --root <dir> [--json] checks the whole tree at this repository's conventional paths; no token needed",
		"propose-from-repo": "propose-from-repo --repo <dir> --out <dir> [--config <argus-config.yaml>] [--json] walks --repo READ-ONLY, proposes HTTP-Ingestion scenario drafts from its routes (OpenAPI/Swagger, Fastify/Express, Go net-http/mux/chi/gin), and writes only the drafts that pass validate-scenario (and, with --config money_handling, the money-path guard) under --out; no token needed",
	})
	return exitOK
}

// --- runner commands (thin wrappers over toolcore) ---

func cmdValidateConfig(e toolcore.Env) int {
	p, failed, err := toolcore.ValidateConfig(e)
	if err != nil {
		return emitErr(exitFailed, "%v", err)
	}
	emit(p)
	return failedExit(failed)
}

// cmdPackageCheck is AC-9's static completeness check of a commit (F4: manifests by digest,
// lockfiles, env and secret schemas, seed data). With --config (AC-36) it certifies the package
// the config's package: block declares, rooted at the config file's directory unless --root is
// given; without --config it reads the whole tree at --root at this repository's conventional
// paths. --json emits the report for an agent, else a human-readable clause-by-clause summary
// that names the certified package. Exit is 0 on PASS, 1 on any FAIL clause.
func cmdPackageCheck(cf *commonFlags) int {
	var rep packagecheck.Report
	if cf.configPath != "" {
		// ParseUnresolved, not Load: the check reads files, never a credential, so the config's
		// ${VAR}s need not resolve — and it still applies every load-time rule, including the
		// package block's path checks.
		c, err := config.ParseUnresolved(cf.configPath)
		if err != nil {
			return emitErr(exitFailed, "package-check: %v", err)
		}
		if c.Package == nil {
			return emitErr(exitUsage, "package-check: %s declares no package: block — declare the package to certify (manifests, lockfiles, env_schema, secrets_example, seed), or run with --root alone for the whole-root check", cf.configPath)
		}
		root := cf.root
		if root == "" {
			root = filepath.Dir(cf.configPath)
		}
		rep = packagecheck.CheckDeclared(root, packagecheck.Declaration{
			Origin:         packagecheck.OriginDeclared,
			Manifests:      c.Package.Manifests,
			Lockfiles:      c.Package.Lockfiles,
			EnvSchema:      c.Package.EnvSchema,
			SecretsExample: c.Package.SecretsExample,
			Seed:           c.Package.Seed,
		})
	} else {
		root := cf.root
		if root == "" {
			root = "."
		}
		rep = packagecheck.Check(root)
	}
	if cf.jsonOut {
		emit(rep)
	} else {
		printPackageCheckReport(rep)
	}
	if rep.Status() == packagecheck.StatusFail {
		return exitErr
	}
	return exitOK
}

func printPackageCheckReport(rep packagecheck.Report) {
	fmt.Printf("package-check %s: %s\n", rep.Root, rep.Status())
	d := rep.Declaration
	origin := "this repository's conventional paths, whole root"
	if d.Origin == packagecheck.OriginDeclared {
		origin = "declared by the argus-config package: block"
	}
	fmt.Printf("  certified package (%s):\n", origin)
	fmt.Printf("    manifests:       %s\n", strings.Join(d.Manifests, ", "))
	fmt.Printf("    lockfiles:       %s\n", strings.Join(d.Lockfiles, ", "))
	fmt.Printf("    env_schema:      %s\n", d.EnvSchema)
	fmt.Printf("    secrets_example: %s\n", d.SecretsExample)
	fmt.Printf("    seed:            %s\n", d.Seed)
	for _, c := range rep.Clauses {
		fmt.Printf("  %s: %s\n", c.Name, c.Status)
		for _, f := range c.Findings {
			fmt.Printf("    %s  %s\n", f.Location, f.Reason)
		}
	}
}

func cmdRun(e toolcore.Env, cf *commonFlags) int {
	// CLI run stays SYNCHRONOUS (no MCP client timeout here); a run_id is generated + stamped
	// so the result + report carry the run handle (DF-06/07). The MCP server runs it async.
	//
	// W1 fence (CP-M3-117, gate follow-up): the bare CLI run is the THIRD direct entry point — when
	// this process is CP-wired it honors the same lifecycle as serve/run-direct: begin (busy →
	// refuse) → heartbeat → report-up on success (D3: ALL runs report up) / abort-push on error.
	// Not CP-wired (or no identity yet) → unchanged standalone behavior.
	runID := cf.runID
	if runID == "" {
		runID = argus.NewRunID()
	}
	var reportUp func(rep *report.Report, rid, scope string, startedAt, finishedAt time.Time)
	var abort func()
	var stopHB func() // joined heartbeat stop; MUST run before any terminal push (gate-117)
	stopHeartbeat := func() {
		if stopHB != nil {
			stopHB()
			stopHB = nil
		}
	}
	if cpURL := os.Getenv("ARGUS_CP_URL"); cpURL != "" {
		inst := envOr("ARGUS_INSTANCE_ID", e.Instance)
		keyPath := envOr("ARGUS_IDENTITY_PATH", "results/"+inst+"/identity.key")
		if priv, kerr := runner.LoadKey(keyPath); kerr == nil {
			c := runner.NewClient(cpURL, inst, priv)
			scope := "full"
			switch {
			case cf.scenarioID != "":
				scope = "single"
			case cf.tag != "":
				scope = "tag"
			case cf.layer != "":
				scope = "layer"
			}
			bctx, bcancel := context.WithTimeout(context.Background(), 10*time.Second)
			berr := c.BeginRun(bctx, runID, scope)
			bcancel()
			switch {
			case errors.Is(berr, runner.ErrCPBusy):
				return emitErr(exitErr, "instance busy — a run is already in flight for this instance (control-plane fence)")
			case berr != nil:
				emit(map[string]any{"run_begin": "unreachable — proceeding offline", "reason": berr.Error(), "run_id": runID})
			default:
				emit(map[string]any{"run_begin": "ok", "run_id": runID, "instance_id": inst})
				started := time.Now().UTC()
				hctx, hcancel := context.WithCancel(context.Background())
				hdone := make(chan struct{})
				go func() {
					defer close(hdone)
					t := time.NewTicker(30 * time.Second)
					defer t.Stop()
					for {
						select {
						case <-hctx.Done():
							return
						case <-t.C:
							pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Second)
							_ = c.PushHeartbeat(pctx, runID, "", scope, started)
							pcancel()
						}
					}
				}()
				stopHB = func() { hcancel(); <-hdone } // JOINED stop (gate-117); called before report-up/abort
				abort = func() {
					stopHeartbeat() // no straggler may trail the abort push
					actx, acancel := context.WithTimeout(context.Background(), 10*time.Second)
					now := time.Now().UTC()
					_ = c.Push(actx, federation.ResultsPush{RunID: runID, Scope: scope, Status: "failed", StartedAt: &now, FinishedAt: &now})
					acancel()
				}
			}
			// F1 (CP-M3-III-62): the `run` CLI is the THIRD caller of ReportUp, and it lost results
			// the same way. Same outbox directory the executor's poll loop drains.
			c.Outbox = &runner.Outbox{
				Dir: runner.OutboxDir(e.ResultsRoot, inst),
				Log: func(f string, a ...any) { emit(map[string]any{"report_up": fmt.Sprintf(f, a...)}) },
			}
			// U2: the CLI run path gets the catalog source too. A k8s executor may be driven
			// through either entry point, and on k8s there is no local scenario directory to
			// fall back on — /scenarios is a per-pod emptyDir.
			e.SetFetcher = func(fctx context.Context, layer, tag, scenarioID string) ([]toolcore.CatalogScenario, error) {
				sel := federation.Selection{Layer: layer, Tag: tag, ScenarioRef: scenarioID}
				resp, ferr := c.FetchSet(fctx, runScopeOf(sel), sel)
				if ferr != nil {
					return nil, catalogErr(ferr)
				}
				out := make([]toolcore.CatalogScenario, 0, len(resp.Scenarios))
				for _, sc := range resp.Scenarios {
					out = append(out, toolcore.CatalogScenario{Path: sc.Path, Body: sc.Body})
				}
				return out, nil
			}
			// VR-F6: the CHEAP question, asked before every direct run. Without it the sync would
			// cost a full set transfer per fix-loop iteration and would rightly get switched off.
			e.SetHasher = func(fctx context.Context, layer, tag, scenarioID string) (string, error) {
				sel := federation.Selection{Layer: layer, Tag: tag, ScenarioRef: scenarioID}
				h, herr := c.FetchSetHash(fctx, runScopeOf(sel), sel)
				return h, catalogErr(herr)
			}
			reportUp = func(rep *report.Report, rid, sc string, startedAt, finishedAt time.Time) {
				stopHeartbeat() // no straggler may trail the terminal report-up push
				rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer rcancel()
				deepLink := toolcore.RunDashboardURL(e, rid)
				if perr := c.ReportUp(rctx, rep, rid, sc, "", deepLink, federation.Annotations{}, startedAt, finishedAt); perr != nil {
					emit(map[string]any{"report_up": "failed", "reason": perr.Error(), "run_id": rid})
					return
				}
				emit(map[string]any{"report_up": "ok", "run_id": rid, "instance_id": inst})
			}
		}
	}
	if reportUp != nil {
		e.RunReporter = reportUp
	}
	// VR-F6: the operator's explicit plan-B choice, if they made one.
	e.UseLocalScenarios = cf.useLocalScenarios
	p, failed, err := toolcore.Run(e, runID, cf.layer, cf.tag, cf.scenarioID)
	stopHeartbeat() // belt+braces: whatever path Run took, the ticker is down before we return
	if err != nil {
		if abort != nil {
			abort() // release the held fence + record the aborted run honestly
		}
		return emitErr(exitErr, "%v", err)
	}
	emit(p)
	return failedExit(failed)
}

func cmdGetReport(e toolcore.Env, hat role.Role, cf *commonFlags) int {
	p, failed, err := toolcore.GetReport(e, hat, cf.runID) // DF-07: --run-id scopes to a run
	if err != nil {
		return emitErr(exitErr, "%v", err)
	}
	emit(p)
	return failedExit(failed)
}

func cmdGetSagas(e toolcore.Env, cf *commonFlags) int {
	if cf.corrID == "" {
		return emitErr(exitUsage, "get-sagas needs --correlation-id")
	}
	p, _ := toolcore.GetSagas(e, cf.corrID, cf.window)
	emit(p)
	return exitOK
}

func cmdTailLogs(e toolcore.Env, cf *commonFlags) int {
	if cf.corrID == "" {
		return emitErr(exitUsage, "tail-logs needs --correlation-id")
	}
	p, _ := toolcore.TailLogs(e, cf.corrID, cf.window)
	emit(p)
	return exitOK
}

// cmdMarkDeployment (UC074) emits a deployment marker to the CP over the machine-identity federation
// channel, so the suite resets Current SUT State for this instance (D-FED.4 / WEB.2a). It reads the
// executor's federation config from env (ARGUS_CP_URL, ARGUS_INSTANCE_ID / --instance-id,
// ARGUS_IDENTITY_PATH) + an optional --commit.
func cmdMarkDeployment(cf *commonFlags) int {
	cpURL := os.Getenv("ARGUS_CP_URL")
	if cpURL == "" {
		return emitErr(exitUsage, "mark-deployment: ARGUS_CP_URL is required")
	}
	inst := cf.instance
	if v := os.Getenv("ARGUS_INSTANCE_ID"); v != "" && inst == "local" {
		inst = v
	}
	idPath := envOr("ARGUS_IDENTITY_PATH", "results/"+inst+"/identity.key")
	priv, err := runner.LoadOrCreateKey(idPath)
	if err != nil {
		return emitErr(exitErr, "mark-deployment: identity: %v", err)
	}
	marker := map[string]any{"at": time.Now().UTC().Format(time.RFC3339)}
	if cf.commit != "" {
		marker["commit"] = cf.commit
	}
	mj, _ := json.Marshal(marker)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := runner.NewClient(cpURL, inst, priv).MarkDeployment(ctx, mj); err != nil {
		return emitErr(exitErr, "mark-deployment: %v", err)
	}
	emit(map[string]any{"marked": true, "instance_id": inst, "marker": marker})
	return exitOK
}

// cmdRunDirect (UC059/060/061/062) triggers a DIRECT local run: resolve the scenario set via the D-FED.3
// hybrid (fresh CP fetch → cached-with-stamp → structured refusal), run it against the SUT, report up on
// its own run_id. Config from env (ARGUS_CP_URL optional; ARGUS_INSTANCE_ID/--instance-id;
// ARGUS_IDENTITY_PATH; ARGUS_SET_CACHE) + the run-selection flags.
func cmdRunDirect(cf *commonFlags) int {
	inst := cf.instance
	if v := os.Getenv("ARGUS_INSTANCE_ID"); v != "" && inst == "local" {
		inst = v
	}
	idPath := envOr("ARGUS_IDENTITY_PATH", "results/"+inst+"/identity.key")
	priv, err := runner.LoadOrCreateKey(idPath)
	if err != nil {
		return emitErr(exitErr, "run-direct: identity: %v", err)
	}
	var client *runner.Client
	if cpURL := os.Getenv("ARGUS_CP_URL"); cpURL != "" {
		client = runner.NewClient(cpURL, inst, priv)
	}
	exec := &runner.Executor{
		Client: client,
		Run: runner.NewRunFunc(runner.ExecConfig{
			InstanceID: inst, ConfigPath: cf.configPath, ResultsRoot: cf.resultsRoot,
			ComposeFile: cf.composeFile, JMeterLocal: cf.jmeterMode == "local", TemplatesDir: cf.templatesDir,
			// tierGrafana, NOT cf.grafana — the SECOND site of the same defect, found by the
			// structural assertion in tiergrafana_test.go rather than by reading. A DIRECT run
			// stores a deep_link exactly like a federated one, so taking the raw --grafana flag here
			// put localhost:3000 into the durable record of every k8s-tier direct run too.
			Grafana: tierGrafana(cf), Loki: cf.loki, LokiTenant: cf.lokiTenant, Pushgateway: cf.pushgateway,
			Cluster: os.Getenv("ARGUS_CLUSTER"),                   // R6/ADR-10
			Tier:    federation.WireTier(os.Getenv("ARGUS_TIER")), // VR-E8
			ObsMode: os.Getenv("ARGUS_OBS_MODE"),                  //
		}),
		// F1 (UC060): a direct run's terminal push is queued durably before the network call. This
		// process exits straight after, so the DELIVERY is done by the executor's poll loop reading
		// the same volume — which is exactly what the old "will land on the next reachable CP" log
		// line claimed while nothing implemented it.
		Outbox: &runner.Outbox{
			Dir: runner.OutboxDir(cf.resultsRoot, inst),
			Log: func(f string, a ...any) { emit(map[string]any{"run-direct": fmt.Sprintf(f, a...)}) },
		},
		Log: func(f string, a ...any) { emit(map[string]any{"run-direct": fmt.Sprintf(f, a...)}) },
	}
	scope, sel := directSelection(cf)
	cachePath := envOr("ARGUS_SET_CACHE", "results/"+inst+"/set-cache.json")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	push, src, err := exec.DirectRun(ctx, cachePath, scope, sel, 5*time.Second)
	if err != nil {
		if errors.Is(err, runner.ErrNoSetAvailable) {
			return emitErr(exitErr, "%v", err) // UC061 structured refusal
		}
		return emitErr(exitErr, "run-direct: %v", err)
	}
	out := map[string]any{"run": true, "run_id": push.RunID, "scope": push.Scope, "status": push.Status,
		"tallies": push.Tallies, "scenarios": len(push.Scenarios), "set_hash": src.SetHash, "cached": src.Cached}
	if src.Stamp != "" {
		out["scenario_set"] = src.Stamp // "cached (CP unreachable)" (UC060)
	}
	emit(out)
	return exitOK
}

// cmdCloudDeregister (D-ONBOARD.6): NON-INTERACTIVE teardown — remove THIS instance's cloud registration
// using the executor's Ed25519 MACHINE identity (the SAME key it registered with), so no Clerk/device-login
// is needed. Reads --control-plane (or ARGUS_CP_URL), --instance-id (or ARGUS_INSTANCE_ID),
// ARGUS_IDENTITY_PATH. Must run where the executor's identity.key lives (mount the results volume).
// Idempotent — an already-gone instance returns success.
// cmdCloudOnboardingState is how onboarding says what it achieved (VR-L5 / V17-021).
//
// Called TWICE by onboard.sh: "incomplete" the moment registration succeeds, and "complete" or
// "degraded" when the run ends. If the run dies in between — which is exactly what happened three
// times on 2026-08-13 — the instance is left saying "incomplete", which is the truth, instead of
// looking indistinguishable from a healthy one.
//
// FAILING TO REPORT MUST NOT FAIL THE ONBOARD. The caller treats a non-zero exit as advisory: an
// instance that is otherwise fine must not be torn down because a status report did not land. That
// is why this prints its reason and returns a distinct code rather than dying loudly.
func cmdCloudOnboardingState(cf *commonFlags, args []string) int {
	fs := flag.NewFlagSet("cloud-onboarding-state", flag.ContinueOnError)
	state := fs.String("state", "", "complete | degraded | incomplete")
	// VR5-O2: onboarding's own verdict on whether the Grafana dashboard really exists. Optional —
	// empty means "no opinion" and the control plane keeps whatever it already holds.
	dashState := fs.String("dashboard-state", "", "present | missing | unknown (VR5-O2; optional)")
	// VR4-B3a: this error used to be discarded. A bad flag then looked exactly like an absent one, and
	// the caller (onboard.sh) folds this command's failure into a NOTE — so the reason never surfaced
	// anywhere. That silence is how V18-003 survived a whole round.
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return emitErr(exitUsage, "cloud-onboarding-state: %v", err)
	}
	if *state == "" {
		if v := os.Getenv("ARGUS_ONBOARDING_STATE"); v != "" {
			*state = v
		}
	}
	// Env fallback for the same reason --state has one (V18-003): onboard.sh runs this inside a
	// container and a flag the binary does not yet know about is a hard parse failure, while an
	// unread env var is merely ignored. So an OLDER image keeps working with a NEWER kit.
	if *dashState == "" {
		*dashState = os.Getenv("ARGUS_DASHBOARD_STATE")
	}
	switch *dashState {
	case "", "present", "missing", "unknown":
	default:
		return emitErr(exitUsage, "cloud-onboarding-state: --dashboard-state %q is not one of present|missing|unknown", *dashState)
	}
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	inst := cf.instance
	if v := os.Getenv("ARGUS_INSTANCE_ID"); v != "" && inst == "local" {
		inst = v
	}
	if cpURL == "" || inst == "" || inst == "local" || *state == "" {
		return emitErr(exitUsage, "cloud-onboarding-state needs --control-plane (or ARGUS_CP_URL), --instance-id <name> and --state <complete|degraded|incomplete>")
	}
	idPath := envOr("ARGUS_IDENTITY_PATH", "results/"+inst+"/identity.key")
	priv, err := runner.LoadKey(idPath)
	if err != nil {
		return emitErr(exitErr, "cloud-onboarding-state: machine identity: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := runner.NewClient(cpURL, inst, priv).SetOnboardingStateWithDashboard(ctx, *state, *dashState); err != nil {
		return emitErr(exitErr, "cloud-onboarding-state: %v", err)
	}
	emit(map[string]any{"instance_id": inst, "onboarding_state": *state, "dashboard_state": *dashState})
	return exitOK
}

func cmdCloudDeregister(cf *commonFlags) int {
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	inst := cf.instance
	if v := os.Getenv("ARGUS_INSTANCE_ID"); v != "" && inst == "local" {
		inst = v
	}
	if cpURL == "" || inst == "" || inst == "local" {
		return emitErr(exitUsage, "cloud-deregister needs --control-plane (or ARGUS_CP_URL) and --instance-id <name>")
	}
	idPath := envOr("ARGUS_IDENTITY_PATH", "results/"+inst+"/identity.key")
	priv, err := runner.LoadKey(idPath)
	if err != nil {
		return emitErr(exitErr, "cloud-deregister: machine identity: %v (run this where the executor's identity.key lives — mount the results volume)", err)
	}
	client := runner.NewClient(cpURL, inst, priv)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// VR5-T1: the response carries the KIT this instance was onboarded from, so teardown can scrub the
	// credentials written there. Empty for anything onboarded before migration 027 — teardown then
	// says it could not find out, rather than scrubbing nothing and reporting success.
	out, err := client.DeregisterWithKitDir(ctx)
	if err != nil {
		return emitErr(exitErr, "cloud-deregister: %v", err)
	}
	// VR8-V1 (V26-004): report what the control plane ACTUALLY said.
	//
	// `"deregistered": true` used to be hardcoded here, so a 401-unknown-instance — which the
	// client correctly treats as idempotent — emitted the same body as a real delete. teardown
	// then printed three specific claims (the name is free, the data is purged, the tokens are
	// revoked) about an instance the control plane had never heard of.
	//
	// not_present is OMITTED rather than emitted false on the success path: a key that is absent
	// means the question does not arise, and a shell reading for it can tell that from a `false`.
	//
	// ⛔ AND `auto_token_revoked` IS NOT EMITTED AT ALL ANY MORE (VR10-T4-7). The de-register no
	// longer decides anything about a credential — the token is per machine, and an instance delete
	// cannot name a machine — so the CLI prints kit_dir only. Teardown reads the fate of this
	// machine's author token (minted during onboarding) from DELETE /api/routers/{id}.
	res := map[string]any{"instance_id": inst, "deregistered": !out.NotPresent, "kit_dir": out.KitDir}
	if out.NotPresent {
		res["not_present"] = true
	}
	// AC-D61: READ BACK what was just done, with the credential that did it. The teardown holds no
	// session token on the ordinary path, so its own verification used to read COULD NOT CHECK on
	// every run — even when the registration was plainly gone. The machine identity is already in
	// hand here, and the control plane answers 401 unknown_instance to it once the row is gone.
	//
	// registration_after is ONE of gone | present | unknown, and `unknown` (the control plane could
	// not be asked, or answered something that is not an answer) is NEVER rendered as gone. When the
	// de-register itself said not_present the control plane has already answered the question.
	if out.NotPresent {
		res["registration_after"] = "gone"
	} else {
		rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
		present, rerr := client.RegistrationPresent(rctx)
		rcancel()
		switch {
		case rerr != nil:
			res["registration_after"] = "unknown"
			res["registration_after_detail"] = rerr.Error()
		case present:
			res["registration_after"] = "present"
		default:
			res["registration_after"] = "gone"
		}
	}
	emit(res)
	return exitOK
}

// directSelection derives the run scope + selection from the CLI flags (single > tag > layer > full).
func directSelection(cf *commonFlags) (string, federation.Selection) {
	switch {
	case cf.scenarioID != "":
		return "single", federation.Selection{ScenarioRef: cf.scenarioID}
	case cf.tag != "":
		return "tag", federation.Selection{Tag: cf.tag}
	case cf.layer != "":
		return "layer", federation.Selection{Layer: cf.layer}
	default:
		return "full", federation.Selection{}
	}
}

// cmdCloudLogin (UC118) runs the OAuth device-code login against the control plane and writes the
// session token to a file (mode 600) — the human prompt goes to stderr; onboard.sh reads the token file
// for the subsequent cloud steps (never echoing the token into logs).
// cmdCloudLogin (AC-D24) runs the device-code sign-in ONCE and persists BOTH tokens to the private
// session store (~/.config/argus/session.json, or $ARGUS_SESSION_FILE) — mode 0600, dir 0700, never
// inside a git tree (onboard.SaveSession). Every later cloud-* call authenticates through that session
// and refreshes itself, so this is the only sign-in a human ever has to do in the 30-day RefreshTTL
// window (internal/control/oauth/endpoints.go:67).
//
// `--token-out` is BACK-COMPAT ONLY now (REQUIRED #1): unset, no token file is written at all — the
// session store is the only credential on disk. Passed EXPLICITLY (cf.tokenOutSet, set by fs.Visit in
// dispatch — see the field comment), the ACCESS token alone is still written there, same as before,
// for onboard.sh's existing `--token-out deploy/compose/cp-session.token` callers. That file carries
// no refresh token — REQUIRED #1's "there is nothing to migrate" — so a caller still reading it gets
// exactly the pre-AC-D24 behaviour (a token that goes stale in 15 minutes) until it is moved onto the
// session store or an explicit --token pointed at a freshly-read one.
func cmdCloudLogin(cf *commonFlags) int {
	// msgbus tester follow-up item 2 (2026-09-28): NO default — refuse before any network call
	// (and before writing anything) rather than silently minting a session in the wrong scope.
	// Checked first, above --control-plane: a caller who forgot BOTH should hear about --scope,
	// not be told to fix --control-plane and then get surprised a second time.
	if cf.scope == "" {
		return emitErr(exitUsage, "cloud-login: --scope is required, there is no default — "+
			"pass --scope author (operator/tester tools) or --scope runner (builder tools)")
	}
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	if cpURL == "" {
		return emitErr(exitUsage, "cloud-login: --control-plane (or ARGUS_CP_URL) is required")
	}
	scope := cf.scope
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	access, refresh, err := onboard.NewCloudClient(cpURL).Login(ctx, scope, os.Stderr)
	if err != nil {
		return emitErr(exitErr, "cloud-login: %v", err)
	}
	sessionPath, err := onboard.PersistLogin("", cpURL, scope, access, refresh)
	if err != nil {
		return emitErr(exitErr, "cloud-login: save session: %v", err)
	}
	result := map[string]any{
		"logged_in":    true,
		"session_file": sessionPath,
		"has_refresh":  refresh != "",
		"scope":        scope,
		"workspace":    onboard.WorkspaceFromToken(access),
	}
	if cf.tokenOutSet {
		if err := writeTokenOutFile(cf.tokenOut, access); err != nil {
			return emitErr(exitErr, "cloud-login: %v", err)
		}
		result["token_file"] = cf.tokenOut
		result["token_file_note"] = "access token only, no refresh — back-compat; prefer the session file"
	}
	emit(result)
	return exitOK
}

// writeTokenOutFile is the ONE place --token-out is written, across cloud-login/cloud-mint-token/
// cloud-switch-workspace — same git-tree refusal as the session store (REQUIRED #1: "with the same
// git-tree refusal"), same atomic 0600 write, so a caller who still asks for a plaintext token file
// gets the identical safety properties the session store has.
func writeTokenOutFile(path, token string) error {
	if err := onboard.SaveTokenOutFile(path, token); err != nil {
		return fmt.Errorf("write token-out %s: %w", path, err)
	}
	return nil
}

// cmdCloudLogout (AC-D24 REQUIRED #3) revokes the session's refresh token at the control plane (POST
// /oauth/revoke, RFC 7009) and deletes the session file. Idempotent: no session at all is success.
func cmdCloudLogout(cf *commonFlags) int {
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	sessionPath, perr := onboard.DefaultSessionPath()
	if perr != nil {
		return emitErr(exitErr, "cloud-logout: %v", perr)
	}
	var client *onboard.CloudClient
	if cpURL != "" {
		client = onboard.NewCloudClient(cpURL)
	} else if d, rerr := onboard.LoadSessionFile(sessionPath); rerr == nil && d.ControlPlane != "" {
		// no --control-plane given — fall back to the control plane the session itself remembers, so
		// a bare `argus cloud-logout` (no flags at all) still reaches the CP to revoke.
		client = onboard.NewCloudClient(d.ControlPlane)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hadSession, revoked, err := onboard.Logout(ctx, client, sessionPath)
	if err != nil {
		return emitErr(exitErr, "cloud-logout: %v", err)
	}
	if !hadSession {
		emit(map[string]any{"logged_out": true, "had_session": false, "message": "no session — already logged out"})
		return exitOK
	}
	emit(map[string]any{"logged_out": true, "had_session": true, "revoked": revoked, "session_file": sessionPath})
	return exitOK
}

// sessionForCommand builds the Session every cloud-* command below authenticates through (AC-D24
// REQUIRED #2). An explicit --token / ARGUS_CP_AUTHOR_TOKEN (formerly ARGUS_CP_TOKEN) takes
// PRECEDENCE and is STATIC — no refresh, no session file, exactly the pre-AC-D24 behaviour.
// Otherwise the on-disk session is loaded ($ARGUS_SESSION_FILE, else the OS default); a missing
// session is the "run cloud-login" error every one of these commands used to spell out by hand
// next to its own --token check.
func sessionForCommand(client *onboard.CloudClient, explicitToken string) (*onboard.Session, error) {
	// F-CRED-1: announce, on stderr, which credential this call is about to present and which
	// workspace it acts in — BEFORE the resolution below picks one, so the banner is computed from
	// the SAME precedence (flag, then env, then session file) as the resolution itself. Never the
	// token value.
	credentialBanner(client.BaseURL, explicitToken)
	cpForDiagnosis = client.BaseURL // so a refusal's doctor pointer names this control plane (doctor_pointer.go)
	if explicitToken == "" {
		explicitToken = envname.Lookup(envname.CPAuthorToken, envname.CPAuthorTokenDeprecated)
	}
	if explicitToken != "" {
		return onboard.NewStaticSession(client, explicitToken), nil
	}
	return onboard.LoadSession(client, "")
}

// sessionForCommandOptional is sessionForCommand for the handful of cloud-* commands that have always
// allowed an ANONYMOUS call (cloud-check-instance, cloud-list-workspaces with no --sut): a missing
// session degrades to an empty one (no Authorization header sent) rather than refusing, preserving
// that pre-AC-D24 behaviour exactly.
func sessionForCommandOptional(client *onboard.CloudClient, explicitToken string) *onboard.Session {
	sess, err := sessionForCommand(client, explicitToken)
	if err != nil {
		return onboard.NewEmptySession(client)
	}
	return sess
}

// cmdCloudEnroll (AC-D25) mints, or with --revoke retires, per-instance enrollment credentials over
// the auto-refreshing session — the direct-onboarding replacement for onboard.sh's own hand-rolled
// `auth_curl ... POST /api/enrollments` (onboarding/onboard.sh:2354). Each instance in --instance-id
// (comma-separated) is minted IN ORDER, on demand — the session refreshes at most once per instance
// that needs it (proactively, ≤60s from expiry, or reactively on a single 401), never in a batch
// up front, so an access token that expires mid-list is caught by the very next mint.
func cmdCloudEnroll(cf *commonFlags) int {
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	if cpURL == "" {
		return emitErr(exitUsage, "cloud-enroll: --control-plane (or ARGUS_CP_URL) is required")
	}
	client := onboard.NewCloudClient(cpURL)
	sess, err := sessionForCommand(client, cf.token)
	if err != nil {
		return emitErr(exitErr, "cloud-enroll: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if cf.revokeID != "" {
		var revoked bool
		doErr := sess.Do(ctx, func(token string) error {
			r, rerr := client.RevokeEnrollment(ctx, token, cf.revokeID)
			revoked = r
			return rerr
		})
		if doErr != nil {
			return emitErr(exitErr, "cloud-enroll --revoke %s: %v", cf.revokeID, doErr)
		}
		emit(map[string]any{"revoked": revoked, "enrollment_id": cf.revokeID})
		return exitOK
	}

	if cf.tokenDir == "" {
		return emitErr(exitUsage, "cloud-enroll: --token-dir <dir> is required (unless --revoke <id> is given)")
	}
	ids := []string{}
	for _, id := range strings.Split(cf.instance, ",") {
		id = strings.TrimSpace(id)
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return emitErr(exitUsage, "cloud-enroll: --instance-id <a[,b,...]> is required")
	}
	if err := ensureTokenDir0700(cf.tokenDir); err != nil {
		return emitErr(exitErr, "cloud-enroll: %v", err)
	}

	results := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		var enr *onboard.EnrollmentResult
		doErr := sess.Do(ctx, func(token string) error {
			e, merr := client.MintEnrollment(ctx, token)
			enr = e
			return merr
		})
		if doErr != nil {
			return emitErr(exitErr, "cloud-enroll: instance %q: %v", id, doErr)
		}
		filePath := filepath.Join(cf.tokenDir, id+".enrollment")
		if err := onboard.SaveTokenOutFile(filePath, enr.Token); err != nil {
			return emitErr(exitErr, "cloud-enroll: instance %q: write %s: %v", id, filePath, err)
		}
		// The enrollment token NEVER reaches stdout — only the handle (VR9-H3's shape, reused here).
		results = append(results, map[string]any{
			"instance_id":   id,
			"enrollment_id": enr.EnrollmentID,
			"workspace":     enr.Workspace,
			"expires_in":    enr.ExpiresIn,
			"file":          filePath,
		})
	}
	emit(map[string]any{"enrolled": results})
	return exitOK
}

// ensureTokenDir0700 creates --token-dir at 0700 (or tightens an existing looser one) before any
// *.enrollment file is written into it — the same posture the session store's own directory gets.
func ensureTokenDir0700(dir string) error {
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", dir)
	}
	if info.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// cmdCloudVerify (UC126) confirms the cloud wiring by running the MCP tools/list handshake against the
// control plane with the author token — the direct-branch onboarding verification.
func cmdCloudVerify(cf *commonFlags) int {
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	if cpURL == "" {
		return emitErr(exitUsage, "cloud-verify: --control-plane (or ARGUS_CP_URL) is required")
	}
	client := onboard.NewCloudClient(cpURL)
	sess, err := sessionForCommand(client, cf.token)
	if err != nil {
		return emitErr(exitErr, "cloud-verify: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var tools []string
	err = sess.Do(ctx, func(token string) error {
		t, verr := client.VerifyToolsList(ctx, token)
		tools = t
		return verr
	})
	if err != nil {
		return emitErr(exitErr, "cloud-verify: %v", err)
	}
	// VR-C5: the CLOUD publishes BARE names (author_request_run) so a gateway that joins its own
	// namespace with "__" produces one separator, not three. The single-underscore prefix matches the
	// bare names AND the old author__* form, so this check keeps working against a control plane that
	// has not been re-deployed yet — cloud-verify must not fail on version skew it is meant to detect.
	hasAuthor := false
	for _, t := range tools {
		if strings.HasPrefix(t, "author_") {
			hasAuthor = true
			break
		}
	}
	if !hasAuthor {
		return emitErr(exitErr, "cloud-verify: no author_* tools visible (wiring/scope wrong): %v", tools)
	}
	emit(map[string]any{"verified": true, "tools_count": len(tools), "has_author_tools": true})
	return exitOK
}

// printSeedSummary (VR10-S4-6/7, V28-006) is the OPERATOR-facing rendering of a seed: one line per
// reject — "  <path>  —  <reason>" — and then the arithmetic "imported N of M", which prints on a
// clean import too. A bare "imported 46" cannot be checked by someone who does not know that 50 were
// offered, and that someone is exactly the operator on a first onboard. It goes to stderr so the JSON
// on stdout stays machine-readable.
func printSeedSummary(w io.Writer, written, total int, failed []onboard.SeedFailure) {
	for _, f := range failed {
		fmt.Fprintf(w, "  %s  —  %s\n", f.Path, oneLine(f.Error))
	}
	if len(failed) > 0 {
		fmt.Fprintf(w, "imported %d of %d — %d rejected\n", written, total, len(failed))
		return
	}
	fmt.Fprintf(w, "imported %d of %d\n", written, total)
}

// oneLine keeps a reject on ONE line: the shell that renders this reads line by line, and a reason
// that wraps would leave half of itself unattributed to any file.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// cmdCloudSeedScenarios (D1, M3 fix plan R9/R11) uploads the baked/BYO scenarios into the cloud catalog
// so the web Scenarios page + author__list_scenarios are populated from the first onboarding — onboard.sh
// runs this after cloud-mint-token with the author token (minted during onboarding). Reads *.md from
// --scenarios, writes each via the cloud author__write_scenario tool under --instance-id.
//
// VR10-S4 (V28-006): a per-scenario refusal never aborts the seed — the valid files are written, because
// a colleague's first onboard should not fail on somebody else's typo — but it is NEVER silent either.
// stdout carries failed[]{path,error} for onboard.sh to render, stderr names every reject with its
// reason and prints "imported N of M", and the exit code is non-zero whenever anything was rejected, so
// a script caller that reads nothing but $? still cannot miss it.
func cmdCloudSeedScenarios(cf *commonFlags) int {
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	if cpURL == "" {
		return emitErr(exitUsage, "cloud-seed-scenarios: --control-plane (or ARGUS_CP_URL) is required")
	}
	client := onboard.NewCloudClient(cpURL)
	sess, err := sessionForCommand(client, cf.token)
	if err != nil {
		return emitErr(exitErr, "cloud-seed-scenarios: %v", err)
	}
	instanceID := cf.instance
	if v := os.Getenv("ARGUS_INSTANCE_ID"); v != "" && instanceID == "local" {
		instanceID = v
	}
	scenarios, err := onboard.LoadScenarioDir(cf.scenariosDir)
	if err != nil {
		return emitErr(exitErr, "cloud-seed-scenarios: read %s: %v", cf.scenariosDir, err)
	}
	if len(scenarios) == 0 {
		return emitErr(exitErr, "cloud-seed-scenarios: no .md scenarios found under %s", cf.scenariosDir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var res *onboard.SeedResult
	err = sess.Do(ctx, func(token string) error {
		r, serr := client.SeedScenarios(ctx, token, instanceID, scenarios)
		res = r
		return serr
	})
	if err != nil {
		return emitErr(exitErr, "cloud-seed-scenarios: %v", err)
	}
	emit(map[string]any{"seeded": res.Written, "total": res.Total, "failed": res.Failed, "instance_id": instanceID})
	printSeedSummary(os.Stderr, res.Written, res.Total, res.Failed)
	if len(res.Failed) > 0 || (res.Written == 0 && res.Total > 0) {
		return exitErr // VR10-S4-9: a reject reaches the exit code, not only the JSON
	}
	return exitOK
}

// cmdCloudCheckInstance (UC121/UC123) validates the instance-name charset then asks the CP whether the
// name is free to claim. Emits {valid, available, reason} — a taken/invalid name is NOT an error (the
// onboarding collision assist reads the fields and loops), only a transport failure is.
func cmdCloudCheckInstance(cf *commonFlags) int {
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	if cpURL == "" {
		return emitErr(exitUsage, "cloud-check-instance: --control-plane (or ARGUS_CP_URL) is required")
	}
	id := cf.instance
	if v := os.Getenv("ARGUS_INSTANCE_ID"); v != "" && id == "local" {
		id = v
	}
	if ok, msg := onboard.ValidInstanceName(id); !ok {
		emit(map[string]any{"instance_id": id, "valid": false, "available": false, "reason": msg})
		return exitOK
	}
	client := onboard.NewCloudClient(cpURL)
	sess := sessionForCommandOptional(client, cf.token) // anonymous check-instance stays allowed
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var ci *onboard.InstanceCheck
	err := sess.Do(ctx, func(token string) error {
		c, cerr := client.CheckInstance(ctx, token, id)
		ci = c
		return cerr
	})
	if err != nil {
		return emitErr(exitErr, "cloud-check-instance: %v", err)
	}
	emit(map[string]any{"instance_id": id, "valid": true, "available": ci.Available, "owned_by_you": ci.OwnedByYou, "stale": ci.Stale})
	return exitOK
}

// cmdCloudMintToken (UC131/UC011) auto-mints a USER-GLOBAL author token via the login session and
// writes it to a 600 file — onboard.sh wires the returned token-FREE endpoint + the Authorization
// header (S7, CP-M3-122/UC192) into the cloud .mcp.json so the user never handles a token on the
// direct path and no PAT ever sits in a URL. The token is not echoed to stdout.
func cmdCloudMintToken(cf *commonFlags) int {
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	if cpURL == "" {
		return emitErr(exitUsage, "cloud-mint-token: --control-plane (or ARGUS_CP_URL) is required")
	}
	client := onboard.NewCloudClient(cpURL)
	sess, err := sessionForCommand(client, cf.token)
	if err != nil {
		return emitErr(exitErr, "cloud-mint-token: %v", err)
	}
	name := cf.name
	if name == "" {
		name = "onboarding"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// AC-D24: the token used below to derive the record owner must be FRESH before that derivation —
	// SubjectFromToken reads the claim right off it, so a refresh has to happen first, not inside the
	// mint call's own retry.
	if err := sess.EnsureFresh(ctx); err != nil {
		return emitErr(exitErr, "cloud-mint-token: %v", err)
	}
	token := sess.Token()
	// VR3-09: THE PER-KIT TOKEN FILE IS GONE, so there is no default any more. A caller that does not
	// ask for a file gets none written; the router is the holder (VR3-08).
	//
	// ⚠ `--token-out` is a SHARED flag whose default is "cp-session.token", meant for cloud-login.
	// Simply deleting the old `cp-author.token` default would leave that shared value in place here,
	// and this subcommand would write the AUTHOR token over the SESSION token — silently swapping two
	// credentials with different scopes and lifetimes. So the shared default is treated as UNSET.
	out := cf.tokenOut
	if out == defaultTokenOut {
		out = ""
	}
	// VR-B3: REUSE before mint. Onboarding is re-run routinely — on every kit refresh — and each run
	// used to mint another token, which is how 195 of them accumulated. Read whatever this machine
	// already holds and hand it to the client, which mints only if that is missing or no longer
	// authenticates.
	//
	// V16 (VR3-08) moved the holder from kit files to the router; V27-009 (0.3.29) moved it again, to the record.
	// V27-009 redesign: the token lives on this machine's record for (control plane, user). The user is the
	// session's subject; "" resolves to the sole record for the URL (a single-user machine, or a lifted 0.3.28 state).
	recURL := recordURL(cpURL)
	recUser := onboard.SubjectFromToken(token)
	existing := ""
	if cf.routerState != "" {
		tok, rerr := router.ReadRecordToken(cf.routerState, recURL, recUser)
		if rerr != nil {
			// A corrupt state or a product folder holding a cloud entry. NOT recoverable by falling
			// through to a mint: this account may well already have a token, and minting a second one
			// is the VR-B3 conflict this round exists to remove. Say what is wrong instead.
			return emitErr(exitErr, "cloud-mint-token: %v", rerr)
		}
		existing = tok
		// V27-009 redesign / VR-B7: NO RECORD → NOTHING IS MINTED. Minting first and refusing at the write
		// would leave a live token on the control plane that no machine holds.
		if !router.HasRecord(cf.routerState, recURL, recUser) {
			return emitErr(exitErr, "cloud-mint-token: this machine holds no record for %s (user %q) — or holds more than one and the user is not named. Run `argus router register --control-plane %s` (onboarding step 8a) first, then mint again; nothing was minted", displayControlPlane(recURL), recUser, onboard.ControlPlaneForCommand(cpURL))
		}
	} else if out != "" {
		blob, _ := os.ReadFile(out)
		existing = strings.TrimSpace(string(blob))
	}
	// V27-009 redesign: the auto token is minted FOR THIS MACHINE — its router id is a pure function of the
	// identity key `router register` (step 8a) created in the same state dir. No state dir → no id → the control
	// plane refuses the auto mint, which is the right answer: a token with no machine to belong to.
	routerID := ""
	if cf.routerState != "" {
		if ident, ierr := router.LoadIdentity(cf.routerState); ierr == nil {
			routerID = ident.RouterID
		}
	}
	var mt *onboard.MintedToken
	var reused bool
	err = sess.Do(ctx, func(tok string) error {
		m, r, merr := client.MintOrReuseAuthorToken(ctx, tok, name, existing, routerID)
		mt, reused = m, r
		return merr
	})
	if err != nil {
		return emitErr(exitErr, "cloud-mint-token: %v", err)
	}
	// VR3-09, PARTIALLY LANDED — and the part that is deferred is stated here rather than implied.
	//
	// No DEFAULT file any more: a caller that does not ask gets none, and the router is the holder.
	// But `--token-out` still writes when asked, because two callers in onboard.sh genuinely need the
	// plaintext in the shell and were NOT converted in this round:
	//
	//   the SEED step   imports scenarios straight to the control plane with `--token "$SEED_TOKEN"`
	//   the HUB path prints the token for a human to paste into Hub
	//
	// Converting those needs each to take --router-state and read the token itself, which is a
	// separate change with its own risk. Until then, deleting the file outright would break scenario
	// import on every onboard — a worse bug than the duplication it removes.
	//
	// NOTE THE CONDITION: it writes on REUSE too, not only on a fresh mint. Before V16 a reuse
	// implied the file already held the token — that is what reuse MEANT. Now reuse reads from the
	// ROUTER, so the file may be absent or stale, and skipping the write would leave SEED_TOKEN empty
	// and silently import nothing.
	if out != "" {
		if err := os.WriteFile(out, []byte(mt.Token), 0o600); err != nil {
			return emitErr(exitErr, "cloud-mint-token: write token: %v", err)
		}
	}
	// VR3-10 — AND THIS IS WHAT MAKES "never in a shell variable" TRUE, not merely intended.
	//
	// Removing the token FILE is only half of it. onboard.sh used to `cat` that file into
	// $AUTHOR_CLOUD_TOKEN and pass it to `router wire --cloud-token`, so the credential sat in a
	// shell variable and on a command line — visible in a process listing — which is precisely what
	// VR-R12 forbids. Deleting the file without closing that path would just move the leak.
	//
	// So a FRESH mint is applied to the router here, in-process, by the same function a rotation
	// uses: every test-hat folder gets it, product folders are skipped by construction. The shell
	// never sees the value.
	//
	// A REUSE writes nothing: the router already holds that token — reuse is the statement that it
	// does — and rewriting it would be a no-op with a failure mode.
	// VR6-I1 (V23-008) — RECORD THE MACHINE-LEVEL HOLDER. THIS RUNS ON BOTH BRANCHES.
	//
	// 🚨 THE GAP IS THE FRESH MINT. ApplyRotatedAuthorToken below propagates to every test-hat FOLDER
	// and, at runtime.go:535, updates `State.Cloud` only `if st.Cloud != nil` — it has never created
	// one. So a fresh mint against a router with no holder yet applied the token to folders and left
	// the machine holding nothing.
	//
	// Measured seven times, most decisively on a router created from nothing by the onboard itself:
	// two folders wired, the test folder holding the token, `has cloud key: FALSE`. The next full
	// teardown then unwired the folders and the account was stranded — the exact loop VR5-C1 existed
	// to close.
	//
	// ⚠ THE REUSE BRANCH WAS ALREADY COVERED, and it is worth writing down so nobody "fixes" it twice.
	// With --router-state set, `existing` can only be non-empty if the router already holds the token;
	// and when a FOLDER holds it, ReadCloudAuthorToken's lift creates and persists the machine holder
	// on the way past. So reuse never lacked a holder. Proven, not assumed: with this write removed,
	// TestCloudMintToken_ReuseAlsoWritesTheMachineHolder still passes while the fresh-mint test fails.
	//
	// It is called unconditionally anyway — the branch that is safe today is not worth the branch that
	// has to stay correct as ReadCloudAuthorToken changes, and the write is idempotent.
	//
	// ⚠ ORDER: the holder is written BEFORE the per-folder propagation. If the folder write fails, a
	// machine that holds the token can still recover; a machine that holds nothing cannot.
	applied := 0
	if cf.routerState != "" && !reused {
		// ONE write, on the record `router register` (step 8a) created. No record → say so; never a second holder.
		exp := parseExpiry(mt.ExpiresAt)
		if werr := router.SetRecordToken(cf.routerState, recURL, recUser, mt.Token, exp); werr != nil {
			return emitErr(exitErr, "cloud-mint-token: obtained the token, but could not store it on this machine's record: %v", werr)
		}
		applied = 1
	}
	// S7 (CP-M3-122): endpoints are token-FREE; the PAT lives in the token file + travels header-only.
	// `minted` stays TRUE when reused so every existing caller keeps working; `reused` is the new
	// fact, and onboard.sh reads it only to choose which sentence to print.
	// `router_folders_updated` is the observable half of VR3-10: onboarding can no longer see the
	// token, so the COUNT is what tells it the store succeeded. 0 on a reuse is correct and expected.
	emit(map[string]any{"minted": true, "reused": reused, "id": mt.ID, "endpoint": mt.Endpoint,
		"sse_endpoint": mt.SSEEndpoint, "token_file": out, "router_folders_updated": applied})
	return exitOK
}

// cmdCloudExecutorStatus (UC128) checks whether the executor has REGISTERED under the workspace — the
// fail-loud confirmation after `serve --mode runner` starts. Emits {registered}.
func cmdCloudExecutorStatus(cf *commonFlags) int {
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	if cpURL == "" {
		return emitErr(exitUsage, "cloud-executor-status: --control-plane (or ARGUS_CP_URL) is required")
	}
	client := onboard.NewCloudClient(cpURL)
	sess, err := sessionForCommand(client, cf.token)
	if err != nil {
		return emitErr(exitErr, "cloud-executor-status: %v", err)
	}
	id := cf.instance
	if v := os.Getenv("ARGUS_INSTANCE_ID"); v != "" && id == "local" {
		id = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// VR6-I2 (V23-014) — REPORT WHETHER A POLL WAS ACCEPTED, not merely that a row exists.
	//
	// `registered` has always been the fail-loud check after the executor starts, and it is not
	// sufficient: orderservice-compose was registered and refused for twenty hours (7,747
	// bad_signature rejections, the first one second after the container started), and every check
	// onboarding ran was satisfied the whole time.
	//
	// `poll_accepted` is the fact that separates a working instance from that one. It costs nothing
	// extra — the same /api/instances response already carries last_seen, which only a poll sets.
	//
	// ⚠ THE ERROR IS NOT FOLDED INTO THE ANSWER. If the check cannot run we say so and fail, rather
	// than emitting poll_accepted:false — "I could not find out" and "it was refused" have different
	// remedies, and a caller that cannot tell them apart will fail a healthy onboard. Both calls run
	// inside ONE Session.Do so a single proactive/reactive refresh covers both.
	var reg, accepted bool
	var lastSeen time.Time
	var pollErr error
	err = sess.Do(ctx, func(token string) error {
		r, rerr := client.ExecutorRegistered(ctx, token, id)
		if rerr != nil {
			return rerr
		}
		reg = r
		a, ls, perr := client.ExecutorPollAccepted(ctx, token, id)
		accepted, lastSeen, pollErr = a, ls, perr
		return perr
	})
	if err != nil {
		if pollErr != nil {
			return emitErr(exitErr, "cloud-executor-status: registered=%v, but could NOT determine whether "+
				"the executor has polled: %v", reg, pollErr)
		}
		return emitErr(exitErr, "cloud-executor-status: %v", err)
	}
	out := map[string]any{"instance_id": id, "registered": reg, "poll_accepted": accepted}
	if accepted {
		out["last_seen"] = lastSeen.UTC().Format(time.RFC3339)
	}
	emit(out)
	return exitOK
}

// cmdCloudTeardown (UC145/UC053) removes the instance from the control plane using the login session —
// the name frees, the data dies, the executor's JWTs are instantly revoked. Idempotent.
func cmdCloudTeardown(cf *commonFlags) int {
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	if cpURL == "" {
		return emitErr(exitUsage, "cloud-teardown: --control-plane (or ARGUS_CP_URL) is required")
	}
	client := onboard.NewCloudClient(cpURL)
	sess, err := sessionForCommand(client, cf.token)
	if err != nil {
		return emitErr(exitErr, "cloud-teardown: %v", err)
	}
	id := cf.instance
	if v := os.Getenv("ARGUS_INSTANCE_ID"); v != "" && id == "local" {
		id = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var deleted bool
	err = sess.Do(ctx, func(token string) error {
		d, terr := client.TeardownInstance(ctx, token, id)
		deleted = d
		return terr
	})
	if err != nil {
		return emitErr(exitErr, "cloud-teardown: %v", err)
	}
	// VR10-T4-7: no `auto_token_revoked` here either. This is the route teardown's ghost recovery
	// takes, and it decides nothing about a credential any more.
	res := map[string]any{"instance_id": id, "cloud_removed": deleted}
	emit(res)
	return exitOK
}

// cmdCloudClockCheck (UC055) is the onboarding clock preflight: it warns when the local clock differs
// from the control plane by more than 2 minutes (federation JWTs may be rejected until it's fixed).
func cmdCloudClockCheck(cf *commonFlags) int {
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	if cpURL == "" {
		return emitErr(exitUsage, "cloud-clock-check: --control-plane (or ARGUS_CP_URL) is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	drift, err := onboard.NewCloudClient(cpURL).ClockDriftSeconds(ctx)
	if err != nil {
		return emitErr(exitErr, "cloud-clock-check: %v", err)
	}
	emit(map[string]any{"drift_seconds": int(drift), "warn": drift > 120 || drift < -120})
	return exitOK
}

// cmdSelectImage (UC164) prints the execution-plane image variant — "slim" or "full" —
// for a SUT's argus-config, chosen TRANSPARENTLY from its declared targets (the user never
// picks). onboard.sh captures the bare token on stdout to pick the image tag; the reason
// goes to stderr. Parses UNRESOLVED (variant selection needs no ${VAR} values).
func cmdSelectImage(cf *commonFlags) int {
	c, err := config.ParseUnresolved(cf.configPath)
	if err != nil {
		return emitErr(exitErr, "select-image: parse config %q: %v", cf.configPath, err)
	}
	variant, reason := onboard.SelectImageVariant(c)
	fmt.Fprintf(os.Stderr, "select-image: %s — %s\n", variant, reason)
	fmt.Println(variant)
	return exitOK
}

// cmdCloudListWorkspaces (UC119/UC120) lists the user's workspaces (id, name, instance count, active) and,
// when --sut is given, the owner's OTHER workspaces already holding that SUT (the same-SUT warning —
// warn, never block). Emits the JSON the onboarding parses to offer the pick/create + the warning.
func cmdCloudListWorkspaces(cf *commonFlags) int {
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	if cpURL == "" {
		return emitErr(exitUsage, "cloud-list-workspaces: --control-plane (or ARGUS_CP_URL) is required")
	}
	client := onboard.NewCloudClient(cpURL)
	sess := sessionForCommandOptional(client, cf.token) // anonymous list stays allowed (no --sut ownership needed)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var list *onboard.WorkspaceList
	err := sess.Do(ctx, func(token string) error {
		l, lerr := client.ListWorkspaces(ctx, token, cf.sut)
		list = l
		return lerr
	})
	if err != nil {
		return emitErr(exitErr, "cloud-list-workspaces: %v", err)
	}
	if cf.summary {
		// B2: flat, shell-parseable lines for the onboarding workspace picker (no jq/python on the host):
		//   ACTIVE <ws-id>
		//   WS <ws-id>\t<name>
		//   IN <ws-id>\t<instance-id>\t<sut-name>\t<tier>
		var b strings.Builder
		fmt.Fprintf(&b, "ACTIVE %s\n", list.Active)
		for _, w := range list.Workspaces {
			fmt.Fprintf(&b, "WS %s\t%s\n", w.ID, w.Name)
			for _, in := range w.Instances {
				fmt.Fprintf(&b, "IN %s\t%s\t%s\t%s\n", w.ID, in.InstanceID, in.SUTName, in.Tier)
			}
		}
		fmt.Print(b.String())
		return exitOK
	}
	emit(list)
	return exitOK
}

// cmdCloudCreateWorkspace (B2/UC002 over the login session): create a NEW workspace owned by the login
// user — the onboarding picker's "create new" branch. Emits {created, workspace_id, name}.
func cmdCloudCreateWorkspace(cf *commonFlags) int {
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	if cpURL == "" {
		return emitErr(exitUsage, "cloud-create-workspace: --control-plane (or ARGUS_CP_URL) is required")
	}
	if cf.name == "" {
		return emitErr(exitUsage, "cloud-create-workspace: --name is required")
	}
	client := onboard.NewCloudClient(cpURL)
	sess, err := sessionForCommand(client, cf.token)
	if err != nil {
		return emitErr(exitErr, "cloud-create-workspace: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var id string
	err = sess.Do(ctx, func(token string) error {
		i, cerr := client.CreateWorkspace(ctx, token, cf.name)
		id = i
		return cerr
	})
	if err != nil {
		return emitErr(exitErr, "cloud-create-workspace: %v", err)
	}
	emit(map[string]any{"created": true, "workspace_id": id, "name": cf.name})
	return exitOK
}

// cmdCloudSwitchWorkspace (B2/UC004 over the login session): re-bind the session to another OWNED
// workspace. The CP mints a NEW session token bound to it; we overwrite --token-out (mode 600) so every
// later onboarding step (mint/seed/register) lands in the picked workspace. Emits {switched, workspace}.
func cmdCloudSwitchWorkspace(cf *commonFlags) int {
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	if cpURL == "" {
		return emitErr(exitUsage, "cloud-switch-workspace: --control-plane (or ARGUS_CP_URL) is required")
	}
	if cf.workspaceID == "" {
		return emitErr(exitUsage, "cloud-switch-workspace: --workspace <id> is required")
	}
	client := onboard.NewCloudClient(cpURL)
	sess, err := sessionForCommand(client, cf.token)
	if err != nil {
		return emitErr(exitErr, "cloud-switch-workspace: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// NOTE (see "Anything you could not do or chose differently" in the evidence): SwitchWorkspace
	// mints a NEW workspace-bound token via its own endpoint, not an OAuth refresh_token grant — it
	// does not return a refresh token, and the session's EXISTING refresh token still targets the
	// PREVIOUS workspace (RefreshToken.WorkspaceID is fixed at issuance). This still writes the new
	// token to --token-out (as it always has), but deliberately does NOT overwrite the session store:
	// doing so would leave the session's refresh token minting access tokens for the OLD workspace
	// again on the very next auto-refresh, which is worse than leaving the session untouched. Only
	// `cloud-login` rebinds the session's workspace today.
	var newTok string
	err = sess.Do(ctx, func(token string) error {
		nt, serr := client.SwitchWorkspace(ctx, token, cf.workspaceID)
		newTok = nt
		return serr
	})
	if err != nil {
		return emitErr(exitErr, "cloud-switch-workspace: %v", err)
	}
	// #340: a BARE invocation must not drop a live bearer token into the caller's working directory
	// (one `git add -A` from inside a repo publishes it). Without an explicit --token-out the token goes
	// under the session directory (next to session.json), named for the workspace, absolute path
	// reported. An EXPLICIT --token-out (cf.tokenOutSet — the onboarding kit's deploy/compose/... path)
	// behaves exactly as before.
	var out string
	if cf.tokenOutSet && cf.tokenOut != "" {
		out = cf.tokenOut
		if err := os.WriteFile(out, []byte(newTok), 0o600); err != nil {
			return emitErr(exitErr, "cloud-switch-workspace: write token: %v", err)
		}
	} else {
		var derr error
		out, derr = defaultSwitchTokenPath(cf.workspaceID)
		if derr != nil {
			return emitErr(exitErr, "cloud-switch-workspace: %v", derr)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return emitErr(exitErr, "cloud-switch-workspace: create %s: %v", filepath.Dir(out), err)
		}
		if err := writeTokenOutFile(out, newTok); err != nil {
			return emitErr(exitErr, "cloud-switch-workspace: %v", err)
		}
	}
	emit(map[string]any{"switched": true, "workspace": cf.workspaceID, "token_file": out})
	return exitOK
}

// defaultSwitchTokenPath is where cloud-switch-workspace writes its token when --token-out is not
// given: <session dir>/cp-session.<workspace>.token, absolute. The session dir is whatever
// onboard.DefaultSessionPath resolves ($ARGUS_SESSION_FILE, else <UserConfigDir>/argus), so the token
// sits beside session.json and never in the working tree. The workspace id is reduced to a safe
// file-name fragment.
func defaultSwitchTokenPath(workspace string) (string, error) {
	sp, err := onboard.DefaultSessionPath()
	if err != nil {
		return "", err
	}
	frag := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, workspace)
	return filepath.Abs(filepath.Join(filepath.Dir(sp), "cp-session."+frag+".token"))
}

// cpWireReporter sets e.RunReporter when this process is CP-wired (D3: ALL runs report up) — shared
// by the M2.5 serve process and the R4 MERGED runner profile (CP-M3-125). LoadKey is load-only: in
// the merged profile Bootstrap has already created the identity, so the two-container load-order
// race of the old split cannot exist.
func cpWireReporter(e *toolcore.Env) {
	// D3 (R10): ALL RUNS REPORT UP. When this serve process is CP-wired (ARGUS_CP_URL + the executor's
	// federation env, injected by the byo-m3 overlay onto the SAME instance), a direct MCP runner__run
	// pushes its evidence-free tallies to the cloud ledger under its OWN run_id (no run_request_id). The
	// push is signed with the executor's machine identity, which both containers share on the results
	// volume (/results/identity.key) — LoadKey is load-only so we never mint a divergent identity; when the
	// key is absent yet (executor not up) the push is skipped, best-effort (results are local files first).
	if cpURL := os.Getenv("ARGUS_CP_URL"); cpURL != "" {
		inst := envOr("ARGUS_INSTANCE_ID", e.Instance)
		keyPath := envOr("ARGUS_IDENTITY_PATH", "results/"+inst+"/identity.key")
		e.RunReporter = func(rep *report.Report, runID, scope string, startedAt, finishedAt time.Time) {
			priv, kerr := runner.LoadKey(keyPath)
			if kerr != nil {
				emit(map[string]any{"report_up": "skipped", "reason": "machine identity not available yet", "run_id": runID})
				return
			}
			rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			deepLink := toolcore.RunDashboardURL(*e, runID) // E1: the run-scoped Grafana link travels up to the ledger row
			cl := runner.NewClient(cpURL, inst, priv)
			// F1 (CP-M3-III-62): the SAME outbox directory the executor's poll loop drains. This
			// process and the executor share the results volume by construction (R4 merged them onto
			// one workload), so a push that fails here is delivered by the next reachable poll.
			cl.Outbox = &runner.Outbox{
				Dir: runner.OutboxDir(e.ResultsRoot, inst),
				Log: func(f string, a ...any) { emit(map[string]any{"report_up": fmt.Sprintf(f, a...)}) },
			}
			if perr := cl.ReportUp(rctx, rep, runID, scope, "", deepLink, federation.Annotations{}, startedAt, finishedAt); perr != nil {
				emit(map[string]any{"report_up": "failed", "reason": perr.Error(), "run_id": runID})
				return
			}
			emit(map[string]any{"report_up": "ok", "run_id": runID, "instance_id": inst})
		}

		// U2: give the run a CATALOG source for its scenario set, so an executor with NO local
		// scenario directory can still run. This is what retires `kubectl cp` on k8s: /scenarios
		// there is a per-pod emptyDir, so at the min-3 axiom three pods have three different
		// empty dirs and there is nothing to copy into. A non-empty local dir still wins
		// (resolveScenarioDir), so the compose fix-loop is untouched and pays no CP round-trip.
		//
		// Wiring this ALSO closes a false green: with no fetcher, a run over an empty directory
		// could not tell "this instance has no scenarios" from "the set never arrived", and it
		// reported the second as passed.
		e.SetFetcher = func(ctx context.Context, layer, tag, scenarioID string) ([]toolcore.CatalogScenario, error) {
			priv, kerr := runner.LoadKey(keyPath)
			if kerr != nil {
				return nil, fmt.Errorf("machine identity not available: %w", kerr)
			}
			sel := federation.Selection{Layer: layer, Tag: tag, ScenarioRef: scenarioID}
			resp, ferr := runner.NewClient(cpURL, inst, priv).FetchSet(ctx, runScopeOf(sel), sel)
			if ferr != nil {
				return nil, catalogErr(ferr)
			}
			out := make([]toolcore.CatalogScenario, 0, len(resp.Scenarios))
			for _, sc := range resp.Scenarios {
				out = append(out, toolcore.CatalogScenario{Path: sc.Path, Body: sc.Body})
			}
			return out, nil
		}
		// VR-F6: same identity, same client, the hash-only question. This is the one the in-env
		// runner__run path asks on EVERY run, so it must stay cheap.
		e.SetHasher = func(ctx context.Context, layer, tag, scenarioID string) (string, error) {
			priv, kerr := runner.LoadKey(keyPath)
			if kerr != nil {
				return "", fmt.Errorf("machine identity not available: %w", kerr)
			}
			sel := federation.Selection{Layer: layer, Tag: tag, ScenarioRef: scenarioID}
			h, herr := runner.NewClient(cpURL, inst, priv).FetchSetHash(ctx, runScopeOf(sel), sel)
			return h, catalogErr(herr)
		}
	}
}

// catalogErr translates the federation client's REFUSED-credential sentinel into toolcore's, so a
// direct run can tell "the control plane is down" from "the control plane said no" (VR-F6).
//
// They need different answers and look identical at the call site: an outage is temporary and the
// operator's own scenario files are a reasonable stand-in, while a withdrawn authorisation is not
// temporary and running anyway would be running without the permission that was just refused.
//
// The translation lives HERE because toolcore deliberately does not import the federation client —
// the fetcher is injected as a plain func to keep that dependency out — so this is the one place
// that can see both sides.
func catalogErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, runner.ErrCatalogUnauthorized) {
		return fmt.Errorf("%w: %v", toolcore.ErrCatalogUnauthorized, err)
	}
	return err
}

// referencedVarRe matches a ${NAME} placeholder. Braces are REQUIRED, mirroring config's own
// rule — a bare $ in a password or URL is not a reference and must not be treated as one.
var referencedVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// referencedVars returns the distinct ${VAR} names a config text references, in first-seen order
// so the caller's behaviour is deterministic. Used to collect ONLY those names from the
// environment: sweeping the whole environment into a k8s Secret would carry unrelated host
// secrets into the cluster.
func referencedVars(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range referencedVarRe.FindAllStringSubmatch(text, -1) {
		if name := m[1]; !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// readEnvFile parses a KEY=VALUE .env. Blank lines and # comments are skipped, surrounding
// quotes are stripped, and an "export " prefix is tolerated because real .env files carry one.
// A missing path is NOT an error: most SUTs reference no ${VAR} at all.
func readEnvFile(path string) (map[string]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		// Splitting on \n leaves a trailing \r on a CRLF .env; TrimSpace removes it.
		v = trimMatchingQuotes(strings.TrimSpace(v))
		if k != "" {
			out[k] = v
		}
	}
	return out, nil
}

// trimMatchingQuotes strips ONE layer of matching surrounding quotes, as a .env writer commonly
// adds around a value containing spaces. A lone quote is left alone rather than guessed at.
func trimMatchingQuotes(v string) string {
	if len(v) < 2 {
		return v
	}
	if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}

// sutIdentity is the SUT name the control plane groups instances by on the Environments page.
// It mirrors the compose onboarder exactly: deploy.compose_project when declared, else the
// project label. Any divergence splits one SUT into two cards, one per tier.
func sutIdentity(c *config.Config) string {
	if p := strings.TrimSpace(c.SUTProject()); p != "" {
		return p
	}
	return c.ProjectLabel()
}

// runScopeOf classifies a selection for the CP materialize call, matching the request_run scope
// vocabulary: an explicit scenario id → "single", else a tag → "tag", else a layer → "layer",
// else "full". The scope and the selection must agree or the CP materializes the wrong set.
func runScopeOf(sel federation.Selection) string {
	switch {
	case sel.ScenarioRef != "":
		return "single"
	case sel.Tag != "":
		return "tag"
	case sel.Layer != "":
		return "layer"
	default:
		return "full"
	}
}

// versionRefusal returns the message to refuse a run with, or "" to allow it (VR-V4).
//
// It names THREE things, because a refusal that names fewer is a puzzle: what this executor is
// running, what it must be running, and what to do about it. The remedy is deliberately the same
// sentence the Environments page gives — updating an executor is always a person's act (owner ruling),
// and two different remedies for one situation is how an operator ends up trying both.
func versionRefusal() string {
	floors := runner.NewFloorStore(envOr("ARGUS_IDENTITY_PATH", "results/"+envOr("ARGUS_INSTANCE_ID", "local")+"/identity.key")).Get()
	return versionRefusalFor(buildinfo.Resolve(os.Getenv("ARGUS_VERSION")), floors)
}

// versionRefusalFor is the pure core: given what is running and what is published, either the refusal
// text or "" to allow the run. Split out from the environment so the MESSAGE — which is the part an
// operator actually consumes — can be asserted without a filesystem or a control plane.
func versionRefusalFor(running string, floors federation.VersionFloors) string {
	if !federation.FloorBlocks(federation.FloorState(running, floors)) {
		return ""
	}
	return fmt.Sprintf(
		"this executor is BELOW the minimum version this control plane supports, so it will not run scenarios: "+
			"running %s, minimum required %s. Update it before running: open the control plane's Environments page "+
			"and use the update button (or, on compose, copy the command shown there and run it where you onboarded). "+
			"Nothing is wrong with your scenarios or your token.",
		running, floors.Absolute)
}

// cpWireFence installs the W1 RunBeginner/RunDone hooks on srv when CP-wired (§D-3.1.2/UC188) —
// shared by serve and the merged runner profile.
func cpWireFence(srv *mcpserver.Server, e toolcore.Env) {
	// Refuse a run that would execute NOTHING, before a run_id is minted. This is NOT part of the
	// CP fence below and is deliberately wired unconditionally: "there is nothing to run" is true
	// on a standalone rig too, and runner__run is async, so without this the agent gets a run_id
	// and has to infer the problem from a later total:0 report.
	srv.RunPreflight = func(layer, tag, scenarioID string) error {
		// VR-V4 (V17-010): an executor below the ABSOLUTE floor is refused work AT CALL TIME, before
		// anything else is checked. This hook exists precisely so a refusal reaches the agent as an
		// answer rather than as a run that returns total:0 and reads like a clean pass.
		//
		// THE FLOORS COME FROM THE EXECUTOR'S DURABLE COPY (VR-V5), not from a live control-plane
		// call. That is what makes this work on the LOCAL fast loop and with the control plane
		// unreachable — the two situations where "we could not check, so carry on" used to be the
		// answer. Never told anything → FloorUnknown → FloorBlocks is false → work proceeds.
		if msg := versionRefusal(); msg != "" {
			return errors.New(msg)
		}
		return toolcore.PreflightRun(e, layer, tag, scenarioID)
	}

	// The W1 direct-path fence (§D-3.1.2/UC188, CP-M3-115): when CP-wired, runner__run acquires the
	// CP instance_run_lock BEFORE the run (RunBeginner) and a graceful infra failure releases it via
	// a minimal terminal 'failed' push (RunAborter). Policy: a real 409 → REFUSE (surface *busy*);
	// a transport failure / missing identity → proceed OFFLINE (the documented CP-partition residual;
	// the local runActive lock is the second line) with an honest emit.
	if cpURL := os.Getenv("ARGUS_CP_URL"); cpURL != "" {
		inst := envOr("ARGUS_INSTANCE_ID", e.Instance)
		keyPath := envOr("ARGUS_IDENTITY_PATH", "results/"+inst+"/identity.key")
		// One in-flight run at a time (the local runActive lock guarantees it), so a single
		// heartbeat slot suffices: RunBeginner arms it on a successful CP begin; RunDone stops it.
		var hbMu sync.Mutex
		var hbStop func() // nil = no ticker armed
		stopHeartbeat := func() {
			hbMu.Lock()
			if hbStop != nil {
				hbStop()
				hbStop = nil
			}
			hbMu.Unlock()
		}
		srv.RunBeginner = func(runID, scope string) error {
			priv, kerr := runner.LoadKey(keyPath)
			if kerr != nil {
				emit(map[string]any{"run_begin": "skipped", "reason": "machine identity not available yet", "run_id": runID})
				return nil // offline residual — proceed on the local lock
			}
			c := runner.NewClient(cpURL, inst, priv)
			bctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			berr := c.BeginRun(bctx, runID, scope)
			switch {
			case berr == nil:
				emit(map[string]any{"run_begin": "ok", "run_id": runID, "instance_id": inst})
				// W1/3: arm the mid-run heartbeat so a long run is never false-reaped and a hard
				// crash of this process is detected within one watchdog cutoff.
				started := time.Now().UTC()
				hctx, hcancel := context.WithCancel(context.Background())
				hdone := make(chan struct{})
				go func() {
					defer close(hdone)
					t := time.NewTicker(30 * time.Second)
					defer t.Stop()
					for {
						select {
						case <-hctx.Done():
							return
						case <-t.C:
							pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Second)
							_ = c.PushHeartbeat(pctx, runID, "", scope, started)
							pcancel()
						}
					}
				}()
				hbMu.Lock()
				hbStop = func() { hcancel(); <-hdone } // JOIN the in-flight tick (gate-117: a straggler heartbeat must never trail the terminal push)
				hbMu.Unlock()
				return nil
			case errors.Is(berr, runner.ErrCPBusy):
				emit(map[string]any{"run_begin": "busy", "run_id": runID, "instance_id": inst})
				return berr // REFUSE — a run is already in flight on the CP fence
			default:
				emit(map[string]any{"run_begin": "unreachable — proceeding offline (local lock is the second line)", "reason": berr.Error(), "run_id": runID})
				return nil
			}
		}
		srv.RunDone = func(runID string, ok bool) {
			stopHeartbeat()
			if ok {
				return // the run's own terminal report-up push released the fence
			}
			priv, kerr := runner.LoadKey(keyPath)
			if kerr != nil {
				return
			}
			actx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			now := time.Now().UTC()
			abort := federation.ResultsPush{RunID: runID, Scope: "full", Status: "failed", StartedAt: &now, FinishedAt: &now}
			if perr := runner.NewClient(cpURL, inst, priv).Push(actx, abort); perr != nil {
				emit(map[string]any{"run_abort": "push failed — the watchdog reaps the stale run on heartbeat staleness", "reason": perr.Error(), "run_id": runID})
				return
			}
			emit(map[string]any{"run_abort": "released", "run_id": runID})
		}
	}
}

// cmdRouter runs the machine's LOCAL MCP ROUTER (VR-R1..R13): one loopback endpoint that routes by
// instance_id, so an agent folder can drive many SUTs while holding only a router token.
//
// It is deliberately thin. The routing table (who may reach what, with which credential) is
// internal/router; the protocol is internal/mcpserver; the upstream calls are internal/mcp. This
// function binds them together and binds a socket.
const routerUsage = "usage: argus router serve|wire|unwire|register|unrecord|status|folders|author-token|last-instance"

func cmdRouter(args []string) int {
	if len(args) == 0 {
		return emitErr(exitUsage, routerUsage)
	}
	if isHelpToken(args[0]) {
		fmt.Println(routerUsage)
		return exitOK
	}
	switch args[0] {
	case "wire":
		return cmdRouterWire(args[1:])
	case "unwire":
		return cmdRouterUnwire(args[1:])
	case "register":
		return cmdRouterRegister(args[1:])
	case "unrecord":
		return cmdRouterUnrecord(args[1:])
	case "status":
		return cmdRouterStatus(args[1:])
	case "folders":
		return cmdRouterFolders(args[1:])
	// VR9-H2 / SA §1.4.10 — the two facts teardown must read BEFORE it unwires anything. Neither was
	// obtainable: `router status` redacts the token to a boolean, and `router folders` prints bare
	// paths with no instance list. See cmd/argus/router_teardown_reads.go.
	case "author-token":
		return cmdRouterAuthorToken(args[1:])
	case "last-instance":
		return cmdRouterLastInstance(args[1:])
	case "serve":
	default:
		return emitErr(exitUsage, "router: unknown subcommand %q (serve|wire|unwire|register|unrecord|status|folders|author-token|last-instance)", args[0])
	}
	fs := flag.NewFlagSet("router serve", flag.ContinueOnError)
	stateDir := fs.String("state", router.StateDir(), "where the router keeps its folder table + port")
	port := fs.Int("port", 0, "bind this port instead of the sticky one (diagnostics only)")
	if err := fs.Parse(args[1:]); err != nil {
		return emitErr(exitUsage, "flag error: %v", err)
	}

	st, err := router.LoadState(*stateDir)
	if err != nil {
		return emitErr(exitErr, "%v", err)
	}
	tbl, err := router.TableFrom(st)
	if err != nil {
		return emitErr(exitErr, "%v", err)
	}

	chosen := *port
	overridden := *port != 0
	if chosen == 0 {
		chosen, err = router.PickPort(st.Port, nil)
		if err != nil {
			return emitErr(exitErr, "%v", err)
		}
	}
	// Persist the port BEFORE serving, so a restart reclaims it and every .mcp.json naming it stays
	// valid (VR-R3). Writing it after a successful bind would lose it on a crash during startup.
	//
	// An EXPLICIT --port is never persisted. The flag is a diagnostic ("serve somewhere else for a
	// minute"), and persisting it would rewrite the sticky port that every agent folder's .mcp.json
	// already names — turning a temporary look-at-it into a machine-wide reconfiguration nobody asked
	// for, discovered later by agents that can no longer connect.
	if !overridden && chosen != st.Port {
		st.Port = chosen
		if err := router.SaveState(*stateDir, st); err != nil {
			return emitErr(exitErr, "could not persist the router port: %v", err)
		}
	}

	// VR-R1: when the router runs as a container, the `localhost:<port>` URLs onboarding recorded name
	// the ROUTER's own container, not the executor — every forwarded call would loop back into the
	// router. The alias is DECLARED (the compose unit sets it), never detected: a router that guessed
	// whether it was containerised would be wrong exactly once, quietly, on someone else's machine.
	alias := router.HostAliasFromEnv()
	srv, err := mcpserver.NewServerWithAuth(router.Authenticator(tbl), router.Tools(tbl, router.MCPForwarder{HostAlias: alias})...)
	if err != nil {
		return emitErr(exitErr, "router: %v", err)
	}

	// VR-R10: report last-seen to the control plane, in the BACKGROUND and never blocking. The
	// router's job is routing; a control plane that cannot be reached must not take this machine's
	// agents down with it, so heartbeat errors are logged and dropped.
	id, ierr := router.LoadOrCreateIdentity(*stateDir)
	if ierr != nil {
		return emitErr(exitErr, "%v", ierr)
	}
	if cp := os.Getenv("ARGUS_CP_URL"); cp != "" {
		// V27-009 redesign: ONE heartbeat loop PER RECORD (control plane × user). A machine that holds no record
		// yet — a fresh install, or a 0.3.28 state with no cloud entries — beats once with the env's control
		// plane and no owner, which the control plane resolves when the machine has exactly one registration.
		host := router.HostName()
		version := buildinfo.Resolve(os.Getenv("ARGUS_VERSION"))
		stopAll := runRecordHeartbeats(routerWiring{CP: cp, Host: host, Version: version, Port: chosen, StateDir: *stateDir, Emit: emit, Boot: id})
		defer stopAll()
	}

	// VR-R9: pick up folders wired AFTER this process started. Without it, onboarding had to be
	// followed by a router restart or the freshly wired agent was told "unrecognized router token" —
	// a message that blames the credential, which was the one correct thing. During the cutover, where
	// seven instances are re-onboarded against a router that must stay up, that would fire six times.
	// A reload that cannot be loaded or is refused KEEPS the current table; it never empties it.
	//
	// ⛔ 0.3.29 SHIPPED WITHOUT THIS CALL (dropped by the V27-009 merge, commit 09722d4) and the first throwaway
	// onboard after the router had already moved was "unrecognized router token" for BOTH new folders until the
	// router was restarted — measured 2026-09-06, register row V29-001. TestRouterServe_ReloadsItsStateWhileServing
	// asserts the call is here; internal/router/reload_test.go asserts the mechanism behind it.
	go router.WatchState(*stateDir, tbl, router.ReloadInterval, nil, nil,
		func(n int) { emit(map[string]any{"router": "reloaded", "folders": n}) },
		func(err error) { emit(map[string]any{"warn": err.Error()}) })

	addr := router.BindAddr(chosen, os.Getenv(router.BindEnv))
	// Redacted, never the state: the folder table holds a bearer token per folder per instance.
	emit(map[string]any{
		// reachable_at is what an AGENT should use; addr is what the process bound. In a container
		// those differ, and printing only the bind address is what made the first containerised
		// router look correct while nothing could reach it.
		"router": "serving", "addr": addr, "reachable_at": router.ListenAddr(chosen), "state_dir": *stateDir,
		"folders": len(st.Folders), "protocolVersion": mcpserver.ProtocolVersion,
		"routing": router.Redacted(st),
		// The PUBLIC half only — this is what onboarding posts to /api/routers/register. The private
		// key never leaves this machine.
		"router_id": id.RouterID, "pubkey_b64": id.PublicKeyB64(),
	})
	// VR8-K2 (V26-003): the router answers "can I serve?" honestly on /ready.
	//
	// hadFolders is captured HERE, from the state this process loaded at startup, and never
	// recomputed. A probe that re-derived it would conclude that a router which just lost
	// everything never had anything — the exact confusion being fixed.
	//
	// ⛔ /healthz is deliberately NOT affected. Liveness means "the process is up", which stays
	// true; Kubernetes kills a pod that fails liveness and a restart cannot recreate a deleted
	// host directory, so wiring this there would produce an unbounded crash-loop.
	rh := mcpserver.NewHTTPHandler(srv)
	rh.SetReady(router.StateReadiness(*stateDir, len(st.Folders) > 0))
	// #607: bind FIRST, then say which port was bound. Onboarding used to read the port out
	// of state.json and believe whatever answered /ready there — another router on that port included. Only
	// after net.Listen succeeded does this process know it holds the port, so that is when it writes
	// {pid, port} to the file onboarding named (router.BoundFileEnv; unset = nothing is written).
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return emitErr(exitErr, "router serve: %v", err)
	}
	if bf := os.Getenv(router.BoundFileEnv); bf != "" {
		bound, perr := router.ListenerPort(ln)
		if perr == nil {
			perr = router.WriteBoundFile(bf, os.Getpid(), bound)
		}
		if perr != nil {
			// Not fatal: the router serves either way. Onboarding, which asked for the file, reports that it never came.
			emit(map[string]any{"warn": fmt.Sprintf("could not write the bound-port file %s: %v", bf, perr)})
		}
	}
	if err := http.Serve(ln, rh); err != nil {
		return emitErr(exitErr, "router serve: %v", err)
	}
	return exitOK
}

func cmdServe(e toolcore.Env, authCfg auth.Config, cf *commonFlags) int {
	// FX-1: the dashboard deep link is for a HUMAN browser, so resolve the host-facing
	// Grafana base from config (default http://localhost:3000) — never the in-container
	// --grafana addr (grafana:3000) the obs stack uses internally.
	// VR-E8: serving REFUSES nothing here — the executor must keep running tests — but a tier that
	// cannot be resolved yields NO link rather than a localhost one. Onboarding is where this
	// condition stops the operator.
	resolveTierGrafana(&e)
	cpWireReporter(&e)
	srv, err := mcpserver.NewServer(authCfg, mcpserver.DefaultTools(e)...)
	if err != nil {
		return emitErr(exitErr, "server: %v", err)
	}
	cpWireFence(srv, e)
	emit(map[string]any{
		"serving": true, "addr": cf.addr, "transport": "http+sse", "protocolVersion": mcpserver.ProtocolVersion,
		"endpoints": []string{"GET /sse", "POST /message?sessionId=", "GET /metrics"}, "tools": 6,
		"report_up": os.Getenv("ARGUS_CP_URL") != "",
	})
	if err := http.ListenAndServe(cf.addr, mcpserver.NewHTTPHandler(srv)); err != nil {
		return emitErr(exitErr, "serve: %v", err)
	}
	return exitOK
}

// cmdServeMode runs the binary in an M3 deploy profile — control | runner (D-EXEC.2.8). Each profile
// carries its own auth model (control: OAuth 2.1 + Clerk; runner: Ed25519 federation), so the M2.5
// CLI token pair does NOT gate it. This is the I.0 SKELETON: bootstrap the service-anatomy floor and
// block until SIGINT/SIGTERM, then shut down gracefully (plan §5.2 I.0).
func cmdServeMode(mode string, cf *commonFlags) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The M3 anatomy floor is :8080; only honour --addr if the caller overrode the M2.5 serve default.
	addr := cf.addr
	if addr == ":8765" {
		addr = ""
	}

	switch mode {
	case "control":
		return emitErr(exitUsage, "serve --mode control is not part of this build: the OneDroid Argus control plane is a hosted service, and this repository contains the runner only")
	case "runner":
		// ARGUS_CP_URL engages the federation executor (register-on-start + the poll loop); absent
		// → the I.0 health-only skeleton. The SUT run path reuses the CLI's config/results/jmeter flags.
		// VR-E8 / TS-F6: resolve the browser-facing Grafana base ONCE, BEFORE anything consumes it.
		// Both the federated ExecConfig below and the MCP env further down must carry the SAME value —
		// they used to be computed separately, and the ExecConfig copy took the raw --grafana flag
		// (the compose default) so every deep_link this executor STORED pointed at localhost on a
		// managed tier. See tierGrafana.
		grafanaBase := tierGrafana(cf)
		rcfg := runner.Config{Addr: addr, Log: func(f string, a ...any) { emit(map[string]any{"executor": fmt.Sprintf(f, a...)}) }}
		if cpURL := os.Getenv("ARGUS_CP_URL"); cpURL != "" {
			inst := envOr("ARGUS_INSTANCE_ID", "local")
			rcfg.Fed = runner.FedConfig{
				CPURL: cpURL, InstanceID: inst, WorkspaceID: os.Getenv("ARGUS_WORKSPACE_ID"),
				IdentityPath: envOr("ARGUS_IDENTITY_PATH", "results/"+inst+"/identity.key"),
				// WireTier: the CP validates the three canonical tiers (compose|k3d|managed), while the
				// onboarder speaks the concrete cluster flavour (--tier aks). Normalize HERE so a new
				// flavour never needs a control-plane redeploy to be registerable.
				SUTName: envOr("ARGUS_SUT_NAME", "sut"), Tier: federation.WireTier(envOr("ARGUS_TIER", "compose")),
				RunnerVersion: buildinfo.Resolve(os.Getenv("ARGUS_VERSION")),
				Enrollment:    os.Getenv("ARGUS_ENROLLMENT_TOKEN"), // S1 (CP-M3-120)
				// AC-16: the transport switch. "" or "fed" (default) keeps CPURL as the direct
				// /fed/* target; "mcp" makes ARGUS_CP_MCP_URL a hub the client calls as
				// tools/call executor__<verb>, bearing ARGUS_EXECUTOR_HUB_TOKEN plus the machine JWT.
				Transport: os.Getenv("ARGUS_CP_TRANSPORT"),
				MCPURL:    os.Getenv("ARGUS_CP_MCP_URL"),
				HubToken:  os.Getenv("ARGUS_EXECUTOR_HUB_TOKEN"),
				Exec: runner.ExecConfig{
					// R5-3: InstanceID is the TELEMETRY/registration id; ToolInstance is the identity the
					// merged MCP surface reads results under (cf.instance, "local") — a federated run must
					// write its report THERE or runner__get_report cannot see a cloud run's evidence.
					InstanceID: inst, ToolInstance: cf.instance,
					// AC-17: the SAME scenarios directory the merged process's own local router runs
					// against (env(cf) below) — a relayed builder run/validate_config executes through
					// toolcore exactly as the in-env product hat does, never a second scenario source.
					ScenariosDir: cf.scenariosDir,
					ConfigPath:   cf.configPath, ResultsRoot: cf.resultsRoot,
					ComposeFile: cf.composeFile, JMeterLocal: cf.jmeterMode == "local", TemplatesDir: cf.templatesDir,
					// grafanaBase, NOT cf.grafana: this is the value that lands in every STORED deep_link.
					Grafana: grafanaBase, Loki: cf.loki, LokiTenant: cf.lokiTenant, Pushgateway: cf.pushgateway,
					Cluster: os.Getenv("ARGUS_CLUSTER"),                          // R6/ADR-10
					Tier:    federation.WireTier(envOr("ARGUS_TIER", "compose")), // VR-E8; matches the registered tier above
					// the --obs mode the instance was onboarded with. The relayed
					// validate_config answers through THIS config, so without it an --obs none
					// instance's own executor refused its config for a missing Grafana URL.
					ObsMode: os.Getenv("ARGUS_OBS_MODE"),
				},
			}
		}
		// R4 (§D-3.1.1/ADR-11, CP-M3-125): the MERGED per-instance workload — when the in-env MCP
		// token pair is configured, this ONE process also serves the 6-tool runner__* surface
		// (/sse, /message, /mcp, /metrics on the same listener), with the same CP wiring the M2.5
		// serve process gets (report-up + the W1 fence + heartbeats). One process owns the
		// identity: Bootstrap creates the key BEFORE the surface serves — the old two-container
		// load-order race cannot exist.
		mcpAuth := auth.Config{RunnerToken: os.Getenv("ARGUS_RUNNER_TOKEN"), AuthorToken: envname.Lookup(envname.ExecutorSecret, envname.ExecutorSecretDeprecated)}
		if mcpAuth.RunnerToken != "" || mcpAuth.AuthorToken != "" {
			e := env(cf)
			e.Grafana = grafanaBase // the SAME value the ExecConfig above carries — one source, not two
			cpWireReporter(&e)
			msrv, merr := mcpserver.NewServer(mcpAuth, mcpserver.DefaultTools(e)...)
			if merr != nil {
				return emitErr(exitErr, "runner mcp: %v", merr)
			}
			cpWireFence(msrv, e)
			rcfg.MCP = mcpserver.NewHTTPHandler(msrv)
		}
		srv, err := runner.Bootstrap(ctx, rcfg)
		if err != nil {
			return emitErr(exitErr, "%v", err)
		}
		emit(map[string]any{"serving": true, "mode": "runner", "addr": srv.Addr(),
			"federation": rcfg.Fed.CPURL != "", "mcp_surface": rcfg.MCP != nil, "federationProtocol": federation.ProtocolVersion})
		if err := srv.Run(ctx); err != nil {
			return emitErr(exitErr, "runner: %v", err)
		}
		return exitOK
	default:
		return emitErr(exitUsage, "unknown mode %q (want control | runner)", mode)
	}
}

// cmdRenderObs renders the bundled obs-stack configs (promtail + prometheus) PER SUT from
// argus-config.yaml's argus-owned block (RO-05/07/08) and writes them next to the compose
// file, so the onboarder parameterizes the obs stack for the colleague's separate compose
// project/network instead of the hardcoded order-service. Runner-scope (build-time helper).
func cmdRenderObs(e toolcore.Env, cf *commonFlags) int {
	c, err := config.Load(e.ConfigPath)
	if err != nil {
		return emitErr(exitErr, "load config: %v", err)
	}
	dir := filepath.Dir(cf.composeFile)
	promtail := filepath.Join(dir, "promtail-config.yaml")
	prometheus := filepath.Join(dir, "prometheus.yml")
	// obs instance = the REGISTERED id when set (ARGUS_INSTANCE_ID → e.ObsInstance), else the tool
	// identity (default "local"). Threads the real id into promtail/prometheus argus_instance so the SUT's
	// logs + metrics carry the SAME label the dashboard filters by (fixes "No data" saga/logs panels).
	obsInst := e.ObsInstance
	if obsInst == "" {
		obsInst = e.Instance
	}
	if err := os.WriteFile(promtail, []byte(obsconfig.RenderPromtail(c, obsInst)), 0o644); err != nil {
		return emitErr(exitErr, "write promtail: %v", err)
	}
	if err := os.WriteFile(prometheus, []byte(obsconfig.RenderPrometheus(c, obsInst)), 0o644); err != nil {
		return emitErr(exitErr, "write prometheus: %v", err)
	}
	// RO-08: parameterize the dashboard's errors-view level matcher from the config (no-op
	// for the default (?i)error|warn). Best-effort: a missing dashboard is not fatal.
	dashPath := filepath.Join(dir, "grafana", "dashboards", "argus-overview.json")
	dashRendered := false
	if db, derr := os.ReadFile(dashPath); derr == nil {
		if werr := os.WriteFile(dashPath, obsconfig.RenderDashboard(db, c), 0o644); werr == nil {
			dashRendered = true
		}
	}
	emit(map[string]any{"rendered": true, "promtail": promtail, "prometheus": prometheus,
		"dashboard": dashRendered, "sut_project": c.SUTProject(), "sut_network": c.SUTNetwork()})
	return exitOK
}

// renderK8sBuild is everything cmdRenderK8s derives from its flags, environment and config BEFORE it
// renders a byte: the k8srender.Instance, the loaded SUT config, the storage class the operator named,
// and the PodMonitor auto-detection note. `argus upgrade` renders through exactly this, so the fresh
// render it compares against is built from the SAME inputs as `render-k8s`, not from a copy of them.
type renderK8sBuild struct {
	in             k8srender.Instance
	c              *config.Config
	storageClass   string
	podMonitorNote string
}

// renderK8sInput turns the common flags (+ env + the SUT config) into the render input, refusing every
// input render-k8s refuses. It returns (nil, exit code) after emitting the error, exactly as the
// inline code did. kubeconfig is used ONLY for the read-only PodMonitor CRD probe (--podmonitor=auto):
// "" for render-k8s (unchanged), the caller's --kubeconfig for `upgrade`.
func renderK8sInput(e toolcore.Env, cf *commonFlags, kubeconfig string) (*renderK8sBuild, int) {
	// Refuse a tier the control plane will refuse, BEFORE anything is written. The rendered executor
	// registers with WireTier(--tier); an unknown name passes through it unchanged and is rejected at
	// RegisterInstance only after the namespace, the executor and its Secret are up — and a non-admin
	// operator cannot delete that namespace again. Empty is left alone: it defaults to k3d below.
	if cf.tier != "" && !federation.RegistrableTier(cf.tier) {
		return nil, emitErr(exitErr, "--tier %q would render, deploy, and then be refused at registration: %s",
			cf.tier, federation.RegistrableTierHint)
	}
	// T2.2: refuse an unrecognised --results-access-mode HERE, before any file is read or written —
	// the same "refuse before touching anything" rule the tier check above already follows. Empty
	// (not given) is left alone; it is resolved below, once we know whether --storage-class was given.
	storageClass := strings.TrimSpace(cf.storageClass)
	accessMode := strings.TrimSpace(cf.resultsAccessMode)
	switch accessMode {
	case "", "ReadWriteMany", "ReadWriteOnce":
	default:
		return nil, emitErr(exitUsage, "--results-access-mode %q — must be ReadWriteMany or ReadWriteOnce", cf.resultsAccessMode)
	}
	// Same "refuse before touching anything" rule for --podmonitor.
	podmonitorMode := strings.ToLower(strings.TrimSpace(cf.podmonitor))
	switch podmonitorMode {
	case "auto", "on", "off":
	default:
		return nil, emitErr(exitUsage, "--podmonitor %q — must be auto, on, or off", cf.podmonitor)
	}
	// --storage-class with no explicit mode defaults to ReadWriteMany: naming a class is asking for
	// SHARED storage, and a class that cannot do RWX leaves the PVC visibly Pending — a loud failure
	// — rather than silently pinning the min-3 replicas to one node the way an unstated RWO would.
	if storageClass != "" && accessMode == "" {
		accessMode = "ReadWriteMany"
	}
	// The SUT's ${VAR} values, read BEFORE config.Load — which hard-fails on an unresolved ${VAR}
	// (D1). Exporting them here means the render VALIDATES the config exactly as a run would,
	// instead of skipping validation for every SUT that uses a secret. The values go into the
	// instance Secret; the ConfigMap keeps the config TEMPLATED, so no secret is ever written
	// next to the config.
	secretEnv, serr := readEnvFile(cf.secretsEnvFile)
	if serr != nil {
		return nil, emitErr(exitErr, "read --secrets-env-file: %v", serr)
	}
	if secretEnv == nil {
		secretEnv = map[string]string{} // no --secrets-env-file: the env sweep below still fills it
	}
	for k, v := range secretEnv {
		if os.Getenv(k) == "" { // an explicit export from the caller wins
			_ = os.Setenv(k, v)
		}
	}
	raw, err := os.ReadFile(e.ConfigPath)
	if err != nil {
		return nil, emitErr(exitErr, "read config: %v", err)
	}
	// ALSO take the ${VAR}s from the PROCESS ENVIRONMENT. The onboarder already injects the SUT's
	// secrets as env (its docker wrapper uses --env-file), so requiring a second, mounted copy of
	// the same secrets just to render would be both redundant and a new place for them to leak.
	// Only names the config actually REFERENCES are collected — never the whole environment, which
	// would sweep unrelated host secrets into a k8s Secret.
	names := referencedVars(string(raw))
	// check_env: a declared name is written WITHOUT ${} in the config text, so the scan
	// above cannot see it. Take the names from the parsed config instead, so a declared name reaches the
	// Secret exactly like a credential reference. A config that does not parse is left to config.Load
	// below, which refuses it with the real reason.
	if declared, perr := config.ParseUnresolved(e.ConfigPath); perr == nil {
		names = append(names, declared.CheckEnvNames()...)
	}
	for _, name := range names {
		if _, have := secretEnv[name]; have {
			continue
		}
		if v := os.Getenv(name); v != "" {
			secretEnv[name] = v
		}
	}
	c, err := config.Load(e.ConfigPath)
	if err != nil {
		return nil, emitErr(exitErr, "load config: %v", err)
	}
	obsMode := strings.ToLower(strings.TrimSpace(cf.obsMode))
	// T3.1 (E3 export mode): the bare ${VAR} NAME behind whichever export credential is active —
	// observability.loki.credential for a hosted-Loki target, observability.betterstack.credential
	// for a BetterStack target — read from an UNRESOLVED parse so this is a NAME, never a value.
	// Excluded from the generic secretEnv sweep below: unlike every other SUT ${VAR} (which DOES
	// travel into the executor's exec-tokens Secret, in plain stringData), the T3.1 design is
	// stricter for this ONE credential — "render must not emit the Secret object [holding it]" —
	// because it is wired instead via a secretKeyRef onboard.sh points at a Secret it creates
	// directly from the operator's environment, OUTSIDE render output (obsCredentialSecretName).
	var obsCredVarName string
	if obsMode == "export" {
		if unresolved, uerr := config.ParseUnresolved(e.ConfigPath); uerr == nil {
			for _, ref := range unresolved.EnvRefs() {
				switch ref.Field {
				case "observability.loki.credential", "observability.betterstack.credential":
					obsCredVarName = ref.Name
				}
			}
		}
		if obsCredVarName != "" {
			delete(secretEnv, obsCredVarName)
		}
	}
	// Derive the ExternalName aliases from the config's OWN declared targets, so a compose-era
	// argus-config (which names services by bare hostname) resolves unchanged inside the
	// executor namespace. An explicit --external-alias overrides a derived one.
	aliases := k8srender.DeriveAliases(c, cf.sutNamespace)
	for _, kv := range strings.Split(cf.externalAlias, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, emitErr(exitErr, "--external-alias %q is not bare=fqdn", kv)
		}
		aliases[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	cpURL := cf.controlPlane
	if cpURL == "" {
		cpURL = os.Getenv("ARGUS_CP_URL")
	}
	// T7.2: refused here, loudly, because the CP would drop it SILENTLY (updatecmd.SafeHostPath on register
	// and poll) and the instance would register as legacy — the very outcome the flag exists to prevent.
	kubeCtx := renderKubeContext(cf)
	if !updatecmd.SafeHostPath(kubeCtx) {
		return nil, emitErr(exitUsage, "render-k8s: --kube-context / ARGUS_KUBE_CONTEXT_HOST contains a quote or a control character; "+
			"the control plane would drop it and `argus update` could not target this instance")
	}
	// --podmonitor: resolve to the PromOperatorLabel value k8srender already understands ("none" =
	// omit the object entirely — see k8srender.obsPodMonitor). auto tries a read-only `kubectl get
	// crd` against --kube-context when one was given; with none given it cannot look, so it keeps
	// today's default (include it) and says so, because a managed cluster with no Prometheus
	// Operator would otherwise fail `kubectl create` on the object with no warning anywhere.
	promLabel := os.Getenv("ARGUS_OBS_PROM_LABEL")
	var podMonitorNote string
	switch podmonitorMode {
	case "off":
		promLabel = "none"
	case "on":
		if strings.EqualFold(strings.TrimSpace(promLabel), "none") {
			promLabel = "" // an explicit --podmonitor=on overrides an env-level opt-out
		}
	case "auto":
		if !strings.EqualFold(strings.TrimSpace(promLabel), "none") {
			if kubeCtx == "" {
				podMonitorNote = "podmonitor: no --kube-context given, so this cluster could not be checked for the Prometheus Operator — including the PodMonitor in obs.yaml by default; if this cluster has none, `kubectl create` on obs.yaml will fail (pass --podmonitor=off to drop it, or --kube-context to auto-detect)"
			} else if has, perr := hasPodMonitorCRD(kubeCtx, kubeconfig); kubectlNotExecutable(perr) {
				// AC-D57 (#391): kubectl is not on PATH where render-k8s runs — the normal case inside the
				// executor image, which ships none. Say exactly that; it is not a cluster or API failure.
				podMonitorNote = fmt.Sprintf("podmonitor: kubectl is not available where render-k8s runs, so the cluster was not checked (kube-context %q) — including the PodMonitor in obs.yaml by default; pass --podmonitor=off if this cluster has no Prometheus Operator, or --podmonitor=on to silence this (onboard.sh checks on the host and passes one of them whenever the host can answer — if it could not, it printed why just above)", kubeCtx)
			} else if perr != nil {
				podMonitorNote = fmt.Sprintf("podmonitor: could not check kube-context %q for the podmonitors.monitoring.coreos.com CRD (%v) — including the PodMonitor in obs.yaml anyway; pass --podmonitor=off if this cluster has no Prometheus Operator", kubeCtx, perr)
			} else if !has {
				promLabel = "none"
				podMonitorNote = fmt.Sprintf("podmonitor: no podmonitors.monitoring.coreos.com CRD found on kube-context %q — omitting the PodMonitor from obs.yaml (pass --podmonitor=on to force it)", kubeCtx)
			}
		}
	}
	in := k8srender.Instance{
		ID:           cf.instance,
		SUTNamespace: cf.sutNamespace,
		Image:        cf.image,
		CPURL:        cpURL,
		WorkspaceID:  os.Getenv("ARGUS_WORKSPACE_ID"),
		// The SUT identity the CP GROUPS BY on the Environments page. It must match what the
		// compose path registers (onboard.sh 8b: SUT_NAME=${SUT_PROJECT}, i.e. deploy.compose_project),
		// or the SAME SUT appears as two separate cards — one per tier — instead of one card whose
		// tier columns fill in. Found live: social-compose registered as "acme-social-mcp" while
		// social-k3d registered as "social" (its project.name), splitting the SUT in the UI.
		// OrderService hid the bug only because its two names happen to be identical.
		SUTName: sutIdentity(c),
		Tier:    cf.tier,
		Cluster: cf.tier,
		// INT-001: the RAW override, empty unless the operator set one — NOT Resolve(), which would
		// substitute this KIT binary's stamp and outrank the executor's own at runtime. See
		// buildinfo.RenderedOverride and k8srender.go:174-177, which already stated this expectation.
		Version: buildinfo.RenderedOverride(os.Getenv("ARGUS_VERSION")),
		// Supplied by the caller's environment — never minted here, never echoed back.
		RunnerToken: os.Getenv("ARGUS_RUNNER_TOKEN"),
		AuthorToken: envname.Lookup(envname.ExecutorSecret, envname.ExecutorSecretDeprecated),
		// S1 (III.3): the single-use enrollment credential onboarding minted from POST /api/enrollments.
		// Env, not a flag, for the same reason as the token pair — a credential in argv is visible in ps.
		// Empty (the common case, and every pre-S1 onboarding) omits the Secret key entirely.
		EnrollmentToken: os.Getenv("ARGUS_ENROLLMENT_TOKEN"),
		// UC138: the environment's browser-facing Grafana base, so a managed deep link is openable.
		GrafanaPublicURL: os.Getenv("ARGUS_GRAFANA_PUBLIC_URL"),
		// INT-015: the label a kube-prometheus-stack Prometheus demands on a PodMonitor before it will
		// read it, as "key=value". Onboarding discovers the cluster's real value from the Prometheus
		// CR's serviceMonitorSelector; empty falls back to release=kube-prometheus-stack. Without the
		// PodMonitor this renders, NOTHING scrapes a managed instance's pushgateway and every metric
		// panel on its dashboard is empty forever, beside a Loki datasource that works.
		PromOperatorLabel: promLabel,
		// U7 (CP-M3-III-80): the host folders + onboarding machine, so the k8s tiers report them the
		// same way compose already did. Env, not flags, matching every other optional render input.
		ProductDir:  os.Getenv("ARGUS_PRODUCT_DIR_HOST"),
		KitDir:      os.Getenv("ARGUS_KIT_DIR_HOST"),
		TestDir:     os.Getenv("ARGUS_TEST_DIR_HOST"),
		OnboardHost: os.Getenv("ARGUS_ONBOARD_HOST"),
		// VR10-U1 (V28-020): the cluster onboarding is pointing at, by the same route and for the
		// same reason — a pod cannot derive it, and without it the update block names no cluster.
		KubeContext:    kubeCtx,
		Kubeconfig:     os.Getenv("ARGUS_KUBECONFIG_HOST"),
		IdentityKeyB64: os.Getenv("ARGUS_IDENTITY_KEY_B64"), // G4/S4: onboarding-minted key → exec-identity Secret
		// VR5-U2 (V19-005): the LOCAL-tier image-pull secret. Set by onboarding ONLY once it has
		// actually created the Secret; empty otherwise, and the emptiness is load-bearing.
		// internal/k8srender records, on the executor Deployment itself, which registry THIS field's
		// secret was made for (the registry of the image being rendered here) — a
		// registryhost.PullSecretRegistriesAnnotation annotation, not a Secret read. V19-006 first
		// closed the "referencing SOME secret allows ANY registry" gap by having
		// internal/runner/updateguard.go read the Secret's own `auths` keys over the API; V19-007
		// withdrew that (it required granting the executor `get` on its own pull-secret credential,
		// which a test-workload runner should never hold, even key-names-only) and moved the decision
		// to this render-time annotation instead — the guard now decides registry coverage from the
		// Deployment it already has RBAC for, no Secret named or fetched. BECAUSE the guard never
		// reads the Secret, it cannot tell whether the Secret exists: guardImageObtainable refuses
		// only when the pod spec references NO pull secret, or when the request moves to a registry
		// the annotation does not list. Setting this variable for a Secret that was never created
		// (or cannot be filled) DOES flip a same-registry update from "refuse" to "allow", and the
		// executor can then be re-imaged to an image it cannot pull. Export it only when the Secret
		// really exists in the namespace. (normalize() still defaults aks to `ghcr-pull` — this
		// only ever ADDS one to a local tier.)
		ImagePullSecret: os.Getenv("ARGUS_IMAGE_PULL_SECRET"),
		ArgusConfig:     string(raw),
		// ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28 §1: every declared `path:` message
		// schema's bytes, embedded into the instance ConfigMap and mounted back at its own
		// relative path (internal/k8srender). c is already config.Load'd above (line ~2945), which
		// is itself what makes a MISSING declared file fail render-k8s BEFORE this point is ever
		// reached — MessageSchemaDecl.validate (internal/config/schemas.go) os.ReadFile's every
		// `path:` at load time and names the schema + path in its error.
		MessageSchemaFiles: c.MessageSchemaFiles(),
		Replicas:           cf.replicas,
		ExternalAliases:    aliases,
		SecretEnv:          secretEnv,
		// T2.2: empty (both flags left unset) reproduces normalize()'s own tier defaults exactly, so
		// this pair is byte-identical to before whenever neither flag is given — see the accessMode
		// resolution above.
		StorageClass: storageClass,
		AccessMode:   accessMode,
		// the observability volumes' class; empty -> obsStorageClassFor(tier).
		ObsStorageClass: strings.TrimSpace(cf.obsStorageClass),
		// U9: source ${WEBHOOK_URL} from the SUT's external.webhook_base_url (FQDN-rewritten) so the
		// config is the truth, not a hardcoded compose default.
		WebhookURL: k8srender.WebhookURLFor(c, cf.sutNamespace),
		// T3.1/T3.2 (E3): the switch, and — for adopt — the operator's own endpoints read straight
		// from the config. ObsLokiURL is deliberately LokiURLConfigured() (no bundled-default
		// fallback): an adopt run with no declared url must refuse (k8srender.Instance.validate()),
		// never silently point at the bundled loki:3100 it never deploys. ObsPushgatewayURL has no
		// such requirement — it is optional in adopt mode (see config.PushgatewayURL).
		ObsMode:           obsMode,
		ObsLokiURL:        c.LokiURLConfigured(),
		ObsPushgatewayURL: c.PushgatewayURL(),
		// T3.1 (E3 export mode): the hosted-Loki push side + the credential var NAME (never the
		// value — see obsCredVarName above) + whether this SUT targets BetterStack instead. All
		// three are ignored by k8srender outside export mode.
		ObsLokiPushURL:       c.LokiPushURLConfigured(),
		ObsCredentialVarName: obsCredVarName,
		ObsUseBetterStack:    c.UseBetterStack(),
		// T3.3 (E3 shared ingest): the environment's shared Loki; the tenant is the instance id.
		ObsSharedURL: strings.TrimSpace(cf.obsSharedURL),
		// SUT-namespace log collection is opt-in (default false) — see cmd.collectSUTLogs's flag help.
		CollectSUTLogs: cf.collectSUTLogs,
	}
	return &renderK8sBuild{in: in, c: c, storageClass: storageClass, podMonitorNote: podMonitorNote}, exitOK
}

// cmdRenderK8s renders the PER-INSTANCE k8s manifests (U1, Stage II) from the instance
// descriptor + the operator's own argus-config.yaml. This is what makes the k8s tier generic:
// the onboarder renders and applies, so no step ever needs a hand-edited manifest.
//
// PRE-AUTH by design, exactly like render-obs: it is a pure local transform of files the
// operator already has, run BEFORE the instance exists. The tokens it embeds are supplied by
// the caller (env), never minted here and never echoed back.
func cmdRenderK8s(e toolcore.Env, cf *commonFlags) int {
	rb, rc := renderK8sInput(e, cf, "")
	if rb == nil {
		return rc
	}
	in, c, storageClass, podMonitorNote := rb.in, rb.c, rb.storageClass, rb.podMonitorNote
	execYAML, err := k8srender.RenderExecutor(in)
	if err != nil {
		return emitErr(exitErr, "render executor: %v", err)
	}
	obsYAML, err := k8srender.RenderObs(in, c)
	if err != nil {
		return emitErr(exitErr, "render obs: %v", err)
	}
	// P3 #23: refused BEFORE any file is written, same posture as the tier/access-mode checks above
	// — --emit-sut-access-role with no --sut-namespace has nothing to target.
	var sutAccessYAML string
	if cf.emitSUTAccessRole {
		if cf.sutNamespace == "" {
			return emitErr(exitUsage, "--emit-sut-access-role requires --sut-namespace")
		}
		sutAccessYAML = k8srender.SUTAccessRoleManifest(in)
	}
	// Said out loud AT RENDER TIME, not left to be discovered later in a rendered YAML: which
	// namespace's pod logs this render will collect, whenever it collects any at all.
	if in.CollectSUTLogs {
		fmt.Fprintf(os.Stderr, "collect-sut-logs: ON — promtail will read every pod log under namespace %q (SUT_NAMESPACE)\n", in.SUTNamespace)
	}
	if podMonitorNote != "" {
		fmt.Fprintln(os.Stderr, podMonitorNote)
	}
	if cf.outDir == "" {
		fmt.Print(execYAML, "\n---\n", obsYAML)
		if sutAccessYAML != "" {
			fmt.Print("\n---\n", sutAccessYAML)
		}
		return exitOK
	}
	if err := os.MkdirAll(cf.outDir, 0o755); err != nil {
		return emitErr(exitErr, "mkdir %s: %v", cf.outDir, err)
	}
	execPath := filepath.Join(cf.outDir, "executor.yaml")
	obsPath := filepath.Join(cf.outDir, "obs.yaml")
	// 0600: executor.yaml carries the token Secret.
	if err := os.WriteFile(execPath, []byte(execYAML), 0o600); err != nil {
		return emitErr(exitErr, "write executor manifest: %v", err)
	}
	if err := os.WriteFile(obsPath, []byte(obsYAML), 0o644); err != nil {
		return emitErr(exitErr, "write obs manifest: %v", err)
	}
	// The single most consequential fact about this render, said on stderr so it survives even
	// when stdout (this command's JSON) is piped or parsed and never read by a human: executor.yaml
	// carries the exec-tokens Secret's values IN CLEAR (stringData). renderNextCommand already
	// steers every caller to `kubectl create` (never `apply`, which would copy it a second time into
	// last-applied-configuration) — this says the rest: where the credentials sit on disk right now,
	// and that the file must be deleted once `kubectl create` has run, and never committed.
	fmt.Fprintf(os.Stderr, "SECURITY: %s contains the exec-tokens Secret's credentials IN CLEAR (plain stringData, not encrypted) — delete this file after `kubectl create` succeeds, and never commit it.\n", execPath)
	var sutAccessPath string
	if sutAccessYAML != "" {
		// 0644, unlike executor.yaml: this manifest carries no Secret — only RBAC rules and names.
		sutAccessPath = filepath.Join(cf.outDir, "sut-access-role.yaml")
		if err := os.WriteFile(sutAccessPath, []byte(sutAccessYAML), 0o644); err != nil {
			return emitErr(exitErr, "write sut access role manifest: %v", err)
		}
	}
	// T2.2: what normalize() actually resolved StorageClass/AccessMode to — an explicit override, or
	// the tier default when neither flag was given. ResolvedStorage delegates to normalize() rather
	// than re-deriving the tier mapping a second time here (see its doc comment).
	resolvedClass, resolvedMode := in.ResolvedStorage()
	out := map[string]any{
		"rendered": true, "instance_id": in.ID, "namespace": in.Namespace(),
		"executor": execPath, "obs": obsPath, "tier": in.Tier, "replicas": in.Replicas,
		"next":                renderNextCommand(execPath, obsPath, in.KubeContext),
		"kube_context":        in.KubeContext,
		"obs_mode":            in.ObsMode,
		"storage_class":       resolvedClass,
		"results_access_mode": resolvedMode,
		// "" = the claims name NO class: the cluster's default StorageClass binds them (every tier but aks
		// without --obs-storage-class). Never a made-up name.
		"obs_storage_class": in.ResolvedObsStorageClass(),
		// P3 #23: present ONLY with --emit-sut-access-role — the caller must forward this file to
		// the SUT owner; render-k8s never applies it itself.
	}
	// T3.1/T3.2: onboard.sh's Grafana-datasource step (the k3d/aks obs-wiring block) has no other
	// way to learn the operator's configured Loki/Pushgateway — it is a shell script with no YAML
	// parser, and the config was already loaded and validated HERE. Emitted in adopt AND export
	// mode: bundled onboarding already knows its own instance-scoped Loki/Pushgateway addresses.
	if in.ObsMode == "adopt" || in.ObsMode == "export" {
		if in.ObsLokiURL != "" { // adopt: always set (validate() refuses otherwise). export+BetterStack: "".
			out["obs_loki_url"] = in.ObsLokiURL
		}
		if in.ObsPushgatewayURL != "" {
			out["obs_pushgateway_url"] = in.ObsPushgatewayURL
		}
	}
	// T3.1: onboard.sh needs the credential VAR NAME (never the value) to create the
	// argus-obs-credential Secret from its own environment — render never emits the Secret itself.
	if in.ObsCredentialVarName != "" {
		out["obs_credential_var"] = in.ObsCredentialVarName
	}
	// T3.3: onboard.sh's Grafana datasource carries the tenant as X-Scope-OrgID.
	if in.ObsMode == "shared" {
		out["obs_shared_url"] = in.ObsSharedURL
		out["obs_tenant"] = in.ID
	}
	if sutAccessPath != "" {
		out["sut_access_role"] = sutAccessPath
	}
	var warnings []string
	if in.KubeContext == "" {
		warnings = append(warnings, "no kube context recorded on this instance: `argus update` will treat it as legacy and "+
			"print a <KUBE_CONTEXT> placeholder instead of targeting its cluster. Re-render with --kube-context <ctx>.")
	}
	// T2.2: the gap this ticket closes — a tier that is neither aks nor a local one (k3d/kind/
	// minikube) always got node-local storage, and #215 caps a ReadWriteOnce results volume at 1
	// executor replica, silently losing the min-3 availability axiom. Say so LOUDLY, at render time,
	// rather than let an operator discover it as "why does this instance only have 1 pod?" later.
	// No warning at all on aks/k3d/kind — those already default to an RWX class.
	if storageClass == "" && !k8srender.LocalTier(in.Tier) && !strings.EqualFold(strings.TrimSpace(in.Tier), "aks") {
		warnings = append(warnings, fmt.Sprintf(
			"no --storage-class given on tier %q: the results volume will be node-local ReadWriteOnce, "+
				"so the executor is capped at 1 replica (#215/ARGUS_RESULTS_ACCESS_MODE) — pass --storage-class <an RWX class> to keep 3.",
			in.Tier))
	}
	if len(warnings) > 0 {
		out["warning"] = strings.Join(warnings, " ")
	}
	emit(out)
	return exitOK
}

// sharedTenantGuard refuses --obs shared with no Loki tenant (T3.3). A shared Loki runs
// auth_enabled: true, so an unscoped request is refused there anyway; refusing at START instead
// turns a stream of silent empty evidence reads into one message naming the missing setting.
// Every other mode passes with no tenant: that is today's behaviour, header absent.
func sharedTenantGuard(obsMode, tenant string) error {
	if !strings.EqualFold(strings.TrimSpace(obsMode), "shared") {
		return nil
	}
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("--obs shared needs a Loki tenant (--loki-tenant or ARGUS_LOKI_TENANT, the instance id) — refusing to read or write the shared Loki unscoped")
	}
	return nil
}

// cmdRenderObsShared renders the environment's ONE shared Loki (T3.3) into <out>/obs-shared.yaml,
// or to stdout with no --out. The managed Grafana's namespace (the NetworkPolicy's Grafana peer)
// is ARGUS_CP_NAMESPACE, the same variable onboard.sh already uses to find that Grafana.
func cmdRenderObsShared(cf *commonFlags) int {
	y, err := k8srender.RenderSharedLoki(k8srender.SharedLoki{
		Tier:             cf.tier,
		GrafanaNamespace: os.Getenv("ARGUS_CP_NAMESPACE"),
		StorageClass:     strings.TrimSpace(cf.obsStorageClass),
	})
	if err != nil {
		return emitErr(exitErr, "render-obs-shared: %v", err)
	}
	if cf.outDir == "" {
		fmt.Print(y)
		return exitOK
	}
	if err := os.MkdirAll(cf.outDir, 0o755); err != nil {
		return emitErr(exitErr, "mkdir %s: %v", cf.outDir, err)
	}
	p := filepath.Join(cf.outDir, "obs-shared.yaml")
	if err := os.WriteFile(p, []byte(y), 0o644); err != nil {
		return emitErr(exitErr, "write shared obs manifest: %v", err)
	}
	emit(map[string]any{"rendered": true, "namespace": k8srender.SharedObsNamespace, "path": p,
		"shared_url": k8srender.SharedLokiInClusterURL, "tier": cf.tier})
	return exitOK
}

// renderKubeContext is the kube context render-k8s records: the flag, else onboarding's env var.
func renderKubeContext(cf *commonFlags) string {
	if kc := strings.TrimSpace(cf.kubeContext); kc != "" {
		return kc
	}
	return strings.TrimSpace(os.Getenv("ARGUS_KUBE_CONTEXT_HOST"))
}

// podMonitorCRDName is the CRD a kube-prometheus-stack Prometheus Operator installs for PodMonitor
// (INT-015 already renders one on every non-local tier); --podmonitor=auto checks for it before
// deciding whether obs.yaml can safely carry that object at all.
const podMonitorCRDName = "podmonitors.monitoring.coreos.com"

// hasPodMonitorCRD reports whether the CRD is installed on kubeContext, via a READ-ONLY
// `kubectl [--kubeconfig <file>] [--context <ctx>] get crd <name> -o name` — through runKubectl, the
// runner `argus upgrade` and `argus secrets` use, rather than a second copy of "shell out to kubectl
// and read its stderr". A distinct return is kept for "the probe could not run at all" (kubectl
// missing, no reachable API server, …) versus "the probe ran and the CRD is not there": only the
// latter should ever flip a render's default from including the object to omitting it silently.
func hasPodMonitorCRD(kubeContext, kubeconfig string) (bool, error) {
	// The same runner and the same flag placement as every other kubectl call `argus upgrade` and
	// `argus secrets` make (secretsCommonFlags.kubectlArgs: --kubeconfig, then --context, before the
	// verb), so a test that asserts "every kubectl call carries them" sees this probe too.
	sf := secretsCommonFlags{kubeconfig: kubeconfig, kubeContext: kubeContext}
	out, err := runKubectl(nil, sf.kubectlArgs("get", "crd", podMonitorCRDName, "-o", "name")...)
	if err == nil {
		return strings.TrimSpace(out) != "", nil
	}
	// kubectl RAN and the API server answered "no such object" — the standard k8s reason string,
	// distinct from "kubectl could not even be executed" (a different error shape from run(), see
	// preflight.go) or a network/auth failure (which carries neither this string nor a clean exit).
	if strings.Contains(err.Error(), "(NotFound)") {
		return false, nil
	}
	return false, err
}

// renderNextCommand is the literal next step render-k8s prints, and an agent will run it verbatim.
//
// ⛔ IT USED TO SAY `kubectl apply`, which is the one command docs/DEPLOY-ARGUS.md §4.3 calls "the
// single most consequential line on this page": executor.yaml carries the exec-tokens Secret, and
// `apply` copies a Secret's data into the last-applied-configuration annotation, where anything that
// can read the object sees every token a second time. The file this function names is written 0600
// four lines above precisely because it holds that Secret. Found 2026-09-23 by a fresh-agent run of
// the door, which obeyed the door over the tool — the next one might not.
//
// ⛔ AND IT ALWAYS CARRIES A `--context`. With --kube-context (T7.2) it is the context the operator
// named; without one it is a PLACEHOLDER, never omitted: a bare `kubectl …` would run against whatever
// context is current, which is how a deploy lands on the wrong cluster. A placeholder makes the command
// refuse to run until someone chooses the cluster on purpose.
//
// A real context is pasted into a shell, so anything beyond the plain kubeconfig-name characters is
// single-quoted (safe: render-k8s has already refused a context holding a quote).
func renderNextCommand(execPath, obsPath, kubeContext string) string {
	ctx := kubeContext
	switch {
	case ctx == "":
		ctx = "<kube-context>"
	case strings.IndexFunc(ctx, notPlainContextRune) >= 0:
		ctx = "'" + ctx + "'"
	}
	return "kubectl --context " + ctx + " create -f " + execPath + " -f " + obsPath
}

func notPlainContextRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("-_.:@/", r)
}

// cmdCapabilities prints the honest per-SUT capability/declared-gap report (CHANGE-4),
// derived from argus-config. Runner-scope onboarding helper (CLI-only).
func cmdCapabilities(e toolcore.Env) int {
	p, err := toolcore.Capabilities(e)
	if err != nil {
		return emitErr(exitErr, "%v", err)
	}
	emit(p)
	return exitOK
}

// cmdOnboardGuard enforces the dark-factory holdout's filesystem boundary (D4) at onboarding
// time: the product folder (dir of argus-config.yaml) and the scenarios folder must be distinct,
// non-overlapping, and no scenario .md may sit under the product folder. The onboarder refuses
// to proceed otherwise (Claude Code's native Read/Glob are not token-gated).
// cmdSecretsScan reports which ${VAR} names the argus-config references in the D2-parity
// credential/wiring fields — the onboarder's COLLECT-stage secrets preflight uses it to know
// which names to look up in the PRODUCT folder's .env. It parses WITHOUT resolving (its whole
// job is to run before the values exist, where Load would D1-fail) and it never reads the
// environment or prints a value.
func cmdSecretsScan(e toolcore.Env) int {
	c, err := config.ParseUnresolved(e.ConfigPath)
	if err != nil {
		return emitErr(exitErr, "parse config: %v", err)
	}
	refs := c.EnvRefs()
	emit(map[string]any{"config": e.ConfigPath, "count": len(refs), "referenced": refs})
	return exitOK
}

func cmdOnboardGuard(e toolcore.Env) int {
	productDir := filepath.Dir(e.ConfigPath)
	if err := onboard.FolderGuard(productDir, e.ScenariosDir); err != nil {
		return emitErr(exitDenied, "holdout guard: %v", err)
	}
	emit(map[string]any{"holdout_guard": "ok", "product_dir": productDir, "scenarios_dir": e.ScenariosDir})
	return exitOK
}

// cmdMCPCall tests a SUT's OWN MCP tool (D3). Runner-scope (both hats); the SUT bearer is
// --mcp-token else targets.mcp.auth.bearer_token from the config (distinct from the Argus
// tokens; the legacy MCP_TOKEN env source is retired). Exit is 0 on protocol-level completion;
// a tool isError is reported in output, not folded in.
func cmdMCPCall(cf *commonFlags) int {
	// CHANGE-1: default the endpoint/transport/token from argus-config targets.mcp when the
	// flags are unset, so `mcp-call` resolves the SUT purely from the config like a scenario
	// does (an explicit --server-url / --transport / --mcp-token still wins). MCP_TOKEN env is
	// retired as a source.
	serverURL, transport, token := cf.serverURL, cf.transport, cf.mcpToken
	// CHANGE-1: default endpoint/transport/token from targets.mcp when the flags are unset.
	// config.Load already resolves any ${VAR} in the token (D1/D2 — SECRETS-VAR-PARITY), so use
	// the resolved value directly; a second os.ExpandEnv would re-mangle a literal '$' in a
	// resolved secret. And if the config is the source of the endpoint but fails to load — e.g. a
	// referenced ${VAR} is unset (D1) — surface it rather than silently calling the SUT with an
	// empty endpoint/token. An explicit --server-url still lets the caller drive purely by flags.
	c, lerr := config.Load(cf.configPath)
	if lerr != nil && serverURL == "" {
		return emitErr(exitErr, "load config: %v", lerr)
	}
	if lerr == nil {
		if serverURL == "" {
			serverURL = c.MCPBaseURL()
		}
		if transport == "" || transport == "streamable-http" { // flag default → adopt the SUT's declared transport
			transport = c.MCPTransport()
		}
		if token == "" {
			token = c.MCPToken() // already ${VAR}-resolved by config.Load (no second expansion)
		}
	}
	if serverURL == "" || cf.tool == "" {
		return emitErr(exitUsage, "mcp-call needs --tool and an endpoint (--server-url or targets.mcp.base_url in the config)")
	}
	args, err := parseArgs(cf.argsRaw)
	if err != nil {
		return emitErr(exitUsage, "args: %v", err)
	}
	code := 0
	if cf.expectCodeStr != "" {
		fmt.Sscanf(cf.expectCodeStr, "%d", &code)
	}
	// GAP-3: honour the SUT's declared targets.mcp.timeout_seconds (zero when the config did
	// not load and the caller drove purely by flags → the 30s default).
	var mcpTimeout time.Duration
	if lerr == nil {
		mcpTimeout = c.MCPTimeout()
	}
	p, err := toolcore.MCPCall(serverURL, transport, cf.tool, args, cf.requestID, cf.expectPlane, code, token, mcpTimeout)
	if err != nil {
		return emitErr(exitErr, "%v", err)
	}
	emit(p)
	return exitOK
}

// cmdPreflightAuth live-checks that the SUT ACCEPTS the declared MCP bearer token BEFORE any run,
// so a stale/unseeded token surfaces as ONE clear early error instead of every scenario failing
// Unauthorized (the Social gateway's -32001). Read-only: initialize + tools/list only, never
// tools/call — safe against effectful SUTs / live keys. Skips cleanly for an HTTP SUT (no
// targets.mcp) or MCP auth "none". Must run on the SUT's docker network to reach the endpoint.
func cmdPreflightAuth(cf *commonFlags) int {
	c, err := config.Load(cf.configPath)
	if err != nil {
		return emitErr(exitErr, "preflight-auth: load config: %v", err)
	}
	// verdict is emitted to stdout in EVERY case so onboard.sh can branch on it (not on the exit code);
	// the loud human message goes to stderr for the fatal cases. Never prints the token value.
	if c.Targets.MCP == nil || c.MCPBaseURL() == "" {
		emit(map[string]any{"verdict": "skipped", "reason": "no MCP endpoint (targets.mcp.base_url) declared — not an MCP SUT / nothing to auth-preflight"})
		return exitOK
	}
	if c.MCPAuthType() == "none" {
		emit(map[string]any{"verdict": "skipped", "reason": "MCP auth type is 'none' — no bearer token to preflight"})
		return exitOK
	}
	if c.MCPToken() == "" {
		// auth is bearer but the token is EMPTY (a literal empty bearer_token — a ${VAR} would already
		// have hard-failed config.Load). Do NOT silently skip the very check meant to catch this.
		emit(map[string]any{"verdict": "config_error", "endpoint": c.MCPBaseURL(),
			"detail": "MCP auth.type is 'bearer' but targets.mcp.auth.bearer_token is EMPTY"})
		fmt.Fprintf(os.Stderr, "\nSUT AUTH PREFLIGHT: MCP auth.type is 'bearer' but the token is EMPTY "+
			"(targets.mcp.auth.bearer_token).\n  Set it (or use auth.type: none for a keyless SUT). "+
			"Every scenario would otherwise fail Unauthorized.\n\n")
		return exitErr
	}
	cl := &mcp.Client{ServerURL: c.MCPBaseURL(), Transport: mcp.Transport(c.MCPTransport()), Token: c.MCPToken(), Timeout: c.MCPTimeout()}
	res := cl.Preflight()
	switch {
	case res.OK:
		emit(map[string]any{"verdict": "ok", "endpoint": c.MCPBaseURL(), "transport": c.MCPTransport(),
			"detail": "the SUT accepted the token (initialize + tools/list succeeded)"})
		return exitOK
	case res.AuthRejected:
		// LOUD, single, actionable message to stderr — NEVER prints the token value.
		emit(map[string]any{"verdict": "auth_rejected", "endpoint": c.MCPBaseURL(), "detail": res.Detail})
		fmt.Fprintf(os.Stderr, "\n==================================================================\n"+
			" SUT AUTH PREFLIGHT FAILED — the SUT rejected your MCP token.\n"+
			"==================================================================\n"+
			"  endpoint:  %s\n  transport: %s\n  auth:      bearer\n  reason:    %s\n\n"+
			"  Every scenario would fail Unauthorized. Fix the token, then re-run onboarding:\n"+
			"    - confirm the SUT is SEEDED (its token store holds this token).\n"+
			"        e.g. Social MCP: `docker compose --profile seed run --rm seed`  (or `make seed`)\n"+
			"    - confirm targets.mcp.auth.bearer_token (the ${VAR} in the product .env)\n"+
			"      matches a token the SUT accepts.\n"+
			"  (validate-config only checks the ${VAR} is SET, not that the SUT accepts its\n"+
			"   value — that is exactly why this preflight exists.)\n"+
			"==================================================================\n\n",
			c.MCPBaseURL(), c.MCPTransport(), res.Detail)
		return exitErr
	case res.Forbidden:
		// 403 is ambiguous — do NOT tell the user to re-seed the token; name both possibilities.
		emit(map[string]any{"verdict": "forbidden", "endpoint": c.MCPBaseURL(), "detail": res.Detail})
		fmt.Fprintf(os.Stderr, "\n==================================================================\n"+
			" SUT AUTH PREFLIGHT FAILED — the SUT returned 403 Forbidden.\n"+
			"==================================================================\n"+
			"  endpoint:  %s\n  transport: %s\n  reason:    %s\n\n"+
			"  A run would hit the same 403. Two common causes:\n"+
			"    - the token lacks the required scope/permission for these tools, OR\n"+
			"    - the SUT is rejecting the request's Origin/proxy/policy (this probe sends\n"+
			"      no Origin header, exactly like the runner) — check any Origin/allowlist/proxy.\n"+
			"==================================================================\n\n",
			c.MCPBaseURL(), c.MCPTransport(), res.Detail)
		return exitErr
	case res.Unreachable:
		// NOT a token verdict — the gate could not establish anything. onboard.sh REFUSES on this
		// verdict (owner ruling 2026-08-07, CP-M3-III-82): a gate that could not run has NOT passed.
		// This comment used to say onboarding continues, which had been false since that ruling.
		emit(map[string]any{"verdict": "unreachable", "endpoint": c.MCPBaseURL(), "detail": res.Detail})
		fmt.Fprintf(os.Stderr, "\nSUT MCP endpoint UNREACHABLE at %s (transport %s): %s\n"+
			"  NOT a pass: onboarding REFUSES on an unconfirmed token — a gate that could not run has\n"+
			"  not passed. If this is a slow start, re-run onboarding: that is cheap, and an instance\n"+
			"  nobody verified is not.\n\n",
			c.MCPBaseURL(), c.MCPTransport(), res.Detail)
		return exitErr
	default:
		emit(map[string]any{"verdict": "inconclusive", "endpoint": c.MCPBaseURL(), "detail": res.Detail})
		fmt.Fprintf(os.Stderr, "\nSUT MCP preflight INCONCLUSIVE at %s: %s\n\n", c.MCPBaseURL(), res.Detail)
		return exitErr
	}
}

func parseArgs(raw string) (any, error) {
	if raw == "" {
		return map[string]any{}, nil
	}
	data := []byte(raw)
	if strings.HasPrefix(raw, "@") {
		b, err := os.ReadFile(raw[1:])
		if err != nil {
			return nil, err
		}
		data = b
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("invalid JSON args: %w", err)
	}
	return v, nil
}

// --- author (scenario-side) commands ---

func cmdValidateScenario(cf *commonFlags) int {
	if cf.file == "" {
		return emitErr(exitUsage, "validate-scenario needs --file")
	}
	b, err := os.ReadFile(cf.file)
	if err != nil {
		return emitErr(exitErr, "read %s: %v", cf.file, err)
	}
	// ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28 §8: with --config, apply the FULL per-record
	// schema check locally (the app's config is only ever available here, never on the control
	// plane) — refusing a record that does not fit its declared schema BY FIELD PATH before
	// anything runs. Without --config, behaviour is exactly what it was before this item
	// (toolcore.ValidateScenarioWithConfig with a nil *config.Config IS toolcore.ValidateScenario).
	var c *config.Config
	if cf.configPath != "" {
		c, err = config.Load(cf.configPath)
		if err != nil {
			return emitErr(exitErr, "load config: %v", err)
		}
	}
	p, failed, _ := toolcore.ValidateScenarioWithConfig(string(b), c)
	emit(p)
	return failedExit(failed)
}

func cmdProposeScenario(cf *commonFlags) int {
	p, err := toolcore.ProposeScenario(cf.from, cf.target, cf.layer) // DF-16: layer-aware skeleton
	if err != nil {
		return emitErr(exitUsage, "%v", err)
	}
	emit(p)
	return exitOK
}

func cmdListScenarios(e toolcore.Env) int {
	p, _ := toolcore.ListScenarios(e)
	emit(p)
	return exitOK
}

func cmdReadScenario(e toolcore.Env, cf *commonFlags) int {
	if cf.scenarioID == "" {
		return emitErr(exitUsage, "read-scenario needs --scenario")
	}
	p, err := toolcore.ReadScenario(e, cf.scenarioID)
	if err != nil {
		return emitErr(exitErr, "%v", err)
	}
	emit(p)
	return exitOK
}

func cmdWriteScenario(cf *commonFlags) int {
	if cf.path == "" || cf.file == "" {
		return emitErr(exitUsage, "write-scenario needs --file (source) and --path (dest)")
	}
	b, err := os.ReadFile(cf.file)
	if err != nil {
		return emitErr(exitErr, "read %s: %v", cf.file, err)
	}
	p, failed, err := toolcore.WriteScenario(env(cf).ScenariosDir, b, cf.path) // DF-05: resolve under --scenarios
	if err != nil {
		return emitErr(exitErr, "%v", err)
	}
	emit(p)
	return failedExit(failed)
}

func cmdDeleteScenario(e toolcore.Env, cf *commonFlags) int {
	if cf.scenarioID == "" {
		return emitErr(exitUsage, "delete-scenario needs --scenario")
	}
	p, err := toolcore.DeleteScenario(e, cf.scenarioID)
	if err != nil {
		return emitErr(exitErr, "%v", err)
	}
	emit(p)
	return exitOK
}

// startIdentityWatcher starts the goroutine that republishes the router's identity whenever
// identity.key changes on disk, and returns a stop function.
//
// ⛔ EXTRACTED SO THE CALL ITSELF CAN BE ASSERTED. Started inline, the whole goroutine could be
// deleted and every test still passed: the round's zero-interval test proves WatchIdentity's fallback
// works when it is called, and nothing proved anything called it. `router register` mints a new
// identity.key under this running process; without the watcher every later beat is signed with the
// start-up key and answered 401 in silence, which is V27-003 exactly.
func startIdentityWatcher(stateDir string, holder *router.IdentityHolder, emit func(any)) func() {
	stop := make(chan struct{})
	go router.WatchIdentity(stateDir, 0, stop, func(next router.Identity) {
		holder.Store(next)
		// Said out loud, in the voice of the existing `local router moved …` line: a silent swap would
		// leave an operator debugging 401s with no idea the key had changed underneath.
		emit(map[string]any{"router": "identity changed on disk — heartbeats now sign with the new key"})
	}, func(err error) {
		emit(map[string]any{"warn": "could not re-read the router identity", "reason": err.Error()})
	})
	var once sync.Once
	return func() { once.Do(func() { close(stop) }) }
}

// routerHeartbeatFor assembles the heartbeat that `router serve` runs on.
//
// ⛔ EXTRACTED SO THE ASSEMBLY ITSELF CAN BE ASSERTED. An adversary gate changed the HolderCount
// field to report st.TestFolderCount() — the router then published the WRONG PREDICATE as its holder
// count, the one number this whole round exists to get right — and the full suite stayed green at
// 1889 PASS / 0 FAIL. Every test named for holder counts passed.
//
// The cause is that nothing tested `router serve`'s WIRING: the tests reconstructed an equivalent
// heartbeat themselves and asserted on their own construction. Testing the two providers separately
// would not close it either — the defect is in WHICH provider is assigned to WHICH field. So the
// assembly is a function, and its test asserts on the struct this returns.
// routerWiring names what `router serve` hands to its heartbeat.
//
// ⛔ NAMED FIELDS, NOT SEVEN POSITIONAL ARGUMENTS. An adversary gate transposed two of them —
// `host` and `stateDir`, both plain strings — and the entire suite stayed green at 726 PASS. In
// production that reports the state-dir PATH as the hostname (which makes the upsert's ON CONFLICT
// arbiter miss, so the control plane answers 500 on every beat and the UI shows only "stale"),
// writes heartbeat-health.json somewhere `router status` will never look, and points the identity
// watcher at a directory holding no identity.key.
//
// ⚠ THIS MAKES THE MISTAKE VISIBLE, NOT IMPOSSIBLE. Four of these fields are strings, so the
// compiler still cannot separate them; what changes is that each value is LABELLED at the call
// site, where before it was a position in a list of seven. Stated plainly rather than claimed as a
// guarantee — the last commit claimed to have "removed the ability to get the wiring wrong" and
// this is the argument that was still open.
type routerWiring struct {
	CP       string
	Host     string
	Version  string
	Port     int
	StateDir string
	Emit     func(any)
	Boot     router.Identity
}

// heartbeatFor builds the heartbeat for ONE record (url, user). Its callbacks read and write that record only.
func heartbeatFor(w routerWiring, holder *router.IdentityHolder, recURL, user string) router.Heartbeat {
	stateDir, emit := w.StateDir, w.Emit
	hb := router.Heartbeat{CPURL: w.CP, Host: w.Host, Version: w.Version, Port: w.Port,
		StateDir: stateDir, RecordURL: recURL, Owner: user,
		OnRotatedToken: func(tok string) error {
			return applyRotatedToken(stateDir, recURL, user, tok, emit)
		},
		OnOwner: func(owner string) {
			// a lifted legacy record learns whose it is from the first accepted beat — written under the
			// same lock as every other record write, never as a bare load/save (F3 of the 0.3.29 gate)
			err := router.UpdateState(stateDir, func(st *router.State) error {
				st.PutRecord(router.CloudRecord{URL: recURL, User: owner})
				return nil
			})
			if err == nil {
				emit(map[string]any{"router": "record owner learned from the control plane", "control_plane": recURL, "owner": owner})
			}
		},
		HolderCount: func() int {
			st, err := router.LoadState(stateDir)
			if err != nil {
				return -1
			}
			return st.Holds(recURL, user)
		},
		TestFolderCount: func() int {
			st, err := router.LoadState(stateDir)
			if err != nil {
				return -1
			}
			return st.FoldersUsing(recURL, user)
		}}
	hb.Identity = holder.Load
	return hb
}

// runRecordHeartbeats starts one heartbeat loop per record and keeps the set in step with the state file: a
// record that appears later (a second user onboarding on this machine) gets a loop on the next tick; a record
// that disappears (teardown's `router unrecord`) has its loop stopped. Returns a stop function for all of them.
func runRecordHeartbeats(w routerWiring) func() {
	cp, stateDir, emit := w.CP, w.StateDir, w.Emit
	if strings.ContainsAny(w.Host, `/\`) {
		emit(map[string]any{"warn": "router host looks like a filesystem path — Host and StateDir may be " +
			"transposed; the control plane will answer 500 on every beat and the UI will show only " +
			"'stale'", "host": w.Host})
	}
	if stateDir != "" && !strings.ContainsAny(stateDir, `/\`) {
		emit(map[string]any{"warn": "router state dir has no path separator — Host and StateDir may be " +
			"transposed; identity.key and heartbeat-health.json will not be where anything looks for " +
			"them", "state_dir": stateDir})
	}
	holder := router.NewIdentityHolder(w.Boot)
	stopWatch := startIdentityWatcher(stateDir, holder, emit)
	var mu sync.Mutex // guards stops: the ticker goroutine and the returned stop closure both touch it
	stops := map[string]chan struct{}{}
	key := func(url, user string) string { return url + "\x00" + user }
	onErr := func(err error) { emit(map[string]any{"warn": "router heartbeat did not land", "reason": err.Error()}) }
	reconcile := func() {
		mu.Lock()
		defer mu.Unlock()
		st, err := router.LoadState(stateDir)
		if err != nil {
			return
		}
		want := map[string]router.CloudRecord{}
		for _, c := range st.Clouds {
			want[key(c.URL, c.User)] = c
		}
		if len(want) == 0 {
			// no record yet: one legacy-shaped loop against the env's control plane, owner unknown
			want[key(recordURL(cp), "")] = router.CloudRecord{URL: recordURL(cp)}
		}
		for k, c := range want {
			if _, running := stops[k]; running {
				continue
			}
			stop := make(chan struct{})
			stops[k] = stop
			go router.RunHeartbeat(heartbeatFor(w, holder, c.URL, c.User), stop, onErr)
		}
		for k, stop := range stops {
			if _, still := want[k]; !still {
				close(stop)
				delete(stops, k)
			}
		}
	}
	reconcile()
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(router.HeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				reconcile()
			}
		}
	}()
	return func() {
		close(done)
		mu.Lock()
		for k, stop := range stops {
			close(stop)
			delete(stops, k)
		}
		mu.Unlock()
		stopWatch()
	}
}

// parseExpiry reads the control plane's expires_at (RFC3339 or a bare date); zero when absent or unreadable.
func parseExpiry(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t
		}
	}
	return time.Time{}
}

func failedExit(failed bool) int {
	if failed {
		return exitFailed
	}
	return exitOK
}
