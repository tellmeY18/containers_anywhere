package recovery

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"path"
	"strings"
)

// validateStream accepts one complete protocol-v1 full send stream. In
// particular it disallows incremental parents, data-less UPDATE_EXTENTs, and
// clones from other UUIDs. The receiver is additionally confined by chroot.
func validateStream(r io.Reader, name string) error {
	hdr := make([]byte, 17)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	if string(hdr[:13]) != "btrfs-stream\x00" || binary.LittleEndian.Uint32(hdr[13:]) != 1 {
		return errors.New("unsupported Btrfs stream header")
	}
	var uuid, trans []byte
	first := true
	table := crc32.MakeTable(crc32.Castagnoli)
	for {
		h := make([]byte, 10)
		if _, err := io.ReadFull(r, h); err != nil {
			return err
		}
		n := binary.LittleEndian.Uint32(h[:4])
		cmd := binary.LittleEndian.Uint16(h[4:6])
		want := binary.LittleEndian.Uint32(h[6:])
		if n > 1<<20 {
			return errors.New("oversized Btrfs stream command")
		}
		payload := make([]byte, int(n))
		if _, err := io.ReadFull(r, payload); err != nil {
			return err
		}
		clear(h[6:])
		crc := ^crc32.Update(^uint32(0), table, append(h, payload...))
		if crc != want {
			return errors.New("Btrfs command checksum mismatch")
		}
		attrs := map[uint16][]byte{}
		for p := payload; len(p) > 0; {
			if len(p) < 4 {
				return errors.New("truncated Btrfs attribute")
			}
			kind := binary.LittleEndian.Uint16(p[:2])
			size := int(binary.LittleEndian.Uint16(p[2:4]))
			p = p[4:]
			if size > len(p) || kind == 0 || kind > 24 {
				return errors.New("invalid Btrfs attribute")
			}
			if _, ok := attrs[kind]; ok {
				return errors.New("duplicate Btrfs attribute")
			}
			attrs[kind] = p[:size]
			p = p[size:]
		}
		if first {
			if cmd != 1 || string(attrs[15]) != name || len(attrs[1]) != 16 || len(attrs[2]) != 8 {
				return errors.New("stream must create exactly its declared full subvolume")
			}
			uuid = append([]byte(nil), attrs[1]...)
			trans = append([]byte(nil), attrs[2]...)
			first = false
			continue
		}
		if cmd < 3 || cmd > 21 {
			return errors.New("incremental, data-less or unknown Btrfs command")
		}
		// A device node could turn a later WRITE into access to the live block
		// device even inside a chroot; FIFOs can block the receiver indefinitely.
		if cmd == 5 || cmd == 6 {
			return fmt.Errorf("portable recovery does not support device nodes or FIFOs (command %d)", cmd)
		}
		if cmd == 7 {
			// Linux send emits PATH, INO, MODE and RDEV for MKSOCK. The
			// btrfs-progs receiver hardcodes mknod(path, 0600|S_IFSOCK, 0):
			// this creates an inert socket inode, never a device or a connection.
			// Validate even the MODE/RDEV attributes it ignores, so this
			// exception cannot smuggle a different special-node type.
			if len(attrs) != 4 || len(attrs[3]) != 8 || len(attrs[5]) != 8 || len(attrs[8]) != 8 {
				return errors.New("invalid MKSOCK attributes")
			}
			mode := binary.LittleEndian.Uint64(attrs[5])
			if mode&0170000 != 0140000 || mode & ^uint64(0177777) != 0 || binary.LittleEndian.Uint64(attrs[8]) != 0 {
				return errors.New("MKSOCK must describe a socket inode with zero device number")
			}
		}
		if cmd == 21 {
			if len(payload) != 0 {
				return errors.New("invalid end marker")
			}
			var tail [1]byte
			n, err := r.Read(tail[:])
			if n != 0 || err != io.EOF {
				return errors.New("trailing Btrfs stream data")
			}
			return nil
		}
		if _, ok := attrs[15]; !ok {
			return errors.New("Btrfs command is missing its path")
		}
		for _, attr := range []uint16{15, 16, 22} {
			if p, ok := attrs[attr]; ok && !streamPath(string(p)) {
				// Linux send represents the subvolume root by an empty PATH
				// for metadata changes. Never extend that exception to a
				// rename/link/clone endpoint or to content/creation commands.
				if attr == 15 && len(p) == 0 && rootMetadataCommand(cmd) {
					continue
				}
				return errors.New("unsafe Btrfs stream path")
			}
		}
		if cmd == 10 && !streamPath(string(attrs[17])) {
			return errors.New("unsafe Btrfs hardlink")
		}
		if cmd == 16 && (!bytes.Equal(attrs[20], uuid) || !bytes.Equal(attrs[21], trans) || !streamPath(string(attrs[22]))) {
			return errors.New("cross-subvolume clone is not a self-contained export")
		}
	}
}

func rootMetadataCommand(cmd uint16) bool {
	switch cmd {
	case 13, 14, 18, 19, 20: // SET_XATTR, REMOVE_XATTR, CHMOD, CHOWN, UTIMES
		return true
	default:
		return false
	}
}

func streamPath(p string) bool {
	return p != "" && p != "." && p != ".." && !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "../") && !strings.ContainsRune(p, 0) && path.Clean(p) == p
}
