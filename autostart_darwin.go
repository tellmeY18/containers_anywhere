//go:build darwin

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

func configureAutostartCmd(cmd *exec.Cmd) {}

func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", autostartLabel+".plist"), nil
}

func platformAutostartStatus(state string) autostartInfo {
	info := autostartInfo{Supported: true, Mechanism: "launchd user agent"}
	path, err := launchAgentPath()
	if err != nil {
		return autostartInfo{Mechanism: info.Mechanism, Note: err.Error()}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return info
	}
	if !bytes.Contains(data, []byte(autostartMarker)) {
		info.Note = "a launch agent with this name exists but is not managed by containers-anywhere"
		return info
	}
	exe, abs, err := autostartTarget(state)
	if err != nil {
		info.Note = err.Error()
		return info
	}
	if bytes.Equal(data, launchAgentPlist(exe, abs, autostartLabel)) {
		info.Enabled = true
	} else {
		info.Note = "the launch agent points at a different install or state directory"
	}
	return info
}

func launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func platformSetAutostart(state string, enable bool) error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}
	existing, readErr := os.ReadFile(path)
	exists := readErr == nil
	if exists && !bytes.Contains(existing, []byte(autostartMarker)) {
		return fmt.Errorf("%s exists but is not managed by containers-anywhere; not touching it", path)
	}
	target := launchdDomain() + "/" + autostartLabel
	if !enable {
		if !exists {
			return nil
		}
		if out, err := runAutostartCommand("launchctl", "bootout", target); err != nil {
			if out2, err2 := runAutostartCommand("launchctl", "unload", "-w", path); err2 != nil {
				_ = out2
				_ = out
				// not loaded is fine; the file removal below is what disables it
			}
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	exe, abs, err := autostartTarget(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if exists {
		_, _ = runAutostartCommand("launchctl", "bootout", target)
	}
	if err := os.WriteFile(path, launchAgentPlist(exe, abs, autostartLabel), 0o644); err != nil {
		return err
	}
	if out, err := runAutostartCommand("launchctl", "bootstrap", launchdDomain(), path); err != nil {
		if out2, err2 := runAutostartCommand("launchctl", "load", "-w", path); err2 != nil {
			return autostartCmdErr("launchctl bootstrap/load", append(out, out2...), err2)
		}
	}
	return nil
}
