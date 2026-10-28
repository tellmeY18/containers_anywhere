package main

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"io"
	"os"
)

// The data disk is a sparse file: its size is a ceiling and only written blocks are real.
// A backup carries it as the map of its data extents ("data.img.map", written first)
// followed by "data.img" holding only those extents back to back, so a 100 GiB disk with
// 3 GiB of data is a 3 GiB archive and a restore does not materialise the rest. A backup
// without a map is a plain, dense image and restores as before.
const (
	sparseMapName   = "data.img.map"
	sparseMapLimit  = 16 << 20
	sparseMaxExtent = 1 << 20
)

type extent struct {
	Offset int64 `json:"o"`
	Length int64 `json:"l"`
}

type sparseMap struct {
	Version int      `json:"version"`
	Size    int64    `json:"size"`
	Extents []extent `json:"extents"`
}

func (m sparseMap) dataBytes() int64 {
	var n int64
	for _, e := range m.Extents {
		n += e.Length
	}
	return n
}

func (m sparseMap) validate() error {
	if m.Version != 1 || m.Size <= 0 || m.Size > maxDataDiskBytes || len(m.Extents) > sparseMaxExtent {
		return errors.New("invalid sparse map")
	}
	var end int64
	for _, e := range m.Extents {
		if e.Offset < end || e.Length <= 0 || e.Offset+e.Length > m.Size || e.Offset+e.Length < e.Offset {
			return errors.New("invalid sparse map")
		}
		end = e.Offset + e.Length
	}
	return nil
}

// writeSparseDisk archives f as a map plus its data extents. It reports false, having
// written nothing, when the file is not sparse or the platform cannot tell.
func writeSparseDisk(tw *tar.Writer, f *os.File, size int64) (bool, error) {
	extents, ok := dataExtents(f, size)
	if !ok {
		return false, nil
	}
	m := sparseMap{Version: 1, Size: size, Extents: extents}
	if m.dataBytes() >= size || len(extents) > sparseMaxExtent {
		return false, nil
	}
	mb, err := json.Marshal(m)
	if err != nil || len(mb) > sparseMapLimit {
		return false, nil
	}
	if err := tw.WriteHeader(&tar.Header{Name: sparseMapName, Mode: 0600, Size: int64(len(mb))}); err != nil {
		return true, err
	}
	if _, err := tw.Write(mb); err != nil {
		return true, err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "data.img", Mode: 0600, Size: m.dataBytes()}); err != nil {
		return true, err
	}
	for _, e := range extents {
		if _, err := io.Copy(tw, io.NewSectionReader(f, e.Offset, e.Length)); err != nil {
			return true, err
		}
	}
	return true, nil
}

// readSparseDisk rebuilds the disk from its extents into f, leaving the holes as holes.
func readSparseDisk(f *os.File, r io.Reader, m sparseMap, archived int64) error {
	if err := m.validate(); err != nil {
		return err
	}
	if m.dataBytes() != archived {
		return errors.New("the sparse map does not match the data")
	}
	if err := f.Truncate(m.Size); err != nil {
		return err
	}
	for _, e := range m.Extents {
		if _, err := f.Seek(e.Offset, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(f, r, e.Length); err != nil {
			return err
		}
	}
	return nil
}
