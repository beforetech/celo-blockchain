// Copyright 2021 The celo Authors
// This file is part of the celo library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package miner

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/celo-org/celo-blockchain/common"
	"github.com/celo-org/celo-blockchain/consensus"
	"github.com/celo-org/celo-blockchain/consensus/misc"
	"github.com/celo-org/celo-blockchain/contracts/blockchain_parameters"
	"github.com/celo-org/celo-blockchain/contracts/currency"
	"github.com/celo-org/celo-blockchain/contracts/random"
	"github.com/celo-org/celo-blockchain/core"
	"github.com/celo-org/celo-blockchain/core/rawdb"
	"github.com/celo-org/celo-blockchain/core/state"
	"github.com/celo-org/celo-blockchain/core/types"
	"github.com/celo-org/celo-blockchain/log"
	"github.com/celo-org/celo-blockchain/params"
)

// blockState is the collection of modified state that is used to assemble a block
type blockState struct {
	signer types.Signer

	state        *state.StateDB    // apply state changes here
	tcount       int               // tx count in cycle
	gasPool      *core.GasPool     // available gas used to pack transactions
	bytesBlock   *core.BytesBlock  // available bytes used to pack transactions
	multiGasPool core.MultiGasPool // available gas to pay for with currency
	gasLimit     uint64
	sysCtx       *core.SysContractCallCtx

	header         *types.Header
	txs            []*types.Transaction
	receipts       []*types.Receipt
	randomness     *types.Randomness // The types.Randomness of the last block by mined by this worker.
	txFeeRecipient common.Address
}

// prepareBlock intializes a new blockState that is ready to have transaction included to.
// Note that if blockState is not nil, blockState.close() needs to be called to shut down the state prefetcher.
func prepareBlock(w *worker) (*blockState, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	timestamp := time.Now().Unix()
	parent := w.chain.CurrentBlock()

	if parent.Time() >= uint64(timestamp) {
		timestamp = int64(parent.Time() + 1)
	}

	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		Extra:      w.extra,
		Time:       uint64(timestamp),
	}

	txFeeRecipient := w.txFeeRecipient
	if !w.chainConfig.IsDonut(header.Number) && w.txFeeRecipient != w.validator {
		txFeeRecipient = w.validator
		log.Warn("TxFeeRecipient and Validator flags set before split etherbase fork is active. Defaulting to the given validator address for the coinbase.")
	}

	// Only set the coinbase if our consensus engine is running (avoid spurious block rewards)
	if w.isRunning() {
		if txFeeRecipient == (common.Address{}) {
			return nil, errors.New("Refusing to mine without etherbase")
		}
		header.Coinbase = txFeeRecipient
	}
	// Note: The parent seal will not be set when not validating
	if err := w.engine.Prepare(w.chain, header); err != nil {
		log.Error("Failed to prepare header for mining", "err", err)
		return nil, fmt.Errorf("Failed to prepare header for mining: %w", err)
	}

	// Initialize the block state itself
	state, err := w.chain.StateAt(parent.Root())
	if err != nil {
		return nil, fmt.Errorf("Failed to get the parent state: %w:", err)
	}
	state.StartPrefetcher("miner")

	vmRunner := w.chain.NewEVMRunner(header, state)
	b := &blockState{
		signer:         types.LatestSigner(w.chainConfig),
		state:          state,
		tcount:         0,
		gasLimit:       blockchain_parameters.GetBlockGasLimitOrDefault(vmRunner),
		header:         header,
		txFeeRecipient: txFeeRecipient,
	}
	b.gasPool = new(core.GasPool).AddGas(b.gasLimit)

	if w.chainConfig.IsGingerbread(header.Number) {
		header.GasLimit = b.gasLimit
		header.Difficulty = big.NewInt(0)
		header.Nonce = types.EncodeNonce(0)
		header.UncleHash = types.EmptyUncleHash
		header.MixDigest = types.EmptyMixDigest
		// Needs the baseFee at the final state of the last block
		parentVmRunner := w.chain.NewEVMRunner(parent.Header(), state.Copy())
		header.BaseFee = misc.CalcBaseFee(w.chainConfig, parent.Header(), parentVmRunner)
	}
	if w.chainConfig.IsGingerbreadP2(header.Number) {
		b.bytesBlock = new(core.BytesBlock).SetLimit(params.MaxTxDataPerBlock)
	}
	b.sysCtx = core.NewSysContractCallCtx(header, state.Copy(), w.chain)

	b.multiGasPool = core.NewMultiGasPool(
		b.gasLimit,
		b.sysCtx.GetWhitelistedCurrencies(),
		w.config.FeeCurrencyDefault,
		w.config.FeeCurrencyLimits,
	)

	// Play our part in generating the random beacon.
	if w.isRunning() && random.IsRunning(vmRunner) {
		istanbul, ok := w.engine.(consensus.Istanbul)
		if !ok {
			log.Crit("Istanbul consensus engine must be in use for the randomness beacon")
		}

		lastCommitment, err := random.GetLastCommitment(vmRunner, w.validator)
		if err != nil {
			return b, fmt.Errorf("Failed to get last commitment: %w", err)
		}

		lastRandomness := common.Hash{}
		if (lastCommitment != common.Hash{}) {
			lastRandomnessParentHash := rawdb.ReadRandomCommitmentCache(w.db, lastCommitment)
			if (lastRandomnessParentHash == common.Hash{}) {
				log.Warn("Randomness cache miss while building a block. Attempting to recover.", "number", header.Number.Uint64())

				// We missed on the cache which should have been populated, attempt to repopulate the cache.
				err := w.chain.RecoverRandomnessCache(lastCommitment, b.header.ParentHash)
				if err != nil {
					log.Error("Error in recovering randomness cache", "error", err, "number", header.Number.Uint64())
					return b, errors.New("failed to recover the randomness cache after miss")
				}
				lastRandomnessParentHash = rawdb.ReadRandomCommitmentCache(w.db, lastCommitment)
				if (lastRandomnessParentHash == common.Hash{}) {
					// Recover failed to fix the issue. Bail.
					return b, errors.New("failed to get last randomness cache entry and failed to recover")
				}
			}

			var err error
			lastRandomness, _, err = istanbul.GenerateRandomness(lastRandomnessParentHash)
			if err != nil {
				return b, fmt.Errorf("Failed to generate last randomness: %w", err)
			}
		}

		_, newCommitment, err := istanbul.GenerateRandomness(b.header.ParentHash)
		if err != nil {
			return b, fmt.Errorf("Failed to generate new randomness: %w", err)
		}

		err = random.RevealAndCommit(vmRunner, lastRandomness, newCommitment, w.validator)
		if err != nil {
			return b, fmt.Errorf("Failed to reveal and commit randomness: %w", err)
		}
		// always true (EIP158)
		b.state.IntermediateRoot(true)

		b.randomness = &types.Randomness{Revealed: lastRandomness, Committed: newCommitment}
	} else {
		b.randomness = &types.EmptyRandomness
	}

	return b, nil
}

// selectAndApplyTransactions selects and applies transactions to the in flight block state.
func (b *blockState) selectAndApplyTransactions(ctx context.Context, w *worker) error {
	// Fill the block with all available pending transactions.
	pending, err := w.eth.TxPool().Pending(true)

	// TODO: should this be a fatal error?
	if err != nil {
		log.Error("Failed to fetch pending transactions", "err", err)
		return nil
	}

	// Short circuit if there is no available pending transactions.
	if len(pending) == 0 {
		return nil
	}
	// Split the pending transactions into locals and remotes
	localTxs, remoteTxs := make(map[common.Address]types.Transactions), pending
	for _, account := range w.eth.TxPool().Locals() {
		if txs := remoteTxs[account]; len(txs) > 0 {
			delete(remoteTxs, account)
			localTxs[account] = txs
		}
	}

	// TODO: Properly inject the basefee & toCELO function here
	// txComparator := createTxCmp(w.chain, b.header, b.state)
	if len(localTxs) > 0 {
		baseFeeFn, toCElOFn := createConversionFunctions(b.sysCtx, w.chain, b.header, b.state)
		txs := types.NewTransactionsByPriceAndNonce(b.signer, localTxs, baseFeeFn, toCElOFn)
		if err := b.commitTransactions(ctx, w, txs, b.txFeeRecipient); err != nil {
			return fmt.Errorf("Failed to commit local transactions: %w", err)
		}
	}
	if len(remoteTxs) > 0 {
		baseFeeFn, toCElOFn := createConversionFunctions(b.sysCtx, w.chain, b.header, b.state)
		txs := types.NewTransactionsByPriceAndNonce(b.signer, remoteTxs, baseFeeFn, toCElOFn)
		if err := b.commitTransactions(ctx, w, txs, b.txFeeRecipient); err != nil {
			return fmt.Errorf("Failed to commit remote transactions: %w", err)
		}
	}
	return nil
}

// commitTransactions attempts to commit every transaction in the transactions list until the block is full or there are no more valid transactions.
func (b *blockState) commitTransactions(ctx context.Context, w *worker, txs *types.TransactionsByPriceAndNonce, txFeeRecipient common.Address) error {
	var coalescedLogs []*types.Log

loop:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// pass
		}
		// If we don't have enough gas for any further transactions then we're done
		if b.gasPool.Gas() < params.TxGas {
			log.Trace("Not enough gas for further transactions", "have", b.gasPool, "want", params.TxGas)
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}
		// Short-circuit if the transaction is using more gas allocated for the
		// given fee currency.
		if b.multiGasPool.PoolFor(tx.FeeCurrency()).Gas() < tx.Gas() {
			log.Trace(
				"Skipping transaction which requires more gas than is left in the pool for a specific fee currency",
				"currency", tx.FeeCurrency(), "tx hash", tx.Hash(),
				"gas", b.multiGasPool.PoolFor(tx.FeeCurrency()).Gas(), "txgas", tx.Gas(),
			)
			txs.Pop()
			continue
		}
		// Short-circuit if the transaction requires more gas than we have in the pool.
		// If we didn't short-circuit here, we would get core.ErrGasLimitReached below.
		// Short-circuiting here saves us the trouble of checking the GPM and so on when the tx can't be included
		// anyway due to the block not having enough gas left.
		if b.gasPool.Gas() < tx.Gas() {
			log.Trace("Skipping transaction which requires more gas than is left in the block", "hash", tx.Hash(), "gas", b.gasPool.Gas(), "txgas", tx.Gas())
			txs.Pop()
			continue
		}
		// Same short-circuit of the gas above, but for bytes in the block (b.bytesBlock != nil => GingerbreadP2)
		if b.bytesBlock != nil && b.bytesBlock.BytesLeft() < uint64(tx.Size()) {
			log.Trace("Skipping transaction which requires more bytes than is left in the block", "hash", tx.Hash(), "bytes", b.bytesBlock.BytesLeft(), "txbytes", uint64(tx.Size()))
			txs.Pop()
			continue
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		from, _ := types.Sender(b.signer, tx)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		if tx.Protected() && !w.chainConfig.IsEIP155(b.header.Number) {
			log.Trace("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", w.chainConfig.EIP155Block)

			txs.Pop()
			continue
		}
		if tx.GatewaySet() && w.chainConfig.IsGingerbread(b.header.Number) {
			log.Trace("Ignoring transaction with gateway fee", "hash", tx.Hash(), "gingerbread", w.chainConfig.GingerbreadBlock)

			txs.Pop()
			continue
		}
		// Start executing the transaction
		b.state.Prepare(tx.Hash(), b.tcount)

		availableGas := b.gasPool.Gas()
		logs, err := b.commitTransaction(w, tx, txFeeRecipient)
		gasUsed := availableGas - b.gasPool.Gas()

		switch {
		case errors.Is(err, core.ErrGasLimitReached):
			// Pop the current out-of-gas transaction without shifting in the next from the account
			log.Trace("Gas limit exceeded for current block", "sender", from)
			txs.Pop()

		case errors.Is(err, core.ErrNonceTooLow):
			// New head notification data race between the transaction pool and miner, shift
			log.Trace("Skipping transaction with low nonce", "sender", from, "nonce", tx.Nonce())
			txs.Shift()

		case errors.Is(err, core.ErrNonceTooHigh):
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Trace("Skipping account with hight nonce", "sender", from, "nonce", tx.Nonce())
			txs.Pop()

		case errors.Is(err, core.ErrGasPriceDoesNotExceedMinimum):
			// We are below the GPM, so we can stop (the rest of the transactions will either have
			// even lower gas price or won't be mineable yet due to their nonce)
			log.Trace("Skipping remaining transaction below the gas price minimum")
			break loop

		case errors.Is(err, nil):
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			b.tcount++
			// bytesBlock != nil => GingerbreadP2
			if b.bytesBlock != nil && w.chainConfig.IsGingerbreadP2(b.header.Number) {
				if err := b.bytesBlock.SubBytes(uint64(tx.Size())); err != nil {
					// This should never happen because we are validating before that we have enough space
					return err
				}
			}

			err = b.multiGasPool.PoolFor(tx.FeeCurrency()).SubGas(gasUsed)
			// Should never happen as we check it above
			if err != nil {
				log.Warn(
					"Unexpectedly reached limit for fee currency",
					"hash", tx.Hash(), "gas", b.multiGasPool.PoolFor(tx.FeeCurrency()).Gas(),
					"tx gas used", gasUsed,
				)
				return err
			}

			txs.Shift()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			txs.Shift()
		}
	}

	if !w.isRunning() && len(coalescedLogs) > 0 {
		// We don't push the pendingLogsEvent while we are mining. The reason is that
		// when we are mining, the worker will regenerate a mining block every 3 seconds.
		// In order to avoid pushing the repeated pendingLog, we disable the pending log pushing.

		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		w.pendingLogsFeed.Send(cpy)
	}
	return nil
}

// commitTransaction attempts to appply a single transaction. If the transaction fails, it's modifications are reverted.
func (b *blockState) commitTransaction(w *worker, tx *types.Transaction, txFeeRecipient common.Address) ([]*types.Log, error) {
	snap := b.state.Snapshot()
	vmRunner := w.chain.NewEVMRunner(b.header, b.state)

	receipt, err := core.ApplyTransaction(w.chainConfig, w.chain, &txFeeRecipient, b.gasPool, b.state, b.header, tx, &b.header.GasUsed, *w.chain.GetVMConfig(), vmRunner, b.sysCtx)
	if err != nil {
		b.state.RevertToSnapshot(snap)
		return nil, err
	}
	b.txs = append(b.txs, tx)
	b.receipts = append(b.receipts, receipt)

	return receipt.Logs, nil
}

// finalizeAndAssemble runs post-transaction state modification and assembles the final block.
func (b *blockState) finalizeAndAssemble(w *worker) (*types.Block, error) {
	block, err := w.engine.FinalizeAndAssemble(w.chain, b.header, b.state, b.txs, b.receipts, b.randomness)
	if err != nil {
		return nil, fmt.Errorf("Error in FinalizeAndAssemble: %w", err)
	}

	// Set the validator set diff in the new header if we're using Istanbul and it's the last block of the epoch
	if istanbul, ok := w.engine.(consensus.Istanbul); ok {
		if err := istanbul.UpdateValSetDiff(w.chain, block.MutableHeader(), b.state); err != nil {
			return nil, fmt.Errorf("Unable to update Validator Set Diff: %w", err)
		}
	}
	// FinalizeAndAssemble adds the "block receipt" to then calculate the Bloom filter and receipts hash.
	// But it doesn't return the receipts.  So we have to add the "block receipt" to b.receipts here, for
	// use in calculating the "pending" block (and also in the `task`, though we could remove it from that).
	b.receipts = core.AddBlockReceipt(b.receipts, b.state, block.Hash())

	return block, nil
}

// totalFees computes total consumed fees in CELO. Block transactions and receipts have to have the same order.
func totalFees(block *types.Block, receipts []*types.Receipt, baseFeeFn func(*common.Address) *big.Int, toCELO types.ToCELOFn, espresso bool) *big.Float {
	feesWei := new(big.Int)
	for i, tx := range block.Transactions() {
		var basefee *big.Int
		if espresso {
			basefee = baseFeeFn(tx.FeeCurrency())
		}
		fee, err := toCELO(new(big.Int).Mul(new(big.Int).SetUint64(receipts[i].GasUsed), tx.EffectiveGasTipValue(basefee)), tx.FeeCurrency())
		if err != nil {
			log.Error("totalFees: Could not convert fees for tx", "tx", tx, "err", err)
			continue
		}
		feesWei.Add(feesWei, fee)
	}
	return new(big.Float).Quo(new(big.Float).SetInt(feesWei), new(big.Float).SetInt(big.NewInt(params.Ether)))
}

// createConversionFunctions creates a function to convert any currency to Celo and a function to get the gas price minimum for that currency.
// Both functions internally cache their results.
func createConversionFunctions(sysCtx *core.SysContractCallCtx, chain *core.BlockChain, header *types.Header, state *state.StateDB) (func(feeCurrency *common.Address) *big.Int, types.ToCELOFn) {
	vmRunner := chain.NewEVMRunner(header, state)
	currencyManager := currency.NewManager(vmRunner)

	baseFeeFn := func(feeCurrency *common.Address) *big.Int {
		return sysCtx.GetGasPriceMinimum(feeCurrency)
	}
	toCeloFn := func(amount *big.Int, feeCurrency *common.Address) (*big.Int, error) {
		curr, err := currencyManager.GetCurrency(feeCurrency)
		if err != nil {
			return nil, fmt.Errorf("toCeloFn: %w", err)
		}
		return curr.ToCELO(amount), nil
	}

	return baseFeeFn, toCeloFn
}

func (b *blockState) close() {
	b.state.StopPrefetcher()
}
