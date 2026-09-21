package main

import (
	"encoding/binary"
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// The attributes of OneDrive's online-only items, which Explorer copies onto what it creates.
const (
	cloudFile = smb2.FILE_ATTRIBUTE_ARCHIVE | smb2.FILE_ATTRIBUTE_SPARSE_FILE | smb2.FILE_ATTRIBUTE_REPARSE_POINT |
		smb2.FILE_ATTRIBUTE_OFFLINE | smb2.FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS | smb2.FILE_ATTRIBUTE_UNPINNED
	cloudDir = smb2.FILE_ATTRIBUTE_DIRECTORY | smb2.FILE_ATTRIBUTE_REPARSE_POINT |
		smb2.FILE_ATTRIBUTE_RECALL_ON_OPEN | smb2.FILE_ATTRIBUTE_UNPINNED
)

// basicInfo is a FILE_BASIC_INFORMATION that leaves the times alone and sets the attributes.
func basicInfo(attributes uint32) []byte {
	buf := make([]byte, 40)
	binary.LittleEndian.PutUint32(buf[32:36], attributes)
	return buf
}

// attributesOf is what the server says the file behind the handle is.
func attributesOf(t *testing.T, cl *testClient, fid []byte) uint32 {
	t.Helper()

	return binary.LittleEndian.Uint32(queriedInfo(t, cl.queryInfo(fid, smb2.FileNetworkOpenInformation, 4096))[48:52])
}

// TestIntegrationCopyingOneDriveItemsLeavesTheirCloudAttributesBehind is Explorer copying online-only
// OneDrive items: the copies are plain files and folders on the share, not placeholders.
func TestIntegrationCopyingOneDriveItemsLeavesTheirCloudAttributesBehind(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	setAttrs := func(fid []byte, attrs uint32) {
		t.Helper()
		resp, err := cl.setInfo(fid, smb2.FileBasicInformation, basicInfo(attrs))
		if err != nil || smb2.Header(resp).Status() != smb2.STATUS_OK {
			t.Fatalf("setting attributes %#x failed: %v", attrs, err)
		}
	}

	dir := createdFileID(cl.createWithOptions("docs", smb2.FILE_CREATE, smb2.FILE_DIRECTORY_FILE))
	setAttrs(dir, cloudDir)

	file := createdFileID(cl.createWithOptions("docs/report.pdf", smb2.FILE_CREATE, 0))
	if _, err := cl.write(file, 0, []byte("the report")); err != nil {
		t.Fatalf("the write failed: %v", err)
	}
	setAttrs(file, cloudFile)

	empty := createdFileID(cl.createWithOptions("docs/empty.txt", smb2.FILE_CREATE, 0))
	setAttrs(empty, cloudFile)
	if _, err := cl.closeHandle(empty); err != nil {
		t.Fatalf("closing the empty file failed: %v", err)
	}

	other := h.dial("alice")
	for _, c := range []struct {
		what  string
		attrs uint32
		want  uint32
	}{
		{"the folder being copied into", attributesOf(t, cl, dir), smb2.FILE_ATTRIBUTE_DIRECTORY},
		{"the file being copied", attributesOf(t, cl, file), smb2.FILE_ATTRIBUTE_ARCHIVE},
		{"the empty file copied", attributesOf(t, other, createdFileID(other.createWithOptions("docs/empty.txt", smb2.FILE_OPEN, 0))), smb2.FILE_ATTRIBUTE_ARCHIVE},
	} {
		if c.attrs != c.want {
			t.Errorf("%s has attributes %#x, want %#x", c.what, c.attrs, c.want)
		}
	}
}

// TestIntegrationSettingAttributesKeepsAFolderAFolder is Explorer copying an online-only folder: it
// sets just FILE_ATTRIBUTE_UNPINNED, and a copy no longer reported as a folder is left empty.
func TestIntegrationSettingAttributesKeepsAFolderAFolder(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	dir := createdFileID(cl.createWithOptions("docs", smb2.FILE_CREATE, smb2.FILE_DIRECTORY_FILE))
	for _, attrs := range []uint32{smb2.FILE_ATTRIBUTE_UNPINNED, smb2.FILE_ATTRIBUTE_HIDDEN} {
		if resp, err := cl.setInfo(dir, smb2.FileBasicInformation, basicInfo(attrs)); err != nil || smb2.Header(resp).Status() != smb2.STATUS_OK {
			t.Fatalf("setting attributes %#x failed: %v", attrs, err)
		}
	}

	if got, want := attributesOf(t, cl, dir), uint32(smb2.FILE_ATTRIBUTE_DIRECTORY|smb2.FILE_ATTRIBUTE_HIDDEN); got != want {
		t.Errorf("the folder has attributes %#x, want %#x", got, want)
	}
}

// TestIntegrationANamedStreamIsRefused is Explorer copying a file's alternate data stream. Stored as
// a file of its own, it outlived the file and kept the folder from being deleted.
func TestIntegrationANamedStreamIsRefused(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	fid := createdFileID(cl.createWithOptions("mail.eml", smb2.FILE_CREATE, 0))
	if _, err := cl.write(fid, 0, []byte("the mail")); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	stream, err := cl.createErr("mail.eml:Properties", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OVERWRITE_IF)
	if err != nil {
		t.Fatalf("the stream's create failed outright: %v", err)
	}
	if status := smb2.Header(stream).Status(); status != smb2.STATUS_OBJECT_NAME_INVALID {
		t.Errorf("the stream's create was answered with %#x, want it refused", status)
	}

	renamed, err := cl.rename(fid, "mail.eml:Properties")
	if err != nil {
		t.Fatalf("the rename failed outright: %v", err)
	}
	if status := smb2.Header(renamed).Status(); status != smb2.STATUS_OBJECT_NAME_INVALID {
		t.Errorf("the rename to a stream was answered with %#x, want it refused", status)
	}
}
