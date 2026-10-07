package federation

import (
	"encoding/json"
	"strings"
	"time"
)

// The federation wire — the explicit, versioned context-to-context contract (ADR-1 / cat-310). These
// message shapes are FINAL from day one (plan §5.2 I.1): the register/poll/results-push schemas, the
// D-FED.4 version fields, and the D-FED.2 distinguished rejection reasons — no later protocol churn.
// All three verbs are outbound-only HTTPS from the executor's vantage.

// RegisterRequest registers an executor (inside the login-based onboarding, D-ONBOARD.7). Only the
// PUBLIC key leaves the environment; the CP stores it on the registration (D-FED.2).
type RegisterRequest struct {
	InstanceID  string `json:"instance_id"`
	WorkspaceID string `json:"workspace_id"`
	SUTName     string `json:"sut_name"`
	Tier        string `json:"tier"`       // compose | k3d | managed
	PublicKey   string `json:"public_key"` // base64 (std) Ed25519 public key, 32 bytes
	// X25519PublicKey (AC-4) is the executor's SECOND registered key, beside the Ed25519 one above — not
	// an authenticator, but the ECDH public half a control plane seals a run's EXPECT material under
	// (migration 036's instances.x25519_pubkey). Optional: an executor that has not minted one yet
	// registers without it, and the control plane ships that executor's EXPECT in the clear exactly as
	// it always has — never a fault, never refused.
	X25519PublicKey string          `json:"x25519_public_key,omitempty"` // base64 (std) X25519 public key, 32 bytes
	DeclaredTargets json.RawMessage `json:"declared_targets,omitempty"`
	RunnerVersion   string          `json:"runner_version,omitempty"`
	SuiteVersion    string          `json:"suite_version,omitempty"`
	ProtocolVersion string          `json:"protocol_version,omitempty"`
	// U7: the two AGENT FOLDERS on the machine that ran onboarding, and that machine's name.
	// Reported so an operator with several onboarded SUTs can find them again — onboarding knows
	// them, prints them once, and until now they existed nowhere anybody could look them up.
	//
	// HOST paths in NATIVE form (C:\... on Windows), never the container's /config and /scenarios and
	// never the MSYS /c/... spelling: the point is that the value pastes straight into the operator's
	// file manager. Nothing dereferences them here.
	ProductDir  string `json:"product_dir,omitempty"`
	TestDir     string `json:"test_dir,omitempty"`
	OnboardHost string `json:"onboard_host,omitempty"`
	// KitDir (VR5-U1) — the kit directory onboarding RAN FROM, i.e. where onboarding/update.sh
	// lives, in the same NATIVE spelling as the two above. It rides the same carrier for the same
	// reason: it is a fact only the onboarding machine has, and the control plane had no way to
	// learn it at all. Omitted by any executor that was not told it; the reader then omits the
	// update command rather than composing a half-built one.
	KitDir string `json:"kit_dir,omitempty"`
	// KubeContext / Kubeconfig (VR10-U1, V28-020) — the kubectl context (and, when one was given, the
	// kubeconfig path) onboarding used for a k3d/managed instance, in the operator's native spelling. The
	// control plane renders the update BLOCK from them; absent means "not told" (a pre-0.3.29 onboarder or
	// a compose instance), never a fault — exactly KitDir's rule.
	KubeContext string `json:"kube_context,omitempty"`
	Kubeconfig  string `json:"kubeconfig,omitempty"`
	// RunnerID (AC-17) pairs this executor with a runner id minted through the control plane's author
	// plane (author_mint_runner_id) or the CLI (`argus runner-id mint`) — the identity a builder outside
	// the environment presents to reach this executor through the relay. Optional: an executor that
	// registers with none simply is not reachable by any relayed runner__* call yet. A revoked id, or
	// one minted for a DIFFERENT instance, is refused BY NAME at register — never silently ignored.
	RunnerID string `json:"runner_id,omitempty"`
	// MoneyHandling (T5.4 follow-up) mirrors the SUT's own config.MoneyHandling() —
	// computed by the executor from the SAME argus-config declaredTargets already reads, and carried
	// here AND on every PollRequest below for the reason stated at PollRequest's U7 note: an instance
	// already onboarded takes 403 on register-on-start on k3d/aks, so a field that rode only here
	// would reach the control plane once, on first onboard, and never self-heal.
	//
	// A *bool, not a bool, following the Replicas*/SUTReachable convention on PollRequest below: nil
	// means NOT REPORTED (an executor older than this change, or a config that failed to load), which
	// the control plane stores as "leave what you have" — a poll that says nothing must never ERASE a
	// true recorded earlier. The control plane's guard treats a never-reported instance (NULL) as NOT
	// money-handling, deliberately (moneyGuardRefusal): the executor-side guard still refuses at validate
	// and run, and guarding NULL would strip authoring from every instance older than this change.
	MoneyHandling *bool `json:"money_handling,omitempty"`
	// MoneyWritesAllow (money_writes follow-up, 2026-09-26) mirrors MoneyHandling's carrier rule
	// exactly, one level down: the executor's own config.MoneyWriteAllowlist(), JSON-encoded. nil
	// means NOT REPORTED (an executor older than this change, or a config that failed to load),
	// which the control plane stores as "leave what you have" — a report that says nothing must
	// never ERASE an allowlist a previous register/poll already recorded. A loadable config that
	// simply declares no money_writes block reports an EXPLICIT "[]", never nil — see
	// runner.moneyWritesAllowFor's own doc for why that distinction matters.
	MoneyWritesAllow json.RawMessage `json:"money_writes_allow,omitempty"`
	// TestTargets (, UI-6) is the executor's own `test_targets` declaration, JSON-encoded
	// (internal/testtargets). NOT DeclaredTargets above (the SUT's CONNECTION targets). nil means NOT
	// REPORTED — an executor older than this change, or a config that failed to load — which the control
	// plane stores as "leave what you have"; an EXPLICIT "[]" is a real report of "none declared" and
	// overwrites. The same carrier rule as MoneyWritesAllow, and it rides every PollRequest too.
	// An older control plane has no such field and drops it (no DisallowUnknownFields on the wire).
	TestTargets json.RawMessage `json:"test_targets,omitempty"`
	// DashboardLinkTemplate / DashboardLinkLabel (, UI-2) are the executor's own
	// observability.dashboard_link block. THREE-VALUED, so *string: nil = NOT REPORTED (an executor older
	// than this change, or a config that failed to load) and the control plane leaves what it has; a
	// pointer to "" = REPORTED NONE (the block was removed from the config) and clears. The same carrier
	// rule as MoneyHandling; they ride every PollRequest too. An older control plane drops them.
	DashboardLinkTemplate *string `json:"dashboard_link_template,omitempty"`
	DashboardLinkLabel    *string `json:"dashboard_link_label,omitempty"`
}

// RegisterResponse confirms a registration.
type RegisterResponse struct {
	InstanceID   string    `json:"instance_id"`
	RegisteredAt time.Time `json:"registered_at"`
}

// PollRequest is the long-poll body. Auth = the executor JWT in the Authorization header. The poll IS
// the heartbeat (D-FED.4): the CP stamps last_seen on every poll. The body carries the version fields.
type PollRequest struct {
	RunnerVersion   string `json:"runner_version,omitempty"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
	// SuiteVersion (INT-026/INT-029, 2026-08-08) rides here for exactly the reason the block below
	// describes. It was first added to the REGISTER payload, where it could never arrive: measured on
	// COMPOSE, a re-onboarded executor took 403 on register-on-start while runner_version updated fine
	// via this poll — so suite_version stayed empty and UC102's drift chip stayed uncomputable. Note
	// the note below says "on the k8s tiers"; the refusal is not k8s-specific.
	SuiteVersion string `json:"suite_version,omitempty"`
	// F10/UC196: what the executor OBSERVED about its own Deployment — the replicas it asked for and
	// the replicas that are actually Ready. POINTERS, so "not reported" is distinguishable from zero:
	// the compose tier has no Deployment to observe and must not be shown a fabricated count.
	// The poll is the carrier because it is the only thing that happens continuously; registration
	// would freeze the number at start-up, which is exactly when a quota shortfall is not yet visible.
	ReplicasDesired *int `json:"replicas_desired,omitempty"`
	ReplicasReady   *int `json:"replicas_ready,omitempty"`
	// ReplicasUpdated (VR-F3) — replicas running the CURRENT pod template. The ONLY one of the three
	// that sees a half-landed rollout: measured 2026-08-10, two k3d instances reported desired=1
	// ready=1 while serving the PREVIOUS image, because the old pod was ready and the new one was
	// Pending on a node that could not pull it. "Ready" is true of a fleet running the wrong version.
	ReplicasUpdated *int `json:"replicas_updated,omitempty"`
	// ReplicasTotal is status.replicas — TOTAL non-terminated pods (021). THE discriminator: a
	// PENDING pod counts as "updated", so a stalled rollout reads desired=1 ready=1 updated=1 and
	// only total=2 reveals that an old pod is still the one serving.
	ReplicasTotal *int `json:"replicas_total,omitempty"`
	// U7 (CP-M3-III-83): the HOST folders this instance was onboarded from, carried on the POLL for
	// the same reason the replica counts are — the poll is the only thing that happens continuously.
	//
	// These used to ride ONLY on register-on-start, and on the k8s tiers that call is REFUSED: onboard
	// registers the instance itself (consuming the single-use enrollment token), so the executor's
	// later attempt has no machine JWT for that identity and takes
	//   403 {"error":"re-register requires the machine JWT of the registered identity"}
	// The poll then "self-heals" liveness while carrying none of the register-only fields — so ANY
	// field only the executor knows could never reach the control plane on k3d or aks. U7 was simply
	// the first feature to depend on one. Moving them to the poll fixes the CLASS, not just U7.
	ProductDir  string `json:"product_dir,omitempty"`
	TestDir     string `json:"test_dir,omitempty"`
	OnboardHost string `json:"onboard_host,omitempty"`
	// KitDir (VR5-U1) — the kit directory onboarding RAN FROM, i.e. where onboarding/update.sh
	// lives, in the same NATIVE spelling as the two above. It rides the same carrier for the same
	// reason: it is a fact only the onboarding machine has, and the control plane had no way to
	// learn it at all. Omitted by any executor that was not told it; the reader then omits the
	// update command rather than composing a half-built one.
	KitDir string `json:"kit_dir,omitempty"`
	// VR10-U1 (V28-020): see RegisterRequest. Reported on every poll so a control plane that learned the
	// instance before 0.3.29 fills the columns without a re-onboard.
	KubeContext string `json:"kube_context,omitempty"`
	Kubeconfig  string `json:"kubeconfig,omitempty"`
	// ResultsPending (VR-F28/INT-004) — the runs that FINISHED in this environment and whose results
	// are still sitting in the durable outbox.
	//
	// WHY THE POLL AND NOT THE PUSH, which is the INT-029 lesson rather than a preference: this fact
	// exists precisely WHEN the push path is not working. Riding it on the push would mean the one
	// condition it reports is the one condition under which it can never be reported. The poll is the
	// only channel proven reachable at the moment of reporting — a poll that returned IS the
	// reachability proof — and it is the only thing that happens continuously.
	//
	// Without this the control plane shows a finished run as `running` for as long as the outbox
	// holds it. The run ended minutes ago, nothing distinguishes it from one still executing, and the
	// watchdog eventually calls it `abandoned` — which is actively wrong: the results exist, they are
	// on a disk, and they are queued.
	ResultsPending []PendingRun `json:"results_pending,omitempty"`
	// SUTReachable / SUTCheckedAt (VR6-W1/V23-011) — what the executor learned by DIALLING the SUT's
	// own declared targets, from the only vantage that can: it is the thing on the SUT's network.
	//
	// A POINTER, following the Replicas* convention above and for the same reason its comment gives —
	// "so 'not reported' is distinguishable from zero". Here the stakes are higher than a count. A
	// plain bool would force `false` to carry both "I dialled it and it refused" and "I never dialled
	// it", and the Environments page would paint a red verdict on a SUT nobody measured. That is
	// V18-008 inverted: it showed a green nobody earned.
	//
	// Confirmed twice, which is why this rides here at all: memstore-compose ran TWO HOURS with zero
	// error lines while its SUT had been dead since a reboot, and social-mcp-k3d showed HEALTHY while
	// its SUT had been deaf ~2.5 h with 13 messages queued against zero consumers. Both executors were
	// genuinely healthy and reporting faithfully — the page had no channel through which the SUT's
	// state could arrive.
	//
	// The poll is the carrier for the reason stated above ResultsPending, and one specific to this
	// fact: a SUT that dies LATER is the whole point, and memstore-compose died at a reboot hours after
	// its onboarding. Freezing this at registration would report the one moment it is always true.
	SUTReachable *bool `json:"sut_reachable,omitempty"`
	// SUTCheckedAt is WHEN, and it is not optional decoration. The page greys out a reading that has
	// aged past the poll's own freshness, so a `true` measured twenty hours ago must render grey
	// rather than green. Without the timestamp the control plane holds a verdict it can never age out.
	SUTCheckedAt *time.Time `json:"sut_checked_at,omitempty"`
	// MoneyHandling (T5.4 follow-up) — see RegisterRequest.MoneyHandling above for
	// the full rule; it rides here too, on EVERY poll, for the same reason ResultsPending and the
	// SUT observation do: the poll is the only carrier that runs continuously and is always
	// authorised, so it is the one place an executor can self-heal a fact register could not deliver.
	MoneyHandling *bool `json:"money_handling,omitempty"`
	// MoneyWritesAllow (money_writes follow-up) — see RegisterRequest.MoneyWritesAllow above for the
	// full rule; it rides here too, on EVERY poll, for the same self-heal reason MoneyHandling does.
	MoneyWritesAllow json.RawMessage `json:"money_writes_allow,omitempty"`
	// TestTargets — see RegisterRequest.TestTargets for the full rule; it rides here on
	// EVERY poll for the same self-heal reason MoneyWritesAllow does.
	TestTargets json.RawMessage `json:"test_targets,omitempty"`
	// DashboardLinkTemplate / DashboardLinkLabel (UI-2) -- see RegisterRequest; on EVERY poll for the
	// same self-heal reason. nil = not reported, pointer to "" = reported none.
	DashboardLinkTemplate *string `json:"dashboard_link_template,omitempty"`
	DashboardLinkLabel    *string `json:"dashboard_link_label,omitempty"`
	// SummaryReadings (UI-7a, A'3) -- the latest batch of named summary numbers read from
	// the environment's own metrics source, at most SummaryMaxReadings, at most one batch per
	// SummaryMinEvery. omitempty: an executor with no summary_metrics block (or older than this field)
	// sends nothing and an older control plane drops the key, so neither direction breaks.
	SummaryReadings []SummaryReading `json:"summary_readings,omitempty"`
}

// SUTObservation is one executor's answer about its own SUT, passed as a unit rather than as two more
// positional arguments (VR6-W1).
//
// The struct is deliberate. pollRequestFor already takes five *int in a row, and INT-026 was a field
// nobody assigned inside an inline struct literal — a named pair cannot be transposed by accident.
//
// The zero value means NOTHING IS KNOWN, and both halves must be present for either to be carried: a
// verdict without a time cannot be aged out, and a time without a verdict says nothing.
type SUTObservation struct {
	Reachable *bool
	CheckedAt *time.Time
}

// PendingRun is one finished-but-undelivered run, as the executor sees it (VR-F28).
//
// It carries the TALLIES because they are the part an operator needs and the part the executor
// already has: "pending, 30/33" is a different message from a bare "pending", and re-deriving it
// would cost the payload that could not be delivered in the first place. Everything here is
// index-level — no per-scenario evidence crosses (VR-C8).
// SetHashResponse answers "has this instance's scenario set changed?" without carrying it (VR-F6).
//
// The whole point is the SIZE. A direct local run must consult the catalog before executing — a
// scenario edited in the registry and then run locally used to execute the OLD copy on disk — but
// paying a full set transfer on every fix-loop iteration would make that consultation something
// people turn off. 64 bytes of hash is cheap enough to ask every time.
type SetHashResponse struct {
	Scope   string `json:"scope"`
	SetHash string `json:"set_hash"`
}

type PendingRun struct {
	RunID string `json:"run_id"`
	Scope string `json:"scope,omitempty"`
	// Tallies as counted in-env. A zero total is meaningful (a run that matched nothing), not unknown.
	Tallies Tallies `json:"tallies"`
	// QueuedAt is when the push was first written to the outbox, so a surface can say how LONG this
	// has been stuck — the difference between a blip and an executor that cannot reach us.
	QueuedAt time.Time `json:"queued_at"`
	// Attempts is how many deliveries have already failed. A growing count against a REACHABLE
	// control plane means the payload is being REJECTED rather than delayed: a different problem with
	// a different fix, and one otherwise visible only on the executor's own disk.
	Attempts int `json:"attempts,omitempty"`
}

// VersionInfo is echoed on EVERY poll response (D-FED.4 version exchange → outdated-blocked).
type VersionInfo struct {
	CurrentVersion string `json:"current_version"`
	// MinSupportedRunnerVersion is the ABSOLUTE floor: below it an executor is refused work.
	// Configured as MIN_EXECUTOR_VERSION_ABSOLUTE (the legacy MIN_RUNNER_VERSION still answers during
	// the rollback window). The JSON name is UNCHANGED on purpose — renaming it would silently turn
	// enforcement off for every executor already deployed, which is the failure this round is about.
	MinSupportedRunnerVersion string `json:"min_supported_runner_version"`
	// MinRecommendedExecutorVersion is the SOFT floor (VR-V1): at or above it an executor is
	// `current`; between the two it works normally and the operator is told a better one exists.
	//
	// One number could not express that. The absolute floor is the only thing that ever forced
	// anything, and with it set to 0.3.0 against a 0.3.18 estate every executor read `current`
	// permanently — a stale floor produces a confident WRONG verdict, which is worse than none.
	//
	// ADDITIVE: an executor that does not read this is unaffected, which is what lets the control
	// plane ship before the estate.
	MinRecommendedExecutorVersion string `json:"min_recommended_executor_version,omitempty"`
	// RecommendedImage is what an executor should update TO — DIGEST-PINNED (F13/UC071). A TAG is
	// not an answer here: tags move, so "update to :m3-dev" cannot be verified afterwards and cannot
	// be rolled back to a known point. A digest can. Empty = the control plane has no
	// recommendation, which the update flow must report rather than guess around.
	//
	// DEPRECATED by RecommendedImages (M3-FX VR-F18). Kept as the SLIM fallback so an already-running
	// control plane configured with only RECOMMENDED_EXECUTOR_IMAGE keeps answering during the
	// cutover. Read it through RecommendedFor, never directly.
	RecommendedImage string `json:"recommended_image,omitempty"`
	// RecommendedImages is the digest-pinned reference PER VARIANT: {"slim": "...@sha256:…",
	// "full": "...@sha256:…"} (VR-F18).
	//
	// WHY PER VARIANT. One recommendation cannot serve both: a SUT with a non-PostgreSQL JDBC driver
	// needs `full`, and handing it the slim digest gives it an executor whose database scenarios fail
	// later looking like the SUT's fault (INT-034). Onboarding already derives which variant a config
	// needs (onboard.SelectImageVariant); what it lacked was a way to ask "I need full — what
	// digest?" and get a pinned answer.
	//
	// This is also what makes two people onboarding the same SUT on the same day get the SAME
	// executor. A tag resolved locally at pull time cannot promise that; a digest can.
	RecommendedImages map[string]string `json:"recommended_images,omitempty"`
}

// RecommendedFor returns the digest-pinned image for a variant, or "" when the control plane has no
// recommendation for it — which callers must REPORT rather than paper over with a tag.
//
// The legacy single-value RecommendedImage answers for `slim` only. Letting it answer for `full`
// would hand a full-needing SUT a slim executor, which is the exact failure VR-F18 exists to stop.
func (v VersionInfo) RecommendedFor(variant string) string {
	if variant == "" {
		variant = "slim"
	}
	if img := v.RecommendedImages[variant]; img != "" {
		return img
	}
	if variant == "slim" {
		return v.RecommendedImage
	}
	return ""
}

// UnpinnedRecommendations lists configured recommendations that are NOT digest-pinned, for the
// startup warning. A moving tag here silently defeats the entire point of publishing one: the
// promise is "you and I get the same executor", and only a digest can keep it.
func (v VersionInfo) UnpinnedRecommendations() []string {
	var out []string
	check := func(label, ref string) {
		if ref != "" && !strings.Contains(ref, "@sha256:") {
			out = append(out, label+"="+ref)
		}
	}
	check("recommended_image", v.RecommendedImage)
	for _, variant := range []string{"slim", "full"} {
		check("recommended_images."+variant, v.RecommendedImages[variant])
	}
	return out
}

// Selection is the run's scope-defining parameters.
type Selection struct {
	Layer       string `json:"layer,omitempty"`
	Tag         string `json:"tag,omitempty"`
	ScenarioRef string `json:"scenario_ref,omitempty"`
}

// Annotations travel with a run (commit / PR / label).
type Annotations struct {
	Commit string `json:"commit,omitempty"`
	PR     string `json:"pr,omitempty"`
	Label  string `json:"label,omitempty"`
}

// ScenarioPayload is one scenario materialized for a run: its registry path + full markdown body
// (D-FED.3). EXPECT values ride here to the TEST side only.
//
// Body and Sealed are MUTUALLY EXCLUSIVE (AC-4): when the control plane holds the executor's X25519
// public key, Body is cleared and Sealed carries the same text wrapped for that executor under a
// run-scoped key (sealedexpect.go) — the wire never carries both the clear and the sealed form of one
// scenario. omitempty on Body means a sealed payload's JSON has no "body" key at all.
type ScenarioPayload struct {
	Path   string      `json:"path"`
	Body   string      `json:"body,omitempty"`
	Sealed *SealedBody `json:"sealed,omitempty"`
}

// OnboardingStateRequest carries what ONBOARDING says about its own run (VR-L5 / V17-021).
//
// Only onboarding can answer this. The executor registering and heartbeating is exactly the evidence
// that made two half-finished onboards look healthy on 2026-08-13; whether the RUN finished is a fact
// only the run has.
type OnboardingStateRequest struct {
	// State is one of: incomplete (stamped once registration succeeds), degraded (finished, not
	// cleanly — the existing PARTIALLY SET), complete. An onboarder that does not send this at all
	// leaves the column NULL, which is rendered as nothing and is NEVER a fault.
	State string `json:"state"`
	// DashboardState (VR5-O2) is one of: present, missing, unknown — onboarding's own verdict on
	// whether the instance's Grafana dashboard really exists after the import.
	//
	// Onboarding is the authority because nothing else CAN be: a local tier's Grafana is
	// http://localhost:3000 on the operator's machine, and the control plane is not on it.
	//
	// OMITTED MEANS "NO OPINION", not "missing". An older kit never sends it, and even a current kit
	// omits it on the first stamp (which happens right after registration, before the Grafana wiring
	// runs). The control plane must therefore LEAVE THE STORED VALUE ALONE on an empty field rather
	// than overwrite it — see Store.SetOnboardingAndDashboardState.
	DashboardState string `json:"dashboard_state,omitempty"`
}

// RunAssignment is a picked-up run-request with its atomically-materialized scenario set + set hash.
type RunAssignment struct {
	RunRequestID string            `json:"run_request_id"`
	Scope        string            `json:"scope"` // full | layer | tag | single
	Selection    Selection         `json:"selection"`
	Annotations  Annotations       `json:"annotations,omitempty"`
	SetHash      string            `json:"set_hash"`
	Scenarios    []ScenarioPayload `json:"scenarios"`
	// Mode is build (default) | final | scheduled — what kind of run this is (AC-3). ArtifactDigest is
	// the OCI/sha256 digest of the artifact a `final` run certifies; a `final` assignment with no
	// digest is refused by the runner (internal/runner), the SECOND refusal — the first is the control
	// plane's own closed tool schema, so a control plane that skipped its check is still caught here.
	Mode           string `json:"mode,omitempty"`
	ArtifactDigest string `json:"artifact_digest,omitempty"`
	// RunID is the run's identity, and it now CROSSES THE WIRE (VR-U1b / V17-012).
	//
	// It used to be `json:"-"`, local-only: a DIRECT run pre-minted its id so the W1 fence begin and
	// the eventual results-push carried the same one (§D-3.1.2/UC188), while "every federated
	// assignment" was empty and the executor minted at run start. That is precisely why a federated
	// run could not be recorded as running at hand-out time — the control plane had no id to write a
	// ledger row with, so the row waited for a 30-second heartbeat that never fired for a short run.
	//
	// The control plane now mints it at claim time and sends it (owner decision D5). BACKWARD
	// COMPATIBLE in both directions: an OLD executor ignores the new field and mints its own, and a
	// NEW executor against an OLD control plane receives "" and mints its own — see
	// runner/executor.go, which mints only when this is empty.
	RunID string `json:"run_id,omitempty"`
	// CommitmentID / HoldoutRoot (AC-4) — the sealed commitment a `final` assignment certifies against.
	// PickupNext attaches the instance's currently sealed commitment when mode is `final`; empty on a
	// `build`/`scheduled` assignment, which has no commitment. A later stage (AC-7's reveal) checks a
	// run's evidence against HoldoutRoot via the same Merkle framing (store/merkle.go) the control plane
	// sealed it under.
	CommitmentID string `json:"commitment_id,omitempty"`
	HoldoutRoot  string `json:"holdout_root,omitempty"`
	// CommitmentImageDigests are the image_digests the sealed commitment anchored as version 1, sent on a
	// final/scheduled assignment so the executor can RECORD which of them it found running. They are
	// recorded, never enforced: only ArtifactDigest decides whether a run is refused.
	CommitmentImageDigests []string `json:"commitment_image_digests,omitempty"`
	// QueuedAt is LOCAL-ONLY (json:"-"), like RunID: the CP uses it to observe how long this request
	// waited before pickup (VR-F10 pickup latency). It deliberately does NOT cross the wire — the
	// executor has no use for it, and a clock it cannot verify is not something to hand it.
	QueuedAt time.Time `json:"-"`
	// CompareScenarios (ARGUS-CMP-2/-07) carries the sealed set of a `compare` run, in
	// place of Scenarios, which stays EMPTY on such an assignment. That is the whole point: an executor
	// that predates this field does not know it, materialises an empty directory and refuses with
	// ErrNoScenarioSource, so it never receives the sealed set at all (design 11.2). Same payload type,
	// same sealing. Mode is "compare".
	CompareScenarios []ScenarioPayload `json:"compare_scenarios,omitempty"`
	// CompareTarget (executor release E2) names a declared named connection target to use for the
	// checks of a compare run that carry no **Target** of their own. Empty = every check keeps its own.
	CompareTarget string `json:"compare_target,omitempty"`
}

// MaterializeResponse is the fresh scenario set for a DIRECT local run (the D-FED.3 hybrid source,
// UC059) — the current active set for the instance/scope + its content hash, WITHOUT enqueuing a run.
type MaterializeResponse struct {
	Scope     string            `json:"scope"`
	SetHash   string            `json:"set_hash"`
	Scenarios []ScenarioPayload `json:"scenarios"`
}

// RotateRequest re-binds an instance to a NEW Ed25519 machine key (S2, §D-3.1.4/ADR-9, UC191):
// authenticated by the CURRENT key's machine JWT on POST /fed/rotate; NON-destructive — scenarios,
// runs and the registration survive; only the key changes (the old key's JWTs die instantly).
type RotateRequest struct {
	NewPublicKey string `json:"new_public_key"` // base64 (std) Ed25519 public key, 32 bytes
}

// RunBeginRequest fences a DIRECT local run (the W1 instance_run_lock, M3.1 §D-3.1.2/ADR-7, UC188):
// the runner acquires the CP-side per-instance lock + the initial 'running' ledger row BEFORE any SUT
// effect. 200 = held; 409 {"error":"instance_busy"} = a run (either path) is already in flight — the
// runner surfaces *busy*. The terminal /fed/results push (same run_id, no run_request_id) releases.
type RunBeginRequest struct {
	RunID string `json:"run_id"`
	Scope string `json:"scope"` // full | layer | tag | single
}

// PollResponse is the long-poll result: an assignment (HasRun) or none (a ~25s timeout), ALWAYS with
// the version info. Outdated=true means the executor is below min_supported and the CP refuses pickup
// (outdated-blocked; the executor stays visible and demands an update, D-FED.4).
type PollResponse struct {
	HasRun   bool           `json:"has_run"`
	Run      *RunAssignment `json:"run,omitempty"`
	Versions VersionInfo    `json:"versions"`
	Outdated bool           `json:"outdated,omitempty"`
	// UpdateTo carries an operator's ON-REQUEST update (F15/UC069). The poll response is the ONLY
	// channel the control plane has to an executor — there is no inbound connection to an in-env
	// executor and there must not be — so a "please update" is parked and collected here.
	//
	// Delivered EXACTLY ONCE: the control plane clears it as it hands it over. A request that
	// survived delivery would re-fire every poll and turn a failing update into a crash-loop.
	UpdateTo string `json:"update_to,omitempty"`
	// Commands (AC-17) carries every relayed builder command still queued for this instance, delivered
	// BESIDE any run assignment — never a new inbound path. Each is claimed (queued -> delivered)
	// atomically as it rides this response, so a retried poll after a dropped response never redelivers
	// one the executor already saw.
	Commands []CommandEnvelope `json:"commands,omitempty"`
}

// CommandEnvelope (AC-17) is one relayed runner__* call, closed to the four verbs the builder
// namespace publishes: validate_config | run | get_report | get_dashboard_url. Args carries the
// tool's own arguments (runner_id excluded — the envelope's routing, not the tool's business).
// Executed through the SAME in-process code the local router's runner__* tools use, never a shell.
type CommandEnvelope struct {
	CommandID string          `json:"command_id"`
	Verb      string          `json:"verb"`
	Args      json.RawMessage `json:"args,omitempty"`
}

// CommandResult (AC-17) carries a relayed command's answer back on the executor's own push (the
// executor__command_result thin adapter, the AC-16 pair pattern) — never a control-plane-initiated
// call. Result is whatever the verb's own custody rule allows through (get_report's is reduced to
// the verdict + object id, expected/observed always stripped before this is built).
type CommandResult struct {
	CommandID string          `json:"command_id"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// Tallies is the per-run count summary.
type Tallies struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Errored int `json:"errored"`
	Total   int `json:"total"`
	// VR10-R1 (V28-009): how many scenarios the SUT REFUSED to measure because we were going too
	// fast, and how many times the run stopped and waited about it. Counts only — the same MAY-CROSS
	// column every other tally sits in; nothing about WHICH scenarios leaves the environment.
	//
	// Both omitempty, and both must reach here or the Runs page can never say it: the page reads the
	// ledger, not report.json, so a summary field with no wire field is a number nobody sees.
	RateLimited     int `json:"rate_limited,omitempty"`
	RateLimitPauses int `json:"rate_limit_pauses,omitempty"`
	// Degraded (AC-11) counts scenarios whose OWN assertions passed while the survival-plane read
	// showed the SUT in distress — never counted in Passed/Failed, the same reasoning as RateLimited.
	Degraded int `json:"degraded,omitempty"`
}

// ScenarioResult is one scenario's OUTCOME + a generic, redaction-safe summary line. Deliberately no
// EXPECT value and NO verdict class — verdicts are in-env only (D-FED.4).
type ScenarioResult struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"` // passed | failed | errored | degraded (AC-11)
	// Layer is carried so a run row stays readable on its own (F2/UC029). It was always available at
	// push time — the report is GROUPED by layer — and was simply dropped by the mapper, which left
	// the web joining it from the live catalog. A scenario deleted after the run then vanished from
	// that catalog and its layer went blank, on a run that had already happened.
	Layer      string `json:"layer,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	Summary    string `json:"summary,omitempty"`
	// EvidenceHash (AC-6/VR-4) is sha256, hex-encoded, over the runner's own in-env evidence record
	// for this scenario — the report.ScenarioResult it never sends (Failure, Steps, Cleanup,
	// MCPEnvelope, Unexecuted, the request counters), marshaled with encoding/json (whose struct
	// field order is fixed by declaration order, so the bytes are stable across identical runs).
	// Computed by the runner where the push is assembled (internal/runner/mapper.go); the evidence
	// itself never crosses this wire, only its digest (D-FED.4 locality rule).
	EvidenceHash string `json:"evidence_hash"`
}

// ResultsPush is the results payload — EXACTLY the D-CP-WEB.1 MAY-cross column. Idempotent by RunID
// (D-FED.4). ALL runs report up, direct runs included (RunRequestID absent for those).
//
// THE LOCALITY RULE IS ENFORCED BY THIS SCHEMA, NOT BY DISCIPLINE: there is deliberately NO field for
// logs, sagas, DB rows, or verdict classes. The upload format has nowhere to put evidence, so evidence
// cannot cross to the cloud. (Guarded by TestResultsPush_NoEvidenceFields.)
type ResultsPush struct {
	RunID        string `json:"run_id"`
	RunRequestID string `json:"run_request_id,omitempty"`
	Scope        string `json:"scope"`
	// running | completed | failed | results_pending | degraded
	//   results_pending (012) = the run FINISHED in-env but its payload has not reached the control
	//   plane. Pushed by the executor's outbox when the CP is reachable yet rejects the results, so
	//   the ledger can say "the results exist, they are not here yet" instead of "failed 0/0/0/0".
	//   'abandoned' is NOT in this set — an executor never claims it; only the watchdog concludes it.
	//   degraded (AC-11, migration 038) = the run completed and NOTHING failed, but at least one
	//   load-mode scenario's survival-plane read showed the SUT in distress. Never overrides
	//   'failed' — see mapper.go's ordering.
	Status  string  `json:"status"`
	Tallies Tallies `json:"tallies"`
	// FailureReason is WHY, in the executor's own words, when it could not produce results at all —
	// a config that would not parse, a SUT it could not reach, a materialize that failed. Empty
	// whenever the run actually ran: a completed run's story is its scenarios, not a sentence.
	//
	// It exists because the executor already KNEW. Live 2026-08-10, two k3d instances had been unable
	// to run anything for days behind `status=failed, tallies 0/0`, while the executor's log carried
	// "line 78: this field is now PER-TIER, not a single value. Replace public_url: ...". The cause
	// was one hop from the only screen anyone reads, and that hop did not exist.
	//
	// NOT a status and not parsed anywhere. Carried verbatim so the operator reads what the executor
	// read. The control plane caps its length rather than rejecting an over-long push (see
	// store.RecordResults): refusing a results push because its ERROR MESSAGE was too long would turn
	// a diagnosable run into an undiagnosable one.
	FailureReason    string           `json:"failure_reason,omitempty"`
	Scenarios        []ScenarioResult `json:"scenarios"`
	Annotations      Annotations      `json:"annotations,omitempty"`
	SetHash          string           `json:"set_hash"`
	DeepLink         string           `json:"deep_link,omitempty"`
	DeploymentMarker json.RawMessage  `json:"deployment_marker,omitempty"`
	// SutFingerprint is WHICH REVISION OF THE SUT THIS RUN ACTUALLY EXERCISED, read by the executor
	// from the SUT's own deployment probe at run start (migration 050). SutFingerprintAt is when that
	// read happened. Both empty/nil = NOT MEASURED, which the ledger stores as NULL and the web renders
	// grey — never as a guess.
	//
	// ⚠ THIS IS NOT DeploymentMarker, AND THE DIFFERENCE IS THE WHOLE POINT. DeploymentMarker is an
	// EDGE: the watcher emits it only when the fingerprint CHANGES, and it rides its own endpoint
	// (POST /fed/deployment), correctly absent from almost every run. SutFingerprint is a LEVEL: every
	// run from a capable executor carries one, whether or not anything was redeployed. One field cannot
	// be both — see 050_run_sut_fingerprint.up.sql for what goes wrong when someone tries.
	//
	// ⚠ ON THE LOCALITY RULE ABOVE: this is an IDENTIFIER, not evidence. It is the MEASURED counterpart
	// of Annotations.Commit, which is the CLAIMED one — the same class of opaque build id that SetHash
	// and ArtifactDigest already are, and it reveals nothing about what the run observed. The fence
	// forbids logs, sagas, DB rows and verdict classes (AC-6: verdict/evidence/expect); a build id is
	// none of those, and TestResultsPush_NoEvidenceFields agrees for a reason, not by luck.
	SutFingerprint   string     `json:"sut_fingerprint,omitempty"`
	SutFingerprintAt *time.Time `json:"sut_fingerprint_at,omitempty"`
	// VersionReadings (077) is a JSON ARRAY of VersionReading: for every Kubernetes
	// namespace the run touched (one empty-namespace reading on the compose tier), a key over the image
	// digests running there when the run began. A NON-EVIDENCE record, like SutFingerprint: it is not in
	// EvidenceBundleHash or ScenarioEvidenceRoot, takes no part in the artifact measurement and refuses
	// nothing. Raw JSON, not a typed slice, so a value the control plane cannot decode is dropped ALONE and
	// the run is still accepted. Absent on an older executor; an older control plane has no such field and
	// drops it (no DisallowUnknownFields on the wire).
	VersionReadings json.RawMessage `json:"version_readings,omitempty"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	FinishedAt      *time.Time      `json:"finished_at,omitempty"`
	DurationMs      int64           `json:"duration_ms,omitempty"`
	// EvidenceBundleHash (AC-6/VR-4) is sha256, hex-encoded, over the per-scenario EvidenceHash
	// values ABOVE, taken in ascending scenario-id order — a single digest for the run's whole
	// evidence set, so the run's binding does not depend on concatenating the evidence itself.
	EvidenceBundleHash string `json:"evidence_bundle_hash,omitempty"`
	// ArtifactMeasurement is the image-digest measurement a final/scheduled run took of the running SUT
	// BEFORE any scenario ran (internal/artifactmeasure.Measurement, JSON). ScenarioEvidenceRoot is the
	// scenario-only hash the bundle used to be. When a measurement is present,
	// EvidenceBundleHash == artifactmeasure.BundleHash(ScenarioEvidenceRoot, measurement) — the control
	// plane recomputes it and refuses a push where it does not hold. Both absent: an executor that does
	// not measure (0.3.45 and older) or a build/rehearsal run; EvidenceBundleHash is then the scenario
	// hash exactly as before. Identifiers, not evidence: image digests are the same class of opaque id
	// ArtifactDigest already is.
	ArtifactMeasurement  json.RawMessage `json:"artifact_measurement,omitempty"`
	ScenarioEvidenceRoot string          `json:"scenario_evidence_root,omitempty"`
	// LoadRamp (069) is the per-step result of every `AMQP Load` scenario the run
	// carried: a JSON ARRAY of report.LoadRampEntry {scenario_id, target, driver, steps[]}. Absent on every
	// run that had none, on an older executor, and on a non-terminal push; the control plane stores it with
	// COALESCE so a later push without it never erases it. MEASUREMENTS ONLY (sessions, rates, microsecond
	// quantiles, error counts by class, blocked windows): no threshold, address, credential or message
	// content crosses, the same locality class as DurationMs and ArtifactMeasurement. An older control plane
	// has no such field and drops it (no DisallowUnknownFields on the wire).
	LoadRamp json.RawMessage `json:"load_ramp,omitempty"`
	// ArtifactDigest (AC-6, echoing AC-3's RunAssignment.ArtifactDigest) is the artifact this run was
	// assigned to certify. Riding it on the push — not only on the run_request the CP already knows —
	// lets RecordResults bind a verdict to a digest even for a push with no run_request_id (a direct
	// run) and lets it detect a re-point without a second lookup of who asked for this run.
	ArtifactDigest string `json:"artifact_digest,omitempty"`
	// Outcome (AC-6) is the run's VERDICT — passed | failed | degraded (AC-11) — serialised generically so it does not
	// collide with the forbidden `verdict` key (TestResultsPush_NoEvidenceFields): a pass/fail
	// summary is already implicit in Tallies and is not evidence. Empty while the run is not yet
	// terminal (running | results_pending): there is no verdict to bind until then.
	Outcome string `json:"outcome,omitempty"`
	// Outputs (ARGUS-CMP-2) is the compare run's recorded outputs: a JSON ARRAY of
	// compare.ScenarioOutput {scenario_id, v, step, sample, state, reason, status, parts, hash, ...}.
	// DIGESTS AND SIZES ONLY: the type has no field for a body, a header value or a claim, and the control
	// plane decodes into that type and re-encodes it (compare.DecodeOutputs) so an unknown key cannot be
	// stored. Absent on every run that is not a compare run and on an older executor. Capped at 1 MiB
	// encoded.
	Outputs json.RawMessage `json:"outputs,omitempty"`
	// OutputsRoot is compare.OutputsRoot over the rows of Outputs; the control plane recomputes it and
	// drops BOTH on a mismatch (a measurement that does not verify is "not measured", never a refusal).
	OutputsRoot string `json:"outputs_root,omitempty"`
	// EnvFingerprint is the executor's environment fingerprint (envcapture.Fingerprint) taken before a
	// compare run: with the running image digests it names the version a cell was measured at. An
	// identifier, not evidence, like SutFingerprint.
	EnvFingerprint string `json:"env_fingerprint,omitempty"`
}

// RejectResponse rides every 401/403 (D-FED.2): the distinguished reason + the CP's CURRENT TIME, so
// the executor computes its clock offset and self-heals its future JWTs.
type RejectResponse struct {
	Reason     RejectReason `json:"reason"`
	ServerTime time.Time    `json:"server_time"`
}

// ValidTier reports whether t is one of the three canonical tiers.
func ValidTier(t string) bool { return t == "compose" || t == "k3d" || t == "managed" }

// ValidInstanceID reports whether id is an instance name onboarding would accept: lowercase letters,
// digits and hyphens, not starting or ending with a hyphen (onboard.ValidInstanceName, which checks it
// with messages, agrees — TestValidInstanceName_AgreesWithTheFederationRule).
//
// ⛔ IT IS A PATH RULE AS MUCH AS A NAME RULE. The id is joined into host paths — the update block's
// staging directory, which the block `rm -rf`s, and the manifest under the router state — and the
// control plane stores whatever a registration sends. `/` and `..` in an id would steer those paths
// anywhere on the operator's machine, so every place that builds a path from it checks this first.
func ValidInstanceID(id string) bool {
	if id == "" || strings.HasPrefix(id, "-") || strings.HasSuffix(id, "-") {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// WireTier maps the CONCRETE tier an operator onboards with onto the canonical tier vocabulary the
// control plane validates (ValidTier). The onboarder speaks in the cluster flavour the operator
// actually has — `--tier aks` selects Azure Files RWX, the ghcr-pull secret, an Always pull policy
// and the system-nodepool exclusion — but the CP only ever needed to know local-vs-managed.
//
// This normalization deliberately lives on the EXECUTOR side. Widening ValidTier instead would make
// every new cluster flavour require a control-plane redeploy before it could be onboarded at all;
// mapping here means a already-running CP accepts tomorrow's `--tier eks` unchanged. Getting this
// wrong is expensive and silent-ish: RegisterInstance rejects the unknown tier, and the onboarder
// only reports "the executor did not REGISTER within ~60s" — after the namespace, the executor and
// the copied pull secret are already up, with no teardown. Pinned by
// TestWireTier_outputIsAlwaysAcceptedByValidTier.
//
// Empty is returned unchanged so the CALLER's default (compose) still applies.
func WireTier(t string) string {
	switch strings.ToLower(t) {
	case "aks", "eks", "gke", "k8s-dev", "managed":
		return "managed"
	default:
		return strings.ToLower(t)
	}
}

// RegistrableTier reports whether an instance rendered with concrete tier t can register at all: the
// executor sends WireTier(t) and the control plane accepts only ValidTier's three. WireTier passes an
// unknown tier through unchanged, so a tier that fails this renders cleanly, deploys cleanly, and is
// refused at RegisterInstance with the whole execution plane already running — the failure the WireTier
// comment above describes, reached by a name nobody listed. Found live 2026-09-23: a fresh agent on a
// homelab k3s cluster passed `--tier k3s`, which `argus preflight` and `render-k8s` both accepted, and
// got `register: 500 invalid tier "k3s"`. `kind` and `minikube`, which the renderer treats as local
// tiers and preflight advertised, are refused the same way. The CALLERS check this before anything is
// created; empty is the caller's default and is not judged here.
func RegistrableTier(t string) bool { return ValidTier(WireTier(t)) }

// RegistrableTierHint is the one sentence every refusal of an unregistrable tier gives, so preflight
// and render-k8s cannot drift apart on what to type instead.
const RegistrableTierHint = "the control plane accepts k3d, compose, or a managed tier (aks | eks | gke | k8s-dev | managed). " +
	"On kind or minikube use --tier k3d, which renders identically. On any other cluster you run yourself " +
	"(k3s, kubeadm, a homelab), use --tier managed: it renders the same local-path / ReadWriteOnce storage an " +
	"unknown tier falls back to, and it registers"
