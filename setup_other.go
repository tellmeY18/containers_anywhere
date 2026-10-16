//go:build !windows

package main

import "fmt"

func pendingSetupRestart(string) bool { return false }

func setupWindowsHost(string) (bool, error) {
	return false, fmt.Errorf("automatic Windows setup is only available on Windows")
}
func runSystemSetup() int       { return 1 }
func restartWindowsHost() error { return fmt.Errorf("restart this computer manually") }
