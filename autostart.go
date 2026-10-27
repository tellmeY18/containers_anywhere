package main

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// autostartInfo describes whether the host launcher starts automatically.
type autostartInfo struct {
	Supported bool   `json:"supported"`
	Enabled   bool   `json:"enabled"`
	Mechanism string `json:"mechanism"`
	Note      string `json:"note,omitempty"`
}

const (
	autostartMarker   = "Managed by containers-anywhere autostart"
	autostartLabel    = "dev.containersanywhere.launcher"
	autostartUnitName = "containers-anywhere.service"
	autostartTaskName = "Containers Anywhere"
)

// runAutostartCommand runs an OS service-manager command. Tests replace it.
var runAutostartCommand = func(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	configureAutostartCmd(cmd)
	return cmd.CombinedOutput()
}

// autostartArgs are the arguments every autostart entry runs after the executable.
func autostartArgs(state string) []string {
	return []string{"serve", "--state", state, "--no-open", "--port", "0"}
}

// autostartTarget returns the absolute executable and state directory.
func autostartTarget(state string) (string, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return "", "", err
	}
	abs, err := filepath.Abs(state)
	if err != nil {
		return "", "", err
	}
	return exe, abs, nil
}

func autostartStatus(state string) autostartInfo {
	return platformAutostartStatus(state)
}

func setAutostart(state string, enable bool) error {
	return platformSetAutostart(state, enable)
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func launchAgentPlist(exe, state, label string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<!-- " + autostartMarker + " -->\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	b.WriteString("\t<key>Label</key>\n\t<string>" + xmlEscape(label) + "</string>\n")
	b.WriteString("\t<key>ManagedBy</key>\n\t<string>" + autostartMarker + "</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	b.WriteString("\t\t<string>" + xmlEscape(exe) + "</string>\n")
	for _, a := range autostartArgs(state) {
		b.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	b.WriteString("\t<key>ProcessType</key>\n\t<string>Background</string>\n")
	b.WriteString("\t<key>StandardOutPath</key>\n\t<string>" + xmlEscape(filepath.Join(state, "autostart.log")) + "</string>\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n\t<string>" + xmlEscape(filepath.Join(state, "autostart.log")) + "</string>\n")
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

// systemdQuote quotes one ExecStart word (systemd.syntax C-style quoting,
// plus %% and $$ so specifiers and variables are not expanded).
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$", "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func systemdUserUnit(exe, state string) []byte {
	words := []string{systemdQuote(exe)}
	for _, a := range autostartArgs(state) {
		words = append(words, systemdQuote(a))
	}
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Containers Anywhere launcher (" + autostartMarker + ")\n\n")
	b.WriteString("[Service]\nType=simple\nExecStart=" + strings.Join(words, " ") + "\n")
	b.WriteString("Restart=on-failure\nRestartSec=10\n\n")
	b.WriteString("[Install]\nWantedBy=default.target\n")
	return []byte(b.String())
}

// windowsQuoteArg quotes an argument per CommandLineToArgvW rules.
func windowsQuoteArg(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, c := range s {
		switch c {
		case '\\':
			slashes++
			continue
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes*2+1))
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
		}
		slashes = 0
		b.WriteRune(c)
	}
	b.WriteString(strings.Repeat(`\`, slashes*2))
	b.WriteByte('"')
	return b.String()
}

func windowsTaskArguments(state string) string {
	args := autostartArgs(state)
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = windowsQuoteArg(a)
	}
	return strings.Join(out, " ")
}

// windowsTaskXML returns the Task Scheduler XML as UTF-8 text declaring
// encoding="UTF-16". schtasks /XML only reliably accepts UTF-16, so callers
// MUST write it with encodeUTF16BOM (UTF-16LE with BOM) rather than as-is.
func windowsTaskXML(exe, state string) []byte {
	user := ""
	if d, u := os.Getenv("USERDOMAIN"), os.Getenv("USERNAME"); u != "" {
		user = u
		if d != "" {
			user = d + `\` + u
		}
	}
	userID := ""
	if user != "" {
		userID = "<UserId>" + xmlEscape(user) + "</UserId>"
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-16"?>` + "\n")
	b.WriteString(`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` + "\n")
	b.WriteString("  <RegistrationInfo><Description>" + autostartMarker + "</Description></RegistrationInfo>\n")
	b.WriteString("  <Triggers><LogonTrigger><Enabled>true</Enabled>" + userID + "</LogonTrigger></Triggers>\n")
	b.WriteString("  <Principals><Principal id=\"Author\">" + userID + "<LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>\n")
	b.WriteString("  <Settings>\n")
	b.WriteString("    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>\n")
	b.WriteString("    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\n")
	b.WriteString("    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>\n")
	b.WriteString("    <StartWhenAvailable>true</StartWhenAvailable>\n")
	b.WriteString("    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>\n")
	b.WriteString("    <RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure>\n")
	b.WriteString("    <Enabled>true</Enabled>\n")
	b.WriteString("  </Settings>\n")
	b.WriteString("  <Actions Context=\"Author\"><Exec><Command>" + xmlEscape(exe) + "</Command><Arguments>" + xmlEscape(windowsTaskArguments(state)) + "</Arguments></Exec></Actions>\n")
	b.WriteString("</Task>\n")
	return []byte(b.String())
}

// encodeUTF16BOM encodes UTF-8 text as UTF-16LE with a byte order mark.
func encodeUTF16BOM(b []byte) []byte {
	u := utf16.Encode([]rune(string(b)))
	out := make([]byte, 2, 2+len(u)*2)
	out[0], out[1] = 0xFF, 0xFE
	for _, c := range u {
		out = binary.LittleEndian.AppendUint16(out, c)
	}
	return out
}

// decodeUTF16Maybe decodes UTF-16 (BOM or NUL-interleaved) output, else returns it as-is.
func decodeUTF16Maybe(b []byte) string {
	if len(b) >= 2 && ((b[0] == 0xFF && b[1] == 0xFE) || (b[0] == 0xFE && b[1] == 0xFF)) {
		be := b[0] == 0xFE
		b = b[2:]
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			if be {
				u = append(u, binary.BigEndian.Uint16(b[i:]))
			} else {
				u = append(u, binary.LittleEndian.Uint16(b[i:]))
			}
		}
		return string(utf16.Decode(u))
	}
	return string(b)
}

func autostartCmdErr(what string, out []byte, err error) error {
	return fmt.Errorf("%s: %w: %s", what, err, strings.TrimSpace(string(out)))
}

var errAutostartUnsupported = fmt.Errorf("autostart is not supported on this operating system")
