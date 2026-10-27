package main

import (
	"context"
	"fmt"
	"github.com/Code-Hex/vz/v3"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type appleVM struct {
	vm    *vz.VirtualMachine
	done  chan struct{}
	log   *os.File
	input *os.File
	mu    sync.Mutex
}

func doctor() error {
	fmt.Println("macOS: native Virtualization.framework. Binary must be signed with com.apple.security.virtualization.")
	return nil
}
func bootVM(bundle, state string, m manifest) (machine, error) {
	boot, e := vz.NewLinuxBootLoader(filepath.Join(bundle, m.Kernel), vz.WithInitrd(filepath.Join(bundle, m.Initrd)), vz.WithCommandLine("console=hvc0 reboot=t panic=1 init="+m.System+"/init"))
	if e != nil {
		return nil, e
	}
	cpus, memory, e := vmResourcesFor(state)
	if e != nil {
		return nil, e
	}
	cfg, e := vz.NewVirtualMachineConfiguration(boot, uint(cpus), uint64(memory)<<20)
	if e != nil {
		return nil, e
	}
	var disks []vz.StorageDeviceConfiguration
	paths := []string{filepath.Join(bundle, "system.img"), filepath.Join(state, "data.img")}
	ids := []string{"ca-base", "ca-data"}
	for i, p := range paths {
		a, e := vz.NewDiskImageStorageDeviceAttachment(p, i != 1)
		if e != nil {
			return nil, e
		}
		d, e := vz.NewVirtioBlockDeviceConfiguration(a)
		if e != nil {
			return nil, e
		}
		if e = d.SetBlockDeviceIdentifier(ids[i]); e != nil {
			return nil, e
		}
		disks = append(disks, d)
	}
	cfg.SetStorageDevicesVirtualMachineConfiguration(disks)
	entropy, e := vz.NewVirtioEntropyDeviceConfiguration()
	if e != nil {
		return nil, e
	}
	cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})
	socket, e := vz.NewVirtioSocketDeviceConfiguration()
	if e != nil {
		return nil, e
	}
	cfg.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{socket})
	// Outbound NAT permits workload image pulls; control stays on vsock.
	if os.Getenv("CONTAINERS_NO_NETWORK") != "1" {
		nat, e := vz.NewNATNetworkDeviceAttachment()
		if e != nil {
			return nil, e
		}
		netCfg, e := vz.NewVirtioNetworkDeviceConfiguration(nat)
		if e != nil {
			return nil, e
		}
		mac, e := vz.NewRandomLocallyAdministeredMACAddress()
		if e != nil {
			return nil, e
		}
		netCfg.SetMACAddress(mac)
		cfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netCfg})
	}
	output, e := os.OpenFile(filepath.Join(state, "console.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if e != nil {
		return nil, e
	}
	input, e := os.Open(os.DevNull)
	if e != nil {
		output.Close()
		return nil, e
	}
	serial, e := vz.NewFileHandleSerialPortAttachment(input, output)
	if e != nil {
		input.Close()
		output.Close()
		return nil, e
	}
	console, e := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serial)
	if e != nil {
		return nil, e
	}
	cfg.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{console})
	if _, e = cfg.Validate(); e != nil {
		return nil, e
	}
	vm, e := vz.NewVirtualMachine(cfg)
	if e != nil {
		return nil, e
	}
	a := &appleVM{vm: vm, done: make(chan struct{}), log: output, input: input}
	if e = vm.Start(); e != nil {
		input.Close()
		output.Close()
		return nil, e
	}
	go func() {
		for s := range vm.StateChangedNotify() {
			if s == vz.VirtualMachineStateStopped {
				close(a.done)
				return
			}
		}
	}()
	return a, nil
}
func (a *appleVM) Done() <-chan struct{} { return a.done }
func (a *appleVM) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	devices := a.vm.SocketDevices()
	if len(devices) == 0 {
		return nil, fmt.Errorf("guest socket unavailable")
	}
	// Virtualization.framework's connect operation has no context parameter.
	type result struct {
		c net.Conn
		e error
	}
	ch := make(chan result)
	go func() {
		c, e := devices[0].Connect(8080)
		select {
		case ch <- result{c, e}:
		case <-ctx.Done():
			if c != nil {
				c.Close()
			}
		}
	}()
	select {
	case r := <-ch:
		return r.c, r.e
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (a *appleVM) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-a.done:
		return nil
	default:
	}
	if _, e := a.vm.RequestStop(); e != nil {
		return e
	}
	select {
	case <-a.done:
		a.input.Close()
		return a.log.Close()
	case <-time.After(150 * time.Second):
		return fmt.Errorf("guest did not shut down; refusing forced power-off")
	}
}
