//go:build !windows

package main

import (
	"os"
	"os/exec"
)

func prepareGatewayProcess(cmd *exec.Cmd) error { return nil }

func privateGatewayDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}
