// Package chainread is the READ-ONLY half of what `argus certificate verify` needs from a chain: read
// one block's data-carrying transactions through an independent JSON-RPC client, recover each sender
// from its signature, and read an anchored payload back under a signer allow-list.
//
// It writes nothing, signs nothing and holds no key. There is deliberately no function in this package
// that sends a transaction, and the ChainService interface cannot express one.
//
// The anchor wire format it reads (a JSON "datablock" in a transaction's data field, mode "hash",
// with the payload spliced in byte-exact) is the one OneDroid Argus writes. It is read here exactly as
// written; this package never produces one.
package chainread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// EthClient is the read-only subset of ethclient.Client that ReadBlock needs. *ethclient.Client
// satisfies it. Nothing in it can send a transaction.
type EthClient interface {
	ethereum.ChainIDReader
	ethereum.BlockNumberReader
	ethereum.ChainReader
}

// BlockTx is one transaction read back from a block. From is never trusted from the payload: it is
// recovered from the transaction signature.
type BlockTx struct {
	Index  uint   // position in the block
	TxHash string // 0x-prefixed
	From   string // EIP-55, recovered via types.Sender(types.LatestSignerForChainID(chainID), tx)
	Nonce  uint64
	Data   []byte
}

// ChainService is the read-only view of a chain.
type ChainService interface {
	ReadBlock(blockNumber int64) ([]BlockTx, error)
	BlockNumber() (uint64, error)
}

// EthereumService is the ChainService over an EthClient.
type EthereumService struct {
	Client EthClient
}

// ReadBlock returns every transaction in the block that carries data, with its recovered sender.
func (s *EthereumService) ReadBlock(blockNumber int64) ([]BlockTx, error) {
	return ReadBlock(s.Client, blockNumber)
}

// BlockNumber is the chain's newest block number.
func (s *EthereumService) BlockNumber() (uint64, error) {
	return s.Client.BlockNumber(context.Background())
}

// A block read that answers not-found is retried for a bounded while. A public RPC behind a load
// balancer can answer from a replica that has not yet seen the block a receipt just named (measured
// on a public L2 testnet: the receipt named block N, the next read of N was not found, the block was
// served a moment later). Only not-found is retried; any other error returns at once.
var (
	readBlockNotFoundRetries = 20
	readBlockNotFoundDelay   = 500 * time.Millisecond
)

// ReadBlock retrieves every non-empty-data transaction in a specific block, with its sender recovered
// from the transaction signature (never from the payload).
func ReadBlock(client EthClient, blockNumber int64) ([]BlockTx, error) {
	ctx := context.Background()
	var block *types.Block
	var err error
	for attempt := 0; ; attempt++ {
		block, err = client.BlockByNumber(ctx, big.NewInt(blockNumber))
		if err == nil {
			break
		}
		if errors.Is(err, types.ErrTxTypeNotSupported) {
			// An OP-stack chain opens every block with a system transaction of a type this client does
			// not decode, so the block as a whole never decodes. Every ordinary transaction in it still
			// does, one by one; the system transaction carries no signature and no anchor and is skipped.
			return readBlockTxByTx(ctx, client, blockNumber)
		}
		if !errors.Is(err, ethereum.NotFound) || attempt >= readBlockNotFoundRetries {
			return nil, err
		}
		time.Sleep(readBlockNotFoundDelay)
	}

	chainID, err := client.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get chain ID: %v", err)
	}
	signer := types.LatestSignerForChainID(chainID)

	var results []BlockTx
	for i, tx := range block.Transactions() {
		bt, keep, err := blockTxOf(signer, uint(i), tx)
		if err != nil {
			return nil, err
		}
		if keep {
			results = append(results, bt)
		}
	}
	return results, nil
}

// readBlockTxByTx reads a block whose full decode fails on an unsupported transaction type: the header
// gives the hash, the count bounds the walk, and each transaction is fetched by index. A transaction
// of an unsupported type is skipped; any other failure is returned.
func readBlockTxByTx(ctx context.Context, client EthClient, blockNumber int64) ([]BlockTx, error) {
	header, err := client.HeaderByNumber(ctx, big.NewInt(blockNumber))
	if err != nil {
		return nil, err
	}
	hash := header.Hash()
	count, err := client.TransactionCount(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("failed to count transactions in block %d: %v", blockNumber, err)
	}
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get chain ID: %v", err)
	}
	signer := types.LatestSignerForChainID(chainID)

	var results []BlockTx
	for i := uint(0); i < count; i++ {
		tx, err := client.TransactionInBlock(ctx, hash, i)
		if errors.Is(err, types.ErrTxTypeNotSupported) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read transaction %d of block %d: %v", i, blockNumber, err)
		}
		bt, keep, err := blockTxOf(signer, i, tx)
		if err != nil {
			return nil, err
		}
		if keep {
			results = append(results, bt)
		}
	}
	return results, nil
}

// blockTxOf turns one transaction into a BlockTx with its sender recovered from the signature; keep is
// false for a transaction that carries no data.
func blockTxOf(signer types.Signer, index uint, tx *types.Transaction) (BlockTx, bool, error) {
	data := tx.Data()
	if len(data) == 0 {
		return BlockTx{}, false, nil
	}
	from, err := types.Sender(signer, tx)
	if err != nil {
		return BlockTx{}, false, fmt.Errorf("failed to recover sender for tx %s: %v", tx.Hash().Hex(), err)
	}
	return BlockTx{
		Index:  index,
		TxHash: tx.Hash().Hex(),
		From:   from.Hex(),
		Nonce:  tx.Nonce(),
		Data:   data,
	}, true, nil
}

// ErrEmptyAllowlist is returned when WithAllowedSigners is given zero addresses: fail closed rather
// than silently accepting every signer.
var ErrEmptyAllowlist = errors.New("chainread: empty signer allowlist")

// ErrSignerNotAllowed is returned when no chunk at the scanned position was signed by one of the
// configured allowed signers.
var ErrSignerNotAllowed = errors.New("chainread: signer not allowed")

// ErrModeMismatch is returned when ReadAnchor meets a chunk that is not a hash-mode anchor.
var ErrModeMismatch = errors.New("chainread: mode mismatch")

// ErrMismatch is returned when a ref's committed digest does not match the payload it is being
// verified against.
var ErrMismatch = errors.New("chainread: ref does not commit to this payload")

// Ref locates one anchor on its chain. Receipt carries the OTS proof bytes; it is empty for an
// Ethereum ref.
type Ref struct {
	Chain   string
	Block   uint64
	TxHash  string
	Receipt []byte
}

// Proof is the result of verifying a Ref: the signer that produced it (empty for a provider with no
// signer concept, e.g. OTS) and whether confirmation is still pending (OTS, before the calendar has
// a Bitcoin attestation).
type Proof struct {
	Ref     Ref
	Signer  string
	Pending bool
}

type config struct {
	allowedSigners map[string]struct{}
	signersSet     bool
}

// Option configures a read.
type Option func(*config)

// WithAllowedSigners restricts reads to chunks signed by one of addrs, compared case-insensitively
// against the recovered EIP-55 address. Zero addresses fails closed with ErrEmptyAllowlist.
func WithAllowedSigners(addrs ...string) Option {
	return func(c *config) {
		c.signersSet = true
		c.allowedSigners = make(map[string]struct{}, len(addrs))
		for _, a := range addrs {
			c.allowedSigners[strings.ToLower(a)] = struct{}{}
		}
	}
}

func newConfig(opts ...Option) (*config, error) {
	c := &config{}
	for _, opt := range opts {
		opt(c)
	}
	if c.signersSet {
		if len(c.allowedSigners) == 0 {
			return nil, ErrEmptyAllowlist
		}
		for a := range c.allowedSigners {
			if !common.IsHexAddress(a) {
				return nil, fmt.Errorf("chainread: malformed allowed-signer address %q", a)
			}
		}
	}
	return c, nil
}

func (c *config) signerAllowed(addr string) bool {
	if c == nil || !c.signersSet {
		return true
	}
	_, ok := c.allowedSigners[strings.ToLower(addr)]
	return ok
}

// Datablock is the JSON stored in a transaction's data field.
type Datablock struct {
	CorrelationID         string          `json:"correlationId"`
	Start                 int             `json:"start,omitempty"`
	SchemaVersion         int             `json:"schemaVersion,omitempty"`
	PreviousChunkBlockID  string          `json:"previousChunkBlockId"`
	ChunkData             string          `json:"chunkData"`
	KeyID                 string          `json:"keyId,omitempty"`
	DEKWrapped            string          `json:"dekWrapped,omitempty"`
	Mode                  string          `json:"mode,omitempty"` // "" = full, "hash" = hash-only
	Payload               json.RawMessage `json:"payload,omitempty"`
	PreviousCorrelationID string          `json:"previousCorrelationId,omitempty"`
	ChunkIndex            int             `json:"chunkIndex,omitempty"`
	ChunkCount            int             `json:"chunkCount,omitempty"`
}

// AnchorRef identifies one anchor version: the correlation ID it was written under and the block its
// single transaction landed in.
type AnchorRef struct {
	CorrelationID string
	Block         uint64
}

// AnchorRecord is one anchor version read back from the chain.
type AnchorRecord struct {
	CorrelationID string
	Block         uint64
	TxHash        string
	From          string     // recovered sender
	Payload       []byte     // byte-exact as written
	Previous      *AnchorRef // nil for a first version
}

// findAnchorChunk looks in blockNum for the tx whose Datablock carries correlationID, regardless of
// mode, and returns it with its tx hash and recovered signer. With an allow-list, only a chunk from an
// allowed signer is considered, failing closed with ErrSignerNotAllowed if the id appears in the block
// only from disallowed signers. found is false only when no chunk at all in the block carries
// correlationID.
func findAnchorChunk(service ChainService, correlationID string, blockNum uint64, cfg *config) (db Datablock, txHash string, from string, found bool, err error) {
	txs, err := service.ReadBlock(int64(blockNum))
	if err != nil {
		return Datablock{}, "", "", false, fmt.Errorf("chainread: read block %d: %v", blockNum, err)
	}
	sawDisallowed := false
	for _, tx := range txs {
		var candidate Datablock
		if json.Unmarshal(tx.Data, &candidate) != nil {
			continue
		}
		if candidate.CorrelationID != correlationID {
			continue
		}
		if !cfg.signerAllowed(tx.From) {
			sawDisallowed = true
			continue
		}
		return candidate, tx.TxHash, tx.From, true, nil
	}
	if sawDisallowed {
		return Datablock{}, "", "", false, ErrSignerNotAllowed
	}
	return Datablock{}, "", "", false, nil
}

// ReadAnchor reads the anchor at ref, returning the byte-exact payload, the recovered signer and the
// previous version's ref (nil for a first version). A ref naming a non-hash chunk fails with
// ErrModeMismatch.
func ReadAnchor(service ChainService, ref AnchorRef, opts ...Option) (AnchorRecord, error) {
	cfg, err := newConfig(opts...)
	if err != nil {
		return AnchorRecord{}, err
	}

	db, txHash, from, found, err := findAnchorChunk(service, ref.CorrelationID, ref.Block, cfg)
	if err != nil {
		return AnchorRecord{}, err
	}
	if !found {
		return AnchorRecord{}, fmt.Errorf("chainread: no chunk for correlation ID %s in block %d", ref.CorrelationID, ref.Block)
	}
	if db.Mode != "hash" {
		return AnchorRecord{}, ErrModeMismatch
	}

	var previous *AnchorRef
	if db.PreviousCorrelationID != "" {
		prevBlock, err := strconv.ParseUint(db.PreviousChunkBlockID, 10, 64)
		if err != nil {
			return AnchorRecord{}, fmt.Errorf("chainread: invalid previous block id %q: %v", db.PreviousChunkBlockID, err)
		}
		previous = &AnchorRef{CorrelationID: db.PreviousCorrelationID, Block: prevBlock}
	}

	return AnchorRecord{
		CorrelationID: ref.CorrelationID,
		Block:         ref.Block,
		TxHash:        txHash,
		From:          from,
		Payload:       append([]byte(nil), db.Payload...),
		Previous:      previous,
	}, nil
}
