package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var autostartPaths = []struct{ name, exe, state string }{
	{"plain", "/usr/local/bin/ca", "/var/state"},
	{"spaces", "/Users/a b/My App/ca", "/Users/a b/State Dir"},
	{"amp", "/opt/A&B/ca", "/opt/A&B/st"},
	{"quotes", `/opt/it's "x"/ca`, `/opt/o'neil "q"/st`},
	{"unicode", "/home/jörg/アプリ/ca", "/home/jörg/状態"},
	{"percent", "/opt/100%/ca", "/opt/$HOME/st"},
}

func autostartXMLCheck(t *testing.T, data []byte) {
	t.Helper()
	d := xml.NewDecoder(bytes.NewReader(data))
	d.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	for {
		if _, err := d.Token(); err == io.EOF {
			return
		} else if err != nil {
			t.Fatalf("not well-formed: %v\n%s", err, data)
		}
	}
}

func autostartUnquote(t *testing.T, line string) []string {
	var words []string
	for i := 0; i < len(line); {
		if line[i] == ' ' {
			i++
			continue
		}
		if line[i] != '"' {
			t.Fatalf("unquoted word in %q", line)
		}
		i++
		var w strings.Builder
		for ; line[i] != '"'; i++ {
			if line[i] == '\\' {
				i++
				if line[i] == 'n' {
					w.WriteByte('\n')
					continue
				}
			}
			w.WriteByte(line[i])
		}
		i++
		words = append(words, strings.NewReplacer("%%", "%", "$$", "$").Replace(w.String()))
	}
	return words
}

func TestAutostartBuilders(t *testing.T) {
	for _, c := range autostartPaths {
		t.Run(c.name, func(t *testing.T) {
			p := launchAgentPlist(c.exe, c.state, autostartLabel)
			autostartXMLCheck(t, p)
			if !bytes.Contains(p, []byte(autostartMarker)) || !bytes.Contains(p, []byte("<key>SuccessfulExit</key>")) {
				t.Fatal("plist missing marker or KeepAlive")
			}
			x := windowsTaskXML(c.exe, c.state)
			autostartXMLCheck(t, x)
			if !bytes.Contains(x, []byte(autostartMarker)) {
				t.Fatal("task missing marker")
			}
			enc := encodeUTF16BOM(x)
			if enc[0] != 0xFF || enc[1] != 0xFE || decodeUTF16Maybe(enc) != string(x) {
				t.Fatal("utf16 round trip failed")
			}
			u := string(systemdUserUnit(c.exe, c.state))
			for _, l := range strings.Split(u, "\n") {
				if v, ok := strings.CutPrefix(l, "ExecStart="); ok {
					got := autostartUnquote(t, v)
					want := append([]string{c.exe}, autostartArgs(c.state)...)
					if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
						t.Fatalf("ExecStart round trip: %q vs %q", got, want)
					}
					return
				}
			}
			t.Fatal("no ExecStart")
		})
	}
}

func TestAutostartWindowsQuote(t *testing.T) {
	for in, want := range map[string]string{`a b`: `"a b"`, `C:\x y\`: `"C:\x y\\"`, `a"b`: `"a\"b"`} {
		if got := windowsQuoteArg(in); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
}

type autostartFake struct {
	calls [][]string
	task  []byte
}

func (f *autostartFake) run(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if name == "schtasks.exe" {
		switch args[0] {
		case "/Query":
			if f.task == nil {
				return nil, errors.New("not found")
			}
			return f.task, nil
		case "/Create":
			b, err := os.ReadFile(args[2])
			f.task = b
			return nil, err
		case "/Delete":
			f.task = nil
		}
	}
	return nil, nil
}

func autostartIsolate(t *testing.T) (*autostartFake, string) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	f := &autostartFake{}
	old := runAutostartCommand
	runAutostartCommand = f.run
	t.Cleanup(func() { runAutostartCommand = old })
	return f, filepath.Join(home, "state")
}

func TestAutostartStatusNothingInstalled(t *testing.T) {
	_, state := autostartIsolate(t)
	info := autostartStatus(state)
	if !info.Supported || info.Enabled || info.Mechanism == "" {
		t.Fatalf("%+v", info)
	}
}

func TestAutostartUnsupportedWhenCommandFails(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	_, state := autostartIsolate(t)
	runAutostartCommand = func(string, ...string) ([]byte, error) { return nil, errors.New("no bus") }
	if info := autostartStatus(state); info.Supported || info.Note == "" {
		t.Fatalf("%+v", info)
	}
}

func TestAutostartEnableDisableIdempotent(t *testing.T) {
	f, state := autostartIsolate(t)
	for i := 0; i < 2; i++ {
		if err := setAutostart(state, true); err != nil {
			t.Fatal(err)
		}
		if info := autostartStatus(state); !info.Enabled {
			t.Fatalf("enable #%d: %+v", i, info)
		}
	}
	for i := 0; i < 2; i++ {
		if err := setAutostart(state, false); err != nil {
			t.Fatal(err)
		}
		if info := autostartStatus(state); info.Enabled {
			t.Fatalf("disable #%d: %+v", i, info)
		}
	}
	if len(f.calls) == 0 {
		t.Fatal("fake runner never used")
	}
}

func TestAutostartDisableLeavesForeignEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file based")
	}
	_, state := autostartIsolate(t)
	if err := setAutostart(state, true); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, "Library", "LaunchAgents", autostartLabel+".plist")
	if runtime.GOOS == "linux" {
		path = filepath.Join(home, ".config", "systemd", "user", autostartUnitName)
	}
	if err := os.WriteFile(path, []byte("foreign"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setAutostart(state, false); err == nil {
		t.Fatal("expected refusal")
	}
	if b, _ := os.ReadFile(path); string(b) != "foreign" {
		t.Fatal("foreign entry modified")
	}
}
