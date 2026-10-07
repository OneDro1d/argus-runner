package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/k8srender"
	"github.com/OneDro1d/argus-runner/internal/onboard"
	"github.com/OneDro1d/argus-runner/internal/preflight"
)

// T2.3 — `argus preflight`. docs/DEPLOY-ARGUS.md §1 asks an operator to run five checks by hand and
// judge the output. An agent cannot judge; it needs a verdict and a next command. This is that, as
// JSON.
//
// ⛔ It is READ-ONLY by construction. Every probe here is a `get` or a GET: preflight is what you
// run when you do not yet know whether it is safe to act, so it must never be the thing that acts.
const preflightUsage = "usage: argus preflight [--tier <tier>] [--kube-context <ctx>] [--control-plane <url>]\n" +
	"                       [--obs bundled|adopt|export|shared|none]\n" +
	"  --obs none: onboarding starts no shared argus-obs stack, so its host ports are not checked.\n" +
	"  Reports, as JSON, whether this machine can deploy an Argus instance — and for anything\n" +
	"  missing, the literal command that fixes it. Read-only: it creates and changes nothing.\n" +
	"  Exit 0 when the verdict is \"ready\", 4 when it is \"blocked\".\n" +
	"  The control-plane credential is looked for in ARGUS_CP_AUTHOR_TOKEN (formerly\n" +
	"  ARGUS_CP_TOKEN, still read as a fallback), else the session file `argus cloud-login`\n" +
	"  wrote — the same order every cloud-* command uses. Its VALUE is never printed."

// cpTokenEnv is ONE of the two places a control-plane credential lives. Named once so the report can
// say where to put one.
const cpTokenEnv = envname.CPAuthorToken

// resolveCPToken answers "does this machine hold a control-plane credential", and it answers it the
// SAME WAY every cloud-* command does — sessionForCommand (main.go): an explicit
// ARGUS_CP_AUTHOR_TOKEN (formerly ARGUS_CP_TOKEN) wins, else the on-disk session `argus cloud-login`
// wrote.
//
// ⛔ This function used to read ARGUS_CP_TOKEN and nothing else, and that made preflight lie in the
// one direction that costs the most. A machine that had logged in — session on disk, every cloud-*
// command working — was reported `control-plane-token: missing`, verdict `blocked`, fix "run
// cloud-login". Measured 2026-09-23 on example-cluster: the author calls succeeded minutes either
// side of a preflight that said there was no token. A precondition check that a logged-in operator
// cannot pass sends them back through an interactive OAuth flow they had already completed.
//
// ⛔ PRESENCE ONLY. The token VALUE never leaves this function — not into the report, not into an
// error. A transcript is stored, so a printed credential counts as leaked.
func resolveCPToken(controlPlane string) (present bool, source string) {
	cred := lookupCPCredential(controlPlane)
	return cred.present, cred.source
}

// cpCredential is what lookupCPCredential learned. `value` is unexported and has exactly one consumer
// — `argus doctor`, which hands it to doctor.Classify and keeps only the facts (kind, scope, expiry),
// and presents it, unrenewed, as the bearer of its one read (GET /api/instances, doctorListInstances).
// Nothing else may read it, and nothing may print it.
type cpCredential struct {
	present        bool
	source         string
	value          string
	fromSession    bool      // found in the session store, which can renew it — an env var cannot
	refreshPresent bool      // the session store holds a refresh token
	obtainedAt     time.Time // when the session was obtained; zero when the store did not record it
	controlPlane   string    // the control plane that issued the session; "" for an env-var credential
}

// lookupCPCredential is the ONE resolution of "which control-plane credential would this machine
// present", shared by preflight (presence only) and doctor (kind and expiry). Two copies of this
// logic is how preflight came to disagree with every cloud-* command in the first place.
func lookupCPCredential(controlPlane string) cpCredential {
	if envname.Lookup(envname.CPAuthorToken, envname.CPAuthorTokenDeprecated) != "" {
		// Name the variable the value actually came from: "found in ARGUS_CP_AUTHOR_TOKEN" for a
		// credential that lives in the deprecated name would send the reader to the wrong place.
		if v := os.Getenv(envname.CPAuthorToken); v != "" {
			return cpCredential{present: true, source: cpTokenEnv, value: v}
		}
		return cpCredential{present: true, source: envname.CPAuthorTokenDeprecated, value: os.Getenv(envname.CPAuthorTokenDeprecated)}
	}
	path, err := onboard.DefaultSessionPath()
	if err != nil {
		// No config dir to look in. Say where we DID look, so "missing" is not mistaken for "we
		// looked everywhere".
		return cpCredential{source: cpTokenEnv}
	}
	both := cpTokenEnv + " and the session file " + path
	d, err := onboard.LoadSessionFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cpCredential{source: both}
		}
		// The file is there and unreadable. That is NOT evidence of a credential — an unreadable
		// session blocks exactly like an absent one (the `unknown` rule, DEPLOY-ARGUS.md §1).
		return cpCredential{source: path + " (present, but it could not be read as a session)"}
	}
	if d.AccessToken == "" {
		return cpCredential{source: path + " (present, but it holds no access token)"}
	}
	// A session is issued BY one control plane and is refused by any other, so a session for a
	// different CP is not a credential for this one.
	if controlPlane != "" && d.ControlPlane != "" && !sameControlPlane(d.ControlPlane, controlPlane) {
		return cpCredential{source: path + " (present, but it holds a session for " + d.ControlPlane + ")"}
	}
	return cpCredential{
		present:        true,
		source:         path,
		value:          d.AccessToken,
		fromSession:    true,
		refreshPresent: d.RefreshToken != "",
		obtainedAt:     d.ObtainedAt,
		controlPlane:   d.ControlPlane,
	}
}

// sameControlPlane compares two control-plane URLs the way an operator means them: a trailing slash
// is not a different control plane.
func sameControlPlane(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// execProbes is the real implementation: kubectl for cluster facts, one HTTP GET for the control
// plane. Each method returns an error ONLY when the probe could not run — never to mean "absent".
// preflight turns that error into "unknown", which blocks.
type execProbes struct{ timeout time.Duration }

func (p execProbes) run(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// kubectl ran and refused. Its own message is far more useful than ours.
			return "", fmt.Errorf("kubectl %s: %s", strings.Join(args, " "), strings.TrimSpace(out.String()))
		}
		return "", fmt.Errorf("could not execute kubectl: %w", err)
	}
	return strings.TrimSpace(out.String()), nil
}

func (p execProbes) CurrentContext() (string, error) {
	return p.run("config", "current-context")
}

func (p execProbes) StorageClasses(kubeContext string) ([]string, error) {
	args := []string{"get", "storageclass", "-o", "jsonpath={.items[*].metadata.name}"}
	if kubeContext != "" {
		args = append([]string{"--context", kubeContext}, args...)
	}
	out, err := p.run(args...)
	if err != nil {
		return nil, err
	}
	// An empty list is a real answer, not a failure: a cluster genuinely may have no storage class,
	// and that is a "missing" verdict with a fix, not an "unknown" one.
	return strings.Fields(out), nil
}

func (p execProbes) Reachable(url string) (int, error) {
	c := &http.Client{Timeout: p.timeout}
	// GET, never HEAD: some gateways answer HEAD differently, and a preflight that disagrees with
	// the thing it is predicting is worse than none.
	resp, err := c.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func cmdPreflight(args []string) int {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		tier        = fs.String("tier", "", "the tier you intend to deploy (aks | k3d | managed; managed for a cluster you run yourself, e.g. k3s). Empty is reported, not rejected.")
		kubeContext = fs.String("kube-context", "", "the kubectl context you intend to deploy into. Never inherited.")
		cp          = fs.String("control-plane", envOr("ARGUS_CP_URL", ""), "the control-plane URL")
		obs         = fs.String("obs", "", "the --obs mode you will onboard with ("+strings.Join(preflight.ObsModes, " | ")+"; default bundled). With none, no shared argus-obs stack starts, so its host ports are not checked.")
		timeout     = fs.Duration("timeout", 10*time.Second, "per-probe timeout")
	)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, preflightUsage)
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}

	if err := preflight.ValidateObs(*obs); err != nil {
		fmt.Fprintln(os.Stderr, "argus preflight: "+err.Error())
		fmt.Fprintln(os.Stderr, preflightUsage)
		return exitUsage
	}

	// ⛔ PRESENCE only. The value never enters this struct, so it cannot be printed by
	// accident — a transcript is stored, and a printed credential counts as leaked.
	tokenPresent, tokenSource := resolveCPToken(*cp)

	in := preflight.Input{
		Tier:            *tier,
		KubeContext:     *kubeContext,
		ControlPlaneURL: *cp,
		TokenPresent:    tokenPresent,
		TokenSource:     tokenSource,
		Obs:             *obs,
	}

	rep := preflight.Run(in, execProbes{timeout: *timeout}, k8srender.StorageDefaultsFor)
	emit(rep)

	if rep.Verdict != "ready" {
		// exitFailed, not exitErr: the command itself worked perfectly. It is the MACHINE that is
		// not ready, and a caller scripting this needs to tell those two apart.
		return exitFailed
	}
	return exitOK
}
