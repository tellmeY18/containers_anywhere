# Windows WHPX diagnosis

## Reproduction on Care, 2026-10-17 (host local date)

- HP Pavilion 14-dv2xxx, Intel Core i7-1255U, 16 GB RAM.
- Windows 11 Home Single Language 25H2, build 26200.9457.
- Bundled QEMU 11.1.0 (`v11.1.0-12130-ge470268ff4`).
- Windows reported VirtualMachinePlatform enabled and HypervisorPlatform disabled,
  but `WHvGetCapability(WHvCapabilityCodeHypervisorPresent)` returned success and
  true. No Windows feature configuration or reboot was needed for these probes.

With `-machine pc -cpu host -accel whpx`, a diskless firmware probe reproduced
`Ignoring request for interrupt vector 0` followed by `Unexpected VP exit code 4`.
Microsoft defines exit reason 4 as an unrecoverable virtual-processor exception;
the number alone does not identify the underlying cause. QEMU remained running
after this error, so process liveness alone did not detect a usable guest.

Changing only the accelerator argument to `whpx,kernel-irqchip=off` avoided that
failure. A diskless kernel/initrd probe reached the expected missing-disk waits.
A subsequent appliance probe used the immutable cached system image and a fresh
copy of the bundled seed data disk, expanded sparsely to 16 GiB. The user's
existing data disk was never attached. No host network forwards were configured.

In that disposable appliance, Kite was observed running at about 65 seconds,
its guest-local port-forward opened at 67 seconds, and CoreDNS/local-path pods
were observed running by 70 seconds. ACPI shutdown completed with QEMU exit code
0. The workaround emulates the interrupt controller while retaining WHPX CPU
acceleration; it is not a TCG fallback.

## Scope of evidence

This establishes a reproduction and boot workaround on this Windows machine.
It does not establish full launcher mTLS/browser acceptance, workload PVC
persistence, restart, or encrypted backup/restore under WHPX. Those still require
the normal smoke test with a CI-built patched launcher and isolated state.

QEMU emitted two `failed to get xsave state: No error` warnings with the workaround.
They did not stop this boot or shutdown, but remain an unresolved diagnostic.
No published binaries, runtime hashes, existing appliance identities, or user
cluster state were modified during diagnosis.

## Acceleration and performance assessment

The observed workaround is a compatibility baseline, not a measured performance
optimum. Microsoft recommends WHP's built-in APIC emulation for best performance;
`kernel-irqchip=off` moves interrupt-controller handling into QEMU while retaining
WHP virtual-processor execution. Measure workload throughput, latency and host CPU
before quantifying its cost. The diskless comparison narrows the failure to the
default interrupt-controller path in this configuration; it does not establish
the exact defect in QEMU, Windows or firmware.

Current QEMU guests use virtio block/network devices, but networking is QEMU
user-mode NAT and device I/O is handled by QEMU. Virtio is paravirtualized I/O, not
physical-device passthrough. GPU acceleration is not provided. Resource defaults
are fixed at two vCPUs and 4 GiB RAM; automatic sizing and I/O tuning have not been
benchmarked. `-cpu host` does not establish that every host instruction-set feature
is exposed correctly, particularly while the XSAVE warnings remain unresolved.

Before restoring the default irqchip, compare a pinned upstream stable QEMU build
and supported Windows configuration on disposable state, then repeat full boot,
browser, workload, restart and restore acceptance. Do not retry alternate VM
configurations automatically against a user's disk after a failed boot.

## Consumer prerequisite setup

The per-user installer requests no elevation. The desktop now checks WHP before
downloads and offers a setup button when it is unavailable. Setup elevates only
the fixed DISM operation, reports cancellation/failure, and records a restart
marker plus a per-user RunOnce command when Windows requests reboot. The desktop
offers an explicit restart button and rechecks capability after sign-in. Firmware
configuration and administrator policy remain support cases.

The broader first-run acceptance target is:

1. Check supported Windows/CPU, available memory and disk space, WHP capabilities,
   feature state and restart requirements before downloading the appliance.
2. If Windows components are missing, present **Set up this computer**. Relaunch
   the same signed executable with UAC elevation for a narrow, fixed prerequisite
   command. Use Windows DISM to enable `HypervisorPlatform` and required parents;
   do not require Docker, WSL distributions or the full Hyper-V management role.
3. Report progress, cancellation, Windows Update/source failures and corporate
   policy restrictions in plain language. Treat hypervisor boot configuration
   separately if activation is disabled; never change it without explaining it.
4. Offer **Restart now** or **Later** when necessary. Persist setup progress and
   resume once after sign-in as the original unelevated user. Recheck actual WHP
   capability after restarting, then perform an isolated compatibility boot probe.
5. Use a tested accelerated configuration and expose a simple resource setting.
   Enable **Open Kite** only after guest health succeeds. Run normal downloads,
   VM execution and local management unelevated.

Firmware virtualization cannot be enabled portably by a normal Windows app. If
it is genuinely disabled, provide device-specific guidance or a support handoff.
Do not infer that from `VirtualizationFirmwareEnabled=False` alone when the
hypervisor is already running. Never disable Memory Integrity/VBS or silently use
TCG to make startup appear successful. A non-admin user may need administrator
credentials; the launcher cannot remove that Windows requirement.

Acceptance must include clean Windows installation, UAC denial, restart/resume,
feature-install failure and repeat launches, in addition to the VM lifecycle tests.
