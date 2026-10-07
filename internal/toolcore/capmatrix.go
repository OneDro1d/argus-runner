package toolcore

import (
	"fmt"
	"os"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// T4.2 — the CAPABILITY MATRIX. validate-config already refuses a scenario whose layer has no
// target, but only for the layers the pack HAPPENS to exercise: a stack with no bus learned it
// could not test Message Flow when its first Message Flow scenario was refused, one scenario at a
// time. The matrix answers the question up front, for all eight layers, from the declared targets
// alone — "here is what this config can test, and here is why not" — in one command.
//
// ⛔ It is INFORMATION ONLY. It never changes `valid`: a layer this SUT does not have (a service
// with no bus has no Message Flow) is a statement about the SUT, not a defect in the file. The
// refusal of a scenario that USES an unavailable layer stays where it was, in Config.Validate.
//
// ⚠ Each rule below is the rule the RUNNER applies, not a restatement of it: the target key comes
// from config.TargetForLayer (the same table Validate refuses with), and a named entry counts
// exactly as SelectTarget lets it cover a scenario. If you change which target a layer needs,
// change it there — this file must not carry a second copy of that table.

// Capability states. "outside-config" is deliberately a third value: Web UI's inputs are not in
// argus-config at all, and a yes/no there would be a guess presented as a fact.
const (
	CapAvailable     = "available"
	CapUnavailable   = "unavailable"
	CapOutsideConfig = "outside-config"
)

// CapabilityRow is one layer's line in the matrix.
type CapabilityRow struct {
	Layer  string `json:"layer"`
	Status string `json:"status"`
	Needs  string `json:"needs"`
	Reason string `json:"reason"`
}

// matrixLayers is the eight layers in the order the product documents them.
var matrixLayers = []string{
	"HTTP Ingestion", "Message Flow", "Database State", "External Delivery",
	"Error Path", "Rate Limiting", "Permissions", "Web UI",
	"AMQP Load", // needs a message_broker target AND the operator's load_allowed_targets entry
	"HTTP Load", // needs an http target AND the operator's load_allowed_targets entry
}

// mcpCoverable are the layers an mcp/chain scenario can carry with targets.mcp alone. Config.Validate
// skips the per-layer target check for every mcp/chain scenario (its trigger is the MCP endpoint), and
// on these four the whole check IS the call and its response — no auth: an unauthenticated or
// throttled call is asserted from the response like any other. The other layers read something besides
// the response (a JDBC connection, a broker, a delivery sink), which MCP does not provide.
//
// ⛔ Found by running the matrix on examples/memstore: a VALID pack of 55 with Permissions scenarios, and
// the first draft of this file called Permissions "unavailable" because targets.auth is absent. A
// matrix that says a stack cannot test what it already tests is worse than no matrix.
// TestCapabilityMatrix_AgreesWithBundledPacks holds that invariant over every bundled example.
var mcpCoverable = map[string]bool{
	"HTTP Ingestion": true, "Error Path": true, "Rate Limiting": true, "Permissions": true,
}

const mcpOnly = " — mcp/chain scenarios only"

// targetOrNamed reports whether the plain targets.<key> slot or any named entry of that kind is
// declared, and describes which, so the reason names what the operator actually wrote.
func targetOrNamed(c *config.Config, key string) (bool, string) {
	var named []string
	switch key {
	case config.KindHTTP, config.KindMCP, config.KindDatabase, config.KindMessageBroker:
		named = c.NamedTargets(key)
	}
	plain := c.TargetPresentExported(key)
	switch {
	case plain && len(named) > 0:
		return true, fmt.Sprintf("targets.%s and %d named %s target(s) (%s) declared", key, len(named), key, strings.Join(named, ", "))
	case plain:
		return true, fmt.Sprintf("targets.%s declared", key)
	case len(named) > 0:
		return true, fmt.Sprintf("%d named %s target(s) declared (%s)", len(named), key, strings.Join(named, ", "))
	}
	return false, ""
}

// CapabilityMatrix derives, for each of the eight layers, whether this config can test it.
// scenariosDir (item 8c, msgbus tester 2026-09-28) is consulted ONLY as a fallback, for chain-step
// coverage — see chainStepCoverage. Passing "" (no scenario set in view) reproduces the pre-8c
// behaviour exactly, since an empty walk finds nothing to credit.
func CapabilityMatrix(c *config.Config, scenariosDir string) []CapabilityRow {
	httpOK, httpWhy := targetOrNamed(c, config.KindHTTP)
	mcpOK, mcpWhy := targetOrNamed(c, config.KindMCP)
	chainCov := chainStepCoverage(scenariosDir)
	rows := make([]CapabilityRow, 0, len(matrixLayers))
	for _, layer := range matrixLayers {
		row := CapabilityRow{Layer: layer}
		switch layer {
		case "Error Path":
			// No target of its own (config.TargetForLayer says so): it fires a request and asserts
			// the status, through the HTTP template or an MCP call — so it needs a trigger.
			row.Needs = "a trigger: targets.http or targets.mcp"
			switch {
			case httpOK:
				row.Status, row.Reason = CapAvailable, httpWhy
			case mcpOK:
				row.Status, row.Reason = CapAvailable, mcpWhy+mcpOnly
			default:
				if reason, ok := chainCoverReason(chainCov, "http", "http", "mcp"); ok {
					row.Status, row.Reason = CapAvailable, reason
				} else {
					row.Status, row.Reason = CapUnavailable, "no targets.http and no targets.mcp — nothing to send a request to"
				}
			}
		case "HTTP Load":
			// a ramp needs a NAMED http target (the plain targets.http slot is never a load
			// target), and runs only once the operator lists it under load_allowed_targets (said below).
			row.Needs = "a targets.http_targets entry listed under load_allowed_targets"
			if named := c.NamedTargets(config.KindHTTP); len(named) > 0 {
				row.Status, row.Reason = CapAvailable, fmt.Sprintf("%d named http target(s) declared (%s)", len(named), strings.Join(named, ", "))
			} else {
				row.Status, row.Reason = CapUnavailable, "no targets.http_targets entry is declared — the plain targets.http slot is never a load target"
			}
		case "Web UI":
			// Not in argus-config at all: the Playwright spec's page comes from the scenario's own
			// app_url, else the runner's APP_URL. Report which, and never claim the layer is ready.
			row.Status = CapOutsideConfig
			row.Needs = "an app_url in the scenario's TRIGGER payload, or APP_URL in the runner's environment"
			if strings.TrimSpace(os.Getenv("APP_URL")) != "" {
				row.Reason = "not declared in argus-config; APP_URL is set in this environment (checked where validate-config ran, which may not be where the run happens)"
			} else {
				row.Reason = "not declared in argus-config; APP_URL is not set here, so each Web UI scenario must carry its own app_url"
			}
		default:
			key, ok := config.TargetForLayer(layer)
			if !ok {
				// A layer added to matrixLayers without a rule here. Say so rather than guess.
				row.Status, row.Needs, row.Reason = CapOutsideConfig, "(no rule)", "this build has no capability rule for the layer"
				break
			}
			row.Needs = "targets." + key
			if mcpCoverable[layer] {
				row.Needs += " (or targets.mcp, for mcp/chain scenarios)"
			}
			present, why := targetOrNamed(c, key)
			switch {
			case present:
				row.Status, row.Reason = CapAvailable, why
			case mcpOK && mcpCoverable[layer]:
				row.Status, row.Reason = CapAvailable, mcpWhy+mcpOnly+"; a plain scenario on this layer still needs targets."+key
			default:
				if reason, ok := chainCoverReason(chainCov, key, chainStepTypesFor(layer)...); ok {
					row.Status, row.Reason = CapAvailable, reason
				} else {
					row.Status, row.Reason = CapUnavailable, "no targets."+key+" declared"+unavailableHint(key)
				}
			}
		}
		if layer == "AMQP Load" && row.Status == CapAvailable && len(c.LoadAllowedTargets) == 0 {
			// S9 is OFF by default: say so, or "available" reads as "will run".
			row.Reason += "; no load_allowed_targets entry is declared, so every AMQP Load run is refused until the operator marks a lab or dedicated test broker"
		}
		if layer == "HTTP Load" && row.Status == CapAvailable && !httpLoadAllowed(c) {
			row.Reason += "; no load_allowed_targets entry names a targets.http_targets entry, so every HTTP Load run is refused until the operator marks a lab or dedicated test deployment"
		}
		rows = append(rows, row)
	}
	return rows
}

// httpLoadAllowed: some load_allowed_targets entry names a declared targets.http_targets entry (HTTP Load's S9).
func httpLoadAllowed(c *config.Config) bool {
	for n := range c.LoadAllowedTargets {
		if c.Targets.HTTPTargets[n] != nil {
			return true
		}
	}
	return false
}

// chainStepTypesFor names the chain-step Types (scenario.ChainStep.Type — "amqp" | "http" | "mcp")
// that self-contain their own connection (url_env / url / target·server_url — see chain.go) and so
// can cover LAYER without any targets.<key> block. nil for a layer no chain step type reaches
// (Database State and External Delivery have no chain step of their own kind — a chain "http" step
// is a REST call, not a JDBC probe or a delivery-sink read).
func chainStepTypesFor(layer string) []string {
	switch layer {
	case "Message Flow":
		return []string{"amqp"}
	case "HTTP Ingestion", "Rate Limiting", "Permissions":
		return []string{"http", "mcp"}
	default:
		return nil
	}
}

// chainStepCoverage scans the discovered scenario set for CHAIN scenarios (scenario.ChainTag — the
// runtime dispatch tag, the SAME one chain_scenario.go keys on, per V29-016) and records, for each
// step TYPE actually present across all of them, the id of one scenario that carries it.
//
// A chain step is SELF-CONTAINED: an amqp step reads its OWN url_env, an http step its OWN url, an
// mcp step its OWN target/server_url (internal/scenario/chain.go) — none of them consult
// argus-config's `targets:` block at all. Before this (item 8c, msgbus tester 2026-09-28) the
// matrix asked ONLY the config, so a chain-only kit that declares no targets read "0 of 8 layers
// testable" even though its AMQP/HTTP/MCP chain steps genuinely reach the SUT. Returns an empty map
// (never nil-panics the caller) for an empty/unreadable dir — the pre-8c behaviour.
func chainStepCoverage(scenariosDir string) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(scenariosDir) == "" {
		return out
	}
	for _, d := range scenario.DiscoverFiles(scenariosDir) {
		s := d.Scenario
		if scenario.NativeEngineTag(s) != scenario.ChainTag {
			continue
		}
		steps, err := scenario.ParseChainSteps(s.Trigger.Payload)
		if err != nil {
			continue
		}
		for _, st := range steps {
			if st.Type == "" {
				continue
			}
			if _, seen := out[st.Type]; !seen {
				out[st.Type] = s.ID
			}
		}
	}
	return out
}

// chainCoverReason credits key's layer with executable chain-step coverage: the first of types
// found in chainCov, or ("", false) when none of them appear in any discovered chain scenario.
func chainCoverReason(chainCov map[string]string, key string, types ...string) (string, bool) {
	for _, t := range types {
		if id, ok := chainCov[t]; ok {
			return fmt.Sprintf("no targets.%s declared, but chain scenario %s runs a %q step with its own connection (self-contained: url_env/url/target) — a chain-only kit needs no global target for this layer", key, id, t), true
		}
	}
	return "", false
}

// unavailableHint adds the one fact an operator needs before declaring the missing target.
func unavailableHint(key string) string {
	switch key {
	case "message_broker":
		return " — AMQP is the only broker engine in this build; a SUT with no bus has no Message Flow layer"
	case "database":
		return " — a read-only JDBC connection to the SUT's own database"
	case "auth":
		return " — the bearer token Permissions scenarios present (bearer is the only auth type)"
	case "external":
		return " — the sink the SUT delivers to (e.g. webhook_base_url)"
	}
	return ""
}

// CapabilitySummary is the one-line reading of the matrix.
func CapabilitySummary(rows []CapabilityRow) string {
	var yes, no, other []string
	for _, r := range rows {
		switch r.Status {
		case CapAvailable:
			yes = append(yes, r.Layer)
		case CapUnavailable:
			no = append(no, r.Layer)
		default:
			other = append(other, r.Layer)
		}
	}
	s := fmt.Sprintf("%d of %d layers testable with this config", len(yes), len(rows))
	if len(no) > 0 {
		s += "; not testable: " + strings.Join(no, ", ")
	}
	if len(other) > 0 {
		s += "; decided outside the config: " + strings.Join(other, ", ")
	}
	return s
}
