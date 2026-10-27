//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func configureAutostartCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

func platformAutostartStatus(state string) autostartInfo {
	info := autostartInfo{Supported: true, Mechanism: "Windows scheduled task"}
	out, err := runAutostartCommand("schtasks.exe", "/Query", "/TN", autostartTaskName, "/XML")
	if err != nil {
		return info
	}
	text := decodeUTF16Maybe(out)
	if !strings.Contains(text, autostartMarker) {
		info.Note = "a task with this name exists but is not managed by containers-anywhere"
		return info
	}
	exe, abs, err := autostartTarget(state)
	if err != nil {
		info.Note = err.Error()
		return info
	}
	if strings.Contains(text, xmlEscape(exe)) && strings.Contains(text, xmlEscape(windowsTaskArguments(abs))) {
		info.Enabled = true
	} else {
		info.Note = "the task points at a different install or state directory"
	}
	return info
}

func platformSetAutostart(state string, enable bool) error {
	out, qerr := runAutostartCommand("schtasks.exe", "/Query", "/TN", autostartTaskName, "/XML")
	exists := qerr == nil
	if exists && !strings.Contains(decodeUTF16Maybe(out), autostartMarker) {
		return fmt.Errorf("a scheduled task named %q exists but is not managed by containers-anywhere; not touching it", autostartTaskName)
	}
	if !enable {
		if !exists {
			return nil
		}
		if out, err := runAutostartCommand("schtasks.exe", "/Delete", "/TN", autostartTaskName, "/F"); err != nil {
			return autostartCmdErr("schtasks /Delete", out, err)
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
	f, err := os.CreateTemp("", "containers-anywhere-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(encodeUTF16BOM(windowsTaskXML(exe, abs))); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if out, err := runAutostartCommand("schtasks.exe", "/Create", "/XML", f.Name(), "/TN", autostartTaskName, "/F"); err != nil {
		return autostartCmdErr("schtasks /Create", out, err)
	}
	return nil
}
