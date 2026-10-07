package main

import "flag"

// ownsArgv reports whether a subcommand parses its OWN argument vector and must therefore receive it
// whole (VR5-D1 / V19-010).
//
// The rule is mechanical, not a curated list: these are exactly the commands `dispatch` routes with the
// raw vector and which never read `commonFlags`. A command that consumes none of the common flags can
// only be broken by having one taken from it — there is no case where the partition helps it.
//
// V19-010 is what happens without this. `router register --control-plane <url> --token <tok>` names two
// flags the COMMON flagset also declares (main.go:131, :111), so the partition routed both away and
// `cmdRouterRegister` saw neither. Its own declarations (router_wire.go:376-377) stayed empty and the
// guard at :382 printed usage — on 7/7 onboards, all three tiers, both flag orders, while 0.3.20
// reached the POST.
//
// ⚠ `render-k8s` is deliberately ABSENT: it is dispatched with `cf` (main.go:304) and does read the
// common flags, so it must stay partitioned. Membership here is "does it read commonFlags", not
// "is it pre-auth".
func ownsArgv(cmd string) bool {
	switch cmd {
	case "init", "router", "mcpjson", "update", "upgrade":
		return true
	// T6.3: `certificate get` declares its OWN --instance-id/--control-plane/--token (all three also
	// common-flagset names) and `certificate verify` its own --chains/--rpc; neither reads commonFlags.
	case "certificate":
		return true
	// `up` declares its OWN --config/--tier/--control-plane/--kube-context/--kubeconfig —
	// every one of them ALSO a common-flagset name (main.go's commonFlags.bind) — and reads none of
	// commonFlags itself, exactly the shape V19-010 is about.
	case "up":
		return true
	// T2.3: `preflight` declares its OWN --tier/--kube-context/--control-plane — all three also
	// common-flagset names — and reads none of commonFlags. Without this the partition takes them
	// away and every probe runs against empty strings, which would report a confidently wrong
	// "blocked" on a machine that is fine. Exactly V19-010, one command later.
	case "preflight":
		return true
	// P1 #9: `secrets` (set|list) declares its OWN --kube-context — also a common-flagset name —
	// and reads none of commonFlags. Exactly V19-010 again: without this, `--kube-context` would be
	// partitioned away and every `secrets set`/`secrets list` would act against whatever context
	// kubectl defaults to instead of the one the caller named.
	case "secrets":
		return true
	// `doctor` declares its OWN --config/--scenarios/--control-plane — all three also common-flagset
	// names — and reads none of commonFlags. The same shape as preflight, one command later.
	case "doctor":
		return true
	// `tester init` / `builder init` declare their OWN --control-plane (also a common-flagset name) and
	// read none of commonFlags: the same shape as doctor.
	case "tester", "builder":
		return true
	// `calm import` declares its OWN --out (also a common-flagset name) and reads none
	// of commonFlags. Without this the partition takes --out away and the importer writes nowhere.
	case "calm":
		return true
	}
	return false
}

// subcommandArgv decides what the common parser gets and what the subcommand gets.
//
// It exists as a named function so the promise can be asserted at the DISPATCH boundary. V18's guard
// asserted that splitCommonFlags returned `rest` intact — the partition function — which is a different
// claim and is why a broken sibling shipped green: `router serve` survives only because `--state` is
// not a common flag, so testing that command proved nothing about `router register`.
func subcommandArgv(cmd string, fs *flag.FlagSet, args []string) (common, sub []string) {
	if ownsArgv(cmd) {
		// Nothing is taken. The subcommand owns every token, including ones the common flagset
		// happens to declare.
		return nil, args
	}
	return splitCommonFlags(fs, args)
}
