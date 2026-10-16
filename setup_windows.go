package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Only this fixed command runs elevated. Never accept paths, scripts or feature
// names from the browser or use the elevated identity for downloads/cluster data.
func runSystemSetup() int {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return 740
	}
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return 1
	}
	cmd := exec.Command(filepath.Join(dir, "dism.exe"), "/Online", "/Enable-Feature", "/FeatureName:HypervisorPlatform", "/All", "/NoRestart", "/Quiet")
	if err := cmd.Run(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			return e.ExitCode()
		}
		return 1
	}
	return 0
}

func setupWindowsHost(state string) (bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return false, err
	}
	// PowerShell is part of Windows; an encoded, fixed script avoids shell
	// interpolation of installation paths. UAC cancellation is reported distinctly.
	script := "$ErrorActionPreference='Stop'; try { $p = Start-Process -FilePath '" + strings.ReplaceAll(exe, "'", "''") + "' -ArgumentList 'setup-system' -Verb RunAs -Wait -PassThru; exit $p.ExitCode } catch { if ($_.Exception.NativeErrorCode -eq 1223) { exit 1223 }; exit 1 }"
	units := utf16.Encode([]rune(script))
	b := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[i*2:], u)
	}
	cmd := exec.Command(filepath.Join(dir, "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(b))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	code := 0
	if err := cmd.Run(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else {
			return false, err
		}
	}
	if code == 1223 {
		return false, fmt.Errorf("setup was cancelled or administrator approval was unavailable; choose Set up this computer to retry")
	}
	if code != 0 && code != 3010 {
		return false, fmt.Errorf("Windows could not enable virtualization (code %d). Connect to the internet and retry, or contact your administrator. Details: C:\\Windows\\Logs\\DISM\\dism.log", code)
	}
	if code == 3010 {
		if err := saveJSON(filepath.Join(state, "setup-restart.json"), time.Now()); err != nil {
			return true, fmt.Errorf("restart Windows to finish setup; could not save restart status: %w", err)
		}
		// This is HKCU of the original, unelevated user, even if UAC used a
		// different administrator account. No machine-wide persistent service.
		key, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\RunOnce`, registry.SET_VALUE)
		if err != nil {
			return true, fmt.Errorf("Windows setup succeeded. Restart, then reopen Containers Anywhere manually: %w", err)
		}
		defer key.Close()
		command := syscall.EscapeArg(exe) + " desktop --state " + syscall.EscapeArg(state)
		if err := key.SetStringValue("ContainersAnywhereSetup", command); err != nil {
			return true, fmt.Errorf("Windows setup succeeded. Restart, then reopen Containers Anywhere manually: %w", err)
		}
		return true, nil
	}
	if err := doctor(); err != nil {
		return false, fmt.Errorf("Windows components are installed, but hardware acceleration is still unavailable. Restart and retry. If it remains unavailable, firmware virtualization or your administrator's policy needs attention: %w", err)
	}
	return false, nil
}

func pendingSetupRestart(state string) bool {
	b, err := os.ReadFile(filepath.Join(state, "setup-restart.json"))
	if err != nil {
		return false
	}
	var requested time.Time
	if json.Unmarshal(b, &requested) != nil {
		return true
	}
	uptime, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetTickCount64").Call()
	booted := time.Now().Add(-time.Duration(uptime) * time.Millisecond)
	if booted.After(requested) {
		_ = os.Remove(filepath.Join(state, "setup-restart.json"))
		return false
	}
	return true
}

func restartWindowsHost() error {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	// No /f: do not force-close applications with unsaved user work.
	return exec.Command(filepath.Join(dir, "shutdown.exe"), "/r", "/t", "0").Run()
}
