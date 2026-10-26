# Containers Anywhere

**A small desktop launcher. A downloadable Kubernetes appliance. Your workloads,
on your computer.**

Containers Anywhere boots a single-node K3s VM and opens **Kite v0.16.0** for
installing Helm charts, applying YAML, inspecting resources, logs and terminals.
The host needs no Docker, Kubernetes, Nix, Python or Node installation.

An independent, application-agnostic project for running cloud-native workloads
on consumer hardware. The first independently versioned 0.1.0 app and appliance
are **beta** releases, not production releases. Production go-live is planned for
v1.0.0; see [release scope and trust](docs/RELEASES.md) for limitations.

Until explicitly declared stable, development targets fresh 0.1.0 installs and
current independent component contracts, not backward compatibility or alpha
migration. Unsupported data requires a new empty state directory; it is never
automatically deleted.
Robust current-design updates remain a 0.1.0 priority; app/appliance independence
and normal-update data preservation are not excluded by that policy.

[Download the beta](https://github.com/tellmeY18/containers_anywhere/releases/tag/app-v0.1.0)
· [Recovery architecture](docs/DISASTER_RECOVERY.md)
· [Roadmap](ROADMAP.md)
· [Appliance optimization](docs/APPLIANCE.md)
· [Workload gateway setup](docs/NETWORKING.md)

CARE Anywhere's original code and documentation are preserved on
[`archive/care-anywhere`](https://github.com/tellmeY18/containers_anywhere/tree/archive/care-anywhere).
Its releases and history are retained. See [project history](docs/HISTORY.md).

`develop` is the universal platform line; `care-mvp` remains the separately
CARE-bundled line. The universal defaults below are source changes, not a change
to already-published alpha 49. No optional application profile/catalog is shipped.

## User experience

1. Install the **Containers Anywhere** DMG, EXE or AppImage.
2. Open it. A small local control panel appears immediately.
3. For a new cluster, the launcher downloads its recommended exact VM archive and, on Linux/Windows, its QEMU
   runtime. Download hashes are pinned **inside the launcher**. Interrupted
   transfers resume; archives are verified before extraction and activation.
   Replacing the app retains an existing cluster's exact selected appliance;
   app and appliance releases are independent.
4. Once the cluster is ready, choose **Open Kite**, create an administrator and
   use the preconfigured `local` cluster. Fresh `develop` appliances do not install
   CNCM or CARE. Workload listeners default to `0.0.0.0` on 80/443, with a
   permission-driven 8080/8443 fallback. The bundled demo advertises on the LAN.
   Authorized workload service accounts manage routing, local TLS and mDNS via
   the [WorkloadPublication CRD](docs/NETWORKING.md), without host-panel grants.
5. Install workloads through Kite's Helm management or YAML editor. HTTP GET
   access through Kite's Kube Proxy is available for simple service previews.

Closing the browser keeps the VM running. Use **Stop cluster** or **Quit** in
the control panel. Restart preserves your cluster, Kite account and local volumes.
Existing data still requires its exact original appliance; this is not an
in-place appliance migration. Incompatible alpha publication configuration fails
explicitly rather than being migrated or silently deleted.

### Platforms

| Host | VM runtime | Requirements |
| --- | --- | --- |
| macOS ARM64 | Apple Virtualization.framework | Apple Silicon, macOS 13+ |
| Linux AMD64 / ARM64 | Downloaded QEMU with KVM | Hardware virtualization, `/dev/kvm` access, browser and `xdg-open` |
| Windows AMD64 | Downloaded QEMU with WHPX | Windows 10/11, Windows Hypervisor Platform enabled, firmware virtualization |

The MVP defaults to **2 vCPUs, 4 GiB guest RAM and a sparse 16 GiB data disk**.
Plan for at least 8 GiB host RAM and 20 GiB free storage, with more for workloads
and backups. These are defaults and capacity guidance, not measured minimums.
The launcher no longer increases RAM automatically for CARE builds. Installations
using implicit sizing should set `CONTAINERS_MEMORY_MIB=8192` before launch if
their workloads require 8 GiB.
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
Appliance caches are under `appliances/` inside state; independently pinned QEMU
runtimes are under `runtimes/`. Avoid cloud-synced folders.

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

Builds, Go tests and packaging run in **GitHub Actions**. Separate
`Build and accept appliance` and `Build and accept app` workflows build the VM
or reuse an already-released VM, respectively. Both package all four host targets.
Linux AMD64 acceptance boots QEMU/KVM, creates a Kite administrator, deploys a
PVC-backed web Deployment via Kite, verifies HTTP access, reboots, and repeats after an
encrypted backup/restore, including app replacement that retains the original
appliance. Windows TCG boot acceptance is currently disabled due to hosted-runner
timeouts.
Physical Windows WHPX, Linux ARM64 KVM and local macOS boot still need hardware
acceptance; successful cross-compilation does not establish those results.

```sh
gh workflow run appliance.yml --ref develop
# After an independent appliance release is available, build only the app:
gh workflow run app.yml --ref develop -f appliance_tag=appliance-v0.1.0
# After a successful build, prepare a complete draft (does not publish):
gh workflow run release.yml --ref develop -f build_run_id=<run-id>
```

App versions come from `VERSION` and use `app-v<semver>`; appliance versions come
from `APPLIANCE_VERSION` and use `appliance-v<semver>`. New app builds pin published
component URLs and fixed hashes; app-only fixes never rebuild or replace the VM.
See [release usage, trust and compatibility](docs/RELEASES.md). The 0.1.0 beta
is published only after its required CI, published-dependency acceptance and
release-asset checks pass. Physical platform acceptance remains incomplete; see
the documented beta limitations.
Initial appliance candidates use local-bundle acceptance until an independently
approved component release exists. Alpha 49 remains published under its original
`containers-v0.1.0-mvp.49` identity. Older launchers rely on GitHub's redirect from
the former repository name; do not reuse that name.

Build tools: Go, Nix on a matching Linux builder, Python 3.11+, platform packaging
tools; no Node frontend toolchain. The flake pins nixpkgs/microvm.nix. Kite Linux
binaries are pinned to v0.16.0 and its published SHA-256 hashes, then assembled
into a container archive at Nix build time without a Docker daemon.

```sh
nix build .#packages.aarch64-linux.bundle
# Export only manifest.json, kernel, initrd, system.img and data.img.
# Never copy the whole build closure to the consumer machine.
python3 scripts/appliance.py /nix/store/<bundle> dist/appliance-arm64.tar.gz
python3 scripts/package.py macos --arch arm64 --version 0.1.0 \
  --appliance dist/appliance-arm64.tar.gz \
  --base-url https://github.com/tellmeY18/containers_anywhere/releases/download/appliance-v0.1.0
```

## MVP boundaries

The source tree includes a native Caddy/Traefik gateway with a preloaded branded
demo workload and CRD-controlled host-side IPv4 mDNS for published, responding `.local`
workloads, pending CI and real-platform acceptance. The panel distinguishes
approved routes from responding endpoints and mDNS announcements; none proves
remote firewall passage or client certificate trust.
See [networking setup](docs/NETWORKING.md) for address, port, firewall
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
