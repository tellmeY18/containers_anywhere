package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// guestRestoreCall talks to the guest agent's restore-point routes. Only the launcher's own
// authenticated surfaces reach it: a workload cannot ask for a restore.
func guestRestoreCall(ctx context.Context, c *http.Client, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://guest"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(string(bytes.TrimSpace(out)))
	}
	return out, nil
}

func runRestorePoint(state, operation, id string, acceptLoss bool) error {
	c, err := readControl(state)
	if err != nil {
		return errors.New("the appliance is not running for this state directory")
	}
	switch operation {
	case "status", "list", "":
		b, err := controlCall(c, "GET", "/control/restore-points", nil, 30*time.Second)
		if err == nil {
			_, err = os.Stdout.Write(append(b, '\n'))
		}
		return err
	case "restore":
		if id == "" {
			return errors.New("--id must be a restore point from `restore-point --operation list`")
		}
		if !acceptLoss {
			return errors.New("a restore returns the whole appliance (the cluster, its volumes and everything installed) to the state in the restore point and discards what was written since; the current state is kept as a restore point first. Require --accept-data-loss")
		}
		b, err := controlCall(c, "POST", "/control/restore-points/restore", []byte(fmt.Sprintf(`{"id":%q}`, id)), 60*time.Second)
		if err == nil {
			_, err = os.Stdout.Write(b)
		}
		return err
	}
	return errors.New("restore-point operation must be list or restore")
}
