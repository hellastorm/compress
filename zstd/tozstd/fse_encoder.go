package tozstd

import (
	"errors"
	"fmt"
)

const (
	tablelogAbsoluteMax = 9
)

const (
	/*!MEMORY_USAGE :
	 *  Memory usage formula : N->2^N Bytes (examples : 10 -> 1KB; 12 -> 4KB ; 16 -> 64KB; 20 -> 1MB; etc.)
	 *  Increasing memory usage improves compression ratio
	 *  Reduced memory usage can improve speed, due to cache effect
	 *  Recommended max value is 14, for 16KB, which nicely fits into Intel x86 L1 cache */
	maxMemoryUsage = 11

	maxTableLog    = maxMemoryUsage - 2
	maxTablesize   = 1 << maxTableLog
	maxTableMask   = (1 << maxTableLog) - 1
	minTablelog    = 5
	maxSymbolValue = 52
)

// Scratch provides temporary storage for compression and decompression.
type fseEncoder struct {
	// Private
	symbolLen      uint16 // Length of active part of the symbol table.
	actualTableLog uint8  // Selected tablelog.
	bw             bitWriter
	ct             cTable // Compression tables.
	zeroBits       bool   // no bits has prob > 50%.
	clearCount     bool   // clear count
	maxCount       int    // count of the most probable symbol
	useRLE         bool
	rleVal         uint8

	// Per block parameters.
	// These can be used to override compression parameters of the block.
	// Do not touch, unless you know what you are doing.

	// Out is output buffer.
	// If the scratch is re-used before the caller is done processing the output,
	// set this field to nil.
	// Otherwise the output buffer will be re-used for next Compression/Decompression step
	// and allocation will be avoided.
	//Out []byte

	count [256]uint32
	norm  [256]int16
}

// cTable contains tables used for compression.
type cTable struct {
	tableSymbol []byte
	stateTable  []uint16
	symbolTT    []symbolTransform
}

// symbolTransform contains the state transform for a symbol.
type symbolTransform struct {
	deltaFindState int32
	deltaNbBits    uint32
}

// String prints values as a human readable string.
func (s symbolTransform) String() string {
	return fmt.Sprintf("dnbits: %08x, fs:%d", s.deltaNbBits, s.deltaFindState)
}

// Histogram allows to populate the histogram and skip that step in the compression,
// It otherwise allows to inspect the histogram when compression is done.
// To indicate that you have populated the histogram call HistogramFinished
// with the value of the highest populated symbol, as well as the number of entries
// in the most populated entry. These are accepted at face value.
// The returned slice will always be length 256.
func (s *fseEncoder) Histogram() []uint32 {
	return s.count[:]
}

// HistogramFinished can be called to indicate that the histogram has been populated.
// maxSymbol is the index of the highest set symbol of the next data segment.
// maxCount is the number of entries in the most populated entry.
// These are accepted at face value.
func (s *fseEncoder) HistogramFinished(maxSymbol uint8, maxCount int) {
	s.maxCount = maxCount
	s.symbolLen = uint16(maxSymbol) + 1
	s.clearCount = maxCount != 0
}

// prepare will prepare and allocate scratch tables used for both compression and decompression.
func (s *fseEncoder) prepare() (*fseEncoder, error) {
	if s == nil {
		s = &fseEncoder{}
	}
	s.useRLE = false
	if s.clearCount && s.maxCount == 0 {
		for i := range s.count {
			s.count[i] = 0
		}
		s.clearCount = false
	}
	//s.br.init(in)

	return s, nil
}

// allocCtable will allocate tables needed for compression.
// If existing tables a re big enough, they are simply re-used.
func (s *fseEncoder) allocCtable() {
	tableSize := 1 << s.actualTableLog
	// get tableSymbol that is big enough.
	if cap(s.ct.tableSymbol) < int(tableSize) {
		s.ct.tableSymbol = make([]byte, tableSize)
	}
	s.ct.tableSymbol = s.ct.tableSymbol[:tableSize]

	ctSize := tableSize
	if cap(s.ct.stateTable) < ctSize {
		s.ct.stateTable = make([]uint16, ctSize)
	}
	s.ct.stateTable = s.ct.stateTable[:ctSize]

	if cap(s.ct.symbolTT) < int(s.symbolLen) {
		s.ct.symbolTT = make([]symbolTransform, 256)
	}
	s.ct.symbolTT = s.ct.symbolTT[:256]
}

// tableStep returns the next table index.
func tableStep(tableSize uint32) uint32 {
	return (tableSize >> 1) + (tableSize >> 3) + 3
}

// buildCTable will populate the compression table so it is ready to be used.
func (s *fseEncoder) buildCTable() error {
	tableSize := uint32(1 << s.actualTableLog)
	highThreshold := tableSize - 1
	var cumul [maxSymbolValue + 2]int16

	s.allocCtable()
	tableSymbol := s.ct.tableSymbol[:tableSize]
	// symbol start positions
	{
		cumul[0] = 0
		for ui, v := range s.norm[:s.symbolLen-1] {
			u := byte(ui) // one less than reference
			if v == -1 {
				// Low proba symbol
				cumul[u+1] = cumul[u] + 1
				tableSymbol[highThreshold] = u
				highThreshold--
			} else {
				cumul[u+1] = cumul[u] + v
			}
		}
		// Encode last symbol separately to avoid overflowing u
		u := int(s.symbolLen - 1)
		v := s.norm[s.symbolLen-1]
		if v == -1 {
			// Low proba symbol
			cumul[u+1] = cumul[u] + 1
			tableSymbol[highThreshold] = byte(u)
			highThreshold--
		} else {
			cumul[u+1] = cumul[u] + v
		}
		if uint32(cumul[s.symbolLen]) != tableSize {
			return fmt.Errorf("internal error: expected cumul[s.symbolLen] (%d) == tableSize (%d)", cumul[s.symbolLen], tableSize)
		}
		cumul[s.symbolLen] = int16(tableSize) + 1
	}
	// Spread symbols
	s.zeroBits = false
	{
		step := tableStep(tableSize)
		tableMask := tableSize - 1
		var position uint32
		// if any symbol > largeLimit, we may have 0 bits output.
		largeLimit := int16(1 << (s.actualTableLog - 1))
		for ui, v := range s.norm[:s.symbolLen] {
			symbol := byte(ui)
			if v > largeLimit {
				s.zeroBits = true
			}
			for nbOccurrences := int16(0); nbOccurrences < v; nbOccurrences++ {
				tableSymbol[position] = symbol
				position = (position + step) & tableMask
				for position > highThreshold {
					position = (position + step) & tableMask
				} /* Low proba area */
			}
		}

		// Check if we have gone through all positions
		if position != 0 {
			return errors.New("position!=0")
		}
	}

	// Build table
	table := s.ct.stateTable
	{
		tsi := int(tableSize)
		for u, v := range tableSymbol {
			// TableU16 : sorted by symbol order; gives next state value
			table[cumul[v]] = uint16(tsi + u)
			cumul[v]++
		}
	}

	// Build Symbol Transformation Table
	{
		total := int16(0)
		symbolTT := s.ct.symbolTT[:s.symbolLen]
		tableLog := s.actualTableLog
		tl := (uint32(tableLog) << 16) - (1 << tableLog)
		for i, v := range s.norm[:s.symbolLen] {
			switch v {
			case 0:
			case -1, 1:
				symbolTT[i].deltaNbBits = tl
				symbolTT[i].deltaFindState = int32(total - 1)
				total++
			default:
				maxBitsOut := uint32(tableLog) - highBit(uint32(v-1))
				minStatePlus := uint32(v) << maxBitsOut
				symbolTT[i].deltaNbBits = (maxBitsOut << 16) - minStatePlus
				symbolTT[i].deltaFindState = int32(total - v)
				total += v
			}
		}
		if total != int16(tableSize) {
			return fmt.Errorf("total mismatch %d (got) != %d (want)", total, tableSize)
		}
	}
	return nil
}

var rtbTable = [...]uint32{0, 473195, 504333, 520860, 550000, 700000, 750000, 830000}

// normalizeCount will normalize the count of the symbols so
// the total is equal to the table size.
func (s *fseEncoder) normalizeCount(in []byte) error {
	var (
		length            = len(in)
		tableLog          = s.actualTableLog
		scale             = 62 - uint64(tableLog)
		step              = (1 << 62) / uint64(length)
		vStep             = uint64(1) << (scale - 20)
		stillToDistribute = int16(1 << tableLog)
		largest           int
		largestP          int16
		lowThreshold      = (uint32)(length >> tableLog)
	)
	if s.maxCount == length {
		s.useRLE = true
		s.rleVal = in[0]
		return nil
	}
	s.optimalTableLog(length)
	for i, cnt := range s.count[:s.symbolLen] {
		// already handled
		// if (count[s] == s.length) return 0;   /* rle special case */

		if cnt == 0 {
			s.norm[i] = 0
			continue
		}
		if cnt <= lowThreshold {
			s.norm[i] = -1
			stillToDistribute--
		} else {
			proba := (int16)((uint64(cnt) * step) >> scale)
			if proba < 8 {
				restToBeat := vStep * uint64(rtbTable[proba])
				v := uint64(cnt)*step - (uint64(proba) << scale)
				if v > restToBeat {
					proba++
				}
			}
			if proba > largestP {
				largestP = proba
				largest = i
			}
			s.norm[i] = proba
			stillToDistribute -= proba
		}
	}

	if -stillToDistribute >= (s.norm[largest] >> 1) {
		// corner case, need another normalization method
		return s.normalizeCount2(length)
	}
	s.norm[largest] += stillToDistribute
	return nil
}

// Secondary normalization method.
// To be used when primary method fails.
func (s *fseEncoder) normalizeCount2(length int) error {
	const notYetAssigned = -2
	var (
		distributed  uint32
		total        = uint32(length)
		tableLog     = s.actualTableLog
		lowThreshold = uint32(total >> tableLog)
		lowOne       = uint32((total * 3) >> (tableLog + 1))
	)
	for i, cnt := range s.count[:s.symbolLen] {
		if cnt == 0 {
			s.norm[i] = 0
			continue
		}
		if cnt <= lowThreshold {
			s.norm[i] = -1
			distributed++
			total -= cnt
			continue
		}
		if cnt <= lowOne {
			s.norm[i] = 1
			distributed++
			total -= cnt
			continue
		}
		s.norm[i] = notYetAssigned
	}
	toDistribute := (1 << tableLog) - distributed

	if (total / toDistribute) > lowOne {
		// risk of rounding to zero
		lowOne = uint32((total * 3) / (toDistribute * 2))
		for i, cnt := range s.count[:s.symbolLen] {
			if (s.norm[i] == notYetAssigned) && (cnt <= lowOne) {
				s.norm[i] = 1
				distributed++
				total -= cnt
				continue
			}
		}
		toDistribute = (1 << tableLog) - distributed
	}
	if distributed == uint32(s.symbolLen)+1 {
		// all values are pretty poor;
		//   probably incompressible data (should have already been detected);
		//   find max, then give all remaining points to max
		var maxV int
		var maxC uint32
		for i, cnt := range s.count[:s.symbolLen] {
			if cnt > maxC {
				maxV = i
				maxC = cnt
			}
		}
		s.norm[maxV] += int16(toDistribute)
		return nil
	}

	if total == 0 {
		// all of the symbols were low enough for the lowOne or lowThreshold
		for i := uint32(0); toDistribute > 0; i = (i + 1) % (uint32(s.symbolLen)) {
			if s.norm[i] > 0 {
				toDistribute--
				s.norm[i]++
			}
		}
		return nil
	}

	var (
		vStepLog = 62 - uint64(tableLog)
		mid      = uint64((1 << (vStepLog - 1)) - 1)
		rStep    = (((1 << vStepLog) * uint64(toDistribute)) + mid) / uint64(total) // scale on remaining
		tmpTotal = mid
	)
	for i, cnt := range s.count[:s.symbolLen] {
		if s.norm[i] == notYetAssigned {
			var (
				end    = tmpTotal + uint64(cnt)*rStep
				sStart = uint32(tmpTotal >> vStepLog)
				sEnd   = uint32(end >> vStepLog)
				weight = sEnd - sStart
			)
			if weight < 1 {
				return errors.New("weight < 1")
			}
			s.norm[i] = int16(weight)
			tmpTotal = end
		}
	}
	return nil
}

// optimalTableLog calculates and sets the optimal tableLog in s.actualTableLog
func (s *fseEncoder) optimalTableLog(length int) {
	tableLog := uint8(maxTableLog)
	minBits := uint8(minTablelog)
	maxBitsSrc := uint8(highBit(uint32(length-1))) - 2
	if maxBitsSrc < tableLog {
		// Accuracy can be reduced
		tableLog = maxBitsSrc
	}
	if minBits > tableLog {
		tableLog = minBits
	}
	// Need a minimum to safely represent all symbol values
	if tableLog < minTablelog {
		tableLog = minTablelog
	}
	if tableLog > maxTableLog {
		tableLog = maxTableLog
	}
	s.actualTableLog = tableLog
}

// validateNorm validates the normalized histogram table.
func (s *fseEncoder) validateNorm() (err error) {
	var total int
	for _, v := range s.norm[:s.symbolLen] {
		if v >= 0 {
			total += int(v)
		} else {
			total -= int(v)
		}
	}
	defer func() {
		if err == nil {
			return
		}
		fmt.Printf("selected TableLog: %d, Symbol length: %d\n", s.actualTableLog, s.symbolLen)
		for i, v := range s.norm[:s.symbolLen] {
			fmt.Printf("%3d: %5d -> %4d \n", i, s.count[i], v)
		}
	}()
	if total != (1 << s.actualTableLog) {
		return fmt.Errorf("warning: Total == %d != %d", total, 1<<s.actualTableLog)
	}
	for i, v := range s.count[s.symbolLen:] {
		if v != 0 {
			return fmt.Errorf("warning: Found symbol out of range, %d after cut", i)
		}
	}
	return nil
}

// writeCount will write the normalized histogram count to header.
// This is read back by readNCount.
func (s *fseEncoder) writeCount(out []byte) ([]byte, error) {
	var (
		tableLog  = s.actualTableLog
		tableSize = 1 << tableLog
		previous0 bool
		charnum   uint16

		maxHeaderSize = ((int(s.symbolLen) * int(tableLog)) >> 3) + 3

		// Write Table Size
		bitStream = uint32(tableLog - minTablelog)
		bitCount  = uint(4)
		remaining = int16(tableSize + 1) /* +1 for extra accuracy */
		threshold = int16(tableSize)
		nbBits    = uint(tableLog + 1)
	)
	if s.useRLE {
		return append(out, s.rleVal), nil
	}
	outP := len(out)
	out = out[:outP+maxHeaderSize]

	// stops at 1
	for remaining > 1 {
		if previous0 {
			start := charnum
			for s.norm[charnum] == 0 {
				charnum++
			}
			for charnum >= start+24 {
				start += 24
				bitStream += uint32(0xFFFF) << bitCount
				out[outP] = byte(bitStream)
				out[outP+1] = byte(bitStream >> 8)
				outP += 2
				bitStream >>= 16
			}
			for charnum >= start+3 {
				start += 3
				bitStream += 3 << bitCount
				bitCount += 2
			}
			bitStream += uint32(charnum-start) << bitCount
			bitCount += 2
			if bitCount > 16 {
				out[outP] = byte(bitStream)
				out[outP+1] = byte(bitStream >> 8)
				outP += 2
				bitStream >>= 16
				bitCount -= 16
			}
		}

		count := s.norm[charnum]
		charnum++
		max := (2*threshold - 1) - remaining
		if count < 0 {
			remaining += count
		} else {
			remaining -= count
		}
		count++ // +1 for extra accuracy
		if count >= threshold {
			count += max // [0..max[ [max..threshold[ (...) [threshold+max 2*threshold[
		}
		bitStream += uint32(count) << bitCount
		bitCount += nbBits
		if count < max {
			bitCount--
		}

		previous0 = count == 1
		if remaining < 1 {
			return nil, errors.New("internal error: remaining<1")
		}
		for remaining < threshold {
			nbBits--
			threshold >>= 1
		}

		if bitCount > 16 {
			out[outP] = byte(bitStream)
			out[outP+1] = byte(bitStream >> 8)
			outP += 2
			bitStream >>= 16
			bitCount -= 16
		}
	}

	out[outP] = byte(bitStream)
	out[outP+1] = byte(bitStream >> 8)
	outP += int((bitCount + 7) / 8)

	if uint16(charnum) > s.symbolLen {
		return nil, errors.New("internal error: charnum > s.symbolLen")
	}
	return out[:outP], s.buildCTable()
}

// cState contains the compression state of a stream.
type cState struct {
	bw         *bitWriter
	stateTable []uint16
	state      uint16
}

// init will initialize the compression state to the first symbol of the stream.
func (c *cState) init(bw *bitWriter, ct *cTable, first symbolTransform) {
	c.bw = bw
	c.stateTable = ct.stateTable

	nbBitsOut := (first.deltaNbBits + (1 << 15)) >> 16
	im := int32((nbBitsOut << 16) - first.deltaNbBits)
	lu := (im >> nbBitsOut) + first.deltaFindState
	c.state = c.stateTable[lu]
	return
}

// encode the output symbol provided and write it to the bitstream.
func (c *cState) encode(symbolTT symbolTransform) {
	nbBitsOut := (uint32(c.state) + symbolTT.deltaNbBits) >> 16
	dstState := int32(c.state>>(nbBitsOut&15)) + symbolTT.deltaFindState
	c.bw.addBits16NC(c.state, uint8(nbBitsOut))
	c.state = c.stateTable[dstState]
}

// encode the output symbol provided and write it to the bitstream.
func (c *cState) encodeZero(symbolTT symbolTransform) {
	nbBitsOut := (uint32(c.state) + symbolTT.deltaNbBits) >> 16
	dstState := int32(c.state>>(nbBitsOut&15)) + symbolTT.deltaFindState
	c.bw.addBits16ZeroNC(c.state, uint8(nbBitsOut))
	c.state = c.stateTable[dstState]
}

// flush will write the tablelog to the output and flush the remaining full bytes.
func (c *cState) flush(tableLog uint8) {
	c.bw.flush32()
	c.bw.addBits16NC(c.state, tableLog)
}