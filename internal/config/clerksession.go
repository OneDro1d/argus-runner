package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// ── auth.type: clerk_session ───────────────────────────────────────────────────
//
// A SUT behind Clerk accepts a short-lived session JWT, not a static token. The executor mints one from
// Clerk's Backend API (internal/clerksession) and sends it exactly where the static bearer goes
// (argus.DeriveProps -> `auth.header`). This file is the CONFIG half: the keys, the load-time refusals,
// the production guard, and the lookup of which auth block a scenario uses.
//
// THE REFERENCES ARE NAMES, NOT VALUES. `secret_key_env` and `user_id_env` hold the NAMES of environment
// variables — the same convention as `check_env` and the `${basic_auth:<user>:<ENV_VAR>}` helper — and the
// values come from the executor's environment (the Secret `exec-tokens`). A config file therefore never
// holds the Clerk secret key, and neither does the loaded Config: the value is read at mint time.

// ClerkSessionType is the `type:` value that selects this mode.
const ClerkSessionType = "clerk_session"

// SessionSource is the run's live token source, attached by argus.RunAll for the duration of one run (it is
// never read from YAML). Bearer answers for one auth block, keyed as ClerkSessionAuths keys them. Values are
// the strings a scrubber must remove from anything it keeps.
type SessionSource interface {
	Bearer(key string) (string, error)
	Values() []string
}

var (
	envNameRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	jwtTemplateRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// IsClerkSession reports whether this auth block selects the Clerk session mode.
func (a *AuthTarget) IsClerkSession() bool {
	return a != nil && strings.EqualFold(strings.TrimSpace(a.Type), ClerkSessionType)
}

// clerkFieldsSet lists the clerk-only keys this block sets, in declaration order.
func (a *AuthTarget) clerkFieldsSet() []string {
	var out []string
	if a.SecretKeyEnv != "" {
		out = append(out, "secret_key_env")
	}
	if a.UserIDEnv != "" {
		out = append(out, "user_id_env")
	}
	if a.JWTTemplate != "" {
		out = append(out, "jwt_template")
	}
	if a.APIURL != "" {
		out = append(out, "api_url")
	}
	if a.AllowClerkSessionOnProd {
		out = append(out, "allow_clerk_session_on_prod")
	}
	return out
}

// HTTPAuthFor answers which auth block a scenario's plain-HTTP request uses: the selected named target's
// own `auth:` when it has one (it wins), else targets.auth. The key identifies the block for the session
// source — the target's name, or "" for the default. nil when nothing is declared.
func (c *Config) HTTPAuthFor(sel *TargetSelection) (key string, a *AuthTarget) {
	if sel != nil && sel.Kind == KindHTTP && sel.HTTP != nil && sel.HTTP.Auth != nil {
		return sel.Name, sel.HTTP.Auth
	}
	return "", c.Targets.Auth
}

// ClerkSessionAuths returns every clerk_session block, keyed as HTTPAuthFor keys them ("" = targets.auth).
func (c *Config) ClerkSessionAuths() map[string]*AuthTarget {
	out := map[string]*AuthTarget{}
	if c.Targets.Auth.IsClerkSession() {
		out[""] = c.Targets.Auth
	}
	for n, t := range c.Targets.HTTPTargets {
		if t != nil && t.Auth.IsClerkSession() {
			out[n] = t.Auth
		}
	}
	return out
}

// ClerkEnv reads the secret key and the user id out of the environment. It is the ONLY place the secret is
// read, and it is read when a session is built, not when the config loads.
func (a *AuthTarget) ClerkEnv() (secretKey, userID string) {
	return os.Getenv(a.SecretKeyEnv), os.Getenv(a.UserIDEnv)
}

// clerkEnvRefs lists the environment names the clerk blocks reference, so EnvRefs (secrets-scan, the
// onboarding preflight, doctor) and expandEnv's missing-name refusal carry them like any ${VAR}.
func (c *Config) clerkEnvRefs() []EnvRef {
	var out []EnvRef
	add := func(at string, a *AuthTarget) {
		if !a.IsClerkSession() {
			return
		}
		if a.SecretKeyEnv != "" {
			out = append(out, EnvRef{Name: a.SecretKeyEnv, Field: at + ".secret_key_env"})
		}
		if a.UserIDEnv != "" {
			out = append(out, EnvRef{Name: a.UserIDEnv, Field: at + ".user_id_env"})
		}
	}
	add("targets.auth", c.Targets.Auth)
	for _, n := range sortedKeys(c.Targets.HTTPTargets) {
		if t := c.Targets.HTTPTargets[n]; t != nil {
			add("targets.http_targets."+n+".auth", t.Auth)
		}
	}
	return out
}

// validateClerkSession is the load-time check (load time IS validate time): the shape, the references, and
// the production guard. Every problem is named at once. It never echoes a value from the file: a base_url
// can carry credentials, and a mistyped secret_key_env may be the secret itself.
func (c *Config) validateClerkSession() error {
	var errs []string
	check := func(at string, a *AuthTarget, scope map[string]string) {
		if a == nil {
			return
		}
		if !a.IsClerkSession() {
			if set := a.clerkFieldsSet(); len(set) > 0 {
				errs = append(errs, fmt.Sprintf("%s: %s only apply to type: clerk_session — set `type: clerk_session` or remove them", at, strings.Join(set, ", ")))
			}
			return
		}
		var missing []string
		if a.SecretKeyEnv == "" {
			missing = append(missing, "secret_key_env")
		}
		if a.UserIDEnv == "" {
			missing = append(missing, "user_id_env")
		}
		if len(missing) > 0 {
			errs = append(errs, fmt.Sprintf("%s: type clerk_session needs both secret_key_env (the NAME of the environment variable holding the Clerk secret key) and user_id_env (the NAME of the one holding the Clerk user id, user_…) — missing: %s", at, strings.Join(missing, ", ")))
		}
		if a.SecretKeyEnv != "" && !envNameRe.MatchString(a.SecretKeyEnv) {
			errs = append(errs, at+".secret_key_env must be an environment variable NAME (letters, digits, underscore), never the key itself")
		}
		if a.UserIDEnv != "" && !envNameRe.MatchString(a.UserIDEnv) {
			errs = append(errs, at+".user_id_env must be an environment variable NAME (letters, digits, underscore), never the user id itself")
		}
		if a.BearerToken != "" {
			errs = append(errs, at+": bearer_token and type clerk_session are two answers to one header — remove bearer_token")
		}
		if a.JWTTemplate != "" && !jwtTemplateRe.MatchString(a.JWTTemplate) {
			errs = append(errs, at+".jwt_template must be a Clerk JWT template name (letters, digits, `_`, `-`, `.`)")
		}
		if a.APIURL != "" {
			if u, err := url.Parse(a.APIURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
				errs = append(errs, at+".api_url must be an absolute http(s) URL without credentials")
			}
		}
		if a.AllowClerkSessionOnProd {
			return
		}
		// ⛔ THE PRODUCTION GUARD. A minted session is a real login as a real Clerk user. The signal is the one
		// the fault-step and bus guards already use (looksSharedOrProd: a delimited prod/production/prd/shared
		// segment in a URL), applied to the http base_url(s) this block authenticates. A live Clerk key is the
		// second signal and is checked in Load, once the environment is resolved.
		for _, name := range sortedKeys(scope) {
			if looksSharedOrProd(scope[name]) {
				errs = append(errs, fmt.Sprintf("%s: type clerk_session is refused on a production instance — %s looks shared/production; a minted session logs in as a real user. Set `allow_clerk_session_on_prod: true` next to it to opt in explicitly (default false)", at, name))
				break
			}
		}
	}

	httpScope := map[string]string{}
	if c.Targets.HTTP != nil {
		httpScope["targets.http.base_url"] = c.Targets.HTTP.BaseURL
		if c.Targets.HTTP.Auth != nil {
			errs = append(errs, "targets.http.auth: the plain http slot takes its credentials from targets.auth — a second place is two answers; declare `auth:` only on a named entry of targets.http_targets")
		}
	}
	for _, n := range sortedKeys(c.Targets.HTTPTargets) {
		if t := c.Targets.HTTPTargets[n]; t != nil {
			httpScope["targets.http_targets."+n+".base_url"] = t.BaseURL
		}
	}
	// targets.auth is the default for EVERY plain-HTTP scenario, so any production-looking http URL taints it.
	check("targets.auth", c.Targets.Auth, httpScope)
	for _, n := range sortedKeys(c.Targets.HTTPTargets) {
		if t := c.Targets.HTTPTargets[n]; t != nil {
			at := "targets.http_targets." + n
			check(at+".auth", t.Auth, map[string]string{at + ".base_url": t.BaseURL})
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

// validateClerkLiveKey is the production guard's second signal, run by Load once the environment is
// resolved: a secret key that starts `sk_live_` is a production Clerk instance whatever the URLs say. The
// prefix is not secret; nothing else of the key is read into the message.
func (c *Config) validateClerkLiveKey() error {
	var errs []string
	add := func(at string, a *AuthTarget) {
		if a.IsClerkSession() && !a.AllowClerkSessionOnProd && strings.HasPrefix(os.Getenv(a.SecretKeyEnv), "sk_live_") {
			errs = append(errs, fmt.Sprintf("%s: type clerk_session is refused on a production instance — the key named by secret_key_env is a LIVE Clerk key (sk_live_…). Set `allow_clerk_session_on_prod: true` next to it to opt in explicitly (default false)", at))
		}
	}
	add("targets.auth", c.Targets.Auth)
	for _, n := range sortedKeys(c.Targets.HTTPTargets) {
		if t := c.Targets.HTTPTargets[n]; t != nil {
			add("targets.http_targets."+n+".auth", t.Auth)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

// clerkSecretValues are the resolved secret keys, for CredentialValues.
func (c *Config) clerkSecretValues() []string {
	var out []string
	for _, a := range c.ClerkSessionAuths() {
		if v := os.Getenv(a.SecretKeyEnv); v != "" {
			out = append(out, v)
		}
	}
	return out
}
