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

type dataFormat struct {
	Contract string `json:"contract"`
}

// initDataFormat runs before anything else writes to the data disk. It never
// modifies existing data: it only stamps a brand-new, empty disk, and otherwise
// refuses to continue unless the disk already carries the supported contract.
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
	entries, err := os.ReadDir(varLib)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != "lost+found" {
			return fmt.Errorf("the data disk has existing contents (%s) but no format marker; refusing to start. Nothing was modified: use a new empty state directory", e.Name())
		}
	}
	b, err = json.Marshal(dataFormat{Contract: protocol.DataContract})
	if err != nil {
		return err
	}
	return atomicfile.WritePrivate(path, b)
}
