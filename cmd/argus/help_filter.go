package main

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

// help_filter.go — msgbus tester follow-up item 1 (2026-09-28): #289 made `argus <cmd> --help`
// answer for all 58 commands (F-CLI-HELP-1, help_every_command_test.go), but every command that
// reads the COMMON flagset (main.go's commonFlags.bind) shares ONE flag.FlagSet, so `fs.Usage()`
// printed EVERY flag any command declares — `argus render-k8s --help` listed `-addr` (serve),
// `-args` (mcp-call), `-scope` (cloud-login), and so on. This file filters that same Usage output
// down to the flags the invoked command actually reads.
//
// It does NOT touch the self-parsing commands (router, certificate, hub, anchor, runner-id,
// mcpjson, update, up, preflight, secrets, skills, keygen, cloud-onboarding-state, init) — those
// own their argv (ownargv.go/unknownflags.go's readsSubArgs) and never reach `fs.Usage()` from
// dispatch at all; each already prints its own scoped usage.

// commandFlags names, for every command dispatched through the common flagset, the flag names
// that command actually reads. Built by tracing:
//   - direct `cf.<field>` reads inside the command's own cmdXxx handler;
//   - `e.<field>` reads inside toolcore functions the handler calls with a bare toolcore.Env
//     (GetReport/GetSagas/TailLogs/GetDashboardURL/Capabilities/ValidateConfig/ListScenarios/
//     ReadScenario/DeleteScenario — internal/toolcore/toolcore.go), since those fields are exactly
//     the commonFlags values env() threaded through, nothing more;
//   - configRequired(cmd, cf) (main.go) — the flags gated BEFORE the handler ever runs;
//   - whether the command is dispatched above or below the "D2 token authorization" gate
//     (main.go, `presented := cf.token`) — everything below it reads --token (and the paired,
//     now-vestigial --role, kept alongside it since that is the only place --role ever meant
//     anything) whether or not the handler names cf.token itself.
//
// A command with an empty (or absent) entry reads none of the common flags (version, runner-state).
var commandFlags = map[string][]string{
	"capabilities":           {"config", "token", "role"},
	"cloud-check-instance":   {"control-plane", "instance-id", "token", "role"},
	"cloud-clock-check":      {"control-plane"},
	"cloud-create-workspace": {"control-plane", "name", "token", "role"},
	"cloud-deregister":       {"control-plane", "instance-id"},
	"cloud-enroll":           {"control-plane", "instance-id", "revoke", "token", "token-dir"},
	"cloud-executor-status":  {"control-plane", "instance-id", "token", "role"},
	"cloud-list-workspaces":  {"control-plane", "summary", "sut", "token", "role"},
	"cloud-login":            {"control-plane", "scope", "token-out"},
	"cloud-logout":           {"control-plane"},
	"cloud-mint-token":       {"control-plane", "name", "router-state", "token", "token-out", "role"},
	"cloud-seed-scenarios":   {"control-plane", "instance-id", "scenarios", "token", "role"},
	"cloud-switch-workspace": {"control-plane", "token", "token-out", "workspace", "role"},
	"cloud-teardown":         {"control-plane", "instance-id", "token", "cloud-only", "role"},
	"cloud-verify":           {"control-plane", "token", "role"},
	"delete-scenario":        {"scenario", "scenarios", "token", "role"},
	"get-dashboard-url":      {"config", "grafana", "results", "instance-id", "correlation-id", "token", "role"},
	"get-report":             {"results", "instance-id", "run-id", "token", "role"},
	"get-sagas":              {"config", "loki", "loki-tenant", "results", "instance-id", "correlation-id", "logs-window", "window", "token", "role"},
	"list-scenarios":         {"scenarios", "token", "role"},
	"mark-deployment":        {"instance-id", "commit"},
	"mcp-call": {
		"args", "config", "expect-code", "expect-plane", "mcp-token", "request-id",
		"server-url", "tool", "transport", "token", "role",
	},
	"onboard-guard":     {"config", "scenarios", "token", "role"},
	"package-check":     {"config", "json", "root"},
	"preflight-auth":    {"config", "token", "role"},
	"propose-from-repo": {"config", "json", "out", "repo"},
	"propose-scenario":  {"from", "layer", "target", "token", "role"},
	"read-scenario":     {"scenario", "scenarios", "token", "role"},
	"render-k8s": {
		"config", "scenarios", "results", "compose-file", "grafana", "loki", "loki-tenant",
		"pushgateway", "jmeter", "templates", "instance-id", "collect-sut-logs", "control-plane",
		"emit-sut-access-role", "external-alias", "image", "kube-context", "obs", "obs-shared-url",
		"obs-storage-class", "out", "podmonitor", "replicas", "results-access-mode", "secrets-env-file",
		"storage-class", "sut-namespace", "tier",
	},
	"render-obs":        {"config", "compose-file", "instance-id", "token", "role"},
	"render-obs-shared": {"tier", "out", "obs-storage-class"},
	"run": {
		"config", "instance-id", "results", "layer", "run-id", "scenario", "tag",
		"use-local-scenarios", "token", "role",
	},
	"run-direct": {
		"config", "results", "compose-file", "jmeter", "templates", "grafana", "loki",
		"loki-tenant", "pushgateway", "instance-id", "layer", "scenario", "tag",
	},
	"runner-state": {},
	"secrets-scan": {"config", "token", "role"},
	"select-image": {"config"},
	"serve": {
		"addr", "mode", "instance-id", "config", "scenarios", "results", "compose-file",
		"grafana", "loki", "loki-tenant", "pushgateway", "jmeter", "templates", "token", "role",
	},
	"tail-logs": {"config", "loki", "loki-tenant", "results", "instance-id", "correlation-id", "logs-window", "window", "token", "role"},
	"validate-config": {
		"config", "scenarios", "results", "compose-file", "grafana", "loki", "loki-tenant",
		"pushgateway", "jmeter", "templates", "instance-id",
	},
	"validate-scenario": {"file", "config", "token", "role"},
	"version":           {},
	"write-scenario":    {"file", "path", "token", "role"},
}

// commandNotes is one line printed under "Usage of <cmd>:" for a command whose flag list alone would
// mislead. validate-scenario lists --token and --role because old invocations pass them, but it is
// dispatched before the token gate and reads neither.
var commandNotes = map[string]string{
	"validate-scenario": "  Checks one scenario file locally; no token needed (no ARGUS_RUNNER_TOKEN/ARGUS_EXECUTOR_SECRET either). A --token that is passed is accepted and ignored.",
}

// filteredUsage returns an fs.Usage replacement that prints only cmd's own flags (commandFlags),
// in the same per-flag layout flag.PrintDefaults uses, so an operator reading `argus <cmd> --help`
// sees `Usage of <cmd>:` followed by ONLY the flags that command reads — never a sibling
// command's, and never every flag the binary declares anywhere.
func filteredUsage(fs *flag.FlagSet, cmd string) func() {
	return func() {
		out := fs.Output()
		fmt.Fprintf(out, "Usage of %s:\n", cmd)
		if note := commandNotes[cmd]; note != "" {
			fmt.Fprintf(out, "%s\n", note)
		}
		own := commandFlags[cmd]
		if len(own) == 0 {
			return
		}
		owned := make(map[string]bool, len(own))
		for _, n := range own {
			owned[n] = true
		}
		var names []string
		fs.VisitAll(func(f *flag.Flag) {
			if owned[f.Name] {
				names = append(names, f.Name)
			}
		})
		sort.Strings(names)
		for _, n := range names {
			printFlagUsage(out, fs.Lookup(n))
		}
	}
}

// printFlagUsage prints one flag's usage line in flag.PrintDefaults' own layout (name/type,
// wrapped usage text, and a trailing "(default ...)" when the default is not the type's zero
// value) so a filtered command's --help reads exactly like the unfiltered stdlib output always
// did — only the SET of flags printed has changed, not their formatting.
func printFlagUsage(out io.Writer, f *flag.Flag) {
	if f == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  -%s", f.Name)
	name, usage := flag.UnquoteUsage(f)
	if len(name) > 0 {
		b.WriteString(" ")
		b.WriteString(name)
	}
	if b.Len() <= 4 {
		b.WriteString("\t")
	} else {
		b.WriteString("\n    \t")
	}
	b.WriteString(strings.ReplaceAll(usage, "\n", "\n    \t"))
	if dv := f.DefValue; dv != "" && dv != "false" && dv != "0" {
		// flag.PrintDefaults quotes only a string flag's default; UnquoteUsage names that type "string".
		if name == "string" {
			fmt.Fprintf(&b, " (default %q)", dv)
		} else {
			fmt.Fprintf(&b, " (default %v)", dv)
		}
	}
	fmt.Fprintln(out, b.String())
}
