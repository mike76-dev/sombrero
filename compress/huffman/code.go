package huffman

import "sort"

// The alphabet is 512 symbols. 0-255 are literal bytes; 256-511 are matches, each carrying how far
// back the match reaches and, up to a point, how long it is. 256 doubles as the end-of-file mark.
const (
	symbolCount = 512
	eofSymbol   = 256

	// maxCodeLen is as long as a code may be. The table gives each length four bits, so nothing
	// longer than 15 can be written down.
	maxCodeLen = 15
)

// codeLengths works out how many bits to give each symbol, given how often each one is used.
//
// Huffman on its own can hand out codes longer than 15 bits when the frequencies are skewed enough,
// and there is nowhere in the table to record one. Rather than a length-limiting construction, the
// frequencies are flattened and the whole thing done again: halving every count leaves the common
// symbols common but pulls the rare ones up towards them, and the longest code shortens with each
// pass. It costs a little compression on inputs that need it, which are rare, and the alternative
// is a much more delicate algorithm for the same outcome.
func codeLengths(freq []uint32) []uint8 {
	for {
		lengths := huffmanLengths(freq)

		longest := uint8(0)
		for _, l := range lengths {
			if l > longest {
				longest = l
			}
		}

		if longest <= maxCodeLen {
			return lengths
		}

		for i, f := range freq {
			if f > 1 {
				freq[i] = (f + 1) / 2
			}
		}
	}
}

// node is one entry in the Huffman construction: either a symbol or a pair of them joined.
type node struct {
	freq        uint32
	symbol      int // -1 for a joined pair
	left, right *node
}

// huffmanLengths builds the tree and reads the depth of each symbol off it.
func huffmanLengths(freq []uint32) []uint8 {
	var live []*node
	for sym, f := range freq {
		if f > 0 {
			live = append(live, &node{freq: f, symbol: sym})
		}
	}

	lengths := make([]uint8, symbolCount)

	switch len(live) {
	case 0:
		return lengths

	case 1:
		// One symbol has no depth to measure, and a code of no bits cannot be written or read. It
		// is given one bit, which is the shortest thing that can be.
		lengths[live[0].symbol] = 1
		return lengths
	}

	// Joining the two rarest each time is the whole of the algorithm. The list is kept in
	// frequency order and re-sorted rather than kept in a heap: there are at most 512 symbols, so
	// the difference is not worth the machinery.
	sort.Slice(live, func(i, j int) bool {
		if live[i].freq != live[j].freq {
			return live[i].freq < live[j].freq
		}
		return live[i].symbol < live[j].symbol
	})

	for len(live) > 1 {
		a, b := live[0], live[1]
		joined := &node{freq: a.freq + b.freq, symbol: -1, left: a, right: b}

		live = live[2:]

		// The joined node goes back in where it belongs, which keeps the list sorted without
		// sorting it again.
		i := sort.Search(len(live), func(i int) bool { return live[i].freq >= joined.freq })
		live = append(live, nil)
		copy(live[i+1:], live[i:])
		live[i] = joined
	}

	depth(live[0], 0, lengths)

	return lengths
}

// depth walks the tree, recording how far down each symbol sits.
func depth(n *node, d uint8, lengths []uint8) {
	if n.symbol >= 0 {
		lengths[n.symbol] = d
		return
	}

	depth(n.left, d+1, lengths)
	depth(n.right, d+1, lengths)
}

// canonicalCodes turns the lengths into the codes themselves. Canonical means the codes are handed
// out shortest first and, within a length, in symbol order, so that the lengths alone are enough to
// rebuild them - which is why the table on the wire carries nothing else.
func canonicalCodes(lengths []uint8) []uint16 {
	var count [maxCodeLen + 1]uint16
	for _, l := range lengths {
		if l > 0 {
			count[l]++
		}
	}

	var next [maxCodeLen + 1]uint16
	code := uint16(0)
	for l := 1; l <= maxCodeLen; l++ {
		code = (code + count[l-1]) << 1
		next[l] = code
	}

	codes := make([]uint16, len(lengths))
	for sym, l := range lengths {
		if l > 0 {
			codes[sym] = next[l]
			next[l]++
		}
	}

	return codes
}

// decodeTable is the other direction: what a run of bits means.
type decodeTable struct {
	// count[l] is how many symbols have codes of length l, and symbols holds every symbol that has
	// a code at all, shortest first and in symbol order within a length. Together they are enough
	// to walk the codes a bit at a time, which needs no table of 2^15 entries to do.
	count   [maxCodeLen + 1]int
	symbols []uint16
}

func newDecodeTable(lengths []uint8) *decodeTable {
	t := &decodeTable{}
	for _, l := range lengths {
		if l > 0 {
			t.count[l]++
		}
	}

	offset := make([]int, maxCodeLen+2)
	for l := 1; l <= maxCodeLen; l++ {
		offset[l+1] = offset[l] + t.count[l]
	}

	t.symbols = make([]uint16, offset[maxCodeLen+1])
	for sym, l := range lengths {
		if l > 0 {
			t.symbols[offset[l]] = uint16(sym)
			offset[l]++
		}
	}

	return t
}

// symbol reads one code and returns what it stands for.
func (t *decodeTable) symbol(r *reader) (int, bool) {
	code, first, index := 0, 0, 0

	for l := 1; l <= maxCodeLen; l++ {
		code |= int(r.take(1))

		if n := t.count[l]; code-first < n {
			return int(t.symbols[index+code-first]), true
		} else {
			index += n
			first = (first + n) << 1
		}

		code <<= 1
	}

	return 0, false
}
