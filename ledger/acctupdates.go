// Copyright (C) 2019-2021 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package ledger

import (
	"container/heap"
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/crypto/merkletrie"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/logging/telemetryspec"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/db"
	"github.com/algorand/go-algorand/util/metrics"
)

const (
	// balancesFlushInterval defines how frequently we want to flush our balances to disk.
	balancesFlushInterval = 5 * time.Second
	// pendingDeltasFlushThreshold is the deltas count threshold above we flush the pending balances regardless of the flush interval.
	pendingDeltasFlushThreshold = 128
	// trieRebuildAccountChunkSize defines the number of accounts that would get read at a single chunk
	// before added to the trie during trie construction
	trieRebuildAccountChunkSize = 16384
	// trieRebuildCommitFrequency defines the number of accounts that would get added before we call evict to commit the changes and adjust the memory cache.
	trieRebuildCommitFrequency = 65536
	// trieAccumulatedChangesFlush defines the number of pending changes that would be applied to the merkle trie before
	// we attempt to commit them to disk while writing a batch of rounds balances to disk.
	trieAccumulatedChangesFlush = 256
)

// trieCachedNodesCount defines how many balances trie nodes we would like to keep around in memory.
// value was calibrated using BenchmarkCalibrateCacheNodeSize
var trieCachedNodesCount = 9000

// merkleCommitterNodesPerPage controls how many nodes will be stored in a single page
// value was calibrated using BenchmarkCalibrateNodesPerPage
var merkleCommitterNodesPerPage = int64(116)

// baseAccountsPendingAccountsBufferSize defines the size of the base account pending accounts buffer size.
// At the beginning of a new round, the entries from this buffer are being flushed into the base accounts map.
const baseAccountsPendingAccountsBufferSize = 100000

// baseAccountsPendingAccountsWarnThreshold defines the threshold at which the lruAccounts would generate a warning
// after we've surpassed a given pending account size. The warning is being generated when the pending accounts data
// is being flushed into the main base account cache.
const baseAccountsPendingAccountsWarnThreshold = 85000

// initializeCachesReadaheadBlocksStream defines how many block we're going to attempt to queue for the
// initializeCaches method before it can process and store the account changes to disk.
const initializeCachesReadaheadBlocksStream = 4

// initializeCachesRoundFlushInterval defines the number of rounds between every to consecutive
// attempts to flush the memory account data to disk. Setting this value too high would increase
// memory utilization. Setting this too low, would increase disk i/o.
const initializeCachesRoundFlushInterval = 1000

// initializingAccountCachesMessageTimeout controls the amount of time passes before we
// log "initializingAccount initializing.." message to the log file. This is primarily for
// nodes with slower disk access, where a feedback that the node is functioning correctly is needed.
const initializingAccountCachesMessageTimeout = 3 * time.Second

// accountsUpdatePerRoundHighWatermark is the warning watermark for updating accounts data that takes
// longer than expected. We set it up here for one second per round, so that if we're bulk updating
// four rounds, we would allow up to 4 seconds. This becomes important when supporting balances recovery
// where we end up batching up to 1000 rounds in a single update.
const accountsUpdatePerRoundHighWatermark = 1 * time.Second

// TrieMemoryConfig is the memory configuration setup used for the merkle trie.
var TrieMemoryConfig = merkletrie.MemoryConfig{
	NodesCountPerPage:         merkleCommitterNodesPerPage,
	CachedNodesCount:          trieCachedNodesCount,
	PageFillFactor:            0.95,
	MaxChildrenPagesThreshold: 64,
}

// A modifiedAccount represents an account that has been modified since
// the persistent state stored in the account DB (i.e., in the range of
// rounds covered by the accountUpdates tracker).
type modifiedAccount struct {
	// data stores the most recent AccountData for this modified
	// account.
	data basics.AccountData

	// ndelta keeps track of how many times this account appears in
	// accountUpdates.deltas.  This is used to evict modifiedAccount
	// entries when all changes to an account have been reflected in
	// the account DB, and no outstanding modifications remain.
	ndeltas int
}

type accountUpdates struct {
	// constant variables ( initialized on initialize, and never changed afterward )

	// dbDirectory is the directory where the ledger and block sql file resides as well as the parent directory for the catchup files to be generated
	dbDirectory string

	// catchpointInterval is the configured interval at which the accountUpdates would generate catchpoint labels and catchpoint files.
	catchpointInterval uint64

	// archivalLedger determines whether the associated ledger was configured as archival ledger or not.
	archivalLedger bool

	// catchpointFileHistoryLength defines how many catchpoint files we want to store back.
	// 0 means don't store any, -1 mean unlimited and positive number suggest the number of most recent catchpoint files.
	catchpointFileHistoryLength int

	// dynamic variables

	// Connection to the database.
	dbs db.Pair

	// Prepared SQL statements for fast accounts DB lookups.
	accountsq *accountsDbQueries

	// cachedDBRound is always exactly tracker DB round (and therefore, accountsRound()),
	// cached to use in lookup functions
	cachedDBRound basics.Round

	// deltas stores updates for every round after dbRound.
	deltas []ledgercore.AccountDeltas

	// accounts stores the most recent account state for every
	// address that appears in deltas.
	accounts map[basics.Address]modifiedAccount

	// creatableDeltas stores creatable updates for every round after dbRound.
	creatableDeltas []map[basics.CreatableIndex]ledgercore.ModifiedCreatable

	// creatables stores the most recent state for every creatable that
	// appears in creatableDeltas
	creatables map[basics.CreatableIndex]ledgercore.ModifiedCreatable

	// versions stores consensus version dbRound and every
	// round after it; i.e., versions is one longer than deltas.
	versions []protocol.ConsensusVersion

	// totals stores the totals for dbRound and every round after it;
	// i.e., totals is one longer than deltas.
	roundTotals []ledgercore.AccountTotals

	// roundDigest stores the digest of the block for every round starting with dbRound and every round after it.
	roundDigest []crypto.Digest

	// log copied from ledger
	log logging.Logger

	// lastFlushTime is the time we last flushed updates to
	// the accounts DB (bumping dbRound).
	lastFlushTime time.Time

	// ledger is the source ledger, which is used to synchronize
	// the rounds at which we need to flush the balances to disk
	// in favor of the catchpoint to be generated.
	ledger ledgerForTracker

	// The Trie tracking the current account balances. Always matches the balances that were
	// written to the database.
	balancesTrie *merkletrie.Trie

	// The last catchpoint label that was written to the database. Should always align with what's in the database.
	// note that this is the last catchpoint *label* and not the catchpoint file.
	lastCatchpointLabel string

	// catchpointWriting help to synchronize the catchpoint file writing. When this atomic variable is 0, no writing is going on.
	// Any non-zero value indicates a catchpoint being written.
	catchpointWriting int32

	// catchpointSlowWriting suggest to the accounts writer that it should finish writing up the catchpoint file ASAP.
	// when this channel is closed, the accounts writer would try and complete the writing as soon as possible.
	// otherwise, it would take it's time and perform periodic sleeps between chunks processing.
	catchpointSlowWriting chan struct{}

	// ctx is the context for the committing go-routine. It's also used as the "parent" of the catchpoint generation operation.
	ctx context.Context

	// ctxCancel is the canceling function for canceling the committing go-routine ( i.e. signaling the committing go-routine that it's time to abort )
	ctxCancel context.CancelFunc

	// deltasAccum stores the accumulated deltas for every round starting dbRound-1.
	deltasAccum []int

	// accountsMu is the synchronization mutex for accessing the various non-static variables.
	accountsMu deadlock.RWMutex

	// accountsReadCond used to synchronize read access to the internal data structures.
	accountsReadCond *sync.Cond

	// voters keeps track of Merkle trees of online accounts, used for compact certificates.
	voters *votersTracker

	// baseAccounts stores the most recently used accounts, at exactly dbRound
	baseAccounts lruAccounts

	// the synchronous mode that would be used for the account database.
	synchronousMode db.SynchronousMode

	// the synchronous mode that would be used while the accounts database is being rebuilt.
	accountsRebuildSynchronousMode db.SynchronousMode

	// logAccountUpdatesMetrics is a flag for enable/disable metrics logging
	logAccountUpdatesMetrics bool

	// logAccountUpdatesInterval sets a time interval for metrics logging
	logAccountUpdatesInterval time.Duration

	// lastMetricsLogTime is the time when the previous metrics logging occurred
	lastMetricsLogTime time.Time
}

type deferredCommit struct {
	offset   uint64
	dbRound  basics.Round
	lookback basics.Round
}

// RoundOffsetError is an error for when requested round is behind earliest stored db entry
type RoundOffsetError struct {
	round   basics.Round
	dbRound basics.Round
}

func (e *RoundOffsetError) Error() string {
	return fmt.Sprintf("round %d before dbRound %d", e.round, e.dbRound)
}

// StaleDatabaseRoundError is generated when we detect that the database round is behind the accountUpdates in-memory dbRound. This
// should never happen, since we update the database first, and only upon a successful update we update the in-memory dbRound.
type StaleDatabaseRoundError struct {
	memoryRound   basics.Round
	databaseRound basics.Round
}

func (e *StaleDatabaseRoundError) Error() string {
	return fmt.Sprintf("database round %d is behind in-memory round %d", e.databaseRound, e.memoryRound)
}

// MismatchingDatabaseRoundError is generated when we detect that the database round is different than the accountUpdates in-memory dbRound. This
// could happen normally when the database and the in-memory dbRound aren't synchronized. However, when we work in non-sync mode, we expect the database to be
// always synchronized with the in-memory data. When that condition is violated, this error is generated.
type MismatchingDatabaseRoundError struct {
	memoryRound   basics.Round
	databaseRound basics.Round
}

func (e *MismatchingDatabaseRoundError) Error() string {
	return fmt.Sprintf("database round %d mismatching in-memory round %d", e.databaseRound, e.memoryRound)
}

// initialize initializes the accountUpdates structure
func (au *accountUpdates) initialize(cfg config.Local, dbPathPrefix string) {
	au.dbDirectory = filepath.Dir(dbPathPrefix)
	au.archivalLedger = cfg.Archival
	switch cfg.CatchpointTracking {
	case -1:
		au.catchpointInterval = 0
	default:
		// give a warning, then fall thought
		logging.Base().Warnf("accountUpdates: the CatchpointTracking field in the config.json file contains an invalid value (%d). The default value of 0 would be used instead.", cfg.CatchpointTracking)
		fallthrough
	case 0:
		if au.archivalLedger {
			au.catchpointInterval = cfg.CatchpointInterval
		} else {
			au.catchpointInterval = 0
		}
	case 1:
		au.catchpointInterval = cfg.CatchpointInterval
	}

	au.catchpointFileHistoryLength = cfg.CatchpointFileHistoryLength
	if cfg.CatchpointFileHistoryLength < -1 {
		au.catchpointFileHistoryLength = -1
	}

	au.accountsReadCond = sync.NewCond(au.accountsMu.RLocker())
	au.synchronousMode = db.SynchronousMode(cfg.LedgerSynchronousMode)
	au.accountsRebuildSynchronousMode = db.SynchronousMode(cfg.AccountsRebuildSynchronousMode)

	// log metrics
	au.logAccountUpdatesMetrics = cfg.EnableAccountUpdatesStats
	au.logAccountUpdatesInterval = cfg.AccountUpdatesStatsInterval
}

func (au *accountUpdates) catchpointEnabled() bool {
	return au.catchpointInterval != 0
}

// loadFromDisk is the 2nd level initialization, and is required before the accountUpdates becomes functional
// The close function is expected to be call in pair with loadFromDisk
func (au *accountUpdates) loadFromDisk(l ledgerForTracker, lastBalancesRound basics.Round) error {
	au.accountsMu.Lock()
	defer au.accountsMu.Unlock()

	au.cachedDBRound = lastBalancesRound
	lastestBlockRound := l.Latest()
	err := au.initializeFromDisk(l, lastBalancesRound)
	if err != nil {
		return err
	}

	writingCatchpointRound, _, err := au.accountsq.readCatchpointStateUint64(context.Background(), catchpointStateWritingCatchpoint)
	if err != nil {
		return err
	}

	writingCatchpointDigest, err := au.initializeCaches(lastBalancesRound, lastestBlockRound, basics.Round(writingCatchpointRound))
	if err != nil {
		return err
	}

	if writingCatchpointRound != 0 && au.catchpointEnabled() {
		au.generateCatchpoint(basics.Round(writingCatchpointRound), au.lastCatchpointLabel, writingCatchpointDigest, time.Duration(0))
	}

	au.voters = &votersTracker{}
	err = au.voters.loadFromDisk(l, au)
	if err != nil {
		return err
	}

	return nil
}

// close closes the accountUpdates, waiting for all the child go-routine to complete
func (au *accountUpdates) close() {
	if au.voters != nil {
		au.voters.close()
	}
	if au.ctxCancel != nil {
		au.ctxCancel()
	}

	au.baseAccounts.prune(0)
}

// IsWritingCatchpointFile returns true when a catchpoint file is being generated. The function is used by the catchup service
// to avoid memory pressure until the catchpoint file writing is complete.
func (au *accountUpdates) IsWritingCatchpointFile() bool {
	return atomic.LoadInt32(&au.catchpointWriting) != 0
}

// LookupWithRewards returns the account data for a given address at a given round.
// Note that the function doesn't update the account with the rewards,
// even while it does return the AccountData which represent the "rewarded" account data.
func (au *accountUpdates) LookupWithRewards(rnd basics.Round, addr basics.Address) (data basics.AccountData, err error) {
	return au.lookupWithRewards(rnd, addr)
}

// LookupWithoutRewards returns the account data for a given address at a given round.
func (au *accountUpdates) LookupWithoutRewards(rnd basics.Round, addr basics.Address) (data basics.AccountData, validThrough basics.Round, err error) {
	return au.lookupWithoutRewards(rnd, addr, true /* take lock*/)
}

// ListAssets lists the assets by their asset index, limiting to the first maxResults
func (au *accountUpdates) ListAssets(maxAssetIdx basics.AssetIndex, maxResults uint64) ([]basics.CreatableLocator, error) {
	return au.listCreatables(basics.CreatableIndex(maxAssetIdx), maxResults, basics.AssetCreatable)
}

// ListApplications lists the application by their app index, limiting to the first maxResults
func (au *accountUpdates) ListApplications(maxAppIdx basics.AppIndex, maxResults uint64) ([]basics.CreatableLocator, error) {
	return au.listCreatables(basics.CreatableIndex(maxAppIdx), maxResults, basics.AppCreatable)
}

// listCreatables lists the application/asset by their app/asset index, limiting to the first maxResults
func (au *accountUpdates) listCreatables(maxCreatableIdx basics.CreatableIndex, maxResults uint64, ctype basics.CreatableType) ([]basics.CreatableLocator, error) {
	au.accountsMu.RLock()
	for {
		currentDbRound := au.cachedDBRound
		currentDeltaLen := len(au.deltas)
		// Sort indices for creatables that have been created/deleted. If this
		// turns out to be too inefficient, we could keep around a heap of
		// created/deleted asset indices in memory.
		keys := make([]basics.CreatableIndex, 0, len(au.creatables))
		for cidx, delta := range au.creatables {
			if delta.Ctype != ctype {
				continue
			}
			if cidx <= maxCreatableIdx {
				keys = append(keys, cidx)
			}
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] > keys[j] })

		// Check for creatables that haven't been synced to disk yet.
		unsyncedCreatables := make([]basics.CreatableLocator, 0, len(keys))
		deletedCreatables := make(map[basics.CreatableIndex]bool, len(keys))
		for _, cidx := range keys {
			delta := au.creatables[cidx]
			if delta.Created {
				// Created but only exists in memory
				unsyncedCreatables = append(unsyncedCreatables, basics.CreatableLocator{
					Type:    delta.Ctype,
					Index:   cidx,
					Creator: delta.Creator,
				})
			} else {
				// Mark deleted creatables for exclusion from the results set
				deletedCreatables[cidx] = true
			}
		}

		au.accountsMu.RUnlock()

		// Check in-memory created creatables, which will always be newer than anything
		// in the database
		if uint64(len(unsyncedCreatables)) >= maxResults {
			return unsyncedCreatables[:maxResults], nil
		}
		res := unsyncedCreatables

		// Fetch up to maxResults - len(res) + len(deletedCreatables) from the database,
		// so we have enough extras in case creatables were deleted
		numToFetch := maxResults - uint64(len(res)) + uint64(len(deletedCreatables))
		dbResults, dbRound, err := au.accountsq.listCreatables(maxCreatableIdx, numToFetch, ctype)
		if err != nil {
			return nil, err
		}

		if dbRound == currentDbRound {
			// Now we merge the database results with the in-memory results
			for _, loc := range dbResults {
				// Check if we have enough results
				if uint64(len(res)) == maxResults {
					return res, nil
				}

				// Creatable was deleted
				if _, ok := deletedCreatables[loc.Index]; ok {
					continue
				}

				// We're OK to include this result
				res = append(res, loc)
			}
			return res, nil
		}
		if dbRound < currentDbRound {
			au.log.Errorf("listCreatables: database round %d is behind in-memory round %d", dbRound, currentDbRound)
			return []basics.CreatableLocator{}, &StaleDatabaseRoundError{databaseRound: dbRound, memoryRound: currentDbRound}
		}
		au.accountsMu.RLock()
		for currentDbRound >= au.cachedDBRound && currentDeltaLen == len(au.deltas) {
			au.accountsReadCond.Wait()
		}
	}
}

// onlineTop returns the top n online accounts, sorted by their normalized
// balance and address, whose voting keys are valid in voteRnd.  See the
// normalization description in AccountData.NormalizedOnlineBalance().
func (au *accountUpdates) onlineTop(rnd basics.Round, voteRnd basics.Round, n uint64) ([]*ledgercore.OnlineAccount, error) {
	proto := au.ledger.GenesisProto()
	au.accountsMu.RLock()
	for {
		currentDbRound := au.cachedDBRound
		currentDeltaLen := len(au.deltas)
		offset, err := au.roundOffset(rnd)
		if err != nil {
			au.accountsMu.RUnlock()
			return nil, err
		}

		// Determine how many accounts have been modified in-memory,
		// so that we obtain enough top accounts from disk (accountdb).
		// If the *onlineAccount is nil, that means the account is offline
		// as of the most recent change to that account, or its vote key
		// is not valid in voteRnd.  Otherwise, the *onlineAccount is the
		// representation of the most recent state of the account, and it
		// is online and can vote in voteRnd.
		modifiedAccounts := make(map[basics.Address]*ledgercore.OnlineAccount)
		for o := uint64(0); o < offset; o++ {
			for i := 0; i < au.deltas[o].Len(); i++ {
				addr, d := au.deltas[o].GetByIdx(i)
				if d.Status != basics.Online {
					modifiedAccounts[addr] = nil
					continue
				}

				if !(d.VoteFirstValid <= voteRnd && voteRnd <= d.VoteLastValid) {
					modifiedAccounts[addr] = nil
					continue
				}

				modifiedAccounts[addr] = accountDataToOnline(addr, &d, proto)
			}
		}

		au.accountsMu.RUnlock()

		// Build up a set of candidate accounts.  Start by loading the
		// top N + len(modifiedAccounts) accounts from disk (accountdb).
		// This ensures that, even if the worst case if all in-memory
		// changes are deleting the top accounts in accountdb, we still
		// will have top N left.
		//
		// Keep asking for more accounts until we get the desired number,
		// or there are no more accounts left.
		candidates := make(map[basics.Address]*ledgercore.OnlineAccount)
		batchOffset := uint64(0)
		batchSize := uint64(1024)
		var dbRound basics.Round
		for uint64(len(candidates)) < n+uint64(len(modifiedAccounts)) {
			var accts map[basics.Address]*ledgercore.OnlineAccount
			start := time.Now()
			ledgerAccountsonlinetopCount.Inc(nil)
			err = au.dbs.Rdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
				accts, err = accountsOnlineTop(tx, batchOffset, batchSize, proto)
				if err != nil {
					return
				}
				dbRound, err = accountsRound(tx)
				return
			})
			ledgerAccountsonlinetopMicros.AddMicrosecondsSince(start, nil)
			if err != nil {
				return nil, err
			}

			if dbRound != currentDbRound {
				break
			}

			for addr, data := range accts {
				if !(data.VoteFirstValid <= voteRnd && voteRnd <= data.VoteLastValid) {
					continue
				}
				candidates[addr] = data
			}

			// If we got fewer than batchSize accounts, there are no
			// more accounts to look at.
			if uint64(len(accts)) < batchSize {
				break
			}

			batchOffset += batchSize
		}
		if dbRound != currentDbRound && dbRound != basics.Round(0) {
			// database round doesn't match the last au.dbRound we sampled.
			au.accountsMu.RLock()
			for currentDbRound >= au.cachedDBRound && currentDeltaLen == len(au.deltas) {
				au.accountsReadCond.Wait()
			}
			continue
		}

		// Now update the candidates based on the in-memory deltas.
		for addr, oa := range modifiedAccounts {
			if oa == nil {
				delete(candidates, addr)
			} else {
				candidates[addr] = oa
			}
		}

		// Get the top N accounts from the candidate set, by inserting all of
		// the accounts into a heap and then pulling out N elements from the
		// heap.
		topHeap := &onlineTopHeap{
			accts: nil,
		}

		for _, data := range candidates {
			heap.Push(topHeap, data)
		}

		var res []*ledgercore.OnlineAccount
		for topHeap.Len() > 0 && uint64(len(res)) < n {
			acct := heap.Pop(topHeap).(*ledgercore.OnlineAccount)
			res = append(res, acct)
		}

		return res, nil
	}
}

// GetLastCatchpointLabel retrieves the last catchpoint label that was stored to the database.
func (au *accountUpdates) GetLastCatchpointLabel() string {
	au.accountsMu.RLock()
	defer au.accountsMu.RUnlock()
	return au.lastCatchpointLabel
}

// GetCreatorForRound returns the creator for a given asset/app index at a given round
func (au *accountUpdates) GetCreatorForRound(rnd basics.Round, cidx basics.CreatableIndex, ctype basics.CreatableType) (creator basics.Address, ok bool, err error) {
	return au.getCreatorForRound(rnd, cidx, ctype, true /* take the lock */)
}

func (au *accountUpdates) committedUpTo(committedRound basics.Round) (retRound basics.Round) {
	au.accountsMu.RLock()
	defer au.accountsMu.RUnlock()

	retRound = basics.Round(0)
	lookback := basics.Round(config.Consensus[au.versions[len(au.versions)-1]].MaxBalLookback)
	if committedRound < lookback {
		return
	}

	retRound = au.cachedDBRound
	return
}

// produceCommittingTask enqueues committing the balances for round committedRound-lookback.
// The deferred committing is done so that we could calculate the historical balances lookback rounds back.
// Since we don't want to hold off the tracker's mutex for too long, we'll defer the database persistence of this
// operation to a syncer goroutine. The one caveat is that when storing a catchpoint round, we would want to
// wait until the catchpoint creation is done, so that the persistence of the catchpoint file would have an
// uninterrupted view of the balances at a given point of time.
func (au *accountUpdates) produceCommittingTask(committedRound basics.Round, dbRound basics.Round) (dc deferredCommit) {
	var isCatchpointRound, hasMultipleIntermediateCatchpoint bool
	var offset uint64
	au.accountsMu.RLock()
	defer au.accountsMu.RUnlock()

	var pendingDeltas int
	lookback := basics.Round(config.Consensus[au.versions[len(au.versions)-1]].MaxBalLookback)
	if committedRound < lookback {
		return
	}

	newBase := committedRound - lookback
	if newBase <= dbRound {
		// Already forgotten
		return
	}

	if newBase > dbRound+basics.Round(len(au.deltas)) {
		au.log.Panicf("produceCommittingTask: block %d too far in the future, lookback %d, dbRound %d (cached %d), deltas %d", committedRound, lookback, dbRound, au.cachedDBRound, len(au.deltas))
	}

	hasIntermediateCatchpoint := false
	hasMultipleIntermediateCatchpoint = false
	// check if there was a catchpoint between au.dbRound+lookback and newBase+lookback
	if au.catchpointInterval > 0 {
		nextCatchpointRound := ((uint64(dbRound+lookback) + au.catchpointInterval) / au.catchpointInterval) * au.catchpointInterval

		if nextCatchpointRound < uint64(newBase+lookback) {
			mostRecentCatchpointRound := (uint64(committedRound) / au.catchpointInterval) * au.catchpointInterval
			newBase = basics.Round(nextCatchpointRound) - lookback
			if mostRecentCatchpointRound > nextCatchpointRound {
				hasMultipleIntermediateCatchpoint = true
				// skip if there is more than one catchpoint in queue
				newBase = basics.Round(mostRecentCatchpointRound) - lookback
			}
			hasIntermediateCatchpoint = true
		}
	}

	// if we're still writing the previous balances, we can't move forward yet.
	if au.IsWritingCatchpointFile() {
		// if we hit this path, it means that we're still writing a catchpoint.
		// see if the new delta range contains another catchpoint.
		if hasIntermediateCatchpoint {
			// check if we're already attempting to perform fast-writing.
			select {
			case <-au.catchpointSlowWriting:
				// yes, we're already doing fast-writing.
			default:
				// no, we're not yet doing fast writing, make it so.
				close(au.catchpointSlowWriting)
			}
		}
		return
	}

	if au.voters != nil {
		newBase = au.voters.lowestRound(newBase)
	}

	offset = uint64(newBase - dbRound)

	offset = au.consecutiveVersion(offset)

	// check to see if this is a catchpoint round
	isCatchpointRound = au.isCatchpointRound(offset, dbRound, lookback)

	// calculate the number of pending deltas
	pendingDeltas = au.deltasAccum[offset] - au.deltasAccum[0]

	// If we recently flushed, wait to aggregate some more blocks.
	// ( unless we're creating a catchpoint, in which case we want to flush it right away
	//   so that all the instances of the catchpoint would contain exactly the same data )
	flushTime := time.Now()
	if !flushTime.After(au.lastFlushTime.Add(balancesFlushInterval)) && !isCatchpointRound && pendingDeltas < pendingDeltasFlushThreshold {
		return
	}

	if isCatchpointRound && au.archivalLedger {
		// store non-zero ( all ones ) into the catchpointWriting atomic variable to indicate that a catchpoint is being written ( or, queued to be written )
		atomic.StoreInt32(&au.catchpointWriting, int32(-1))
		au.catchpointSlowWriting = make(chan struct{}, 1)
		if hasMultipleIntermediateCatchpoint {
			close(au.catchpointSlowWriting)
		}
	}

	// submit committing task only if offset is non-zero in addition to
	// 1) no pending catchpoint writes
	// 2) batching requirements meet or catchpoint round
	dc = deferredCommit{
		offset:   offset,
		dbRound:  dbRound,
		lookback: lookback,
	}
	return
}

func (au *accountUpdates) consecutiveVersion(offset uint64) uint64 {
	// check if this update chunk spans across multiple consensus versions. If so, break it so that each update would tackle only a single
	// consensus version.
	if au.versions[1] != au.versions[offset] {
		// find the tip point.
		tipPoint := sort.Search(int(offset), func(i int) bool {
			// we're going to search here for version inequality, with the assumption that consensus versions won't repeat.
			// that allow us to support [ver1, ver1, ..., ver2, ver2, ..., ver3, ver3] but not [ver1, ver1, ..., ver2, ver2, ..., ver1, ver3].
			return au.versions[1] != au.versions[1+i]
		})
		// no need to handle the case of "no found", or tipPoint==int(offset), since we already know that it's there.
		offset = uint64(tipPoint)
	}
	return offset
}

// newBlock is the accountUpdates implementation of the ledgerTracker interface. This is the "external" facing function
// which invokes the internal implementation after taking the lock.
func (au *accountUpdates) newBlock(blk bookkeeping.Block, delta ledgercore.StateDelta) {
	au.accountsMu.Lock()
	au.newBlockImpl(blk, delta)
	au.accountsMu.Unlock()
	au.accountsReadCond.Broadcast()
}

// Totals returns the totals for a given round
func (au *accountUpdates) Totals(rnd basics.Round) (totals ledgercore.AccountTotals, err error) {
	au.accountsMu.RLock()
	defer au.accountsMu.RUnlock()
	return au.totalsImpl(rnd)
}

// LatestTotals returns the totals of all accounts for the most recent round, as well as the round number
func (au *accountUpdates) LatestTotals() (basics.Round, ledgercore.AccountTotals, error) {
	au.accountsMu.RLock()
	defer au.accountsMu.RUnlock()
	return au.latestTotalsImpl()
}

// ReadCloseSizer interface implements the standard io.Reader and io.Closer as well
// as supporting the Size() function that let the caller know what the size of the stream would be (in bytes).
type ReadCloseSizer interface {
	io.ReadCloser
	Size() (int64, error)
}

// readCloseSizer is an instance of the ReadCloseSizer interface
type readCloseSizer struct {
	io.ReadCloser
	size int64
}

// Size returns the length of the associated stream.
func (r *readCloseSizer) Size() (int64, error) {
	if r.size < 0 {
		return 0, fmt.Errorf("unknown stream size")
	}
	return r.size, nil
}

// GetCatchpointStream returns a ReadCloseSizer to the catchpoint file associated with the provided round
func (au *accountUpdates) GetCatchpointStream(round basics.Round) (ReadCloseSizer, error) {
	dbFileName := ""
	fileSize := int64(0)
	start := time.Now()
	ledgerGetcatchpointCount.Inc(nil)
	err := au.dbs.Rdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		dbFileName, _, fileSize, err = getCatchpoint(tx, round)
		return
	})
	ledgerGetcatchpointMicros.AddMicrosecondsSince(start, nil)
	if err != nil && err != sql.ErrNoRows {
		// we had some sql error.
		return nil, fmt.Errorf("accountUpdates: getCatchpointStream: unable to lookup catchpoint %d: %v", round, err)
	}
	if dbFileName != "" {
		catchpointPath := filepath.Join(au.dbDirectory, dbFileName)
		file, err := os.OpenFile(catchpointPath, os.O_RDONLY, 0666)
		if err == nil && file != nil {
			return &readCloseSizer{ReadCloser: file, size: fileSize}, nil
		}
		// else, see if this is a file-not-found error
		if os.IsNotExist(err) {
			// the database told us that we have this file.. but we couldn't find it.
			// delete it from the database.
			err := au.saveCatchpointFile(round, "", 0, "")
			if err != nil {
				au.log.Warnf("accountUpdates: getCatchpointStream: unable to delete missing catchpoint entry: %v", err)
				return nil, err
			}

			return nil, ledgercore.ErrNoEntry{}
		}
		// it's some other error.
		return nil, fmt.Errorf("accountUpdates: getCatchpointStream: unable to open catchpoint file '%s' %v", catchpointPath, err)
	}

	// if the database doesn't know about that round, see if we have that file anyway:
	fileName := filepath.Join("catchpoints", catchpointRoundToPath(round))
	catchpointPath := filepath.Join(au.dbDirectory, fileName)
	file, err := os.OpenFile(catchpointPath, os.O_RDONLY, 0666)
	if err == nil && file != nil {
		// great, if found that we should have had this in the database.. add this one now :
		fileInfo, err := file.Stat()
		if err != nil {
			// we couldn't get the stat, so just return with the file.
			return &readCloseSizer{ReadCloser: file, size: -1}, nil
		}

		err = au.saveCatchpointFile(round, fileName, fileInfo.Size(), "")
		if err != nil {
			au.log.Warnf("accountUpdates: getCatchpointStream: unable to save missing catchpoint entry: %v", err)
		}
		return &readCloseSizer{ReadCloser: file, size: fileInfo.Size()}, nil
	}
	return nil, ledgercore.ErrNoEntry{}
}

// functions below this line are all internal functions

// accountUpdatesLedgerEvaluator is a "ledger emulator" which is used *only* by initializeCaches, as a way to shortcut
// the locks taken by the real ledger object when making requests that are being served by the accountUpdates.
// Using this struct allow us to take the tracker lock *before* calling the loadFromDisk, and having the operation complete
// without taking any locks. Note that it's not only the locks performance that is gained : by having the loadFrom disk
// not requiring any external locks, we can safely take a trackers lock on the ledger during reloadLedger, which ensures
// that even during catchpoint catchup mode switch, we're still correctly protected by a mutex.
type accountUpdatesLedgerEvaluator struct {
	// au is the associated accountUpdates structure which invoking the trackerEvalVerified function, passing this structure as input.
	// the accountUpdatesLedgerEvaluator would access the underlying accountUpdates function directly, bypassing the balances mutex lock.
	au *accountUpdates
	// prevHeader is the previous header to the current one. The usage of this is only in the context of initializeCaches where we iteratively
	// building the ledgercore.StateDelta, which requires a peek on the "previous" header information.
	prevHeader bookkeeping.BlockHeader
}

// GenesisHash returns the genesis hash
func (aul *accountUpdatesLedgerEvaluator) GenesisHash() crypto.Digest {
	return aul.au.ledger.GenesisHash()
}

// CompactCertVoters returns the top online accounts at round rnd.
func (aul *accountUpdatesLedgerEvaluator) CompactCertVoters(rnd basics.Round) (voters *ledgercore.VotersForRound, err error) {
	return aul.au.voters.getVoters(rnd)
}

// BlockHdr returns the header of the given round. When the evaluator is running, it's only referring to the previous header, which is what we
// are providing here. Any attempt to access a different header would get denied.
func (aul *accountUpdatesLedgerEvaluator) BlockHdr(r basics.Round) (bookkeeping.BlockHeader, error) {
	if r == aul.prevHeader.Round {
		return aul.prevHeader, nil
	}
	return bookkeeping.BlockHeader{}, ledgercore.ErrNoEntry{}
}

// LatestTotals returns the totals of all accounts for the most recent round, as well as the round number
func (aul *accountUpdatesLedgerEvaluator) LatestTotals() (basics.Round, ledgercore.AccountTotals, error) {
	return aul.au.latestTotalsImpl()
}

// CheckDup test to see if the given transaction id/lease already exists. It's not needed by the accountUpdatesLedgerEvaluator and implemented as a stub.
func (aul *accountUpdatesLedgerEvaluator) CheckDup(config.ConsensusParams, basics.Round, basics.Round, basics.Round, transactions.Txid, ledgercore.Txlease) error {
	// this is a non-issue since this call will never be made on non-validating evaluation
	return fmt.Errorf("accountUpdatesLedgerEvaluator: tried to check for dup during accountUpdates initialization ")
}

// lookupWithoutRewards returns the account balance for a given address at a given round, without the reward
func (aul *accountUpdatesLedgerEvaluator) LookupWithoutRewards(rnd basics.Round, addr basics.Address) (basics.AccountData, basics.Round, error) {
	return aul.au.lookupWithoutRewards(rnd, addr, false /*don't sync*/)
}

// GetCreatorForRound returns the asset/app creator for a given asset/app index at a given round
func (aul *accountUpdatesLedgerEvaluator) GetCreatorForRound(rnd basics.Round, cidx basics.CreatableIndex, ctype basics.CreatableType) (creator basics.Address, ok bool, err error) {
	return aul.au.getCreatorForRound(rnd, cidx, ctype, false /* don't sync */)
}

// totalsImpl returns the totals for a given round
func (au *accountUpdates) totalsImpl(rnd basics.Round) (totals ledgercore.AccountTotals, err error) {
	offset, err := au.roundOffset(rnd)
	if err != nil {
		return
	}

	totals = au.roundTotals[offset]
	return
}

// latestTotalsImpl returns the totals of all accounts for the most recent round, as well as the round number
func (au *accountUpdates) latestTotalsImpl() (basics.Round, ledgercore.AccountTotals, error) {
	offset := len(au.deltas)
	rnd := au.cachedDBRound + basics.Round(len(au.deltas))
	return rnd, au.roundTotals[offset], nil
}

// initializeCaches fills up the accountUpdates cache with the most recent ~320 blocks ( on normal execution ).
// the method also support balances recovery in cases where the difference between the lastBalancesRound and the lastestBlockRound
// is far greater than 320; in these cases, it would flush to disk periodically in order to avoid high memory consumption.
func (au *accountUpdates) initializeCaches(lastBalancesRound, lastestBlockRound, writingCatchpointRound basics.Round) (catchpointBlockDigest crypto.Digest, err error) {
	var blk bookkeeping.Block
	var delta ledgercore.StateDelta

	accLedgerEval := accountUpdatesLedgerEvaluator{
		au: au,
	}
	if lastBalancesRound < lastestBlockRound {
		accLedgerEval.prevHeader, err = au.ledger.BlockHdr(lastBalancesRound)
		if err != nil {
			return
		}
	}

	skipAccountCacheMessage := make(chan struct{})
	writeAccountCacheMessageCompleted := make(chan struct{})
	defer func() {
		close(skipAccountCacheMessage)
		select {
		case <-writeAccountCacheMessageCompleted:
			if err == nil {
				au.log.Infof("initializeCaches completed initializing account data caches")
			}
		default:
		}
	}()

	// this goroutine logs a message once if the parent function have not completed in initializingAccountCachesMessageTimeout seconds.
	// the message is important, since we're blocking on the ledger block database here, and we want to make sure that we log a message
	// within the above timeout.
	go func() {
		select {
		case <-time.After(initializingAccountCachesMessageTimeout):
			au.log.Infof("initializeCaches is initializing account data caches")
			close(writeAccountCacheMessageCompleted)
		case <-skipAccountCacheMessage:
		}
	}()

	blocksStream := make(chan bookkeeping.Block, initializeCachesReadaheadBlocksStream)
	blockEvalFailed := make(chan struct{}, 1)
	var blockRetrievalError error
	go func() {
		defer close(blocksStream)
		for roundNumber := lastBalancesRound + 1; roundNumber <= lastestBlockRound; roundNumber++ {
			blk, blockRetrievalError = au.ledger.Block(roundNumber)
			if blockRetrievalError != nil {
				return
			}
			select {
			case blocksStream <- blk:
			case <-blockEvalFailed:
				return
			}
		}
	}()

	lastFlushedRound := lastBalancesRound
	const accountsCacheLoadingMessageInterval = 5 * time.Second
	lastProgressMessage := time.Now().Add(-accountsCacheLoadingMessageInterval / 2)

	// rollbackSynchronousMode ensures that we switch to "fast writing mode" when we start flushing out rounds to disk, and that
	// we exit this mode when we're done.
	rollbackSynchronousMode := false
	defer func() {
		if rollbackSynchronousMode {
			// restore default synchronous mode
			au.dbs.Wdb.SetSynchronousMode(context.Background(), au.synchronousMode, au.synchronousMode >= db.SynchronousModeFull)
		}
	}()

	for blk := range blocksStream {
		delta, err = au.ledger.trackerEvalVerified(blk, &accLedgerEval)
		if err != nil {
			close(blockEvalFailed)
			return
		}

		au.newBlockImpl(blk, delta)

		if blk.Round() == basics.Round(writingCatchpointRound) {
			catchpointBlockDigest = blk.Digest()
		}

		// flush to disk if any of the following applies:
		// 1. if we have loaded up more than initializeCachesRoundFlushInterval rounds since the last time we flushed the data to disk
		// 2. if we completed the loading and we loaded up more than 320 rounds.
		flushIntervalExceed := blk.Round()-lastFlushedRound > initializeCachesRoundFlushInterval
		loadCompleted := (lastestBlockRound == blk.Round() && lastBalancesRound+basics.Round(blk.ConsensusProtocol().MaxBalLookback) < lastestBlockRound)
		if flushIntervalExceed || loadCompleted {
			// adjust the last flush time, so that we would not hold off the flushing due to "working too fast"
			au.lastFlushTime = time.Now().Add(-balancesFlushInterval)

			if !rollbackSynchronousMode {
				// switch to rebuild synchronous mode to improve performance
				au.dbs.Wdb.SetSynchronousMode(context.Background(), au.accountsRebuildSynchronousMode, au.accountsRebuildSynchronousMode >= db.SynchronousModeFull)

				// flip the switch to rollback the synchronous mode once we're done.
				rollbackSynchronousMode = true
			}

			var roundsBehind basics.Round
			func() {
				// The unlocking/relocking here isn't very elegant, but it does get the work done :
				// this method is called on either startup or when fast catchup is complete. In the former usecase, the
				// locking here is not really needed since the system is only starting up, and there are no other
				// consumers for the accounts update. On the latter usecase, the function would always have exactly 320 rounds,
				// and therefore this wouldn't be an issue.
				// However, to make sure we're not missing any other future codepath, unlocking here and re-locking later on is a pretty
				// safe bet.
				// Wrapping locking/unlocking within a function scope prevents possible loss of the expected accountsMu state in case of panicing inside scheduleCommit.
				au.accountsMu.Unlock()
				defer au.accountsMu.Lock()

				// flush the account data
				// TODO: figure out how to move it the upper level
				au.ledger.scheduleCommit(blk.Round())

				// wait for the writing to complete.
				au.ledger.waitAccountsWriting()

				// The au.dbRound after writing should be ~320 behind the block round.
				roundsBehind = blk.Round() - au.cachedDBRound
			}()

			// are we too far behind ? ( taking into consideration the catchpoint writing, which can stall the writing for quite a bit )
			if roundsBehind > initializeCachesRoundFlushInterval+basics.Round(au.catchpointInterval) {
				// we're unable to persist changes. This is unexpected, but there is no point in keep trying batching additional changes since any further changes
				// would just accumulate in memory.
				close(blockEvalFailed)
				au.log.Errorf("initializeCaches was unable to fill up the account caches accounts round = %d, block round = %d. See above error for more details.", au.cachedDBRound, blk.Round())
				err = fmt.Errorf("initializeCaches failed to initialize the account data caches")
				return
			}

			// and once we flushed it to disk, update the lastFlushedRound
			lastFlushedRound = blk.Round()
		}

		// if enough time have passed since the last time we wrote a message to the log file then give the user an update about the progess.
		if time.Now().Sub(lastProgressMessage) > accountsCacheLoadingMessageInterval {
			// drop the initial message if we're got to this point since a message saying "still initializing" that comes after "is initializing" doesn't seems to be right.
			select {
			case skipAccountCacheMessage <- struct{}{}:
				// if we got to this point, we should be able to close the writeAccountCacheMessageCompleted channel to have the "completed initializing" message written.
				close(writeAccountCacheMessageCompleted)
			default:
			}
			au.log.Infof("initializeCaches is still initializing account data caches, %d rounds loaded out of %d rounds", blk.Round()-lastBalancesRound, lastestBlockRound-lastBalancesRound)
			lastProgressMessage = time.Now()
		}

		// prepare for the next iteration.
		accLedgerEval.prevHeader = *delta.Hdr
	}

	if blockRetrievalError != nil {
		err = blockRetrievalError
	}
	return
}

// initializeFromDisk performs the atomic operation of loading the accounts data information from disk
// and preparing the accountUpdates for operation.
func (au *accountUpdates) initializeFromDisk(l ledgerForTracker, lastBalancesRound basics.Round) (err error) {
	au.dbs = l.trackerDB()
	au.log = l.trackerLog()
	au.ledger = l

	start := time.Now()
	ledgerAccountsinitCount.Inc(nil)
	err = au.dbs.Wdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		err0 := au.accountsInitializeHashes(ctx, tx, lastBalancesRound)
		if err0 != nil {
			return err0
		}
		totals, err0 := accountsTotals(tx, false)
		if err0 != nil {
			return err0
		}

		au.roundTotals = []ledgercore.AccountTotals{totals}
		return nil
	})

	ledgerAccountsinitMicros.AddMicrosecondsSince(start, nil)
	if err != nil {
		return
	}

	au.accountsq, err = accountsInitDbQueries(au.dbs.Rdb.Handle, au.dbs.Wdb.Handle)
	if err != nil {
		return
	}
	au.lastCatchpointLabel, _, err = au.accountsq.readCatchpointStateString(context.Background(), catchpointStateLastCatchpoint)
	if err != nil {
		return
	}

	hdr, err := l.BlockHdr(lastBalancesRound)
	if err != nil {
		return
	}

	au.versions = []protocol.ConsensusVersion{hdr.CurrentProtocol}
	au.deltas = nil
	au.creatableDeltas = nil
	au.accounts = make(map[basics.Address]modifiedAccount)
	au.creatables = make(map[basics.CreatableIndex]ledgercore.ModifiedCreatable)
	au.deltasAccum = []int{0}
	au.roundDigest = nil

	au.catchpointWriting = 0
	// keep these channel closed if we're not generating catchpoint
	au.catchpointSlowWriting = make(chan struct{}, 1)
	close(au.catchpointSlowWriting)
	au.ctx, au.ctxCancel = context.WithCancel(context.Background())

	au.baseAccounts.init(au.log, baseAccountsPendingAccountsBufferSize, baseAccountsPendingAccountsWarnThreshold)
	return
}

// accountHashBuilder calculates the hash key used for the trie by combining the account address and the account data
func accountHashBuilder(addr basics.Address, accountData basics.AccountData, encodedAccountData []byte) []byte {
	hash := make([]byte, 4+crypto.DigestSize)
	// write out the lowest 32 bits of the reward base. This should improve the caching of the trie by allowing
	// recent updated to be in-cache, and "older" nodes will be left alone.
	for i, rewards := 3, accountData.RewardsBase; i >= 0; i, rewards = i-1, rewards>>8 {
		// the following takes the rewards & 255 -> hash[i]
		hash[i] = byte(rewards)
	}
	entryHash := crypto.Hash(append(addr[:], encodedAccountData[:]...))
	copy(hash[4:], entryHash[:])
	return hash[:]
}

// accountsInitializeHashes initializes account hashes.
// as part of the initialization, it tests if a hash table matches to account base and updates the former.
func (au *accountUpdates) accountsInitializeHashes(ctx context.Context, tx *sql.Tx, rnd basics.Round) error {
	hashRound, err := accountsHashRound(tx)
	if err != nil {
		return err
	}

	if hashRound != rnd {
		// if the hashed round is different then the base round, something was modified, and the accounts aren't in sync
		// with the hashes.
		err = resetAccountHashes(tx)
		if err != nil {
			return err
		}
		// if catchpoint is disabled on this node, we could complete the initialization right here.
		if !au.catchpointEnabled() {
			return nil
		}
	}

	// create the merkle trie for the balances
	committer, err := MakeMerkleCommitter(tx, false)
	if err != nil {
		return fmt.Errorf("accountsInitialize was unable to makeMerkleCommitter: %v", err)
	}

	trie, err := merkletrie.MakeTrie(committer, TrieMemoryConfig)
	if err != nil {
		return fmt.Errorf("accountsInitialize was unable to MakeTrie: %v", err)
	}

	// we might have a database that was previously initialized, and now we're adding the balances trie. In that case, we need to add all the existing balances to this trie.
	// we can figure this out by examining the hash of the root:
	rootHash, err := trie.RootHash()
	if err != nil {
		return fmt.Errorf("accountsInitialize was unable to retrieve trie root hash: %v", err)
	}

	if rootHash.IsZero() {
		au.log.Infof("accountsInitialize rebuilding merkle trie for round %d", rnd)
		accountBuilderIt := makeOrderedAccountsIter(tx, trieRebuildAccountChunkSize)
		defer accountBuilderIt.Close(ctx)
		startTrieBuildTime := time.Now()
		accountsCount := 0
		lastRebuildTime := startTrieBuildTime
		pendingAccounts := 0
		totalOrderedAccounts := 0
		for {
			accts, processedRows, err := accountBuilderIt.Next(ctx)
			if err == sql.ErrNoRows {
				// the account builder would return sql.ErrNoRows when no more data is available.
				break
			} else if err != nil {
				return err
			}

			if len(accts) > 0 {
				accountsCount += len(accts)
				pendingAccounts += len(accts)
				for _, acct := range accts {
					added, err := trie.Add(acct.digest)
					if err != nil {
						return fmt.Errorf("accountsInitialize was unable to add changes to trie: %v", err)
					}
					if !added {
						au.log.Warnf("accountsInitialize attempted to add duplicate hash '%s' to merkle trie for account %v", hex.EncodeToString(acct.digest), acct.address)
					}
				}

				if pendingAccounts >= trieRebuildCommitFrequency {
					// this trie Evict will commit using the current transaction.
					// if anything goes wrong, it will still get rolled back.
					_, err = trie.Evict(true)
					if err != nil {
						return fmt.Errorf("accountsInitialize was unable to commit changes to trie: %v", err)
					}
					pendingAccounts = 0
				}

				if time.Now().Sub(lastRebuildTime) > 5*time.Second {
					// let the user know that the trie is still being rebuilt.
					au.log.Infof("accountsInitialize still building the trie, and processed so far %d accounts", accountsCount)
					lastRebuildTime = time.Now()
				}
			} else if processedRows > 0 {
				totalOrderedAccounts += processedRows
				// if it's not ordered, we can ignore it for now; we'll just increase the counters and emit logs periodically.
				if time.Now().Sub(lastRebuildTime) > 5*time.Second {
					// let the user know that the trie is still being rebuilt.
					au.log.Infof("accountsInitialize still building the trie, and hashed so far %d accounts", totalOrderedAccounts)
					lastRebuildTime = time.Now()
				}
			}
		}

		// this trie Evict will commit using the current transaction.
		// if anything goes wrong, it will still get rolled back.
		_, err = trie.Evict(true)
		if err != nil {
			return fmt.Errorf("accountsInitialize was unable to commit changes to trie: %v", err)
		}

		// we've just updated the merkle trie, update the hashRound to reflect that.
		err = updateAccountsHashRound(tx, rnd)
		if err != nil {
			return fmt.Errorf("accountsInitialize was unable to update the account hash round to %d: %v", rnd, err)
		}

		au.log.Infof("accountsInitialize rebuilt the merkle trie with %d entries in %v", accountsCount, time.Now().Sub(startTrieBuildTime))
	}
	au.balancesTrie = trie
	return nil
}

// deleteStoredCatchpoints iterates over the storedcatchpoints table and deletes all the files stored on disk.
// once all the files have been deleted, it would go ahead and remove the entries from the table.
func deleteStoredCatchpoints(ctx context.Context, dbQueries *accountsDbQueries, dbDirectory string) (err error) {
	catchpointsFilesChunkSize := 50
	for {
		fileNames, err := dbQueries.getOldestCatchpointFiles(ctx, catchpointsFilesChunkSize, 0)
		if err != nil {
			return err
		}
		if len(fileNames) == 0 {
			break
		}

		for round, fileName := range fileNames {
			absCatchpointFileName := filepath.Join(dbDirectory, fileName)
			err = os.Remove(absCatchpointFileName)
			if err == nil || os.IsNotExist(err) {
				// it's ok if the file doesn't exist. just remove it from the database and we'll be good to go.
			} else {
				// we can't delete the file, abort -
				return fmt.Errorf("unable to delete old catchpoint file '%s' : %v", absCatchpointFileName, err)
			}
			// clear the entry from the database
			err = dbQueries.storeCatchpoint(ctx, round, "", "", 0)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// accountsUpdateBalances applies the given compactAccountDeltas to the merkle trie
func (au *accountUpdates) accountsUpdateBalances(accountsDeltas compactAccountDeltas) (err error) {
	if !au.catchpointEnabled() {
		return nil
	}
	var added, deleted bool
	accumulatedChanges := 0

	for i := 0; i < accountsDeltas.len(); i++ {
		addr, delta := accountsDeltas.getByIdx(i)
		if !delta.old.accountData.IsZero() {
			deleteHash := accountHashBuilder(addr, delta.old.accountData, protocol.Encode(&delta.old.accountData))
			deleted, err = au.balancesTrie.Delete(deleteHash)
			if err != nil {
				return fmt.Errorf("failed to delete hash '%s' from merkle trie for account %v: %w", hex.EncodeToString(deleteHash), addr, err)
			}
			if !deleted {
				au.log.Warnf("failed to delete hash '%s' from merkle trie for account %v", hex.EncodeToString(deleteHash), addr)
			} else {
				accumulatedChanges++
			}
		}

		if !delta.new.IsZero() {
			addHash := accountHashBuilder(addr, delta.new, protocol.Encode(&delta.new))
			added, err = au.balancesTrie.Add(addHash)
			if err != nil {
				return fmt.Errorf("attempted to add duplicate hash '%s' to merkle trie for account %v: %w", hex.EncodeToString(addHash), addr, err)
			}
			if !added {
				au.log.Warnf("attempted to add duplicate hash '%s' to merkle trie for account %v", hex.EncodeToString(addHash), addr)
			} else {
				accumulatedChanges++
			}
		}
	}
	if accumulatedChanges >= trieAccumulatedChangesFlush {
		accumulatedChanges = 0
		_, err = au.balancesTrie.Commit()
		if err != nil {
			return
		}
	}

	// write it all to disk.
	if accumulatedChanges > 0 {
		_, err = au.balancesTrie.Commit()
	}

	return
}

// newBlockImpl is the accountUpdates implementation of the ledgerTracker interface. This is the "internal" facing function
// which assumes that no lock need to be taken.
func (au *accountUpdates) newBlockImpl(blk bookkeeping.Block, delta ledgercore.StateDelta) {
	rnd := blk.Round()

	if rnd <= au.latest() {
		// Duplicate, ignore.
		return
	}

	if rnd != au.latest()+1 {
		au.log.Panicf("accountUpdates: newBlockImpl %d too far in the future, dbRound %d, deltas %d", rnd, au.cachedDBRound, len(au.deltas))
	}
	au.deltas = append(au.deltas, delta.Accts)
	au.versions = append(au.versions, blk.CurrentProtocol)
	au.creatableDeltas = append(au.creatableDeltas, delta.Creatables)
	au.roundDigest = append(au.roundDigest, blk.Digest())
	au.deltasAccum = append(au.deltasAccum, delta.Accts.Len()+au.deltasAccum[len(au.deltasAccum)-1])

	au.baseAccounts.flushPendingWrites()

	for i := 0; i < delta.Accts.Len(); i++ {
		addr, data := delta.Accts.GetByIdx(i)
		macct := au.accounts[addr]
		macct.ndeltas++
		macct.data = data
		au.accounts[addr] = macct
	}

	for cidx, cdelta := range delta.Creatables {
		mcreat := au.creatables[cidx]
		mcreat.Creator = cdelta.Creator
		mcreat.Created = cdelta.Created
		mcreat.Ctype = cdelta.Ctype
		mcreat.Ndeltas++
		au.creatables[cidx] = mcreat
	}

	au.roundTotals = append(au.roundTotals, delta.Totals)

	// calling prune would drop old entries from the base accounts.
	newBaseAccountSize := (len(au.accounts) + 1) + baseAccountsPendingAccountsBufferSize
	au.baseAccounts.prune(newBaseAccountSize)

	if au.voters != nil {
		au.voters.newBlock(blk.BlockHeader)
	}
}

// lookupWithRewards returns the account data for a given address at a given round.
// The rewards are added to the AccountData before returning. Note that the function doesn't update the account with the rewards,
// even while it does return the AccountData which represent the "rewarded" account data.
func (au *accountUpdates) lookupWithRewards(rnd basics.Round, addr basics.Address) (data basics.AccountData, err error) {
	au.accountsMu.RLock()
	needUnlock := true
	defer func() {
		if needUnlock {
			au.accountsMu.RUnlock()
		}
	}()
	var offset uint64
	var rewardsProto config.ConsensusParams
	var rewardsLevel uint64
	var persistedData persistedAccountData
	withRewards := true
	for {
		currentDbRound := au.cachedDBRound
		currentDeltaLen := len(au.deltas)
		offset, err = au.roundOffset(rnd)
		if err != nil {
			return
		}

		rewardsProto = config.Consensus[au.versions[offset]]
		rewardsLevel = au.roundTotals[offset].RewardsLevel

		// we're testing the withRewards here and setting the defer function only once, and only if withRewards is true.
		// we want to make this defer only after setting the above rewardsProto/rewardsLevel.
		if withRewards {
			defer func() {
				if err == nil {
					data = data.WithUpdatedRewards(rewardsProto, rewardsLevel)
				}
			}()
			withRewards = false
		}

		// check if we've had this address modified in the past rounds. ( i.e. if it's in the deltas )
		macct, indeltas := au.accounts[addr]
		if indeltas {
			// Check if this is the most recent round, in which case, we can
			// use a cache of the most recent account state.
			if offset == uint64(len(au.deltas)) {
				return macct.data, nil
			}
			// the account appears in the deltas, but we don't know if it appears in the
			// delta range of [0..offset], so we'll need to check :
			// Traverse the deltas backwards to ensure that later updates take
			// priority if present.
			for offset > 0 {
				offset--
				d, ok := au.deltas[offset].Get(addr)
				if ok {
					return d, nil
				}
			}
		}

		// check the baseAccounts -
		if macct, has := au.baseAccounts.read(addr); has && macct.round == currentDbRound {
			// we don't technically need this, since it's already in the baseAccounts, however, writing this over
			// would ensure that we promote this field.
			au.baseAccounts.writePending(macct)
			return macct.accountData, nil
		}

		au.accountsMu.RUnlock()
		needUnlock = false

		// No updates of this account in the in-memory deltas; use on-disk DB.
		// The check in roundOffset() made sure the round is exactly the one
		// present in the on-disk DB.  As an optimization, we avoid creating
		// a separate transaction here, and directly use a prepared SQL query
		// against the database.
		persistedData, err = au.accountsq.lookup(addr)
		if persistedData.round == currentDbRound {
			au.baseAccounts.writePending(persistedData)
			return persistedData.accountData, err
		}

		if persistedData.round < currentDbRound {
			au.log.Errorf("accountUpdates.lookupWithRewards: database round %d is behind in-memory round %d", persistedData.round, currentDbRound)
			return basics.AccountData{}, &StaleDatabaseRoundError{databaseRound: persistedData.round, memoryRound: currentDbRound}
		}
		au.accountsMu.RLock()
		needUnlock = true
		for currentDbRound >= au.cachedDBRound && currentDeltaLen == len(au.deltas) {
			au.accountsReadCond.Wait()
		}
	}
}

// lookupWithoutRewards returns the account data for a given address at a given round.
func (au *accountUpdates) lookupWithoutRewards(rnd basics.Round, addr basics.Address, synchronized bool) (data basics.AccountData, validThrough basics.Round, err error) {
	needUnlock := false
	if synchronized {
		au.accountsMu.RLock()
		needUnlock = true
	}
	defer func() {
		if needUnlock {
			au.accountsMu.RUnlock()
		}
	}()
	var offset uint64
	var persistedData persistedAccountData
	for {
		currentDbRound := au.cachedDBRound
		currentDeltaLen := len(au.deltas)
		offset, err = au.roundOffset(rnd)
		if err != nil {
			return
		}

		// check if we've had this address modified in the past rounds. ( i.e. if it's in the deltas )
		macct, indeltas := au.accounts[addr]
		if indeltas {
			// Check if this is the most recent round, in which case, we can
			// use a cache of the most recent account state.
			if offset == uint64(len(au.deltas)) {
				return macct.data, rnd, nil
			}
			// the account appears in the deltas, but we don't know if it appears in the
			// delta range of [0..offset], so we'll need to check :
			// Traverse the deltas backwards to ensure that later updates take
			// priority if present.
			for offset > 0 {
				offset--
				d, ok := au.deltas[offset].Get(addr)
				if ok {
					// the returned validThrough here is not optimal, but it still correct. We could get a more accurate value by scanning
					// the deltas forward, but this would be time consuming loop, which might not pay off.
					return d, rnd, nil
				}
			}
		} else {
			// we know that the account in not in the deltas - so there is no point in scanning it.
			// we've going to fall back to search in the database, but before doing so, we should
			// update the rnd so that it would point to the end of the known delta range.
			// ( that would give us the best validity range )
			rnd = currentDbRound + basics.Round(currentDeltaLen)
		}

		// check the baseAccounts -
		if macct, has := au.baseAccounts.read(addr); has {
			// we don't technically need this, since it's already in the baseAccounts, however, writing this over
			// would ensure that we promote this field.
			au.baseAccounts.writePending(macct)
			return macct.accountData, rnd, nil
		}

		if synchronized {
			au.accountsMu.RUnlock()
			needUnlock = false
		}
		// No updates of this account in the in-memory deltas; use on-disk DB.
		// The check in roundOffset() made sure the round is exactly the one
		// present in the on-disk DB.  As an optimization, we avoid creating
		// a separate transaction here, and directly use a prepared SQL query
		// against the database.
		persistedData, err = au.accountsq.lookup(addr)
		if persistedData.round == currentDbRound {
			au.baseAccounts.writePending(persistedData)
			return persistedData.accountData, rnd, err
		}
		if synchronized {
			if persistedData.round < currentDbRound {
				au.log.Errorf("accountUpdates.lookupWithoutRewards: database round %d is behind in-memory round %d", persistedData.round, currentDbRound)
				return basics.AccountData{}, basics.Round(0), &StaleDatabaseRoundError{databaseRound: persistedData.round, memoryRound: currentDbRound}
			}
			au.accountsMu.RLock()
			needUnlock = true
			for currentDbRound >= au.cachedDBRound && currentDeltaLen == len(au.deltas) {
				au.accountsReadCond.Wait()
			}
		} else {
			// in non-sync mode, we don't wait since we already assume that we're synchronized.
			au.log.Errorf("accountUpdates.lookupWithoutRewards: database round %d mismatching in-memory round %d", persistedData.round, currentDbRound)
			return basics.AccountData{}, basics.Round(0), &MismatchingDatabaseRoundError{databaseRound: persistedData.round, memoryRound: currentDbRound}
		}
	}
}

// getCreatorForRound returns the asset/app creator for a given asset/app index at a given round
func (au *accountUpdates) getCreatorForRound(rnd basics.Round, cidx basics.CreatableIndex, ctype basics.CreatableType, synchronized bool) (creator basics.Address, ok bool, err error) {
	unlock := false
	if synchronized {
		au.accountsMu.RLock()
		unlock = true
	}
	defer func() {
		if unlock {
			au.accountsMu.RUnlock()
		}
	}()
	var dbRound basics.Round
	var offset uint64
	for {
		currentDbRound := au.cachedDBRound
		currentDeltaLen := len(au.deltas)
		offset, err = au.roundOffset(rnd)
		if err != nil {
			return basics.Address{}, false, err
		}

		// If this is the most recent round, au.creatables has the latest
		// state and we can skip scanning backwards over creatableDeltas
		if offset == uint64(len(au.deltas)) {
			// Check if we already have the asset/creator in cache
			creatableDelta, ok := au.creatables[cidx]
			if ok {
				if creatableDelta.Created && creatableDelta.Ctype == ctype {
					return creatableDelta.Creator, true, nil
				}
				return basics.Address{}, false, nil
			}
		} else {
			for offset > 0 {
				offset--
				creatableDelta, ok := au.creatableDeltas[offset][cidx]
				if ok {
					if creatableDelta.Created && creatableDelta.Ctype == ctype {
						return creatableDelta.Creator, true, nil
					}
					return basics.Address{}, false, nil
				}
			}
		}

		if synchronized {
			au.accountsMu.RUnlock()
			unlock = false
		}
		// Check the database
		creator, ok, dbRound, err = au.accountsq.lookupCreator(cidx, ctype)

		if dbRound == currentDbRound {
			return
		}
		if synchronized {
			if dbRound < currentDbRound {
				au.log.Errorf("accountUpdates.getCreatorForRound: database round %d is behind in-memory round %d", dbRound, currentDbRound)
				return basics.Address{}, false, &StaleDatabaseRoundError{databaseRound: dbRound, memoryRound: currentDbRound}
			}
			au.accountsMu.RLock()
			unlock = true
			for currentDbRound >= au.cachedDBRound && currentDeltaLen == len(au.deltas) {
				au.accountsReadCond.Wait()
			}
		} else {
			au.log.Errorf("accountUpdates.getCreatorForRound: database round %d mismatching in-memory round %d", dbRound, currentDbRound)
			return basics.Address{}, false, &MismatchingDatabaseRoundError{databaseRound: dbRound, memoryRound: currentDbRound}
		}
	}
}

// accountsCreateCatchpointLabel creates a catchpoint label and write it.
func (au *accountUpdates) accountsCreateCatchpointLabel(committedRound basics.Round, totals ledgercore.AccountTotals, ledgerBlockDigest crypto.Digest, trieBalancesHash crypto.Digest) (label string, err error) {
	cpLabel := ledgercore.MakeCatchpointLabel(committedRound, ledgerBlockDigest, trieBalancesHash, totals)
	label = cpLabel.String()
	_, err = au.accountsq.writeCatchpointStateString(context.Background(), catchpointStateLastCatchpoint, label)
	return
}

// roundOffset calculates the offset of the given round compared to the current dbRound. Requires that the lock would be taken.
func (au *accountUpdates) roundOffset(rnd basics.Round) (offset uint64, err error) {
	if rnd < au.cachedDBRound {
		err = &RoundOffsetError{
			round:   rnd,
			dbRound: au.cachedDBRound,
		}
		return
	}

	off := uint64(rnd - au.cachedDBRound)
	if off > uint64(len(au.deltas)) {
		err = fmt.Errorf("round %d too high: dbRound %d, deltas %d", rnd, au.cachedDBRound, len(au.deltas))
		return
	}

	return off, nil
}

func (au *accountUpdates) isCatchpointRound(offset uint64, dbRound basics.Round, lookback basics.Round) bool {
	return ((offset + uint64(lookback+dbRound)) > 0) && (au.catchpointInterval != 0) && ((uint64((offset + uint64(lookback+dbRound))) % au.catchpointInterval) == 0)
}

func (au *accountUpdates) handleUnorderedCommit(offset uint64, dbRound basics.Round, lookback basics.Round) {
	// if this is an archival ledger, we might need to update the catchpointWriting variable.
	if au.archivalLedger {
		// determine if this was a catchpoint round
		if au.isCatchpointRound(offset, dbRound, lookback) {
			// it was a catchpoint round, so update the catchpointWriting to indicate that we're done.
			atomic.StoreInt32(&au.catchpointWriting, 0)
		}
	}
}

// prepareCommit prepares data to write to the database a "chunk" of rounds, and update the cached dbRound accordingly.
func (au *accountUpdates) prepareCommit(dcc *deferredCommitContext) error {
	if au.logAccountUpdatesMetrics {
		now := time.Now()
		if now.Sub(au.lastMetricsLogTime) >= au.logAccountUpdatesInterval {
			dcc.updateStats = true
			au.lastMetricsLogTime = now
		}
	}

	offset := dcc.offset
	dbRound := dcc.oldBase
	lookback := dcc.lookback

	au.accountsMu.RLock()

	dcc.isCatchpointRound = au.isCatchpointRound(offset, dbRound, lookback)

	// create a copy of the deltas, round totals and protos for the range we're going to flush.
	dcc.deltas = make([]ledgercore.AccountDeltas, offset)
	creatableDeltas := make([]map[basics.CreatableIndex]ledgercore.ModifiedCreatable, offset)
	dcc.roundTotals = au.roundTotals[offset]
	copy(dcc.deltas, au.deltas[:offset])
	copy(creatableDeltas, au.creatableDeltas[:offset])

	// verify version correctness : all the entries in the au.versions[1:offset+1] should have the *same* version, and the committedUpTo should be enforcing that.
	if au.versions[1] != au.versions[offset] {
		au.accountsMu.RUnlock()

		// in scheduleCommit, we expect that this function to update the catchpointWriting when
		// it's on a catchpoint round and it's an archival ledger. Doing this in a deferred function
		// here would prevent us from "forgetting" to update this variable later on.
		// The same is repeated in commitRound on errors.
		if dcc.isCatchpointRound && au.archivalLedger {
			atomic.StoreInt32(&au.catchpointWriting, 0)
		}
		return fmt.Errorf("attempted to commit series of rounds with non-uniform consensus versions")
	}

	if dcc.isCatchpointRound {
		dcc.committedRoundDigest = au.roundDigest[offset+uint64(lookback)-1]
	}

	// compact all the deltas - when we're trying to persist multiple rounds, we might have the same account
	// being updated multiple times. When that happen, we can safely omit the intermediate updates.
	dcc.compactAccountDeltas = makeCompactAccountDeltas(dcc.deltas, au.baseAccounts)
	dcc.compactCreatableDeltas = compactCreatableDeltas(creatableDeltas)

	au.accountsMu.RUnlock()

	dcc.genesisProto = au.ledger.GenesisProto()

	if dcc.updateStats {
		dcc.stats.DatabaseCommitDuration = time.Duration(time.Now().UnixNano())
	}

	return nil
}

// commitRound closure is called within the same transaction for all trackers
// it receives current offset and dbRound
func (au *accountUpdates) commitRound(ctx context.Context, tx *sql.Tx, dcc *deferredCommitContext) (err error) {
	treeTargetRound := basics.Round(0)
	offset := dcc.offset
	dbRound := dcc.oldBase

	defer func() {
		if err != nil {
			if dcc.isCatchpointRound && au.archivalLedger {
				atomic.StoreInt32(&au.catchpointWriting, 0)
			}
		}
	}()

	if au.catchpointEnabled() {
		var mc *MerkleCommitter
		mc, err = MakeMerkleCommitter(tx, false)
		if err != nil {
			return
		}

		var trie *merkletrie.Trie
		if au.balancesTrie == nil {
			trie, err = merkletrie.MakeTrie(mc, TrieMemoryConfig)
			if err != nil {
				au.log.Warnf("unable to create merkle trie during committedUpTo: %v", err)
				return err
			}
			au.balancesTrie = trie
		} else {
			au.balancesTrie.SetCommitter(mc)
		}
		treeTargetRound = dbRound + basics.Round(offset)
	}

	_, err = db.ResetTransactionWarnDeadline(ctx, tx, time.Now().Add(accountsUpdatePerRoundHighWatermark*time.Duration(offset)))
	if err != nil {
		return err
	}

	if dcc.updateStats {
		dcc.stats.OldAccountPreloadDuration = time.Duration(time.Now().UnixNano())
	}

	err = dcc.compactAccountDeltas.accountsLoadOld(tx)
	if err != nil {
		return err
	}

	if dcc.updateStats {
		dcc.stats.OldAccountPreloadDuration = time.Duration(time.Now().UnixNano()) - dcc.stats.OldAccountPreloadDuration
	}

	err = accountsPutTotals(tx, dcc.roundTotals, false)
	if err != nil {
		return err
	}

	if dcc.updateStats {
		dcc.stats.MerkleTrieUpdateDuration = time.Duration(time.Now().UnixNano())
	}

	err = au.accountsUpdateBalances(dcc.compactAccountDeltas)
	if err != nil {
		return err
	}

	if dcc.updateStats {
		now := time.Duration(time.Now().UnixNano())
		dcc.stats.MerkleTrieUpdateDuration = now - dcc.stats.MerkleTrieUpdateDuration
		dcc.stats.AccountsWritingDuration = now
	}

	// the updates of the actual account data is done last since the accountsNewRound would modify the compactDeltas old values
	// so that we can update the base account back.
	dcc.updatedPersistedAccounts, err = accountsNewRound(tx, dcc.compactAccountDeltas, dcc.compactCreatableDeltas, dcc.genesisProto, dbRound+basics.Round(offset))
	if err != nil {
		return err
	}

	if dcc.updateStats {
		dcc.stats.AccountsWritingDuration = time.Duration(time.Now().UnixNano()) - dcc.stats.AccountsWritingDuration
	}

	err = updateAccountsHashRound(tx, treeTargetRound)
	if err != nil {
		return err
	}

	if dcc.isCatchpointRound {
		dcc.trieBalancesHash, err = au.balancesTrie.RootHash()
		if err != nil {
			return err
		}
	}

	return
}

func (au *accountUpdates) postCommit(dcc deferredCommitContext) {
	if dcc.updateStats {
		spentDuration := dcc.stats.DatabaseCommitDuration + dcc.stats.AccountsWritingDuration + dcc.stats.MerkleTrieUpdateDuration + dcc.stats.OldAccountPreloadDuration
		dcc.stats.DatabaseCommitDuration = time.Duration(time.Now().UnixNano()) - spentDuration
	}

	offset := dcc.offset
	dbRound := dcc.oldBase
	lookback := dcc.lookback
	newBase := dcc.newBase

	var catchpointLabel string
	var err error
	if dcc.isCatchpointRound {
		catchpointLabel, err = au.accountsCreateCatchpointLabel(dbRound+basics.Round(offset)+lookback, dcc.roundTotals, dcc.committedRoundDigest, dcc.trieBalancesHash)
		if err != nil {
			au.log.Warnf("commitRound : unable to create a catchpoint label: %v", err)
		}
	}
	if au.balancesTrie != nil {
		_, err = au.balancesTrie.Evict(false)
		if err != nil {
			au.log.Warnf("merkle trie failed to evict: %v", err)
		}
	}

	if dcc.isCatchpointRound && catchpointLabel != "" {
		au.lastCatchpointLabel = catchpointLabel
	}
	updatingBalancesDuration := time.Since(dcc.flushTime)

	if dcc.updateStats {
		dcc.stats.MemoryUpdatesDuration = time.Duration(time.Now().UnixNano())
	}

	au.accountsMu.Lock()
	// Drop reference counts to modified accounts, and evict them
	// from in-memory cache when no references remain.
	for i := 0; i < dcc.compactAccountDeltas.len(); i++ {
		addr, acctUpdate := dcc.compactAccountDeltas.getByIdx(i)
		cnt := acctUpdate.ndeltas
		macct, ok := au.accounts[addr]
		if !ok {
			au.log.Panicf("inconsistency: flushed %d changes to %s, but not in au.accounts", cnt, addr)
		}

		if cnt > macct.ndeltas {
			au.log.Panicf("inconsistency: flushed %d changes to %s, but au.accounts had %d", cnt, addr, macct.ndeltas)
		} else if cnt == macct.ndeltas {
			delete(au.accounts, addr)
		} else {
			macct.ndeltas -= cnt
			au.accounts[addr] = macct
		}
	}

	for _, persistedAcct := range dcc.updatedPersistedAccounts {
		au.baseAccounts.write(persistedAcct)
	}

	for cidx, modCrt := range dcc.compactCreatableDeltas {
		cnt := modCrt.Ndeltas
		mcreat, ok := au.creatables[cidx]
		if !ok {
			au.log.Panicf("inconsistency: flushed %d changes to creatable %d, but not in au.creatables", cnt, cidx)
		}

		if cnt > mcreat.Ndeltas {
			au.log.Panicf("inconsistency: flushed %d changes to creatable %d, but au.creatables had %d", cnt, cidx, mcreat.Ndeltas)
		} else if cnt == mcreat.Ndeltas {
			delete(au.creatables, cidx)
		} else {
			mcreat.Ndeltas -= cnt
			au.creatables[cidx] = mcreat
		}
	}

	au.deltas = au.deltas[offset:]
	au.deltasAccum = au.deltasAccum[offset:]
	au.roundDigest = au.roundDigest[offset:]
	au.versions = au.versions[offset:]
	au.roundTotals = au.roundTotals[offset:]
	au.creatableDeltas = au.creatableDeltas[offset:]
	au.cachedDBRound = newBase
	au.lastFlushTime = dcc.flushTime

	au.accountsMu.Unlock()

	if dcc.updateStats {
		dcc.stats.MemoryUpdatesDuration = time.Duration(time.Now().UnixNano()) - dcc.stats.MemoryUpdatesDuration
	}

	au.accountsReadCond.Broadcast()

	if dcc.isCatchpointRound && au.archivalLedger && catchpointLabel != "" {
		// generate the catchpoint file. This need to be done inline so that it will block any new accounts that from being written.
		// the generateCatchpoint expects that the accounts data would not be modified in the background during it's execution.
		au.generateCatchpoint(basics.Round(offset)+dbRound+lookback, catchpointLabel, dcc.committedRoundDigest, updatingBalancesDuration)
	}

	// log telemetry event
	if dcc.updateStats {
		dcc.stats.StartRound = uint64(dbRound)
		dcc.stats.RoundsCount = offset
		dcc.stats.UpdatedAccountsCount = uint64(len(dcc.updatedPersistedAccounts))
		dcc.stats.UpdatedCreatablesCount = uint64(len(dcc.compactCreatableDeltas))

		var details struct{}
		au.log.Metrics(telemetryspec.Accounts, dcc.stats, details)
	}

	// in scheduleCommit, we expect that this function to update the catchpointWriting when
	// it's on a catchpoint round and it's an archival ledger. Doing this in a deferred function
	// here would prevent us from "forgetting" to update this variable later on.
	if dcc.isCatchpointRound && au.archivalLedger {
		atomic.StoreInt32(&au.catchpointWriting, 0)
	}
}

// compactCreatableDeltas takes an array of creatables map deltas ( one array entry per round ), and compact the array into a single
// map that contains all the deltas changes. While doing that, the function eliminate any intermediate changes.
// It counts the number of changes per round by specifying it in the ndeltas field of the modifiedCreatable.
func compactCreatableDeltas(creatableDeltas []map[basics.CreatableIndex]ledgercore.ModifiedCreatable) (outCreatableDeltas map[basics.CreatableIndex]ledgercore.ModifiedCreatable) {
	if len(creatableDeltas) == 0 {
		return
	}
	// the sizes of the maps here aren't super accurate, but would hopefully be a rough estimate for a reasonable starting point.
	outCreatableDeltas = make(map[basics.CreatableIndex]ledgercore.ModifiedCreatable, 1+len(creatableDeltas[0])*len(creatableDeltas))
	for _, roundCreatable := range creatableDeltas {
		for creatableIdx, creatable := range roundCreatable {
			if prev, has := outCreatableDeltas[creatableIdx]; has {
				outCreatableDeltas[creatableIdx] = ledgercore.ModifiedCreatable{
					Ctype:   creatable.Ctype,
					Created: creatable.Created,
					Creator: creatable.Creator,
					Ndeltas: prev.Ndeltas + 1,
				}
			} else {
				outCreatableDeltas[creatableIdx] = ledgercore.ModifiedCreatable{
					Ctype:   creatable.Ctype,
					Created: creatable.Created,
					Creator: creatable.Creator,
					Ndeltas: 1,
				}
			}
		}
	}
	return
}

// latest returns the latest round
func (au *accountUpdates) latest() basics.Round {
	return au.cachedDBRound + basics.Round(len(au.deltas))
}

// generateCatchpoint generates a single catchpoint file
func (au *accountUpdates) generateCatchpoint(committedRound basics.Round, label string, committedRoundDigest crypto.Digest, updatingBalancesDuration time.Duration) {
	beforeGeneratingCatchpointTime := time.Now()
	catchpointGenerationStats := telemetryspec.CatchpointGenerationEventDetails{
		BalancesWriteTime: uint64(updatingBalancesDuration.Nanoseconds()),
	}

	// the retryCatchpointCreation is used to repeat the catchpoint file generation in case the node crashed / aborted during startup
	// before the catchpoint file generation could be completed.
	retryCatchpointCreation := false
	au.log.Debugf("accountUpdates: generateCatchpoint: generating catchpoint for round %d", committedRound)
	defer func() {
		if !retryCatchpointCreation {
			// clear the writingCatchpoint flag
			_, err := au.accountsq.writeCatchpointStateUint64(context.Background(), catchpointStateWritingCatchpoint, uint64(0))
			if err != nil {
				au.log.Warnf("accountUpdates: generateCatchpoint unable to clear catchpoint state '%s' for round %d: %v", catchpointStateWritingCatchpoint, committedRound, err)
			}
		}
	}()

	_, err := au.accountsq.writeCatchpointStateUint64(context.Background(), catchpointStateWritingCatchpoint, uint64(committedRound))
	if err != nil {
		au.log.Warnf("accountUpdates: generateCatchpoint unable to write catchpoint state '%s' for round %d: %v", catchpointStateWritingCatchpoint, committedRound, err)
		return
	}

	relCatchpointFileName := filepath.Join("catchpoints", catchpointRoundToPath(committedRound))
	absCatchpointFileName := filepath.Join(au.dbDirectory, relCatchpointFileName)

	more := true
	const shortChunkExecutionDuration = 50 * time.Millisecond
	const longChunkExecutionDuration = 1 * time.Second
	var chunkExecutionDuration time.Duration
	select {
	case <-au.catchpointSlowWriting:
		chunkExecutionDuration = longChunkExecutionDuration
	default:
		chunkExecutionDuration = shortChunkExecutionDuration
	}

	var catchpointWriter *catchpointWriter
	start := time.Now()
	ledgerGeneratecatchpointCount.Inc(nil)
	err = au.dbs.Rdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		catchpointWriter = makeCatchpointWriter(au.ctx, absCatchpointFileName, tx, committedRound, committedRoundDigest, label)
		for more {
			stepCtx, stepCancelFunction := context.WithTimeout(au.ctx, chunkExecutionDuration)
			writeStepStartTime := time.Now()
			more, err = catchpointWriter.WriteStep(stepCtx)
			// accumulate the actual time we've spent writing in this step.
			catchpointGenerationStats.CPUTime += uint64(time.Now().Sub(writeStepStartTime).Nanoseconds())
			stepCancelFunction()
			if more && err == nil {
				// we just wrote some data, but there is more to be written.
				// go to sleep for while.
				// before going to sleep, extend the transaction timeout so that we won't get warnings:
				db.ResetTransactionWarnDeadline(ctx, tx, time.Now().Add(1*time.Second))
				select {
				case <-time.After(100 * time.Millisecond):
					// increase the time slot allocated for writing the catchpoint, but stop when we get to the longChunkExecutionDuration limit.
					// this would allow the catchpoint writing speed to ramp up while still leaving some cpu available.
					chunkExecutionDuration *= 2
					if chunkExecutionDuration > longChunkExecutionDuration {
						chunkExecutionDuration = longChunkExecutionDuration
					}
				case <-au.ctx.Done():
					retryCatchpointCreation = true
					err2 := catchpointWriter.Abort()
					if err2 != nil {
						return fmt.Errorf("error removing catchpoint file : %v", err2)
					}
					return nil
				case <-au.catchpointSlowWriting:
					chunkExecutionDuration = longChunkExecutionDuration
				}
			}
			if err != nil {
				err = fmt.Errorf("unable to create catchpoint : %v", err)
				err2 := catchpointWriter.Abort()
				if err2 != nil {
					au.log.Warnf("accountUpdates: generateCatchpoint: error removing catchpoint file : %v", err2)
				}
				return
			}
		}
		return
	})
	ledgerGeneratecatchpointMicros.AddMicrosecondsSince(start, nil)

	if err != nil {
		au.log.Warnf("accountUpdates: generateCatchpoint: %v", err)
		return
	}
	if catchpointWriter == nil {
		au.log.Warnf("accountUpdates: generateCatchpoint: nil catchpointWriter")
		return
	}

	err = au.saveCatchpointFile(committedRound, relCatchpointFileName, catchpointWriter.GetSize(), catchpointWriter.GetCatchpoint())
	if err != nil {
		au.log.Warnf("accountUpdates: generateCatchpoint: unable to save catchpoint: %v", err)
		return
	}
	catchpointGenerationStats.FileSize = uint64(catchpointWriter.GetSize())
	catchpointGenerationStats.WritingDuration = uint64(time.Now().Sub(beforeGeneratingCatchpointTime).Nanoseconds())
	catchpointGenerationStats.AccountsCount = catchpointWriter.GetTotalAccounts()
	catchpointGenerationStats.CatchpointLabel = catchpointWriter.GetCatchpoint()
	au.log.EventWithDetails(telemetryspec.Accounts, telemetryspec.CatchpointGenerationEvent, catchpointGenerationStats)
	au.log.With("writingDuration", catchpointGenerationStats.WritingDuration).
		With("CPUTime", catchpointGenerationStats.CPUTime).
		With("balancesWriteTime", catchpointGenerationStats.BalancesWriteTime).
		With("accountsCount", catchpointGenerationStats.AccountsCount).
		With("fileSize", catchpointGenerationStats.FileSize).
		With("catchpointLabel", catchpointGenerationStats.CatchpointLabel).
		Infof("Catchpoint file was generated")
}

// catchpointRoundToPath calculate the catchpoint file path for a given round
func catchpointRoundToPath(rnd basics.Round) string {
	irnd := int64(rnd) / 256
	outStr := ""
	for irnd > 0 {
		outStr = filepath.Join(outStr, fmt.Sprintf("%02x", irnd%256))
		irnd = irnd / 256
	}
	outStr = filepath.Join(outStr, strconv.FormatInt(int64(rnd), 10)+".catchpoint")
	return outStr
}

// saveCatchpointFile stores the provided fileName as the stored catchpoint for the given round.
// after a successful insert operation to the database, it would delete up to 2 old entries, as needed.
// deleting 2 entries while inserting single entry allow us to adjust the size of the backing storage and have the
// database and storage realign.
func (au *accountUpdates) saveCatchpointFile(round basics.Round, fileName string, fileSize int64, catchpoint string) (err error) {
	if au.catchpointFileHistoryLength != 0 {
		err = au.accountsq.storeCatchpoint(context.Background(), round, fileName, catchpoint, fileSize)
		if err != nil {
			au.log.Warnf("accountUpdates: saveCatchpoint: unable to save catchpoint: %v", err)
			return
		}
	} else {
		err = os.Remove(fileName)
		if err != nil {
			au.log.Warnf("accountUpdates: saveCatchpoint: unable to remove file (%s): %v", fileName, err)
			return
		}
	}
	if au.catchpointFileHistoryLength == -1 {
		return
	}
	var filesToDelete map[basics.Round]string
	filesToDelete, err = au.accountsq.getOldestCatchpointFiles(context.Background(), 2, au.catchpointFileHistoryLength)
	if err != nil {
		return fmt.Errorf("unable to delete catchpoint file, getOldestCatchpointFiles failed : %v", err)
	}
	for round, fileToDelete := range filesToDelete {
		absCatchpointFileName := filepath.Join(au.dbDirectory, fileToDelete)
		err = os.Remove(absCatchpointFileName)
		if err == nil || os.IsNotExist(err) {
			// it's ok if the file doesn't exist. just remove it from the database and we'll be good to go.
			err = nil
		} else {
			// we can't delete the file, abort -
			return fmt.Errorf("unable to delete old catchpoint file '%s' : %v", absCatchpointFileName, err)
		}
		err = au.accountsq.storeCatchpoint(context.Background(), round, "", "", 0)
		if err != nil {
			return fmt.Errorf("unable to delete old catchpoint entry '%s' : %v", fileToDelete, err)
		}
	}
	return
}

// the vacuumDatabase performs a full vacuum of the accounts database.
func (au *accountUpdates) vacuumDatabase(ctx context.Context) (err error) {
	// vaccumming the database would modify the some of the tables rowid, so we need to make sure any stored in-memory
	// rowid are flushed.
	au.baseAccounts.prune(0)

	startTime := time.Now()
	vacuumExitCh := make(chan struct{}, 1)
	vacuumLoggingAbort := sync.WaitGroup{}
	vacuumLoggingAbort.Add(1)
	// vacuuming the database can take a while. A long while. We want to have a logging function running in a separate go-routine that would log the progress to the log file.
	// also, when we're done vacuuming, we should sent an event notifying of the total time it took to vacuum the database.
	go func() {
		defer vacuumLoggingAbort.Done()
		au.log.Infof("Vacuuming accounts database started")
		for {
			select {
			case <-time.After(5 * time.Second):
				au.log.Infof("Vacuuming accounts database in progress")
			case <-vacuumExitCh:
				return
			}
		}
	}()

	ledgerVacuumCount.Inc(nil)
	vacuumStats, err := au.dbs.Wdb.Vacuum(ctx)
	close(vacuumExitCh)
	vacuumLoggingAbort.Wait()

	if err != nil {
		au.log.Warnf("Vacuuming account database failed : %v", err)
		return err
	}
	vacuumElapsedTime := time.Now().Sub(startTime)
	ledgerVacuumMicros.AddUint64(uint64(vacuumElapsedTime.Microseconds()), nil)

	au.log.Infof("Vacuuming accounts database completed within %v, reducing number of pages from %d to %d and size from %d to %d", vacuumElapsedTime, vacuumStats.PagesBefore, vacuumStats.PagesAfter, vacuumStats.SizeBefore, vacuumStats.SizeAfter)

	vacuumTelemetryStats := telemetryspec.BalancesAccountVacuumEventDetails{
		VacuumTimeNanoseconds:  vacuumElapsedTime.Nanoseconds(),
		BeforeVacuumPageCount:  vacuumStats.PagesBefore,
		AfterVacuumPageCount:   vacuumStats.PagesAfter,
		BeforeVacuumSpaceBytes: vacuumStats.SizeBefore,
		AfterVacuumSpaceBytes:  vacuumStats.SizeAfter,
	}

	au.log.EventWithDetails(telemetryspec.Accounts, telemetryspec.BalancesAccountVacuumEvent, vacuumTelemetryStats)
	return
}

var ledgerAccountsonlinetopCount = metrics.NewCounter("ledger_accountsonlinetop_count", "calls")
var ledgerAccountsonlinetopMicros = metrics.NewCounter("ledger_accountsonlinetop_micros", "µs spent")
var ledgerGetcatchpointCount = metrics.NewCounter("ledger_getcatchpoint_count", "calls")
var ledgerGetcatchpointMicros = metrics.NewCounter("ledger_getcatchpoint_micros", "µs spent")
var ledgerAccountsinitCount = metrics.NewCounter("ledger_accountsinit_count", "calls")
var ledgerAccountsinitMicros = metrics.NewCounter("ledger_accountsinit_micros", "µs spent")
var ledgerCommitroundCount = metrics.NewCounter("ledger_commitround_count", "calls")
var ledgerCommitroundMicros = metrics.NewCounter("ledger_commitround_micros", "µs spent")
var ledgerGeneratecatchpointCount = metrics.NewCounter("ledger_generatecatchpoint_count", "calls")
var ledgerGeneratecatchpointMicros = metrics.NewCounter("ledger_generatecatchpoint_micros", "µs spent")
var ledgerVacuumCount = metrics.NewCounter("ledger_vacuum_count", "calls")
var ledgerVacuumMicros = metrics.NewCounter("ledger_vacuum_micros", "µs spent")
