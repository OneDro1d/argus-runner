// Package preflight answers one question, machine-readably: is this machine able to deploy an
// Argus instance, and if not, exactly what is missing and what command fixes it.
//
// T2.3 of the MVP2 sprint. It exists because docs/DEPLOY-ARGUS.md §1 currently asks an operator to
// run five checks by hand and judge the output. An agent cannot judge; it needs a verdict and a
// next command.
//
// ⛔ THE RULE THIS PACKAGE IS BUILT AROUND: a probe that COULD NOT RUN must never report ok.
// "unknown" is its own status and it BLOCKS, exactly like "missing". This is not pedantry — the
// failure it prevents is the one this estate keeps hitting: a guard that finds nothing and reports
// success, a suite that skips and prints `ok`, a scanner that read the wrong files and printed
// CLEAN. Those all fail in the reassuring direction, and an error in the reassuring direction is
// never audited, because nobody double-checks good news.
//
// ⚠️ Every Check therefore carries a Subject: WHAT it actually looked at. A verdict without its
// subject is how a check answers a question you did not ask — the scanner above printed basenames,
// so nothing on screen contradicted the assumption that it had scanned the file it was given.
//
// ⛔ Nothing here ever prints a credential. The token check reports PRESENCE and nothing else: a
// transcript is stored, so a printed value counts as leaked.
package preflight

import (
	"fmt"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/buildinfo"
	"github.com/OneDro1d/argus-runner/internal/federation"
)

// Status is a check's outcome. There are three, and the third is the point of the package.
type Status string

const (
	// StatusOK: the probe ran and the thing is there.
	StatusOK Status = "ok"
	// StatusMissing: the probe ran and the thing is absent. Actionable — Fix says how.
	StatusMissing Status = "missing"
	// StatusUnknown: the probe COULD NOT RUN. Never treat this as ok. It blocks.
	StatusUnknown Status = "unknown"
)

// Check is one precondition, and its Subject is as load-bearing as its Status.
type Check struct {
	ID string `json:"id"`
	// What is the precondition in one human sentence.
	What string `json:"what"`
	// Subject is WHAT WAS EXAMINED — the context name, the URL, the classes actually found. It is
	// how a reader confirms the check answered the question they asked, rather than a neighbouring
	// one that happens to be true.
	Subject string `json:"subject"`
	Status  Status `json:"status"`
	// Detail explains a non-ok status in the reader's terms.
	Detail string `json:"detail,omitempty"`
	// Fix is the LITERAL next command or action. Not "configure kubectl" — the command.
	Fix string `json:"fix,omitempty"`
	// Blocking marks a check that must be ok before a deploy can start.
	Blocking bool `json:"blocking"`
}

// Report is the whole answer. Verdict is derived, never set by hand.
type Report struct {
	// Verdict is "ready" or "blocked". There is deliberately no third value: an agent has to
	// decide whether to proceed, and "mostly ready" is not a decision.
	Verdict string `json:"verdict"`
	// Blocking lists the ids of every check that stands in the way, in report order, so a caller
	// can act without re-deriving it from Checks.
	Blocking []string `json:"blocking,omitempty"`
	Checks   []Check  `json:"checks"`
}

// Input is what the caller knows before anything is probed.
type Input struct {
	// Tier decides which storage class the instance will need. An empty tier is NOT rejected here —
	// it is reported, because an empty tier silently means local-path/ReadWriteOnce and the whole
	// point of this check is to say so out loud.
	Tier string
	// KubeContext is the context the caller intends to deploy into. Required on a k8s tier and
	// never inherited: a deploy that used "whatever was current" is how you deploy to the wrong
	// cluster.
	KubeContext string
	// ControlPlaneURL is the CP the executor will report to.
	ControlPlaneURL string
	// TokenPresent says whether a control-plane token was found. The VALUE is never passed in.
	TokenPresent bool
	// TokenSource names where the token was looked for (e.g. an env var name), so a "missing"
	// verdict tells the operator where to put one.
	TokenSource string
	// Obs is the observability mode the onboarder will be given (--obs). Empty means bundled. Under "none"
	// onboarding starts no shared argus-obs stack, so the compose-host-ports check does not apply
	// (, UI-2).
	Obs string
}

// ObsModes are the values `argus onboard --obs` accepts (onboarding/onboard.sh), in the onboarder's order.
var ObsModes = []string{"bundled", "adopt", "export", "shared", "none"}

// ValidateObs refuses an --obs value the onboarder would refuse, naming the valid list. Empty is bundled.
func ValidateObs(mode string) error {
	if mode == "" {
		return nil
	}
	for _, m := range ObsModes {
		if m == mode {
			return nil
		}
	}
	return fmt.Errorf("--obs must be one of %s (got %q)", strings.Join(ObsModes, ", "), mode)
}

// Probes are the side-effecting lookups, behind an interface so the verdict logic can be tested
// without a cluster. Each returns an error when the probe COULD NOT RUN; that becomes
// StatusUnknown, never StatusOK and never StatusMissing.
//
// ⚠️ The distinction matters: "kubectl is not installed" and "the cluster has no such storage
// class" are different facts with different fixes, and collapsing them into one failure is how an
// operator ends up installing a storage class on a machine with no kubectl.
type Probes interface {
	// CurrentContext returns the kubectl context currently selected.
	CurrentContext() (string, error)
	// StorageClasses returns the names of the storage classes the cluster has.
	StorageClasses(kubeContext string) ([]string, error)
	// Reachable reports the HTTP status returned by a GET of url.
	Reachable(url string) (int, error)

	// DockerReady reports whether the Docker daemon answers. It returns an error
	// when Docker could not be asked, or did not answer: the compose tier cannot start without it.
	DockerReady() error
	// ComposeHostPorts returns the host ports the compose tier's stack publishes, READ from the kit's
	// own compose files — never a list written down here, which would drift from them. The error is
	// "the files could not be read", which is unknown, not "no ports".
	ComposeHostPorts() ([]int, error)
	// HostPortHolder says what, if anything, holds a host port right now. The error is "could not
	// find out", which is unknown, not "free".
	HostPortHolder(port int) (PortHolder, error)
}

// PortHolder is what holds one host port. The zero value means nothing does.
type PortHolder struct {
	// Held is true when something accepts connections on the port or a container publishes it.
	Held bool
	// Container is the Docker container publishing the port; empty when the holder is not Docker.
	Container string
	// Project is that container's compose project label.
	Project string
}

// StorageClassForTier is injected rather than imported so this package does not depend on the
// renderer (and so a test can pin the mapping without rendering a manifest). Callers pass
// k8srender.StorageDefaultsFor.
type StorageClassForTier func(tier string) (storageClass, accessMode string)

// Run executes every check and returns the report. It never panics on a nil probe result and never
// short-circuits: an operator wants the WHOLE list, because fixing one thing at a time across five
// round-trips is precisely the attention cost this command exists to remove.
func Run(in Input, p Probes, classFor StorageClassForTier) Report {
	var checks []Check

	checks = append(checks, checkKubeContext(in, p))
	checks = append(checks, checkTier(in))
	checks = append(checks, checkStorageClass(in, p, classFor))
	checks = append(checks, checkControlPlane(in, p))
	checks = append(checks, checkToken(in))
	// the compose tier (no kube context) runs on this machine's Docker and publishes host
	// ports; on a k8s tier both checks report not-applicable and never block.
	checks = append(checks, checkDocker(in, p))
	checks = append(checks, checkComposeHostPorts(in, p))

	rep := Report{Checks: checks}
	for _, c := range checks {
		if c.Blocking && c.Status != StatusOK {
			rep.Blocking = append(rep.Blocking, c.ID)
		}
	}
	if len(rep.Blocking) == 0 {
		rep.Verdict = "ready"
	} else {
		rep.Verdict = "blocked"
	}
	return rep
}

func checkKubeContext(in Input, p Probes) Check {
	c := Check{
		ID:       "kube-context",
		What:     "kubectl is available and the intended context is selected",
		Blocking: true,
	}
	current, err := p.CurrentContext()
	if err != nil {
		c.Status = StatusUnknown
		c.Subject = "kubectl current-context"
		c.Detail = fmt.Sprintf("could not ask kubectl which context is current: %v. This is NOT the "+
			"same as having the wrong context — the probe did not run.", err)
		c.Fix = "install kubectl and make sure a kubeconfig is readable, then re-run"
		return c
	}
	c.Subject = "current context " + quote(current)
	if in.KubeContext == "" {
		// Not an error: a compose-tier deploy needs no cluster. But say which context WOULD be
		// used, because silence here is what lets a stray current-context become the target.
		c.Status = StatusOK
		c.Detail = "no --kube-context requested; nothing will be deployed to a cluster. If you meant " +
			"to deploy to one, pass it explicitly — it is never inherited."
		return c
	}
	if current != in.KubeContext {
		c.Status = StatusMissing
		c.Subject = fmt.Sprintf("current context %s, wanted %s", quote(current), quote(in.KubeContext))
		c.Detail = "the current context is not the one you asked to deploy into"
		c.Fix = fmt.Sprintf("pass --kube-context %s to every argus and kubectl command "+
			"(preferred), or run: kubectl config use-context %s", in.KubeContext, in.KubeContext)
		return c
	}
	c.Status = StatusOK
	return c
}

// checkTier asks whether the control plane will accept the tier at all. It needs no probe: the answer
// is the same fold the executor applies when it registers (federation.RegistrableTier). Without it,
// preflight said `ready` to `--tier k3s` and the agent found out ~60s after its executor came up, as
// `register: 500 invalid tier "k3s"`, in a namespace it then had no permission to delete (2026-09-23).
func checkTier(in Input) Check {
	c := Check{
		ID:       "tier",
		What:     "the control plane accepts the tier this instance will register with",
		Subject:  "tier " + quote(in.Tier),
		Blocking: true,
	}
	if in.Tier == "" {
		// Reported by the storage-class check, which says what an empty tier resolves to.
		c.Status = StatusOK
		c.Detail = "no --tier given; the storage-class check reports what that resolves to"
		return c
	}
	if federation.RegistrableTier(in.Tier) {
		c.Status = StatusOK
		return c
	}
	c.Status = StatusMissing
	c.Subject = fmt.Sprintf("tier %s registers as %s", quote(in.Tier), quote(federation.WireTier(in.Tier)))
	c.Detail = "the executor would deploy and then be refused at registration, with its namespace already " +
		"created: " + federation.RegistrableTierHint
	c.Fix = "re-run with --tier managed (or k3d / aks), and pass the same --tier to render-k8s"
	return c
}

func checkStorageClass(in Input, p Probes, classFor StorageClassForTier) Check {
	want, mode := classFor(in.Tier)
	c := Check{
		ID: "storage-class",
		What: fmt.Sprintf("the cluster has the storage class tier %s needs (%s, %s)",
			quote(in.Tier), want, mode),
		Blocking: true,
	}
	if in.KubeContext == "" {
		c.Status = StatusOK
		c.Subject = "no cluster targeted"
		c.Detail = "no --kube-context requested, so no PersistentVolumeClaim will be created"
		return c
	}
	have, err := p.StorageClasses(in.KubeContext)
	if err != nil {
		c.Status = StatusUnknown
		c.Subject = "kubectl get storageclass on " + quote(in.KubeContext)
		c.Detail = fmt.Sprintf("could not list storage classes: %v. Treat this as unknown, not as "+
			"absent — the class may well be there.", err)
		c.Fix = "check the context is reachable: kubectl --context " + in.KubeContext + " get storageclass"
		return c
	}
	sorted := append([]string(nil), have...)
	sort.Strings(sorted)
	c.Subject = fmt.Sprintf("tier %s needs %s; cluster has [%s]", quote(in.Tier), want, strings.Join(sorted, " "))

	for _, h := range have {
		if h == want {
			c.Status = StatusOK
			if in.Tier == "" {
				// Present, but the operator almost certainly did not mean this.
				c.Detail = "⚠ the tier is EMPTY, which resolves to " + want + " + " + mode +
					". That is the documented fallback, not a default anyone chooses: with " +
					"ReadWriteOnce every replica pins to the node that bound the volume first. " +
					"Name your tier explicitly unless you meant this."
			}
			return c
		}
	}
	c.Status = StatusMissing
	c.Detail = fmt.Sprintf("the cluster has no storage class named %s. The PersistentVolumeClaim "+
		"will sit Pending and the executor will never become Ready — and nothing in that failure "+
		"names the tier as the cause, which is why this check exists.", want)
	if in.Tier == "" {
		c.Fix = "name your tier explicitly (--tier aks | k3d | managed). An empty tier " +
			"resolves to " + want + ", which this cluster does not have."
	} else {
		c.Fix = fmt.Sprintf("either install a storage class named %s, or choose a tier whose class this cluster "+
			"has: kubectl --context %s get storageclass", want, in.KubeContext)
	}
	return c
}

func checkControlPlane(in Input, p Probes) Check {
	c := Check{
		ID:       "control-plane",
		What:     "the control plane answers",
		Blocking: true,
	}
	if in.ControlPlaneURL == "" {
		c.Status = StatusMissing
		c.Subject = "no control-plane URL given"
		c.Detail = "without a control plane the executor has nowhere to register, and no run can be requested"
		c.Fix = "pass --control-plane <url> (the dev CP is " + buildinfo.DefaultControlPlane() + ")"
		return c
	}
	c.Subject = in.ControlPlaneURL
	code, err := p.Reachable(in.ControlPlaneURL)
	if err != nil {
		c.Status = StatusUnknown
		c.Detail = fmt.Sprintf("could not reach it: %v. The executor needs OUTBOUND HTTPS to the "+
			"control plane and nothing else — check egress before assuming the CP is down.", err)
		c.Fix = "curl -sS -o /dev/null -w '%{http_code}\\n' " + in.ControlPlaneURL
		return c
	}
	c.Subject = fmt.Sprintf("%s returned HTTP %d", in.ControlPlaneURL, code)
	// Any answer at all proves reachability, which is what this check is about. A 401 from a CP
	// that requires auth is a REACHABLE control plane; calling that a failure would send an
	// operator to debug the network over a working one.
	if code >= 500 {
		c.Status = StatusMissing
		c.Detail = "the control plane answered, but with a server error"
		c.Fix = "ask the Argus session whether the control plane is healthy before deploying"
		return c
	}
	c.Status = StatusOK
	return c
}

func checkToken(in Input) Check {
	c := Check{
		ID:       "control-plane-token",
		What:     "a control-plane token is available",
		Blocking: true,
		// ⛔ Subject names the SOURCE, never the value. A transcript is stored.
		Subject: "looked in " + orNone(in.TokenSource),
	}
	if in.TokenPresent {
		c.Status = StatusOK
		c.Detail = "a token is present. ⚠ This check does NOT prove it is valid or in scope — only " +
			"that one was found. The first cloud call is what proves the rest."
		return c
	}
	c.Status = StatusMissing
	c.Detail = "no control-plane token found"
	c.Fix = "argus cloud-login --control-plane <url> --scope author"
	return c
}

// composeOwnProjects are the compose projects whose containers holding a stack port are a RE-ONBOARD
// (`up -d` reuses them), not a conflict — the same rule onboarding/lib/host-ports.sh applies.
var composeOwnProjects = map[string]bool{"argus-obs": true}

// checkDocker: the compose tier needs a working Docker daemon. Docker that cannot be asked is unknown and
// blocks — a machine with no Docker used to be reported `ready`.
func checkDocker(in Input, p Probes) Check {
	c := Check{
		ID:       "docker",
		What:     "the Docker daemon answers (the compose tier runs the stack in Docker)",
		Blocking: true,
	}
	if in.KubeContext != "" {
		c.Status = StatusOK
		c.Subject = "not applicable: kube context " + quote(in.KubeContext) + " targeted"
		return c
	}
	c.Subject = "docker info"
	if err := p.DockerReady(); err != nil {
		c.Status = StatusUnknown
		c.Detail = fmt.Sprintf("could not get an answer from the Docker daemon: %v. This is NOT the same as a "+
			"stack that is down — nothing here could ask.", err)
		c.Fix = "start Docker (Docker Desktop, or: systemctl start docker), then confirm it answers: docker info"
		return c
	}
	c.Status = StatusOK
	return c
}

// checkComposeHostPorts: every host port the compose stack publishes, read from the kit's compose files by
// the probe, must be free or held by the stack's own project. All conflicts are reported at once.
func checkComposeHostPorts(in Input, p Probes) Check {
	c := Check{
		ID:       "compose-host-ports",
		What:     "the host ports the compose stack publishes are free",
		Blocking: true,
	}
	if in.KubeContext != "" {
		c.Status = StatusOK
		c.Subject = "not applicable: kube context " + quote(in.KubeContext) + " targeted"
		return c
	}
	// --obs none starts no shared argus-obs stack, so there are no ports of ours to check
	// (and the operator's own Grafana may hold :3000). No probe is called.
	if in.Obs == "none" {
		c.Status = StatusOK
		c.Subject = "not applicable: --obs none starts no shared obs stack"
		return c
	}
	ports, err := p.ComposeHostPorts()
	if err != nil {
		c.Status = StatusUnknown
		c.Subject = "the host ports in the kit's compose files"
		c.Detail = fmt.Sprintf("could not read which host ports the stack publishes: %v. Unknown, not free.", err)
		c.Fix = "run from the kit directory (argus init creates it) with Docker running, then: docker compose -f deploy/compose/docker-compose.obs-shared.yml -p argus-obs config"
		return c
	}
	if len(ports) == 0 {
		c.Status = StatusUnknown
		c.Subject = "the host ports in the kit's compose files (none read)"
		c.Detail = "no published host port was read from the compose files, which is not what the kit ships — " +
			"treating that as 'nothing to check' would pass a check that examined nothing"
		c.Fix = "docker compose -f deploy/compose/docker-compose.obs-shared.yml -p argus-obs config"
		return c
	}
	var held, fixes []string
	for _, port := range ports {
		h, herr := p.HostPortHolder(port)
		if herr != nil {
			c.Status = StatusUnknown
			c.Subject = fmt.Sprintf("host port %d (holder lookup failed)", port)
			c.Detail = fmt.Sprintf("could not find out what holds port %d: %v. Unknown, not free.", port, herr)
			c.Fix = fmt.Sprintf("docker ps --filter publish=%d", port)
			return c
		}
		if !h.Held || (h.Container != "" && composeOwnProjects[h.Project]) {
			continue
		}
		if h.Container != "" {
			held = append(held, fmt.Sprintf("%d (container %s)", port, h.Container))
			fixes = append(fixes, "docker stop "+h.Container)
		} else {
			held = append(held, fmt.Sprintf("%d (a process outside Docker)", port))
			fixes = append(fixes, fmt.Sprintf("lsof -nP -iTCP:%d -sTCP:LISTEN", port))
		}
	}
	if len(held) > 0 {
		c.Status = StatusMissing
		c.Subject = "held: " + strings.Join(held, ", ")
		c.Detail = "the stack publishes fixed host ports and `up` fails late, on the first taken one, when any is held"
		c.Fix = strings.Join(fixes, " ; ")
		return c
	}
	var ns []string
	for _, port := range ports {
		ns = append(ns, fmt.Sprint(port))
	}
	c.Status = StatusOK
	c.Subject = "free: " + strings.Join(ns, " ")
	return c
}

func quote(s string) string {
	if s == "" {
		return `""`
	}
	return `"` + s + `"`
}

func orNone(s string) string {
	if s == "" {
		return "(nowhere — no token source configured)"
	}
	return s
}
