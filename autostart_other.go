//go:build !darwin && !linux && !windows

package main

import "os/exec"

func configureAutostartCmd(cmd *exec.Cmd) {}

func platformAutostartStatus(state string) autostartInfo {
	return autostartInfo{Mechanism: "none", Note: "autostart is not supported on this operating system"}
}

func platformSetAutostart(state string, enable bool) error {
	return errAutostartUnsupported
}
