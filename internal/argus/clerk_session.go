package argus

import (
	"errors"
	"fmt"
	"log"
	"sort"

	"github.com/OneDro1d/argus-runner/internal/clerksession"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// ── auth.type: clerk_session, the run half ─────────────────────────────────────────
//
// config.go declares the block; internal/clerksession mints; THIS file ties one run to it:
//
//   - RunAll attaches a sessionSet to a COPY of the config for the length of the run (attachSessions) and
//     revokes every session it created when the run returns — normally, on error, or by panic (a defer).
//   - DeriveProps asks the set for the bearer exactly where it used to read the static one. Every call goes
//     through clerksession.Manager.Token, which re-checks the token's life, so the refresh check runs before
//     EACH scenario's request and never from a value cached here.
//   - a mint failure is a *SessionError: the run loop reports the scenario `error` (never `failed`) with the
//     error's own text, which names the stage and the HTTP status and nothing secret.
//
// ⛔ The session is minted lazily, on the first scenario that needs it: a run of MCP-only or AMQP-only checks
// against a config that also declares clerk_session never calls Clerk.
// ⛔ One session per distinct identity (secret key, user, template, api_url): two targets that name the same
// identity share it.

// SessionError is a Clerk mint/refresh failure. It is the run's, not the SUT's: the scenario is `error`.
type SessionError struct{ Err error }

func (e *SessionError) Error() string { return e.Err.Error() }
func (e *SessionError) Unwrap() error { return e.Err }

// sessionOptions are extra clerksession options for every manager attachSessions builds. Empty in production;
// a test sets a controllable clock.
var sessionOptions []clerksession.Option

// sessionSet is the run's config.SessionSource.
type sessionSet struct {
	byKey map[string]*clerksession.Manager // auth block key -> manager
	all   []*clerksession.Manager          // distinct managers, for revoke and Values
}

func (s *sessionSet) Bearer(key string) (string, error) {
	m := s.byKey[key]
	if m == nil {
		return "", &SessionError{errors.New("clerk_session: no session is configured for this target")}
	}
	tok, err := m.Token()
	if err != nil {
		return "", &SessionError{err}
	}
	return tok, nil
}

func (s *sessionSet) Values() []string {
	var out []string
	for _, m := range s.all {
		out = append(out, m.Values()...)
	}
	return out
}

// revoke ends every session. A failure is logged, without a secret, and changes nothing else.
func (s *sessionSet) revoke() {
	for _, m := range s.all {
		if err := m.Revoke(); err != nil {
			log.Printf("clerk_session: could not revoke the session (the run's verdict is unchanged): %v", err)
		}
	}
}

// attachSessions returns the config the run should use and the function that ends its sessions. With no
// clerk_session block declared, or a source already attached (a test's), it returns c itself and a no-op.
func attachSessions(c *config.Config) (*config.Config, func()) {
	if c.Sessions != nil {
		return c, func() {}
	}
	auths := c.ClerkSessionAuths()
	if len(auths) == 0 {
		return c, func() {}
	}
	set := &sessionSet{byKey: map[string]*clerksession.Manager{}}
	byIdentity := map[clerksession.Params]*clerksession.Manager{}
	keys := make([]string, 0, len(auths))
	for k := range auths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := auths[k]
		// Names only (the environment variables), never their values.
		log.Printf("clerk_session: auth %q will mint a session on first use (secret key from %s, user from %s)", k, a.SecretKeyEnv, a.UserIDEnv)
		secret, user := a.ClerkEnv()
		p := clerksession.Params{APIURL: a.APIURL, SecretKey: secret, UserID: user, Template: a.JWTTemplate}
		m := byIdentity[p]
		if m == nil {
			m = clerksession.New(p, sessionOptions...)
			byIdentity[p] = m
			set.all = append(set.all, m)
		}
		set.byKey[k] = m
	}
	c2 := *c // the caller's config is not ours to change
	c2.Sessions = set
	return &c2, set.revoke
}

// clerkHeaderKey says whether this scenario's `Authorization` comes from a Clerk session: a clerk_session block
// applies AND the scenario sets no Authorization of its own (its own line, even an empty one, wins and needs
// no session).
func clerkHeaderKey(c *config.Config, sel *config.TargetSelection, s *scenario.Scenario) (string, bool) {
	key, a := c.HTTPAuthFor(sel)
	if !a.IsClerkSession() {
		return "", false
	}
	if _, own := s.Trigger.Headers["Authorization"]; own {
		return "", false
	}
	return key, true
}

// sessionRefusal turns a mint/refresh failure into the scenario's row: `error`, never `failed` — it says
// nothing about the SUT, and nothing was fired. The text is the error's own and names the stage.
func sessionRefusal(s *scenario.Scenario, corr string, err error) (report.ScenarioResult, bool) {
	var se *SessionError
	if !errors.As(err, &se) {
		return report.ScenarioResult{}, false
	}
	return refusedBeforeFiring(report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: report.StatusError,
		Failure: &report.Failure{Observed: se.Error() + " — preflight"}}), true
}

// sessionBearer fetches the bearer for a scenario whose header comes from a session.
func sessionBearer(c *config.Config, key string) (string, error) {
	if c.Sessions == nil {
		return "", &SessionError{fmt.Errorf("clerk_session: no session source is attached to this run")}
	}
	return c.Sessions.Bearer(key)
}
