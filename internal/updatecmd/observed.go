package updatecmd

import "sort"

// observed.go — V31-001 (VR13-UP): WHAT THE HOST SAW, and the allow-list that keeps it printable.
//
// `update discover` cannot look at the machine: the executor image carries no docker CLI, no kubectl
// and no compose plugin (Dockerfile.onedroid-argus-execution-plane:88). So it writes a bash script, the
// HOST runs it, and the script writes this structure to `$STAGE/observed.json`. Everything the
// planner knows about the machine arrives through here.
//
// ⛔ SA-D18 — `Env` IS AN ALLOW-LIST OF VALUES, NOT A DUMP.
//
// `$STAGE` outlives the update — the NEXT update clears it — and an operator pastes what is in it
// when something goes wrong. Only the keys in EnvValueAllowed may carry their value; every other key
// is recorded as a NAME with an empty value, and an unknown key is denied by default. Denying by
// default is the whole point: a key added to onboarding next quarter is silent until someone decides
// it is safe, rather than leaking until someone notices.

// EnvValueAllowed is the set of env.<id> keys whose VALUES may be recorded. Anything absent from it
// is recorded as a name only.
var EnvValueAllowed = map[string]bool{
	"ARGUS_CP_URL":            true,
	"ARGUS_KIT_DIR_HOST":      true,
	"ARGUS_KUBECONFIG_HOST":   true,
	"ARGUS_KUBE_CONTEXT_HOST": true,
	"ARGUS_MCP_IMAGE":         true,
	"ARGUS_MCP_PORT":          true,
	"ARGUS_ONBOARD_HOST":      true,
	"ARGUS_PRODUCT_DIR_HOST":  true,
	"ARGUS_TEST_DIR_HOST":     true,
	"PROMTAIL_CONFIG":         true,
}

// EnvScrubKeys is every env.<id> key the update must remove from the environment of its `docker compose`
// calls (AC-D55, #389). It is DERIVED from EnvValueAllowed — one source, so a key added to onboarding and
// allow-listed here is scrubbed without a second list to forget. The scripts add, at run time, every
// ARGUS_* key the instance's own env file carries (the token is deliberately not allow-listed).
func EnvScrubKeys() []string {
	keys := make([]string, 0, len(EnvValueAllowed))
	for k := range EnvValueAllowed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ScrubEnv applies the allow-list. A denied key KEEPS ITS NAME — the planner needs to know the key is
// present (the health wait reads ARGUS_RUNNER_TOKEN by name at run time) and must never learn its
// value.
func ScrubEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if EnvValueAllowed[k] {
			out[k] = v
			continue
		}
		out[k] = ""
	}
	return out
}

// Observed is `$STAGE/observed.json` — the machine as the host script found it.
type Observed struct {
	Tier    string       `json:"tier"`
	Compose ComposeState `json:"compose"`
	K8s     K8sState     `json:"k8s"`
	// ExecutorVersion is what the RUNNING executor said `argus version` is, read before anything
	// moves. "" = it could not say (stopped, docker did not answer) — a fact, never a guess. It is the
	// only witness to where a pre-0.3.32 instance CAME FROM, which has no manifest to record it
	// (V32 Release QA finding (d): orderservice-k3d updated 0.3.30 → 0.3.32 and offered no rollback).
	ExecutorVersion string `json:"executor_version,omitempty"`
	// Router is the machine router discover.sh found, read before anything moves (V32 Release QA defect Q1).
	// A router A-7 does not move — every rollback, and a router already newer — is recorded from THIS
	// reading, never from the plan, whose router entry carries the TARGET image. Empty = no router answered.
	Router RouterState       `json:"router"`
	Env    map[string]string `json:"env"`
	// ObsNone is whether this instance was onboarded with `--obs none`: discover reads it
	// off the instance's env file — ARGUS_OBS_LOKI SET AND EMPTY (what `--obs none` onboarding on compose has
	// written since) or ARGUS_OBS_MODE=none — because Env records only NAMES for a key that
	// is not allow-listed, which cannot tell a set-empty value from an unset one. false = not recorded as none.
	ObsNone bool         `json:"obs_none,omitempty"`
	Folders []Folder     `json:"folders"`
	Grafana GrafanaState `json:"grafana"`
	Loki    string       `json:"loki,omitempty"`
	Healthz string       `json:"healthz,omitempty"`
}

// Empty reports whether this observation says nothing about the machine.
//
// ⛔ IT IS A HARD STOP, NOT A DEFAULT (PO: "a missing/unparseable observed.json is a hard stop, never
// an empty reconstruction"). A planner that treated an empty reading as "nothing is installed" would
// plan a first onboard over the top of a live instance.
func (o Observed) Empty() bool {
	return o.Tier == "" && len(o.Compose.Services) == 0 && len(o.K8s.Objects) == 0 && len(o.Env) == 0
}

type ComposeState struct {
	Project  string         `json:"project"`
	Services []ServiceState `json:"services"`
}

// RouterState is `docker inspect argus-router`'s image and the running router's own `argus version`.
// Either is "" when it could not be read — a fact, never a guess.
type RouterState struct {
	Image   string `json:"image"`
	Version string `json:"version"`
}

type ServiceState struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

type K8sState struct {
	Context   string        `json:"context"`
	Namespace string        `json:"namespace"`
	Objects   []ObjectState `json:"objects"`
}

type ObjectState struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

// Folder is one agent folder and whether this machine can write into it. The path comes from the
// ROUTER's own Folder records (internal/router/table.go:75-92) — never a second list, which would
// drift from the one the router actually serves.
type Folder struct {
	Path string `json:"path"`
	// Hat is the router's own record of what this folder wears ("product" | "test"). A-5 installs
	// exactly the skills onboarding installs for that hat (internal/skills.Installed) — a product
	// folder handed scenario-author could write its own expectations (VR-R4).
	Hat      string `json:"hat,omitempty"`
	Writable bool   `json:"writable"`
}

type GrafanaState struct {
	Reachable  bool     `json:"reachable"`
	Dashboard  DocState `json:"dashboard"`
	Datasource DocState `json:"datasource"`
}

// DocState records a Grafana document's presence. An ABSENT document is `{present:false}` and its
// undo DELETEs rather than re-posting an error body — measured: social-aks-v1 has no dashboard at all
// (INT-039), so "absent" is a real state, not a failure to read.
type DocState struct {
	Present bool   `json:"present"`
	UID     string `json:"uid,omitempty"`
}

// Sibling is one OTHER instance in this workspace, as GET /fed/installed/siblings reports it
// (SA-D25). It is the authority for the shared-folder exception.
//
// ⛔ GET /fed/state IS NOT THIS. Measured: it returns exactly one field (running_run_id, fed.go:927)
// and internal/runner/state_test.go:84 pins it single-field.
type Sibling struct {
	InstanceID string `json:"instance_id"`
	// InstalledVersion is "" when the control plane does not know — which HOLDS THE FOLDER BACK.
	// Not knowing is not evidence of being current.
	InstalledVersion string   `json:"installed_version"`
	SkillsHostPaths  []string `json:"skills_host_paths"`
}
