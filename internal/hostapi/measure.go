package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// DirSize adds up regular file sizes beneath root, stopping at the deadline. It
// never follows symlinks and reports whether it finished, so a slow disk yields a
// labelled partial figure instead of a stalled agent.
func DirSize(ctx context.Context, root string, budget time.Duration) (size int64, complete bool) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	complete = true
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			complete = false
			return ctx.Err()
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return nil // unreadable entries are skipped, not fatal
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		complete = false
	}
	return size, complete
}

// ImageBytes sums the sizes in `crictl images -o json`.
func ImageBytes(raw []byte) (int64, error) {
	var out struct {
		Images []struct {
			ID   string          `json:"id"`
			Size json.RawMessage `json:"size"`
		} `json:"images"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, err
	}
	var total int64
	for _, img := range out.Images {
		var n int64
		if err := json.Unmarshal(img.Size, &n); err != nil {
			var s string
			if err2 := json.Unmarshal(img.Size, &s); err2 != nil {
				return 0, errors.New("unrecognised image size")
			}
			for _, ch := range s {
				if ch < '0' || ch > '9' {
					return 0, errors.New("unrecognised image size")
				}
				n = n*10 + int64(ch-'0')
			}
		}
		total += n
	}
	return total, nil
}

// PruneResult reports what a prune reclaimed; it never touches volumes.
type PruneResult struct {
	ImagesBefore int64 `json:"imageBytesBefore"`
	ImagesAfter  int64 `json:"imageBytesAfter"`
	Reclaimed    int64 `json:"reclaimedBytes"`
}

func NewPruneResult(before, after int64) PruneResult {
	r := PruneResult{ImagesBefore: before, ImagesAfter: after}
	if before > after {
		r.Reclaimed = before - after
	}
	return r
}
