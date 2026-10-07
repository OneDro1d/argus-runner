// Package k8srender renders the PER-INSTANCE k8s manifests (executor + instance-scoped obs)
// from an instance descriptor + the SUT's own argus-config.yaml — the k8s analogue of what
// internal/obsconfig already does for the compose tier, and for the same reason.
//
// U1 (Stage II): `k8s/per-instance-template/{executor,obs}.yaml` were HAND-FILLED for the one
// concrete instance `orderservice-k3d` (23 + 17 hardcoded occurrences). They could not express
// a second instance, so every k3d bring-up so far needed a hand-edited `kubectl apply` — which
// the locked Stage II scope rules out: "if any gate step needed a hand-applied kubectl apply,
// the path is NOT done". This package makes the tier GENERIC: any compatible SUT onboards to
// k3d (or a managed cluster) with no manual edits, N instances side by side in one cluster.
//
// Two deliberate choices, both learned from the compose tier:
//
//   - The operator's argus-config.yaml is embedded VERBATIM. The old template synthesized
//     OrderService's config inline, which cannot serve a bring-your-own SUT.
//   - The promtail pipeline is rendered from the SUT's DECLARED log-field translation table
//     (log_format / level_field / correlation_field / saga_event_field / saga_event_values),
//     not the the operator defaults. Hardcoding a `json` stage silently extracts NOTHING from a logfmt
//     SUT (GAP-2), and normalizing only the first saga marker value leaves the dashboard's saga
//     panel partial (PROB-2).
package k8srender

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/obsconfig"
	"github.com/OneDro1d/argus-runner/internal/registryhost"
)

// Instance describes ONE registered argus instance's k8s footprint. Everything the old
// template hardcoded is a field here.
type Instance struct {
	ID           string // registered instance id, e.g. "orderservice-k3d" (DNS-1123 label)
	SUTNamespace string // namespace where the SUT's Services live
	Image        string // the onedroid-testing-suite image the executor runs
	CPURL        string // control-plane URL (in-cluster Service DNS, or external)
	WorkspaceID  string
	SUTName      string
	Tier         string // k3d | aks — the tier label reported to the CP
	Cluster      string
	Version      string
	// PromOperatorLabel (INT-015) is the label a kube-prometheus-stack Prometheus requires on a
	// PodMonitor before it will look at it, as "key=value". Empty ⇒ the documented default
	// "release=kube-prometheus-stack"; onboarding discovers the cluster's real value from the
	// Prometheus CR's serviceMonitorSelector and passes it here, so this is not hardcoded to one
	// cluster's convention. Used on every non-local tier — see obsPodMonitor. "none" renders no
	// PodMonitor, for a cluster without the Prometheus Operator.
	PromOperatorLabel string
	RunnerToken       string // gates ONLY the local runner__* MCP surface
	AuthorToken       string
	// EnrollmentToken (S1, CP-M3-120 + III.3) is the workspace-bound, single-use credential the
	// executor presents on its FIRST /fed/register when the CP runs with
	// ARGUS_FED_REGISTER_AUTH=enrollment. onboard.sh mints it from POST /api/enrollments and
	// passes it here; it is rendered into the exec-tokens Secret (NEVER the ConfigMap) because the
	// executor reads it from the environment via `envFrom: secretRef: exec-tokens`. Empty = the key
	// is OMITTED entirely — enrollment is opt-in, and a present-but-empty value would make the
	// executor present an empty credential and take a 403 that reads like a control-plane bug.
	EnrollmentToken string
	// IdentityKeyB64 (G4/S4), when set, is the base64 of the pre-minted Ed25519 machine key (from
	// `argus keygen`). It is rendered into the `exec-identity` Secret and mounted READ-ONLY at
	// /etc/argus/identity — so the key lives OFF the results volume. Empty = the legacy behaviour
	// (the first pod self-mints /results/identity.key on the shared PVC); kept for backward-compat.
	IdentityKeyB64 string
	ArgusConfig    string // the operator's argus-config.yaml, embedded verbatim
	// MessageSchemaFiles (ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28 §1) is
	// config.Config.MessageSchemaFiles(): every declared `message_schemas.<name>.path:` schema's
	// cleaned relative path (relative to argus-config.yaml) -> its raw file bytes. An `inline:`
	// schema needs no file and is never in this map. Each entry is embedded as an EXTRA key in the
	// argus-config ConfigMap and mounted back at /config/<that same relative path> (design §1: "the
	// config loader resolves `path:` relative to the config file") — see messageSchemaConfigMapData
	// / messageSchemaVolumeItems. Empty/nil (no message_schemas, or all inline) renders the ORIGINAL
	// flat `configMap: {name: argus-config}` volume form, byte-for-byte, so every render from before
	// this field existed is unchanged.
	MessageSchemaFiles map[string][]byte
	Replicas           int // 1 at genesis (identity.key race), then the min-3 axiom
	// GrafanaPublicURL (UC138), when set, is injected as ARGUS_GRAFANA_PUBLIC_URL so
	// runner__get_dashboard_url builds a deep link a HUMAN can open for THIS environment. It is plain
	// env, not a Secret entry: a public Grafana base is not a credential. Empty renders nothing, so the
	// compose/k3d manifests are unchanged and keep the localhost default.
	GrafanaPublicURL string

	// U7 (CP-M3-III-80): the HOST folders this instance was onboarded from, in the operator's native
	// spelling, plus the machine that ran onboarding. CP-M3-III-73 wired these for the COMPOSE tier
	// only — docker-compose.byo-m3.yml passes them, k8srender did not — so the FOLDERS row was blank
	// on exactly the two tiers where "where did this come from?" is hardest to answer: a k3d or AKS
	// executor is a pod on a cluster with no visible relationship to anybody's disk. The executor
	// cannot derive them (it sees /config and /scenarios and knows nothing of their mount sources),
	// so onboarding must tell it here too. Empty renders nothing.
	ProductDir  string
	TestDir     string
	OnboardHost string
	// VR5-U1: the kit onboarding ran from, carried to the executor so it can report it on its poll.
	KitDir string
	// VR10-U1 (V28-020): the kubectl CONTEXT this instance is being onboarded against, and the
	// kubeconfig FILE when one was named. A pod cannot derive either — it has no view of the
	// operator's kubeconfig — so onboarding tells it, exactly as it tells it the kit folder. Both
	// empty means "not recorded"; the pod env then omits the keys entirely rather than blanking them.
	KubeContext string
	Kubeconfig  string

	// WebhookURL, when set, is injected as the executor's WEBHOOK_URL env so scenarios' ${WEBHOOK_URL}
	// resolves from the SUT's declared external.webhook_base_url (FQDN-rewritten) rather than a
	// hardcoded compose default (U9). Set it via WebhookURLFor.
	WebhookURL string

	// SecretEnv carries the ${VAR}s the SUT's argus-config references (its MCP bearer, DB
	// password, …). The config is embedded VERBATIM, i.e. still templated, so the pod must be
	// able to expand it — config.Load hard-fails on an unresolved ${VAR}. These land in the same
	// Secret as the token pair, never in the ConfigMap: a ConfigMap is world-readable to anything
	// with namespace read access, and the whole point of the ${VAR} convention is that values
	// never sit next to the config.
	SecretEnv map[string]string

	// Optional knobs; sensible defaults applied by normalize().
	StorageClass string
	// AccessMode for the shared results volume. RWX on local tiers so the min-3 replicas can
	// SPREAD across nodes while still sharing the one volume that carries the machine identity;
	// a node-local RWO class pins them all to one node (see normalize).
	AccessMode  string
	ResultsSize string
	// ObsStorageClass is the StorageClass of the observability volumes (the instance Loki's data and
	// the Pushgateway's file). Empty takes obsStorageClassFor(tier). These volumes are single-writer
	// ReadWriteOnce, so the class is NOT the results volume's RWX one — see obsStorageClassFor.
	ObsStorageClass string
	// ImagePullSecret names a dockerconfig Secret in the instance namespace the executor uses to
	// pull the (private) suite image. Empty on the local tiers — they import the image into the
	// node's containerd store, so there is no registry pull and no secret to reference. On the
	// managed AKS tier normalize() defaults it to the cluster convention `ghcr-pull` (which
	// onboard.sh copies into the namespace), since AKS nodes MUST pull from GHCR.
	ImagePullSecret string
	// ExternalAliases maps a BARE hostname a scenario may still carry from the compose era
	// (e.g. "webhook-mock") to the in-cluster FQDN it should resolve to. Rendered as
	// ExternalName Services in the executor namespace.
	ExternalAliases map[string]string
	// Quota bounds the instance namespace so one instance cannot starve the others.
	CPULimit    string
	MemoryLimit string
	PodLimit    string

	// ObsMode (T3.1/T3.2, E3): "" (-> bundled below), "bundled", "adopt" or "export".
	// An unrecognized value is refused by validate() — RenderExecutor/RenderObs write NOTHING
	// before refusing, same as every other pre-render check in this file. cmd/argus is the only
	// production caller and it also refuses an unknown mode up front (cmdRenderK8s), so validate()
	// here is the second line of defense for anyone calling this package directly (tests included).
	ObsMode string
	// ObsLokiURL is the operator's OWN Loki (observability.loki.url, unresolved — the caller must
	// pass config.LokiURLConfigured(), NOT LokiURL(), or an adopt/export run would silently point
	// at the bundled default that neither mode deploys). REQUIRED when ObsMode is "adopt", and
	// REQUIRED (with ObsLokiPushURL + ObsCredentialVarName) for an "export" hosted-Loki target;
	// ignored in bundled mode, where the executor keeps its hardcoded bundled --loki argument.
	ObsLokiURL string
	// ObsPushgatewayURL is the operator's OPTIONAL Pushgateway (observability.pushgateway.url).
	// Ignored in bundled mode. In adopt AND export mode: set -> the executor's --pushgateway
	// carries it; empty -> the executor gets an EXPLICIT --pushgateway "" (obsquery.PushMetrics
	// already no-ops on ""), never the CLI's bundled-looking default pointed at a Pushgateway
	// neither mode deploys.
	ObsPushgatewayURL string
	// ObsLokiPushURL (T3.1, E3 export hosted-Loki target) is the operator's hosted Loki PUSH
	// endpoint (observability.loki.push_url, e.g. https://logs-xxx.grafana.net/loki/api/v1/push —
	// see config.LokiPushURLConfigured). Ignored outside export mode. Together with ObsLokiURL and
	// ObsCredentialVarName this is the "hosted-Loki triple" validate() requires for an export run
	// that does not use BetterStack (ObsUseBetterStack).
	ObsLokiPushURL string
	// ObsCredentialVarName (T3.1) is the BARE ${VAR} NAME — e.g. "LOKI_CREDENTIAL" — that
	// observability.loki.credential or observability.betterstack.credential resolves from.
	// Deliberately NEVER the resolved value: it is rendered only as a secretKeyRef.key against the
	// argus-obs-credential Secret (obsCredentialSecretName) — onboard.sh creates that Secret
	// directly from the operator's environment, OUTSIDE render output (T3.1 design: "render must
	// not emit the Secret object"). Required for an export run, whichever backend it targets.
	ObsCredentialVarName string
	// ObsUseBetterStack (T3.1, E3 export BetterStack target) is true when the SUT's argus-config
	// declares observability.betterstack (config.UseBetterStack()) — the sibling of a hosted-Loki
	// target. In export mode this makes RenderObs deploy NOTHING (log shipping into BetterStack is
	// the operator's own pipeline, same "deploys nothing of its own" promise as adopt/hosted-Loki
	// export minus even promtail) and takes obsExecArgsBlock's --loki/--pushgateway args out of use
	// (backendFor, internal/toolcore, picks BetterStack by config alone). Ignored outside export mode.
	ObsUseBetterStack bool
	// ObsSharedURL (T3.3, E3 shared ingest) is the environment's ONE shared Loki base URL
	// (--obs-shared-url; default SharedLokiInClusterURL). REQUIRED when ObsMode is "shared", ignored
	// otherwise. The instance renders no Loki of its own: promtail pushes here with tenant_id = the
	// instance id, and the executor reads here with X-Scope-OrgID = the instance id.
	ObsSharedURL string

	// CollectSUTLogs (render-k8s --collect-sut-logs), default false, gates whether promtail is
	// rendered AT ALL and, with it, whether the SUT namespace's pod logs are read. The bundled
	// promtail tails every pod under /var/log/pods/<SUTNamespace>_* — for a SUT whose logs may carry
	// user or agent content (e.g. a message-bus app), shipping that into Loki is a privacy decision
	// the tester never made just by choosing a k8s tier. False renders NO promtail object anywhere
	// (bundled, the export hosted-Loki target, and shared mode all gate on this one field) — Loki and
	// the pushgateway (metrics) still render in bundled/shared, since they carry no SUT log content.
	// cmd/argus prints which namespace will be read whenever this is true (render-k8s time, not
	// apply time), so the decision is said out loud, not left to be discovered in a rendered YAML.
	CollectSUTLogs bool
}

// SharedObsNamespace is where the ONE shared Loki lives (T3.3). Never an instance namespace, so no
// instance teardown's `kubectl delete ns argus-inst-<id>` can reach it.
const SharedObsNamespace = "argus-obs"

// SharedLokiInClusterURL is the in-cluster address of the shared Loki RenderSharedLoki renders —
// the single named default for --obs-shared-url. Both the executor and promtail run in-cluster on
// every k8s tier, so this one URL serves k3d and aks alike; only the off-cluster Grafana on a local
// tier needs the NodePort instead (onboard.sh reads it from the Service).
const SharedLokiInClusterURL = "http://loki." + SharedObsNamespace + ".svc.cluster.local:3100"

// obsModeLabel marks an instance namespace that runs --obs=shared. It is load-bearing twice: the
// shared Loki's NetworkPolicy admits exactly the namespaces carrying it, and teardown reads it to
// say that this instance's logs outlive it (they age out by retention). Rendered ONLY in shared
// mode, so every other mode's Namespace is byte-unchanged.
const obsModeLabel = "argus.onedroid.ai/obs-mode"

// sharedTenantCheck refuses an empty tenant. Under shared mode the tenant is the ONLY thing
// separating one instance's logs from another's in the query path, and an unscoped request is the
// failure the whole mode exists to prevent. cmd/argus runs the same refusal at executor start.
func sharedTenantCheck(tenant string) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("--obs=shared needs a Loki tenant (the instance id) — refusing to read or write the shared Loki unscoped")
	}
	return nil
}

// obsCredentialSecretName is the Secret every export-mode credential env (executor AND, for a
// hosted-Loki target, promtail) references via secretKeyRef. Onboard.sh creates and updates it
// directly from the operator's environment — render never emits the Secret object, so the
// credential VALUE never appears in any file this package writes (T3.1 design item 4).
const obsCredentialSecretName = "argus-obs-credential"

// obsModeBundled/obsModeAdopt/obsModeExport name the three --obs values (T3.1). Centralized so
// the accepted set and its user-facing listing can't drift apart the way two separately spelled
// literals would.
const (
	obsModeBundled = "bundled"
	obsModeAdopt   = "adopt"
	obsModeExport  = "export"
	obsModeShared  = "shared" // T3.3: one Loki per environment, tenant = instance id
	// obsModeNone (E3 follow-up) renders NO observability objects at all — no Loki, no
	// pushgateway, no promtail, no PodMonitor, no obs.yaml content. Unlike adopt/export it makes NO
	// claim about an operator-owned backend either: the executor's own --loki/--pushgateway args
	// are explicit empties (obsExecArgsBlock), so a run produces no observability evidence anywhere.
	// The executor itself still starts — this mode only removes the OBS PLANE, never the SUT tier.
	obsModeNone = "none"
)

// Namespace is the instance's own namespace. One namespace per instance is what makes
// teardown a single `kubectl delete ns` and keeps two instances from colliding.
func (in Instance) Namespace() string { return "argus-inst-" + in.ID }

// dns1123 is the k8s label rule: lowercase alphanumerics and '-', starting and ending
// alphanumeric. Rendering an invalid id produces a manifest the API server rejects with a
// message that points at the manifest rather than at the id, so reject it here instead.
var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const maxNamespaceLen = 63 // k8s hard limit on a namespace name

func (in *Instance) normalize() {
	// Tier key for the storage/pull defaults below. Deliberately NOT defaulted to k3d here even
	// though in.Tier is defaulted further down: an EMPTY tier has always fallen through to the
	// non-local branch (local-path / RWO), and quietly promoting it to k3d's RWX class would bind
	// the PVC to `argus-rwx` — a class that exists only after onboard.sh installs it — leaving
	// the PVC Pending and the executor never Ready on any cluster without it. Pinned by
	// TestNormalize_storageDefaultsPerTierAreStable.
	tier := strings.ToLower(in.Tier)
	if in.StorageClass == "" {
		// The class must give the shared results PVC an access mode that lets the min-3 replicas
		// SPREAD across nodes while sharing the one volume that carries the machine identity. A
		// node-local RWO class (k3d's local-path, AKS's managed disk) pins every replica to the node
		// that first bound the volume — measured on a 2-node k3d cluster: 3/3 on one node, which
		// satisfies the min-3 COUNT while defeating its availability intent, since they die with the node.
		switch {
		case localTiers[tier]:
			in.StorageClass = "argus-rwx" // the in-cluster NFS RWX class onboard.sh installs
		case tier == "aks":
			in.StorageClass = "azurefile-csi" // Azure Files = native RWX (file.csi.azure.com)
		default:
			in.StorageClass = "local-path"
		}
	}
	if in.AccessMode == "" {
		if localTiers[tier] || tier == "aks" {
			in.AccessMode = "ReadWriteMany"
		} else {
			in.AccessMode = "ReadWriteOnce"
		}
	}
	// The managed tier pulls the PRIVATE suite image from GHCR (AKS nodes cannot import from a host
	// docker daemon the way k3d does), so its executor needs an imagePullSecret. Default to the
	// cluster convention the CP and managed Social already use (`ghcr-pull`); onboard.sh copies that
	// secret into the instance namespace. Local tiers import the image and reference no secret.
	if in.ImagePullSecret == "" && tier == "aks" {
		in.ImagePullSecret = "ghcr-pull"
	}
	if in.ResultsSize == "" {
		in.ResultsSize = "2Gi"
	}
	if in.ObsStorageClass == "" {
		in.ObsStorageClass = obsStorageClassFor(tier)
	}
	if in.Replicas <= 0 {
		in.Replicas = 1
	}
	if in.Tier == "" {
		in.Tier = "k3d"
	}
	if in.Cluster == "" {
		in.Cluster = in.Tier
	}
	// T3.1: empty -> bundled, today's behaviour, unchanged. A caller (cmd/argus, or a direct test)
	// that never sets ObsMode at all must render exactly what it always rendered.
	if in.ObsMode == "" {
		in.ObsMode = obsModeBundled
	}
	// F12/UC071: deliberately NOT defaulted. A hardcoded "0.1.0" here made every k8s executor report
	// the same version as every other one, whatever binary it was actually running. An empty value
	// travels as an empty ARGUS_VERSION, and the binary reports its own build stamp instead.
	_ = in.Version
	// The quota must SCALE with the replica count. It used to be a flat 4 CPU / 6Gi / 12 pods, which
	// fits one replica exactly and starves three: 3 executors claim 3 CPU on their own, and with loki
	// (500m) + pushgateway (200m) the namespace sits at 3900m, so the quota then refuses the SECOND
	// promtail — `exceeded quota: instance-quota, requested: limits.cpu=200m, used: 3900m, limited: 4`.
	// promtail is a DaemonSet, one pod per node tailing only ITS node's pods, so the pod that cannot be
	// scheduled takes that whole node's SUT logs with it, silently: the run still passes and the
	// dashboard is simply missing half the cluster. (Found live on orderservice-k3d, 2026-07-23.)
	//
	// Headroom beyond the executors covers loki + pushgateway (700m), a promtail on each of up to four
	// nodes (800m), and the one-shot pods onboarding/teardown create in this namespace (de-register,
	// auth-preflight). The executor count used is never below the run-time scale-up (quotaReplicas):
	// a manifest rendered at 1 replica still gets 6 CPU / 9Gi / 14 pods, because the executor scales
	// itself to 3 when a run starts.
	if in.CPULimit == "" {
		in.CPULimit = strconv.Itoa(quotaReplicas(in.Replicas) + 3)
	}
	if in.MemoryLimit == "" {
		in.MemoryLimit = strconv.Itoa(quotaReplicas(in.Replicas)*1536+4608) + "Mi"
	}
	if in.PodLimit == "" {
		in.PodLimit = strconv.Itoa(quotaReplicas(in.Replicas) + 11)
	}
}

// StorageDefaultsFor reports the storage class and access mode normalize() would choose for a
// tier, without rendering anything. It exists so that `argus preflight` can tell an operator which
// storage class their cluster needs BEFORE the manifests are created, instead of after the PVC has
// sat Pending for ten minutes with nothing naming the tier as the cause.
//
// ⛔ It DELEGATES to normalize rather than restating the mapping. A copy would be a second home for
// one rule, and the two would drift the first time either moved — which is the whole failure this
// function is meant to prevent, not reproduce. TestStorageDefaultsFor_AgreesWithRenderedManifest
// pins the agreement.
//
// ⚠️ An EMPTY tier really does return local-path/ReadWriteOnce, even though normalize defaults the
// tier itself to k3d further down. That asymmetry is deliberate and load-bearing (see the comment
// at the top of normalize): callers must be told what the STORAGE path does, not what the tier
// field ends up saying.
func StorageDefaultsFor(tier string) (storageClass, accessMode string) {
	in := Instance{Tier: tier}
	in.normalize()
	return in.StorageClass, in.AccessMode
}

// ResolvedStorage reports the storage class and access mode this Instance will actually render
// with, once normalize() has applied its tier defaults to whichever of the two fields the caller
// left empty (T2.2). Unlike StorageDefaultsFor — which answers "what would an UNSET instance on
// this tier get" — this respects an operator-supplied `--storage-class` / `--results-access-mode`
// already sitting on the Instance, so cmdRenderK8s can report what it is ABOUT to use without
// re-deriving the tier mapping a second time. Value receiver: normalize() cannot escape into the
// caller's own Instance.
func (in Instance) ResolvedStorage() (storageClass, accessMode string) {
	in.normalize()
	return in.StorageClass, in.AccessMode
}

// ResolvedObsStorageClass reports the StorageClass the observability volumes will render with: the
// operator's --obs-storage-class, else the tier default. EMPTY means the claims name no class and the
// cluster's default StorageClass binds them. Value receiver, like ResolvedStorage.
func (in Instance) ResolvedObsStorageClass() string {
	in.normalize()
	return in.ObsStorageClass
}

// activeReplicas is the replica count the executor scales ITS OWN Deployment to while a run is active
// (runner/autoscale.go defaultActiveReplicas, overridable only by ARGUS_SCALE_ACTIVE, which the renderer
// never sets). The quota has to hold that many executors, whatever count the manifest was rendered at.
const activeReplicas = 3

// quotaReplicas is the executor count the instance quota is sized for: the rendered count or the run-time
// scale-up, whichever is larger. Onboarding renders at 1 (the identity-key genesis), so without this the
// quota always refused the executor's own third replica the moment a run started.
func quotaReplicas(rendered int) int {
	if rendered < activeReplicas {
		return activeReplicas
	}
	return rendered
}

func (in Instance) validate() error {
	if !dns1123.MatchString(in.ID) {
		return fmt.Errorf("instance id %q is not a valid DNS-1123 label (lowercase alphanumerics and '-', must start and end alphanumeric)", in.ID)
	}
	if len(in.Namespace()) > maxNamespaceLen {
		return fmt.Errorf("instance id %q yields namespace %q, which exceeds the %d-character k8s limit", in.ID, in.Namespace(), maxNamespaceLen)
	}
	if !dns1123.MatchString(in.SUTNamespace) {
		return fmt.Errorf("SUT namespace %q is not a valid DNS-1123 label", in.SUTNamespace)
	}
	if strings.TrimSpace(in.Image) == "" {
		return fmt.Errorf("image is required (the onedroid-testing-suite image the executor runs)")
	}
	if strings.TrimSpace(in.CPURL) == "" {
		return fmt.Errorf("control-plane URL is required")
	}
	if strings.TrimSpace(in.ArgusConfig) == "" {
		return fmt.Errorf("argus-config.yaml content is required — it is embedded verbatim into the instance ConfigMap")
	}
	// T3.1/T3.2 (E3): refuse before rendering ANYTHING — same "write nothing before refusing" rule
	// every other check in this function already follows. normalize() has already turned "" into
	// "bundled", so this switch only ever sees a real caller value.
	switch strings.ToLower(strings.TrimSpace(in.ObsMode)) {
	case obsModeBundled, obsModeNone:
	case obsModeAdopt:
		// Deliberately in.ObsLokiURL, never a resolved/defaulted URL: the caller (cmd/argus) must
		// pass config.LokiURLConfigured(), which returns "" when the operator declared none. Silently
		// accepting an empty value here would let an adopt run point the executor at a Loki (the
		// bundled default) that --obs=adopt never deploys — exactly the "do not silently fall back"
		// promise this ticket makes.
		if strings.TrimSpace(in.ObsLokiURL) == "" {
			return fmt.Errorf("--obs=adopt requires observability.loki.url in the SUT's argus-config.yaml (Argus deploys no Loki of its own in adopt mode) — set that key, or run with --obs=bundled")
		}
	case obsModeExport:
		// T3.1: two accepted shapes, checked independently of each other (BetterStack wins outright
		// when declared — config.validateObservabilityBackend already refuses a config that declares
		// BOTH loki and betterstack, so in practice at most one of these two conditions is ever true).
		hasHostedLoki := strings.TrimSpace(in.ObsLokiURL) != "" &&
			strings.TrimSpace(in.ObsLokiPushURL) != "" &&
			strings.TrimSpace(in.ObsCredentialVarName) != ""
		if !in.ObsUseBetterStack && !hasHostedLoki {
			return fmt.Errorf("--obs=export requires either a hosted Loki target (observability.loki.url + push_url + credential, all set) or observability.betterstack declared in the SUT's argus-config.yaml — neither is present; set one, or run with --obs=bundled or --obs=adopt")
		}
	case obsModeShared:
		// T3.3: k8s tiers only. Compose has no cluster to host the argus-obs namespace in.
		if strings.EqualFold(strings.TrimSpace(in.Tier), "compose") {
			return fmt.Errorf("--obs=shared is k8s-only: --tier compose has no cluster to host the shared Loki (namespace %s) — use --tier k3d or --tier aks, or --obs=bundled on compose", SharedObsNamespace)
		}
		if strings.TrimSpace(in.ObsSharedURL) == "" {
			return fmt.Errorf("--obs=shared requires the shared Loki's base URL (--obs-shared-url; the in-cluster default is %s) — refusing to fall back to a per-instance Loki shared mode never deploys", SharedLokiInClusterURL)
		}
		if err := sharedTenantCheck(in.ID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("--obs must be one of bundled, adopt, export, shared, none (got %q)", in.ObsMode)
	}
	return nil
}

// obsExecArgsBlock renders the executor's --loki/--pushgateway args (T3.1/T3.2). This is the ONE
// place bundled vs adopt diverges inside RenderExecutor.
//
//   - bundled: the ORIGINAL two hardcoded lines, byte-for-byte — this must never change, or
//     render-k8s --obs bundled would stop being byte-identical to the pre-T3 renderer (the
//     promise T3.1 makes explicit).
//   - adopt: the operator's own Loki (validate() has already refused an empty one) and, when set,
//     the operator's own Pushgateway. When the operator declared NO pushgateway, this still emits
//     an EXPLICIT `--pushgateway ""` rather than omitting the flag — the CLI's own --pushgateway
//     default (http://localhost:9091, cmd/argus/main.go) would otherwise point the executor at a
//     Pushgateway that adopt mode never deploys, and obsquery.PushMetrics only skips the push
//     cleanly on an EMPTY url, not on an unreachable one (internal/obsquery/push.go:20-23).
func obsExecArgsBlock(in Instance) string {
	const ind = "            " // matches the surrounding `args:` list's indent
	// T3.3 shared: the environment's Loki plus this instance's tenant, and the mode itself so the
	// executor can refuse at start if the tenant ever arrives empty (cmd/argus). The pushgateway
	// stays per-instance (the design keeps it out of scope), so its bundled address is unchanged.
	if strings.EqualFold(in.ObsMode, obsModeShared) {
		return fmt.Sprintf("%[1]s- --obs\n%[1]s- shared\n%[1]s- --loki\n%[1]s- %[2]q\n%[1]s- --loki-tenant\n%[1]s- %[3]q\n"+
			"%[1]s- --pushgateway\n%[1]s- http://pushgateway:9091\n", ind, in.ObsSharedURL, in.ID)
	}
	// none: no obs plane exists anywhere (RenderObs returns nothing), so the executor must be told
	// EXPLICITLY that there is nothing to push to or query — the same "explicit empty, never a
	// default that points at nothing" rule adopt's no-pushgateway case already follows below.
	if strings.EqualFold(in.ObsMode, obsModeNone) {
		return ind + "- --loki\n" + ind + "- \"\"\n" +
			ind + "- --pushgateway\n" + ind + "- \"\"\n"
	}
	// T3.1: export uses the SAME args shape as adopt — "--loki <url> as in adopt", per the design.
	// For a hosted-Loki export target that url is the read/query side (ObsLokiURL, same field
	// adopt uses); for a BetterStack export target it is typically empty (BetterStack declares no
	// observability.loki block at all), which is harmless — backendFor (internal/toolcore) selects
	// BetterStack purely from config.UseBetterStack() and never looks at --loki when it does.
	if !strings.EqualFold(in.ObsMode, obsModeAdopt) && !strings.EqualFold(in.ObsMode, obsModeExport) {
		return ind + "- --loki\n" + ind + "- http://loki:3100\n" +
			ind + "- --pushgateway\n" + ind + "- http://pushgateway:9091\n"
	}
	return fmt.Sprintf("%[1]s- --loki\n%[1]s- %[2]q\n%[1]s- --pushgateway\n%[1]s- %[3]q\n",
		ind, in.ObsLokiURL, in.ObsPushgatewayURL)
}

// obsModeLabelLine is the instance Namespace's obs-mode label (4-space indent, under labels:) —
// shared mode only, "" otherwise so a bundled/adopt/export Namespace is byte-unchanged (T3.3).
func obsModeLabelLine(in Instance) string {
	if !strings.EqualFold(in.ObsMode, obsModeShared) {
		return ""
	}
	return "    # T3.3: the shared Loki's NetworkPolicy admits namespaces carrying this, and teardown reads it.\n" +
		"    " + obsModeLabel + ": " + obsModeShared + "\n"
}

// obsCredentialEnvBlock renders ONE container env entry sourcing VALUE from the shared
// argus-obs-credential Secret (T3.1) via secretKeyRef — indent is the caller's env-list item
// indent (12 spaces for the executor's `env:` list, 10 for promtail's container `env:` list). The
// env var NAME equals varName, matching the ${VAR} the embedded (still-templated) argus-config.yaml
// references, so the process's own config.Load can expand it. "" when varName is "" (outside
// export mode, or an export target that needs no credential env here), so callers stay unchanged.
func obsCredentialEnvBlock(indent, varName string) string {
	if strings.TrimSpace(varName) == "" {
		return ""
	}
	return fmt.Sprintf("%[1]s- name: %[2]s\n%[1]s  valueFrom:\n%[1]s    secretKeyRef: {name: %[3]s, key: %[2]s}\n",
		indent, varName, obsCredentialSecretName)
}

// indent re-indents a block so it can be embedded under a YAML key.
func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l == "" {
			continue // keep blank lines blank: trailing spaces would be significant noise
		}
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}

// messageSchemaKeys returns in.MessageSchemaFiles' relative paths sorted, so every render of the
// SAME declarations is byte-identical (a Go map iterates in random order).
func messageSchemaKeys(files map[string][]byte) []string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// messageSchemaConfigMapKey turns a `path:` schema's relative path (e.g. "schemas/ping.avsc") into
// a valid ConfigMap data KEY (design §1: "ConfigMap keys cannot contain `/`, so use a key encoding
// plus items:/subPath mounts"). A ConfigMap key must match [-._a-zA-Z0-9]+; a relative path may
// contain "/" (and, in principle, any other path segment), so this hashes the path rather than
// substituting "/" for some other separator — a substitution scheme risks two DIFFERENT declared
// paths encoding to the SAME key (e.g. "a/b.avsc" and "a_b.avsc" both -> "a_b.avsc"), silently
// dropping one schema's content from the rendered ConfigMap. The path, not the schema NAME, is
// hashed: two schemas could share a path only by being the same declaration (config.Load already
// refuses two `message_schemas` entries with the same key), so this never has to resolve a
// collision between two DIFFERENT files.
func messageSchemaConfigMapKey(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "schema-" + hex.EncodeToString(sum[:8])
}

// messageSchemaConfigMapData renders the EXTRA `data:` entries a `path:` message schema needs
// beyond argus-config.yaml — "" when there are none, which is what keeps a render with no
// message_schemas byte-for-byte identical to before this field existed (Instance.MessageSchemaFiles
// doc comment). Each entry is embedded VERBATIM (design §1's "embedded verbatim" rule, matching
// argus-config.yaml's own treatment), under a 2-space key/4-space content indent matching the
// existing `argus-config.yaml: |` entry immediately above it.
func messageSchemaConfigMapData(files map[string][]byte) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range messageSchemaKeys(files) {
		b.WriteString("  " + messageSchemaConfigMapKey(p) + ": |\n")
		b.WriteString(indent(string(files[p]), 4))
		b.WriteString("\n")
	}
	return b.String()
}

// messageSchemaConfigVolumeSource renders the `config` volume's `configMap:` source line(s). With
// no declared `path:` schema files it is the ORIGINAL flat one-liner `configMap: {name:
// argus-config}` — byte-for-byte what every render produced before this field existed, since a
// ConfigMap volume with no `items:` already projects every key as a file at the mount root and
// there is only ever the one key. With declared schema files it adds an EXPLICIT `items:` list:
// argus-config.yaml mapped to itself, plus one entry per schema mapping its ConfigMap key back to
// its ORIGINAL relative path (e.g. `path: schemas/ping.avsc`) — this is what makes kubelet
// materialize the schema at /config/schemas/ping.avsc inside the pod (mountPath /config + this
// subPath), exactly where the config loader resolves a `path:` schema relative to argus-config.yaml
// (design §1).
func messageSchemaConfigVolumeSource(files map[string][]byte) string {
	if len(files) == 0 {
		return "configMap: {name: argus-config}"
	}
	var b strings.Builder
	b.WriteString("configMap:\n            name: argus-config\n            items:\n")
	b.WriteString("              - {key: argus-config.yaml, path: argus-config.yaml}\n")
	keys := messageSchemaKeys(files)
	for i, p := range keys {
		fmt.Fprintf(&b, "              - {key: %s, path: %s}", messageSchemaConfigMapKey(p), p)
		if i < len(keys)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// RenderExecutor renders the instance's execution plane: Namespace, results PVC, token Secret,
// argus-config ConfigMap, executor Deployment, ClusterIP Service, SUT-alias ExternalName
// Services, a default-deny NetworkPolicy, and a ResourceQuota.
func RenderExecutor(in Instance) (string, error) {
	in.normalize()
	if err := in.validate(); err != nil {
		return "", err
	}
	ns := in.Namespace()

	var b strings.Builder
	fmt.Fprintf(&b, `# RENDERED by the argus onboarder (internal/k8srender) for instance %q. Do not edit by hand.
# One namespace per instance: teardown is a single "kubectl delete ns %s", and N instances
# coexist in one cluster with no name collisions.
apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    app.kubernetes.io/part-of: argus
    argus.onedroid.ai/instance: %s
    argus.onedroid.ai/tier: %s
    # UC007: which SUT namespace THIS instance watches. Recorded as a label so onboarding can answer
    # "is any existing instance already watching this SUT namespace?" with one label query, instead of
    # parsing promtail configs. It is load-bearing, not decorative: per-instance log isolation is a
    # STATIC namespace glob (/var/log/pods/<sut-ns>_*/*/*.log) and each promtail stamps its own
    # argus_instance on every line it reads, so TWO instances watching one SUT namespace both ingest the
    # same pod logs and each claims them — instance B's Loki then contains instance A's run, and A's
    # contains B's. The corruption is silent and MUTUAL: onboarding a second watcher damages the
    # instance that was already running. Same exposure on k3d and aks — both render this identical glob.
    argus.onedroid.ai/sut-namespace: %s
%s---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: exec-results
  namespace: %s
spec:
  # All replicas SHARE this claim, so the machine identity (LoadOrCreateKey ->
  # /results/identity.key) is created once by the first pod and LOADED by the rest:
  # ONE instance, N pollers.
  #
  # The access mode is NOT cosmetic. A node-local RWO class (k3d's default local-path) gives its
  # PV a hard node affinity, and WaitForFirstConsumer binding then OBLIGES the scheduler to put
  # every replica on the node that first bound the volume — measured on a 2-node cluster: 3/3
  # replicas pinned to one node. That satisfies the min-3 replica COUNT while defeating its
  # availability intent, since one node taking them all down is exactly what min-3 exists to
  # prevent. RWX lets the replicas spread AND keep sharing the identity.
  accessModes: ["%s"]
  storageClassName: %s
  resources:
    requests:
      storage: %s
---
apiVersion: v1
kind: Secret
metadata:
  name: exec-tokens
  namespace: %s
type: Opaque
stringData:
  # These gate ONLY the executor's local runner__* MCP surface (what the agent presents
  # through the tunnel). Federation register/push use the machine identity, not these.
  ARGUS_RUNNER_TOKEN: %q
  # ARGUS_AUTHOR_TOKEN is the OLD name for this same value; ARGUS_EXECUTOR_SECRET (below) is the
  # new one. Both keys carry the SAME value so an executor image built before the rename (still
  # reading ARGUS_AUTHOR_TOKEN) and one built after (reading ARGUS_EXECUTOR_SECRET, falling back to
  # ARGUS_AUTHOR_TOKEN via internal/envname) both start correctly off ONE rendered Secret — this is
  # what makes a rollback to an older executor image safe across this rename. ⛔ NEITHER SECRET KEY
  # NAME may be renamed or removed here without a matching release decision: the old key stays
  # until a later release explicitly drops it (see internal/envname's package doc).
  ARGUS_AUTHOR_TOKEN: %q
  ARGUS_EXECUTOR_SECRET: %q
  # ARGUS_TOKEN is the PRESENTED credential the CLI reads (--token or ARGUS_TOKEN); the two
  # above only declare which SCOPE a presented token carries. Without it every GATED argus
  # subcommand run INSIDE this pod fails with
  #   {"error":"denied: auth: a token is required (supply --token or ARGUS_TOKEN; ...)"}
  # which is how preflight-auth silently never ran on any k8s tier (CP-M3-III-30), and why
  # argus get-report is unusable in-pod. Set to the RUNNER token - in-pod commands are runner-scope.
  # compose does NOT mirror this: argus_sut()'s one-shot helpers pass -e ARGUS_TOKEN, but the
  # long-lived executor service in docker-compose.byo-m3.yml sets only the two scope tokens above.
  # That is why argus runner-state was denied there, and passed here only by accident (V28-001).
  # From 0.3.29 runner-state is dispatched ABOVE the token gate (cmd/argus/main.go) and reads none
  # of these three on any tier; this value stays for the gated in-pod commands only.
  ARGUS_TOKEN: %q
%s%s%s---
apiVersion: v1
kind: ConfigMap
metadata:
  name: argus-config
  namespace: %s
data:
  # The OPERATOR's argus-config.yaml, embedded verbatim — this is what makes the tier
  # bring-your-own-SUT rather than OrderService-only.
  argus-config.yaml: |
%s
%s`,
		in.ID, ns,
		ns, in.ID, in.Tier, in.SUTNamespace, obsModeLabelLine(in),
		ns, in.AccessMode, in.StorageClass, in.ResultsSize,
		ns, in.RunnerToken, in.AuthorToken, in.AuthorToken, in.RunnerToken, enrollmentSecretEntry(in.EnrollmentToken), secretEnvBlock(in.SecretEnv), identitySecretBlock(ns, in.IdentityKeyB64),
		ns, indent(in.ArgusConfig, 4), messageSchemaConfigMapData(in.MessageSchemaFiles))

	fmt.Fprintf(&b, `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: executor
  namespace: %s
  labels:
    app.kubernetes.io/name: argus-executor
    app.kubernetes.io/part-of: argus
    argus.onedroid.ai/instance: %s
%sspec:
  # GENESIS: start at 1 so the FIRST pod creates /results/identity.key on the shared PVC with
  # no race; scale to the min-3 axiom once the key exists (the others then LOAD it).
  replicas: %d
  revisionHistoryLimit: 5
  # INT-032 (2026-08-08): an EXPLICIT strategy, because the Kubernetes default cannot update this
  # Deployment on a cluster that is at capacity — and a cluster at capacity is the normal case.
  #
  # The default is maxSurge 25 percent and maxUnavailable 25 percent. Against replicas: 1 above those
  # resolve to maxSurge 1 (0.25 rounds UP) and maxUnavailable 0 (0.25 rounds DOWN): the update may create
  # pod but may NOT remove the first, so it must be GIVEN capacity before it can free any. Measured on
  # example-cluster: the Deployment carried the new digest, the new pod sat Pending on "Insufficient
  # cpu" with the autoscaler in backoff, and the OLD pod kept serving and kept reporting the OLD
  # version — while onboarding reported success. Patching to 0/1 converged it immediately.
  #
  # 0/1 is right at BOTH sizes: at one replica the old pod goes first and the new one reuses exactly
  # the resources it released; at the min-3 axiom it rolls one at a time and still never asks the
  # cluster for headroom. The cost is a brief gap at replicas: 1, which is the correct trade against
  # "can never be updated" — and it is the gap the executor's durable outbox and the CP's re-poll
  # already exist to absorb. UC069's self-update depends on this working under pressure.
  strategy:
    type: RollingUpdate
    rollingUpdate: {maxSurge: 0, maxUnavailable: 1}
  # MINIMAL selector, deliberately. A Deployment's selector is IMMUTABLE, so anything put here
  # can never change for the life of the object — folding the instance id in would permanently
  # weld the Deployment to that id and make an in-place update impossible (caught by a
  # server-side dry-run: "spec.selector: field is immutable"). Isolation between instances is
  # already provided by the per-instance NAMESPACE, so the instance id stays a plain label.
  selector:
    matchLabels:
      app.kubernetes.io/name: argus-executor
  template:
    metadata:
      labels:
        app.kubernetes.io/name: argus-executor
        app.kubernetes.io/part-of: argus
        argus.onedroid.ai/instance: %s
    spec:
      # UC069 (W9): the executor's ONE k8s-API use — self-delete its own pod when the CP reports it
      # outdated, so the Deployment recreates it current. That needs a mounted SA token + a
      # namespace-scoped delete-pods Role (both rendered below). This is the narrowest possible grant:
      # one verb, one resource, this namespace only.
      serviceAccountName: argus-executor
      automountServiceAccountToken: true
%s      affinity:
%s        podAntiAffinity:
          # PREFERRED, not required: spread the replicas across nodes so the min-3 axiom buys real
          # node-loss survival rather than three pods sharing one failure domain. It must stay
          # preferred — a required rule would leave replicas Pending forever on a single-node
          # cluster, turning a resilience nicety into an outage.
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 100
              podAffinityTerm:
                labelSelector:
                  matchLabels:
                    app.kubernetes.io/name: argus-executor
                topologyKey: kubernetes.io/hostname
      containers:
        - name: executor
          image: %s
          # LOCAL tiers: IfNotPresent — the image was IMPORTED into the node's containerd store and is
          # not pullable from there (the registry is private; an anonymous pull 401s). MANAGED tiers:
          # Always — the node PULLS, and our suite tags MOVE (:m3-dev is rebuilt in place), so a node
          # that cached the tag would otherwise serve a STALE build forever with nothing reporting it.
          # Measured on k3d: two nodes held two different builds under the single tag and a
          # Deployment's replicas ran two different binaries depending on where they landed. k3d
          # catches that by digest-comparing on import; a managed tier has no import step to catch it.
          imagePullPolicy: %s
          # the image ENTRYPOINT is ["argus"], so use args: (a k8s command: would REPLACE
          # the entrypoint and "serve" alone is not on PATH).
          args:
            - serve
            - --mode
            - runner
            - --jmeter
            - local
            - --templates
            - /templates
            - --config
            - /config/argus-config.yaml
            - --scenarios
            - /scenarios
            - --results
            - /results
%s            - --grafana
            - http://localhost:3000
          env:
            - name: ARGUS_CP_URL
              value: %q
            - name: ARGUS_INSTANCE_ID
              value: %q
            - name: ARGUS_WORKSPACE_ID
              value: %q
            - name: ARGUS_SUT_NAME
              value: %q
            # P3 #23: the SAME namespace value the NetworkPolicy below already scopes this executor
            # to (in.SUTNamespace) — never re-derived from the SUT's own argus-config.yaml, which
            # describes ENDPOINTS, not a Kubernetes namespace. Read by internal/argus (load-run
            # environment capture) ONLY when a run declares a LOAD section; absent this, a load
            # run reports environment captured=false with a named reason.
            - name: ARGUS_SUT_NAMESPACE
              value: %q
            - name: ARGUS_TIER
              value: %q
            - name: ARGUS_CLUSTER
              value: %q
            - name: ARGUS_VERSION
              value: %q
            - name: ARGUS_IDENTITY_PATH
              value: %s
            # #215: the autoscaler caps itself at 1 replica unless this volume is ReadWriteMany.
            - name: ARGUS_RESULTS_ACCESS_MODE
              value: %q
            # UC069: the downward API gives the executor its own pod name/namespace so it can
            # self-delete on outdated (the loop guard lives on the shared results volume).
            - name: POD_NAME
              valueFrom:
                fieldRef: {fieldPath: metadata.name}
            - name: POD_NAMESPACE
              valueFrom:
                fieldRef: {fieldPath: metadata.namespace}
%s%s%s%s%s          envFrom:
            - secretRef:
                name: exec-tokens          # the tokens live ONLY here
          ports:
            - name: mcp
              containerPort: 8080
          readinessProbe:
            httpGet: {path: /healthz, port: mcp}
            periodSeconds: 10
            timeoutSeconds: 3
            failureThreshold: 3
          livenessProbe:
            httpGet: {path: /healthz, port: mcp}
            periodSeconds: 30
            timeoutSeconds: 3
            failureThreshold: 3
          resources:
            requests: {cpu: "150m", memory: "384Mi"}
            limits:   {cpu: "1", memory: "1536Mi"}     # JMeter (JVM) headroom
          volumeMounts:
            - {name: results, mountPath: /results}
            - {name: config, mountPath: /config, readOnly: true}
            - {name: scenarios, mountPath: /scenarios}
%s      volumes:
        - name: results
          persistentVolumeClaim: {claimName: exec-results}
        - name: config
          %s
        - name: scenarios
          # Deliberately EMPTY and per-pod. Since U2 the run sources its set from the CONTROL-PLANE
          # CATALOG when no local set is present, so nothing needs to be copied in and all replicas
          # behave identically — which is what makes the min-3 axiom honest here. (Stage II delivered
          # scenarios with "kubectl cp" into the single pod; that only worked at replicas=1, because
          # this volume is per-pod and three pods would have three different empty dirs.)
          # A run that can establish NO source refuses rather than reporting a 0-scenario pass.
          emptyDir: {}
%s---
apiVersion: v1
kind: Service
metadata:
  name: executor
  namespace: %s
  labels:
    app.kubernetes.io/name: argus-executor
    argus.onedroid.ai/instance: %s
spec:
  # S6: ClusterIP ONLY. The executor is never exposed via Ingress/NodePort/LoadBalancer;
  # the test agent reaches its runner__* MCP surface through connect.sh's kubectl port-forward.
  type: ClusterIP
  selector:
    app.kubernetes.io/name: argus-executor
  ports:
    - name: mcp
      port: 8080
      targetPort: mcp
`,
		ns, in.ID,
		pullSecretRegistryAnnotationBlock(in.Image, in.ImagePullSecret),
		in.Replicas, in.ID,
		imagePullSecretBlock(in.ImagePullSecret),
		in.nodeAffinityBlock(),
		in.Image, in.imagePullPolicy(),
		obsExecArgsBlock(in),
		in.CPURL, in.ID, in.WorkspaceID, in.SUTName, in.SUTNamespace, in.Tier, in.Cluster, in.Version,
		identityPath(in.IdentityKeyB64),
		in.AccessMode,
		webhookEnvBlock(in.WebhookURL), grafanaEnvBlock(in.GrafanaPublicURL),
		foldersEnvBlock(in.ProductDir, in.TestDir, in.OnboardHost, in.KitDir, in.KubeContext, in.Kubeconfig),
		// T3.1: the executor's own credential env, 12-space indent (matches the surrounding `env:`
		// list) — "" outside export mode, so a non-export render is byte-unchanged.
		obsCredentialEnvBlock("            ", in.ObsCredentialVarName),
		obsModeEnvBlock(in),
		identityMountBlock(in.IdentityKeyB64),
		messageSchemaConfigVolumeSource(in.MessageSchemaFiles),
		identityVolumeBlock(in.IdentityKeyB64),
		ns, in.ID)

	// UC069 (W9) self-delete RBAC. The executor's ONLY k8s-API rights: delete its own pod in THIS
	// namespace so the Deployment recreates it current on an outdated poll, and right-size and
	// re-image its OWN Deployment. No cluster-scoped rights, no other verbs, no other resources — the
	// namespace already isolates it to this instance.
	//
	// V19-007: this Role must NEVER grant anything on `secrets`, even a narrow get on one named
	// Secret — V19-006 briefly did (to read a pull secret's registry coverage) and was withdrawn; see
	// pullSecretRegistryAnnotationBlock above for where that decision now lives instead.
	fmt.Fprintf(&b, `---
apiVersion: v1
kind: ServiceAccount
metadata: {name: argus-executor, namespace: %s}
automountServiceAccountToken: true
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: argus-executor-selfdelete, namespace: %s}
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [get, delete]     # get = pre-delete existence check; delete = the self-update (UC069)
  - apiGroups: [apps]
    resources: [deployments/scale]
    verbs: [get, patch]      # UC196: right-size the OWN executor Deployment (idle→1, run→min-3)
  - apiGroups: [apps]
    resources: [deployments]
    resourceNames: [executor]
    # F15/UC069 (CP-M3-III-71): update-ON-REQUEST changes the IMAGE, which self-delete alone cannot
    # do — deleting a pod recreates it from the SAME Deployment spec. resourceNames pins this to the
    # executor's OWN Deployment: it can re-image itself and nothing else in the namespace.
    verbs: [get, patch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: argus-executor-selfdelete, namespace: %s}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: argus-executor-selfdelete}
subjects:
  - {kind: ServiceAccount, name: argus-executor, namespace: %s}
`, ns, ns, ns, ns)

	// CROSS-NAMESPACE SUT ALIASES. Scenarios carried over from the compose era may still name a
	// SUT service by its BARE hostname. An ExternalName in the executor namespace makes that bare
	// name resolve to the real SUT Service.
	// U9 UPDATE: ${WEBHOOK_URL} is no longer a hardcoded compose default — WebhookURLFor sources it
	// from the SUT's external.webhook_base_url and FQDN-rewrites it, injected as the executor's
	// WEBHOOK_URL env (see webhookEnvBlock above). So a scenario using ${WEBHOOK_URL} resolves
	// DIRECTLY without the webhook alias. These aliases remain as defense-in-depth for any scenario
	// that hardcodes a bare host instead of the variable, and for the non-webhook bare targets; they
	// are harmless when redundant.
	for _, alias := range sortedKeys(in.ExternalAliases) {
		fmt.Fprintf(&b, `---
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
spec:
  type: ExternalName
  externalName: %s
`, alias, ns, in.ExternalAliases[alias])
	}

	// NetworkPolicy (UC139). Default-deny ingress, then allow ONLY what the instance needs:
	// same-namespace traffic (executor -> loki/pushgateway, promtail -> loki).
	//
	// HONESTY NOTE: a NetworkPolicy is only a real control if the cluster runs a policy
	// controller that ENFORCES it. k3s ships one; a bare-flannel cluster does not, and there
	// the API server accepts this object and silently enforces nothing. Rendering it is
	// therefore NOT evidence of isolation — the gate must prove enforcement with a negative
	// control (a connection that SUCCEEDS before the policy and FAILS after).
	fmt.Fprintf(&b, `---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-ingress
  namespace: %s
spec:
  podSelector: {}          # every pod in the instance namespace
  policyTypes: [Ingress]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-same-namespace
  namespace: %s
spec:
  podSelector: {}
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector: {}   # only pods in THIS instance namespace
---
apiVersion: v1
kind: ResourceQuota
metadata:
  name: instance-quota
  namespace: %s
spec:
  # Bounds the instance so one SUT's suite cannot starve the others sharing the cluster.
  hard:
    limits.cpu: "%s"
    limits.memory: "%s"
    pods: "%s"
`, ns, ns, ns, in.CPULimit, in.MemoryLimit, in.PodLimit)

	return b.String(), nil
}

// bareHost reports whether h is a BARE service name — a single DNS label, as a compose-era
// config writes it (`order-api`, `postgres`, `rabbitmq`). An FQDN, an IP, or localhost is
// already resolvable (or deliberately host-local) and must be left alone.
func bareHost(h string) bool {
	if h == "" || h == "localhost" || strings.Contains(h, ".") || strings.Contains(h, ":") {
		return false
	}
	return dns1123.MatchString(h)
}

// hostOf pulls the hostname out of a URL, tolerating the shapes argus-config actually carries:
// a plain URL, a JDBC URL (jdbc:postgresql://host:5432/db), and an AMQP URL with credentials.
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = strings.TrimPrefix(raw, "jdbc:") // jdbc:postgresql://… -> postgresql://…
	i := strings.Index(raw, "://")
	if i < 0 {
		return ""
	}
	rest := raw[i+3:]
	if at := strings.LastIndex(rest, "@"); at >= 0 { // strip user:pass@
		rest = rest[at+1:]
	}
	if s := strings.IndexAny(rest, "/?"); s >= 0 {
		rest = rest[:s]
	}
	if c := strings.LastIndex(rest, ":"); c >= 0 { // strip :port
		rest = rest[:c]
	}
	return rest
}

// promtailRunVolume renders the promtail DaemonSet's `run` volume — where the rendered config
// above points `positions: filename:` (/run/promtail/positions.yaml) — as a per-NAMESPACE hostPath
// rather than an emptyDir (AC-D35). An emptyDir's contents die with the pod: every promtail
// restart/recreate re-reads the WHOLE node's Docker/CRI log history from scratch, the same defect
// as the compose tiers' positions loss. hostPath survives a pod restart because it is backed by the
// NODE's filesystem, not the pod's. Keyed by ns (the instance's own namespace, one per instance) —
// never a bare shared path — so two instances' promtail DaemonSets scheduled on the same node can
// never share, or reset, each other's read cursor. DirectoryOrCreate: kubelet creates the directory
// on first mount, so onboarding needs no node-side provisioning step. One helper for all three
// promtail renderers (RenderObs bundled, renderExportPromtail, renderSharedInstanceObs) so they
// cannot drift from each other.
func promtailRunVolume(ns string) string {
	return fmt.Sprintf("{name: run, hostPath: {path: /var/lib/argus-promtail/%s, type: DirectoryOrCreate}}", ns)
}

// promtailRBAC renders promtail's cluster-scoped ClusterRole + ClusterRoleBinding — on LOCAL tiers
// only. The grant (nodes, nodes/proxy, services, endpoints, pods, cluster-wide) exists for a
// `kubernetes_sd` discovery this renderer does NOT use: the promtail config below ships
// `static_configs` and isolates by the namespace log glob, because the kubernetes_sd `__path__`
// relabel yielded 0 targets. So the grant is dead privilege on every tier.
//
// On a laptop k3d that is harmless and the binding is left exactly as it was (Stage II is accepted
// and live-proven there; this must not churn it). On a SHARED managed cluster it is not harmless:
// `nodes/proxy` is a kubelet-API read of any pod's logs on any node, plus cluster-wide enumeration
// of every other tenant's pods/services/endpoints — bound cluster-wide, once PER INSTANCE, to a
// ServiceAccount in a namespace that also runs SUT-authored scenario code. Managed tiers get none.
// Pinned by TestRenderObs_managedTierGrantsNoClusterWideRBAC / _localTierRBACUnchanged.
func promtailRBAC(in Instance, rbacName, ns string) string {
	if !localTiers[strings.ToLower(in.Tier)] {
		return ""
	}
	return fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: %s}
rules:
  - apiGroups: [""]
    resources: [nodes, nodes/proxy, services, endpoints, pods]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: %s}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: %s}
subjects:
  - {kind: ServiceAccount, name: promtail, namespace: %s}
---
`, rbacName, rbacName, rbacName, ns)
}

// nodeAffinityBlock keeps the executor OFF a managed cluster's system node pool. On AKS that pool is
// UNTAINTED by default, so a pod with no node constraint can land beside CoreDNS / metrics-server /
// konnectivity — and this pod runs JMeter (a JVM under load), the one neighbour those add-ons should
// not have. The same drift was caught for the control plane (a CP replica landed on the system pool).
//
// It keys on the AKS-GENERIC label `kubernetes.azure.com/mode` (system|user, set on every AKS
// cluster) rather than this cluster's pool NAMES, so the rule is portable across AKS clusters.
// `NotIn` also matches a node that lacks the label entirely, so a hypothetical unlabelled node is
// still schedulable. Emitted for the aks tier ONLY: on a local single-node cluster a REQUIRED rule
// that nothing satisfies would leave every replica Pending forever — an outage dressed as hardening.
// (Returns a 8-space-indented block under `affinity:`, or "" to leave the spec unchanged.)
func (in Instance) nodeAffinityBlock() string {
	if strings.ToLower(in.Tier) != "aks" {
		return ""
	}
	return `        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              - matchExpressions:
                  - key: kubernetes.azure.com/mode
                    operator: NotIn
                    values: ["system"]
`
}

// imagePullPolicy is IfNotPresent on the local tiers (the image is imported into the node store and
// is not pullable from there) and Always on a managed tier (the node pulls from the registry, and a
// MOVING tag such as :m3-dev would otherwise leave a cached stale build running undetected).
func (in Instance) imagePullPolicy() string {
	if localTiers[strings.ToLower(in.Tier)] {
		return "IfNotPresent"
	}
	return "Always"
}

// enrollmentSecretEntry renders the S1 enrollment credential as an exec-tokens Secret entry (2-space
// indent, trailing newline), or "" so the Secret is byte-identical to before when no credential was
// minted. Absent-vs-empty is deliberate: see Instance.EnrollmentToken.
func enrollmentSecretEntry(tok string) string {
	if strings.TrimSpace(tok) == "" {
		return ""
	}
	return fmt.Sprintf("  ARGUS_ENROLLMENT_TOKEN: %q\n", tok)
}

// imagePullSecretBlock renders the pod-spec imagePullSecrets entry (6-space indent, trailing
// newline) when a secret name is set, or "" so the spec is unchanged. Managed tiers pull the
// private suite image and reference `ghcr-pull`; local tiers import the image and get "".
func imagePullSecretBlock(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	return fmt.Sprintf("      imagePullSecrets:\n        - name: %s\n", name)
}

// pullSecretRegistryAnnotationBlock renders the executor Deployment's own record of which registry
// its ImagePullSecret was made for — the registry host of the image THIS render pins the executor to
// (registryhost.HostOfImage(image)), so the update guard (internal/runner/updateguard.go) can decide
// registry coverage from a read it already has RBAC for (get/patch on its OWN Deployment) instead of
// a Secret read.
//
// V19-007 (withdrawing V19-006): the guard used to read the pull secret's OWN dockerconfigjson
// `auths` keys over the API — which required granting the executor `get` on a Secret, even scoped to
// one name and keys-only. The orchestrator ruled that trade wrong: the executor runs test workloads
// and should hold NO read access to a registry credential, and the ghcr-pull token is shared across
// every tester (some of those Secrets also carry a full credential copy in their
// last-applied-configuration annotation — "keys only" did not bound the exposure the way it read).
// This annotation moves the decision to RENDER time, where the answer is already known: onboarding
// only ever creates/wires a pull secret FOR the registry the image it is deploying came from.
//
// 2-space indent (a sibling of `labels:` under Deployment `metadata:`), ending in a trailing newline
// so it composes with the following `spec:` line unchanged. Empty when no ImagePullSecret is set
// (local tiers import the image and reference no secret at all) — same "" contract as
// imagePullSecretBlock, so a render with no pull secret changes nothing about this Deployment.
func pullSecretRegistryAnnotationBlock(image, imagePullSecret string) string {
	if strings.TrimSpace(imagePullSecret) == "" {
		return ""
	}
	return fmt.Sprintf("  annotations:\n    %s: %q\n", registryhost.PullSecretRegistriesAnnotation, registryhost.HostOfImage(image))
}

// webhookEnvBlock renders the WEBHOOK_URL env entry (12-space indent, trailing newline) when a
// webhook URL is set, or "" so the env block is unchanged. Kept in the exec-tokens-less plain env:
// the webhook sink URL is not a secret.
func webhookEnvBlock(url string) string {
	if url == "" {
		return ""
	}
	return fmt.Sprintf("            - name: WEBHOOK_URL\n              value: %q\n", url)
}

// obsModeEnvBlock renders the executor's ARGUS_OBS_MODE env entry (12-space indent, trailing newline)
// for `--obs none` ONLY, and "" for every other mode, so every other render is byte-unchanged.
// the executor's own validate_config (the relayed verb and runner__validate_config)
// reads it and then does not require observability.grafana.public_url, exactly as onboarding's did.
func obsModeEnvBlock(in Instance) string {
	if !strings.EqualFold(strings.TrimSpace(in.ObsMode), obsModeNone) {
		return ""
	}
	return "            - name: ARGUS_OBS_MODE\n              value: \"none\"\n"
}

// grafanaEnvBlock renders the ARGUS_GRAFANA_PUBLIC_URL env entry (12-space indent, trailing
// newline) when a base is set, or "" so the env block is unchanged. See Instance.GrafanaPublicURL.
func grafanaEnvBlock(u string) string {
	if strings.TrimSpace(u) == "" {
		return ""
	}
	return fmt.Sprintf("            - name: ARGUS_GRAFANA_PUBLIC_URL\n              value: %q\n", u)
}

// foldersEnvBlock renders the U7 host-folder trio. Each is omitted individually when empty: a
// present-but-empty env var would make the executor report "" as a real answer, which the control
// plane cannot tell from "this tier does not know" — the absence-is-not-health trap this round keeps
// finding. Absent means absent.
func foldersEnvBlock(productDir, testDir, onboardHost, kitDir, kubeContext, kubeconfig string) string {
	var b strings.Builder
	for _, kv := range []struct{ k, v string }{
		{"ARGUS_PRODUCT_DIR_HOST", productDir},
		{"ARGUS_TEST_DIR_HOST", testDir},
		{"ARGUS_ONBOARD_HOST", onboardHost},
		// VR5-U1: the kit onboarding ran from, so a k8s executor can report it on its poll exactly
		// as it reports the other three. Omitted individually when empty, for the same reason.
		{"ARGUS_KIT_DIR_HOST", kitDir},
		// VR10-U1 (V28-020): the cluster this instance was onboarded against. The update block has to
		// NAME it — without it update.sh acts on whatever context the operator's kubectl points at —
		// and the executor can only report what its own environment was told. Same omit-when-empty
		// rule, and it carries more weight here than anywhere: an EMPTY context is exactly what marks
		// an instance onboarded before 0.3.29, and the block's one placeholder depends on telling
		// that apart from a context that happens to be blank.
		{"ARGUS_KUBE_CONTEXT_HOST", kubeContext},
		{"ARGUS_KUBECONFIG_HOST", kubeconfig},
	} {
		if kv.v == "" {
			continue
		}
		fmt.Fprintf(&b, "            - name: %s\n              value: %q\n", kv.k, kv.v)
	}
	return b.String()
}

// ── G4/S4: the machine identity as a Secret (off the results volume). When a key is provisioned the
// four helpers below render the exec-identity Secret, mount it read-only, and point
// ARGUS_IDENTITY_PATH at the mount; all return the legacy behaviour ("" / the results path) when it
// is empty, so a render without a key is unchanged (backward-compatible).

// identityPath is where the executor reads its Ed25519 key from.
func identityPath(keyB64 string) string {
	if keyB64 == "" {
		return "/results/identity.key" // legacy: self-minted on the shared results PVC
	}
	return "/etc/argus/identity/identity.key" // the read-only exec-identity Secret mount (S4)
}

// identitySecretBlock renders the exec-identity Secret carrying the pre-minted key (base64 → data),
// as its own YAML document. Empty when no key was provided.
func identitySecretBlock(ns, keyB64 string) string {
	if keyB64 == "" {
		return ""
	}
	return fmt.Sprintf(`---
apiVersion: v1
kind: Secret
metadata:
  name: exec-identity
  namespace: %s
type: Opaque
data:
  # The Ed25519 machine identity, provisioned by onboarding (argus keygen) and mounted READ-ONLY
  # (S4: off the results volume). All replicas read the SAME key from this one Secret — no genesis race.
  identity.key: %s
`, ns, keyB64)
}

// identityMountBlock / identityVolumeBlock mount that Secret read-only at /etc/argus/identity.
func identityMountBlock(keyB64 string) string {
	if keyB64 == "" {
		return ""
	}
	return "            - {name: identity, mountPath: /etc/argus/identity, readOnly: true}\n"
}

func identityVolumeBlock(keyB64 string) string {
	if keyB64 == "" {
		return ""
	}
	return "        - name: identity\n          secret:\n            secretName: exec-identity\n            items: [{key: identity.key, path: identity.key}]\n"
}

// WebhookURLFor returns the executor's WEBHOOK_URL: the SUT's `external.webhook_base_url` with a
// BARE host rewritten to its in-cluster FQDN, so a scenario's ${WEBHOOK_URL} resolves DIRECTLY
// without depending on the webhook ExternalName alias (U9 — "type the external block so
// external.webhook_base_url flows into ${WEBHOOK_URL}"). Returns "" when the config declares no
// webhook sink (the executor then keeps whatever WEBHOOK_URL the environment already provides).
func WebhookURLFor(c *config.Config, sutNamespace string) string {
	if c == nil || sutNamespace == "" {
		return ""
	}
	raw := c.ExternalWebhookBaseURL()
	if raw == "" {
		return ""
	}
	h := hostOf(raw)
	if h == "" || !bareHost(h) {
		return raw // already an FQDN / IP / localhost — leave it untouched
	}
	return strings.Replace(raw, h, h+"."+sutNamespace+".svc.cluster.local", 1)
}

// DeriveAliases finds every BARE service hostname the SUT's argus-config declares and maps it
// to the in-cluster FQDN in the SUT's namespace.
//
// This is what lets ONE argus-config serve both tiers unchanged. A compose config names the SUT
// by bare service name (`http://order-api:8080`); inside the executor's own namespace that name
// does not resolve, and the hand-filled k3d template solved it by rewriting every URL to an FQDN
// by hand — which is exactly the manual editing the generic path must not require. Rendering an
// ExternalName Service per bare host makes the compose names resolve, so the same scenarios and
// the same config run on compose and k3d and any difference in result is a real difference.
func DeriveAliases(c *config.Config, sutNamespace string) map[string]string {
	out := map[string]string{}
	if c == nil || sutNamespace == "" {
		return out
	}
	var raws []string
	if c.Targets.HTTP != nil {
		raws = append(raws, c.Targets.HTTP.BaseURL)
	}
	if c.Targets.Database != nil {
		raws = append(raws, c.Targets.Database.JDBCURL)
	}
	if c.Targets.MessageBroker != nil {
		raws = append(raws, c.Targets.MessageBroker.URL, c.Targets.MessageBroker.ManagementURL)
	}
	if c.Targets.MCP != nil {
		raws = append(raws, c.Targets.MCP.BaseURL)
	}
	// VR10-S3-10: every NAMED entry's URL fields too — the same fields per kind as the plain slot.
	// On compose nothing is needed (the runner joins the SUT network), so a named target that is
	// missing here resolves on compose and fails on k3d/AKS — the failure a compose-only test misses.
	for _, t := range c.Targets.HTTPTargets {
		if t != nil {
			raws = append(raws, t.BaseURL)
		}
	}
	for _, t := range c.Targets.DatabaseTargets {
		if t != nil {
			raws = append(raws, t.JDBCURL)
		}
	}
	for _, t := range c.Targets.MessageBrokerTargets {
		if t != nil {
			raws = append(raws, t.URL, t.ManagementURL)
		}
	}
	for _, t := range c.Targets.MCPTargets {
		if t != nil {
			raws = append(raws, t.BaseURL)
		}
	}
	// The `external` block is parsed untyped, so walk it for anything URL-shaped.
	raws = append(raws, walkURLs(c.Targets.External)...)

	for _, r := range raws {
		if h := hostOf(r); bareHost(h) {
			out[h] = h + "." + sutNamespace + ".svc.cluster.local"
		}
	}
	return out
}

// walkURLs collects every string that looks like a URL from an untyped YAML subtree.
func walkURLs(v any) []string {
	var out []string
	switch t := v.(type) {
	case string:
		if strings.Contains(t, "://") {
			out = append(out, t)
		}
	case map[string]any:
		for _, k := range sortedAnyKeys(t) {
			out = append(out, walkURLs(t[k])...)
		}
	case map[any]any:
		for _, vv := range t {
			out = append(out, walkURLs(vv)...)
		}
	case []any:
		for _, vv := range t {
			out = append(out, walkURLs(vv)...)
		}
	}
	return out
}

func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// secretEnvBlock renders the SUT's ${VAR} values as additional stringData entries, sorted so the
// manifest is byte-stable across runs (a manifest that reorders produces spurious kubectl diff
// noise and defeats change review).
func secretEnvBlock(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("  # The ${VAR}s this SUT's argus-config references. They live HERE, in the Secret,\n")
	b.WriteString("  # and reach the config only by expansion at load time — never inline in the ConfigMap.\n")
	for _, k := range sortedKeys(m) {
		fmt.Fprintf(&b, "  %s: %q\n", k, m[k])
	}
	return b.String()
}

// obsIngressPolicy grants the OBS PODS — and only them — ingress from outside the cluster, on a
// local tier. Without it the instance's default-deny leaves the published NodePorts unreachable:
// the query layer is not a pod in this namespace, so `allow-same-namespace` denies it.
//
// This was found the hard way and is worth stating precisely, because the failure looked like a
// networking problem and was actually a policy working correctly. On this cluster kube-router
// enforces NetworkPolicy, and a curl from the Grafana container to the Loki NodePort measured:
//
//	with the policies    -> 000 (refused)
//	with them removed    -> 200 (ready)
//
// That is the negative control proving enforcement is REAL here, not just declared.
//
// The exception is deliberately narrow: it selects the obs pods by label and their ports only.
// The EXECUTOR keeps default-deny with no such carve-out — S6 — so the same experiment run
// against the executor must still fail. Managed tiers get no policy at all here, because their
// obs plane is not published in the first place.
// obsPodMonitor renders the PodMonitor that makes the MANAGED tier's metrics reachable at all
// (INT-015, 2026-08-07). Empty on the local tiers, which are scraped by the off-cluster compose
// Prometheus through targets.d file_sd — a mechanism that has no equivalent inside a cluster.
//
// The defect this closes: onboarding assumed "the cluster's kube-prometheus-stack already scrapes
// namespace-wide, so the pushgateway is discovered without a targets.d file". Namespace-wide is the
// wrong half of true. The operator's namespaceSelector really is {} (every namespace), but its
// serviceMonitorSelector/podMonitorSelector is matchLabels{release: kube-prometheus-stack} —
// discovery is by LABELLED CRD, never by namespace membership. With neither object rendered,
// nothing scraped the instance's pushgateway, and EVERY metric panel on the managed dashboard was
// empty forever, next to a Loki datasource that worked. Logs worked; metrics did not; so it read as
// a rendering glitch rather than a missing pipeline. Measured live on social-aks-v1:
// /api/v1/label/argus_instance/values returned [] while its pushgateway held 57 argus_* series.
//
// Why a PodMonitor and not a ServiceMonitor: a ServiceMonitor selects on the SERVICE's OWN
// metadata.labels, and the rendered pushgateway Service carries none — only its spec.selector says
// app=pushgateway. Proven the hard way: a ServiceMonitor with matchLabels{app: pushgateway} IS
// discovered by the operator and then drops every target. The POD does carry app=pushgateway, so a
// PodMonitor matches what is actually deployed without also changing the Service.
//
// honorLabels is REQUIRED, mirroring honor_labels in prometheus.obs-shared.yml: the executor pushes
// argus_instance/project/cluster/job, and without it Prometheus overwrites instance and job with the
// pod address — the dashboard's argus_instance filter then matches nothing and the panels stay empty
// in a new and more confusing way.
func obsPodMonitor(in Instance, ns string) string {
	if localTiers[strings.ToLower(in.Tier)] {
		return ""
	}
	// PromOperatorLabel "none": the cluster has no Prometheus Operator, so a PodMonitor cannot even be
	// created — `kubectl create` fails on it with "no matches for kind PodMonitor" (a homelab k3s on the
	// managed tier, 2026-09-23). The renderer never sees the cluster, so the operator says so explicitly.
	// Metrics then have no in-cluster scraper, which is the honest state of such a cluster.
	if strings.EqualFold(strings.TrimSpace(in.PromOperatorLabel), "none") {
		return ""
	}
	k, v := "release", "kube-prometheus-stack"
	if s := strings.TrimSpace(in.PromOperatorLabel); s != "" {
		if i := strings.Index(s, "="); i > 0 {
			k, v = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
		}
	}
	return fmt.Sprintf(`---
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: argus-%s
  namespace: %s
  labels: {%s: %s}   # INT-015: this label IS the operator's entire selector — without it, invisible
spec:
  selector: {matchLabels: {app: pushgateway}}
  podMetricsEndpoints:
    - port: http          # the port NAME. A numeric port here silently selects nothing.
      honorLabels: true   # the pushed argus_instance/job labels must WIN over the pod address
      interval: 15s
`, in.ID, ns, k, v)
}

func obsIngressPolicy(in Instance, ns string) string {
	if !localTiers[strings.ToLower(in.Tier)] {
		return ""
	}
	return fmt.Sprintf(`---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-obs-ingress
  namespace: %s
spec:
  # ONLY the obs pods. The executor is deliberately absent: it stays default-deny (S6), and the
  # negative control that proves this policy works must still fail when aimed at the executor.
  podSelector:
    matchExpressions:
      - {key: app, operator: In, values: [loki, pushgateway]}
  policyTypes: [Ingress]
  ingress:
    - ports:
        - {protocol: TCP, port: 3100}   # loki query API  (the shared Grafana datasource)
        - {protocol: TCP, port: 9091}   # pushgateway     (the shared Prometheus scrape)
`, ns)
}

// localTiers are the clusters that live on the operator's own machine, where the shared query
// layer (Grafana + Prometheus) runs OUTSIDE the cluster and must still reach the instance's obs
// plane. Anything else is treated as managed: keep the obs plane ClusterIP-only.
var localTiers = map[string]bool{"k3d": true, "kind": true, "minikube": true}

// LocalTier reports whether t is one of the tiers that live on the operator's own machine (k3d,
// kind, minikube) — exported so a caller outside this package (T2.2: cmdRenderK8s's managed-tier
// storage warning) can ask the same question normalize() answers internally, instead of keeping a
// second, driftable copy of the three names.
func LocalTier(t string) bool { return localTiers[strings.ToLower(strings.TrimSpace(t))] }

// obsServiceType is the Service type for the instance's Loki/pushgateway.
//
// The distinction is deliberate and load-bearing. On a LOCAL cluster the query layer is a compose
// project on the same host, so a ClusterIP is unreachable (measured: a curl from the Grafana
// container to the Loki ClusterIP returned 000) and the obs plane must be published — the same
// trust boundary the compose Loki already occupies. On a MANAGED cluster, publishing an
// unauthenticated log store on every node would be a genuine exposure, so it stays ClusterIP and
// Stage III wires the query layer differently.
func (in Instance) obsServiceType() string {
	if localTiers[strings.ToLower(in.Tier)] {
		return "NodePort"
	}
	return "ClusterIP"
}

// sortedKeys gives deterministic rendering — a manifest that reorders between runs produces
// spurious `kubectl diff` noise and defeats change review.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// lokiConfigYAML is the Loki config EVERY tier runs (single binary, filesystem store under /loki: a
// PersistentVolumeClaim on the k8s tiers, the `loki-data` named volume on compose). The compose Loki
// mounts deploy/compose/loki-config.yaml, which TestComposeLoki_configFileIsTheRenderersConfigByteForByte
// holds equal to this constant: a persistent volume under a config with retention off would grow
// without bound. It replaces the image's baked /etc/loki/local-config.yaml, which leaves deletes and
// retention OFF: there /loki/api/v1/delete answers 404 and lines never age out, so the only way to
// remove a line (e.g. one carrying an e-mail address) was to wipe the whole store (memstore-dev,
// 2026-09-19). Here:
//   - allow_deletes + a filesystem delete_request_store: POST /loki/api/v1/delete works, per selector
//     and time range; deletion_mode filter-and-delete hides matching lines from queries at once and
//     the compactor removes them from chunks.
//   - delete_request_cancel_period 10m: a request is processed ten minutes after it is made, not the
//     default 24 h.
//   - retention 7d: matches reject_old_samples_max_age (1w), so nothing older than a week is either
//     accepted or kept.
const lokiConfigYAML = `auth_enabled: false
server:
  http_listen_port: 3100
  grpc_listen_port: 9096
  log_level: info
common:
  instance_addr: 127.0.0.1
  path_prefix: /loki
  storage:
    filesystem:
      chunks_directory: /loki/chunks
      rules_directory: /loki/rules
  replication_factor: 1
  ring:
    kvstore:
      store: inmemory
schema_config:
  configs:
    - from: 2020-10-24
      store: tsdb
      object_store: filesystem
      schema: v13
      index:
        prefix: index_
        period: 24h
compactor:
  working_directory: /loki/compactor
  compaction_interval: 10m
  retention_enabled: true
  retention_delete_delay: 2h
  delete_request_store: filesystem
  delete_request_cancel_period: 10m
limits_config:
  allow_deletes: true
  deletion_mode: filter-and-delete
  retention_period: 168h
  reject_old_samples: true
  reject_old_samples_max_age: 168h
`

// The sizes of the observability volumes, held in ONE place. Loki keeps 168h (lokiConfigYAML); 5Gi is
// a rendered request, not a measured need. The Pushgateway holds one small metric group per push.
const (
	obsLokiVolumeSize        = "5Gi"
	obsPushgatewayVolumeSize = "1Gi"
)

// obsStorageClassFor is the default StorageClass of the observability volumes on a tier.
// Deliberately NOT the results volume's class (argus-rwx / azurefile-csi): that one is a ReadWriteMany
// FILE share so the min-3 executors can spread across nodes, while Loki runs ONE replica and its
// filesystem store on a network file share (NFS/SMB) is what Loki's own documentation warns against, and
// the Pushgateway's file is likewise single-writer. So these are block-backed ReadWriteOnce:
//
//   - aks -> managed-csi (Azure Disk), a class AKS always ships;
//   - EVERY other tier -> "" = NO storageClassName in the claim, so the cluster's own default StorageClass
//     binds it. No class name is right everywhere (kind and minikube call theirs "standard", EKS and GKE
//     have their own, k3d's default IS local-path), and a claim naming a class the cluster lacks sits
//     Pending forever.
//
// onboard.sh's preflight (obs_storage_preflight) checks the cluster can bind the claim; its tier->class
// rule is held equal to this function by TestOnboardSh_ObsStoragePreflightWantsWhatTheRendererRenders.
func obsStorageClassFor(tier string) string {
	if strings.EqualFold(strings.TrimSpace(tier), "aks") {
		return "managed-csi"
	}
	return ""
}

// obsPVCDoc renders one observability PersistentVolumeClaim as a YAML document (leading `---`).
// ReadWriteOnce always: a single pod writes each of these volumes. Class and size are IMMUTABLE once
// the claim is bound — a re-apply with another class is refused by the API server, never migrated.
// An empty class OMITS storageClassName (the cluster default applies); `storageClassName: ""` would
// instead mean "no class at all" and never provision.
func obsPVCDoc(name, ns, class, size, app string) string {
	classLine := ""
	if class != "" {
		classLine = "  storageClassName: " + class + "\n"
	}
	return fmt.Sprintf(`---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: %s, namespace: %s, labels: {app: %s}}
spec:
  accessModes: ["ReadWriteOnce"]
%s  resources:
    requests:
      storage: %s
`, name, ns, app, classLine, size)
}

// obsRecreateStrategy is the rollout strategy of every Deployment that owns a ReadWriteOnce volume.
// The default RollingUpdate starts the NEW pod before the old one is gone; a ReadWriteOnce volume
// (Azure Disk, local-path) is attachable to one node's pod at a time, so the new pod can sit
// ContainerCreating on a Multi-Attach error and the rollout never finishes. Recreate stops the old pod
// first. `rollingUpdate: null` is NOT what makes a client-side `kubectl apply` switch a live Deployment
// to Recreate: spec.strategy carries `patchStrategy: retainKeys`, which drops the live rollingUpdate
// when `type` changes (internal/k8supgrade addRetainKeys relies on the same thing). It is kept because
// it is harmless there and is what keeps the object valid where retainKeys does not apply: under
// server-side apply, and in the patch body `argus upgrade --apply` sends, a Recreate that still carries
// a rollingUpdate block is refused by the API server.
const obsRecreateStrategy = `  strategy:
    type: Recreate
    rollingUpdate: null
`

// lokiPodSecurity runs Loki as the user the grafana/loki image runs as (10001) and makes the fresh
// volume group-writable by it (fsGroup). Not root: the compose Pushgateway runs as root only because a
// fresh Docker named volume mounts root-owned, and a PVC with fsGroup has no such problem.
const lokiPodSecurity = `      securityContext: {runAsNonRoot: true, runAsUser: 10001, runAsGroup: 10001, fsGroup: 10001}
`

// pushgatewayPodSecurity: prom/pushgateway runs as nobody (65534); same reasoning as lokiPodSecurity.
const pushgatewayPodSecurity = `      securityContext: {runAsNonRoot: true, runAsUser: 65534, runAsGroup: 65534, fsGroup: 65534}
`

// pushgatewayPersistArgs is shared by both Pushgateway renderers (bundled and shared), so the two
// cannot drift. The Pushgateway writes its file every minute and on shutdown.
const pushgatewayPersistArgs = `          args: ["--persistence.file=/pushgateway/data", "--persistence.interval=1m"]
`

// lokiConfigSHA256 stamps the Loki pod template so a changed config rolls the pod on apply.
func lokiConfigSHA256() string {
	sum := sha256.Sum256([]byte(lokiConfigYAML))
	return hex.EncodeToString(sum[:])
}

// RenderObs renders the instance-scoped observability plane (Option-C): Loki + pushgateway +
// promtail, living and dying with the instance namespace. The promtail pipeline is derived
// from the SUT's declared translation table, exactly as obsconfig.RenderPromtail does for
// compose — the two must agree or the same SUT reads differently on the two tiers.
func RenderObs(in Instance, c *config.Config) (string, error) {
	in.normalize()
	if err := in.validate(); err != nil {
		return "", err
	}
	if c == nil {
		return "", fmt.Errorf("argus config is required to render the obs plane (it carries the log-field translation table)")
	}
	// none: no observability object of ANY kind — no Loki, no pushgateway, no promtail, no
	// PodMonitor, no obs ingress NetworkPolicy. Checked FIRST, ahead of adopt/export/shared, since
	// it overrides every one of their own "deploy less" behaviours with "deploy nothing at all".
	if strings.EqualFold(in.ObsMode, obsModeNone) {
		return "", nil
	}
	// T3.2 (E3 adopt mode): Argus deploys NO observability of its own — no Loki, no pushgateway,
	// no promtail, no obs PodMonitor, no obs ingress NetworkPolicy. The executor's --loki/
	// --pushgateway args already point at the operator's own endpoints (RenderExecutor,
	// obsExecArgsBlock); there is nothing left for this function to render. EMPTY, not a
	// comment-only stub — an operator or a test diffing obs.yaml should see nothing, not a
	// document that LOOKS empty but carries a preamble.
	if strings.EqualFold(in.ObsMode, obsModeAdopt) {
		return "", nil
	}
	// T3.1 (E3 export mode): validate() has already refused an export run that declares neither
	// accepted shape, so by the time we get here exactly one of these two branches applies.
	if strings.EqualFold(in.ObsMode, obsModeExport) {
		// BetterStack target: Argus deploys NOTHING, not even promtail — log shipping into
		// BetterStack is the operator's OWN pipeline (documented in docs/DEPLOY-ARGUS.md and
		// onboard.sh's printed summary), and the executor reads evidence back over BetterStack's
		// own SQL API (obsquery.BetterStack), which needs no in-cluster forwarder.
		if in.ObsUseBetterStack {
			return "", nil
		}
		// Hosted-Loki target: promtail ONLY, forwarding to the operator's hosted push endpoint.
		// Without SUT log collection there is nothing to forward — the whole point of this target
		// IS the SUT's logs — so an export run with --collect-sut-logs unset renders nothing either,
		// same as BetterStack above, rather than standing up a promtail with no consenting scrape.
		if !in.CollectSUTLogs {
			return "", nil
		}
		return renderExportPromtail(in, c), nil
	}
	// T3.3 (E3 shared ingest): no Loki of the instance's own — promtail writes to the environment's
	// shared Loki as tenant <instance-id>; the pushgateway stays per-instance (out of scope).
	if strings.EqualFold(in.ObsMode, obsModeShared) {
		return renderSharedInstanceObs(in, c), nil
	}
	ns := in.Namespace()
	obsType := in.obsServiceType()
	obsPolicy := obsIngressPolicy(in, ns)

	var b strings.Builder
	fmt.Fprintf(&b, `# RENDERED by the argus onboarder (internal/k8srender) for instance %q. Do not edit by hand.
# Instance-scoped obs (Option-C): Loki + pushgateway in the INSTANCE namespace, so they live and
# die with it (P2/P8 isolation + clean teardown). The cluster-scoped brought-Prometheus and the
# shared Grafana are a separate concern. promtail (the SUT-namespace log collector) is a SEPARATE,
# OPT-IN block below — see --collect-sut-logs — because reading a SUT's own pod logs is a
# privacy decision neither Loki nor the pushgateway need the tester to make.
apiVersion: v1
kind: ConfigMap
metadata: {name: loki-config, namespace: %s}
data:
  loki.yaml: |
%s
%s---
apiVersion: apps/v1
kind: Deployment
metadata: {name: loki, namespace: %s, labels: {app: loki}}
spec:
  replicas: 1
%s  selector: {matchLabels: {app: loki}}
  template:
    metadata:
      labels: {app: loki}
      # A config change must roll the pod: Loki reads its config only at startup.
      annotations: {argus.onedroid.ai/loki-config-sha256: "%s"}
    spec:
%s      containers:
        - name: loki
          # PINNED, tag@digest: lokiConfigYAML is written against this Loki's schema, and ":latest" would
          # let a major bump break it on the next pod restart. Digest = what every live instance ran 2026-09-23.
          image: grafana/loki:3.7.8@sha256:1107dd5274e0ada47e42472b7a7e71f3b2a2fe878878108f3e2f9e51528f0193
          # OUR config, not the image's baked local-config.yaml: that one has deletes and retention OFF,
          # so a log line (e.g. one carrying PII) could never be purged short of wiping the whole store.
          args: ["-config.file=/etc/loki-argus/loki.yaml"]
          ports: [{containerPort: 3100, name: http}]
          # REQUIRED, not optional: the instance ResourceQuota declares limits.cpu/limits.memory,
          # and a quota that sets a limit makes that limit MANDATORY for every pod in the
          # namespace — a pod without one is rejected outright ("must specify limits.cpu for:
          # loki"). Found live: the obs pods never started while the executor did, because only
          # the executor happened to declare limits. A server-side dry-run cannot catch this,
          # since a Deployment dry-run never creates a Pod and the quota is enforced at Pod
          # creation by the ReplicaSet controller.
          resources:
            requests: {cpu: "50m", memory: "128Mi"}
            limits:   {cpu: "500m", memory: "512Mi"}
          # GENEROUS readiness: in k8s readiness GATES traffic, and a slow WAL replay would
          # otherwise leave Loki NotReady and silently break the saga/logs panels.
          readinessProbe: {httpGet: {path: /ready, port: http}, initialDelaySeconds: 15, periodSeconds: 10, failureThreshold: 30}
          volumeMounts:
            - {name: data, mountPath: /loki}
            - {name: config, mountPath: /etc/loki-argus, readOnly: true}
      volumes:
        # The loki-data PVC, not an emptyDir: a pod recreate must not take the run logs with it
        #. Retention (lokiConfigYAML) is what bounds its size.
        - {name: data, persistentVolumeClaim: {claimName: loki-data}}
        - {name: config, configMap: {name: loki-config}}
---
apiVersion: v1
kind: Service
metadata: {name: loki, namespace: %s}
spec:
  # U5: the shared Grafana lives OFF the cluster (it is a compose project serving every tier), and a
  # ClusterIP is not routable from there — proven, not assumed: a curl from the Grafana container to
  # this Service's ClusterIP returned 000. On a LOCAL cluster the Service is therefore published as a
  # NodePort so the off-cluster query layer can reach it. On a MANAGED cluster it stays ClusterIP;
  # publishing an unauthenticated log store off-cluster there would be a real exposure, and Stage III
  # solves it differently (in-cluster Grafana, or an authenticated ingress).
  #
  # This is NOT an S6 exception: S6 governs the EXECUTOR, which stays ClusterIP-only on every tier.
  # Loki is observability, and on a local dev cluster it sits on exactly the trust boundary the
  # compose Loki already occupies — localhost.
  type: %s
  selector: {app: loki}
  ports: [{name: http, port: 3100, targetPort: http}]
%s---
apiVersion: apps/v1
kind: Deployment
metadata: {name: pushgateway, namespace: %s, labels: {app: pushgateway}}
spec:
  replicas: 1
%s  selector: {matchLabels: {app: pushgateway}}
  template:
    metadata: {labels: {app: pushgateway}}
    spec:
%s      containers:
        - name: pushgateway
          image: prom/pushgateway:v1.11.3@sha256:74fa117cef2d7e383112d25139ff1c2d2e309c35389a9e0554a47136a1482e48
%s          ports: [{containerPort: 9091, name: http}]
          readinessProbe: {httpGet: {path: /-/ready, port: http}, initialDelaySeconds: 5, periodSeconds: 10}
          resources:                                    # mandatory under the instance ResourceQuota
            requests: {cpu: "25m", memory: "64Mi"}
            limits:   {cpu: "200m", memory: "256Mi"}
          volumeMounts:
            - {name: data, mountPath: /pushgateway}
      volumes:
        - {name: data, persistentVolumeClaim: {claimName: pushgateway-data}}
---
apiVersion: v1
kind: Service
metadata: {name: pushgateway, namespace: %s}
spec:
  type: %s          # same reasoning as loki above — the shared Prometheus scrapes from off-cluster
  selector: {app: pushgateway}
  ports: [{name: http, port: 9091, targetPort: http}]
`,
		in.ID,
		ns,                        // loki-config ConfigMap
		indent(lokiConfigYAML, 4), // its loki.yaml
		obsPVCDoc("loki-data", ns, in.ObsStorageClass, obsLokiVolumeSize, "loki"),
		ns,                  // loki Deployment
		obsRecreateStrategy, // RWO volume: never a surge pod
		lokiConfigSHA256(),  // its roll-on-change annotation
		lokiPodSecurity,
		ns, obsType, // loki Service        (namespace, type)
		obsPVCDoc("pushgateway-data", ns, in.ObsStorageClass, obsPushgatewayVolumeSize, "pushgateway"),
		ns, // pushgateway Deployment
		obsRecreateStrategy,
		pushgatewayPodSecurity,
		pushgatewayPersistArgs,
		ns, obsType, // pushgateway Service (namespace, type)
	)

	// promtail (the SUT-namespace pod-log collector) is OPT-IN (--collect-sut-logs, default off):
	// its ONLY job is reading /var/log/pods/<SUTNamespace>_*, and a SUT's logs may carry user or
	// agent content the tester never opted into shipping into Loki. Appended as a whole, separate
	// block of independent YAML documents — document order carries no meaning to `kubectl create`,
	// so this can be added or omitted without touching Loki/pushgateway above.
	if in.CollectSUTLogs {
		b.WriteString(renderBundledPromtailBlock(in, c, ns))
	}
	// Empty on a managed tier, where the obs plane is not published and needs no carve-out.
	b.WriteString(obsPolicy)
	// INT-015: and the mirror image — empty on the LOCAL tiers, which targets.d file_sd already
	// covers. Without this the managed tier has no metrics pipeline at all.
	b.WriteString(obsPodMonitor(in, ns))
	return b.String(), nil
}

// renderBundledPromtailBlock renders promtail's ServiceAccount, its local-tier cluster RBAC, its
// ConfigMap (the SUT-namespace log-scrape pipeline: static path glob, parse stage, saga-marker
// normalization — identical to the export/shared promtail pipelines, see their own comments) and
// its DaemonSet. Called only when Instance.CollectSUTLogs is true (RenderObs).
func renderBundledPromtailBlock(in Instance, c *config.Config, ns string) string {
	// Cluster-scoped RBAC MUST be per-instance-named or a second instance silently rebinds
	// the first one's ClusterRole.
	rbacName := "argus-promtail-" + in.ID
	// The `project` label must match the METRIC project label (push.go) and the dashboard's
	// `project` variable, else the dashboard's Loki panels and its metric panels scope to
	// different things and one of them renders empty. That is why this is ProjectLabel() and
	// not the SUT namespace (the hand-filled template used the namespace, which does not match).
	project := c.ProjectLabel()

	var b strings.Builder
	fmt.Fprintf(&b, `---
apiVersion: v1
kind: ServiceAccount
metadata: {name: promtail, namespace: %s}
---
%sapiVersion: v1
kind: ConfigMap
metadata: {name: promtail-config, namespace: %s}
data:
  config.yml: |
    server:
      http_listen_port: 9080
    positions:
      filename: /run/promtail/positions.yaml
    clients:
      - url: http://loki:3100/loki/api/v1/push
        # Flush near-instantly: a saga-presence scenario queries Loki within its window right
        # after the SUT emits the saga, so promtail must not sit on the batch.
        batchwait: 100ms
        batchsize: 65536
    scrape_configs:
      # STATIC path-glob over THIS instance's SUT namespace pod logs. k3d/containerd writes them
      # to /var/log/pods/<ns>_<pod>_<uid>/<container>/*.log. This is deliberately NOT
      # kubernetes_sd: its __path__ relabel yielded 0 targets. Isolation is by the namespace glob.
      - job_name: sut-logs
        static_configs:
          - targets: [localhost]
            labels:
              argus_instance: %s
              project: %s
              cluster: %s
              __path__: /var/log/pods/%s_*/*/*.log
        pipeline_stages:
          - cri: {}     # strip the CRI envelope (ts stream _P/F) -> the raw log line
          # Derive "service" from the container dir in the log path: the saga-presence query is
          # {service=~".+", …}, so a non-empty per-container service label is REQUIRED.
          - regex:
              source: filename
              expression: '/[^/]+/(?P<service>[^/]+)/[0-9]+\.log$'
          - labels:
              service:
%s
%s
          # NORMALIZE this SUT's saga marker to the CANONICAL event_type="saga", so the SHARED
          # dashboard can filter event_type=~"saga" for every SUT. Any other value of the field
          # passes through unchanged, so it never masquerades as a saga.
          # The default-empty pipe: a line with NO saga field must yield an EMPTY event_type, not the
          # Go template literal that spelled out no-value — that literal was showing up as a spurious
          # event_type bucket on the dashboard (U9). A real marker value still passes through.
          - template:
              source: event_type
              template: '{{ if %s }}saga{{ else }}{{ .saga_marker | default "" }}{{ end }}'
          - labels:
              level:
              correlation_id:
              event_type:
---
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: promtail, namespace: %s, labels: {app: promtail}}
spec:
  selector: {matchLabels: {app: promtail}}
  template:
    metadata: {labels: {app: promtail}}
    spec:
      serviceAccountName: promtail
      containers:
        - name: promtail
          image: grafana/promtail:3.6.8@sha256:6cfa64ec432b24a912d640e2edb940eeae2666f61861a66c121d763dd7241381
          args: ["-config.file=/etc/promtail/config.yml"]
          resources:                                    # mandatory under the instance ResourceQuota
            requests: {cpu: "25m", memory: "64Mi"}
            limits:   {cpu: "200m", memory: "256Mi"}
          volumeMounts:
            - {name: config, mountPath: /etc/promtail}
            - {name: run, mountPath: /run/promtail}
            - {name: pods, mountPath: /var/log/pods, readOnly: true}
            - {name: containers, mountPath: /var/log/containers, readOnly: true}
      volumes:
        - {name: config, configMap: {name: promtail-config}}
        - %s
        - {name: pods, hostPath: {path: /var/log/pods}}
        - {name: containers, hostPath: {path: /var/log/containers}}
`,
		ns,
		promtailRBAC(in, rbacName, ns),
		ns, // promtail-config ConfigMap
		in.ID, project, in.Cluster, in.SUTNamespace,
		parseStage(c),
		indent(obsconfig.CorrelationLabelStage, 4), // AC-D32: only tr- ids become a label, same stage as compose
		sagaMarkerCond(c),
		ns,
		promtailRunVolume(ns))
	return b.String()
}

// promtailPushClientURL builds the `clients: - url:` promtail pushes to for a hosted-Loki export
// target (T3.1): pushURL with the credential ${VAR} inserted as URL USERINFO right after the
// scheme — `https://${VARNAME}@host/path` — the same convention Grafana Cloud's own promtail
// examples use for Basic Auth over a plain URL. This is deliberately NOT promtail's
// `basic_auth: {username, password}` block: the credential resolves to a single "user:password"
// string (same shape as observability.betterstack.credential), and there is no way to split that
// into two separate fields using only promtail's `-config.expand-env=true` var expansion, which
// substitutes whole ${VAR} tokens and does nothing else. The userinfo form needs no split and
// promtail (like any net/url-based HTTP client) reads Basic Auth out of it automatically.
//
// The rendered manifest NEVER carries the resolved value — only the literal `${varName}` text,
// exactly as `-config.expand-env=true` requires; promtail's own process environment resolves it
// at startup from the argus-obs-credential Secret (obsCredentialEnvBlock). varName == "" (should
// never happen once validate() has run) returns pushURL unchanged rather than emitting a bare "@".
func promtailPushClientURL(pushURL, varName string) string {
	if strings.TrimSpace(varName) == "" {
		return pushURL
	}
	if i := strings.Index(pushURL, "://"); i >= 0 {
		return pushURL[:i+3] + "${" + varName + "}@" + pushURL[i+3:]
	}
	return pushURL
}

// renderExportPromtail (T3.1, E3 export hosted-Loki target) renders ONLY promtail — its
// ServiceAccount, its local-tier RBAC, its ConfigMap and its DaemonSet — pointed at the
// operator's own hosted push endpoint. No Loki, no pushgateway, no PodMonitor, no obs
// NetworkPolicy: there is nothing else left to deploy, and no in-cluster NodePort/ClusterIP to
// carve an ingress exception for (contrast RenderObs's bundled branch above). The scrape side
// (SUT log glob, parse stage, saga-marker normalization) is UNCHANGED from bundled/adopt — only
// the `clients:` target and its auth differ — so the same SUT reads the same dashboard fields on
// every obs mode.
func renderExportPromtail(in Instance, c *config.Config) string {
	ns := in.Namespace()
	rbacName := "argus-promtail-" + in.ID
	project := c.ProjectLabel()
	clientURL := promtailPushClientURL(in.ObsLokiPushURL, in.ObsCredentialVarName)

	var b strings.Builder
	fmt.Fprintf(&b, `# RENDERED by the argus onboarder (internal/k8srender) for instance %q. Do not edit by hand.
# --obs=export, hosted-Loki target (T3.1/E3): promtail ONLY, forwarding to the operator's own
# hosted push endpoint over HTTP Basic Auth (credential resolved from the argus-obs-credential
# Secret at promtail's own startup via -config.expand-env=true — never a literal here). No Loki,
# no pushgateway, no PodMonitor, no obs NetworkPolicy: Argus deploys the minimum and keeps
# evidence readable from where it went (the executor's own --loki/obsquery reads it back).
apiVersion: v1
kind: ServiceAccount
metadata: {name: promtail, namespace: %s}
---
%sapiVersion: v1
kind: ConfigMap
metadata: {name: promtail-config, namespace: %s}
data:
  config.yml: |
    server:
      http_listen_port: 9080
    positions:
      filename: /run/promtail/positions.yaml
    clients:
      - url: %s
        # Flush near-instantly: a saga-presence scenario queries the hosted Loki within its
        # window right after the SUT emits the saga, so promtail must not sit on the batch —
        # same reasoning and same values as the bundled/adopt promtail config.
        batchwait: 100ms
        batchsize: 65536
    scrape_configs:
      - job_name: sut-logs
        static_configs:
          - targets: [localhost]
            labels:
              argus_instance: %s
              project: %s
              cluster: %s
              __path__: /var/log/pods/%s_*/*/*.log
        pipeline_stages:
          - cri: {}     # strip the CRI envelope (ts stream _P/F) -> the raw log line
          - regex:
              source: filename
              expression: '/[^/]+/(?P<service>[^/]+)/[0-9]+\.log$'
          - labels:
              service:
%s
%s
          - template:
              source: event_type
              template: '{{ if %s }}saga{{ else }}{{ .saga_marker | default "" }}{{ end }}'
          - labels:
              level:
              correlation_id:
              event_type:
---
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: promtail, namespace: %s, labels: {app: promtail}}
spec:
  selector: {matchLabels: {app: promtail}}
  template:
    metadata: {labels: {app: promtail}}
    spec:
      serviceAccountName: promtail
      containers:
        - name: promtail
          image: grafana/promtail:3.6.8@sha256:6cfa64ec432b24a912d640e2edb940eeae2666f61861a66c121d763dd7241381
          # -config.expand-env=true: the ONLY way the ${varName} userinfo in the clients: url above
          # (and nowhere else in this manifest) ever becomes the real credential — resolved from
          # promtail's OWN process environment (the env: entry just below), never from this file.
          args: ["-config.file=/etc/promtail/config.yml", "-config.expand-env=true"]
%s          resources:
            requests: {cpu: "25m", memory: "64Mi"}
            limits:   {cpu: "200m", memory: "256Mi"}
          volumeMounts:
            - {name: config, mountPath: /etc/promtail}
            - {name: run, mountPath: /run/promtail}
            - {name: pods, mountPath: /var/log/pods, readOnly: true}
            - {name: containers, mountPath: /var/log/containers, readOnly: true}
      volumes:
        - {name: config, configMap: {name: promtail-config}}
        - %s
        - {name: pods, hostPath: {path: /var/log/pods}}
        - {name: containers, hostPath: {path: /var/log/containers}}
`,
		in.ID,
		ns,
		promtailRBAC(in, rbacName, ns),
		ns,
		clientURL,
		in.ID, project, in.Cluster, in.SUTNamespace,
		parseStage(c),
		indent(obsconfig.CorrelationLabelStage, 4),
		sagaMarkerCond(c),
		ns,
		promtailEnvBlock(in.ObsCredentialVarName),
		promtailRunVolume(ns),
	)
	return b.String()
}

// promtailEnvBlock renders promtail's WHOLE `env:` key (10-space indent) plus its one credential
// entry (12-space indent, via obsCredentialEnvBlock), or "" when there is no credential var — an
// omitted key, never an `env:` with nothing under it (which some validators read as `env: null`).
func promtailEnvBlock(varName string) string {
	entry := obsCredentialEnvBlock("            ", varName)
	if entry == "" {
		return ""
	}
	return "          env:\n" + entry
}

// renderSharedInstanceObs (T3.3, E3 shared ingest) is an instance's obs plane under --obs=shared:
// promtail + the per-instance pushgateway, and NO Loki. promtail pushes to the environment's shared
// Loki as tenant_id <instance-id>. The scrape side (static labels, parse stage, saga-marker
// normalization) is the bundled one line for line — TestRenderObs_shared_pipelineMatchesBundled pins
// that — so a SUT reads the same on the dashboard whichever mode it was onboarded with.
//
// The argus_instance label stays even though the tenant already separates instances: the shared
// dashboard's variables and panels key on it, and a tenant is invisible to a LogQL selector.
func renderSharedInstanceObs(in Instance, c *config.Config) string {
	ns := in.Namespace()
	obsType := in.obsServiceType()

	var b strings.Builder
	fmt.Fprintf(&b, `# RENDERED by the argus onboarder (internal/k8srender) for instance %q. Do not edit by hand.
# --obs=shared (T3.3/E3): NO Loki of this instance's own. The pushgateway stays per-instance.
# promtail (the SUT-namespace log collector, writing to the environment's ONE shared Loki as
# tenant %q) is a SEPARATE, OPT-IN block below — see --collect-sut-logs.
%s---
apiVersion: apps/v1
kind: Deployment
metadata: {name: pushgateway, namespace: %s, labels: {app: pushgateway}}
spec:
  replicas: 1
%s  selector: {matchLabels: {app: pushgateway}}
  template:
    metadata: {labels: {app: pushgateway}}
    spec:
%s      containers:
        - name: pushgateway
          image: prom/pushgateway:v1.11.3@sha256:74fa117cef2d7e383112d25139ff1c2d2e309c35389a9e0554a47136a1482e48
%s          ports: [{containerPort: 9091, name: http}]
          readinessProbe: {httpGet: {path: /-/ready, port: http}, initialDelaySeconds: 5, periodSeconds: 10}
          resources:                                    # mandatory under the instance ResourceQuota
            requests: {cpu: "25m", memory: "64Mi"}
            limits:   {cpu: "200m", memory: "256Mi"}
          volumeMounts:
            - {name: data, mountPath: /pushgateway}
      volumes:
        - {name: data, persistentVolumeClaim: {claimName: pushgateway-data}}
---
apiVersion: v1
kind: Service
metadata: {name: pushgateway, namespace: %s}
spec:
  type: %s          # same reasoning as bundled — the shared Prometheus scrapes from off-cluster on a local tier
  selector: {app: pushgateway}
  ports: [{name: http, port: 9091, targetPort: http}]
`,
		in.ID, in.ID,
		obsPVCDoc("pushgateway-data", ns, in.ObsStorageClass, obsPushgatewayVolumeSize, "pushgateway"),
		ns, // pushgateway Deployment
		obsRecreateStrategy,
		pushgatewayPodSecurity,
		pushgatewayPersistArgs,
		ns, obsType, // pushgateway Service
	)

	// promtail is OPT-IN (--collect-sut-logs, default off) — same reasoning as bundled mode
	// (RenderObs): its only job is reading the SUT namespace's pod logs.
	if in.CollectSUTLogs {
		rbacName := "argus-promtail-" + in.ID
		project := c.ProjectLabel()
		pushURL := strings.TrimRight(in.ObsSharedURL, "/") + "/loki/api/v1/push"
		fmt.Fprintf(&b, `---
apiVersion: v1
kind: ServiceAccount
metadata: {name: promtail, namespace: %s}
---
%sapiVersion: v1
kind: ConfigMap
metadata: {name: promtail-config, namespace: %s}
data:
  config.yml: |
    server:
      http_listen_port: 9080
    positions:
      filename: /run/promtail/positions.yaml
    clients:
      - url: %s
        # The shared Loki runs auth_enabled: true: every push must name its tenant, and this
        # instance's tenant IS its id. The executor reads with the same value, or it sees nothing.
        tenant_id: %s
        batchwait: 100ms
        batchsize: 65536
    scrape_configs:
      - job_name: sut-logs
        static_configs:
          - targets: [localhost]
            labels:
              argus_instance: %s
              project: %s
              cluster: %s
              __path__: /var/log/pods/%s_*/*/*.log
        pipeline_stages:
          - cri: {}     # strip the CRI envelope (ts stream _P/F) -> the raw log line
          - regex:
              source: filename
              expression: '/[^/]+/(?P<service>[^/]+)/[0-9]+\.log$'
          - labels:
              service:
%s
%s
          - template:
              source: event_type
              template: '{{ if %s }}saga{{ else }}{{ .saga_marker | default "" }}{{ end }}'
          - labels:
              level:
              correlation_id:
              event_type:
---
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: promtail, namespace: %s, labels: {app: promtail}}
spec:
  selector: {matchLabels: {app: promtail}}
  template:
    metadata: {labels: {app: promtail}}
    spec:
      serviceAccountName: promtail
      containers:
        - name: promtail
          image: grafana/promtail:3.6.8@sha256:6cfa64ec432b24a912d640e2edb940eeae2666f61861a66c121d763dd7241381
          args: ["-config.file=/etc/promtail/config.yml"]
          resources:                                    # mandatory under the instance ResourceQuota
            requests: {cpu: "25m", memory: "64Mi"}
            limits:   {cpu: "200m", memory: "256Mi"}
          volumeMounts:
            - {name: config, mountPath: /etc/promtail}
            - {name: run, mountPath: /run/promtail}
            - {name: pods, mountPath: /var/log/pods, readOnly: true}
            - {name: containers, mountPath: /var/log/containers, readOnly: true}
      volumes:
        - {name: config, configMap: {name: promtail-config}}
        - %s
        - {name: pods, hostPath: {path: /var/log/pods}}
        - {name: containers, hostPath: {path: /var/log/containers}}
`,
			ns,
			promtailRBAC(in, rbacName, ns),
			ns, // promtail-config
			pushURL, in.ID,
			in.ID, project, in.Cluster, in.SUTNamespace,
			parseStage(c),
			indent(obsconfig.CorrelationLabelStage, 4),
			sagaMarkerCond(c),
			ns,
			promtailRunVolume(ns))
	}
	// The local-tier carve-out for the pushgateway NodePort only: there is no Loki here to admit.
	if localTiers[strings.ToLower(in.Tier)] {
		fmt.Fprintf(&b, `---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-obs-ingress
  namespace: %s
spec:
  # ONLY the pushgateway (shared mode renders no Loki here). The executor stays default-deny (S6).
  podSelector:
    matchLabels: {app: pushgateway}
  policyTypes: [Ingress]
  ingress:
    - ports:
        - {protocol: TCP, port: 9091}   # pushgateway (the shared Prometheus scrape)
`, ns)
	}
	b.WriteString(obsPodMonitor(in, ns))
	return b.String()
}

// SharedLoki describes the ONE Loki an environment's --obs=shared instances write to (T3.3).
type SharedLoki struct {
	// Tier: a local tier (k3d/kind/minikube) publishes it as a NodePort for the off-cluster Grafana;
	// any other tier keeps it ClusterIP. "compose" is refused.
	Tier string
	// GrafanaNamespace is where the MANAGED shared Grafana runs (the NetworkPolicy's Grafana peer on
	// a managed tier). Default sharedGrafanaNamespaceDefault, the managed overlay's namespace.
	GrafanaNamespace string
	// StorageClass is the StorageClass of the shared Loki's data volume. Empty takes
	// obsStorageClassFor(Tier).
	StorageClass string
}

// sharedGrafanaNamespaceDefault is the managed Grafana's namespace (k8s/overlays/*/obs-grafana.yaml,
// and onboard.sh's ${ARGUS_CP_NAMESPACE:-argus-dev}).
const sharedGrafanaNamespaceDefault = "argus-dev"

// localPodCIDR is k3s's default cluster CIDR. On a local tier the shared Grafana is OFF-cluster and
// reaches Loki through its NodePort, so no namespace selector can name it; the policy admits sources
// OUTSIDE the pod network instead. Excluding this CIDR is what keeps a pod in some other namespace
// (a SUT's, say) from using that rule to walk in.
const localPodCIDR = "10.42.0.0/16"

// sharedLokiConfig is the bundled Loki config with ONE change: auth_enabled: true. Derived rather
// than copied so retention, deletes and the schema cannot drift between the two. auth_enabled makes
// tenancy fail CLOSED — a request with no X-Scope-OrgID is refused, and one with another tenant's id
// sees none of this one's streams. A copy of the bundled prefix is checked, so a reworded bundled
// config fails the render loudly instead of silently shipping auth_enabled: false.
func sharedLokiConfig() (string, error) {
	const bundledPrefix = "auth_enabled: false\n"
	if !strings.HasPrefix(lokiConfigYAML, bundledPrefix) {
		return "", fmt.Errorf("internal: lokiConfigYAML no longer starts with %q — the shared Loki's auth_enabled cannot be derived", bundledPrefix)
	}
	return "auth_enabled: true\n" + strings.TrimPrefix(lokiConfigYAML, bundledPrefix), nil
}

// RenderSharedLoki renders the ONE shared Loki of an environment (T3.3): namespace argus-obs, a
// tag@digest-pinned Loki with auth_enabled: true and the bundled retention, a Service (NodePort on
// a local tier), and a NetworkPolicy that admits only --obs=shared instance namespaces and the
// shared Grafana. Pure and deterministic: rendering and applying it twice changes nothing.
//
// ⚠ What isolates tenants here is the NetworkPolicy, not X-Scope-OrgID: anything that reaches this
// Service may claim any tenant. That is why per-instance (--obs=bundled) stays the default.
func RenderSharedLoki(s SharedLoki) (string, error) {
	tier := strings.ToLower(strings.TrimSpace(s.Tier))
	if tier == "" {
		tier = "k3d"
	}
	if tier == "compose" {
		return "", fmt.Errorf("--obs=shared is k8s-only: --tier compose has no cluster to host the shared Loki (namespace %s)", SharedObsNamespace)
	}
	grafanaNS := strings.TrimSpace(s.GrafanaNamespace)
	if grafanaNS == "" {
		grafanaNS = sharedGrafanaNamespaceDefault
	}
	if !dns1123.MatchString(grafanaNS) {
		return "", fmt.Errorf("grafana namespace %q is not a valid DNS-1123 label", grafanaNS)
	}
	cfg, err := sharedLokiConfig()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(cfg))
	local := localTiers[tier]
	storageClass := strings.TrimSpace(s.StorageClass)
	if storageClass == "" {
		storageClass = obsStorageClassFor(tier)
	}
	ns := SharedObsNamespace

	svcType, svcExtra, podPlacement, grafanaPeer := "ClusterIP", "", "", ""
	if local {
		svcType = "NodePort"
		// Local, so the Grafana's source address survives to the policy check (no SNAT to a node or
		// pod-network IP, which the ipBlock below excludes). Paired with the control-plane pin: the
		// onboarder addresses k3d-<cluster>-server-0, so Loki must be on that node for Local to answer.
		svcExtra = "  externalTrafficPolicy: Local\n"
		podPlacement = "      nodeSelector: {node-role.kubernetes.io/control-plane: \"true\"}\n"
		grafanaPeer = fmt.Sprintf(`        # The shared Grafana, OFF-cluster (a compose project) via the NodePort. A NetworkPolicy
        # cannot name it by namespace, so admit sources outside the pod network — the same localhost
        # trust boundary the bundled NodePort already sits on — and nothing inside it.
        - ipBlock: {cidr: 0.0.0.0/0, except: [%s]}
`, localPodCIDR)
	} else {
		grafanaPeer = fmt.Sprintf(`        # The managed shared Grafana, in-cluster: that namespace AND that pod (one peer = both must match).
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: %s}}
          podSelector: {matchLabels: {app.kubernetes.io/name: argus-grafana}}
`, grafanaNS)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `# RENDERED by the argus onboarder (internal/k8srender): the environment's ONE shared Loki (T3.3).
# Do not edit by hand. Every --obs=shared instance writes here as tenant <instance-id>.
# ⚠ X-Scope-OrgID SCOPES a request; it does not authenticate it. The NetworkPolicy below is the
# boundary. Per-instance (--obs=bundled) stays the default: it is the only mode where one
# instance physically cannot read another's logs.
# Instance teardown NEVER deletes this namespace; an instance's logs here age out by retention.
apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    app.kubernetes.io/part-of: argus
    argus.onedroid.ai/role: shared-obs
---
apiVersion: v1
kind: ConfigMap
metadata: {name: loki-config, namespace: %s}
data:
  loki.yaml: |
%s
%s---
apiVersion: apps/v1
kind: Deployment
metadata: {name: loki, namespace: %s, labels: {app: loki}}
spec:
  replicas: 1
%s  selector: {matchLabels: {app: loki}}
  template:
    metadata:
      labels: {app: loki}
      annotations: {argus.onedroid.ai/loki-config-sha256: "%s"}
    spec:
%s%s      containers:
        - name: loki
          # PINNED, tag@digest — the same image the bundled per-instance Loki runs (T3.4: no floating tag).
          image: grafana/loki:3.7.8@sha256:1107dd5274e0ada47e42472b7a7e71f3b2a2fe878878108f3e2f9e51528f0193
          args: ["-config.file=/etc/loki-argus/loki.yaml"]
          ports: [{containerPort: 3100, name: http}]
          # Sized for the environment, not for one instance: twice the bundled request and limit.
          # These are RENDERED requests/limits, not measured usage.
          resources:
            requests: {cpu: "100m", memory: "256Mi"}
            limits:   {cpu: "1", memory: "1Gi"}
          readinessProbe: {httpGet: {path: /ready, port: http}, initialDelaySeconds: 15, periodSeconds: 10, failureThreshold: 30}
          volumeMounts:
            - {name: data, mountPath: /loki}
            - {name: config, mountPath: /etc/loki-argus, readOnly: true}
      volumes:
        # A PVC, as bundled: a pod recreate must not lose the logs — here EVERY tenant's, not one
        # instance's. Instance teardown never deletes this claim (it never touches argus-obs).
        - {name: data, persistentVolumeClaim: {claimName: loki-data}}
        - {name: config, configMap: {name: loki-config}}
---
apiVersion: v1
kind: Service
metadata: {name: loki, namespace: %s}
spec:
  type: %s
%s  selector: {app: loki}
  ports: [{name: http, port: 3100, targetPort: http}]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: default-deny-ingress, namespace: %s}
spec:
  podSelector: {}
  policyTypes: [Ingress]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: shared-loki-ingress, namespace: %s}
spec:
  # HONESTY NOTE (as UC139): only real where the cluster enforces NetworkPolicy (k3s does; bare
  # flannel does not). Rendering it is not evidence of isolation.
  podSelector: {matchLabels: {app: loki}}
  policyTypes: [Ingress]
  ingress:
    - from:
        # Instance namespaces onboarded with --obs=shared (their promtail pushes, their executor reads).
        - namespaceSelector: {matchLabels: {%s: %s}}
%s      ports:
        - {protocol: TCP, port: 3100}
`,
		ns,
		ns, indent(cfg, 4),
		obsPVCDoc("loki-data", ns, storageClass, obsLokiVolumeSize, "loki"),
		ns, obsRecreateStrategy, hex.EncodeToString(sum[:]),
		lokiPodSecurity, podPlacement,
		ns, svcType, svcExtra,
		ns,
		ns, obsModeLabel, obsModeShared,
		grafanaPeer)
	return b.String(), nil
}

// parseStage renders the promtail parse stage matching the SUT's log ENCODING. A logfmt SUT
// given a `json` stage extracts NOTHING — its level/correlation_id labels come out empty and
// event_type renders as the Go template's literal `<no value>` (GAP-2). promtail's logfmt stage
// takes bare key names, not JSON expressions, so the two are rendered differently.
func parseStage(c *config.Config) string {
	const ind = "          "
	if c.LogFormat() == "logfmt" {
		return fmt.Sprintf(`%s- logfmt:
%s    mapping:
%s      level: %s
%s      correlation_id: %s
%s      saga_marker: %s`, ind, ind, ind, c.LevelField(), ind, c.CorrelationField(), ind, c.SagaEventField())
	}
	return fmt.Sprintf(`%s- json:
%s    expressions:
%s      level: %s
%s      correlation_id: %s
%s      saga_marker: %s`, ind, ind, ind, c.LevelField(), ind, c.CorrelationField(), ind, c.SagaEventField())
}

// sagaMarkerCond builds the promtail template condition matching ANY declared saga marker
// value: `eq .saga_marker "a"` for one, `or (eq …"a") (eq …"b")` for several. A SUT that
// spreads control actions over several values would otherwise have only its FIRST value
// normalized, leaving the dashboard's saga panel partial (PROB-2).
func sagaMarkerCond(c *config.Config) string {
	vals := c.SagaEventValues()
	if len(vals) == 0 {
		return `eq .saga_marker "saga"`
	}
	eqs := make([]string, 0, len(vals))
	for _, v := range vals {
		eqs = append(eqs, `(eq .saga_marker "`+v+`")`)
	}
	if len(eqs) == 1 {
		return strings.TrimSuffix(strings.TrimPrefix(eqs[0], "("), ")")
	}
	return "or " + strings.Join(eqs, " ")
}

// SUTAccessRoleManifest renders the OPTIONAL Role + RoleBinding the SUT owner applies in THEIR OWN
// namespace (P3 #23, tester msgbus 2026-09-27) to grant the executor's ServiceAccount read access
// for load-run environment capture (internal/envcapture, internal/argus/environment.go). It is
// DELIBERATELY NOT part of RenderExecutor's output: that renders manifests for the executor's OWN
// namespace (in.Namespace()); this manifest targets a DIFFERENT namespace Argus does not own and
// must never self-apply. It is emitted only when the caller opts in (cmd/argus's
// --emit-sut-access-role) and is meant to be reviewed and applied BY THE SUT OWNER, exactly the
// pattern msgbus's own deploy/k8s/55-argus-test-access.yaml already uses for a different grant.
//
// Namespaced only — pods/deployments/statefulsets/replicasets in in.SUTNamespace. Node reads
// (allocatable capacity, see envcapture.Node's doc comment) are CLUSTER-SCOPED and cannot be
// granted from a namespace-scoped Role; a cluster operator who wants that section populated must
// separately grant the executor's ServiceAccount a ClusterRole on `nodes` (get, list) — outside
// this manifest, and outside any single SUT owner's authority.
func SUTAccessRoleManifest(in Instance) string {
	execNS := in.Namespace()
	return fmt.Sprintf(`# Apply this in YOUR OWN namespace (%s) to let the Argus executor read your
# pods/deployments/statefulsets/replicasets for load-run environment capture (P3 #23) AND to let a
# certifying (final/scheduled) run MEASURE which image digests are running before it runs, so a
# certificate can say whether the build under test was the declared one. The measurement needs only
# get/list on pods. Without this grant the run still happens and its certificate says "not measured:
# forbidden" — it never says "matched". Argus cannot grant itself this access — only you can, in your
# own namespace.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: argus-load-environment-read, namespace: %s}
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [get, list]
  - apiGroups: [apps]
    resources: [deployments, statefulsets, replicasets]
    verbs: [get, list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: argus-load-environment-read, namespace: %s}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: argus-load-environment-read}
subjects:
  - {kind: ServiceAccount, name: argus-executor, namespace: %s}
`, in.SUTNamespace, in.SUTNamespace, in.SUTNamespace, execNS)
}
