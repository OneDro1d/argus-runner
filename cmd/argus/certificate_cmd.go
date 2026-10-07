package main

// certificate_cmd.go — T6.3: `argus certificate get` and `argus certificate verify`.
//
//   - `get` carries its own author session token (--token or ARGUS_CP_AUTHOR_TOKEN, formerly
//     ARGUS_CP_TOKEN), exactly like `runner-id` (AC-17) — PRE-AUTH, above the
//     ARGUS_RUNNER_TOKEN/ARGUS_EXECUTOR_SECRET hat gate, and it reaches the control plane over the
//     exact MCP surface an agent would (author_get_certificate).
//   - `verify` is a THIRD PARTY'S OWN verb: OFFLINE and INDEPENDENT — it never calls the control plane
//     at all, needs no --control-plane and no token, and reads every anchor back from its own chain
//     (internal/certificate.Verify, built on internal/chainread, a read-only chain reader).
//
// Both own their argv (ownargv.go): `get` declares --instance-id/--control-plane/--token, and
// `verify` declares --rpc/--btc-headers/--chains — several of which the common flagset also declares
// (V19-010).

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/OneDro1d/argus-runner/internal/certificate"
	"github.com/OneDro1d/argus-runner/internal/chainread"
	"github.com/OneDro1d/argus-runner/internal/chainread/ots"
	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/mcp"
)

const certificateUsage = "usage: argus certificate get|verify ...\n" +
	"  get:    argus certificate get --instance-id <id> --run-id <id> --control-plane <url> [--token <tok>] [--out <file>]\n" +
	"  verify: argus certificate verify <certificate.json> [--chains <chains.json>] [--rpc <url>] [--btc-headers <url-or-file>] [--json]"

// cmdCertificate dispatches `certificate get | verify`.
func cmdCertificate(args []string) int {
	if len(args) == 0 {
		return emitErr(exitUsage, "usage: argus certificate get|verify ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "get":
		return cmdCertificateGet(rest)
	case "verify":
		return cmdCertificateVerify(rest)
	case "-h", "-help", "--help", "help":
		fmt.Println(certificateUsage)
		return exitOK
	default:
		return emitErr(exitUsage, "unknown certificate subcommand %q (want get | verify)", sub)
	}
}

// cmdCertificateGet: `argus certificate get --instance-id <id> --run-id <id> --control-plane <url>
// [--token <tok>] [--out <file>]`.
func cmdCertificateGet(args []string) int {
	fs := flag.NewFlagSet("certificate get", flag.ContinueOnError)
	instanceID := fs.String("instance-id", "", "REQUIRED. The instance the run belongs to.")
	runID := fs.String("run-id", "", "REQUIRED. The final run's own run id.")
	cpURL := fs.String("control-plane", os.Getenv("ARGUS_CP_URL"), "the control-plane base URL")
	// ⛔ NO DEFAULT HERE (#44, matching up.go). flag's own usage text (printed by --help and by every
	// flag error) renders a StringVar's/String's default verbatim as `(default "...")` — a default of
	// os.Getenv("ARGUS_CP_AUTHOR_TOKEN"/"ARGUS_CP_TOKEN") put the token in that text. The env pair is
	// read AFTER Parse instead, below, only when the flag itself was left empty.
	token := fs.String("token", "", "an author session token — the same credential cloud-seed-scenarios/runner-id use (else ARGUS_CP_AUTHOR_TOKEN, formerly ARGUS_CP_TOKEN)")
	out := fs.String("out", "", "optional. Write the certificate JSON to this file instead of only stdout.")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	rawFlagToken := *token
	if *token == "" {
		*token = envname.Lookup(envname.CPAuthorToken, envname.CPAuthorTokenDeprecated)
	}
	if *cpURL == "" || *token == "" {
		return emitErr(exitUsage, "certificate get: --control-plane (or ARGUS_CP_URL) and --token (or ARGUS_CP_AUTHOR_TOKEN, formerly ARGUS_CP_TOKEN, an author session) are required")
	}
	if *instanceID == "" || *runID == "" {
		return emitErr(exitUsage, "certificate get: --instance-id and --run-id are required")
	}
	// F-CRED-1: always an EXPLICIT credential here too (the refusal above catches the "neither given"
	// case) — never the session file.
	credentialBanner(*cpURL, rawFlagToken)

	cl := &mcp.Client{ServerURL: strings.TrimRight(*cpURL, "/") + "/mcp", Transport: mcp.Streamable, Token: *token, Timeout: 30 * time.Second}
	res := cl.Call(mcp.CallInput{Tool: "author_get_certificate", Args: map[string]any{"instance_id": *instanceID, "run_id": *runID}})
	if res.Unreachable {
		return emitErr(exitErr, "certificate get: control plane unreachable")
	}
	if res.TransportErr != "" {
		return emitErr(exitErr, "certificate get: %s", res.TransportErr)
	}
	if res.JSONRPCError != nil {
		return emitErr(exitErr, "certificate get: %s", res.JSONRPCError.Message)
	}
	if len(res.Content) == 0 {
		return emitErr(exitErr, "certificate get: empty response")
	}
	text := res.Content[0].Text
	var payload any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return emitErr(exitErr, "certificate get: malformed response: %v", err)
	}
	if *out != "" {
		pretty, _ := json.MarshalIndent(payload, "", "  ")
		if werr := os.WriteFile(*out, append(pretty, '\n'), 0o600); werr != nil {
			return emitErr(exitErr, "certificate get: write %s: %v", *out, werr)
		}
	}
	emit(payload)
	if res.IsError {
		return exitErr
	}
	return exitOK
}

// cmdCertificateVerify: `argus certificate verify <certificate.json> [--chains <chains.json>] [--rpc
// <ethereum-json-rpc-url>] [--btc-headers <url-or-file>] [--json]`. OFFLINE and INDEPENDENT — never
// calls the Argus control plane; no --control-plane flag exists on this command at all.
func cmdCertificateVerify(args []string) int {
	fs := flag.NewFlagSet("certificate verify", flag.ContinueOnError)
	chainsPath := fs.String("chains", "", "optional. A chains.json carrying each chain's OWN allowedSigners — cross-checks each anchor's declared signer against a THIRD PARTY'S copy, not only against itself.")
	rpcURL := fs.String("rpc", "", "an Ethereum JSON-RPC URL — the independent chain client every Ethereum-style anchor is read back through.")
	btcHeaders := fs.String("btc-headers", "", "a Bitcoin header source for an OTS receipt: an http:// or https:// URL (a public block-explorer API, e.g. https://blockstream.info/api) or a local JSON file {\"<height>\": \"<merkle root>\"} (offline use). Either way the merkle root is in DISPLAY order, as a block explorer or bitcoin-cli getblockheader prints it (e.g. {\"969288\": \"b8100af5...b8d9\"}); it is reversed to the header's internal order before comparing.")
	asJSON := fs.Bool("json", false, "print the per-anchor results (and overall verdict) as JSON instead of the human summary.")
	flagArgs, rest := splitPositional(args)
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return emitErr(exitUsage, "certificate verify: %v", err)
	}
	if len(rest) != 1 {
		return emitErr(exitUsage, "usage: argus certificate verify <certificate.json> [--chains <chains.json>] [--rpc <url>] [--btc-headers <url-or-file>]")
	}

	b, err := os.ReadFile(rest[0])
	if err != nil {
		return emitErr(exitErr, "certificate verify: read %s: %v", rest[0], err)
	}
	var cert certificate.Certificate
	if err := json.Unmarshal(b, &cert); err != nil {
		return emitErr(exitErr, "certificate verify: parse %s: %v", rest[0], err)
	}
	if cert.Format != certificate.Format && cert.Format != certificate.FormatV2 && cert.Format != certificate.FormatV1 {
		return emitErr(exitErr, "certificate verify: format %q, want %q, %q or %q", cert.Format, certificate.Format, certificate.FormatV2, certificate.FormatV1)
	}

	var allow certificate.AllowList
	if *chainsPath != "" {
		allow, err = certificate.LoadAllowList(*chainsPath)
		if err != nil {
			return emitErr(exitErr, "certificate verify: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var svc chainread.ChainService
	if *rpcURL != "" {
		client, derr := ethclient.DialContext(ctx, *rpcURL)
		if derr != nil {
			return emitErr(exitErr, "certificate verify: dial --rpc %s: %v", *rpcURL, derr)
		}
		svc = rpcService{EthereumService: &chainread.EthereumService{Client: client}, client: client}
	}
	// ots.HTTPBlockExplorerHeaders/FileHeaders are the SAME two ots.HeaderSource implementations
	// --btc-headers builds.
	var hdr interface {
		MerkleRootAt(ctx context.Context, height uint64) ([32]byte, error)
	}
	switch {
	case strings.HasPrefix(*btcHeaders, "http://") || strings.HasPrefix(*btcHeaders, "https://"):
		hdr = ots.HTTPBlockExplorerHeaders{BaseURL: *btcHeaders}
	case *btcHeaders != "":
		hdr = ots.FileHeaders{Path: *btcHeaders}
	}

	results := certificate.Verify(ctx, &cert, svc, hdr, allow)
	overall := certificate.Overall(&cert, results)
	unanchored := certificate.Unanchored(&cert)

	if *asJSON {
		type line struct {
			Name, Status, Detail string
		}
		out := struct {
			Verified    bool     `json:"verified"`
			Results     []line   `json:"results"`
			NotAnchored []string `json:"not_anchored"`
		}{Verified: overall, NotAnchored: unanchored}
		for _, r := range results {
			out.Results = append(out.Results, line{Name: r.Name, Status: string(r.Status), Detail: r.Detail})
		}
		emit(out)
	} else {
		for _, l := range certificate.Render(&cert, results) {
			fmt.Println(l)
		}
	}
	if overall {
		return exitOK
	}
	return exitErr
}

// rpcService is the --rpc chain client plus the one question verify asks it only to explain a failed
// read: which chain does this endpoint serve (certificate.ChainIDer).
type rpcService struct {
	*chainread.EthereumService
	client *ethclient.Client
}

func (s rpcService) ChainID(ctx context.Context) (*big.Int, error) { return s.client.ChainID(ctx) }
