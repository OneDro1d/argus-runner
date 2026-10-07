// Package runner is the M3 in-env EXECUTOR deploy profile (ARGUS_MODE=runner, D-EXEC.2.8): the
// runner MCP tool set, the JMeter drive, and the federation client (register · long-poll · pickup ·
// results-push) that grafts the proven M2.5 runner core onto the D-FED wire (plan §5.2 I.2). One Go
// module, two profiles — this package holds the executor profile; internal/control holds the CP.
//
// At I.2 it wires the federation poll loop (client + identity + materialization + the M2.5 runner core
// + evidence-stripping push) alongside the service-anatomy floor. With no CP configured it degrades to
// the I.0 health-only skeleton.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/OneDro1d/argus-runner/internal/anatomy"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/federation"
)

// FedConfig, when CPURL is set, starts the executor poll loop against that control plane.
type FedConfig struct {
	CPURL         string
	InstanceID    string
	WorkspaceID   string // for register-on-start (stub until the login-based onboarding, I.4)
	IdentityPath  string // where the Ed25519 key lives (created if absent)
	SUTName       string
	Tier          string // compose | k3d | managed
	RunnerVersion string
	// Enrollment is the S1 first-register bearer (ARGUS_ENROLLMENT_TOKEN, CP-M3-120): required
	// by an enrollment-mode CP on the FIRST register; single-use server-side. Restarts re-register
	// via the machine JWT automatically (the client attaches it when no enrollment is armed).
	Enrollment string
	// Transport (AC-16, ARGUS_CP_TRANSPORT) selects how the federation client reaches the control
	// plane: "" or "fed" (default) keeps CPURL as the direct /fed/* HTTPS target; "mcp" makes MCPURL a
	// hub (or the control plane's own /mcp) that the client calls as tools/call executor__<verb>,
	// bearing HubToken plus the machine JWT. CPURL is unused in mcp mode.
	Transport string
	// MCPURL (ARGUS_CP_MCP_URL) is the tools/call endpoint when Transport is "mcp".
	MCPURL string
	// HubToken (ARGUS_EXECUTOR_HUB_TOKEN) is the bearer the hub expects when Transport is "mcp" — the
	// executor's own PAT (SY-11, pending); the machine JWT still rides every call as the envelope.
	HubToken string
	Exec     ExecConfig // the real run path (SUT config, results, compose, jmeter)
}

// Config configures the executor process.
type Config struct {
	// Addr is the service-anatomy listen address. Empty defaults to :8080.
	Addr string
	// Fed, when CPURL is set, engages the federation executor.
	Fed FedConfig
	// MCP, when set, is the in-env runner__* MCP surface mounted on THIS listener (R4 the MERGED
	// per-instance workload, M3.1 §D-3.1.1/ADR-11, CP-M3-125): ONE process owns the machine
	// identity + the federation loop + the MCP surface — the two-container identity-key load-order
	// race is dead by construction. The caller (cmd/argus) assembles the mcpserver handler.
	MCP http.Handler
	// Log is an optional structured logger for the poll loop.
	Log func(format string, args ...any)
}

// Server is a running (or ready-to-run) executor process.
type Server struct {
	addr    string
	ln      net.Listener
	httpSrv *http.Server
	exec    *Executor
	fed     FedConfig
	log     func(string, ...any)
	// x25519PubB64 (AC-4b) is this executor's minted X25519 public key, base64 std — carried here so
	// Run's register-on-start can hand it to registerRequestFor. "" when Bootstrap never reached the
	// federation branch (no CP configured).
	x25519PubB64 string
}

// Bootstrap binds the anatomy listener and, when a CP is configured, builds the federation executor
// (loading/creating the machine identity). Call Run to serve + poll.
func Bootstrap(ctx context.Context, cfg Config) (*Server, error) {
	_ = ctx
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	mux := http.NewServeMux()
	anatomy.Health(mux, func() bool { return true })
	// R4 (CP-M3-125): the merged workload serves the runner__* MCP surface on the same listener.
	if cfg.MCP != nil {
		for _, p := range []string{"/sse", "/message", "/mcp", "/metrics"} {
			mux.Handle(p, cfg.MCP)
		}
	}

	s := &Server{fed: cfg.Fed, log: cfg.Log}
	if cfg.Fed.CPURL != "" {
		priv, err := LoadOrCreateKey(cfg.Fed.IdentityPath)
		if err != nil {
			return nil, fmt.Errorf("runner: %w", err)
		}
		// AC-4b: the X25519 key. An existing "<identity>.x25519" file is used as-is; otherwise the key
		// is derived from the Ed25519 machine key and nothing is written, because on the Kubernetes
		// tiers the identity is a read-only Secret mount (identity.go). Its public half rides on
		// register (below); its private half opens a Sealed scenario body at execute time
		// (ExecConfig.X25519Priv, execute.go).
		x25519Priv, err := LoadOrDeriveX25519Key(cfg.Fed.IdentityPath+".x25519", priv)
		if err != nil {
			return nil, fmt.Errorf("runner: %w", err)
		}
		s.x25519PubB64 = X25519PublicKeyB64(x25519Priv)
		cfg.Fed.Exec.X25519Priv = x25519Priv
		// UC073 + migration 050: ONE probe closure over the SUT's declared deployment_probe, built HERE
		// and handed to BOTH readers -- the run path (a LEVEL: which revision did this run exercise?)
		// and the DeploymentWatcher below (an EDGE: did it just change?). nil when the SUT declares no
		// probe, which both readers already treat as "not measured". Two closures would let a later edit
		// change one probe and not the other, and the symptom would be the Live panel disagreeing with
		// the Timeline about the same SUT.
		//
		// ⛔ BUILT BEFORE THE EXECUTOR LITERAL, DELIBERATELY, for the reason the Floors comment below
		// gives. `NewRunFunc(cfg.Fed.Exec)` takes ExecConfig BY VALUE: a SutFingerprint assigned after
		// that line is written to a struct the run path has already copied, so the probe would be
		// silently absent from every run while the watcher still worked and every unit test stayed
		// green. Setting it beside X25519Priv, before anything reads cfg.Fed.Exec, makes the ordering
		// impossible to get wrong rather than merely documented.
		//
		// ParseUnresolved (not Load) so an unexpanded ${VAR} elsewhere in the config cannot block it.
		var sutProbe FingerprintFunc
		if p := cfg.Fed.Exec.ConfigPath; p != "" {
			if c, perr := config.ParseUnresolved(p); perr == nil {
				if u, f := c.DeploymentProbeURL(), c.DeploymentProbeField(); u != "" && f != "" {
					sutProbe = HTTPFingerprint(u, f, nil)
				}
			}
		}
		cfg.Fed.Exec.SutFingerprint = sutProbe
		// Artifact measurement: which image digests is the SUT actually running? Installed here, in the
		// same place and for the same by-value-copy reason as SutFingerprint above.
		if cfg.Fed.Exec.MeasureArtifact == nil {
			cfg.Fed.Exec.MeasureArtifact = systemMeasurer(cfg.Fed.Exec)
		}
		// Migration 077: the per-namespace version reader, installed for the same reason and
		// in the same place: NewRunFunc copies the struct below.
		if cfg.Fed.Exec.ReadNamespaceVersion == nil {
			cfg.Fed.Exec.ReadNamespaceVersion = namespaceVersionReader(cfg.Fed.Exec)
		}
		// VR-F7 / INT-014: keep a container-private copy, and SAY when the source has gone.
		//
		// On compose the key is a host file bind-mounted in. Delete the host file and the mount turns
		// unreadable while the container keeps running on the copy it read into memory — reporting
		// perfect health the whole time. That is how a wiped kit directory became a browser sign-in
		// at teardown, hours later and attributed to the wrong thing.
		cachePath := identityCachePath()
		if cerr := CacheKey(priv, cachePath); cerr != nil && cfg.Log != nil {
			// Not fatal: the in-memory key still works. But an executor whose identity cannot survive
			// a recreate should not be the only one who knows.
			cfg.Log("identity: could not cache the machine key at %s (%v) — this executor still works, "+
				"but if its identity source is lost it cannot be recovered from the running container", cachePath, cerr)
		}
		if serr := IdentitySourceReadable(cfg.Fed.IdentityPath); serr != nil && cfg.Log != nil {
			cfg.Log("identity: WARNING — the machine identity source is UNREADABLE (%v). "+
				"This executor keeps working from the key it loaded at start, but: recreating this "+
				"container would lose the identity, and teardown cannot read it back out. On compose "+
				"the usual cause is that the onboarding kit directory was deleted. Recovery: restore "+
				"the kit's identity.<instance>.key, or tear down with an author PAT "+
				"(ARGUS_CP_AUTHOR_TOKEN, formerly ARGUS_CP_TOKEN) which takes the workspace-authenticated path.", serr)
		}
		client := NewClient(cfg.Fed.CPURL, cfg.Fed.InstanceID, priv)
		if cfg.Fed.Transport == string(TransportMCP) {
			client = NewMCPClient(cfg.Fed.MCPURL, cfg.Fed.HubToken, cfg.Fed.InstanceID, priv)
		}
		// item 7b (msgbus tester 2026-09-28): wire the SAME authenticated catalog client, and this
		// executor's own logger, into the relay's ExecConfig — BEFORE the NewCommandFunc(cfg.Fed.Exec)
		// call below, which takes ExecConfig BY VALUE (the exact ordering trap the SutFingerprint
		// comment above already names for NewRunFunc: a field set after the call is invisible to it).
		cfg.Fed.Exec.CatalogClient = client
		cfg.Fed.Exec.Log = cfg.Log
		s.exec = &Executor{
			Client: client,
			Run:    NewRunFunc(cfg.Fed.Exec),
			// AC-17: the SAME ExecConfig, so a relayed builder command runs through the identical
			// toolcore path a real federated run already does.
			Commands: NewCommandFunc(cfg.Fed.Exec),
			// ARGUS-CMP-3: old compare outputs are removed only AFTER a push the control plane accepted,
			// keeping the newest 20 runs of this instance (OutputRetention). Set in the literal like
			// Floors below, so it cannot be forgotten by an ordering slip.
			AfterPush:     OutputRetention(cfg.Fed.Exec, nil),
			RunnerVersion: cfg.Fed.RunnerVersion,
			// VR9-C1 rule 5: what this executor needs to render its own update command, from the same
			// config and environment everything else here reads. KitDir comes from the env onboarding
			// set, exactly as the poll payload reads it (client.go) — and is legitimately empty on a
			// deployment that was never told it.
			Tier:     cfg.Fed.Tier,
			Instance: cfg.Fed.InstanceID,
			KitDir:   os.Getenv("ARGUS_KIT_DIR_HOST"),
			// VR10-U1 (V28-020): so the block this executor LOGS names the same cluster as the block
			// the control plane's page shows. Two sites assembling one renderer's input from two
			// sources is where they can still drift, and a forgotten field here would print a block
			// that runs against the wrong cluster while every unit test stayed green.
			KubeContext: os.Getenv("ARGUS_KUBE_CONTEXT_HOST"),
			Kubeconfig:  os.Getenv("ARGUS_KUBECONFIG_HOST"),
			// VR-V5: the durable floor store lives beside the identity key — the executor's existing
			// durable state, which survives a pod recreate on the shared results volume.
			//
			// IN THE LITERAL, DELIBERATELY. This was written 26 lines EARLIER as `s.exec.Floors = …`,
			// before `s.exec` was constructed — a nil-pointer dereference that segfaulted EVERY
			// executor with a control plane configured, on its first start. Assigning a field of a
			// struct that does not exist yet cannot be made safe by ordering discipline; putting it
			// here makes the ordering impossible to get wrong.
			Floors: NewFloorStore(cfg.Fed.IdentityPath),
			// F1 (UC065/UC060): terminal pushes go through the durable outbox on the results volume,
			// so a control plane that is unreachable when a run finishes costs a delay, not the run.
			Outbox: &Outbox{
				Dir: OutboxDir(cfg.Fed.Exec.ResultsRoot, cfg.Fed.InstanceID),
				Log: cfg.Log,
			},
			Log: cfg.Log,
			// VR6-W1: what the executor DIALS to answer "is this instance's SUT reachable?". It is the
			// only thing on the SUT's network, so it is the only vantage that can answer at all.
			//
			// nil when the config is absent or unresolvable — the probe then reports NOT MEASURED, which
			// the page renders as grey and explains, rather than inventing a verdict. See sutConfigFor for
			// why this loads RESOLVED while the deployment watcher below deliberately does not.
			SUTConfig: sutConfigFor(cfg.Fed.Exec.ConfigPath),
			// T5.4 follow-up: so Loop can recompute money_handling on every poll —
			// see Executor.ConfigPath's own doc for why it is carried here rather than re-derived
			// through ExecConfig, which Loop does not hold.
			ConfigPath: cfg.Fed.Exec.ConfigPath,
		}
		// UC073: if the SUT declares a deployment probe, watch it and auto-emit markers on redeploy.
		// The closure itself was built above, beside X25519Priv, so the run path's copy of ExecConfig
		// carries the same one; nil here means the SUT declared no probe and there is nothing to watch.
		if sutProbe != nil {
			cache := filepath.Join(filepath.Dir(cfg.Fed.IdentityPath), "deploy-fingerprint")
			s.exec.DeployWatch = NewDeploymentWatcher(sutProbe, cache)
		}
		// VR-V3 (V17-010): the UC069 SELF-DELETE wiring was removed here. Updating an executor is
		// always a person's act now, so there is nothing to arm. See selfupdate.go for why the deleted
		// mechanism was already inert on every tier we recommend.
		//
		// What remains is the OPERATOR'S on-request update (F15), which patches the Deployment IMAGE —
		// the thing self-delete could never do, because deleting a pod recreates it from the same spec.
		if cfg.Fed.Tier != "" && cfg.Fed.Tier != "compose" {
			podName, podNS := os.Getenv("POD_NAME"), os.Getenv("POD_NAMESPACE")
			if podName != "" && podNS != "" {
				s.exec.ImageUpdate = NewK8sImageUpdate(podNS, "executor", cfg.Log)
				// UC196: right-size the executor Deployment (idle→cheap, run→min-3). Same namespace,
				// same SA; the Deployment is always named "executor" in the rendered manifest. The
				// floors + cooldown are env-tunable (operators; also lets a test use a short cooldown), and
				// the active floor is capped at 1 on a volume that is not ReadWriteMany (#215).
				s.exec.Autoscale = newAutoscalerFromEnv(podNS, cfg.Log)
			} else {
				cfg.Log("W9 self-update DISABLED on tier %q: POD_NAME/POD_NAMESPACE absent (downward API not wired) — will fall back to outdated-blocked", cfg.Fed.Tier)
			}
		}
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("runner: listen %s: %w", cfg.Addr, err)
	}
	s.addr = ln.Addr().String()
	s.ln = ln
	s.httpSrv = &http.Server{Handler: mux}
	return s, nil
}

// Addr is the actual bound address.
func (s *Server) Addr() string { return s.addr }

// Mode identifies this deploy profile.
func (s *Server) Mode() string { return "runner" }

// Run serves the anatomy floor and, when configured, registers-on-start then runs the federation poll
// loop, until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	if s.exec != nil {
		if s.fed.WorkspaceID != "" {
			if s.fed.Enrollment != "" {
				s.exec.Client.SetEnrollment(s.fed.Enrollment) // S1 first-register bearer (CP-M3-120)
			}
			// register-on-start (idempotent upsert; re-registers a changed config, D-FED.2). Best-
			// effort — a failure just means the first polls get unknown_instance and self-heal.
			if err := s.exec.Client.Register(ctx, registerRequestFor(
				s.fed, s.exec.Client.PublicKeyB64(), s.x25519PubB64, declaredTargets(s.fed.Exec.ConfigPath),
				moneyHandlingFor(s.fed.Exec.ConfigPath), moneyWritesAllowFor(s.fed.Exec.ConfigPath),
				// (UI-6): the instance's own test_targets declaration.
				withRegisterTestTargets(testTargetsFor(s.fed.Exec.ConfigPath)),
				// (UI-2): the operator's dashboard link template (three-valued).
				withRegisterDashboardLink(dashboardLinkFor(s.fed.Exec.ConfigPath)),
			)); err != nil && s.log != nil {
				s.log("register-on-start failed (will self-heal via polling): %v", err)
			}
		}
		go func() { _ = s.exec.Loop(ctx) }()
	}
	return anatomy.Serve(ctx, s.httpSrv, s.ln, 10*time.Second)
}

// registerRequestFor builds the registration payload. Extracted so the payload's CONTENT is testable —
// it used to be an inline literal, and INT-026 was a field nobody assigned inside it.
//
// INT-026 (2026-08-08): SuiteVersion was plumbed end to end and never set. It exists on the wire
// (wire.go:24), the DB column is written and read back, and instanceview.go:38 renders it — but no
// caller assigned it, so all seven instances and every database row carried "". UC044 names it among
// the fields get_executor_status must return, and UC102.4 wants a drift chip when the SUITE version is
// older than the newest in the workspace; with every value empty that comparison is silently vacuous.
// The chip was absent not because there was no drift, but because drift was uncomputable — while the
// estate WAS drifted (social-aks-v1 on m3-iii35 against six on m3-iii34).
//
// In M3 the executor binary and the test suite ship as ONE image, so the suite version IS the runner
// version. Populating it duplicates a value rather than adding a fact; the alternative is deleting the
// field and re-specifying the chip against runner_version. Populating is smaller and reversible — the
// wire field, the column, the view and two UCs all name suite_version — and it makes UC044 and UC102
// correct today. A test pins the two together, so a future divergence has to be deliberate.
func registerRequestFor(fed FedConfig, pubKeyB64, x25519PubKeyB64 string, declared json.RawMessage, moneyHandling *bool, moneyWritesAllow json.RawMessage, opts ...RegisterOption) federation.RegisterRequest {
	req := federation.RegisterRequest{
		InstanceID: fed.InstanceID, WorkspaceID: fed.WorkspaceID, SUTName: fed.SUTName,
		Tier: fed.Tier, PublicKey: pubKeyB64,
		// T5.4 follow-up: computed by moneyHandlingFor the same way DeclaredTargets
		// is, immediately above — see its doc for the three-valued nil/false/true rule.
		MoneyHandling: moneyHandling,
		// money_writes follow-up (2026-09-26): computed by moneyWritesAllowFor the same way, one
		// level down — see its doc for the nil/"[]"/"[{…}]" rule.
		MoneyWritesAllow: moneyWritesAllow,
		// AC-4b: the executor's SECOND registered key, riding the SAME struct both transports marshal
		// whole (client.go's Register/registerMCP) — one assignment here covers register-on-start over
		// /fed/register AND executor__register. Empty when this executor has not minted one yet: never
		// refused, the control plane simply ships this instance's EXPECT in the clear (as it always has).
		X25519PublicKey: x25519PubKeyB64,
		RunnerVersion:   fed.RunnerVersion,
		SuiteVersion:    fed.RunnerVersion, // one image in M3 — see the note above
		ProtocolVersion: federation.ProtocolVersion,
		DeclaredTargets: declared, // UC030 coverability source
		// U7: passed through from onboarding via the executor's environment. The executor cannot DERIVE
		// these — it sees /config and /scenarios inside its own container and has no idea what they are
		// mounted from — so onboarding has to tell it.
		ProductDir:  os.Getenv("ARGUS_PRODUCT_DIR_HOST"),
		KitDir:      os.Getenv("ARGUS_KIT_DIR_HOST"),
		TestDir:     os.Getenv("ARGUS_TEST_DIR_HOST"),
		OnboardHost: os.Getenv("ARGUS_ONBOARD_HOST"),
		// VR10-U1 (V28-020): the cluster this instance was onboarded against — the ONE moment a
		// fresh instance can tell the control plane, since the poll only repairs what registration
		// already created.
		KubeContext: os.Getenv("ARGUS_KUBE_CONTEXT_HOST"),
		Kubeconfig:  os.Getenv("ARGUS_KUBECONFIG_HOST"),
	}
	for _, o := range opts {
		o(&req)
	}
	return req
}
