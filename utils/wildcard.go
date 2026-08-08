package utils

// The characters that mean something in a search pattern, as [MS-FSA] 2.1.4.4 lays them out.
// These five are the whole of it: every other character in a pattern stands for itself,
// brackets and backslashes included. A general-purpose glob matcher reads "[MS-SMB2].pdf" as a
// character class followed by ".pdf" and so never finds the file of that name, which is a name
// a client is perfectly entitled to use.
const (
	wildStar   = '*' // Zero or more characters.
	wildQM     = '?' // Exactly one character.
	wildDOSDot = '"' // A period, or nothing at the end of the name.
	wildDOSQM  = '>' // One character, or nothing at a period or at the end of the name.
	wildDOSStr = '<' // Zero or more characters, stopping at the last period of what is left.
)

// MatchPattern reports whether name matches the search pattern of a directory query. The
// pattern is matched as SMB defines it rather than as a shell glob: the DOS wildcards a client
// may still send are honoured, and anything that is not one of the five wildcard characters is
// compared literally.
//
// The comparison runs as a table over the two strings rather than by backtracking, so a pattern
// that is nothing but wildcards costs the length of the pattern times the length of the name
// instead of blowing up exponentially. The pattern comes off the wire, so its cost is the
// client's to choose and must stay bounded.
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

	// dp[i][j] says whether p[i:] matches n[j:]. Both ends of both strings are included, so the
	// empty pattern against the empty remainder is the one case that starts out true.
	dp := make([][]bool, np+1)
	for i := range dp {
		dp[i] = make([]bool, nn+1)
	}
	dp[np][nn] = true

	for i := np - 1; i >= 0; i-- {
		for j := nn; j >= 0; j-- {
			switch p[i] {
			case wildStar:
				// Nothing, or one more character and the same wildcard again.
				dp[i][j] = dp[i+1][j] || (j < nn && dp[i][j+1])

			case wildDOSStr:
				// As above, except that it may not pass the final period of what is left: that
				// period is where it gives up and hands the rest of the name to the rest of the
				// pattern. A name with no period left in it is consumed to the end.
				dp[i][j] = dp[i+1][j] || (j < nn && (lastDot[j] < 0 || j < lastDot[j]) && dp[i][j+1])

			case wildQM:
				dp[i][j] = j < nn && dp[i+1][j+1]

			case wildDOSQM:
				// One character, unless the name is out or a period has been reached, in which
				// case this wildcard and every one behind it match nothing at all.
				if j < nn && n[j] != '.' {
					dp[i][j] = dp[i+1][j+1]
				} else {
					dp[i][j] = dp[i+1][j]
				}

			case wildDOSDot:
				// A period, or nothing once the name has run out. This is how a pattern ending
				// in a period asks for the names that have no extension.
				if j < nn {
					dp[i][j] = n[j] == '.' && dp[i+1][j+1]
				} else {
					dp[i][j] = dp[i+1][j]
				}

			default:
				dp[i][j] = j < nn && p[i] == n[j] && dp[i+1][j+1]
			}
		}
	}

	return dp[0][0]
}
