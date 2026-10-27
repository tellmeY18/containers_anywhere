//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const linuxLingerNote = "to start at boot without logging in, run: loginctl enable-linger"

func configureAutostartCmd(cmd *exec.Cmd) {}

func systemdUnitPath() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "systemd", "user", autostartUnitName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", autostartUnitName), nil
}

func systemdUserAvailable() error {
	if out, err := runAutostartCommand("systemctl", "--user", "show-environment"); err != nil {
		return autostartCmdErr("systemd user instance unavailable", out, err)
	}
	return nil
}

func platformAutostartStatus(state string) autostartInfo {
	info := autostartInfo{Mechanism: "systemd user unit"}
	if err := systemdUserAvailable(); err != nil {
		info.Note = err.Error()
		return info
	}
	info.Supported = true
	info.Note = linuxLingerNote
	path, err := systemdUnitPath()
	if err != nil {
		info.Supported, info.Note = false, err.Error()
		return info
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return info
	}
	if !bytes.Contains(data, []byte(autostartMarker)) {
		info.Note = "a unit with this name exists but is not managed by containers-anywhere"
		return info
	}
	exe, abs, err := autostartTarget(state)
	if err == nil && bytes.Equal(data, systemdUserUnit(exe, abs)) {
		info.Enabled = true
	} else {
		info.Note = "the unit points at a different install or state directory; " + linuxLingerNote
	}
	return info
}

func platformSetAutostart(state string, enable bool) error {
	if err := systemdUserAvailable(); err != nil {
		return err
	}
	path, err := systemdUnitPath()
	if err != nil {
		return err
	}
	existing, readErr := os.ReadFile(path)
	exists := readErr == nil
	if exists && !bytes.Contains(existing, []byte(autostartMarker)) {
		return fmt.Errorf("%s exists but is not managed by containers-anywhere; not touching it", path)
	}
	if !enable {
		if !exists {
			return nil
		}
		_, _ = runAutostartCommand("systemctl", "--user", "disable", "--now", autostartUnitName)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		_, _ = runAutostartCommand("systemctl", "--user", "daemon-reload")
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
	if err := os.WriteFile(path, systemdUserUnit(exe, abs), 0o644); err != nil {
		return err
	}
	if out, err := runAutostartCommand("systemctl", "--user", "daemon-reload"); err != nil {
		return autostartCmdErr("systemctl daemon-reload", out, err)
	}
	if out, err := runAutostartCommand("systemctl", "--user", "enable", "--now", autostartUnitName); err != nil {
		return autostartCmdErr("systemctl enable", out, err)
	}
	return nil
}
