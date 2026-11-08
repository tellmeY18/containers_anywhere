package recovery

import (
	"fmt"
	"os"
	"path/filepath"
)

// NeedsRestoreResume is a read-only preflight for init-data, before the live
// data-format guard. It never constructs an Engine, creates directories/locks,
// rewrites records, or cleans staging. Only a checksummed, contract-valid,
// already-destructive restore journal can bypass the initial live marker check.
// The cold engine still validates source/safety snapshots before changing data.
// Call while boot ordering excludes agent/K3s writers; this is not a live lock.
func NeedsRestoreResume(root string) (bool, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return false, fmt.Errorf("%w: recovery root must be a clean absolute path", ErrInconsistent)
	}
	st, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspecting recovery root: %v", ErrInconsistent, err)
	}
	if !st.IsDir() {
		return false, fmt.Errorf("%w: recovery root is not a real directory", ErrInconsistent)
	}
	ops, err := readOperations(root)
	if os.IsNotExist(err) {
		// A crash during the very first mkdir can leave an empty root. An
		// established catalog without its operations directory is ambiguous.
		entries, listErr := os.ReadDir(root)
		if listErr == nil && len(entries) == 0 {
			return false, nil
		}
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspecting recovery journals: %v", ErrInconsistent, err)
	}
	var pending int
	resume := false
	for _, op := range ops {
		if active(op) {
			pending++
		}
		if op.Request.Action == "restore" && (op.Phase == "restoring" || op.Phase == "rolling-back") {
			resume = true
		}
	}
	if pending > 1 {
		return false, fmt.Errorf("%w: multiple active recovery operations", ErrInconsistent)
	}
	return resume, nil
}
