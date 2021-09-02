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
	"encoding/binary"
	"errors"
	"math"

	"github.com/algorand/go-algorand/data/pooldata"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/util/bloom"
)

// bloomFilterFalsePositiveRate is used as the target false positive rate for the multiHashBloomFilter implementation.
// the xor based bloom filters have their own hard-coded false positive rate, and therefore require no configuration.
const bloomFilterFalsePositiveRate = 0.01

var errInvalidBloomFilterEncoding = errors.New("invalid bloom filter encoding")

//msgp:ignore bloomFilterType
type bloomFilterType byte

const (
	invalidBloomFilter bloomFilterType = iota
	multiHashBloomFilter
	xorBloomFilter32
	xorBloomFilter8
)

// transactionsRange helps us to identify a subset of the transaction pool pending transaction groups.
// it's being used as part of an optimization when we're attempting to recreate a bloom filter :
// if the new bloom filter shares the same set of parameters, then the result is expected to be the
// same and therefore the old bloom filter can be used.
type transactionsRange struct {
	firstCounter      uint64
	lastCounter       uint64
	transactionsCount uint64
}

type bloomFilter struct {
	encodingParams requestParams

	filter bloom.GenericFilter

	containedTxnsRange transactionsRange

	encoded *encodedBloomFilter

	filterType bloomFilterType
}

func decodeBloomFilter(enc encodedBloomFilter) (outFilter bloomFilter, err error) {
	switch bloomFilterType(enc.BloomFilterType) {
	case multiHashBloomFilter:
		outFilter.filter, err = bloom.UnmarshalBinary(enc.BloomFilter)
	case xorBloomFilter32:
		outFilter.filter = new(bloom.XorFilter)
		err = outFilter.filter.UnmarshalBinary(enc.BloomFilter)
	case xorBloomFilter8:
		outFilter.filter = new(bloom.XorFilter8)
		err = outFilter.filter.UnmarshalBinary(enc.BloomFilter)
	default:
		return bloomFilter{}, errInvalidBloomFilterEncoding
	}

	if err != nil {
		return bloomFilter{}, err
	}
	outFilter.filterType = bloomFilterType(enc.BloomFilterType)
	outFilter.encodingParams = enc.EncodingParams
	return outFilter, nil
}

func (bf *bloomFilter) encode() (out *encodedBloomFilter, err error) {
	if bf.encoded != nil {
		return bf.encoded, nil
	}
	out = new(encodedBloomFilter)
	out.BloomFilterType = byte(invalidBloomFilter)
	out.EncodingParams = bf.encodingParams
	if bf.filter != nil {
		out.BloomFilterType = byte(bf.filterType)
		out.BloomFilter, err = bf.filter.MarshalBinary()
		if err != nil || len(out.BloomFilter) == 0 {
			out = nil
		} else {
			bf.encoded = out
			// increase the counter for a successful bloom filter encoding
			txsyncEncodedBloomFiltersTotal.Inc(nil)
		}
	}
	return
}

func (bf *bloomFilter) sameParams(other bloomFilter) bool {
	return (bf.encodingParams == other.encodingParams) && (bf.containedTxnsRange == other.containedTxnsRange)
}

func (bf *bloomFilter) test(txID transactions.Txid) bool {
	if bf.filter != nil {
		if bf.encodingParams.Modulator > 1 {
			if txidToUint64(txID)%uint64(bf.encodingParams.Modulator) != uint64(bf.encodingParams.Offset) {
				return false
			}
		}
		return bf.filter.Test(txID[:])
	}
	return false
}

func filterFactoryBloom(numEntries int, s *syncState) (filter bloom.GenericFilter, filterType bloomFilterType) {
	shuffler := uint32(s.node.Random(math.MaxUint64))
	sizeBits, numHashes := bloom.Optimal(numEntries, bloomFilterFalsePositiveRate)
	return bloom.New(sizeBits, numHashes, shuffler), multiHashBloomFilter
}

func filterFactoryXor8(numEntries int, s *syncState) (filter bloom.GenericFilter, filterType bloomFilterType) { //nolint:deadcode,unused
	s.xorBuilder.RandomNumberGeneratorSeed = s.node.Random(math.MaxUint64)
	return bloom.NewXor8(numEntries, &s.xorBuilder), xorBloomFilter8
}

func filterFactoryXor32(numEntries int, s *syncState) (filter bloom.GenericFilter, filterType bloomFilterType) {
	s.xorBuilder.RandomNumberGeneratorSeed = s.node.Random(math.MaxUint64)
	return bloom.NewXor(numEntries, &s.xorBuilder), xorBloomFilter32
}

var filterFactory func(int, *syncState) (filter bloom.GenericFilter, filterType bloomFilterType) = filterFactoryXor32

func (s *syncState) makeBloomFilter(encodingParams requestParams, txnGroups []pooldata.SignedTxGroup, hintPrevBloomFilter *bloomFilter) (result bloomFilter) {
	result.encodingParams = encodingParams
	switch {
	case encodingParams.Modulator == 0:
		// we want none.
		return
	case encodingParams.Modulator == 1:
		// we want all.
		if len(txnGroups) > 0 {
			result.containedTxnsRange.firstCounter = txnGroups[0].GroupCounter
			result.containedTxnsRange.lastCounter = txnGroups[len(txnGroups)-1].GroupCounter
			result.containedTxnsRange.transactionsCount = uint64(len(txnGroups))
		}

		if hintPrevBloomFilter != nil {
			if result.sameParams(*hintPrevBloomFilter) {
				return *hintPrevBloomFilter
			}
		}

		result.filter, result.filterType = filterFactory(len(txnGroups), s)
		for _, group := range txnGroups {
			result.filter.Set(group.GroupTransactionID[:])
		}
		_, err := result.encode()
		if err != nil {
			// fall back to standard bloom filter
			result.filter, result.filterType = filterFactoryBloom(len(txnGroups), s)
			for _, group := range txnGroups {
				result.filter.Set(group.GroupTransactionID[:])
			}
		}
	default:
		// we want subset.
		result.containedTxnsRange.firstCounter = math.MaxUint64
		filteredTransactionsIDs := getTxIDSliceBuffer(len(txnGroups))
		defer releaseTxIDSliceBuffer(filteredTransactionsIDs)

		for _, group := range txnGroups {
			txID := group.GroupTransactionID
			if txidToUint64(txID)%uint64(encodingParams.Modulator) != uint64(encodingParams.Offset) {
				continue
			}
			filteredTransactionsIDs = append(filteredTransactionsIDs, txID)
			if result.containedTxnsRange.firstCounter == math.MaxUint64 {
				result.containedTxnsRange.firstCounter = group.GroupCounter
			}
			result.containedTxnsRange.lastCounter = group.GroupCounter
		}

		result.containedTxnsRange.transactionsCount = uint64(len(filteredTransactionsIDs))

		if hintPrevBloomFilter != nil {
			if result.sameParams(*hintPrevBloomFilter) {
				return *hintPrevBloomFilter
			}
		}

		result.filter, result.filterType = filterFactory(len(filteredTransactionsIDs), s)

		for _, txid := range filteredTransactionsIDs {
			result.filter.Set(txid[:])
		}
		_, err := result.encode()
		if err != nil {
			// fall back to standard bloom filter
			result.filter, result.filterType = filterFactoryBloom(len(txnGroups), s)
			for _, txid := range filteredTransactionsIDs {
				result.filter.Set(txid[:])
			}
		}
	}

	return
}

func txidToUint64(txID transactions.Txid) uint64 {
	return binary.LittleEndian.Uint64(txID[:8])
}