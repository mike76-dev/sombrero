package utils

// The characters that mean something in a search pattern, as [MS-FSA] 2.1.4.4 lays them out.
// These five are the whole of it: every other character in a pattern stands for itself,
// brackets and backslashes included.
const (
	wildStar   = '*' // Zero or more characters.
	wildQM     = '?' // Exactly one character.
	wildDOSDot = '"' // A period, or nothing at the end of the name.
	wildDOSQM  = '>' // One character, or nothing at a period or at the end of the name.
	wildDOSStr = '<' // Zero or more characters, stopping at the last period of what is left.
)

// MaxPatternLength is the longest search pattern worth matching: a pattern stands for one path
// component, and no name is longer than that.
const MaxPatternLength = 255

// MatchPattern reports whether name matches the search pattern of a directory query. The
// pattern is matched as SMB defines it rather than as a shell glob: the DOS wildcards a client
// may still send are honoured, and anything that is not one of the five wildcard characters is
// compared literally.
//
// The comparison runs as a table over the two strings rather than by backtracking, so a pattern
// that is nothing but wildcards costs the length of the pattern times the length of the name
// instead of blowing up exponentially. Only one row of the table is held at a time, so a long
// pattern costs no memory beyond the name.
func MatchPattern(pattern, name string) bool {
	p, n := []rune(pattern), []rune(name)
	np, nn := len(p), len(n)

	// lastDot[j] is the position of the final period in n[j:], or -1 when there is none. It is
	// what tells DOS_STAR where to stop, and it has to be measured from where the wildcard
	// starts rather than over the whole name: in "*.*" - which a client sends as DOS_STAR,
	// DOS_DOT, DOS_STAR - the second DOS_STAR begins past the period the first one stopped at,
	// and there is nothing left for it to stop at.
	lastDot := make([]int, nn+1)
	lastDot[nn] = -1
	for j := nn - 1; j >= 0; j-- {
		switch {
		case lastDot[j+1] >= 0:
			lastDot[j] = lastDot[j+1]
		case n[j] == '.':
			lastDot[j] = j
		default:
			lastDot[j] = -1
		}
	}

	// cur[j] says whether p[i:] matches n[j:], and next[j] the same for p[i+1:]. Both ends of both
	// strings are included, so the empty pattern against the empty remainder is the one case that
	// starts out true. Every rule reads the row behind it and, for the wildcards that stand for
	// more than one character, the same row further along the name, which the descending j has
	// already filled in.
	cur, next := make([]bool, nn+1), make([]bool, nn+1)
	next[nn] = true

	for i := np - 1; i >= 0; i-- {
		for j := nn; j >= 0; j-- {
			switch p[i] {
			case wildStar:
				// Nothing, or one more character and the same wildcard again.
				cur[j] = next[j] || (j < nn && cur[j+1])

			case wildDOSStr:
				// As above, except that it may not pass the final period of what is left: that
				// period is where it gives up and hands the rest of the name to the rest of the
				// pattern. A name with no period left in it is consumed to the end.
				cur[j] = next[j] || (j < nn && (lastDot[j] < 0 || j < lastDot[j]) && cur[j+1])

			case wildQM:
				cur[j] = j < nn && next[j+1]

			case wildDOSQM:
				// One character, unless the name is out or a period has been reached, in which
				// case this wildcard and every one behind it match nothing at all.
				if j < nn && n[j] != '.' {
					cur[j] = next[j+1]
				} else {
					cur[j] = next[j]
				}

			case wildDOSDot:
				// A period, or nothing once the name has run out. This is how a pattern ending
				// in a period asks for the names that have no extension.
				if j < nn {
					cur[j] = n[j] == '.' && next[j+1]
				} else {
					cur[j] = next[j]
				}

			default:
				cur[j] = j < nn && p[i] == n[j] && next[j+1]
			}
		}

		cur, next = next, cur
	}

	// The rows were swapped after the last pattern character was done, so the answer is in next.
	return next[0]
}
