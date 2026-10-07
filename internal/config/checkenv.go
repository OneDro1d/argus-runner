package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// checkenv.go -- / a declared home for the environment variables a
// CHECK uses.
//
//	# argus-config.yaml, TOP LEVEL, beside `targets:` (never under it)
//	check_env:
//	  - SOME_PASSWORD            # a NAME the checks write as ${SOME_PASSWORD}, never a value
//
// Until this key, the only names the suite carried to the executor were the ${VAR}s written in the
// fixed credential fields walkCredentialFields visits. A check body that needs its own secret (a
// password in an HTTP body, a chain step's header) had no field to declare it in, so a developer
// parked the name under targets.auth.bearer_token just to get it delivered. This list is that
// declaration: it feeds EnvRefs, so `secrets-scan`, the onboarding preflight, the render-k8s Secret
// and the doctor's unset-name report all treat a declared name exactly like a credential reference.
//
// ⚠ DELIVERY, NOT USE. This list decides what onboarding DELIVERS to the executor. It does not restrict
// what a check may substitute: resolveVars (internal/argus/mcp_scenario.go) fills ANY name set in the
// executor's environment, declared or not (, OPEN).
// ⛔ TOP-LEVEL, never under `targets`, for the reason load_allowed_targets is: `targets` is decoded
// strictly, so an executor built before this key would refuse it there. A top-level key falls into an
// older executor's lenient inline map and is IGNORED -- silently, which is why the docs say to upgrade
// the executor before relying on it (an ignored key means the name is never scanned or delivered).
// ⛔ NAMES ONLY. A value is never read, stored or printed here, and a refusal never echoes the entry:
// an author who pasted `NAME=value` by mistake must not see the value in a validate-config line.

// CheckEnvMax bounds the list; a check that needs more than this is carrying a configuration file's
// worth of settings, not a handful of secrets.
const CheckEnvMax = 64

// checkEnvReservedPrefix is the namespace Argus's own variables live in (ARGUS_RUNNER_TOKEN, ...). A
// declared name under it would be copied into the executor's Secret beside, and over, the executor's
// own credentials.
const checkEnvReservedPrefix = "ARGUS_"

// CheckEnvReservedNames and CheckEnvReservedPrefixes are the ONE list of names `check_env` refuses
// because they change how the executor process itself runs (and, on Kubernetes, a declared name's value
// is written into the Secret the executor pod takes its environment from). The schema mirrors this list
// (schemas/argus-config.schema.yaml, check_env.items.allOf) and TestCheckEnv_ReservedListAndSchemaDoNotDrift
// fails when the two differ.
func CheckEnvReservedNames() []string {
	return []string{"PATH", "HOME", "USER", "SHELL", "PWD", "HOSTNAME", "TMPDIR",
		"JAVA_TOOL_OPTIONS", "JAVA_HOME", "GODEBUG", "SSL_CERT_FILE", "SSL_CERT_DIR",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"}
}

// CheckEnvReservedPrefixes: ARGUS_ is Argus's own namespace (the executor's token and settings); LD_ is the
// dynamic linker's (LD_PRELOAD runs code in the executor); KUBERNETES_ is the cluster's own service
// discovery (KUBERNETES_SERVICE_HOST repoints the executor's API client).
func CheckEnvReservedPrefixes() []string {
	return []string{checkEnvReservedPrefix, "LD_", "KUBERNETES_"}
}

func reservedPrefix(n string) string {
	for _, p := range CheckEnvReservedPrefixes() {
		if strings.HasPrefix(n, p) {
			return p
		}
	}
	return ""
}

func reservedName(n string) bool {
	for _, r := range CheckEnvReservedNames() {
		if n == r {
			return true
		}
	}
	return false
}

// validateCheckEnv is the load-time check (so validate-config and onboarding see it).
func (c *Config) validateCheckEnv() error {
	if len(c.CheckEnv) > CheckEnvMax {
		return fmt.Errorf("check_env: %d names declared, at most %d are accepted", len(c.CheckEnv), CheckEnvMax)
	}
	var errs []string
	for i, n := range c.CheckEnv {
		where := fmt.Sprintf("check_env[%d]", i)
		switch {
		case !scenario.ValidURLEnv(n):
			// Not echoed: it may be a value pasted by mistake.
			errs = append(errs, where+" is not an environment variable NAME — use letters A-Z, digits and underscores, not starting with a digit (names only, never a value)")
		case strings.HasPrefix(n, checkEnvReservedPrefix):
			errs = append(errs, fmt.Sprintf("%s: %s... is reserved for Argus's own variables — use the name your product already uses", where, checkEnvReservedPrefix))
		case reservedPrefix(n) != "":
			// The rule is named, the entry is not.
			errs = append(errs, fmt.Sprintf("%s: a name starting %s... is reserved because it changes how the executor process itself runs — use the name your product already uses", where, reservedPrefix(n)))
		case reservedName(n):
			errs = append(errs, where+": this name is reserved because it changes how the executor process itself runs (process, library-loading, certificate, proxy and runtime-tuning variables) — use the name your product already uses")
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// CheckEnvNames returns the declared names, de-duplicated, in first-declared order. A name that is
// listed twice is one name.
func (c *Config) CheckEnvNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range c.CheckEnv {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// credentialRefNames is the set of names the credential fields reference (runtime placeholders
// excluded). A declared name that is also referenced there is already scanned and checked under that
// field's label, so it is not listed a second time.
func (c *Config) credentialRefNames() map[string]bool {
	out := map[string]bool{}
	c.walkCredentialFields(func(field, _ string) string {
		for _, m := range envVarRe.FindAllStringSubmatch(field, -1) {
			if !runtimeVars[m[1]] {
				out[m[1]] = true
			}
		}
		return field
	})
	return out
}

// declaredEnvRefs lists the declared names the credential fields do not already reference.
func (c *Config) declaredEnvRefs() []EnvRef {
	have := c.credentialRefNames()
	var out []EnvRef
	for _, n := range c.CheckEnvNames() {
		if !have[n] {
			out = append(out, EnvRef{Name: n, Field: "check_env"})
		}
	}
	return out
}

// declaredEnvMissing is expandEnv's half: the declared names that are unset or empty in the
// environment, in the same "NAME (referenced by <field>)" shape. It never reads a value out.
func (c *Config) declaredEnvMissing() []string {
	var missing []string
	for _, r := range c.declaredEnvRefs() {
		if v, ok := os.LookupEnv(r.Name); ok && v != "" {
			continue
		}
		missing = append(missing, fmt.Sprintf("%s (referenced by %s)", r.Name, r.Field))
	}
	return missing
}
