package compress

import "github.com/mike76-dev/sombrero/smb2"

// ScanForDataPatternsV1 scans the buffer for consecutive series
// of equal bytes.
func ScanForDataPatternsV1(buf []byte) (forward, backward *smb2.PatternV1) {
	if len(buf) == 0 {
		return
	}

	forward = &smb2.PatternV1{Pattern: buf[0], Repetitions: 1}
	for i := 1; i < len(buf); i++ {
		if forward.Pattern == buf[i] {
			forward.Repetitions++
		} else {
			break
		}
	}

	if forward.Repetitions < 64 {
		forward.Repetitions = 0
	}

	if forward.Repetitions == uint32(len(buf)) {
		return
	}

	backward = &smb2.PatternV1{Pattern: buf[len(buf)-1], Repetitions: 1}
	for i := len(buf) - 2; i >= 0; i-- {
		if backward.Pattern == buf[i] {
			backward.Repetitions++
		} else {
			break
		}
	}

	if backward.Repetitions < 64 {
		backward.Repetitions = 0
	}

	return
}
