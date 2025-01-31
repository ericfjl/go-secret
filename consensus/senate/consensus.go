package senate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/SecretBlockChain/go-secret/accounts"
	"github.com/SecretBlockChain/go-secret/common"
	"github.com/SecretBlockChain/go-secret/consensus"
	"github.com/SecretBlockChain/go-secret/core/state"
	"github.com/SecretBlockChain/go-secret/core/types"
	"github.com/SecretBlockChain/go-secret/crypto"
	"github.com/SecretBlockChain/go-secret/log"
	"github.com/SecretBlockChain/go-secret/params"
	"github.com/SecretBlockChain/go-secret/rlp"
	"github.com/SecretBlockChain/go-secret/trie"
	lru "github.com/hashicorp/golang-lru"
	"golang.org/x/crypto/sha3"
)

// ecrecover extracts the Ethereum account address from a signed header.
func ecrecover(header *types.Header, sigcache *lru.ARCCache) (common.Address, error) {
	// If the signature's already cached, return that
	hash := header.Hash()
	if address, known := sigcache.Get(hash); known {
		return address.(common.Address), nil
	}
	// Retrieve the signature from the header extra-data
	if len(header.Extra) < extraSeal {
		return common.Address{}, errMissingSignature
	}
	signature := header.Extra[len(header.Extra)-extraSeal:]

	// Recover the public key and the Ethereum address
	pubkey, err := crypto.Ecrecover(SealHash(header).Bytes(), signature)
	if err != nil {
		return common.Address{}, err
	}
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])

	sigcache.Add(hash, signer)
	return signer, nil
}

// Author retrieves the Ethereum address of the account that minted the given
// block, which may be different from the header's coinbase if a consensus
// engine is based on signatures.
func (senate *Senate) Author(header *types.Header) (common.Address, error) {
	return ecrecover(header, senate.signatures)
}

// VerifyHeader checks whether a header conforms to the consensus rules of a
// given engine. Verifying the seal may be done optionally here, or explicitly
// via the VerifySeal method.
func (senate *Senate) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header, seal bool) error {
	return senate.verifyHeader(chain, header, nil)
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
// concurrently. The method returns a quit channel to abort the operations and
// a results channel to retrieve the async verifications (the order is that of
// the input slice).
func (senate *Senate) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))
	numbers := make([]int64, 0)
	for _, header := range headers {
		numbers = append(numbers, header.Number.Int64())
	}

	go func() {
		for i, header := range headers {
			err := senate.verifyHeader(chain, header, headers[:i])
			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

// verifyHeader checks whether a header conforms to the consensus rules.The
// caller may optionally pass in a batch of parents (ascending order) to avoid
// looking those up from the database. This is useful for concurrently verifying
// a batch of new headers.
func (senate *Senate) verifyHeader(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	if header.Number == nil {
		return errUnknownBlock
	}
	log.Trace("[DPOS] VerifyHeader", "number", header.Number.Int64())

	// Don't waste time checking blocks from the future
	if header.Time > uint64(time.Now().Unix()) {
		return consensus.ErrFutureBlock
	}

	// Check that the extra-data contains both the vanity and signature
	if len(header.Extra) < extraVanity {
		return errMissingVanity
	}
	if len(header.Extra) < extraVanity+extraSeal {
		return errMissingSignature
	}

	// Ensure that the mix digest is zero as we don't have fork protection currently
	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}

	// Ensure that the block doesn't contain any uncles which are meaningless in DPOS
	if header.UncleHash != uncleHash {
		return errInvalidUncleHash
	}

	// All basic checks passed, verify cascading fields
	err := senate.verifyCascadingFields(chain, header, parents)
	if err != nil {
		log.Warn("[DPOS] Failed to verify cascading fields", "number", header.Number.Int64(), "reason", err)
	}
	return err
}

// verifyCascadingFields verifies all the header fields that are not standalone,
// rather depend on a batch of previous headers. The caller may optionally pass
// in a batch of parents (ascending order) to avoid looking those up from the
// database. This is useful for concurrently verifying a batch of new headers.
func (senate *Senate) verifyCascadingFields(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	// The genesis block is the always valid dead-end
	number := header.Number.Uint64()
	if number == 0 {
		return nil
	}

	// Ensure that the block's timestamp isn't too close to it's parent
	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}
	if parent == nil || parent.Number.Uint64() != number-1 || parent.Hash() != header.ParentHash {
		return consensus.ErrUnknownAncestor
	}
	if parent.Time > header.Time {
		return ErrInvalidTimestamp
	}

	// Load snapshot of parent block
	var snap *Snapshot
	config := *senate.config
	headerExtra, err := decodeHeaderExtra(header)
	if err != nil {
		return err
	}

	parentHeaderExtra := headerExtra
	if parent.Number.Int64() == 0 {
		snap, err = newSnapshot(senate.db)
		if err != nil {
			return err
		}
	} else {
		parentHeaderExtra, err = decodeHeaderExtra(parent)
		if err != nil {
			return err
		}

		config, err = senate.chainConfigByHash(parentHeaderExtra.Root.ConfigHash)
		if err != nil {
			return err
		}

		snap, err = loadSnapshot(senate.db, parentHeaderExtra.Root)
		if err != nil {
			return err
		}
	}

	// Ensure that the epoch timestamp and parent block are continuous
	if headerExtra.Epoch != parentHeaderExtra.Epoch || headerExtra.EpochTime != parentHeaderExtra.EpochTime {
		if headerExtra.Epoch != parentHeaderExtra.Epoch+1 || headerExtra.EpochTime != header.Time {
			return ErrInvalidTimestamp
		}
	}

	// Retrieve the snapshot needed to verify this header and cache it
	err = snap.apply(header, headerExtra)
	if err != nil {
		return err
	}

	root, err := snap.Root()
	if err != nil {
		return err
	}
	if root != headerExtra.Root {
		log.Info(fmt.Sprintf("root \n %s \n headerExtra.Root %s ",Root2String(root),Root2String(headerExtra.Root)))
		return errors.New("invalid trie root")
	}

	// Verify the seal and return
	err = senate.verifySeal(config, header, parent)
	if err != nil {
		return err
	}

	// All basic checks passed, save snapshot to disk
	if err = snap.Commit(root); err != nil {
		return errors.New("failed to write snapshot")
	}
	return nil
}

func Root2String(root Root) string {
	return fmt.Sprintf("\nCandidateHash=%s \nConfigHash=%s \nDeclareHash=%s \nDelegateHash= %s \nCandidateHash=%s \nEpochHash=%s \nMintCntHash=%s \nProposalHash=%s \nVoteHash=%s",root.CandidateHash.String(),root.ConfigHash.String(),root.DeclareHash.String(),root.DelegateHash.String(),root.CandidateHash.String(),root.EpochHash.String(),root.MintCntHash.String(),root.ProposalHash.String(),root.VoteHash.String())
}
// VerifyUncles verifies that the given block's uncles conform to the consensus
// rules of a given engine.
func (senate *Senate) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errUnclesNotAllowed
	}
	return nil
}

// VerifySeal checks whether the crypto seal on a header is valid according to
// the consensus rules of the given engine.
func (senate *Senate) VerifySeal(chain consensus.ChainHeaderReader, header *types.Header) error {
	log.Trace("[DPOS] VerifySeal", "number", header.Number.Int64())

	config := *senate.config
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if header.Number.Int64() > 1 {
		var err error
		config, err = senate.chainConfig(parent)
		if err != nil {
			return err
		}
	}
	return senate.verifySeal(config, header, parent)
}

// verifySeal checks whether the signature contained in the header satisfies the
// consensus protocol requirements. The method accepts an optional list of parent
// headers that aren't yet part of the local blockchain to generate the snapshots
// from.
func (senate *Senate) verifySeal(config params.SenateConfig, header, parent *types.Header) error {
	// Verifying the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}

	// Resolve the authorization key and check against signers
	signer, err := ecrecover(header, senate.signatures)
	if err != nil {
		return err
	}
	if !senate.inTurn(config, parent, header.Time, signer) {
		return errUnauthorized
	}
	return nil
}

// Prepare initializes the consensus fields of a block header according to the
// rules of a particular engine. The changes are executed inline.
func (senate *Senate) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	log.Trace("[DPOS] Prepare", "number", header.Number.Int64())

	// Mix digest is reserved for now, set to empty
	header.MixDigest = common.Hash{}

	// Set the correct difficulty
	header.Difficulty = senate.CalcDifficulty(chain, 0, nil)

	// Initialize HeaderExtra, update epoch for block
	var headerExtra HeaderExtra
	var config params.SenateConfig
	number := header.Number.Uint64()
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	if number == 1 {
		config = *senate.config
		now := time.Now().Unix()
		header.Time = parent.Time + config.Period
		if int64(header.Time) < now {
			header.Time = uint64(now)
		}

		headerExtra.Epoch = 1
		headerExtra.EpochTime = header.Time
	} else {
		parentHeaderExtra, err := decodeHeaderExtra(parent)
		if err != nil {
			return err
		}

		config, err = senate.chainConfigByHash(parentHeaderExtra.Root.ConfigHash)
		if err != nil {
			return err
		}

		now := time.Now().Unix()
		header.Time = parent.Time + config.Period
		if int64(header.Time) < now {
			header.Time = uint64(now)
		}

		headerExtra.Root = parentHeaderExtra.Root
		headerExtra.Epoch = parentHeaderExtra.Epoch
		headerExtra.EpochTime = parentHeaderExtra.EpochTime
		duration := header.Time - parentHeaderExtra.EpochTime
		if duration/config.Epoch >= 1 && duration%config.Epoch > 0 {
			headerExtra.Epoch = parentHeaderExtra.Epoch + 1
			headerExtra.EpochTime = header.Time
		}
	}

	// Ensure the extra data has HeaderExtra struct
	data, err := headerExtra.Encode()
	if err != nil {
		return err
	}

	if len(header.Extra) < extraVanity {
		header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, extraVanity-len(header.Extra))...)
	}
	header.Extra = header.Extra[:extraVanity]
	header.Extra = append(header.Extra, data...)
	header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, extraSeal)...)
	return nil
}

// Finalize runs any post-transaction state modifications (e.g. block rewards)
// but does not assemble the block.
//
// Note: The block header and state database might be updated to reflect any
// consensus rules that happen at finalization (e.g. block rewards).
func (senate *Senate) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction,
	uncles []*types.Header) {

	log.Trace("[DPOS] Finalize", "number", header.Number.Int64())

	// Load snapshot of parent block
	var snap *Snapshot
	number := header.Number.Uint64()
	headerExtra, err := decodeHeaderExtra(header)
	if err != nil {
		panic(err)
	}

	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if number <= 1 {
		snap, err = newSnapshot(senate.db)
	} else {
		parentHeaderExtra, err := decodeHeaderExtra(parent)
		if err != nil {
			panic(err)
		}
		snap, err = loadSnapshot(senate.db, parentHeaderExtra.Root)
	}
	if err != nil {
		panic(err)
	}

	// Get the chain configuration
	config, err := senate.chainConfig(parent)
	if err != nil {
		panic(err)
	}

	// Accumulate any block rewards and commit the final state root
	senate.accumulateRewards(config, state, header)

	// Replay custom transactions and check HeaderExtra of block header
	temp := HeaderExtra{
		Root:      headerExtra.Root,
		Epoch:     headerExtra.Epoch,
		EpochTime: headerExtra.EpochTime,
	}
	senate.processTransactions(config, state, header, snap, &temp, txs, nil)
	if err = senate.tryElect(config, state, header, snap, &temp); err != nil || !temp.Equal(headerExtra) {
		panic(err)
	}

	// Accumulate any block and uncle rewards and commit the final state root
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	header.UncleHash = types.CalcUncleHash(nil)
}

// FinalizeAndAssemble runs any post-transaction state modifications (e.g. block
// rewards) and assembles the final block.
//
// Note: The block header and state database might be updated to reflect any
// consensus rules that happen at finalization (e.g. block rewards).
func (senate *Senate) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction,
	uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {

	log.Trace("[DPOS] FinalizeAndAssemble", "number", header.Number.Int64())

	// Load snapshot of last block
	oldHeaderExtra, err := decodeHeaderExtra(header)
	if err != nil {
		return nil, err
	}
	headerExtra := HeaderExtra{
		Epoch:     oldHeaderExtra.Epoch,
		EpochTime: oldHeaderExtra.EpochTime,
	}
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if header.Number.Int64() > 1 {
		parentHeaderExtra, err := decodeHeaderExtra(parent)
		if err != nil {
			return nil, err
		}
		headerExtra.Root = parentHeaderExtra.Root
	}
	snap, err := loadSnapshot(senate.db, headerExtra.Root)
	if err != nil {
		return nil, err
	}

	// Get the chain configuration
	config, err := senate.chainConfig(parent)
	if err != nil {
		return nil, err
	}

	// Accumulate any block rewards and commit the final state root
	senate.accumulateRewards(config, state, header)

	// Save validator of block to snapshot
	if err = snap.MintBlock(headerExtra.Epoch, header.Number.Uint64(), header.Coinbase); err != nil {
		return nil, err
	}

	// Parse and process custom transactions
	senate.processTransactions(config, state, header, snap, &headerExtra, txs, receipts)

	// Elect validators in first block for epoch
	if err = senate.tryElect(config, state, header, snap, &headerExtra); err != nil {
		log.Warn("[DPOS] Failed to try elect", "reason", err)
		return nil, err
	}

	// Save snapshot of current block to db
	headerExtra.Root, err = snap.Root()
	if err != nil {
		return nil, err
	}
	if err = snap.Commit(headerExtra.Root); err != nil {
		return nil, err
	}

	// Write HeaderExtra of current block into header.Extra
	data, err := headerExtra.Encode()
	if err != nil {
		return nil, err
	}
	header.Extra = header.Extra[:extraVanity]
	header.Extra = append(header.Extra, data...)
	header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, extraSeal)...)

	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	header.UncleHash = types.CalcUncleHash(nil)
	return types.NewBlock(header, txs, nil, receipts, new(trie.Trie)), nil
}

// Seal generates a new sealing request for the given input block and pushes
// the result into the given channel.
//
// Note, the method returns immediately and will send the result async. More
// than one result may also be returned depending on the consensus algorithm.
func (senate *Senate) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	log.Trace("[DPOS] Seal", "number", block.Number().Int64())

	// Sealing the genesis block is not supported
	header := block.Header()
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}

	// Check that the extra-data contains both the vanity and signature
	if len(header.Extra) < extraVanity {
		return errMissingVanity
	}

	if len(header.Extra) < extraVanity+extraSeal {
		return errMissingSignature
	}

	// Get the chain configuration
	parent := chain.GetHeader(header.ParentHash, number-1)
	config, err := senate.chainConfig(parent)
	if err != nil {
		return err
	}

	// Bail out if we're unauthorized to sign a block
	if !senate.inTurn(config, parent, header.Time, header.Coinbase) {
		return errUnauthorized
	}

	// Don't hold the signer fields for the entire sealing procedure
	senate.lock.RLock()
	signer, signFn := senate.signer, senate.signFn
	senate.lock.RUnlock()

	// Sign all the things!
	sigHash, err := signFn(accounts.Account{Address: signer}, accounts.MimetypeClique, SenateRLP(header))
	if err != nil {
		return err
	}
	copy(header.Extra[len(header.Extra)-extraSeal:], sigHash)

	// Wait until sealing is terminated or delay timeout.
	delay := time.Unix(int64(header.Time), 0).Sub(time.Now())
	log.Info("[DPOS] Waiting for slot to sign and propagate", "delay", common.PrettyDuration(delay))
	go func() {
		select {
		case <-stop:
			return
		case <-time.After(delay):
		}

		select {
		case results <- block.WithSeal(header):
		default:
			log.Warn("[DPOS] Sealing result is not read by miner", "sealhash", SealHash(header))
		}
	}()
	return nil
}

// SealHash returns the hash of a block prior to it being sealed.
func (senate *Senate) SealHash(header *types.Header) (hash common.Hash) {
	return SealHash(header)
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have.
func (senate *Senate) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	return big.NewInt(defaultDifficulty)
}

// SealHash returns the hash of a block prior to it being sealed.
func SealHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()
	encodeSigHeader(hasher, header)
	hasher.Sum(hash[:0])
	return hash
}

// SenateRLP returns the rlp bytes which needs to be signed for the delegated-proof-of-stake
// sealing. The RLP to sign consists of the entire header apart from the 65 byte signature
// contained at the end of the extra data.
//
// Note, the method requires the extra data to be at least 65 bytes, otherwise it
// panics. This is done to avoid accidentally using both forms (signature present
// or not), which could be abused to produce different hashes for the same header.
func SenateRLP(header *types.Header) []byte {
	b := new(bytes.Buffer)
	encodeSigHeader(b, header)
	return b.Bytes()
}

func encodeSigHeader(w io.Writer, header *types.Header) {
	err := rlp.Encode(w, []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra[:len(header.Extra)-crypto.SignatureLength], // Yes, this will panic if extra is too short
		header.MixDigest,
		header.Nonce,
	})
	if err != nil {
		panic("can't encode: " + err.Error())
	}
}
