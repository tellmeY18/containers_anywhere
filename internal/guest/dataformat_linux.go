package guest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tellmeY18/containers_anywhere/internal/atomicfile"
	"github.com/tellmeY18/containers_anywhere/internal/protocol"
)

const dataFormatFile = "containers-anywhere/data-format.json"

// Directories that only a previous appliance generation creates.
var applicationData = []string{"rancher", "containers-anywhere"}

type dataFormat struct {
	Contract string `json:"contract"`
}

// initDataFormat runs before the cluster or any application writes to the data
// disk. It never modifies existing data: it only stamps a disk that holds no
// cluster or application data, and otherwise refuses to continue unless the disk already carries the supported contract.
func initDataFormat(varLib string) error {
	path := filepath.Join(varLib, dataFormatFile)
	b, err := os.ReadFile(path)
	if err == nil {
		var f dataFormat
		if json.Unmarshal(b, &f) != nil || f.Contract != protocol.DataContract {
			return fmt.Errorf("the data disk uses format %q but this appliance supports %q; refusing to start. Nothing was modified: use the matching appliance or a new empty state directory", f.Contract, protocol.DataContract)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	// Early boot services (networkd, systemd, NixOS) legitimately create their own
	// directories under /var/lib before this runs, so "empty" cannot mean empty.
	// What must never be adopted unmarked is existing cluster or application data.
	for _, name := range applicationData {
		if _, err := os.Lstat(filepath.Join(varLib, name)); err == nil {
			return fmt.Errorf("the data disk already holds %s but has no format marker; refusing to start. Nothing was modified: use a new empty state directory", name)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	b, err = json.Marshal(dataFormat{Contract: protocol.DataContract})
	if err != nil {
		return err
	}
	return atomicfile.WritePrivate(path, b)
}
