package main

import (
	"bytes"
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// A name that begins with a dot is a name, not a kind of file. A macOS client writes the extended
// attributes and the resource fork of every file it copies into a sidecar named "._" and the file's
// own name, and it treats failure to write that sidecar as failure of the copy: refused, it errors,
// deletes what it had copied, and starts over somewhere else. The same names carry a desktop's folder
// settings, and somebody's dotfiles, and there is no telling them apart that is worth making.

// TestIntegrationTheSidecarOfACopiedFileCanBeWritten is the copy macOS could not finish. The sidecar
// was answered with STATUS_NOT_SUPPORTED, which is what ended every copy from a Mac — at a different
// point each time, since the sidecar is written whenever the client gets to it.
func TestIntegrationTheSidecarOfACopiedFileCanBeWritten(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	// The file, and then the sidecar beside it, as a copy does.
	file, _ := cl.create("clip.mp4", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(file).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create of the file was answered with %#x", status)
	}
	if _, err := cl.write(createdFileID(file), 0, bytes.Repeat([]byte("v"), 4096)); err != nil {
		t.Fatalf("the write of the file failed: %v", err)
	}
	if _, err := cl.closeHandle(createdFileID(file)); err != nil {
		t.Fatalf("the close of the file failed: %v", err)
	}

	sidecar, _ := cl.create("._clip.mp4", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(sidecar).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create of the sidecar was answered with %#x, want it made like any other file", status)
	}

	// AppleDouble, as the client writes it: the magic number, the version, and what follows.
	attrs := append([]byte{0x00, 0x05, 0x16, 0x07, 0x00, 0x02, 0x00, 0x00}, bytes.Repeat([]byte{0}, 24)...)
	if _, err := cl.write(createdFileID(sidecar), 0, attrs); err != nil {
		t.Fatalf("the write of the sidecar failed: %v", err)
	}

	closed, err := cl.closeHandle(createdFileID(sidecar))
	if err != nil {
		t.Fatalf("the close of the sidecar failed outright: %v", err)
	}
	if status := smb2.Header(closed).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the close of the sidecar was answered with %#x", status)
	}

	// What the client wrote is what the store holds. Answered but discarded — which is what a write
	// to such a name used to be — the client reads its own metadata back as nothing.
	if got := h.files.dataOf("._clip.mp4"); !bytes.Equal(got, attrs) {
		t.Errorf("the store holds %d bytes of the sidecar, want the %d that were written", len(got), len(attrs))
	}
}

// TestIntegrationADottedNameIsHiddenRatherThanRefused is how the clutter is kept out of sight without
// keeping the file out of the share. The convention is understood on the systems that use it and
// carried in the attribute for the systems that do not, which is what a listing shows.
func TestIntegrationADottedNameIsHiddenRatherThanRefused(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	for _, name := range []string{".DS_Store", "._clip.mp4", ".bashrc"} {
		created, _ := cl.create(name, smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
		if status := smb2.Header(created).Status(); status != smb2.STATUS_OK {
			t.Fatalf("the create of %q was answered with %#x, want it made", name, status)
		}

		op := h.srv.globalOpenTable[openIDOf(createdFileID(created))]
		if op == nil {
			t.Fatalf("the create of %q left no open behind it", name)
		}
		if attr := op.file.attributesNow(); attr&smb2.FILE_ATTRIBUTE_HIDDEN == 0 {
			t.Errorf("%q is %#x, want it marked hidden", name, attr)
		}
	}

	// And an ordinary name is not hidden for it.
	plain, _ := cl.create("clip.mp4", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	op := h.srv.globalOpenTable[openIDOf(createdFileID(plain))]
	if attr := op.file.attributesNow(); attr&smb2.FILE_ATTRIBUTE_HIDDEN != 0 {
		t.Errorf("an ordinary file is %#x, want it left alone", attr)
	}
}
