package main

// usageCommands is what `argus --help` prints, and it is EVERY command this binary dispatches.
// TestUsageListsEveryDispatchedCommand reads main.go's dispatch sites and fails when the two drift,
// in either direction — a command added and not listed, or listed and not dispatched.
//
// ⛔ THIS LIST USED TO BE A CURATED SUBSET, AND THE OMISSION WAS NOT COSMETIC. docs/DEPLOY-ARGUS.md
// sends a first-time operator to `preflight` and five `cloud-*` commands; not one of them appeared
// here. Measured 2026-09-23 in a fresh-agent run of that door: the agent's installed binary predated
// `preflight`, and because `--help` did not list the command either, "this binary is older than the
// guide" was indistinguishable from "I typed it wrong" — it retried four times, three of them with
// invented tokens, before the truth appeared. **A help list that omits the documented path is worse
// than no help list, because it is read as a denial that the path exists.**
//
// ⚠️ It is a FLAT list on purpose. Grouping commands into "the ones you need" and "the internal ones"
// is the judgment that produced the subset above; whoever curates it next will be as sure as the last
// curator was. The door (docs/DEPLOY-ARGUS.md) is where the six-command path is named — not here.
var usageCommands = []string{
	"builder",
	"calm",
	"capabilities",
	"certificate",
	"cloud-check-instance",
	"cloud-clock-check",
	"cloud-create-workspace",
	"cloud-deregister",
	"cloud-enroll",
	"cloud-executor-status",
	"cloud-list-workspaces",
	"cloud-login",
	"cloud-logout",
	"cloud-mint-token",
	"cloud-onboarding-state",
	"cloud-seed-scenarios",
	"cloud-switch-workspace",
	"cloud-teardown",
	"cloud-verify",
	"delete-scenario",
	"doctor",
	"get-dashboard-url",
	"get-report",
	"get-sagas",
	"init",
	"keygen",
	"list-scenarios",
	"mark-deployment",
	"mcp-call",
	"mcpjson",
	"onboard-guard",
	"package-check",
	"preflight",
	"preflight-auth",
	"propose-from-repo",
	"propose-scenario",
	"read-scenario",
	"render-k8s",
	"render-obs",
	"render-obs-shared", // T3.3: the environment's ONE shared Loki (namespace argus-obs)
	"router",
	"run",
	"run-direct",
	"runner-id",
	"runner-state",
	"secrets",
	"secrets-scan",
	"select-image",
	"serve",
	"skills",
	"tail-logs",
	"tester",
	"up",
	"update",
	"upgrade",
	"validate-config",
	"validate-scenario",
	"version",
	"write-scenario",
}
