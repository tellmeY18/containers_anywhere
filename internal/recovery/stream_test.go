package recovery

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"strings"
	"testing"
)

func streamAttr(kind uint16, b []byte) []byte {
	out := make([]byte, 4)
	binary.LittleEndian.PutUint16(out, kind)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(b)))
	return append(out, b...)
}
func streamCmd(kind uint16, attrs ...[]byte) []byte {
	b := bytes.Join(attrs, nil)
	h := make([]byte, 10)
	binary.LittleEndian.PutUint32(h, uint32(len(b)))
	binary.LittleEndian.PutUint16(h[4:], kind)
	out := append(h, b...)
	crc := ^crc32.Update(^uint32(0), crc32.MakeTable(crc32.Castagnoli), out)
	binary.LittleEndian.PutUint32(out[6:], crc)
	return out
}
func testStream(cmds ...[]byte) []byte {
	header := append([]byte("btrfs-stream\x00"), 1, 0, 0, 0)
	header = append(header, streamCmd(1, streamAttr(15, []byte("system")), streamAttr(1, make([]byte, 16)), streamAttr(2, make([]byte, 8)))...)
	for _, c := range cmds {
		header = append(header, c...)
	}
	return append(header, streamCmd(21)...)
}

func TestStreamRejectsEscapeIncrementalAndExternalClone(t *testing.T) {
	for name, b := range map[string][]byte{
		"traversal":       testStream(streamCmd(3, streamAttr(15, []byte("../escape")))),
		"absolute":        testStream(streamCmd(3, streamAttr(15, []byte("/escape")))),
		"incremental":     testStream(streamCmd(2)),
		"device-node":     testStream(streamCmd(5)),
		"data-less":       testStream(streamCmd(22)),
		"external-clone":  testStream(streamCmd(16, streamAttr(15, []byte("destination")), streamAttr(20, bytes.Repeat([]byte{1}, 16)), streamAttr(21, make([]byte, 8)), streamAttr(22, []byte("file")))),
		"extra-subvolume": testStream(streamCmd(1, streamAttr(15, []byte("other")))),
		"hardlink-escape": testStream(streamCmd(10, streamAttr(15, []byte("link")), streamAttr(17, []byte("../other")))),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateStream(bytes.NewReader(b), "system"); err == nil {
				t.Fatal("unsafe stream accepted")
			}
		})
	}
	b := testStream(streamCmd(3, streamAttr(15, []byte("file"))))
	if err := validateStream(bytes.NewReader(b), "system"); err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 1
	if err := validateStream(bytes.NewReader(b), "system"); err == nil {
		t.Fatal("corrupt command accepted")
	}
}

func TestFullSendRootMetadataUsesEmptyPath(t *testing.T) {
	u64 := func(n uint64) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, n)
		return b
	}
	stamp := make([]byte, 12) // btrfs_timespec: u64 seconds, u32 nanoseconds
	binary.LittleEndian.PutUint64(stamp, 1794182400)
	binary.LittleEndian.PutUint32(stamp[8:], 123456789)
	// Like Linux get_cur_path() for the send root, PATH is present with a
	// zero-length value. Include normal file creation to exercise both forms.
	b := testStream(
		streamCmd(19, streamAttr(15, nil), streamAttr(6, u64(1000)), streamAttr(7, u64(1000))),
		streamCmd(18, streamAttr(15, nil), streamAttr(5, u64(0750))),
		streamCmd(13, streamAttr(15, nil), streamAttr(13, []byte("user.recovery")), streamAttr(14, []byte("root metadata"))),
		streamCmd(14, streamAttr(15, nil), streamAttr(13, []byte("user.recovery"))),
		streamCmd(3, streamAttr(15, []byte("data")), streamAttr(3, u64(257))),
		streamCmd(20, streamAttr(15, nil), streamAttr(9, stamp), streamAttr(10, stamp), streamAttr(11, stamp)),
	)
	if err := validateStream(bytes.NewReader(b), "system"); err != nil {
		t.Fatalf("valid root metadata rejected: %v", err)
	}
}

func TestRootPathExceptionCannotCreateLinkRenameOrClone(t *testing.T) {
	for name, command := range map[string][]byte{
		"create-root":      streamCmd(3, streamAttr(15, nil)),
		"mkdir-root":       streamCmd(4, streamAttr(15, nil)),
		"symlink-root":     streamCmd(8, streamAttr(15, nil), streamAttr(17, []byte("file"))),
		"rename-root-from": streamCmd(9, streamAttr(15, nil), streamAttr(16, []byte("file"))),
		"rename-root-to":   streamCmd(9, streamAttr(15, []byte("file")), streamAttr(16, nil)),
		"link-root":        streamCmd(10, streamAttr(15, nil), streamAttr(17, []byte("file"))),
		"link-from-root":   streamCmd(10, streamAttr(15, []byte("file")), streamAttr(17, nil)),
		"link-dot-root":    streamCmd(10, streamAttr(15, []byte("file")), streamAttr(17, []byte("."))),
		"unlink-root":      streamCmd(11, streamAttr(15, nil)),
		"rmdir-root":       streamCmd(12, streamAttr(15, nil)),
		"write-root":       streamCmd(15, streamAttr(15, nil), streamAttr(18, make([]byte, 8)), streamAttr(19, []byte("data"))),
		"clone-to-root":    streamCmd(16, streamAttr(15, nil), streamAttr(20, make([]byte, 16)), streamAttr(21, make([]byte, 8)), streamAttr(22, []byte("file"))),
		"clone-from-root":  streamCmd(16, streamAttr(15, []byte("file")), streamAttr(20, make([]byte, 16)), streamAttr(21, make([]byte, 8)), streamAttr(22, nil)),
		"truncate-root":    streamCmd(17, streamAttr(15, nil), streamAttr(4, make([]byte, 8))),
		"wrong-attribute":  streamCmd(18, streamAttr(15, nil), streamAttr(16, nil), streamAttr(5, make([]byte, 8))),
		"missing-path":     streamCmd(18, streamAttr(5, make([]byte, 8))),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateStream(bytes.NewReader(testStream(command)), "system"); err == nil {
				t.Fatal("root metadata exception admitted an unsafe command/path")
			}
		})
	}
}

func streamUint64Attr(kind uint16, value uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, value)
	return streamAttr(kind, b)
}

func socketCommand(path string, mode, device uint64) []byte {
	return streamCmd(7, streamAttr(15, []byte(path)), streamUint64Attr(3, 259), streamUint64Attr(5, mode), streamUint64Attr(8, device))
}

func TestFullSystemStreamPreservesInertRuntimeSocket(t *testing.T) {
	// Linux send creates a uniquely named socket with PATH/INO/MODE/RDEV,
	// renames it into the tree, then sets ordinary metadata. No stream command
	// connects to it. This models a stale kubelet CSI registration socket.
	path := "kubelet/plugins_registry/btrfs.csi.local-reg.sock"
	stamp := make([]byte, 12)
	binary.LittleEndian.PutUint64(stamp, 1794182400)
	b := testStream(
		streamCmd(4, streamAttr(15, []byte("kubelet")), streamUint64Attr(3, 257)),
		streamCmd(4, streamAttr(15, []byte("kubelet/plugins_registry")), streamUint64Attr(3, 258)),
		socketCommand("o259-7-0", 0140600, 0),
		streamCmd(9, streamAttr(15, []byte("o259-7-0")), streamAttr(16, []byte(path))),
		streamCmd(19, streamAttr(15, []byte(path)), streamUint64Attr(6, 0), streamUint64Attr(7, 0)),
		streamCmd(18, streamAttr(15, []byte(path)), streamUint64Attr(5, 0600)),
		streamCmd(20, streamAttr(15, []byte(path)), streamAttr(9, stamp), streamAttr(10, stamp), streamAttr(11, stamp)),
	)
	if err := validateStream(bytes.NewReader(b), "system"); err != nil {
		t.Fatalf("valid system socket stream rejected: %v", err)
	}
}

func TestSocketSupportDoesNotAdmitDevicesFIFOsOrMalformedNodes(t *testing.T) {
	for name, command := range map[string][]byte{
		"block-device":     socketCommand("socket", 0060600, 0),
		"character-device": socketCommand("socket", 0020600, 0),
		"fifo-mode":        socketCommand("socket", 0010600, 0),
		"regular-mode":     socketCommand("socket", 0100600, 0),
		"nonzero-device":   socketCommand("socket", 0140600, 1),
		"high-mode-bits":   socketCommand("socket", uint64(1)<<40|0140600, 0),
		"root-path":        socketCommand("", 0140600, 0),
		"dot-root":         socketCommand(".", 0140600, 0),
		"traversal":        socketCommand("../socket", 0140600, 0),
		"absolute-path":    socketCommand("/socket", 0140600, 0),
		"missing-inode":    streamCmd(7, streamAttr(15, []byte("socket")), streamUint64Attr(5, 0140600), streamUint64Attr(8, 0)),
		"malformed-mode":   streamCmd(7, streamAttr(15, []byte("socket")), streamUint64Attr(3, 259), streamAttr(5, []byte{1}), streamUint64Attr(8, 0)),
		"unexpected-data":  streamCmd(7, streamAttr(15, []byte("socket")), streamUint64Attr(3, 259), streamUint64Attr(5, 0140600), streamUint64Attr(8, 0), streamAttr(19, []byte("payload"))),
		"generic-mknod":    streamCmd(5, streamAttr(15, []byte("socket")), streamUint64Attr(3, 259), streamUint64Attr(5, 0140600), streamUint64Attr(8, 0)),
		"block-mknod":      streamCmd(5, streamAttr(15, []byte("device")), streamUint64Attr(3, 259), streamUint64Attr(5, 0060600), streamUint64Attr(8, 2048)),
		"mkfifo":           streamCmd(6, streamAttr(15, []byte("pipe")), streamUint64Attr(3, 259), streamUint64Attr(5, 0010600), streamUint64Attr(8, 0)),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateStream(bytes.NewReader(testStream(command)), "system"); err == nil {
				t.Fatal("socket exception admitted an unsafe or malformed special node")
			}
		})
	}
}

func TestSendFailureKeepsValidatorCauseAheadOfBrokenPipe(t *testing.T) {
	validationErr := validateStream(bytes.NewReader(testStream(streamCmd(6, streamAttr(15, []byte("pipe"))))), "system")
	if validationErr == nil {
		t.Fatal("expected FIFO rejection")
	}
	brokenPipe := errors.New("signal: broken pipe")
	err := sendErrors(brokenPipe, validationErr, "At subvol /recovery/points/test/system\n")
	if !errors.Is(err, validationErr) || !errors.Is(err, brokenPipe) {
		t.Fatalf("send lost an error cause: %v", err)
	}
	message := err.Error()
	if !strings.Contains(message, "FIFOs") || !strings.Contains(message, "At subvol") || strings.Index(message, validationErr.Error()) >= strings.Index(message, brokenPipe.Error()) {
		t.Fatalf("validator failure masked by secondary sender error: %v", err)
	}
	if sendErrors(nil, nil, "") != nil {
		t.Fatal("successful send reported failure")
	}
	if err := sendErrors(nil, validationErr, ""); !errors.Is(err, validationErr) {
		t.Fatal("standalone validator failure lost", err)
	}
	if err := sendErrors(brokenPipe, nil, "sender failed"); !errors.Is(err, brokenPipe) || !strings.Contains(err.Error(), "sender failed") {
		t.Fatal("standalone sender failure lost", err)
	}
}
