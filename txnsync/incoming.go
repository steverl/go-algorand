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
	"errors"
	"time"

	"github.com/algorand/go-algorand/data/pooldata"
)

var (
	errUnsupportedTransactionSyncMessageVersion = errors.New("unsupported transaction sync message version")
	errTransactionSyncIncomingMessageQueueFull  = errors.New("transaction sync incoming message queue is full")
	errInvalidBloomFilter                       = errors.New("invalid bloom filter")
	errDecodingReceivedTransactionGroupsFailed  = errors.New("failed to decode incoming transaction groups")
)

type incomingMessage struct {
	networkPeer       interface{}
	message           transactionBlockMessage
	sequenceNumber    uint64
	peer              *Peer
	encodedSize       int // the byte length of the incoming network message
	bloomFilter       *testableBloomFilter
	transactionGroups []pooldata.SignedTxGroup
	timeReceived      int64
}

// incomingMessageHandler
// note - this message is called by the network go-routine dispatch pool, and is not synchronized with the rest of the transaction synchronizer
func (s *syncState) asyncIncomingMessageHandler(networkPeer interface{}, peer *Peer, message []byte, sequenceNumber uint64, receivedTimestamp int64) (err error) {
	// increase number of incoming messages metric.
	txsyncIncomingMessagesTotal.Inc(nil)

	// check the return value when we exit this function. if we fail, we increase the metric.
	defer func() {
		if err != nil {
			// increase number of unprocessed incoming messages metric.
			txsyncUnprocessedIncomingMessagesTotal.Inc(nil)
		}
	}()

	incomingMessage := incomingMessage{networkPeer: networkPeer, sequenceNumber: sequenceNumber, encodedSize: len(message), peer: peer, timeReceived: receivedTimestamp}
	_, err = incomingMessage.message.UnmarshalMsg(message)
	if err != nil {
		// if we received a message that we cannot parse, disconnect.
		s.log.Infof("received unparsable transaction sync message from peer. disconnecting from peer: %v, bytes: %d", err, len(message))
		s.incomingMessagesQ.erase(peer, networkPeer)
		return err
	}

	if incomingMessage.message.Version != txnBlockMessageVersion {
		// we receive a message from a version that we don't support, disconnect.
		s.log.Infof("received unsupported transaction sync message version from peer (%d). disconnecting from peer.", incomingMessage.message.Version)
		s.incomingMessagesQ.erase(peer, networkPeer)
		return errUnsupportedTransactionSyncMessageVersion
	}

	// if the peer sent us a bloom filter, decode it
	if !incomingMessage.message.TxnBloomFilter.MsgIsZero() {
		bloomFilter, err := decodeBloomFilter(incomingMessage.message.TxnBloomFilter)
		if err != nil {
			s.log.Infof("Invalid bloom filter received from peer : %v", err)
			s.incomingMessagesQ.erase(peer, networkPeer)
			return errInvalidBloomFilter
		}
		incomingMessage.bloomFilter = bloomFilter
		// increase number of decoded bloom filters.
		txsyncDecodedBloomFiltersTotal.Inc(nil)
	}

	// if the peer sent us any transactions, decode these.
	incomingMessage.transactionGroups, err = decodeTransactionGroups(incomingMessage.message.TransactionGroups, s.genesisID, s.genesisHash)
	if err != nil {
		s.log.Infof("failed to decode received transactions groups: %v\n", err)
		s.incomingMessagesQ.erase(peer, networkPeer)
		return errDecodingReceivedTransactionGroupsFailed
	}

	if peer == nil {
		// if we don't have a peer, then we need to enqueue this task to be handled by the main loop since we want to ensure that
		// all the peer objects are created synchronously.
		enqueued := s.incomingMessagesQ.enqueue(incomingMessage)
		if !enqueued {
			// if we failed to enqueue, it means that the queue is full. Try to remove disconnected
			// peers from the queue before re-attempting.
			peers := s.node.GetPeers()
			if s.incomingMessagesQ.prunePeers(peers) {
				// if we were successful in removing at least a single peer, then try to add the entry again.
				enqueued = s.incomingMessagesQ.enqueue(incomingMessage)
			}
			if !enqueued {
				// if we can't enqueue that, return an error, which would disconnect the peer.
				// ( we have to disconnect, since otherwise, we would have no way to synchronize the sequence number)
				s.log.Infof("unable to enqueue incoming message from a peer without txsync allocated data; incoming messages queue is full. disconnecting from peer.")
				s.incomingMessagesQ.erase(peer, networkPeer)
				return errTransactionSyncIncomingMessageQueueFull
			}

		}
		return nil
	}
	// place the incoming message on the *peer* heap, allowing us to dequeue it in the order by which it was received by the network library.
	err = peer.incomingMessages.enqueue(incomingMessage)
	if err != nil {
		// if the incoming message queue for this peer is full, disconnect from this peer.
		s.log.Infof("unable to enqueue incoming message into peer incoming message backlog. disconnecting from peer.")
		s.incomingMessagesQ.erase(peer, networkPeer)
		return err
	}

	// (maybe) place the peer message on the main queue. This would get skipped if the peer is already on the queue.
	enqueued := s.incomingMessagesQ.enqueue(incomingMessage)
	if !enqueued {
		// if we failed to enqueue, it means that the queue is full. Try to remove disconnected
		// peers from the queue before re-attempting.
		peers := s.node.GetPeers()
		if s.incomingMessagesQ.prunePeers(peers) {
			// if we were successful in removing at least a single peer, then try to add the entry again.
			enqueued = s.incomingMessagesQ.enqueue(incomingMessage)
		}
		if !enqueued {
			// if we can't enqueue that, return an error, which would disconnect the peer.
			s.log.Infof("unable to enqueue incoming message from a peer with txsync allocated data; incoming messages queue is full. disconnecting from peer.")
			s.incomingMessagesQ.erase(peer, networkPeer)
			return errTransactionSyncIncomingMessageQueueFull
		}
	}
	return nil
}

func (s *syncState) evaluateIncomingMessage(message incomingMessage) {
	peer := message.peer
	if peer == nil {
		// check if a peer was created already for this network peer object.
		peerInfo := s.node.GetPeer(message.networkPeer)
		if peerInfo.NetworkPeer == nil {
			// the message.networkPeer isn't a valid unicast peer, so we can exit right here.
			return
		}
		if peerInfo.TxnSyncPeer == nil {
			// we couldn't really do much about this message previously, since we didn't have the peer.
			peer = makePeer(message.networkPeer, peerInfo.IsOutgoing, s.isRelay, &s.config, s.log, s.node.GetPeerLatency(message.networkPeer))
			// let the network peer object know about our peer
			s.node.UpdatePeers([]*Peer{peer}, []interface{}{message.networkPeer}, 0)
		} else {
			peer = peerInfo.TxnSyncPeer
		}
		message.peer = peer
		err := peer.incomingMessages.enqueue(message)
		if err != nil {
			// this is not really likely, since we won't saturate the peer heap right after creating it..
			return
		}
	}

	messageProcessed := false
	transactionPoolSize := 0
	totalAccumulatedTransactionsCount := 0 // the number of transactions that were added during the execution of this method
	transactionHandlerBacklogFull := false
incomingMessageLoop:
	for {
		incomingMsg, seq, err := peer.incomingMessages.popSequence(peer.nextReceivedMessageSeq)
		switch err {
		case errHeapEmpty:
			// this is very likely, once we run out of consecutive messages.
			break incomingMessageLoop
		case errSequenceNumberMismatch:
			// if we receive a message which wasn't in-order, just let it go.
			s.log.Debugf("received message out of order; seq = %d, expecting seq = %d\n", seq, peer.nextReceivedMessageSeq)
			break incomingMessageLoop
		}

		// increase the message sequence number, since we're processing this message.
		peer.nextReceivedMessageSeq++

		// skip txnsync messages with proposalData for now
		if !incomingMsg.message.RelayedProposal.MsgIsZero() {
			continue
		}

		// update the round number if needed.
		if incomingMsg.message.Round > peer.lastRound {
			peer.lastRound = incomingMsg.message.Round
		} else if incomingMsg.message.Round < peer.lastRound {
			// peer sent us message for an older round, *after* a new round ?!
			continue
		}

		// if the peer sent us a bloom filter, store this.
		if incomingMsg.bloomFilter != nil {
			peer.addIncomingBloomFilter(incomingMsg.message.Round, incomingMsg.bloomFilter, s.round)
		}

		peer.updateRequestParams(incomingMsg.message.UpdatedRequestParams.Modulator, incomingMsg.message.UpdatedRequestParams.Offset)
		timeInQueue := time.Duration(0)
		if incomingMsg.timeReceived > 0 {
			timeInQueue = time.Since(time.Unix(0, incomingMsg.timeReceived))
		}
		peer.updateIncomingMessageTiming(incomingMsg.message.MsgSync, s.round, s.clock.Since(), timeInQueue, peer.cachedLatency, incomingMsg.encodedSize)

		// if the peer's round is more than a single round behind the local node, then we don't want to
		// try and load the transactions. The other peer should first catch up before getting transactions.
		if (peer.lastRound + 1) < s.round {
			if s.config.EnableVerbosedTransactionSyncLogging {
				s.log.Infof("Incoming Txsync #%d late round %d", seq, peer.lastRound)
			}
			continue
		}

		// add the received transaction groups to the peer's recentSentTransactions so that we won't be sending these back to the peer.
		peer.updateIncomingTransactionGroups(incomingMsg.transactionGroups)

		// before enqueuing more data to the transaction pool, make sure we flush the ack channel
		peer.dequeuePendingTransactionPoolAckMessages()

		// if we received at least a single transaction group, then forward it to the transaction handler.
		if len(incomingMsg.transactionGroups) > 0 {
			// get the number of transactions ( not transaction groups !! ) from the transaction groups slice.
			// this code is using the fact the we allocate all the transactions as a single array, and then slice
			// them for the different transaction groups. The transaction handler would re-allocate the transactions that
			// would be stored in the transaction pool.
			totalTransactionCount := cap(incomingMsg.transactionGroups[0].Transactions)

			// send the incoming transaction group to the node last, so that the txhandler could modify the underlaying array if needed.
			currentTransactionPoolSize := s.node.IncomingTransactionGroups(peer, peer.nextReceivedMessageSeq-1, incomingMsg.transactionGroups)
			// was the call reached the transaction handler queue ?
			if currentTransactionPoolSize >= 0 {
				// we want to store in transactionPoolSize only the first call to IncomingTransactionGroups:
				// when multiple IncomingTransactionGroups calls are made within this for-loop, we want to get the current transaction pool size,
				// plus an estimate for the optimistic size after all the transaction groups would get added. For that purpose, it would be sufficient
				// to get the transaction pool size once. The precise size of the transaction pool here is not critical - we use it only for the purpose
				// of calculating the beta number as well as figure if the transaction pool is full or not ( both of them are range-based ).
				if transactionPoolSize == 0 {
					transactionPoolSize = currentTransactionPoolSize
				}
				// add the transactions count to the accumulated count.
				totalAccumulatedTransactionsCount += totalTransactionCount
			} else {
				// no - we couldn't add this group since the transaction handler buffer backlog exceeded it's capacity.
				transactionHandlerBacklogFull = true
			}
		}

		s.log.incomingMessage(msgStats{seq, incomingMsg.message.Round, len(incomingMsg.transactionGroups), incomingMsg.message.UpdatedRequestParams, len(incomingMsg.message.TxnBloomFilter.BloomFilter), incomingMsg.message.MsgSync.NextMsgMinDelay, peer.networkAddress()})
		messageProcessed = true
	}

	// if we're a relay, this is an outgoing peer and we've processed a valid message,
	// then we want to respond right away as well as schedule bloom message.
	if messageProcessed && peer.isOutgoing && s.isRelay && peer.lastReceivedMessageNextMsgMinDelay != time.Duration(0) {
		peer.state = peerStateStartup
		// if we had another message coming from this peer previously, we need to ensure there are not scheduled tasks.
		s.scheduler.peerDuration(peer)

		s.scheduler.schedulePeer(peer, s.clock.Since())
	}
	if transactionPoolSize > 0 || transactionHandlerBacklogFull {
		s.onTransactionPoolChangedEvent(MakeTransactionPoolChangeEvent(transactionPoolSize+totalAccumulatedTransactionsCount, transactionHandlerBacklogFull))
	}
}
