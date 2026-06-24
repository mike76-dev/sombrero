package smb2

import (
	"encoding/binary"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/ntlm"
	"github.com/mike76-dev/sombrero/utils"
	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	"github.com/oiweiwei/go-msrpc/ndr"
	"golang.org/x/crypto/blake2b"
	"lukechampine.com/frand"
)

const (
	SMB2QueryDirectoryRequestMinSize       = 32
	SMB2QueryDirectoryRequestStructureSize = 33

	SMB2QueryDirectoryResponseMinSize       = 8
	SMB2QueryDirectoryResponseStructureSize = 9

	SMB2QueryInfoRequestMinSize       = 40
	SMB2QueryInfoRequestStructureSize = 41

	SMB2QueryInfoResponseMinSize       = 8
	SMB2QueryInfoResponseStructureSize = 9
)

const (
	// File information classes for SMB2_QUERY_DIRECTORY.
	FILE_DIRECTORY_INFORMATION                  = 0x01
	FILE_FULL_DIRECTORY_INFORMATION             = 0x02
	FILE_ID_FULL_DIRECTORY_INFORMATION          = 0x26
	FILE_BOTH_DIRECTORY_INFORMATION             = 0x03
	FILE_ID_BOTH_DIRECTORY_INFORMATION          = 0x25
	FILE_NAMES_INFORMATION                      = 0x0c
	FILE_ID_EXTD_DIRECTORY_INFORMATION          = 0x3c
	FILE_ID_64_EXTD_DIRECTORY_INFORMATION       = 0x4e
	FILE_ID_64_EXTD_BOTH_DIRECTORY_INFORMATION  = 0x4f
	FILE_ID_ALL_EXTD_DIRECTORY_INFORMATION      = 0x50
	FILE_ID_ALL_EXTD_BOTH_DIRECTORY_INFORMATION = 0x51
	FILE_INFORMATION_CLASS_RESERVED             = 0x64
)

const (
	// Query flags for SMB2_QUERY_DIRECTORY.
	RESTART_SCANS       = 0x01
	RETURN_SINGLE_ENTRY = 0x02
	INDEX_SPECIFIED     = 0x04
	REOPEN              = 0x10
)

const (
	// Information types.
	INFO_FILE       = 0x01
	INFO_FILESYSTEM = 0x02
	INFO_SECURITY   = 0x03
	INFO_QUOTA      = 0x04
)

const (
	// File information classes for SMB2_QUERY_INFO.
	FileAccessInformation          = 0x08
	FileAlignmentInformation       = 0x11
	FileAllInformation             = 0x12
	FileAllocationInformation      = 0x13
	FileAlternateNameInformation   = 0x15
	FileAttributeTagInformation    = 0x23
	FileBasicInformation           = 0x04
	FileCompressionInformation     = 0x1c
	FileDispositionInformation     = 0x0d
	FileEaInformation              = 0x07
	FileEndOfFileInformation       = 0x14
	FileFullEaInformation          = 0x0f
	FileIdInformation              = 0x3b
	FileInternalInformation        = 0x06
	FileLinkInformation            = 0x0b
	FileModeInformation            = 0x10
	FileNetworkOpenInformation     = 0x22
	FileNormalizedNameInformation  = 0x30
	FilePipeInformation            = 0x17
	FilePipeLocalInformation       = 0x18
	FilePipeRemoteInformation      = 0x19
	FilePositionInformation        = 0x0e
	FileRenameInformation          = 0x0a
	FileShortNameInformation       = 0x28
	FileStandardInformation        = 0x05
	FileStreamInformation          = 0x16
	FileValidDataLengthInformation = 0x27
	FileInfoClass_Reserved         = 0x64

	// File system information classes for SMB2_QUERY_INFO.
	FileFsAttributeInformation  = 0x05
	FileFsControlInformation    = 0x06
	FileFsDeviceInformation     = 0x04
	FileFsFullSizeInformation   = 0x07
	FileFsObjectIdInformation   = 0x08
	FileFsSectorSizeInformation = 0x0b
	FileFsSizeInformation       = 0x03
	FileFsVolumeInformation     = 0x01
)

const (
	// Security information flags.
	OWNER_SECURITY_INFORMATION     = 0x00000001
	GROUP_SECURITY_INFORMATION     = 0x00000002
	DACL_SECURITY_INFORMATION      = 0x00000004
	SACL_SECURITY_INFORMATION      = 0x00000008
	LABEL_SECURITY_INFORMATION     = 0x00000010
	ATTRIBUTE_SECURITY_INFORMATION = 0x00000020
	SCOPE_SECURITY_INFORMATION     = 0x00000040
	BACKUP_SECURITY_INFORMATION    = 0x00010000
)

const (
	// Query flags for SMB2_QUERY_INFO.
	SL_RESTART_SCAN        = 0x00000001
	SL_RETURN_SINGLE_ENTRY = 0x00000002
	SL_INDEX_SPECIFIED     = 0x00000004
)

// QueryDirectoryRequest represents an SMB2_QUERY_DIRECTORY request.
type QueryDirectoryRequest struct {
	Request
}

// Validate implements GenericRequest interface.
func (qdr QueryDirectoryRequest) Validate(supportsMultiCredit bool) error {
	if err := Header(qdr.data).Validate(); err != nil {
		return err
	}

	if len(qdr.data) < SMB2HeaderSize+SMB2QueryDirectoryRequestMinSize {
		return ErrWrongLength
	}

	if qdr.structureSize() != SMB2QueryDirectoryRequestStructureSize {
		return ErrWrongFormat
	}

	off := binary.LittleEndian.Uint16(qdr.data[SMB2HeaderSize+24 : SMB2HeaderSize+26])
	length := binary.LittleEndian.Uint16(qdr.data[SMB2HeaderSize+26 : SMB2HeaderSize+28])
	if off+length > uint16(len(qdr.data)) {
		return ErrInvalidParameter
	}

	// Validate CreditCharge.
	if supportsMultiCredit {
		sps := uint32(len(qdr.data) - SMB2HeaderSize - SMB2QueryDirectoryRequestMinSize)
		ers := qdr.OutputBufferLength()
		if qdr.Header().CreditCharge() == 0 {
			if sps > 65536 || ers > 65536 {
				return ErrInvalidParameter
			}
		} else if qdr.Header().CreditCharge() < uint16((max(sps, ers)-1)/65536)+1 {
			return ErrInvalidParameter
		}
	}

	return nil
}

// FileInformationClass returns the FileInformationClass field of the SMB2_QUERY_DIRECTORY request.
func (qdr QueryDirectoryRequest) FileInformationClass() uint8 {
	return qdr.data[SMB2HeaderSize+2]
}

// Flags returns the Flags field of the SMB2_QUERY_DIRECTORY request.
func (qdr QueryDirectoryRequest) Flags() uint8 {
	return qdr.data[SMB2HeaderSize+3]
}

// FileIndex returns the FileIndex field of the SMB2_QUERY_DIRECTORY request.
func (qdr QueryDirectoryRequest) FileIndex() uint32 {
	return binary.LittleEndian.Uint32(qdr.data[SMB2HeaderSize+4 : SMB2HeaderSize+8])
}

// FileID returns the FileID field of the SMB2_QUERY_DIRECTORY request.
func (qdr QueryDirectoryRequest) FileID() []byte {
	fid := make([]byte, 16)
	copy(fid, qdr.data[SMB2HeaderSize+8:SMB2HeaderSize+24])
	return fid
}

// OutputBufferLength returns the OutputBufferLength field of the SMB2_QUERY_DIRECTORY request.
func (qdr QueryDirectoryRequest) OutputBufferLength() uint32 {
	return binary.LittleEndian.Uint32(qdr.data[SMB2HeaderSize+28 : SMB2HeaderSize+32])
}

// FileName returns the filename referenced by the Buffer field of the SMB2_QUERY_DIRECTORY request.
func (qdr QueryDirectoryRequest) FileName() string {
	off := binary.LittleEndian.Uint16(qdr.data[SMB2HeaderSize+24 : SMB2HeaderSize+26])
	length := binary.LittleEndian.Uint16(qdr.data[SMB2HeaderSize+26 : SMB2HeaderSize+28])
	return utils.DecodeToString(qdr.data[off : off+length])
}

type dirInfo struct {
	FileIndex       uint32
	CreationTime    time.Time
	LastAccessTime  time.Time
	LastWriteTime   time.Time
	ChangeTime      time.Time
	EndOfFile       uint64
	AllocationSize  uint64
	FileAttributes  uint32
	EaSize          uint32
	ReparsePointTag uint32
	ShortName       string
	FileID64        uint64
	FileID128       []byte
	FileName        string
}

type fileDirInfo []dirInfo
type fileBothDirInfo []dirInfo
type fileIDBothDirInfo []dirInfo
type fileID64ExtdBothDirInfo []dirInfo
type fileFullDirInfo []dirInfo
type fileIDFullDirInfo []dirInfo
type fileIDExtdDirInfo []dirInfo
type fileID64ExtdDirInfo []dirInfo
type fileIDAllExtdDirInfo []dirInfo
type fileIDAllExtdBothDirInfo []dirInfo

func (info fileDirInfo) encode() []byte {
	var buf []byte
	for i, entry := range info {
		long := utils.EncodeStringToBytes(entry.FileName)
		length := 64 + len(long)
		if i < len(info)-1 {
			length = utils.Roundup(length, 8)
		}

		di := make([]byte, length)
		if i < len(info)-1 {
			binary.LittleEndian.PutUint32(di[:4], uint32(length))
		}

		binary.LittleEndian.PutUint32(di[4:8], entry.FileIndex)
		binary.LittleEndian.PutUint64(di[8:16], utils.UnixToFiletime(entry.CreationTime))
		binary.LittleEndian.PutUint64(di[16:24], utils.UnixToFiletime(entry.LastAccessTime))
		binary.LittleEndian.PutUint64(di[24:32], utils.UnixToFiletime(entry.LastWriteTime))
		binary.LittleEndian.PutUint64(di[32:40], utils.UnixToFiletime(entry.ChangeTime))
		binary.LittleEndian.PutUint64(di[40:48], entry.EndOfFile)
		binary.LittleEndian.PutUint64(di[48:56], entry.AllocationSize)
		binary.LittleEndian.PutUint32(di[56:60], entry.FileAttributes)
		binary.LittleEndian.PutUint32(di[60:64], uint32(len(long)))
		copy(di[64:64+len(long)], long)

		buf = append(buf, di...)
	}

	return buf
}

func (info fileBothDirInfo) encode() []byte {
	var buf []byte
	for i, entry := range info {
		short := utils.EncodeStringToBytes(entry.ShortName)
		long := utils.EncodeStringToBytes(entry.FileName)
		length := 94 + len(long)
		if i < len(info)-1 {
			length = utils.Roundup(length, 8)
		}

		di := make([]byte, length)
		if i < len(info)-1 {
			binary.LittleEndian.PutUint32(di[:4], uint32(length))
		}

		binary.LittleEndian.PutUint32(di[4:8], entry.FileIndex)
		binary.LittleEndian.PutUint64(di[8:16], utils.UnixToFiletime(entry.CreationTime))
		binary.LittleEndian.PutUint64(di[16:24], utils.UnixToFiletime(entry.LastAccessTime))
		binary.LittleEndian.PutUint64(di[24:32], utils.UnixToFiletime(entry.LastWriteTime))
		binary.LittleEndian.PutUint64(di[32:40], utils.UnixToFiletime(entry.ChangeTime))
		binary.LittleEndian.PutUint64(di[40:48], entry.EndOfFile)
		binary.LittleEndian.PutUint64(di[48:56], entry.AllocationSize)
		binary.LittleEndian.PutUint32(di[56:60], entry.FileAttributes)
		binary.LittleEndian.PutUint32(di[64:68], entry.EaSize)

		di[68] = uint8(len(short))
		copy(di[70:94], short)
		binary.LittleEndian.PutUint32(di[60:64], uint32(len(long)))
		copy(di[94:94+len(long)], long)

		buf = append(buf, di...)
	}

	return buf
}

func (info fileIDBothDirInfo) encode() []byte {
	var buf []byte
	for i, entry := range info {
		short := utils.EncodeStringToBytes(entry.ShortName)
		long := utils.EncodeStringToBytes(entry.FileName)
		length := 104 + len(long)
		if i < len(info)-1 {
			length = utils.Roundup(length, 8)
		}

		di := make([]byte, length)
		if i < len(info)-1 {
			binary.LittleEndian.PutUint32(di[:4], uint32(length))
		}

		binary.LittleEndian.PutUint32(di[4:8], entry.FileIndex)
		binary.LittleEndian.PutUint64(di[8:16], utils.UnixToFiletime(entry.CreationTime))
		binary.LittleEndian.PutUint64(di[16:24], utils.UnixToFiletime(entry.LastAccessTime))
		binary.LittleEndian.PutUint64(di[24:32], utils.UnixToFiletime(entry.LastWriteTime))
		binary.LittleEndian.PutUint64(di[32:40], utils.UnixToFiletime(entry.ChangeTime))
		binary.LittleEndian.PutUint64(di[40:48], entry.EndOfFile)
		binary.LittleEndian.PutUint64(di[48:56], entry.AllocationSize)
		binary.LittleEndian.PutUint32(di[56:60], entry.FileAttributes)
		binary.LittleEndian.PutUint32(di[64:68], entry.EaSize)
		binary.LittleEndian.PutUint64(di[96:104], entry.FileID64)

		di[68] = uint8(len(short))
		copy(di[70:94], short)
		binary.LittleEndian.PutUint32(di[60:64], uint32(len(long)))
		copy(di[104:104+len(long)], long)

		buf = append(buf, di...)
	}

	return buf
}

func (info fileID64ExtdBothDirInfo) encode() []byte {
	var buf []byte
	for i, entry := range info {
		short := utils.EncodeStringToBytes(entry.ShortName)
		long := utils.EncodeStringToBytes(entry.FileName)
		length := 106 + len(long)
		if i < len(info)-1 {
			length = utils.Roundup(length, 8)
		}

		di := make([]byte, length)
		if i < len(info)-1 {
			binary.LittleEndian.PutUint32(di[:4], uint32(length))
		}

		binary.LittleEndian.PutUint32(di[4:8], entry.FileIndex)
		binary.LittleEndian.PutUint64(di[8:16], utils.UnixToFiletime(entry.CreationTime))
		binary.LittleEndian.PutUint64(di[16:24], utils.UnixToFiletime(entry.LastAccessTime))
		binary.LittleEndian.PutUint64(di[24:32], utils.UnixToFiletime(entry.LastWriteTime))
		binary.LittleEndian.PutUint64(di[32:40], utils.UnixToFiletime(entry.ChangeTime))
		binary.LittleEndian.PutUint64(di[40:48], entry.EndOfFile)
		binary.LittleEndian.PutUint64(di[48:56], entry.AllocationSize)
		binary.LittleEndian.PutUint32(di[56:60], entry.FileAttributes)
		binary.LittleEndian.PutUint32(di[64:68], entry.EaSize)
		binary.LittleEndian.PutUint32(di[68:72], entry.ReparsePointTag)
		binary.LittleEndian.PutUint64(di[72:80], entry.FileID64)

		di[80] = uint8(len(short))
		copy(di[82:106], short)
		binary.LittleEndian.PutUint32(di[60:64], uint32(len(long)))
		copy(di[106:106+len(long)], long)

		buf = append(buf, di...)
	}

	return buf
}

func (info fileFullDirInfo) encode() []byte {
	var buf []byte
	for i, entry := range info {
		long := utils.EncodeStringToBytes(entry.FileName)
		length := 68 + len(long)
		if i < len(info)-1 {
			length = utils.Roundup(length, 8)
		}

		di := make([]byte, length)
		if i < len(info)-1 {
			binary.LittleEndian.PutUint32(di[:4], uint32(length))
		}

		binary.LittleEndian.PutUint32(di[4:8], entry.FileIndex)
		binary.LittleEndian.PutUint64(di[8:16], utils.UnixToFiletime(entry.CreationTime))
		binary.LittleEndian.PutUint64(di[16:24], utils.UnixToFiletime(entry.LastAccessTime))
		binary.LittleEndian.PutUint64(di[24:32], utils.UnixToFiletime(entry.LastWriteTime))
		binary.LittleEndian.PutUint64(di[32:40], utils.UnixToFiletime(entry.ChangeTime))
		binary.LittleEndian.PutUint64(di[40:48], entry.EndOfFile)
		binary.LittleEndian.PutUint64(di[48:56], entry.AllocationSize)
		binary.LittleEndian.PutUint32(di[56:60], entry.FileAttributes)
		binary.LittleEndian.PutUint32(di[60:64], uint32(len(long)))
		binary.LittleEndian.PutUint32(di[64:68], entry.EaSize)
		copy(di[68:68+len(long)], long)

		buf = append(buf, di...)
	}

	return buf
}

func (info fileIDFullDirInfo) encode() []byte {
	var buf []byte
	for i, entry := range info {
		name := utils.EncodeStringToBytes(entry.FileName)
		length := 80 + len(name)
		if i < len(info)-1 {
			length = utils.Roundup(length, 8)
		}

		di := make([]byte, length)
		if i < len(info)-1 {
			binary.LittleEndian.PutUint32(di[:4], uint32(length))
		}

		binary.LittleEndian.PutUint32(di[4:8], entry.FileIndex)
		binary.LittleEndian.PutUint64(di[8:16], utils.UnixToFiletime(entry.CreationTime))
		binary.LittleEndian.PutUint64(di[16:24], utils.UnixToFiletime(entry.LastAccessTime))
		binary.LittleEndian.PutUint64(di[24:32], utils.UnixToFiletime(entry.LastWriteTime))
		binary.LittleEndian.PutUint64(di[32:40], utils.UnixToFiletime(entry.ChangeTime))
		binary.LittleEndian.PutUint64(di[40:48], entry.EndOfFile)
		binary.LittleEndian.PutUint64(di[48:56], entry.AllocationSize)
		binary.LittleEndian.PutUint32(di[56:60], entry.FileAttributes)
		binary.LittleEndian.PutUint32(di[60:64], uint32(len(name)))
		binary.LittleEndian.PutUint32(di[64:68], entry.EaSize)
		binary.LittleEndian.PutUint64(di[72:80], entry.FileID64)
		copy(di[80:80+len(name)], name)

		buf = append(buf, di...)
	}

	return buf
}

func (info fileIDExtdDirInfo) encode() []byte {
	var buf []byte
	for i, entry := range info {
		name := utils.EncodeStringToBytes(entry.FileName)
		length := 88 + len(name)
		if i < len(info)-1 {
			length = utils.Roundup(length, 8)
		}

		di := make([]byte, length)
		if i < len(info)-1 {
			binary.LittleEndian.PutUint32(di[:4], uint32(length))
		}

		binary.LittleEndian.PutUint32(di[4:8], entry.FileIndex)
		binary.LittleEndian.PutUint64(di[8:16], utils.UnixToFiletime(entry.CreationTime))
		binary.LittleEndian.PutUint64(di[16:24], utils.UnixToFiletime(entry.LastAccessTime))
		binary.LittleEndian.PutUint64(di[24:32], utils.UnixToFiletime(entry.LastWriteTime))
		binary.LittleEndian.PutUint64(di[32:40], utils.UnixToFiletime(entry.ChangeTime))
		binary.LittleEndian.PutUint64(di[40:48], entry.EndOfFile)
		binary.LittleEndian.PutUint64(di[48:56], entry.AllocationSize)
		binary.LittleEndian.PutUint32(di[56:60], entry.FileAttributes)
		binary.LittleEndian.PutUint32(di[60:64], uint32(len(name)))
		binary.LittleEndian.PutUint32(di[64:68], entry.EaSize)
		binary.LittleEndian.PutUint32(di[68:72], entry.ReparsePointTag)
		copy(di[72:88], entry.FileID128)
		copy(di[88:88+len(name)], name)

		buf = append(buf, di...)
	}

	return buf
}

func (info fileID64ExtdDirInfo) encode() []byte {
	var buf []byte
	for i, entry := range info {
		name := utils.EncodeStringToBytes(entry.FileName)
		length := 80 + len(name)
		if i < len(info)-1 {
			length = utils.Roundup(length, 8)
		}

		di := make([]byte, length)
		if i < len(info)-1 {
			binary.LittleEndian.PutUint32(di[:4], uint32(length))
		}

		binary.LittleEndian.PutUint32(di[4:8], entry.FileIndex)
		binary.LittleEndian.PutUint64(di[8:16], utils.UnixToFiletime(entry.CreationTime))
		binary.LittleEndian.PutUint64(di[16:24], utils.UnixToFiletime(entry.LastAccessTime))
		binary.LittleEndian.PutUint64(di[24:32], utils.UnixToFiletime(entry.LastWriteTime))
		binary.LittleEndian.PutUint64(di[32:40], utils.UnixToFiletime(entry.ChangeTime))
		binary.LittleEndian.PutUint64(di[40:48], entry.EndOfFile)
		binary.LittleEndian.PutUint64(di[48:56], entry.AllocationSize)
		binary.LittleEndian.PutUint32(di[56:60], entry.FileAttributes)
		binary.LittleEndian.PutUint32(di[60:64], uint32(len(name)))
		binary.LittleEndian.PutUint32(di[64:68], entry.EaSize)
		binary.LittleEndian.PutUint32(di[68:72], entry.ReparsePointTag)
		binary.LittleEndian.PutUint64(di[72:80], entry.FileID64)
		copy(di[80:80+len(name)], name)

		buf = append(buf, di...)
	}

	return buf
}

func (info fileIDAllExtdDirInfo) encode() []byte {
	var buf []byte
	for i, entry := range info {
		name := utils.EncodeStringToBytes(entry.FileName)
		length := 96 + len(name)
		if i < len(info)-1 {
			length = utils.Roundup(length, 8)
		}

		di := make([]byte, length)
		if i < len(info)-1 {
			binary.LittleEndian.PutUint32(di[:4], uint32(length))
		}

		binary.LittleEndian.PutUint32(di[4:8], entry.FileIndex)
		binary.LittleEndian.PutUint64(di[8:16], utils.UnixToFiletime(entry.CreationTime))
		binary.LittleEndian.PutUint64(di[16:24], utils.UnixToFiletime(entry.LastAccessTime))
		binary.LittleEndian.PutUint64(di[24:32], utils.UnixToFiletime(entry.LastWriteTime))
		binary.LittleEndian.PutUint64(di[32:40], utils.UnixToFiletime(entry.ChangeTime))
		binary.LittleEndian.PutUint64(di[40:48], entry.EndOfFile)
		binary.LittleEndian.PutUint64(di[48:56], entry.AllocationSize)
		binary.LittleEndian.PutUint32(di[56:60], entry.FileAttributes)
		binary.LittleEndian.PutUint32(di[64:68], entry.EaSize)
		binary.LittleEndian.PutUint32(di[68:72], entry.ReparsePointTag)
		binary.LittleEndian.PutUint64(di[72:80], entry.FileID64)
		copy(di[80:96], entry.FileID128)

		binary.LittleEndian.PutUint32(di[60:64], uint32(len(name)))
		copy(di[96:96+len(name)], name)

		buf = append(buf, di...)
	}

	return buf
}

func (info fileIDAllExtdBothDirInfo) encode() []byte {
	var buf []byte
	for i, entry := range info {
		short := utils.EncodeStringToBytes(entry.ShortName)
		long := utils.EncodeStringToBytes(entry.FileName)
		length := 122 + len(long)
		if i < len(info)-1 {
			length = utils.Roundup(length, 8)
		}

		di := make([]byte, length)
		if i < len(info)-1 {
			binary.LittleEndian.PutUint32(di[:4], uint32(length))
		}

		binary.LittleEndian.PutUint32(di[4:8], entry.FileIndex)
		binary.LittleEndian.PutUint64(di[8:16], utils.UnixToFiletime(entry.CreationTime))
		binary.LittleEndian.PutUint64(di[16:24], utils.UnixToFiletime(entry.LastAccessTime))
		binary.LittleEndian.PutUint64(di[24:32], utils.UnixToFiletime(entry.LastWriteTime))
		binary.LittleEndian.PutUint64(di[32:40], utils.UnixToFiletime(entry.ChangeTime))
		binary.LittleEndian.PutUint64(di[40:48], entry.EndOfFile)
		binary.LittleEndian.PutUint64(di[48:56], entry.AllocationSize)
		binary.LittleEndian.PutUint32(di[56:60], entry.FileAttributes)
		binary.LittleEndian.PutUint32(di[64:68], entry.EaSize)
		binary.LittleEndian.PutUint32(di[68:72], entry.ReparsePointTag)
		binary.LittleEndian.PutUint64(di[72:80], entry.FileID64)
		copy(di[80:96], entry.FileID128)

		di[96] = uint8(len(short))
		copy(di[98:122], short)
		binary.LittleEndian.PutUint32(di[60:64], uint32(len(long)))
		copy(di[122:122+len(long)], long)

		buf = append(buf, di...)
	}

	return buf
}

// QueryDirectoryBuffer generates the query result depending on the provided parameters.
func QueryDirectoryBuffer(class uint8, entries []client.ObjectInfo, bufSize uint32, single, root bool, dir, parent client.FileInfo) (buf []byte, num int) {
	var info []dirInfo
	size := uint32(224) // The minimal size of the buffer for safety
	if bufSize < size {
		return nil, 0
	}

	if root { // "." and ".." directories need to be included in the response
		info = append(info,
			dirInfo{
				CreationTime:   dir.CreatedAt,
				LastAccessTime: dir.ModifiedAt,
				LastWriteTime:  dir.ModifiedAt,
				ChangeTime:     dir.ModifiedAt,
				FileAttributes: FILE_ATTRIBUTE_DIRECTORY,
				FileID64:       dir.ID64,
				FileID128:      dir.ID,
				FileName:       ".",
			},
			dirInfo{
				CreationTime:   parent.CreatedAt,
				LastAccessTime: parent.ModifiedAt,
				LastWriteTime:  parent.ModifiedAt,
				ChangeTime:     parent.ModifiedAt,
				FileAttributes: FILE_ATTRIBUTE_DIRECTORY,
				FileID64:       parent.ID64,
				FileID128:      parent.ID,
				FileName:       "..",
			},
		)
	}

	for i, entry := range entries {
		_, name, isDir := utils.ExtractFilename(entry.Key)
		length := 104 + uint32(len(name))*2

		// Check if the buffer length exceeds bufSize after adding the new record.
		if size+length > bufSize {
			break
		}

		di := dirInfo{
			CreationTime:   entry.CreatedAt,
			LastAccessTime: entry.ModifiedAt,
			LastWriteTime:  entry.ModifiedAt,
			ChangeTime:     entry.ModifiedAt,
			FileName:       name,
		}

		if isDir {
			di.FileAttributes = FILE_ATTRIBUTE_DIRECTORY
		} else {
			di.FileAttributes = FILE_ATTRIBUTE_NORMAL
			di.EndOfFile = entry.Size
			di.AllocationSize = entry.Size
		}

		hash := blake2b.Sum256([]byte(entry.Key))
		di.FileID64 = binary.LittleEndian.Uint64(hash[:8])
		di.FileID128 = make([]byte, 16)
		frand.Read(di.FileID128)

		info = append(info, di)
		num++
		if !single && i < len(entries)-1 && size+uint32(utils.Roundup(104+len(name)*2, 8)) <= bufSize {
			size += uint32(utils.Roundup(104+len(name)*2, 8))
		} else { // Either single entry requested or the buffer length exceeds bufSize
			break
		}
	}

	switch class {
	case FILE_BOTH_DIRECTORY_INFORMATION:
		return fileBothDirInfo(info).encode(), num
	case FILE_DIRECTORY_INFORMATION:
		return fileDirInfo(info).encode(), num
	case FILE_ID_64_EXTD_BOTH_DIRECTORY_INFORMATION:
		return fileID64ExtdBothDirInfo(info).encode(), num
	case FILE_ID_64_EXTD_DIRECTORY_INFORMATION:
		return fileID64ExtdDirInfo(info).encode(), num
	case FILE_ID_ALL_EXTD_BOTH_DIRECTORY_INFORMATION:
		return fileIDAllExtdBothDirInfo(info).encode(), num
	case FILE_ID_ALL_EXTD_DIRECTORY_INFORMATION:
		return fileIDAllExtdDirInfo(info).encode(), num
	case FILE_ID_BOTH_DIRECTORY_INFORMATION:
		return fileIDBothDirInfo(info).encode(), num
	case FILE_ID_EXTD_DIRECTORY_INFORMATION:
		return fileIDExtdDirInfo(info).encode(), num
	case FILE_ID_FULL_DIRECTORY_INFORMATION:
		return fileIDFullDirInfo(info).encode(), num
	default:
		return
	}
}

// QueryDirectoryResponse represents an SMB2_QUERY_DIRECTORY response.
type QueryDirectoryResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the SMB2_QUERY_DIRECTORY response.
func (qdr *QueryDirectoryResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(qdr.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2QueryDirectoryResponseStructureSize)
}

// SetOutputBuffer sets the Buffer field of the SMB2_QUERY_DIRECTORY response.
func (qdr *QueryDirectoryResponse) SetOutputBuffer(buf []byte) {
	binary.LittleEndian.PutUint16(qdr.data[SMB2HeaderSize+2:SMB2HeaderSize+4], uint16(len(qdr.data)))
	binary.LittleEndian.PutUint32(qdr.data[SMB2HeaderSize+4:SMB2HeaderSize+8], uint32(len(buf)))
	qdr.data = append(qdr.data, buf...)
}

// FromRequest implements GenericResponse interface.
func (qdr *QueryDirectoryResponse) FromRequest(req GenericRequest) {
	qdr.Response.FromRequest(req)

	body := make([]byte, SMB2QueryDirectoryResponseMinSize)
	qdr.data = append(qdr.data, body...)

	qdr.setStructureSize()
	Header(qdr.data).SetNextCommand(0)
	Header(qdr.data).SetStatus(STATUS_OK)
	if Header(qdr.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(qdr.data).SetCreditResponse(0)
	} else {
		Header(qdr.data).SetCreditResponse(max(req.Header().CreditCharge(), req.Header().CreditRequest()))
	}
}

// Generate populates the fields of the SMB2_QUERY_DIRECTORY response.
func (qdr *QueryDirectoryResponse) Generate(buf []byte) {
	qdr.SetOutputBuffer(buf)
}

// QueryInfoRequest represents an SMB2_QUERY_INFO request.
type QueryInfoRequest struct {
	Request
}

// Validate implements GenericRequest interface.
func (qir QueryInfoRequest) Validate(supportsMultiCredit bool) error {
	if err := Header(qir.data).Validate(); err != nil {
		return err
	}

	if len(qir.data) < SMB2HeaderSize+SMB2QueryInfoRequestMinSize {
		return ErrWrongLength
	}

	if qir.structureSize() != SMB2QueryInfoRequestStructureSize {
		return ErrWrongFormat
	}

	off := binary.LittleEndian.Uint16(qir.data[SMB2HeaderSize+8 : SMB2HeaderSize+10])
	length := binary.LittleEndian.Uint32(qir.data[SMB2HeaderSize+12 : SMB2HeaderSize+16])
	if uint32(off)+length > uint32(len(qir.data)) {
		return ErrInvalidParameter
	}

	// Validate CreditCharge.
	if supportsMultiCredit {
		sps := uint32(len(qir.data) - SMB2HeaderSize - SMB2QueryInfoRequestMinSize)
		ers := qir.OutputBufferLength()
		if qir.Header().CreditCharge() == 0 {
			if sps > 65536 || ers > 65536 {
				return ErrInvalidParameter
			}
		} else if qir.Header().CreditCharge() < uint16((max(sps, ers)-1)/65536)+1 {
			return ErrInvalidParameter
		}
	}

	return nil
}

// InfoType returns the InfoType field of the SMB2_QUERY_INFO request.
func (qir QueryInfoRequest) InfoType() uint8 {
	return qir.data[SMB2HeaderSize+2]
}

// FileInfoClass returns the FileInfoClass field of the SMB2_QUERY_INFO request.
func (qir QueryInfoRequest) FileInfoClass() uint8 {
	return qir.data[SMB2HeaderSize+3]
}

// OutputBufferLength returns the OutputBufferLength field of the SMB2_QUERY_INFO request.
func (qir QueryInfoRequest) OutputBufferLength() uint32 {
	return binary.LittleEndian.Uint32(qir.data[SMB2HeaderSize+4 : SMB2HeaderSize+8])
}

// InputBuffer returns the Buffer field of the SMB2_QUERY_INFO request.
func (qir QueryInfoRequest) InputBuffer() []byte {
	off := binary.LittleEndian.Uint16(qir.data[SMB2HeaderSize+8 : SMB2HeaderSize+10])
	length := binary.LittleEndian.Uint32(qir.data[SMB2HeaderSize+12 : SMB2HeaderSize+16])
	return qir.data[off : uint32(off)+length]
}

// AdditionalInformation returns the AdditionalInformation field of the SMB2_QUERY_INFO request.
func (qir QueryInfoRequest) AdditionalInformation() uint32 {
	return binary.LittleEndian.Uint32(qir.data[SMB2HeaderSize+16 : SMB2HeaderSize+20])
}

// Flags returns the Flags field of the SMB2_QUERY_INFO request.
func (qir QueryInfoRequest) Flags() uint32 {
	return binary.LittleEndian.Uint32(qir.data[SMB2HeaderSize+20 : SMB2HeaderSize+24])
}

// FileID returns the FileID field of the SMB2_QUERY_INFO request.
func (qir QueryInfoRequest) FileID() []byte {
	fid := make([]byte, 16)
	copy(fid, qir.data[SMB2HeaderSize+24:SMB2HeaderSize+40])
	return fid
}

// QueryInfoResponse represents an SMB2_QUERY_INFO response.
type QueryInfoResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the SMB2_QUERY_INFO response.
func (qir *QueryInfoResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(qir.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2QueryInfoResponseStructureSize)
}

// SetOutputBuffer sets the Buffer field of the SMB2_QUERY_INFO response.
func (qir *QueryInfoResponse) SetOutputBuffer(buf []byte) {
	binary.LittleEndian.PutUint16(qir.data[SMB2HeaderSize+2:SMB2HeaderSize+4], uint16(len(qir.data)))
	binary.LittleEndian.PutUint32(qir.data[SMB2HeaderSize+4:SMB2HeaderSize+8], uint32(len(buf)))
	qir.data = append(qir.data, buf...)
}

// FromRequest implements GenericResponse interface.
func (qir *QueryInfoResponse) FromRequest(req GenericRequest) {
	qir.Response.FromRequest(req)

	body := make([]byte, SMB2QueryInfoResponseMinSize)
	qir.data = append(qir.data, body...)

	qir.setStructureSize()
	Header(qir.data).SetNextCommand(0)
	Header(qir.data).SetStatus(STATUS_OK)
	if Header(qir.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(qir.data).SetCreditResponse(0)
	} else {
		Header(qir.data).SetCreditResponse(max(req.Header().CreditCharge(), req.Header().CreditRequest()))
	}
}

// Generate populates the fields of the SMB2_QUERY_INFO response.
func (qir *QueryInfoResponse) Generate(buf []byte) {
	qir.SetOutputBuffer(buf)
}

// FileFsVolumeInfo generates the output buffer for the FileFsVolumeInformation info class.
func FileFsVolumeInfo(createdAt time.Time, serialNo uint32, label string) []byte {
	vl := utils.EncodeStringToBytes(label)
	if len(vl) > 32 {
		vl = vl[:32]
	}

	info := make([]byte, 18+len(vl))
	binary.LittleEndian.PutUint64(info[:8], utils.UnixToFiletime(createdAt))
	binary.LittleEndian.PutUint32(info[8:12], serialNo)
	binary.LittleEndian.PutUint32(info[12:16], uint32(len(vl)))
	copy(info[18:18+len(vl)], vl)

	return info
}

// FileFsAttributeInfo generates the output buffer for the FileFsAttributeInformation info class.
func FileFsAttributeInfo(fsType string) []byte {
	name := utils.EncodeStringToBytes(fsType)
	info := make([]byte, 12+len(name))
	binary.LittleEndian.PutUint32(info[:4], 0x01100103)
	binary.LittleEndian.PutUint32(info[4:8], 255)
	binary.LittleEndian.PutUint32(info[8:12], uint32(len(name)))
	copy(info[12:12+len(name)], name)
	return info
}

// FileFsSizeInfo generates the output buffer for the FileFsSizeInformation info class.
func FileFsSizeInfo(si client.StorageInfo) []byte {
	var spu uint32
	if si.MinShards == 0 {
		spu = 1
	} else {
		spu = uint32(si.TotalShards / si.MinShards)
	}
	info := make([]byte, 24)
	binary.LittleEndian.PutUint64(info[:8], (si.RemainingStorage+si.UsedStorage)/BytesPerSector/uint64(spu))
	binary.LittleEndian.PutUint64(info[8:16], si.RemainingStorage/BytesPerSector/uint64(spu))
	binary.LittleEndian.PutUint32(info[16:20], spu)
	binary.LittleEndian.PutUint32(info[20:24], uint32(BytesPerSector))
	return info
}

// FileFsFullSizeInfo generates the output buffer for the FileFsFullSizeInformation info class.
func FileFsFullSizeInfo(si client.StorageInfo) []byte {
	var spu uint32
	if si.MinShards == 0 {
		spu = 1
	} else {
		spu = uint32(si.TotalShards / si.MinShards)
	}
	info := make([]byte, 32)
	binary.LittleEndian.PutUint64(info[:8], (si.RemainingStorage+si.UsedStorage)/BytesPerSector/uint64(spu))
	binary.LittleEndian.PutUint64(info[8:16], si.RemainingStorage/BytesPerSector/uint64(spu))
	binary.LittleEndian.PutUint64(info[16:24], si.RemainingStorage/BytesPerSector/uint64(spu))
	binary.LittleEndian.PutUint32(info[24:28], spu)
	binary.LittleEndian.PutUint32(info[28:32], uint32(BytesPerSector))
	return info
}

// FileFsDeviceInfo generates the output buffer for the FileFsDeviceInformation info class.
func FileFsDeviceInfo() []byte {
	buf := binary.LittleEndian.AppendUint32(nil, 0x00000007)
	buf = binary.LittleEndian.AppendUint32(buf, 0x00000030)
	return buf
}

// FileFsObjectIDInfo generates the output buffer for the FileFsObjectIDInformation info class.
func FileFsObjectIDInfo(volumeID uint64) []byte {
	id := make([]byte, 64)
	binary.LittleEndian.PutUint64(id[:8], volumeID)
	return id
}

// FileBasicInfo is the output buffer structure for the FileBasicInformation info class.
type FileBasicInfo struct {
	CreationTime   time.Time
	LastAccessTime time.Time
	LastWriteTime  time.Time
	ChangeTime     time.Time
	FileAttributes uint32
}

// Encode marshals the FileBasicInfo structure into a byte sequence.
func (fbi FileBasicInfo) Encode() []byte {
	buf := make([]byte, 40)
	binary.LittleEndian.PutUint64(buf[:8], utils.UnixToFiletime(fbi.CreationTime))
	binary.LittleEndian.PutUint64(buf[8:16], utils.UnixToFiletime(fbi.LastAccessTime))
	binary.LittleEndian.PutUint64(buf[16:24], utils.UnixToFiletime(fbi.LastWriteTime))
	binary.LittleEndian.PutUint64(buf[24:32], utils.UnixToFiletime(fbi.ChangeTime))
	binary.LittleEndian.PutUint32(buf[32:36], fbi.FileAttributes)
	return buf
}

// Decode unmarshals the buffer into a FileBasicInfo structure.
func (fbi *FileBasicInfo) Decode(buf []byte) error {
	if len(buf) < 40 {
		return ErrInvalidParameter
	}

	fbi.CreationTime = utils.FiletimeToUnix(binary.LittleEndian.Uint64(buf[:8]))
	fbi.LastAccessTime = utils.FiletimeToUnix(binary.LittleEndian.Uint64(buf[8:16]))
	fbi.LastWriteTime = utils.FiletimeToUnix(binary.LittleEndian.Uint64(buf[16:24]))
	fbi.ChangeTime = utils.FiletimeToUnix(binary.LittleEndian.Uint64(buf[24:32]))
	fbi.FileAttributes = binary.LittleEndian.Uint32(buf[32:36])

	return nil
}

// FileStandardInfo is the output buffer structure for the FileStandardInformation info class.
type FileStandardInfo struct {
	AllocationSize uint64
	EndOfFile      uint64
	NumberOfLinks  uint32
	DeletePending  bool
	Directory      bool
}

// Encode marshals the FileStandardInfo structure into a byte sequence.
func (fsi FileStandardInfo) Encode() []byte {
	buf := make([]byte, 24)
	binary.LittleEndian.PutUint64(buf[:8], fsi.AllocationSize)
	binary.LittleEndian.PutUint64(buf[8:16], fsi.EndOfFile)
	binary.LittleEndian.PutUint32(buf[16:20], fsi.NumberOfLinks)
	if fsi.DeletePending {
		buf[20] = 1
	}
	if fsi.Directory {
		buf[21] = 1
	}
	return buf
}

// FileInternalInfo is the output buffer structure for the FileInternalInformation info class.
type FileInternalInfo struct {
	IndexNumber uint64
}

// Encode marshals the FileInternalInfo structure into a byte sequence.
func (fii FileInternalInfo) Encode() []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, fii.IndexNumber)
	return buf
}

// FileEaInfo is the output buffer structure for the FileEaInformation info class.
type FileEaInfo struct {
	EaSize uint32
}

// Encode marshals the FileEaInfo structure into a byte sequence.
func (fei FileEaInfo) Encode() []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, fei.EaSize)
	return buf
}

// FileAccessInfo is the output buffer structure for the FileAccessInformation info class.
type FileAccessInfo struct {
	AccessFlags uint32
}

// Encode marshals the FileAccessInfo structure into a byte sequence.
func (fai FileAccessInfo) Encode() []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, fai.AccessFlags)
	return buf
}

// FilePositionInfo is the output buffer structure for the FilePositionInformation info class.
type FilePositionInfo struct {
	CurrentByteOffset uint64
}

// Encode marshals the FilePositionInfo structure into a byte sequence.
func (fpi FilePositionInfo) Encode() []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, fpi.CurrentByteOffset)
	return buf
}

// FileModeInfo is the output buffer structure for the FileModeInformation info class.
type FileModeInfo struct {
	Mode uint32
}

// Encode marshals the FileModeInfo structure into a byte sequence.
func (fmi FileModeInfo) Encode() []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, fmi.Mode)
	return buf
}

// FileAlignmentInfo is the output buffer structure for the FileAlignmentInformation info class.
type FileAlignmentInfo struct {
	AlignmentRequirement uint32
}

// Encode marshals the FileAlignmentInfo structure into a byte sequence.
func (fai FileAlignmentInfo) Encode() []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, fai.AlignmentRequirement)
	return buf
}

// FileNameInfo is the output buffer structure for the FileNameInformation info class.
type FileNameInfo struct {
	FileName string
}

// Encode marshals the FileNameInfo structure into a byte sequence.
func (fni FileNameInfo) Encode() []byte {
	name := utils.EncodeStringToBytes(fni.FileName)
	buf := make([]byte, len(name)+6)
	binary.LittleEndian.PutUint32(buf[:4], uint32(len(name)))
	copy(buf[4:], name)
	padLen := utils.Roundup(len(buf), 4)
	padding := make([]byte, padLen-len(buf))
	buf = append(buf, padding...)
	return buf
}

// FileAllInfo is the output buffer structure for the FileAllInformation info class.
type FileAllInfo struct {
	BasicInfo     FileBasicInfo
	StandardInfo  FileStandardInfo
	InternalInfo  FileInternalInfo
	EaInfo        FileEaInfo
	AccessInfo    FileAccessInfo
	PositionInfo  FilePositionInfo
	ModeInfo      FileModeInfo
	AlignmentInfo FileAlignmentInfo
	NameInfo      FileNameInfo
}

// Encode marshals the FileAllInfo structure into a byte sequence.
func (fai FileAllInfo) Encode() []byte {
	return append(
		append(
			append(
				append(
					append(
						append(
							append(
								append(
									fai.BasicInfo.Encode(),
									fai.StandardInfo.Encode()...,
								),
								fai.InternalInfo.Encode()...,
							),
							fai.EaInfo.Encode()...,
						),
						fai.AccessInfo.Encode()...,
					),
					fai.PositionInfo.Encode()...,
				),
				fai.ModeInfo.Encode()...,
			),
			fai.AlignmentInfo.Encode()...,
		),
		fai.NameInfo.Encode()...,
	)
}

// FileNetworkOpenInfo is the output buffer structure for the FileNetworkOpenInformation info class.
type FileNetworkOpenInfo struct {
	CreationTime   time.Time
	LastAccessTime time.Time
	LastWriteTime  time.Time
	ChangeTime     time.Time
	AllocationSize uint64
	EndOfFile      uint64
	FileAttributes uint32
}

// Encode marshals the FileNetworkOpenInfo structure into a byte sequence.
func (fnoi FileNetworkOpenInfo) Encode() []byte {
	buf := make([]byte, 56)
	binary.LittleEndian.PutUint64(buf[:8], utils.UnixToFiletime(fnoi.CreationTime))
	binary.LittleEndian.PutUint64(buf[8:16], utils.UnixToFiletime(fnoi.LastAccessTime))
	binary.LittleEndian.PutUint64(buf[16:24], utils.UnixToFiletime(fnoi.LastWriteTime))
	binary.LittleEndian.PutUint64(buf[24:32], utils.UnixToFiletime(fnoi.ChangeTime))
	binary.LittleEndian.PutUint64(buf[32:40], fnoi.AllocationSize)
	binary.LittleEndian.PutUint64(buf[40:48], fnoi.EndOfFile)
	binary.LittleEndian.PutUint32(buf[48:52], fnoi.FileAttributes)
	return buf
}

// FileNormalizedNameInfo is the output buffer structure for the FileNormalizedNameInformation info class.
type FileNormalizedNameInfo struct {
	Filename string
}

// Encode marshals the FileNormalizedNameInfo structure into a byte sequence.
func (fnni FileNormalizedNameInfo) Encode() []byte {
	fn := utils.EncodeStringToBytes(fnni.Filename)
	buf := make([]byte, len(fn)+4)
	binary.LittleEndian.PutUint32(buf[:4], uint32(len(fn)))
	copy(buf[4:], fn)
	return buf
}

// FileStreamInfo is the output buffer structure for the FileStreamInformation info class.
type FileStreamInfo struct {
	StreamName           string
	StreamSize           uint64
	StreamAllocationSize uint64
}

// Encode marshals the FileStreamInfo structure into a byte sequence.
func (fsi FileStreamInfo) Encode() []byte {
	name := utils.EncodeStringToBytes(fsi.StreamName)
	buf := make([]byte, 24)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(name)))
	binary.LittleEndian.PutUint64(buf[8:16], fsi.StreamSize)
	binary.LittleEndian.PutUint64(buf[16:24], fsi.StreamAllocationSize)
	buf = append(buf, name...)
	return buf
}

// ACE defines an Access Control Entry.
type ACE struct {
	Type   uint8
	Flags  uint8
	Access uint32
	SID    dtyp.SID
}

// Encode marshals the ACE into a byte sequence.
func (ace *ACE) Encode() []byte {
	var buf []byte
	buf = append(buf, ace.Type)
	buf = append(buf, ace.Flags)
	buf = binary.LittleEndian.AppendUint16(buf, 0)
	buf = binary.LittleEndian.AppendUint32(buf, ace.Access)
	sid, err := ndr.Marshal(&ace.SID)
	if err != nil {
		return nil
	}

	sid = sid[4:]
	buf = append(buf, sid...)
	binary.LittleEndian.PutUint16(buf[2:4], uint16(len(buf)))
	return buf
}

// ACL defines an Access Control List consisting of one or more ACEs.
type ACL struct {
	Revision uint16
	ACEs     []ACE
}

// Encode marshals the ACL into a byte sequence.
func (acl *ACL) Encode() []byte {
	var buf []byte
	buf = binary.LittleEndian.AppendUint16(buf, acl.Revision)
	var aceBuf []byte
	var count int
	for _, ace := range acl.ACEs {
		b := ace.Encode()
		if b != nil {
			aceBuf = append(aceBuf, b...)
			count++
		}
	}

	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(aceBuf)))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(count))
	buf = append(buf, aceBuf...)
	return buf
}

// SecInfo is a data structure for the SMB2_0_INFO_SECURITY info type.
type SecInfo struct {
	Revision uint16
	Type     uint16
	Owner    dtyp.SID
	Group    dtyp.SID
	SACL     ACL
	DACL     ACL
}

// Encode marshals the SecInfo structure into a byte sequence.
func (si *SecInfo) Encode() []byte {
	var buf []byte
	buf = binary.LittleEndian.AppendUint16(buf, si.Revision)
	buf = binary.LittleEndian.AppendUint16(buf, si.Type)
	var owner []byte
	var err error
	if si.Type&dtyp.OwnerDefaulted == 0 {
		owner, err = ndr.Marshal(&si.Owner)
		if err != nil {
			return nil
		}

		owner = owner[4:]
		buf = binary.LittleEndian.AppendUint32(buf, 20)
	} else {
		buf = binary.LittleEndian.AppendUint32(buf, 0)
	}

	var group []byte
	if si.Type&dtyp.GroupDefaulted == 0 {
		group, err = ndr.Marshal(&si.Group)
		if err != nil {
			return nil
		}

		group = group[4:]
		buf = binary.LittleEndian.AppendUint32(buf, 20+uint32(len(owner)))
	} else {
		buf = binary.LittleEndian.AppendUint32(buf, 0)
	}

	var sacl []byte
	if si.Type&dtyp.SACLPresent > 0 {
		sacl = si.SACL.Encode()
		buf = binary.LittleEndian.AppendUint32(buf, 20+uint32(len(owner)+len(group)))
	} else {
		buf = binary.LittleEndian.AppendUint32(buf, 0)
	}

	var dacl []byte
	if si.Type&dtyp.DACLPresent > 0 {
		dacl = si.DACL.Encode()
		buf = binary.LittleEndian.AppendUint32(buf, 20+uint32(len(owner)+len(group)+len(sacl)))
	} else {
		buf = binary.LittleEndian.AppendUint32(buf, 0)
	}

	buf = append(buf, owner...)
	buf = append(buf, group...)
	buf = append(buf, sacl...)
	buf = append(buf, dacl...)
	return buf
}

// NewSecInfo generates the output buffer for the SMB2_0_INFO_SECURITY info type request.
func NewSecInfo(ctx ntlm.SecurityContext, info uint32, access uint32) []byte {
	si := SecInfo{
		Revision: 1,
		Type:     dtyp.SelfRelative,
	}

	if info&OWNER_SECURITY_INFORMATION > 0 {
		si.Owner = dtyp.SID{
			Revision:          1,
			SubAuthorityCount: uint8(len(ctx.DomainSID.SubAuthority)) + 1,
			IDAuthority:       &dtyp.SIDIDAuthority{Value: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x05}},
			SubAuthority:      append(ctx.DomainSID.SubAuthority, ctx.UserRID),
		}
	}

	if info&GROUP_SECURITY_INFORMATION > 0 {
		si.Group = dtyp.SID{
			Revision:          1,
			SubAuthorityCount: 2,
			IDAuthority:       &dtyp.SIDIDAuthority{Value: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x16}},
			SubAuthority:      append([]uint32{2}, ctx.UserRID),
		}
	}

	if info&SACL_SECURITY_INFORMATION > 0 {
		si.Type |= dtyp.SACLPresent | dtyp.SACLProtected
		si.SACL = ACL{
			Revision: 2,
			ACEs:     []ACE{},
		}
	}

	if info&DACL_SECURITY_INFORMATION > 0 {
		si.Type |= dtyp.DACLPresent | dtyp.DACLProtected
		si.DACL = ACL{
			Revision: 2,
			ACEs: []ACE{
				{
					Type:   0,
					Flags:  0,
					Access: access,
					SID:    si.Owner,
				},
				{
					Type:   1,
					Flags:  0,
					Access: 0,
					SID:    si.Group,
				},
				{
					Type:   1,
					Flags:  0,
					Access: 0,
					SID: dtyp.SID{
						Revision:          1,
						SubAuthorityCount: 1,
						IDAuthority:       &dtyp.SIDIDAuthority{Value: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x01}},
						SubAuthority:      []uint32{0},
					},
				},
			},
		}
	}

	return si.Encode()
}
