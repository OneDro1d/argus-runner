package doctor

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OneDro1d/argus-runner/internal/envname"
)

// Lifetimes the control plane issues its tokens with (internal/control/oauth/endpoints.go: AccessTTL
// 15m, RefreshTTL 30d). Mirrored, not imported: this package must stay free of the control plane's
// dependencies so `argus doctor` can run on a machine that has nothing else working.
const (
	accessTokenLife  = 15 * time.Minute
	refreshTokenLife = 30 * 24 * time.Hour
	// deviceCodeLife is how long a device sign-in code can be approved (oauth/endpoints.go DeviceTTL).
	deviceCodeLife = 15 * time.Minute
	// expiryWarning is how close to expiry a token that CANNOT renew itself is reported as a risk: a
	// run that starts with three minutes of credential left loses its control-plane session midway.
	expiryWarning = 5 * time.Minute
)

// CredentialInput is what the caller found out about the control-plane credential the CLI would
// present, resolved the SAME way every cloud-* command resolves it (ARGUS_CP_AUTHOR_TOKEN, formerly
// ARGUS_CP_TOKEN and still read as a fallback, else the session store). The token VALUE is deliberately
// not a field: Classify it first, pass the facts.
type CredentialInput struct {
	// Source is where the credential was found — or, when Present is false, where it was looked for.
	Source  string
	Present bool
	Facts   TokenFacts
	// FromSession says the credential came from the session store, which renews an expired access
	// token through its refresh token. An env var cannot renew anything.
	FromSession    bool
	RefreshPresent bool
	// ObtainedAt is when the session was obtained; zero when the store did not record it.
	ObtainedAt time.Time
	// ControlPlaneURL is used in fix commands only; may be empty.
	ControlPlaneURL string
	Now             time.Time
	// Refused is the control plane's own refusal of this credential, when executor-version's read (or the
	// renewal before it) got one. It outranks everything the value looks like.
	Refused CredentialRefusal
	// Accepted says the control plane ANSWERED doctor's own GET /api/instances with this credential. It is
	// false when doctor did not ask or the read failed for a reason that says nothing about the credential.
	Accepted bool
	// Renewed says that read carried an access token doctor renewed in memory, not the one found at Source: what
	// was accepted is the session, after a renewal, and the sentence must not claim more.
	Renewed bool
}

// CredentialRefusal is a control plane refusing the credential itself, as opposed to failing to answer.
// The zero value is "not refused", which includes "not asked".
type CredentialRefusal struct {
	// Status is the HTTP status: 401, not accepted at all; 403, accepted but refused for the author API;
	// 400 or 401 with Renewal, the refresh token was refused.
	Status int
	// Renewal says the refusal answered the refresh-token grant, not the read.
	Renewal bool
	// Msg is the control plane's answer, as the client reported it.
	Msg string
}

// CheckCredential names the KIND of credential present, and what that kind can and cannot do, before any
// command discovers it the hard way. It is not the whole credentials phase (38 of 169 recorded
// incidents): most of those were custody and process (a secret pasted into Slack, a spent Vault wrap)
// or SUT defects, which no local check can see.
func CheckCredential(in CredentialInput) Check {
	c := credentialKind(in)
	if in.Present && in.Refused.Status != 0 {
		refused(&c, in)
	}
	return c
}

// refused turns a kind verdict into the control plane's own. A refusal is a fact about the credential; the
// kind verdict was only what the value looks like ("not verified against the control plane").
func refused(c *Check, in CredentialInput) {
	r, cp := in.Refused, orURL(in.ControlPlaneURL)
	c.Status = StatusFail
	c.Fix = loginFix(in.ControlPlaneURL)
	if !in.FromSession {
		// The env var outranks the session store, so a sign-in alone would change nothing.
		c.Fix = bareTokenFix(in.ControlPlaneURL)
	}
	switch {
	case r.Renewal:
		c.Detail = "the access token has expired, and the control plane at " + cp + " refused the refresh token that would renew it (" +
			r.Msg + "): it was revoked (argus cloud-logout does that), has expired, or was issued by another control plane. " +
			"Every cloud-* command will be refused until you sign in again."
	case r.Status == 403:
		c.Detail = "the control plane at " + cp + " accepted it, then refused it for the author API (" + r.Msg + "): it signs in, " +
			"but not as an author. The usual cause is a builder token (#259: runner scope, same odts_ prefix as an author PAT) or a " +
			"runner-scope sign-in; the author__* tools (propose, validate, write, request_run) refuse both."
	default:
		c.Detail = "the control plane at " + cp + " refused it (" + r.Msg + "): it does not accept this credential at all. It was " +
			"revoked, has expired, was mistyped or cut short in a paste, or was issued by another control plane."
	}
}

// credentialKind is CheckCredential's verdict from the value alone.
func credentialKind(in CredentialInput) Check {
	c := Check{
		ID:      "control-plane-credential",
		What:    "the control-plane credential this machine would present is the kind the next command needs, and has not expired",
		Subject: "looked in " + orNowhere(in.Source),
	}
	login := loginFix(in.ControlPlaneURL)
	if !in.Present {
		c.Status = StatusFail
		c.Detail = "no control-plane credential found. Every cloud-* command, and `run` against a control plane, needs one."
		c.Fix = login
		return c
	}
	f := in.Facts
	switch f.Kind {
	case KindAuthorPAT:
		c.Subject = in.Source + ": an odts_ PAT"
		c.Status = StatusOK
		c.Detail = "long-lived. Most likely an author PAT, the default kind, which drives the author__* tools (propose, validate, write, " +
			"request_run) — but a builder token (#259: runner scope, one workspace or, by explicit choice, all of the owner's, minted on the Tokens page) has the same odts_ prefix, " +
			"and the author__* tools refuse one. Only the control plane's Tokens page shows which this is. "
		if in.Accepted {
			c.Detail += acceptedSentence(in)
		} else {
			c.Detail += "Not verified against the control plane: the first cloud call proves the rest."
		}
		return c
	case KindUnrecognised:
		c.Subject = in.Source + ": neither an odts_ PAT nor a JWT"
		c.Status = StatusWarn
		c.Detail = "the control plane accepts author PATs (odts_…) and its own OAuth access tokens (JWTs). " +
			"This value is neither, so the first cloud call will most likely answer 401 — unless it is a " +
			"credential kind this doctor does not know. A wrong paste (a runner id, a workspace id, a URL) " +
			"looks exactly like this."
		c.Fix = login
		return c
	case KindJWT:
		// the rest of this function
	default:
		c.Status = StatusUnknown
		c.Detail = "the credential could not be classified"
		c.Fix = login
		return c
	}
	if f.Malformed != "" {
		c.Subject = in.Source + ": a JWT whose claims could not be read"
		c.Status = StatusUnknown
		c.Detail = "its claims could not be read: " + f.Malformed + ". Nothing can be said about its scope or expiry — " +
			"and that is not the same as it being fine."
		c.Fix = login
		return c
	}
	scope := f.Scope
	if scope == "" {
		scope = "(no scope claim)"
	}
	c.Subject = fmt.Sprintf("%s: a JWT, scope %s, issuer %s", in.Source, scope, orNone(f.Issuer))
	renews := in.FromSession && in.RefreshPresent

	if f.HasExpiry {
		left := f.ExpiresAt.Sub(in.Now)
		switch {
		case left <= 0 && renews:
			age := in.Now.Sub(in.ObtainedAt)
			if !in.ObtainedAt.IsZero() && age > refreshTokenLife {
				c.Status = StatusFail
				c.Detail = fmt.Sprintf("the access token expired %s ago, and the refresh token that would renew it was obtained %s ago — "+
					"past its %s life. Every cloud-* command will be refused until you log in again.",
					human(-left), human(age), human(refreshTokenLife))
				c.Fix = login
				return c
			}
			c.Status = StatusOK
			c.Detail = fmt.Sprintf("the access token expired %s ago. That is routine: access tokens live %s, and every cloud-* "+
				"command renews one through the session store's refresh token (%s life; this session was obtained %s). Nothing to do.",
				human(-left), human(accessTokenLife), human(refreshTokenLife), obtained(in.ObtainedAt))
		case left <= 0:
			c.Status = StatusFail
			c.Detail = fmt.Sprintf("this access token expired %s ago, and where it is (%s) nothing can renew it: a bare access token lives %s "+
				"from issue. This is the --token-out / %s trap — the value was valid when it was copied and is stale now.",
				human(-left), in.Source, human(accessTokenLife), envname.CPAuthorToken)
			c.Fix = bareTokenFix(in.ControlPlaneURL)
			return c
		case left < expiryWarning && !renews:
			c.Status = StatusWarn
			c.Detail = fmt.Sprintf("this access token expires in %s, and where it is (%s) nothing can renew it. "+
				"A run that starts now may lose its control-plane session midway.", human(left), in.Source)
			c.Fix = bareTokenFix(in.ControlPlaneURL)
			return c
		default:
			c.Status = StatusOK
			c.Detail = fmt.Sprintf("valid for another %s.", human(left))
			if !renews {
				c.Detail += fmt.Sprintf(" It will not renew where it is (%s): access tokens live %s. For anything longer, use the "+
					"session store (%s) or mint an author PAT.", in.Source, human(accessTokenLife), login)
			}
		}
	} else {
		c.Status = StatusOK
		c.Detail = "no exp claim — this doctor cannot say when it expires."
	}

	if f.Scope == "runner" {
		// Whatever the expiry said, a runner token in the operator's hands is the wrong kind: it resolves to
		// the builder's hat (role.Product, control/cloudauth.go), not the author's. The 2026-09 Hub
		// onboarding lost a day to exactly this — a runner token saved where an author token was expected,
		// every author verb refused, no message saying why.
		c.Status = StatusWarn
		c.Detail = "a RUNNER-scope token: a builder presents it to runner__run. The author__* tools (propose, validate, write, " +
			"request_run) refuse it. " + c.Detail
		c.Fix = login
		return c
	}
	if in.FromSession {
		// B:16: an approve page answering invalid_request, with nothing on it saying no sign-in was waiting.
		c.Detail += fmt.Sprintf(" This machine is signed in, so no sign-in is waiting for approval: a device-approval page that answers "+
			"invalid_request (\"unknown, expired, or already-decided user code\") is showing a code that expired (codes live %s) or was "+
			"already used — there is nothing to approve.", human(deviceCodeLife))
	}
	c.Detail += " Claims were read, not verified — the control plane's signature check is what proves them."
	if in.Accepted {
		c.Detail += " " + acceptedSentence(in)
	}
	return c
}

// acceptedSentence: doctor's own GET /api/instances with the credential answered, which is the control plane's
// verdict on it (as far as the author API goes), so the credential is no longer only "what it looks like".
func acceptedSentence(in CredentialInput) string {
	if in.Renewed {
		return "The control plane at " + orURL(in.ControlPlaneURL) + " accepted this session: doctor renewed the expired access " +
			"token in memory, and its own GET /api/instances with the renewed token answered."
	}
	return "The control plane at " + orURL(in.ControlPlaneURL) + " accepted it: doctor's own GET /api/instances with this credential answered."
}

// ConfigInput is what the caller learned by parsing --config WITHOUT resolving ${VAR}s (an unset
// variable is a finding of its own, not a reason to stop diagnosing).
type ConfigInput struct {
	// Path is the --config given; empty when none was.
	Path string
	// Err is the parser's message when the file could not be read or parsed.
	Err         string
	ProjectName string
	// Validated says validate-config's local checks (config.Validate: target types, each scenario's layers
	// and **Target** against the declared targets, the money guard) ran against ScenariosDir. They are the
	// checks onboarding runs in a container whose stderr it drops on failure (A:8).
	Validated        bool
	ScenariosChecked int
	ScenariosDir     string
	// Invalid is what those checks refused, one line each, as the validator wrote them.
	Invalid []string
	// Warnings is what validate-config WARNS about, verbatim (toolcore.ConfigWarnings is the one source,
	// shared by both commands). A config with any is never reported as passing.
	Warnings []string
	// Unset lists each ${VAR} the config references that is unset or empty HERE, as "NAME (field)". Never a value.
	Unset []string
	// Portless lists the http targets whose literal base_url names no port. A base_url that is still a
	// ${VAR} is not judged: its value is not known here.
	Portless []string
	// Targets is how many targets the parsed config declares, counted by the enumeration validate-config
	// probes (toolcore.TargetCount); nil = no --config, or it did not parse.
	Targets *int
}

// CheckConfig answers the question every config-reading command asks first, in the doctor's terms.
func CheckConfig(in ConfigInput) Check {
	c := Check{
		ID:   "argus-config",
		What: "the SUT's argus-config.yaml is named, readable and parses",
	}
	if in.Path == "" {
		c.Subject = "no --config given"
		c.Status = StatusWarn
		c.Detail = "every run needs --config (there is no default: a forgotten --config used to run the bundled demo SUT " +
			"and print run_begin: ok). Nothing about the SUT could be checked; pass the same --config to doctor that you " +
			"will pass to run."
		c.Fix = "argus doctor --config <argus-config.yaml> --scenarios <dir>"
		return c
	}
	c.Subject = in.Path
	if in.Err != "" {
		c.Status = StatusFail
		c.Detail = in.Err
		c.Fix = "fix the error above, then: argus validate-config --config " + in.Path + " --scenarios <dir>"
		return c
	}
	if in.ProjectName == "" {
		c.Status = StatusWarn
		c.Detail = "project.name is empty, so nothing can say which product these scenarios are for"
		c.Fix = "set project.name in " + in.Path
		return c
	}
	c.Subject = in.Path + ": project " + quote(in.ProjectName)
	dir := in.ScenariosDir
	if dir == "" {
		dir = "<dir>"
	}
	validate := "argus validate-config --config " + in.Path + " --scenarios " + dir
	var found, fixes []string
	if len(in.Invalid) > 0 {
		found = append(found, fmt.Sprintf("validate-config's checks refuse %d of %d scenarios: %s.",
			len(in.Invalid), in.ScenariosChecked, strings.Join(in.Invalid, "; ")))
		fixes = append(fixes, "fix each refusal above, then: "+validate)
	}
	if len(in.Warnings) > 0 {
		found = append(found, "validate-config warns: "+strings.Join(in.Warnings, "; ")+".")
		fixes = append(fixes, "act on each warning above, then: "+validate)
	}
	if len(in.Portless) > 0 {
		// config.HTTPTarget.HostPort: an http base_url with no port keeps the historical localhost:8080 default.
		found = append(found, strings.Join(in.Portless, ", ")+" names no port, so Argus reaches it on :8080 (its own default), "+
			"not on :80 as a browser would.")
		fixes = append(fixes, "write the port the service listens on into "+strings.Join(in.Portless, ", ")+" (for example http://<service>:80)")
	}
	if len(in.Unset) > 0 {
		found = append(found, "unset or empty here: "+strings.Join(in.Unset, ", ")+". A run started from this machine refuses "+
			"(unresolved ${VAR}); a run on an executor resolves them from its own environment, so this matters only for runs started here.")
		fixes = append(fixes, "for runs from this machine, export them (the values live in the product folder's .env, never in the config)")
	}
	switch {
	case len(in.Invalid) > 0:
		c.Status = StatusFail
	case len(found) > 0:
		c.Status = StatusWarn
	default:
		c.Status = StatusOK
		if in.Validated {
			c.Detail = fmt.Sprintf("validate-config's checks pass for %d scenarios in %s.", in.ScenariosChecked, in.ScenariosDir)
		} else {
			c.Detail = "parsed, but not checked against scenarios: pass --scenarios <dir> to run validate-config's checks too."
		}
		return c
	}
	c.Detail = strings.Join(found, " ")
	c.Fix = strings.Join(fixes, "; ")
	return c
}

// ScenariosInput is what the caller found at --scenarios.
type ScenariosInput struct {
	Dir string
	// IsDefault says the directory is `run`'s built-in default — the OrderService demo bundled with
	// the kit — rather than something the operator chose.
	IsDefault bool
	Exists    bool
	// Files is the number of scenario files found (CountScenarioFiles); meaningful only when Exists.
	Files int
	// WalkErr is set when the directory exists but could not be read. Unknown, not absent.
	WalkErr string
	// ProjectName is the config's project.name, or empty when no config was readable.
	ProjectName string
	// Imported says the import preview ran: every file the catalog import (`cloud-seed-scenarios`, which
	// onboarding runs) would upload was put through the rules the control plane's author_write_scenario
	// refuses by (toolcore.ValidateAll), as this binary has them. ImportFiles is how many it would upload.
	Imported    bool
	ImportFiles int
	Refused     []ScenarioRefusal
	// ImportErr is why the import's own reader stopped. It reads every file before it writes any, so one
	// it cannot read means nothing is imported.
	ImportErr string
	// Version is this binary's: the rules checked are this build's, not the control plane's or an executor's.
	Version string
}

// ScenarioRefusal is one scenario file the catalog import would refuse.
type ScenarioRefusal struct {
	Path   string // relative to the scenarios directory, as the import names it
	ID     string // "" when the file declares none
	Errors int
	First  string // the first refusal, "line N: message"
}

// CheckScenarios exists because the scenarios directory has a default and the config does not, so the
// one mistake `run` cannot refuse at flag parsing is "your config, the demo's scenarios".
func CheckScenarios(in ScenariosInput) Check {
	c := Check{
		ID:      "scenarios-dir",
		What:    "the scenarios directory exists, holds scenarios, and belongs to the product under test",
		Subject: in.Dir,
	}
	if in.IsDefault {
		c.Subject = in.Dir + " (the built-in default: the bundled OrderService demo)"
	}
	if in.WalkErr != "" {
		c.Status = StatusUnknown
		c.Detail = "the directory exists but could not be read: " + in.WalkErr
		c.Fix = "ls -la " + in.Dir
		return c
	}
	if !in.Exists {
		if in.IsDefault {
			// a flag nobody passed is "not checked", never a failure. GETTING-STARTED's
			// own no-flag command (`argus doctor --control-plane <url>`, for a tester whose app someone
			// else onboarded) used to FAIL here only because --scenarios was missing.
			c.Status = StatusWarn
			c.Detail = "not checked: --scenarios was not given, and the built-in default (the bundled OrderService demo) is not " +
				"on this machine. Nothing about your scenarios was looked at. A local `run` would find nothing to run."
		} else {
			c.Status = StatusFail
			c.Detail = "the directory does not exist"
		}
		c.Fix = "pass --scenarios <dir>; in an onboarded kit your scenarios are in test-agent/scenarios (for example: argus doctor --scenarios test-agent/scenarios)"
		return c
	}
	if in.Files == 0 {
		c.Status = StatusFail
		c.Detail = "no scenario files (*.md) found. A run over an empty directory reports success with zero scenarios."
		c.Fix = "pass --scenarios <dir that holds your *.md scenarios>"
		return c
	}
	c.Subject += fmt.Sprintf(": %d scenario files", in.Files)
	if in.IsDefault && in.ProjectName != "" && !isOrderServiceName(in.ProjectName) {
		c.Status = StatusWarn
		c.Detail = fmt.Sprintf("these are the bundled OrderService demo scenarios, and the config is for project %s. "+
			"A run would drive the demo's scenarios against your product and report their failures as yours.",
			quote(in.ProjectName))
		c.Fix = "pass --scenarios test-agent/scenarios (the onboarded kit's folder, or wherever your scenarios live)"
		return c
	}
	if in.ImportErr != "" {
		c.Status = StatusUnknown
		c.Detail = "the catalog import (`cloud-seed-scenarios`, which onboarding runs) reads every file before it writes any, and one " +
			"here could not be read: " + in.ImportErr + ". Unless that is fixed, the import stops there and imports nothing."
		c.Fix = "fix or remove that file, then: argus doctor --scenarios " + in.Dir
		return c
	}
	// A:9, A:56: a file the import refuses never reaches the catalog, and the import said so only as a
	// count, after onboarding (VR10-S4 now names each reject — still only once onboarding has run).
	if len(in.Refused) > 0 {
		c.Status = StatusFail
		c.Detail = fmt.Sprintf("the catalog import would refuse %d of %d scenario files, so no run could ever select them: %s. "+
			"Checked by this argus %s's rules, the ones the control plane's author_write_scenario refuses by; the control "+
			"plane applies its own build's when you import, and an executor its own when it runs.",
			len(in.Refused), in.ImportFiles, refusals(in.Refused), orDash(in.Version))
		c.Fix = "fix each file named above, then re-check: argus doctor --scenarios " + in.Dir + ". Every error with its line: " +
			"argus validate-scenario --file <file> (local; it needs no token)."
		return c
	}
	c.Status = StatusOK
	if in.Imported {
		c.Detail = fmt.Sprintf("the catalog import would accept all %d files (this argus %s's rules).", in.ImportFiles, orDash(in.Version))
	}
	if in.ProjectName == "" {
		c.Detail = strings.TrimSpace(c.Detail + " No readable --config, so the scenarios were not compared with a project name.")
	}
	return c
}

// refusals names up to five refused files, then how many more. When every file fails first in the same
// words — a whole pack in a format the rules no longer accept (A:56) — it says those words once.
func refusals(rs []ScenarioRefusal) string {
	const shown = 5
	shared := len(rs) > 1
	for _, r := range rs[1:] {
		if withoutLine(r.First) != withoutLine(rs[0].First) {
			shared = false
			break
		}
	}
	var parts []string
	for i, r := range rs {
		if i == shown {
			parts = append(parts, fmt.Sprintf("and %d more", len(rs)-shown))
			break
		}
		id := ""
		if r.ID != "" {
			id = " (ID " + quote(r.ID) + ")"
		}
		errs := "errors"
		if r.Errors == 1 {
			errs = "error"
		}
		if shared {
			parts = append(parts, fmt.Sprintf("%s%s, %d %s", r.Path, id, r.Errors, errs))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s%s: %d %s, first %s", r.Path, id, r.Errors, errs, clip(r.First)))
	}
	if shared {
		return "each fails first on " + quote(clip(withoutLine(rs[0].First))) + ": " + strings.Join(parts, "; ")
	}
	return strings.Join(parts, "; ")
}

var lineRef = regexp.MustCompile(`^line \d+: `)

// withoutLine drops a scenario.Error's "line N: " prefix, which differs from file to file.
func withoutLine(s string) string { return lineRef.ReplaceAllString(s, "") }

// clip keeps at most 160 bytes of s, cut on a character boundary.
func clip(s string) string {
	if len(s) <= 160 {
		return s
	}
	i := 160
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "…"
}

func orDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

// CountScenarioFiles counts what the scenario loader would look at: *.md files under dir, recursively,
// minus anything named README* and anything under a dot-directory (internal/scenario/discover.go
// applies exactly these rules before parsing). It is a count of candidates, not of valid scenarios —
// `validate-config` is what parses them.
func CountScenarioFiles(dir string) (int, error) {
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(p) == ".md" && !strings.HasPrefix(d.Name(), "README") {
			n++
		}
		return nil
	})
	return n, err
}

// isOrderServiceName recognises the bundled demo's project name in the spellings it has been written.
func isOrderServiceName(name string) bool {
	n := strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "").Replace(name))
	return n == "orderservice" || n == "orderservicedemo"
}

func loginFix(cp string) string {
	if cp == "" {
		cp = "<url>"
	}
	return "argus cloud-login --control-plane " + cp + " --scope author"
}

// bareTokenFix: a PAT that lasts comes from the Tokens page (authorFix in tokenfile.go says why). It is NOT
// `argus cloud-mint-token --control-plane <url>`: without --router-state that command sends no router id and
// the control plane answers 400 "router_id is required for an auto token"; it mints this machine's author
// token (minted during onboarding), not a PAT to keep. Same correction as the refusal text in onboard.Session (#374).
func bareTokenFix(cp string) string {
	return "unset " + envname.CPAuthorToken + " and let the commands use the session store (" + loginFix(cp) + "), or for a credential " +
		"that lasts, create an author token on the control plane's Tokens page (" + orURL(cp) + ") after that sign-in"
}

func orURL(cp string) string {
	if cp == "" {
		return "<url>"
	}
	return cp
}

func orNowhere(s string) string {
	if s == "" {
		return "(nowhere — no credential source configured)"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func obtained(t time.Time) string {
	if t.IsZero() {
		return "at an unrecorded time"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// human renders a duration the way an operator reads one: seconds under a minute, minutes under an
// hour, hours under a day, else days.
func human(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int((d - time.Duration(h)*time.Hour).Round(time.Minute).Minutes())
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
