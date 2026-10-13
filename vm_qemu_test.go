//go:build linux || windows

package main

import (
	"crypto/tls"
	"io"
	"path/filepath"
	"reflect"
	"testing"
)

func TestQEMUDiskArgs(t *testing.T) {
	for _, c := range []struct {
		name          string
		bundle, state string
	}{
		{"appliance", "bundle", "state"},
		{"escaped paths", "bundle, with spaces", "state, with spaces"},
	} {
		t.Run(c.name, func(t *testing.T) {
			bundle, state := c.bundle, c.state
			if c.name == "escaped paths" {
				bundle, state = "bundle,, with spaces", "state,, with spaces"
			}
			want := []string{
				"-drive", "file=" + filepath.Join(bundle, "system.img") + ",format=raw,if=none,id=ca-base,readonly=on",
				"-device", "virtio-blk-pci,drive=ca-base,serial=ca-base",
				"-drive", "file=" + filepath.Join(state, "data.img") + ",format=raw,if=none,id=ca-data,discard=unmap",
				"-device", "virtio-blk-pci,drive=ca-data,serial=ca-data",
			}
			if got := qemuDiskArgs(c.bundle, c.state); !reflect.DeepEqual(got, want) {
				t.Fatalf("disk arguments:\n got %q\nwant %q", got, want)
			}
		})
	}
}

func TestGuestTLSRejectsOtherClinics(t *testing.T) {
	credentials, err := newGuestCredentials()
	if err != nil {
		t.Fatal(err)
	}
	server, err := guestTLS(credentials, true)
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", server)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for i := 0; i < 3; i++ {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("ok"))
			c.Close()
		}
	}()
	other, err := newGuestCredentials()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		pem       []byte
		accept    bool
		anonymous bool
	}{{credentials, true, false}, {other, false, false}, {credentials, false, true}} {
		cfg, err := guestTLS(c.pem, false)
		if err != nil {
			t.Fatal(err)
		}
		if c.anonymous {
			cfg.Certificates = nil
		}
		conn, err := tls.Dial("tcp", l.Addr().String(), cfg)
		var b []byte
		if err == nil {
			b, err = io.ReadAll(conn)
			conn.Close()
		}
		if c.accept && (err != nil || string(b) != "ok") {
			t.Fatalf("valid clinic rejected: %v", err)
		}
		if !c.accept && err == nil && string(b) == "ok" {
			t.Fatal("unauthenticated clinic accepted")
		}
	}
}
