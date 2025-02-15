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
	"context"
	"database/sql"
	"flag"
	"io"
	"io/ioutil"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/pooldata"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/rpcs"
	"github.com/algorand/go-algorand/test/partitiontest"
	"github.com/algorand/go-algorand/util/db"
)

var blockDBFilename = flag.String("db", "", "Location of block db")
var startRound = flag.Int("start", 0, "Starting round")
var endRound = flag.Int("end", 10, "Ending round")

func TestNibble(t *testing.T) {
	partitiontest.PartitionTest(t)

	var b []byte
	for i := 0; i < 10; i++ {
		b = append(b, byte(i))
	}
	compactNibblesArray(&b)
	for i := 0; i < 10; i++ {
		val, err := getNibble(b, i)
		require.NoError(t, err)
		require.Equal(t, byte(i), val)
	}
}

// old encoding method
func encodeTransactionGroupsOld(inTxnGroups []pooldata.SignedTxGroup) []byte {
	stub := txGroupsEncodingStubOld{
		TxnGroups: make([]txnGroups, len(inTxnGroups)),
	}
	for i := range inTxnGroups {
		stub.TxnGroups[i] = txnGroups(inTxnGroups[i].Transactions)
	}

	return stub.MarshalMsg(protocol.GetEncodingBuf()[:0])
}

// old decoding method
func decodeTransactionGroupsOld(bytes []byte) (txnGroups []pooldata.SignedTxGroup, err error) {
	if len(bytes) == 0 {
		return nil, nil
	}
	var stub txGroupsEncodingStubOld
	_, err = stub.UnmarshalMsg(bytes)
	if err != nil {
		return nil, err
	}
	txnGroups = make([]pooldata.SignedTxGroup, len(stub.TxnGroups))
	for i := range stub.TxnGroups {
		txnGroups[i].Transactions = pooldata.SignedTxnSlice(stub.TxnGroups[i])
	}
	return txnGroups, nil
}

func TestTxnGroupEncodingSmall(t *testing.T) {
	partitiontest.PartitionTest(t)

	genesisHash := crypto.Hash([]byte("gh"))
	genesisID := "gID"

	inTxnGroups := []pooldata.SignedTxGroup{
		pooldata.SignedTxGroup{
			Transactions: []transactions.SignedTxn{
				{
					Txn: transactions.Transaction{
						Type: protocol.PaymentTx,
						Header: transactions.Header{
							Sender:      basics.Address(crypto.Hash([]byte("2"))),
							Fee:         basics.MicroAlgos{Raw: 100},
							GenesisHash: genesisHash,
						},
						PaymentTxnFields: transactions.PaymentTxnFields{
							Receiver: basics.Address(crypto.Hash([]byte("4"))),
							Amount:   basics.MicroAlgos{Raw: 1000},
						},
					},
					Sig: crypto.Signature{1},
				},
			},
		},
		pooldata.SignedTxGroup{
			Transactions: []transactions.SignedTxn{
				{
					Txn: transactions.Transaction{
						Type: protocol.PaymentTx,
						Header: transactions.Header{
							Sender:      basics.Address(crypto.Hash([]byte("1"))),
							Fee:         basics.MicroAlgos{Raw: 100},
							GenesisHash: genesisHash,
							GenesisID:   genesisID,
						},
						PaymentTxnFields: transactions.PaymentTxnFields{
							Receiver: basics.Address(crypto.Hash([]byte("2"))),
							Amount:   basics.MicroAlgos{Raw: 1000},
						},
					},
					Sig: crypto.Signature{2},
				},
				{
					Txn: transactions.Transaction{
						Type: protocol.KeyRegistrationTx,
						Header: transactions.Header{
							Sender:      basics.Address(crypto.Hash([]byte("1"))),
							GenesisHash: genesisHash,
							GenesisID:   genesisID,
						},
					},
					Sig: crypto.Signature{3},
				},
			},
		},
		pooldata.SignedTxGroup{
			Transactions: []transactions.SignedTxn{
				{
					Txn: transactions.Transaction{
						Type: protocol.AssetConfigTx,
						Header: transactions.Header{
							Sender:      basics.Address(crypto.Hash([]byte("1"))),
							Fee:         basics.MicroAlgos{Raw: 100},
							GenesisHash: genesisHash,
						},
					},
					Sig: crypto.Signature{4},
				},
				{
					Txn: transactions.Transaction{
						Type: protocol.AssetFreezeTx,
						Header: transactions.Header{
							Sender:      basics.Address(crypto.Hash([]byte("1"))),
							GenesisHash: genesisHash,
						},
					},
					Sig: crypto.Signature{5},
				},
				{
					Txn: transactions.Transaction{
						Type: protocol.CompactCertTx,
						Header: transactions.Header{
							Sender:      basics.Address(crypto.Hash([]byte("1"))),
							GenesisHash: genesisHash,
						},
					},
					Msig: crypto.MultisigSig{Version: 1},
				},
			},
		},
	}
	err := addGroupHashes(inTxnGroups, 6, []byte{1})
	require.NoError(t, err)
	var s syncState
	ptg, err := s.encodeTransactionGroups(inTxnGroups, 1000000000)
	require.NoError(t, err)
	require.Equal(t, ptg.CompressionFormat, compressionFormatNone)
	out, err := decodeTransactionGroups(ptg, genesisID, genesisHash)
	require.NoError(t, err)
	require.ElementsMatch(t, inTxnGroups, out)
}

// txnGroupsData fetches a sample dataset of txns, specify numBlocks up to 969
func txnGroupsData(numBlocks int) (txnGroups []pooldata.SignedTxGroup, genesisID string, genesisHash crypto.Digest, err error) {
	dat, err := ioutil.ReadFile("../test/testdata/mainnetblocks")
	if err != nil {
		return
	}
	dec := protocol.NewDecoderBytes(dat)
	blocksData := make([]rpcs.EncodedBlockCert, numBlocks)
	for i := 0; i < len(blocksData); i++ {
		err = dec.Decode(&blocksData[i])
		if err == io.EOF {
			break
		}
		if err != nil {
			return
		}
	}

	for _, blockData := range blocksData {
		block := blockData.Block
		genesisID = block.GenesisID()
		genesisHash = block.GenesisHash()
		var payset [][]transactions.SignedTxnWithAD
		payset, err = block.DecodePaysetGroups()
		if err != nil {
			return
		}
		for _, txns := range payset {
			var txnGroup pooldata.SignedTxGroup
			for _, txn := range txns {
				txnGroup.Transactions = append(txnGroup.Transactions, txn.SignedTxn)
			}
			txnGroups = append(txnGroups, txnGroup)
		}
	}
	return
}

func TestTxnGroupEncodingLarge(t *testing.T) {
	partitiontest.PartitionTest(t)

	txnGroups, genesisID, genesisHash, err := txnGroupsData(500)
	require.NoError(t, err)

	var s syncState
	ptg, err := s.encodeTransactionGroups(txnGroups, 0)
	require.NoError(t, err)
	require.Equal(t, ptg.CompressionFormat, compressionFormatDeflate)
	out, err := decodeTransactionGroups(ptg, genesisID, genesisHash)
	require.NoError(t, err)
	require.ElementsMatch(t, txnGroups, out)

	encodedGroupsBytes := encodeTransactionGroupsOld(txnGroups)
	out, err = decodeTransactionGroupsOld(encodedGroupsBytes)
	require.NoError(t, err)
	require.ElementsMatch(t, txnGroups, out)

	// check dataset
	count := make(map[protocol.TxType]int)
	sigs := 0
	msigs := 0
	lsigs := 0
	for _, txg := range txnGroups {
		for _, txn := range txg.Transactions {
			count[txn.Txn.Type]++
			if !txn.Sig.MsgIsZero() {
				sigs++
			}
			if !txn.Msig.MsgIsZero() {
				msigs++
			}
			if !txn.Lsig.MsgIsZero() {
				lsigs++
			}
		}
	}
	require.Equal(t, 2, len(count))
	require.Equal(t, 9834, count["axfer"])
	require.Equal(t, 850, count["pay"])
	require.Equal(t, 10678, sigs)
	require.Equal(t, 6, msigs)
	require.Equal(t, 0, lsigs)
}

func BenchmarkTxnGroupEncoding(b *testing.B) {
	txnGroups, _, _, err := txnGroupsData(4)
	require.NoError(b, err)
	var size int
	var s syncState

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ptg, err := s.encodeTransactionGroups(txnGroups, 1000000000)
		require.NoError(b, err)
		size = len(ptg.Bytes)
		releaseEncodedTransactionGroups(ptg.Bytes)
	}

	b.ReportMetric(float64(size), "encodedDataBytes")
}

func BenchmarkTxnGroupCompression(b *testing.B) {
	txnGroups, _, _, err := txnGroupsData(4)
	require.NoError(b, err)
	var size int
	var s syncState
	ptg, err := s.encodeTransactionGroups(txnGroups, 1000000000)
	require.NoError(b, err)
	require.Equal(b, ptg.CompressionFormat, compressionFormatNone)

	b.ReportAllocs()
	b.ResetTimer()
	loopStartTime := time.Now()
	for i := 0; i < b.N; i++ {
		compressedGroupBytes, compressionFormat := s.compressTransactionGroupsBytes(ptg.Bytes)
		require.Equal(b, compressionFormat, compressionFormatDeflate)
		size = len(compressedGroupBytes)
	}
	loopDuration := time.Since(loopStartTime)
	b.StopTimer()
	b.ReportMetric(float64(len(ptg.Bytes)*b.N)/loopDuration.Seconds(), "estimatedGzipCompressionSpeed")
	b.ReportMetric(float64(len(ptg.Bytes)-size)/float64(len(ptg.Bytes)), "estimatedGzipCompressionGains")
}

func BenchmarkTxnGroupDecoding(b *testing.B) {
	txnGroups, genesisID, genesisHash, err := txnGroupsData(4)
	require.NoError(b, err)

	var s syncState
	ptg, err := s.encodeTransactionGroups(txnGroups, 1000000000)
	require.NoError(b, err)
	require.Equal(b, ptg.CompressionFormat, compressionFormatNone)
	require.NoError(b, err)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err = decodeTransactionGroups(ptg, genesisID, genesisHash)
		require.NoError(b, err)
	}
}

func BenchmarkTxnGroupDecompression(b *testing.B) {
	txnGroups, _, _, err := txnGroupsData(4)
	require.NoError(b, err)

	var s syncState
	ptg, err := s.encodeTransactionGroups(txnGroups, 0)
	require.NoError(b, err)
	require.Equal(b, ptg.CompressionFormat, compressionFormatDeflate)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err = decompressTransactionGroupsBytes(ptg.Bytes, ptg.LenDecompressedBytes)
		require.NoError(b, err)
	}
}

func BenchmarkTxnGroupEncodingOld(b *testing.B) {
	txnGroups, _, _, err := txnGroupsData(4)
	require.NoError(b, err)
	var encodedGroupsBytes []byte

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		encodedGroupsBytes = encodeTransactionGroupsOld(txnGroups)
		releaseEncodedTransactionGroups(encodedGroupsBytes)
	}

	b.ReportMetric(float64(len(encodedGroupsBytes)), "encodedDataBytes")
}

func BenchmarkTxnGroupDecodingOld(b *testing.B) {
	txnGroups, _, _, err := txnGroupsData(4)
	require.NoError(b, err)

	encodedGroupsBytes := encodeTransactionGroupsOld(txnGroups)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err = decodeTransactionGroupsOld(encodedGroupsBytes)
		require.NoError(b, err)
	}
}

// TestTxnGroupEncodingReflection generates random
// txns of each type using reflection
func TestTxnGroupEncodingReflection(t *testing.T) {
	partitiontest.PartitionTest(t)

	for i := 0; i < 10; i++ {
		v0, err := protocol.RandomizeObject(&transactions.SignedTxn{})
		require.NoError(t, err)
		stx, ok := v0.(*transactions.SignedTxn)
		require.True(t, ok)

		var txns []transactions.SignedTxn
		for _, txType := range protocol.TxnTypes {
			txn := *stx
			txn.Txn.PaymentTxnFields = transactions.PaymentTxnFields{}
			txn.Txn.KeyregTxnFields = transactions.KeyregTxnFields{}
			txn.Txn.AssetConfigTxnFields = transactions.AssetConfigTxnFields{}
			txn.Txn.AssetTransferTxnFields = transactions.AssetTransferTxnFields{}
			txn.Txn.AssetFreezeTxnFields = transactions.AssetFreezeTxnFields{}
			txn.Txn.ApplicationCallTxnFields = transactions.ApplicationCallTxnFields{}
			txn.Txn.CompactCertTxnFields = transactions.CompactCertTxnFields{}
			txn.Txn.Type = txType
			txn.Lsig.Logic = []byte("logic")
			switch i % 3 {
			case 0: // only have normal sig
				txn.Msig = crypto.MultisigSig{}
				txn.Lsig = transactions.LogicSig{}
			case 1: // only have multi sig
				txn.Sig = crypto.Signature{}
				txn.Lsig = transactions.LogicSig{}
			case 2: // only have logic sig
				txn.Msig = crypto.MultisigSig{}
				txn.Sig = crypto.Signature{}
			}
			switch txType {
			case protocol.UnknownTx:
				continue
			case protocol.PaymentTx:
				v0, err := protocol.RandomizeObject(&txn.Txn.PaymentTxnFields)
				require.NoError(t, err)
				PaymentTxnFields, ok := v0.(*transactions.PaymentTxnFields)
				require.True(t, ok)
				txn.Txn.PaymentTxnFields = *PaymentTxnFields
			case protocol.KeyRegistrationTx:
				v0, err := protocol.RandomizeObject(&txn.Txn.KeyregTxnFields)
				require.NoError(t, err)
				KeyregTxnFields, ok := v0.(*transactions.KeyregTxnFields)
				require.True(t, ok)
				txn.Txn.KeyregTxnFields = *KeyregTxnFields
			case protocol.AssetConfigTx:
				v0, err := protocol.RandomizeObject(&txn.Txn.AssetConfigTxnFields)
				require.NoError(t, err)
				AssetConfigTxnFields, ok := v0.(*transactions.AssetConfigTxnFields)
				require.True(t, ok)
				txn.Txn.AssetConfigTxnFields = *AssetConfigTxnFields
			case protocol.AssetTransferTx:
				v0, err := protocol.RandomizeObject(&txn.Txn.AssetTransferTxnFields)
				require.NoError(t, err)
				AssetTransferTxnFields, ok := v0.(*transactions.AssetTransferTxnFields)
				require.True(t, ok)
				txn.Txn.AssetTransferTxnFields = *AssetTransferTxnFields
			case protocol.AssetFreezeTx:
				v0, err := protocol.RandomizeObject(&txn.Txn.AssetFreezeTxnFields)
				require.NoError(t, err)
				AssetFreezeTxnFields, ok := v0.(*transactions.AssetFreezeTxnFields)
				require.True(t, ok)
				txn.Txn.AssetFreezeTxnFields = *AssetFreezeTxnFields
			case protocol.ApplicationCallTx:
				v0, err := protocol.RandomizeObject(&txn.Txn.ApplicationCallTxnFields)
				require.NoError(t, err)
				ApplicationCallTxnFields, ok := v0.(*transactions.ApplicationCallTxnFields)
				require.True(t, ok)
				txn.Txn.ApplicationCallTxnFields = *ApplicationCallTxnFields
				txn.Txn.ApplicationCallTxnFields.OnCompletion = 1
			case protocol.CompactCertTx:
				v0, err := protocol.RandomizeObject(&txn.Txn.CompactCertTxnFields)
				require.NoError(t, err)
				CompactCertTxnFields, ok := v0.(*transactions.CompactCertTxnFields)
				require.True(t, ok)
				txn.Txn.CompactCertTxnFields = *CompactCertTxnFields
			default:
				require.Fail(t, "unsupported txntype for txnsync msg encoding")
			}
			txn.Txn.Group = crypto.Digest{}
			txns = append(txns, txn)
		}
		txnGroups := []pooldata.SignedTxGroup{
			pooldata.SignedTxGroup{
				Transactions: txns,
			},
		}
		err = addGroupHashes(txnGroups, len(txns), []byte{1})
		require.NoError(t, err)
		var s syncState
		ptg, err := s.encodeTransactionGroups(txnGroups, 0)
		require.NoError(t, err)
		out, err := decodeTransactionGroups(ptg, stx.Txn.GenesisID, stx.Txn.GenesisHash)
		require.NoError(t, err)
		require.ElementsMatch(t, txnGroups, out)
	}
}

// pass in flag -db to specify db, start round, end round
func TestTxnGroupEncodingArchival(t *testing.T) {
	partitiontest.PartitionTest(t)

	if *blockDBFilename == "" {
		t.Skip("no archival node db was provided")
	}
	blockDBs, err := db.OpenPair(*blockDBFilename, false)
	require.NoError(t, err)
	for r := basics.Round(*startRound); r < basics.Round(*endRound); r++ {
		var block bookkeeping.Block
		err = blockDBs.Rdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {
			var buf []byte
			err = tx.QueryRow("SELECT blkdata FROM blocks WHERE rnd=?", r).Scan(&buf)
			if err != nil {
				if err == sql.ErrNoRows {
					err = ledgercore.ErrNoEntry{Round: r}
				}
				return err
			}
			return protocol.Decode(buf, &block)
		})
		require.NoError(t, err)

		var txnGroups []pooldata.SignedTxGroup
		genesisID := block.GenesisID()
		genesisHash := block.GenesisHash()
		var payset [][]transactions.SignedTxnWithAD
		payset, err := block.DecodePaysetGroups()
		require.NoError(t, err)
		for _, txns := range payset {
			var txnGroup pooldata.SignedTxGroup
			for _, txn := range txns {
				txnGroup.Transactions = append(txnGroup.Transactions, txn.SignedTxn)
			}
			txnGroups = append(txnGroups, txnGroup)
		}

		var s syncState
		ptg, err := s.encodeTransactionGroups(txnGroups, 0)
		require.NoError(t, err)
		out, err := decodeTransactionGroups(ptg, genesisID, genesisHash)
		require.NoError(t, err)
		require.ElementsMatch(t, txnGroups, out)
	}
}
