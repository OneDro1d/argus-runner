package main

// reveal_cmd.go — AC-7: `argus replay` and `argus anchor verify`. Both are PRE-AUTH (main.go's dispatch):
// a third party running either holds no Argus token — replay deploys and runs against their OWN
// environment, and verify never calls the control plane at all.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/OneDro1d/argus-runner/internal/chainread"
	"github.com/OneDro1d/argus-runner/internal/chainread/ots"
	"github.com/OneDro1d/argus-runner/internal/reveal"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

func loadRevealDocument(path string) (*reveal.Document, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc reveal.Document
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &doc, nil
}

// cmdReplay: `argus replay <scenario_id> --against <artifact_digest> --reveal <reveal.json>
// --config <argus-config.yaml>`. From a clean container, with the pinned runner release, re-runs
// one revealed scenario against an artefact the caller deployed themselves.
func cmdReplay(args []string) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	against := fs.String("against", "", "REQUIRED. The artefact digest you deployed yourself, being replayed against (refused if it does not match the reveal's own declared artifact_digest).")
	revealPath := fs.String("reveal", "", "REQUIRED. Path to the reveal document (author_get_reveal's output, saved to a file).")
	configPath := fs.String("config", "", "REQUIRED. The argus-config.yaml of the SUT you deployed yourself.")
	jmeterMode := fs.String("jmeter", "local", "jmeter execution: docker (compose exec) | local (subprocess)")
	composeFile := fs.String("compose-file", "deploy/compose/docker-compose.yaml", "compose file for the jmeter exec (jmeter=docker)")
	templatesDir := fs.String("templates", "/templates", "jmeter=local: dir holding the <layer>.jmx templates")
	flagArgs, rest := splitPositional(args)
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return emitErr(exitUsage, "replay: %v", err)
	}
	if len(rest) != 1 {
		return emitErr(exitUsage, "usage: argus replay <scenario_id> --against <artifact_digest> --reveal <reveal.json> --config <argus-config.yaml>")
	}
	scenarioID := rest[0]
	if *against == "" || *revealPath == "" || *configPath == "" {
		return emitErr(exitUsage, "replay: --against, --reveal and --config are all required")
	}
	doc, err := loadRevealDocument(*revealPath)
	if err != nil {
		return emitErr(exitErr, "replay: %v", err)
	}
	exec := &runnerScenarioExecutor{
		configPath: *configPath, jmeterLocal: *jmeterMode == "local",
		composeFile: *composeFile, templatesDir: *templatesDir,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, rerr := reveal.Replay(ctx, doc, scenarioID, *against, exec)
	if rerr != nil {
		return emitErr(exitErr, "replay: %v", rerr)
	}
	emit(map[string]any{
		"scenario_id": res.ScenarioID, "refused": res.Refused, "refusal_reason": res.RefusalReason,
		"declared_digest": res.DeclaredDigest, "against_digest": res.AgainstDigest,
		"declared_verdict": res.DeclaredVerdict, "observed_outcome": res.ObservedOutcome, "observed_detail": res.ObservedDetail,
		"match": res.Match, "exit_code": res.ExitCode(),
	})
	return res.ExitCode()
}

// runnerScenarioExecutor runs one sealed scenario through TODAY's runner path — the same
// toolcore.Run a bare `argus run --scenario` already uses — against the caller's OWN --config: their
// own environment, their own evidence, printed in full (allowed; it never touches the reveal's
// original observed values).
type runnerScenarioExecutor struct {
	configPath, composeFile, templatesDir string
	jmeterLocal                           bool
}

func (e *runnerScenarioExecutor) Execute(ctx context.Context, path, body string) (string, string, error) {
	dir, err := os.MkdirTemp("", "argus-replay-*")
	if err != nil {
		return "", "", fmt.Errorf("temp scenarios dir: %w", err)
	}
	defer os.RemoveAll(dir)
	dest := filepath.Join(dir, filepath.Base(path))
	if werr := os.WriteFile(dest, []byte(body), 0o600); werr != nil {
		return "", "", fmt.Errorf("write sealed scenario: %w", werr)
	}
	resultsRoot, err := os.MkdirTemp("", "argus-replay-results-*")
	if err != nil {
		return "", "", fmt.Errorf("temp results dir: %w", err)
	}
	defer os.RemoveAll(resultsRoot)

	env := toolcore.Env{
		Instance: "replay", ConfigPath: e.configPath, ScenariosDir: dir, ResultsRoot: resultsRoot,
		ComposeFile: e.composeFile, JMeterLocal: e.jmeterLocal, TemplatesDir: e.templatesDir,
	}
	out, failed, rerr := toolcore.Run(env, "", "", "", "")
	if rerr != nil {
		return "", "", fmt.Errorf("run: %w", rerr)
	}
	outcome := "passed"
	if failed {
		outcome = "failed"
	}
	detail, _ := json.Marshal(out)
	return outcome, string(detail), nil
}

// cmdAnchor: `argus anchor verify <run>.json --rpc <url> --btc-headers <url-or-file>`. OFFLINE and
// INDEPENDENT — never calls the Argus control plane.
const anchorUsage = "usage: argus anchor verify <reveal.json> --rpc <ethereum-json-rpc-url> --btc-headers <url-or-file>"

// anchorVerifyHelp says how the exit code is decided. It is the same judgement `argus certificate
// verify` makes (certificate.Overall).
const anchorVerifyHelp = `
Each check prints one line: VERIFIED, MISMATCH, UNREACHABLE or PENDING.
  VERIFIED     the check ran and agrees with the reveal document.
  MISMATCH     a real contradiction: a tampered payload, holdout root, inclusion proof or evidence
               bundle hash, a payload that differs from the chain's, a signer the chain refuses, or an
               OTS receipt that the --btc-headers source contradicts (it commits to a different payload,
               its fold is not the block's merkle root at its height, or it does not parse).
  UNREACHABLE  could not be checked, nothing contradicted: an anchor on a chain other than the one
               --rpc serves (the line names the anchor's chain and the chain id --rpc serves), no --rpc,
               an OTS receipt with no --btc-headers, or a --btc-headers source that could not answer.
  PENDING      an OTS receipt not yet attested in a Bitcoin block header.
UNREACHABLE and PENDING never fail the run by themselves.

Exit 0 only when NO line is a MISMATCH and at least one anchor of the required record is VERIFIED.
The required record is this run's verdict anchor (version 3 or later), as in certificate verify:
one verified verdict anchor on any chain is enough. If every verdict anchor is UNREACHABLE or
PENDING the run is NOT verified (exit 1): absence of contradiction is not confirmation. Set and
reveal anchors are checked and a MISMATCH on them fails the run, but they need not verify.
There is no flag that turns UNREACHABLE into a failure (certificate verify has none either).
`

func cmdAnchor(args []string) int {
	if len(args) > 0 && isHelpToken(args[0]) {
		fmt.Println(anchorUsage)
		return exitOK
	}
	if len(args) == 0 || args[0] != "verify" {
		return emitErr(exitUsage, anchorUsage)
	}
	fs := flag.NewFlagSet("anchor verify", flag.ContinueOnError)
	rpcURL := fs.String("rpc", "", "an Ethereum JSON-RPC URL — the independent chain client every Ethereum-style anchor is read back through.")
	btcHeaders := fs.String("btc-headers", "", "a Bitcoin header source for the OTS receipt: an http:// or https:// URL (a public block-explorer API, e.g. https://blockstream.info/api) or a local JSON file {\"<height>\": \"<merkle root>\"} (offline use). Either way the merkle root is in DISPLAY order, as a block explorer or bitcoin-cli getblockheader prints it (e.g. {\"969288\": \"b8100af5...b8d9\"}); it is reversed to the header's internal order before comparing.")
	fs.Usage = func() {
		fmt.Println(anchorUsage)
		fmt.Print(anchorVerifyHelp)
		fs.SetOutput(os.Stdout)
		fs.PrintDefaults()
	}
	flagArgs, rest := splitPositional(args[1:])
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return emitErr(exitUsage, "anchor verify: %v", err)
	}
	if len(rest) != 1 {
		return emitErr(exitUsage, "usage: argus anchor verify <reveal.json> --rpc <ethereum-json-rpc-url> --btc-headers <url-or-file>")
	}
	doc, err := loadRevealDocument(rest[0])
	if err != nil {
		return emitErr(exitErr, "anchor verify: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var svc chainread.ChainService
	if *rpcURL != "" {
		client, derr := ethclient.DialContext(ctx, *rpcURL)
		if derr != nil {
			return emitErr(exitErr, "anchor verify: dial --rpc %s: %v", *rpcURL, derr)
		}
		svc = rpcService{EthereumService: &chainread.EthereumService{Client: client}, client: client}
	}
	var hdr ots.HeaderSource
	switch {
	case strings.HasPrefix(*btcHeaders, "http://") || strings.HasPrefix(*btcHeaders, "https://"):
		hdr = ots.HTTPBlockExplorerHeaders{BaseURL: *btcHeaders}
	case *btcHeaders != "":
		hdr = ots.FileHeaders{Path: *btcHeaders}
	}

	results := reveal.VerifyReveal(ctx, doc, svc, hdr)
	for _, l := range reveal.Render(results) {
		fmt.Println(l)
	}
	if reveal.Overall(results) {
		return exitOK
	}
	return exitErr
}
