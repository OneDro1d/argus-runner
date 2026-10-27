package argus

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/pgread"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// dialPostgres opens a db step's connection. A package var ONLY so tests can put a fake querier
// behind the whole chain path; production never reassigns it.
var dialPostgres chain.DBDialer = pgread.Dial

// dbPrepared is one db step, resolved and checked, ready to be built into a chain.Step.
type dbPrepared struct {
	spec chain.DBSpec
	want []mcp.BodyAssert
	poll *chain.Poll
}

// dbPreflight is the executor's door for db steps (door B), run BEFORE any step of the chain is built, so a refusal
// leaves nothing fired and nothing half-assembled. It asks the SAME functions the authoring validator asks
// (scenario.DBStepProblems with full=false: shape, the read-only subset of the SQL rules, `${…}` in the query and
// the params; scenario.DBStepClaims: only `body …` claims), then resolves the target and its URL.
//
// ⛔ The statement text, the params and the stray-field check are read from the step AS AUTHORED (pristine, the
// unsubstituted payload), never from specs: parseChainSpec has already filled every environment value into specs,
// and an environment value must never be spliced into SQL text. The run's own id is the only thing placed in the
// query here, by fillCorrelationID.
//
// The returned map is keyed by the step's index. step and why name the first refusal ("" = fine).
func dbPreflight(c *config.Config, s *scenario.Scenario, specs, pristine []chainStepSpec, claims map[string][]string, corr string) (map[int]dbPrepared, string, string) {
	if !scenario.HasDBStep(specs) {
		return nil, "", ""
	}
	raws := scenarioRawSteps(s)
	out := map[int]dbPrepared{}
	for i, st := range specs {
		if st.Type != "db" {
			continue
		}
		if i >= len(pristine) {
			return nil, st.Name, "the step could not be read as authored"
		}
		p := pristine[i]
		var raw map[string]json.RawMessage
		if i < len(raws) {
			raw = raws[i]
		}
		if probs := scenario.DBStepProblems(p, p.Query, raw, false); len(probs) > 0 {
			return nil, st.Name, strings.Join(probs, "; ")
		}
		asserts, cerr := scenario.DBStepClaims(claims[st.Name])
		if cerr != nil {
			return nil, st.Name, cerr.Error()
		}
		if len(asserts) == 0 {
			return nil, st.Name, "a db step needs at least one `body …` claim on its rows (e.g. `body has row_count >= 1`)"
		}
		conn, why := dbTarget(c, p)
		if why != "" {
			return nil, st.Name, why
		}
		params, _ := scenario.DBStepParams(p.Params)
		for j := range params {
			params[j] = fillCorrelationID(params[j], corr)
		}
		timeout, _ := scenario.DBStepTimeout(p.Timeout)
		maxRows, _ := scenario.DBStepMaxRows(p.MaxRows)
		var poll *chain.Poll
		if p.Poll != nil {
			pt, terr := time.ParseDuration(p.Poll.Timeout)
			if terr != nil {
				return nil, st.Name, "poll.timeout is not a valid duration: " + terr.Error()
			}
			iv, _ := scenario.DBStepPollInterval(p.Poll)
			poll = &chain.Poll{Timeout: pt, Interval: iv}
		}
		out[i] = dbPrepared{
			spec: chain.DBSpec{Query: fillCorrelationID(p.Query, corr), Params: params, Conn: conn,
				Timeout: timeout, MaxRows: maxRows, Scrub: credentialScrub(c)},
			want: resolveBodyAssertsCorrelationID(asserts, corr),
			poll: poll,
		}
	}
	return out, "", ""
}

// dbTarget resolves the step's database target and reads its URL. It names what is missing and never prints the
// URL, the username or the password.
func dbTarget(c *config.Config, st chainStepSpec) (chain.DBConn, string) {
	var t *config.DBTarget
	label := "targets.database"
	if name := strings.TrimSpace(st.Target); name != "" {
		nt, err := c.DBTargetNamed(name)
		if err != nil {
			return chain.DBConn{}, err.Error()
		}
		t, label = nt, "targets.database_targets."+name
	} else {
		t = c.Targets.Database
	}
	if t == nil {
		return chain.DBConn{}, "a db step needs a database target: argus-config.yaml declares no targets.database " +
			"(jdbc_url, username, password) - declare it, or name one under targets.database_targets with `target`"
	}
	conn, err := chain.ParsePostgresJDBC(t.JDBCURL, t.Username, t.Password)
	if err != nil {
		return chain.DBConn{}, fmt.Sprintf("%s: %s", label, err.Error())
	}
	return conn, ""
}
