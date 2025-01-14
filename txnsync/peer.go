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

package txnsync

import (
	"math"
	"sort"
	"time"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/pooldata"
	"github.com/algorand/go-algorand/data/transactions"
)

//msgp:ignore peerState
type peerState int

//msgp:ignore peersOps
type peersOps int

//msgp:ignore messageConstructionOps
type messageConstructionOps int

const maxIncomingBloomFilterHistory = 200

// shortTermRecentTransactionsSentBufferLength is the size of the short term storage for the recently sent transaction ids.
// it should be configured sufficiently high so that any number of transaction sent would not exceed that number before
// the other peer has a chance of sending a feedback. ( when the feedback is received, we will store these IDs into the long-term cache )
const shortTermRecentTransactionsSentBufferLength = 5000

// pendingUnconfirmedRemoteMessages is the number of messages we would cache before receiving a feedback from the other
// peer that these message have been accepted. The general guideline here is that if we have a message every 200ms on one side
// and a message every 20ms on the other, then the ratio of 200/20 = 10, should be the number of required messages (min).
const pendingUnconfirmedRemoteMessages = 20

// longTermRecentTransactionsSentBufferLength is the size of the long term transaction id cache.
const longTermRecentTransactionsSentBufferLength = 15000
const minDataExchangeRateThreshold = 500 * 1024            // 500KB/s, which is ~3.9Mbps
const maxDataExchangeRateThreshold = 100 * 1024 * 1024 / 8 // 100Mbps
const defaultDataExchangeRate = minDataExchangeRateThreshold
const defaultRelayToRelayDataExchangeRate = 10 * 1024 * 1024 / 8 // 10Mbps
const bloomFilterRetryCount = 3                                  // number of bloom filters we would try against each transaction group before skipping it.
const maxTransactionGroupTrackers = 15                           // number of different bloom filter parameters we store before rolling over

const (
	// peerStateStartup is before the timeout for the sending the first message to the peer has reached.
	// for an outgoing peer, it means that an incoming message arrived, and one or more messages need to be sent out.
	peerStateStartup peerState = iota
	// peerStateHoldsoff is set once a message was sent to a peer, and we're holding off before sending additional messages.
	peerStateHoldsoff
	// peerStateInterrupt is set once the holdoff period for the peer have expired.
	peerStateInterrupt
	// peerStateLateBloom is set for outgoing peers on relays, indicating that the next message should be a bloom filter only message.
	peerStateLateBloom

	peerOpsSendMessage        peersOps = 1
	peerOpsSetInterruptible   peersOps = 2
	peerOpsClearInterruptible peersOps = 4
	peerOpsReschedule         peersOps = 8

	messageConstBloomFilter         messageConstructionOps = 1
	messageConstTransactions        messageConstructionOps = 2
	messageConstNextMinDelay        messageConstructionOps = 4
	messageConstUpdateRequestParams messageConstructionOps = 8

	// defaultSignificantMessageThreshold is the minimal transmitted message size which would be used for recalculating the
	// data exchange rate.
	defaultSignificantMessageThreshold = 50000
)

// incomingBloomFilter stores an incoming bloom filter, along with the associated round number.
// the round number allow us to prune filters from rounds n-2 and below.
type incomingBloomFilter struct {
	filter *testableBloomFilter
	round  basics.Round
}

// Peer contains peer-related data which extends the data "known" and managed by the network package.
type Peer struct {
	// networkPeer is the network package exported peer. It's created on construction and never change afterward.
	networkPeer interface{}
	// isOutgoing defines whether the peer is an outgoing peer or not. For relays, this is meaningful as these have
	// slightly different message timing logic.
	isOutgoing bool
	// significantMessageThreshold is the minimal transmitted message size which would be used for recalculating the
	// data exchange rate. When significantMessageThreshold is equal to math.MaxUint64, no data exchange rate updates would be
	// performed.
	significantMessageThreshold uint64
	// state defines the peer state ( in terms of state machine state ). It's touched only by the sync main state machine
	state peerState

	log Logger

	// lastRound is the latest round reported by the peer.
	lastRound basics.Round

	// incomingMessages contains the incoming messages from this peer. This heap help us to reorder the incoming messages so that
	// we could process them in the tcp-transport order.
	incomingMessages messageOrderingHeap

	// nextReceivedMessageSeq is a counter containing the next message sequence number that we expect to see from this peer.
	nextReceivedMessageSeq uint64 // the next message seq that we expect to receive from that peer; implies that all previous messages have been accepted.

	// recentIncomingBloomFilters contains the recent list of bloom filters sent from the peer. When considering sending transactions, we check this
	// array to determine if the peer already has this message.
	recentIncomingBloomFilters []incomingBloomFilter

	// recentSentTransactions contains the recently sent transactions. It's needed since we don't want to rely on the other peer's bloom filter while
	// sending back-to-back messages.
	recentSentTransactions *transactionCache
	// recentSentTransactionsRound is the round associated with the cache of recently sent transactions. We keep this variable around so that we can
	// flush the cache on every round so that we can give pending transaction another chance of being transmitted.
	recentSentTransactionsRound basics.Round

	// these two fields describe "what does that peer asked us to send it"
	requestedTransactionsModulator byte
	requestedTransactionsOffset    byte

	// lastSentMessageSequenceNumber is the last sequence number of the message that we sent.
	lastSentMessageSequenceNumber uint64
	// lastSentMessageRound is the round the last sent message was sent on. The timestamps are relative to the beginning of the round
	// and therefore need to be evaluated togather.
	lastSentMessageRound basics.Round
	// lastSentMessageTimestamp the timestamp at which the last message was sent.
	lastSentMessageTimestamp time.Duration
	// lastSentMessageSize is the encoded message size of the last sent message
	lastSentMessageSize int
	// lastSentBloomFilter is the last bloom filter that was sent to this peer.
	// This bloom filter could be stale if no bloom filter was included in the last message.
	lastSentBloomFilter bloomFilter

	// sentFilterParams records the Round and max txn group counter of the last filter sent to a peer (for each {Modulator,Offset}).
	// From this an efficient next filter can be calculated for just the new txns, or a full filter after a Round turnover.
	sentFilterParams sentFilters

	// lastConfirmedMessageSeqReceived is the last message sequence number that was confirmed by the peer to have been accepted.
	lastConfirmedMessageSeqReceived    uint64
	lastReceivedMessageLocalRound      basics.Round
	lastReceivedMessageTimestamp       time.Duration
	lastReceivedMessageSize            int
	lastReceivedMessageNextMsgMinDelay time.Duration

	// dataExchangeRate is the combined upload/download rate in bytes/second
	dataExchangeRate uint64
	// cachedLatency is the measured network latency of a peer, updated every round
	cachedLatency time.Duration

	// these two fields describe "what does the local peer want the remote peer to send back"
	localTransactionsModulator  byte
	localTransactionsBaseOffset byte

	// lastTransactionSelectionTracker tracks the last transaction group counter that we've evaluated on the selectPendingTransactions method.
	// it used to ensure that on subsequent calls, we won't need to scan the entire pending transactions array from the beginning.
	// the implementation here is breaking it up per request params, so that we can apply the above logic per request params ( i.e. different
	// offset/modulator ), as well as add retry attempts for multiple bloom filters.
	lastTransactionSelectionTracker transactionGroupCounterTracker

	// nextStateTimestamp indicates the next timestamp where the peer state would need to be changed.
	// it used to allow sending partial message while retaining the "next-beta time", or, in the case of outgoing relays,
	// its being used to hold when we need to send the last (bloom) message.
	nextStateTimestamp time.Duration
	// messageSeriesPendingTransactions contain the transactions we are sending in the current "message-series". It allows us to pick a given
	// "snapshot" from the transaction pool, and send that "snapshot" to completion before attempting to re-iterate.
	messageSeriesPendingTransactions []pooldata.SignedTxGroup

	// transactionPoolAckCh is passed to the transaction handler when incoming transaction arrives. The channel is passed upstream, so that once
	// a transaction is added to the transaction pool, we can get some feedback for that.
	transactionPoolAckCh chan uint64

	// transactionPoolAckMessages maintain a list of the recent incoming messages sequence numbers whose transactions were added fully to the transaction
	// pool. This list is being flushed out every time we send a message to the peer.
	transactionPoolAckMessages []uint64

	// used by the selectPendingTransactions method, the lastSelectedTransactionsCount contains the number of entries selected on the previous iteration.
	// this value is used to optimize the memory preallocation for the selection IDs array.
	lastSelectedTransactionsCount int
}

// requestParamsGroupCounterState stores the latest group counters for a given set of request params.
// we use this to ensure we can have multiple iteration of bloom filter scanning over each individual
// transaction group. This method allow us to reduce the bloom filter errors while avoid scanning the
// list of transactions redundently.
//msgp:ignore transactionGroupCounterState
type requestParamsGroupCounterState struct {
	offset        byte
	modulator     byte
	groupCounters [bloomFilterRetryCount]uint64
}

// transactionGroupCounterTracker manages the group counter state for each request param.
//msgp:ignore transactionGroupCounterTracker
type transactionGroupCounterTracker []requestParamsGroupCounterState

// get returns the group counter for a given set of request param.
func (t *transactionGroupCounterTracker) get(offset, modulator byte) uint64 {
	i := t.index(offset, modulator)
	if i >= 0 {
		return (*t)[i].groupCounters[0]
	}
	return 0
}

// set updates the group counter for a given set of request param. If no such request
// param currently exists, it create it.
func (t *transactionGroupCounterTracker) set(offset, modulator byte, counter uint64) {
	i := t.index(offset, modulator)
	if i >= 0 {
		(*t)[i].groupCounters[0] = counter
		return
	}
	// if it doesn't exists -
	state := requestParamsGroupCounterState{
		offset:    offset,
		modulator: modulator,
	}
	state.groupCounters[0] = counter

	if len(*t) == maxTransactionGroupTrackers {
		// shift all entries by one.
		copy((*t)[0:], (*t)[1:])
		(*t)[maxTransactionGroupTrackers-1] = state
	} else {
		*t = append(*t, state)
	}
}

// roll the counters for a given requests params, so that we would go back and
// rescan some of the previous transaction groups ( but not all !) when selectPendingTransactions is called.
func (t *transactionGroupCounterTracker) roll(offset, modulator byte) {
	i := t.index(offset, modulator)
	if i < 0 {
		return
	}

	if (*t)[i].groupCounters[1] >= (*t)[i].groupCounters[0] {
		return
	}
	firstGroupCounter := (*t)[i].groupCounters[0]
	copy((*t)[i].groupCounters[0:], (*t)[i].groupCounters[1:])
	(*t)[i].groupCounters[bloomFilterRetryCount-1] = firstGroupCounter
}

// index is a helper method for the transactionGroupCounterTracker, helping to locate the index of
// a requestParamsGroupCounterState in the array that matches the provided request params. The method
// uses a linear search, which works best against small arrays.
func (t *transactionGroupCounterTracker) index(offset, modulator byte) int {
	for i, counter := range *t {
		if counter.offset == offset && counter.modulator == modulator {
			return i
		}
	}
	return -1
}

func makePeer(networkPeer interface{}, isOutgoing bool, isLocalNodeRelay bool, cfg *config.Local, log Logger, latency time.Duration) *Peer {
	p := &Peer{
		networkPeer:                 networkPeer,
		isOutgoing:                  isOutgoing,
		recentSentTransactions:      makeTransactionCache(shortTermRecentTransactionsSentBufferLength, longTermRecentTransactionsSentBufferLength, pendingUnconfirmedRemoteMessages),
		dataExchangeRate:            defaultDataExchangeRate,
		cachedLatency:               latency,
		transactionPoolAckCh:        make(chan uint64, maxAcceptedMsgSeq),
		transactionPoolAckMessages:  make([]uint64, 0, maxAcceptedMsgSeq),
		significantMessageThreshold: defaultSignificantMessageThreshold,
		log:                         log,
	}
	if isLocalNodeRelay {
		p.requestedTransactionsModulator = 1
		p.dataExchangeRate = defaultRelayToRelayDataExchangeRate
	}
	if cfg.TransactionSyncDataExchangeRate > 0 {
		p.dataExchangeRate = cfg.TransactionSyncDataExchangeRate
		p.significantMessageThreshold = math.MaxUint64
	}
	if cfg.TransactionSyncSignificantMessageThreshold > 0 && cfg.TransactionSyncDataExchangeRate == 0 {
		p.significantMessageThreshold = cfg.TransactionSyncSignificantMessageThreshold
	}
	// increase the number of total created peers.
	txsyncCreatedPeersTotal.Inc(nil)
	return p
}

// GetNetworkPeer returns the network peer associated with this particular peer.
func (p *Peer) GetNetworkPeer() interface{} {
	return p.networkPeer
}

// GetTransactionPoolAckChannel returns the transaction pool ack channel
func (p *Peer) GetTransactionPoolAckChannel() chan uint64 {
	return p.transactionPoolAckCh
}

// dequeuePendingTransactionPoolAckMessages removed the pending entries from transactionPoolAckCh and add them to transactionPoolAckMessages
func (p *Peer) dequeuePendingTransactionPoolAckMessages() {
	for {
		select {
		case msgSeq := <-p.transactionPoolAckCh:
			if len(p.transactionPoolAckMessages) == maxAcceptedMsgSeq {
				p.transactionPoolAckMessages = append(p.transactionPoolAckMessages[1:], msgSeq)
			} else {
				p.transactionPoolAckMessages = append(p.transactionPoolAckMessages, msgSeq)
			}
		default:
			return
		}
	}
}

// outgoing related methods :

// getAcceptedMessages returns the content of the transactionPoolAckMessages and clear the existing buffer.
func (p *Peer) getAcceptedMessages() []uint64 {
	p.dequeuePendingTransactionPoolAckMessages()
	acceptedMessages := p.transactionPoolAckMessages
	p.transactionPoolAckMessages = make([]uint64, 0, maxAcceptedMsgSeq)
	return acceptedMessages
}

func (p *Peer) selectPendingTransactions(pendingTransactions []pooldata.SignedTxGroup, sendWindow time.Duration, round basics.Round, bloomFilterSize int) (selectedTxns []pooldata.SignedTxGroup, selectedTxnIDs []transactions.Txid, partialTransactionsSet bool) {
	// if peer is too far back, don't send it any transactions ( or if the peer is not interested in transactions )
	if p.lastRound < round.SubSaturate(1) || p.requestedTransactionsModulator == 0 {
		return nil, nil, false
	}

	if len(p.messageSeriesPendingTransactions) > 0 {
		pendingTransactions = p.messageSeriesPendingTransactions
	}

	if len(pendingTransactions) == 0 {
		return nil, nil, false
	}

	// flush the recent sent transaction cache on the beginning of a new round to give pending transactions another
	// chance of being transmitted.
	if p.recentSentTransactionsRound != round {
		p.recentSentTransactions.reset()
		p.recentSentTransactionsRound = round
	}

	windowLengthBytes := int(uint64(sendWindow) * p.dataExchangeRate / uint64(time.Second))
	windowLengthBytes -= bloomFilterSize

	accumulatedSize := 0

	lastTransactionSelectionGroupCounter := p.lastTransactionSelectionTracker.get(p.requestedTransactionsOffset, p.requestedTransactionsModulator)

	startIndex := sort.Search(len(pendingTransactions), func(i int) bool {
		return pendingTransactions[i].GroupCounter >= lastTransactionSelectionGroupCounter
	})

	selectedIDsSliceLength := len(pendingTransactions) - startIndex
	if selectedIDsSliceLength > p.lastSelectedTransactionsCount*2 {
		selectedIDsSliceLength = p.lastSelectedTransactionsCount * 2
	}
	selectedTxnIDs = make([]transactions.Txid, 0, selectedIDsSliceLength)
	selectedTxns = make([]pooldata.SignedTxGroup, 0, selectedIDsSliceLength)

	windowSizedReached := false
	hasMorePendingTransactions := false

	// create a list of all the bloom filters that might need to be tested. This list excludes bloom filters
	// which has the same modulator and a different offset.
	var effectiveBloomFilters []int
	effectiveBloomFilters = make([]int, 0, len(p.recentIncomingBloomFilters))
	for filterIdx := len(p.recentIncomingBloomFilters) - 1; filterIdx >= 0; filterIdx-- {
		if p.recentIncomingBloomFilters[filterIdx].filter == nil {
			continue
		}
		if p.recentIncomingBloomFilters[filterIdx].filter.encodingParams.Modulator != p.requestedTransactionsModulator || p.recentIncomingBloomFilters[filterIdx].filter.encodingParams.Offset != p.requestedTransactionsOffset {
			continue
		}
		effectiveBloomFilters = append(effectiveBloomFilters, filterIdx)
	}

	// removedTxn := 0
	grpIdx := startIndex
scanLoop:
	for ; grpIdx < len(pendingTransactions); grpIdx++ {
		txID := pendingTransactions[grpIdx].GroupTransactionID

		// check if the peer would be interested in these messages -
		if p.requestedTransactionsModulator > 1 {
			if txidToUint64(txID)%uint64(p.requestedTransactionsModulator) != uint64(p.requestedTransactionsOffset) {
				continue
			}
		}

		// filter out transactions that we already previously sent.
		if p.recentSentTransactions.contained(txID) {
			// we already sent that transaction. no need to send again.
			continue
		}

		// check if the peer already received these messages from a different source other than us.
		for _, filterIdx := range effectiveBloomFilters {
			if p.recentIncomingBloomFilters[filterIdx].filter.test(txID) {
				// removedTxn++
				continue scanLoop
			}
		}

		if windowSizedReached {
			hasMorePendingTransactions = true
			break
		}
		selectedTxns = append(selectedTxns, pendingTransactions[grpIdx])
		selectedTxnIDs = append(selectedTxnIDs, txID)

		// add the size of the transaction group
		accumulatedSize += pendingTransactions[grpIdx].EncodedLength

		if accumulatedSize > windowLengthBytes {
			windowSizedReached = true
		}
	}

	p.lastSelectedTransactionsCount = len(selectedTxnIDs)

	// if we've over-allocated, resize the buffer; This becomes important on relays,
	// as storing these arrays can consume considerable amount of memory.
	if len(selectedTxnIDs)*2 < cap(selectedTxnIDs) {
		exactBuffer := make([]transactions.Txid, len(selectedTxnIDs))
		copy(exactBuffer, selectedTxnIDs)
		selectedTxnIDs = exactBuffer
	}

	// update the lastTransactionSelectionGroupCounter if needed -
	// if we selected any transaction to be sent, update the lastTransactionSelectionGroupCounter with the latest
	// group counter. If the startIndex was *after* the last pending transaction, it means that we don't
	// need to update the lastTransactionSelectionGroupCounter since it's already ahead of everything in the pending transactions.
	if grpIdx >= 0 && startIndex < len(pendingTransactions) {
		if grpIdx == len(pendingTransactions) {
			if grpIdx > 0 {
				p.lastTransactionSelectionTracker.set(p.requestedTransactionsOffset, p.requestedTransactionsModulator, pendingTransactions[grpIdx-1].GroupCounter+1)
			}
		} else {
			p.lastTransactionSelectionTracker.set(p.requestedTransactionsOffset, p.requestedTransactionsModulator, pendingTransactions[grpIdx].GroupCounter)
		}
	}

	if !hasMorePendingTransactions {
		// we're done with the current sequence.
		p.messageSeriesPendingTransactions = nil
	}

	// fmt.Printf("selectPendingTransactions : selected %d transactions, %d not needed and aborted after exceeding data length %d/%d more = %v\n", len(selectedTxnIDs), removedTxn, accumulatedSize, windowLengthBytes, hasMorePendingTransactions)

	return selectedTxns, selectedTxnIDs, hasMorePendingTransactions
}

// getLocalRequestParams returns the local requests params
func (p *Peer) getLocalRequestParams() (offset, modulator byte) {
	return p.localTransactionsBaseOffset, p.localTransactionsModulator
}

// update the peer once the message was sent successfully.
func (p *Peer) updateMessageSent(txMsg *transactionBlockMessage, selectedTxnIDs []transactions.Txid, timestamp time.Duration, sequenceNumber uint64, messageSize int) {
	p.recentSentTransactions.addSlice(selectedTxnIDs, sequenceNumber, timestamp)
	p.lastSentMessageSequenceNumber = sequenceNumber
	p.lastSentMessageRound = txMsg.Round
	p.lastSentMessageTimestamp = timestamp
	p.lastSentMessageSize = messageSize
}

// update the peer's lastSentBloomFilter.
func (p *Peer) updateSentBoomFilter(filter bloomFilter, round basics.Round) {
	if filter.encodedLength > 0 {
		p.lastSentBloomFilter = filter
		p.sentFilterParams.setSentFilter(filter, round)
	}
}

// setLocalRequestParams stores the peer request params.
func (p *Peer) setLocalRequestParams(offset, modulator uint64) {
	if modulator > 255 {
		modulator = 255
	}
	p.localTransactionsModulator = byte(modulator)
	if modulator != 0 {
		p.localTransactionsBaseOffset = byte(offset % modulator)
	}
}

// peers array functions

// incomingPeersOnly scan the input peers array and return a subset of the peers that are incoming peers.
func incomingPeersOnly(peers []*Peer) (incomingPeers []*Peer) {
	incomingPeers = make([]*Peer, 0, len(peers))
	for _, peer := range peers {
		if !peer.isOutgoing {
			incomingPeers = append(incomingPeers, peer)
		}
	}
	return
}

// incoming related functions

// addIncomingBloomFilter keeps the most recent {maxIncomingBloomFilterHistory} filters
func (p *Peer) addIncomingBloomFilter(round basics.Round, incomingFilter *testableBloomFilter, currentRound basics.Round) {
	minRound := currentRound.SubSaturate(2)
	if round < minRound {
		// ignore data from the past
		return
	}
	bf := incomingBloomFilter{
		round:  round,
		filter: incomingFilter,
	}
	elemOk := func(i int) bool {
		ribf := p.recentIncomingBloomFilters[i]
		if ribf.filter == nil {
			return false
		}
		if ribf.round < minRound {
			return false
		}
		if incomingFilter.clearPrevious && ribf.filter.encodingParams.Offset == incomingFilter.encodingParams.Offset && ribf.filter.encodingParams.Modulator == incomingFilter.encodingParams.Modulator {
			return false
		}
		return true
	}
	// compact the prior list to the front of the array.
	// order doesn't matter.
	pos := 0
	last := len(p.recentIncomingBloomFilters) - 1
	oldestRound := currentRound + 1
	firstOfOldest := -1
	for pos <= last {
		if elemOk(pos) {
			if p.recentIncomingBloomFilters[pos].round < oldestRound {
				oldestRound = p.recentIncomingBloomFilters[pos].round
				firstOfOldest = pos
			}
			pos++
			continue
		}
		p.recentIncomingBloomFilters[pos] = p.recentIncomingBloomFilters[last]
		p.recentIncomingBloomFilters[last].filter = nil // GC
		last--
	}
	p.recentIncomingBloomFilters = p.recentIncomingBloomFilters[:last+1]
	// Simple case: append
	if last+1 < maxIncomingBloomFilterHistory {
		p.recentIncomingBloomFilters = append(p.recentIncomingBloomFilters, bf)
		return
	}
	// Too much traffic case: replace the first thing we find of the oldest round
	if firstOfOldest >= 0 {
		p.recentIncomingBloomFilters[firstOfOldest] = bf
		return
	}
	// This line should be unreachable, but putting in an error log to test that assumption.
	p.log.Error("addIncomingBloomFilter failed to trim p.recentIncomingBloomFilters (new filter lost)")
}

func (p *Peer) updateRequestParams(modulator, offset byte) {
	p.requestedTransactionsModulator = modulator
	p.requestedTransactionsOffset = offset
}

// update the recentSentTransactions with the incoming transaction groups. This would prevent us from sending the received transactions back to the
// peer that sent it to us. This comes in addition to the bloom filter, if being sent by the other peer.
func (p *Peer) updateIncomingTransactionGroups(txnGroups []pooldata.SignedTxGroup) {
	for _, txnGroup := range txnGroups {
		if len(txnGroup.Transactions) > 0 {
			// The GroupTransactionID field is not yet updated, so we'll be calculating it's value here and passing it.
			p.recentSentTransactions.add(txnGroup.Transactions.ID())
		}
	}
}

func (p *Peer) updateIncomingMessageTiming(timings timingParams, currentRound basics.Round, currentTime time.Duration, timeInQueue time.Duration, peerLatency time.Duration, incomingMessageSize int) {
	p.lastConfirmedMessageSeqReceived = timings.RefTxnBlockMsgSeq
	// if we received a message that references our previous message, see if they occurred on the same round
	if p.lastConfirmedMessageSeqReceived == p.lastSentMessageSequenceNumber && p.lastSentMessageRound == currentRound && p.lastSentMessageTimestamp > 0 {
		// if so, we might be able to calculate the bandwidth.
		timeSinceLastMessageWasSent := currentTime - timeInQueue - p.lastSentMessageTimestamp
		networkMessageSize := uint64(p.lastSentMessageSize + incomingMessageSize)
		if timings.ResponseElapsedTime != 0 && peerLatency > 0 && timeSinceLastMessageWasSent > time.Duration(timings.ResponseElapsedTime)+peerLatency && networkMessageSize >= p.significantMessageThreshold {
			networkTrasmitTime := timeSinceLastMessageWasSent - time.Duration(timings.ResponseElapsedTime) - peerLatency
			dataExchangeRate := uint64(time.Second) * networkMessageSize / uint64(networkTrasmitTime)

			// clamp data exchange rate to realistic metrics
			if dataExchangeRate < minDataExchangeRateThreshold {
				dataExchangeRate = minDataExchangeRateThreshold
			} else if dataExchangeRate > maxDataExchangeRateThreshold {
				dataExchangeRate = maxDataExchangeRateThreshold
			}
			// fmt.Printf("incoming message : updating data exchange to %d; network msg size = %d+%d, transmit time = %v\n", dataExchangeRate, p.lastSentMessageSize, incomingMessageSize, networkTrasmitTime)
			p.dataExchangeRate = dataExchangeRate
		}

		// given that we've (maybe) updated the data exchange rate, we need to clear out the lastSendMessage information
		// so we won't use that again on a subsequent incoming message.
		p.lastSentMessageSequenceNumber = 0
		p.lastSentMessageRound = 0
		p.lastSentMessageTimestamp = 0
		p.lastSentMessageSize = 0
	}
	p.lastReceivedMessageLocalRound = currentRound
	p.lastReceivedMessageTimestamp = currentTime - timeInQueue
	p.lastReceivedMessageSize = incomingMessageSize
	p.lastReceivedMessageNextMsgMinDelay = time.Duration(timings.NextMsgMinDelay) * time.Nanosecond
	p.recentSentTransactions.acknowledge(timings.AcceptedMsgSeq)
}

// advancePeerState is called when a peer schedule arrives, before we're doing any operation.
// The method would determine whether a message need to be sent, and adjust the peer state
// accordingly.
func (p *Peer) advancePeerState(currenTime time.Duration, isRelay bool) (ops peersOps) {
	if isRelay {
		if p.isOutgoing {
			// outgoing peers are "special", as they respond to messages rather then generating their own.
			// we need to figure the special state needed for "late bloom filter message"
			switch p.state {
			case peerStateStartup:
				p.nextStateTimestamp = currenTime + p.lastReceivedMessageNextMsgMinDelay
				messagesCount := p.lastReceivedMessageNextMsgMinDelay / messageTimeWindow
				if messagesCount <= 2 {
					// we have time to send only a single message. This message need to include both transactions and bloom filter.
					p.state = peerStateLateBloom
				} else {
					// we have enough time to send multiple messages, make the first n-1 message have no bloom filter, and have the last one
					// include a bloom filter.
					p.state = peerStateHoldsoff
				}

				// send a message
				ops |= peerOpsSendMessage
			case peerStateHoldsoff:
				// calculate how more messages we can send ( if needed )
				messagesCount := (p.nextStateTimestamp - currenTime) / messageTimeWindow
				if messagesCount <= 2 {
					// we have time to send only a single message. This message need to include both transactions and bloom filter.
					p.state = peerStateLateBloom
				}

				// send a message
				ops |= peerOpsSendMessage

				// the rescehduling would be done in the sendMessageLoop, since we need to know if additional messages are needed.
			case peerStateLateBloom:
				// send a message
				ops |= peerOpsSendMessage

			default:
				// this isn't expected, so we can just ignore this.
				// todo : log
			}
		} else {
			// non-outgoing
			switch p.state {
			case peerStateStartup:
				p.state = peerStateHoldsoff
				fallthrough
			case peerStateHoldsoff:
				// prepare the send message array.
				ops |= peerOpsSendMessage
			default: // peerStateInterrupt & peerStateLateBloom
				// this isn't expected, so we can just ignore this.
				// todo : log
			}
		}
	} else {
		switch p.state {
		case peerStateStartup:
			p.state = peerStateHoldsoff
			ops |= peerOpsSendMessage

		case peerStateHoldsoff:
			if p.nextStateTimestamp == 0 {
				p.state = peerStateInterrupt
				ops |= peerOpsSetInterruptible | peerOpsReschedule
			} else {
				ops |= peerOpsSendMessage
			}

		case peerStateInterrupt:
			p.state = peerStateHoldsoff
			ops |= peerOpsSendMessage | peerOpsClearInterruptible

		default: // peerStateLateBloom
			// this isn't expected, so we can just ignore this.
			// todo : log
		}
	}
	return ops
}

// getMessageConstructionOps constructs the messageConstructionOps that would be needed when
// sending a message back to the peer. The two arguments are:
// - isRelay defines whether the local node is a relay.
// - fetchTransactions defines whether the local node is interested in receiving transactions from
//   the peer ( this is essentially allow us to skip receiving transactions for non-relays that aren't going
//   to make any proposals )
func (p *Peer) getMessageConstructionOps(isRelay bool, fetchTransactions bool) (ops messageConstructionOps) {
	// on outgoing peers of relays, we want have some custom logic.
	if isRelay {
		if p.isOutgoing {
			switch p.state {
			case peerStateLateBloom:
				if p.localTransactionsModulator != 0 {
					ops |= messageConstBloomFilter
				}
			case peerStateHoldsoff:
				ops |= messageConstTransactions
			}
		} else {
			if p.requestedTransactionsModulator != 0 {
				ops |= messageConstTransactions
				if p.nextStateTimestamp == 0 && p.localTransactionsModulator != 0 {
					ops |= messageConstBloomFilter
				}
			}
			if p.nextStateTimestamp == 0 {
				ops |= messageConstNextMinDelay
			}
		}
		ops |= messageConstUpdateRequestParams
	} else {
		ops |= messageConstTransactions // send transactions to the other peer
		if fetchTransactions {
			switch p.localTransactionsModulator {
			case 0:
				// don't send bloom filter.
			case 1:
				// special optimization if we have just one relay that we're connected to:
				// generate the bloom filter only once per 2*beta message.
				// this would reduce the number of unneeded bloom filters generation dramatically.
				// that single relay would know which messages it previously sent us, and would refrain from
				// sending these again.
				if p.nextStateTimestamp == 0 {
					ops |= messageConstBloomFilter
				}
			default:
				ops |= messageConstBloomFilter
			}
			ops |= messageConstUpdateRequestParams
		}
	}
	return ops
}

// getNextScheduleOffset is called after a message was sent to the peer, and we need to evaluate the next
// scheduling time.
func (p *Peer) getNextScheduleOffset(isRelay bool, beta time.Duration, partialMessage bool, currentTime time.Duration) (offset time.Duration, ops peersOps) {
	if partialMessage {
		if isRelay {
			if p.isOutgoing {
				if p.state == peerStateHoldsoff {
					// we have enough time to send another message.
					return messageTimeWindow, peerOpsReschedule
				}
			} else {
				// a partial message was sent to an incoming peer
				if p.nextStateTimestamp > time.Duration(0) {
					if currentTime+messageTimeWindow*2 < p.nextStateTimestamp {
						// we have enough time to send another message
						return messageTimeWindow, peerOpsReschedule
					}
					// we don't have enough time to send another message.
					next := p.nextStateTimestamp
					p.nextStateTimestamp = 0
					return next - currentTime, peerOpsReschedule
				}
				p.nextStateTimestamp = currentTime + 2*beta
				return messageTimeWindow, peerOpsReschedule
			}
		} else {
			if p.nextStateTimestamp > time.Duration(0) {
				if currentTime+messageTimeWindow*2 < p.nextStateTimestamp {
					// we have enough time to send another message
					return messageTimeWindow, peerOpsReschedule
				}
				// we don't have enough time, so don't get into "interrupt" state,
				// since we're already sending messages.
				next := p.nextStateTimestamp
				p.nextStateTimestamp = 0
				p.messageSeriesPendingTransactions = nil
				// move to the next state.
				p.state = peerStateHoldsoff
				return next - currentTime, peerOpsReschedule | peerOpsClearInterruptible

			}
			// this is the first message
			p.nextStateTimestamp = currentTime + 2*beta

			return messageTimeWindow, peerOpsReschedule
		}
	} else {
		if isRelay {
			if p.isOutgoing {
				if p.state == peerStateHoldsoff {
					// even that we're done now, we need to send another message that would contain the bloom filter
					p.state = peerStateLateBloom

					bloomMessageExtrapolatedSendingTime := messageTimeWindow
					// try to improve the sending time by using the last sent bloom filter as the expected message size.
					if p.lastSentBloomFilter.containedTxnsRange.transactionsCount > 0 {
						lastBloomFilterSize := uint64(p.lastSentBloomFilter.encodedLength)
						bloomMessageExtrapolatedSendingTime = time.Duration(lastBloomFilterSize * p.dataExchangeRate)
					}

					next := p.nextStateTimestamp - bloomMessageExtrapolatedSendingTime - currentTime
					p.nextStateTimestamp = 0
					return next, peerOpsReschedule
				}
				p.nextStateTimestamp = 0
			} else {
				// we sent a message to an incoming connection. No more data to send.
				if p.nextStateTimestamp > time.Duration(0) {
					next := p.nextStateTimestamp
					p.nextStateTimestamp = 0
					return next - currentTime, peerOpsReschedule
				}
				p.nextStateTimestamp = 0
				return beta * 2, peerOpsReschedule
			}
		} else {
			if p.nextStateTimestamp > time.Duration(0) {
				next := p.nextStateTimestamp
				p.nextStateTimestamp = 0
				return next - currentTime, peerOpsReschedule
			}
			return beta, peerOpsReschedule
		}
	}
	return time.Duration(0), 0
}

func (p *Peer) networkAddress() string {
	if peerAddress, supportInterface := p.networkPeer.(networkPeerAddress); supportInterface {
		return peerAddress.GetAddress()
	}
	return ""
}
