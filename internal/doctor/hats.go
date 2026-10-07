package doctor

import (
	"strings"

	"github.com/OneDro1d/argus-runner/internal/auth"
	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/role"
)

// HatsInput is what doctor is allowed to know about the local (M2.5) auth model's three env vars —
// ARGUS_TOKEN (what a verb presents), ARGUS_RUNNER_TOKEN and ARGUS_EXECUTOR_SECRET (formerly
// ARGUS_AUTHOR_TOKEN, #280; the two hats it is matched against). Build it with HatsFromValues; the
// values stay there.
type HatsInput struct {
	Presented bool // ARGUS_TOKEN is set
	Runner    bool // ARGUS_RUNNER_TOKEN is set
	Author    bool // ARGUS_EXECUTOR_SECRET (formerly ARGUS_AUTHOR_TOKEN) is set
	// ConfigErr is auth.Config.Validate's message when the map itself is invalid (a hat unset, or the
	// two equal). Empty when the map is valid, or when nothing at all is set.
	ConfigErr string
	// Hat is the role the presented token resolves to — role.Test (the author hat) or role.Product
	// (the runner hat) — or "" when it resolves to none.
	Hat role.Role
	// VerifyErr is auth's reason when a presented token resolved to no hat.
	VerifyErr string
	// ControlPlaneToken says ARGUS_TOKEN classifies (Classify; the kind only, never the value) as a control-plane
	// credential, an odts_ PAT or a JWT. It is what a cloud tester holds there: the cloud-* commands read it,
	// the local verbs never do.
	ControlPlaneToken bool
}

// HatsFromValues runs the SAME verifier the CLI and the MCP server run (internal/auth), so the verdict
// here is the verdict a verb would get — not a re-implementation that could drift from it.
func HatsFromValues(presented, runner, author string) HatsInput {
	in := HatsInput{Presented: presented != "", Runner: runner != "", Author: author != ""}
	if !in.Presented && !in.Runner && !in.Author {
		return in
	}
	cfg := auth.Config{RunnerToken: runner, AuthorToken: author}
	if err := cfg.Validate(); err != nil {
		in.ConfigErr = err.Error()
		k := Classify(presented).Kind
		in.ControlPlaneToken = k == KindAuthorPAT || k == KindJWT
		return in
	}
	if presented == "" {
		return in
	}
	hat, err := cfg.Verify(presented)
	if err != nil {
		in.VerifyErr = err.Error()
		return in
	}
	in.Hat = hat
	return in
}

const presentAuthorHat = `export ARGUS_TOKEN="$` + envname.ExecutorSecret + `"   # author verbs (propose, validate, write); "$ARGUS_RUNNER_TOKEN" for run-only`

// setRoleMap is the literal fix for a missing role map: two placeholders, because the values are any two
// distinct strings nobody issues.
const setRoleMap = "export ARGUS_RUNNER_TOKEN=<one string> " + envname.ExecutorSecret + "=<a different string>; then " + presentAuthorHat

// CheckLocalHats names the confusion behind the most-quoted first-run error, `denied`: the two hat
// variables are a ROLE MAP, not credentials anyone issues, and the presented token has to equal one
// of them. Nothing in the `denied` message said so.
func CheckLocalHats(in HatsInput) Check {
	c := Check{
		ID:      "local-hats",
		What:    "the local (M2.5) hat map is either absent or consistent with the token you would present",
		Subject: describeHats(in),
	}
	switch {
	case !in.Presented && !in.Runner && !in.Author:
		c.Status = StatusOK
		c.Detail = "none of the three is set, which is fine for the cloud-* path (it authenticates through the session " +
			"store). `validate-scenario` (like `validate-config`) needs no token at all. The local verbs behind the auth gate — `list-scenarios`, `read-scenario`, " +
			"`write-scenario`, `run`, `serve`, `mcp-call` among them — refuse to start with \"auth not configured\" until " +
			"ARGUS_RUNNER_TOKEN and " + envname.ExecutorSecret + " are set and differ. They are a ROLE MAP, not credentials " +
			"anyone issues: any two distinct strings work. A local verb then needs one of them presented, too: " + presentAuthorHat + "."
	case in.ConfigErr != "" && in.ControlPlaneToken && !in.Runner && !in.Author:
		// A working cloud tester: ARGUS_TOKEN carries the control-plane PAT, and no local role map was ever needed.
		c.Status = StatusWarn
		c.Detail = "ARGUS_TOKEN holds a control-plane token, which only the cloud-* commands use; the local verbs (`run`, `serve`, " +
			"`write-scenario`, ...) are not configured on this machine, which is fine if you only use cloud commands. To use the local " +
			"verbs, set the role map (ARGUS_RUNNER_TOKEN and " + envname.ExecutorSecret + ", two distinct strings nobody issues) and present one of them."
		c.Fix = setRoleMap
	case in.ConfigErr != "":
		c.Status = StatusFail
		c.Detail = in.ConfigErr + ". ARGUS_RUNNER_TOKEN and " + envname.ExecutorSecret + " are the ROLE MAP a presented token is " +
			"matched against — nobody issues them; any two distinct strings work — and every local verb refuses to start " +
			"until both are set and differ."
		c.Fix = setRoleMap
	case !in.Presented:
		c.Status = StatusWarn
		c.Detail = "the map is configured but nothing is presented: every local verb exits denied with " +
			`"supply --token or ARGUS_TOKEN". --role is not an auth mechanism.`
		c.Fix = presentAuthorHat
	case in.VerifyErr != "":
		c.Status = StatusFail
		c.Detail = in.VerifyErr + ": the presented ARGUS_TOKEN equals neither hat, so every local verb exits denied. " +
			"The matched token IS the hat — there is no third value that works."
		c.Fix = presentAuthorHat
	case in.Hat == role.Test:
		c.Status = StatusOK
		c.Detail = "ARGUS_TOKEN is the author hat (runner + author scope): every local verb is available."
	case in.Hat == role.Product:
		c.Status = StatusOK
		c.Detail = "ARGUS_TOKEN is the runner hat (runner scope only): `run` works; the scenario-side (author) commands — " +
			"propose, write — will be refused (validating needs no token). If you need them, present " + envname.ExecutorSecret + " instead."
	default:
		c.Status = StatusUnknown
		c.Detail = "the verifier answered with a hat this doctor does not know: " + quote(string(in.Hat))
	}
	return c
}

func describeHats(in HatsInput) string {
	var set, unset []string
	name := func(ok bool, v string) {
		if ok {
			set = append(set, v)
		} else {
			unset = append(unset, v)
		}
	}
	name(in.Presented, "ARGUS_TOKEN")
	name(in.Runner, "ARGUS_RUNNER_TOKEN")
	name(in.Author, envname.ExecutorSecret)
	switch {
	case len(set) == 0:
		return "ARGUS_TOKEN, ARGUS_RUNNER_TOKEN, " + envname.ExecutorSecret + ": none set"
	case len(unset) == 0:
		return strings.Join(set, ", ") + ": all set"
	default:
		return strings.Join(set, ", ") + " set; " + strings.Join(unset, ", ") + " unset"
	}
}
