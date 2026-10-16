# Containers Anywhere

**A small desktop launcher. A downloadable Kubernetes appliance. Your workloads,
on your computer.**

Containers Anywhere boots a single-node K3s VM and opens **Kite v0.16.0** for
installing Helm charts, applying YAML, inspecting resources, logs and terminals.
The host needs no Docker, Kubernetes, Nix, Python or Node installation.

An independent, application-agnostic project for running cloud-native workloads
on consumer hardware. The current release is an **alpha**; mission-critical
durability and disaster recovery are the next engineering milestones.

[Download the alpha](https://github.com/tellmeY18/containers_anywhere/releases/tag/containers-v0.1.0-mvp.49)
· [Recovery architecture](docs/DISASTER_RECOVERY.md)
· [Roadmap](ROADMAP.md)
· [Appliance optimization](docs/APPLIANCE.md)
· [Workload gateway setup](docs/NETWORKING.md)

CARE Anywhere's original code and documentation are preserved on
[`archive/care-anywhere`](https://github.com/tellmeY18/containers_anywhere/tree/archive/care-anywhere).
Its releases and history are retained. See [project history](docs/HISTORY.md).

## User experience

1. Install the **Containers Anywhere** DMG, EXE or AppImage.
2. Open it. A small local control panel appears immediately.
3. The launcher downloads its exact VM archive and, on Linux/Windows, its QEMU
   runtime. Download hashes are pinned **inside the launcher**. Interrupted
   transfers resume; archives are verified before extraction and activation.
4. Once K3s and Kite are ready, choose **Open Kite**, create your administrator,
   and use the preconfigured `local` cluster.
5. Install workloads through Kite's Helm management or YAML editor. HTTP GET
   access through Kite's Kube Proxy is available for simple service previews.

Closing the browser keeps the VM running. Use **Stop cluster** or **Quit** in
the control panel. Restart preserves your cluster, Kite account and local volumes.

### Platforms

| Host | VM runtime | Requirements |
| --- | --- | --- |
| macOS ARM64 | Apple Virtualization.framework | Apple Silicon, macOS 13+ |
| Linux AMD64 / ARM64 | Downloaded QEMU with KVM | Hardware virtualization, `/dev/kvm` access, browser and `xdg-open` |
| Windows AMD64 | Downloaded QEMU with WHPX | Windows 10/11, Windows Hypervisor Platform enabled, firmware virtualization |

The MVP defaults to **2 vCPUs, 4 GiB guest RAM and a sparse 16 GiB data disk**.
Plan for at least 8 GiB host RAM and 20 GiB free storage, with more for workloads
and backups. These are defaults and capacity guidance, not measured minimums.
Intel Macs, Windows ARM, mobile devices, GPU passthrough and non-Linux containers
are not supported. Images must support the guest's native CPU architecture.

AppImages need executable permission and FUSE 2; `--appimage-extract-and-run`
is available without FUSE. macOS builds are ad-hoc signed unless distribution
credentials are configured; Windows installers are unsigned. Never disable OS
security globally or run the launcher as root to work around virtualization access.

On Windows, missing virtualization support is detected before appliance downloads.
Choose **Set up this computer**, approve the Windows administrator prompt, and
restart if requested. Setup enables Windows Hypervisor Platform using Windows'
built-in tools; ordinary cluster execution remains unelevated. Firmware settings
or administrator policies may still require assistance. Windows uses a WHPX
interrupt-controller compatibility setting while retaining hardware CPU execution.

## What is shipped

```text
DMG / EXE / AppImage                 Separate downloadable assets
  native Go launcher                  appliance-{arm64,amd64}.tar.gz
  embedded bootstrap panel              kernel + initrd + system.img + data.img
  pinned URLs and SHA-256 hashes       runtime-{linux,windows}-{arch}.tar.gz
  licenses                              QEMU + native libraries + licenses
```

The immutable NixOS system image contains K3s/containerd, its airgap images,
Kite's preloaded container image, and a small guest agent. There is no runtime
`pip install` or `npm install`. K3s starts with SQLite, CoreDNS, Flannel and
local-path storage; packaged Traefik, ServiceLB and metrics-server are disabled. Additional
workloads and optional Kite plugins pull from registries as needed.

Kite runs as a pod with cluster-admin permissions because this MVP is a
single-owner cluster management tool. Anonymous access and analytics are disabled.
Installation-specific JWT/encryption keys, Kite SQLite/plugins, K3s state,
container images and local-path volumes live on the writable `/var/lib` disk.
The node password also persists across boots. No shared secrets ship in images.

Host control and Kite browser listeners bind only to `127.0.0.1`. macOS uses vsock;
QEMU uses per-boot mutual TLS through one loopback forward. Browser access to Kite
requires a private per-boot launcher capability as well as Kite authentication.
Management uses a separate origin. WebSocket upgrades for Kite terminals/logs
pass through the proxy. The Kubernetes API is not forwarded to the host.

## Run and configure

Release launchers download automatically. Developer builds require a trusted
local bundle unless built with the release download pins:

```sh
containers-anywhere desktop --bundle /path/to/bundle
containers-anywhere serve --bundle /path/to/bundle --state /path/to/new-cluster
containers-anywhere status --state /path/to/new-cluster
containers-anywhere logs --state /path/to/new-cluster
containers-anywhere stop --state /path/to/new-cluster
```

For Linux/Windows local-bundle testing, put the verified `qemu/` runtime beside
`bundle/`. `serve` uses port 8484 by default; `--port 0` selects a free port.
The desktop selects a free port automatically. A private Kite access link is
saved as `<state>/open.html`; do not share it.

Environment settings (inherited by the desktop's launcher child):

- `CONTAINERS_CPUS=4` and `CONTAINERS_MEMORY_MIB=8192`: guest resources.
- `CONTAINERS_NO_BROWSER=1`: suppress browser opening.
- `CONTAINERS_NO_NETWORK=1`: diagnostic offline mode; new image pulls fail.
- `CONTAINERS_QEMU_ACCEL=tcg`: explicitly slow CI/debug emulation, never automatic.
- `CONTAINERS_GUEST_DEBUG=1`: QEMU guest journal on the console.

State defaults to `~/Library/Application Support/containers-anywhere` on macOS,
`${XDG_CONFIG_HOME:-~/.config}/containers-anywhere` on Linux, and
`%APPDATA%\containers-anywhere` on Windows. Uninstalling the launcher preserves it.
Download caches are under `appliances/` inside state. Avoid cloud-synced folders.

## Backup and restore

**Available today: manual, encrypted, stopped-VM recovery points. PITR,
scheduled/off-device backups and automated recovery drills are not implemented.**
See the [recovery contract and planned architecture](docs/DISASTER_RECOVERY.md)
for protection layers, failure scenarios and acceptance criteria.

```sh
containers-anywhere stop --state /path/to/cluster
containers-anywhere backup --state /path/to/cluster --file /external/cluster.age
containers-anywhere restore --state /new/empty/cluster \
  --file /external/cluster.age --key /separate/cluster.age.key
containers-anywhere serve --bundle /original/bundle --state /new/empty/cluster
```

Cold backups encrypt the whole data disk and its appliance identity using age.
The OS lock prevents backup while the VM is running. Existing backups and keys
are never overwritten; restore requires an empty destination and authenticates
the archive before publishing data. Store the generated `.key` separately.
Restore needs the same architecture and **exact original appliance**. Retain the
original launcher and bundle; this MVP refuses in-place appliance upgrades.

## Build and acceptance

Builds, Go tests and packaging run in **GitHub Actions**. The `Build Containers
Anywhere MVP` workflow builds both appliances and all four desktop packages.
Linux AMD64 acceptance boots QEMU/KVM, creates a Kite administrator, deploys a
PVC-backed web Deployment via Kite, verifies HTTP access, reboots, and repeats after an
encrypted backup/restore. Windows runs equivalent boot/restart checks under TCG.
Physical Windows WHPX, Linux ARM64 KVM and local macOS boot still need hardware
acceptance; successful cross-compilation does not establish those results.

```sh
gh workflow run alpha.yml --ref containers-anywhere
# After a successful build, prepare a complete draft (does not publish):
gh workflow run release.yml --ref containers-anywhere -f build_run_id=<run-id>
```

Artifacts use `0.1.0-mvp.<run-number>`. Launchers point to immutable
`containers-v<version>` release URLs in the repository that built them. **The
complete draft must be published before first-run downloads work for users.**
Actions artifacts alone are useful for local-bundle testing; their future public
download URLs are not live until publication. Alpha 49 is already published.
New builds use `tellmeY18/containers_anywhere`; older launchers rely on GitHub's
redirect from the former repository name. Do not reuse that old repository name.

Build tools: Go, Nix on a matching Linux builder, Python 3.11+, platform packaging
tools; no Node frontend toolchain. The flake pins nixpkgs/microvm.nix. Kite Linux
binaries are pinned to v0.16.0 and its published SHA-256 hashes, then assembled
into a container archive at Nix build time without a Docker daemon.

```sh
nix build .#packages.aarch64-linux.bundle
# Export only manifest.json, kernel, initrd, system.img and data.img.
# Never copy the whole build closure to the consumer machine.
python3 scripts/appliance.py /nix/store/<bundle> dist/appliance-arm64.tar.gz
python3 scripts/package.py macos --arch arm64 --version 0.1.0-mvp.1 \
  --appliance dist/appliance-arm64.tar.gz \
  --base-url https://github.com/<owner>/<repo>/releases/download/containers-v0.1.0-mvp.1
```

## MVP boundaries

The source tree includes a native Caddy/Traefik gateway with a preloaded branded
demo.local workload and host-side IPv4 mDNS, pending CI and real-platform
acceptance. See [networking setup](docs/NETWORKING.md) for address, port, firewall
and certificate requirements. This is not part of already-published alpha 49.

This is a local single-node environment, not a highly available cluster or a
production-certified consumer runtime. Internet is needed for initial downloads
and uncached workloads. Single-node local-path volumes cannot survive loss of
the host disk without a backup. Cloud load balancers, storage integrations and
architecture-specific workloads may need adaptation.

No general arbitrary TCP/UDP forwarding, host kubeconfig export, automatic updates,
scheduled backups, suspend/resume acceptance, full Prometheus stack or application
catalog is included. Kite's GET-only Kube Proxy is a preview facility, not a
general ingress solution. Keep unrelated/untrusted web applications off the
management origin. Download hashes authenticate against the launcher build;
publisher-signed update channels and notarized/signed installers remain release
hardening work. Alpha 49 downloads are 789.2 MB (ARM64 appliance), 1,002.3 MB
(AMD64 appliance), 19.0–19.2 MB (Linux QEMU) and 93.2 MB (Windows QEMU).
Installers are 2.5 MB (DMG), 5.0 MB (EXE), and 3.7–4.1 MB (AppImage), in decimal
units. Idle resource use remains unmeasured; see the optimization plan.

Use Deployments or StatefulSets for long-running workloads: Kubernetes graceful
node shutdown terminates bare Pods, and only controllers recreate them after boot.
