package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tellmeY18/containers_anywhere/internal/atomicfile"
)

const recoveryActivityFile = "cluster-recovery-active.json"

type recoveryActivity struct {
	UID string `json:"uid"`
}

// Persist outside data.img so an update cannot change the selected root between
// accepting a cold operation and executing it at the next boot.
func recordRecoveryActivity(state, uid string) error {
	if !validRecoveryPathID(uid) {
		return errors.New("invalid recovery operation identity")
	}
	if old, err := readRecoveryActivity(state); err == nil && old.UID != uid {
		return fmt.Errorf("recovery operation %s is already pending", old.UID)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return saveJSON(filepath.Join(state, recoveryActivityFile), recoveryActivity{UID: uid})
}

func readRecoveryActivity(state string) (recoveryActivity, error) {
	var activity recoveryActivity
	b, err := os.ReadFile(filepath.Join(state, recoveryActivityFile))
	if err != nil {
		return activity, err
	}
	if err := json.Unmarshal(b, &activity); err != nil || !validRecoveryPathID(activity.UID) {
		return activity, errors.New("recovery operation journal is invalid; preserve it for recovery")
	}
	return activity, nil
}

func finishRecoveryActivity(state, uid string) error {
	activity, err := readRecoveryActivity(state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if activity.UID != uid {
		return errors.New("recovery operation identity changed")
	}
	if err := os.Remove(filepath.Join(state, recoveryActivityFile)); err != nil {
		return err
	}
	return atomicfile.SyncDir(state)
}

func checkRecoveryIdle(state string) error {
	activity, err := readRecoveryActivity(state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("recovery operation %s is pending; start the selected appliance to finish it before updating", activity.UID)
}
