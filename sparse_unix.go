//go:build darwin || linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// dataExtents lists the regions of f that hold data. ok is false if the filesystem cannot say.
func dataExtents(f *os.File, size int64) ([]extent, bool) {
	fd := int(f.Fd())
	defer unix.Seek(fd, 0, unix.SEEK_SET) // probing moves the offset; a dense copy reads from the start
	var out []extent
	off := int64(0)
	for off < size {
		data, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if err == unix.ENXIO {
			break // no data from here to the end
		}
		if err != nil {
			return nil, false
		}
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil {
			return nil, false
		}
		if hole > size {
			hole = size
		}
		if hole > data {
			out = append(out, extent{Offset: data, Length: hole - data})
		}
		off = hole
	}
	return out, true
}
