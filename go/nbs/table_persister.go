// Copyright 2016 Attic Labs, Inc. All rights reserved.
// Licensed under the Apache License, version 2.0:
// http://www.apache.org/licenses/LICENSE-2.0

package nbs

import (
	"bytes"
	"crypto/sha512"
	"encoding/binary"
	"sort"

	"github.com/attic-labs/noms/go/d"
	"github.com/attic-labs/noms/go/util/sizecache"
)

// tablePersister allows interaction with persistent storage. It provides
// primitives for pushing the contents of a memTable to persistent storage,
// opening persistent tables for reading, and conjoining a number of existing
// chunkSources into one.
type tablePersister interface {
	// Persist makes the contents of mt durable. Chunks already present in
	// |haver| may be dropped in the process.
	Persist(mt *memTable, haver chunkReader, stats *Stats) chunkSource

	// CompactAll conjoins all chunks in |sources| into a single, new
	// chunkSource.
	CompactAll(sources chunkSources, stats *Stats) chunkSource

	// Open a table named |name|, containing |chunkCount| chunks. The
	// tablePersister is responsible for managing the lifetime of the returned
	// chunkSource. TODO: Is that actually true? Or can we get rid of explicit 'close'
	Open(name addr, chunkCount uint32) chunkSource
}

type indexCache struct {
	cache *sizecache.SizeCache
}

// Returns an indexCache which will burn roughly |size| bytes of memory
func newIndexCache(size uint64) *indexCache {
	return &indexCache{sizecache.New(size)}
}

func (sic indexCache) get(name addr) (tableIndex, bool) {
	idx, found := sic.cache.Get(name)
	if found {
		return idx.(tableIndex), true
	}

	return tableIndex{}, false
}

func (sic indexCache) put(name addr, idx tableIndex) {
	indexSize := uint64(idx.chunkCount) * (addrSize + ordinalSize + lengthSize + uint64Size)
	sic.cache.Add(name, indexSize, idx)
}

type chunkSourcesByAscendingCount chunkSources

func (csbc chunkSourcesByAscendingCount) Len() int { return len(csbc) }
func (csbc chunkSourcesByAscendingCount) Less(i, j int) bool {
	srcI, srcJ := csbc[i], csbc[j]
	if srcI.count() == srcJ.count() {
		hi, hj := srcI.hash(), srcJ.hash()
		return bytes.Compare(hi[:], hj[:]) < 0
	}
	return srcI.count() < srcJ.count()
}
func (csbc chunkSourcesByAscendingCount) Swap(i, j int) { csbc[i], csbc[j] = csbc[j], csbc[i] }

type chunkSourcesByDescendingDataSize []sourceWithSize

func (csbds chunkSourcesByDescendingDataSize) Len() int { return len(csbds) }
func (csbds chunkSourcesByDescendingDataSize) Less(i, j int) bool {
	swsI, swsJ := csbds[i], csbds[j]
	if swsI.dataLen == swsJ.dataLen {
		hi, hj := swsI.source.hash(), swsJ.source.hash()
		return bytes.Compare(hi[:], hj[:]) < 0
	}
	return swsI.dataLen > swsJ.dataLen
}
func (csbds chunkSourcesByDescendingDataSize) Swap(i, j int) { csbds[i], csbds[j] = csbds[j], csbds[i] }

type sourceWithSize struct {
	source  chunkSource
	dataLen uint64
}

type compactionPlan struct {
	sources             chunkSourcesByDescendingDataSize
	mergedIndex         []byte
	chunkCount          uint32
	totalCompressedData uint64
}

func (cp compactionPlan) lengths() []byte {
	lengthsStart := uint64(cp.chunkCount) * prefixTupleSize
	return cp.mergedIndex[lengthsStart : lengthsStart+uint64(cp.chunkCount)*lengthSize]
}

func (cp compactionPlan) suffixes() []byte {
	suffixesStart := uint64(cp.chunkCount) * (prefixTupleSize + lengthSize)
	return cp.mergedIndex[suffixesStart : suffixesStart+uint64(cp.chunkCount)*addrSuffixSize]
}

func planCompaction(sources chunkSources, stats *Stats) (plan compactionPlan) {
	var totalUncompressedData uint64
	for _, src := range sources {
		totalUncompressedData += src.uncompressedLen()
		index := src.index()
		plan.chunkCount += index.chunkCount

		// Calculate the amount of chunk data in |src|
		chunkDataLen := calcChunkDataLen(index)
		plan.sources = append(plan.sources, sourceWithSize{src, chunkDataLen})
		plan.totalCompressedData += chunkDataLen
	}
	sort.Sort(plan.sources)

	lengthsPos := lengthsOffset(plan.chunkCount)
	suffixesPos := suffixesOffset(plan.chunkCount)
	plan.mergedIndex = make([]byte, indexSize(plan.chunkCount)+footerSize)

	prefixIndexRecs := make(prefixIndexSlice, 0, plan.chunkCount)
	var ordinalOffset uint32
	for _, sws := range plan.sources {
		index := sws.source.index()

		// Add all the prefix tuples from this index to the list of all prefixIndexRecs, modifying the ordinals such that all entries from the 1st item in sources come after those in the 0th and so on.
		for j, prefix := range index.prefixes {
			rec := prefixIndexRec{prefix: prefix, order: ordinalOffset + index.ordinals[j]}
			prefixIndexRecs = append(prefixIndexRecs, rec)
		}
		ordinalOffset += sws.source.count()

		// TODO: copy the lengths and suffixes as a byte-copy from src BUG #3438
		// Bring over the lengths block, in order
		for _, length := range index.lengths {
			binary.BigEndian.PutUint32(plan.mergedIndex[lengthsPos:], length)
			lengthsPos += lengthSize
		}

		// Bring over the suffixes block, in order
		n := copy(plan.mergedIndex[suffixesPos:], index.suffixes)
		d.Chk.True(n == len(index.suffixes))
		suffixesPos += uint64(n)
	}

	// Sort all prefixTuples by hash and then insert them starting at the beginning of plan.mergedIndex
	sort.Sort(prefixIndexRecs)
	var pfxPos uint64
	for _, pi := range prefixIndexRecs {
		binary.BigEndian.PutUint64(plan.mergedIndex[pfxPos:], pi.prefix)
		pfxPos += addrPrefixSize
		binary.BigEndian.PutUint32(plan.mergedIndex[pfxPos:], pi.order)
		pfxPos += ordinalSize
	}

	writeFooter(plan.mergedIndex[uint64(len(plan.mergedIndex))-footerSize:], plan.chunkCount, totalUncompressedData)

	stats.BytesPerConjoin.Sample(uint64(plan.totalCompressedData) + uint64(len(plan.mergedIndex)))
	return plan
}

func nameFromSuffixes(suffixes []byte) (name addr) {
	sha := sha512.New()
	sha.Write(suffixes)

	var h []byte
	h = sha.Sum(h) // Appends hash to h
	copy(name[:], h)
	return
}

func calcChunkDataLen(index tableIndex) uint64 {
	return index.offsets[index.chunkCount-1] + uint64(index.lengths[index.chunkCount-1])
}
