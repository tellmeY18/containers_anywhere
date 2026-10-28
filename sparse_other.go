//go:build !darwin && !linux

package main

import "os"

// Other platforms archive the disk densely.
func dataExtents(*os.File, int64) ([]extent, bool) { return nil, false }
