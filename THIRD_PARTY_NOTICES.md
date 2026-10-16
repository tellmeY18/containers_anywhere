# Containers Anywhere third-party notices

## CARE Clinic reuse

`internal/atomicfile` adapts CARE Clinic's atomic replacement and Windows private
file ACL implementation from `app/internal/sys/atomicfile`.
`internal/diskspace` adapts its nearest-existing-directory and filesystem capacity
checks.
Source: https://github.com/ohcnetwork/care_clinic (main, retrieved 2026-10-04).
The original API and durability ordering are retained, with platform files combined.

The operation lock, staged restore-before-activation model, separate recovery
material, and prohibition on replacing live data follow CARE Clinic's architecture.
Docker-specific backup helpers cannot be reused directly for a VM disk format.
Existing Clinic backups are not interchangeable with this preview's appliance
backup format.

`gateway_mdns.go` adapts CARE Clinic's host-interface filtering, per-interface
multicast sockets and conflict-before-advertise design from
`app/internal/sys/mdns` at f2836fd87c0d0eab1440126dbe5be34e751bd3c2.
It uses the existing golang.org/x/net DNS codec instead of Clinic's DNS libraries.

## CARE Clinic MIT License

Copyright (c) 2026 Open Healthcare Network Foundation

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

Other components retain their own licenses. Distributing appliance images or
QEMU helpers requires their corresponding notices and source-offer
obligations, independent of the launcher license.

# K3s and Kite

The guest redistributes K3s (Apache-2.0), containerd and Kubernetes components
under their respective licenses. Source versions are pinned by flake.lock/nixpkgs.
Kite v0.16.0 (Apache-2.0) is redistributed as its unmodified, checksum-pinned
Linux binary with embedded UI inside a locally assembled container image.
Source and license: https://github.com/kite-org/kite/tree/v0.16.0.
`nix/kite.yaml` adapts Kite's deployment manifest for persistent local storage,
generated secrets and the preloaded image.

# Downloaded QEMU runtime

## Caddy and Traefik gateway

Host packages include the upstream Caddy 2.11.7 binary (Apache-2.0), downloaded
from https://github.com/caddyserver/caddy/releases/tag/v2.11.7 with archive pins
in scripts/caddy.py. The launcher's build embeds its final binary SHA-256.
Caddy source and license: https://github.com/caddyserver/caddy/tree/v2.11.7.

Traefik (MIT) is built from the nixpkgs revision pinned in flake.lock and
preloaded as containers-anywhere/traefik. Its version is taken from that pinned
package, not a mutable latest tag. Source: https://github.com/traefik/traefik.

The Windows gateway-directory ACL follows the same CARE Clinic-derived private
file ACL pattern already attributed above, with directory inheritance enabled.

## QEMU

Linux packages redistribute Ubuntu's QEMU and shared-library packages with their
runtime license files. Corresponding source packages are available from
https://archive.ubuntu.com/ubuntu/pool/ (QEMU: pool/main/q/qemu/).
Windows packages redistribute Stefan Weil's QEMU 11.1.0 build dated 2026-08-11,
including its notices, from https://qemu.weilnetz.de/w64/2026/.
Source and build scripts: https://github.com/stweil/qemu/tree/ar7.
QEMU is licensed under GPL-2.0 with individual components under other licenses;
see the bundled upstream notices. Packaging tools are build-time dependencies.
