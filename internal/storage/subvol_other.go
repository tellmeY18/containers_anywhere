//go:build !darwin && !linux

package storage

import "os"

func isSubvolumeRoot(os.FileInfo) bool { return false }
