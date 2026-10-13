//go:build linux || windows

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type qemuVM struct {
	done    chan struct{}
	address string
	tls     *tls.Config
}

func bootVM(bundle, state string, m manifest) (machine, error) {
	accel := "kvm"
	if runtime.GOOS == "windows" {
		accel = "whpx"
	}
	// Explicit slow software emulation is useful on CI without nested virtualization.
	if os.Getenv("CONTAINERS_QEMU_ACCEL") == "tcg" {
		accel = "tcg"
	} else if err := doctor(); err != nil {
		return nil, err
	}
	runtimeDir := filepath.Join(filepath.Dir(bundle), "qemu")
	if m.hostRuntime != "" {
		runtimeDir = m.hostRuntime
	}
	if err := verifyRuntime(runtimeDir); err != nil {
		return nil, err
	}
	name := "qemu-system-x86_64"
	if m.Arch == "arm64" {
		name = "qemu-system-aarch64"
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	credentials, err := newGuestCredentials()
	if err != nil {
		return nil, err
	}
	secret := filepath.Join(state, "guest-tls.pem")
	if err = atomicWrite(secret, credentials, 0600); err != nil {
		return nil, err
	}
	config, err := guestTLS(credentials, false)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	address := l.Addr().String()
	l.Close() // QEMU owns the listener; TLS pinning makes a port race fail closed.
	_, port, _ := net.SplitHostPort(address)
	console, machine, cpu := "ttyS0", "q35", "max"
	if accel == "kvm" {
		cpu = "host"
	}
	if m.Arch == "arm64" {
		console, machine = "ttyAMA0", "virt"
	}
	cpus, memory, err := vmResources()
	if err != nil {
		return nil, err
	}
	args := []string{"-machine", machine, "-accel", accel, "-cpu", cpu, "-m", fmt.Sprint(memory), "-smp", fmt.Sprint(cpus), "-display", "none", "-monitor", "none", "-serial", "stdio", "-no-reboot",
		"-L", filepath.Join(runtimeDir, "share", "qemu"),
		"-kernel", filepath.Join(bundle, m.Kernel), "-initrd", filepath.Join(bundle, m.Initrd),
		"-append", "console=" + console + " panic=1 init=" + m.System + "/init",
		"-fw_cfg", "name=opt/containers-anywhere/tls,file=" + qemuEscape(secret),
		"-device", "virtio-net-pci,netdev=net0"}
	args = append(args, qemuDiskArgs(bundle, state)...)
	network := "user,id=net0,hostfwd=tcp:127.0.0.1:" + port + "-:8080"
	if os.Getenv("CONTAINERS_NO_NETWORK") == "1" {
		network += ",restrict=on"
	}
	args = append(args, "-netdev", network)
	if os.Getenv("CONTAINERS_GUEST_DEBUG") == "1" {
		for i := range args {
			if args[i] == "-append" {
				args[i+1] += " systemd.journald.forward_to_console=1"
				break
			}
		}
	}
	cmd := exec.Command(filepath.Join(runtimeDir, name), args...)
	if err = prepareQEMU(cmd); err != nil {
		return nil, err
	}
	output, err := os.OpenFile(filepath.Join(state, "console.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = output, output
	if err = cmd.Start(); err != nil {
		output.Close()
		return nil, err
	}
	v := &qemuVM{done: make(chan struct{}), address: address, tls: config}
	go func() { cmd.Wait(); output.Close(); os.Remove(secret); close(v.done) }()
	return v, nil
}

func qemuDiskArgs(bundle, state string) []string {
	disks := []struct {
		id, path, options string
	}{
		{"base", filepath.Join(bundle, "system.img"), ",readonly=on"},
		{"data", filepath.Join(state, "data.img"), ",discard=unmap"},
	}
	var args []string
	for _, disk := range disks {
		args = append(args,
			"-drive", "file="+qemuEscape(disk.path)+",format=raw,if=none,id=ca-"+disk.id+disk.options,
			"-device", "virtio-blk-pci,drive=ca-"+disk.id+",serial=ca-"+disk.id)
	}
	return args
}

func qemuEscape(s string) string        { return strings.ReplaceAll(s, ",", ",,") }
func (v *qemuVM) Done() <-chan struct{} { return v.done }
func (v *qemuVM) Dial(ctx context.Context, _, _ string) (net.Conn, error) {
	return (&tls.Dialer{Config: v.tls}).DialContext(ctx, "tcp", v.address)
}
func (v *qemuVM) Close() error {
	select {
	case <-v.done:
		return nil
	default:
	}
	t := &http.Transport{DialContext: v.Dial}
	defer t.CloseIdleConnections()
	c := &http.Client{Transport: t, Timeout: 10 * time.Second}
	if r, err := c.Post("http://guest/_agent/stop", "application/json", nil); err == nil {
		r.Body.Close()
	}
	select {
	case <-v.done:
		return nil
	case <-time.After(150 * time.Second):
		return fmt.Errorf("QEMU is still running; retaining clinic lock until shutdown")
	}
}

func newGuestCredentials() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	t := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "care-guest"}, DNSNames: []string{"care-guest"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, t, t, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private})...), nil
}

func guestTLS(credentials []byte, server bool) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(credentials, credentials)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(credentials) {
		return nil, fmt.Errorf("invalid guest certificate")
	}
	c := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, RootCAs: roots, ServerName: "care-guest"}
	if server {
		c.ClientCAs = roots
		c.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return c, nil
}

func verifyRuntime(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if err != nil {
		return err
	}
	var files map[string]string
	if err = json.Unmarshal(b, &files); err != nil {
		return err
	}
	name := "qemu-system-x86_64"
	if runtime.GOARCH == "arm64" {
		name = "qemu-system-aarch64"
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if files[name] == "" {
		return fmt.Errorf("QEMU runtime missing checksum: %s", name)
	}
	for name, want := range files {
		if !filepath.IsLocal(name) || strings.Contains(name, "\\") {
			return fmt.Errorf("invalid QEMU runtime filename")
		}
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("QEMU runtime member must be regular")
		}
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != want {
			return fmt.Errorf("QEMU checksum mismatch: %s", name)
		}
	}
	return nil
}
