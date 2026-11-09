package smb2

import (
	"encoding/binary"
	"time"

	"github.com/mike76-dev/sombrero/utils"
)

// SnapshotTokenLayout is the form a previous version goes by, in a path and in
// the FSCTL_SRV_ENUMERATE_SNAPSHOTS response: its time, in UTC ([MS-SMB2] 2.2.32.2).
const SnapshotTokenLayout = "@GMT-2006.01.02-15.04.05"

// SnapshotToken names the snapshot taken at the time.
func SnapshotToken(t time.Time) string {
	return t.UTC().Format(SnapshotTokenLayout)
}

// ParseSnapshotToken reads the time a snapshot token names.
func ParseSnapshotToken(s string) (time.Time, bool) {
	t, err := time.ParseInLocation(SnapshotTokenLayout, s, time.UTC)
	return t, err == nil
}

// ParseTimewarpToken reads an SMB2_CREATE_TIMEWARP_TOKEN create context: the
// time of the snapshot the file is to be opened as of ([MS-SMB2] 2.2.13.2.7).
func ParseTimewarpToken(ctx []byte) (time.Time, bool) {
	if len(ctx) != 8 {
		return time.Time{}, false
	}
	return utils.FiletimeToUnix(binary.LittleEndian.Uint64(ctx)).UTC(), true
}

// SnapshotArray encodes an SRV_SNAPSHOT_ARRAY naming the snapshots, or only
// counting them where a response of maxOutput bytes cannot hold the names, in
// which case the array size still says how much room they need.
func SnapshotArray(snapshots []time.Time, maxOutput uint32) []byte {
	const header = 12
	names := make([]byte, 0, len(snapshots)*2*(len(SnapshotTokenLayout)+1)+2)
	for _, t := range snapshots {
		names = append(names, utils.EncodeStringToBytes(SnapshotToken(t))...)
		names = append(names, 0, 0)
	}
	names = append(names, 0, 0)

	out := make([]byte, header, header+len(names))
	binary.LittleEndian.PutUint32(out[0:4], uint32(len(snapshots)))
	binary.LittleEndian.PutUint32(out[8:12], uint32(len(names)))
	if uint32(header+len(names)) > maxOutput {
		return append(out, 0, 0, 0, 0)
	}
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(snapshots)))
	return append(out, names...)
}
