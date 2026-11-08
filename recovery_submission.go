package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	"github.com/tellmeY18/containers_anywhere/internal/recovery"
)

var recoverySubmissionMu sync.Mutex
var recoveryStopping atomic.Bool

// Every app/controller/control entry point shares this interlock. Shutdown marks
// the session closing and waits for in-flight submissions before stopping the
// guest, so an update cannot slip between a guest queue write and its host marker.
func submitRecoveryOperation(ctx context.Context, state string, client *http.Client, req recovery.Request) (recovery.Operation, error) {
	recoverySubmissionMu.Lock()
	defer recoverySubmissionMu.Unlock()
	if recoveryStopping.Load() {
		return recovery.Operation{}, errors.New("the appliance is stopping; retry recovery after it starts")
	}
	if req.UID == "" {
		req.UID = req.ID
	}
	if !validRecoveryPathID(req.UID) || (req.ID != "" && req.ID != req.UID) || (req.Action != "capture" && req.Action != "restore") || (req.Scope != "system" && req.Scope != "full") {
		return recovery.Operation{}, errors.New("invalid recovery operation identity, action or scope")
	}
	if err := checkUpdateStart(state); err != nil {
		return recovery.Operation{}, err
	}
	if swap, err := readSwap(state); err == nil && swap.Phase == "trying" {
		return recovery.Operation{}, errors.New("finish the root swap trial before requesting recovery")
	} else if err != nil && !os.IsNotExist(err) {
		return recovery.Operation{}, err
	}
	if err := recordRecoveryActivity(state, req.UID); err != nil {
		return recovery.Operation{}, err
	}
	var result recovery.Operation
	err := recoveryJSON(ctx, client, "POST", "/operations", req, &result)
	if err == nil && result.Request.UID != req.UID {
		err = errors.New("guest returned another recovery operation")
	}
	return result, err
}
