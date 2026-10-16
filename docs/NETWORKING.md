# Caddy edge and Traefik workload gateway

Status: implemented in source, pending CI build/boot/restore and physical LAN
acceptance. Native Caddy, Traefik, the preloaded demo and IPv4 mDNS are wired
together. Automatic firewall setup, privileged port setup, OS trust enrollment,
Gateway API conformance and physical-platform acceptance remain outstanding.

Packaged launchers default to HTTP 80/HTTPS 443 on loopback and the explicit
private IPv4 LAN addresses discovered on first launch. The default grant
publishes only the built-in demo. Management stays private. Ports are not
silently changed when unavailable; Linux commonly requires OS socket/capability
setup for these ports. Never run the VM launcher as root as a workaround.

## Architecture

```
browser HTTP(S) on configured host addresses
  -> native Caddy (local CA, TLS termination, no admin API)
  -> private capability-authenticated loopback bridge
  -> authenticated HTTP CONNECT over existing VM transport
     (Virtualization.framework vsock or QEMU per-boot mTLS)
  -> guest relay -> Traefik ClusterIP -> workload Service
```

CONNECT is a fixed-destination internal operation, not an open proxy. Requests
arriving at the workload listener cannot invoke CONNECT or agent control routes.
The bridge strips its capability before forwarding; Caddy discards untrusted
forwarding metadata and supplies the browser's actual address and HTTPS scheme.
Traefik trusts only the fixed guest relay source and the default single-node
CNI gateway address. This source-address assumption must pass the real smoke
test; changing the PodCIDR requires revisiting that trust list.

Traefik is an independent Deployment, not the packaged K3s Traefik add-on.
ServiceLB stays disabled. Its binary is built from the flake.lock-pinned nixpkgs
package and preloaded into the appliance. Static manifests avoid runtime chart
downloads and a second Helm owner. Pod replacement uses Kubernetes Service
routing, not kubectl port-forward. The host bridge retains the exact fixed
Service reference; user input never selects an IP or arbitrary target port.

## Configure on a stopped cluster

Use a packaged launcher: its adjacent Caddy helper must match the binary digest
embedded in the launcher. There is no PATH lookup or checksum-bypass switch.

Create a JSON configuration outside the cluster state directory:

```json
{
  "port": 8443,
  "httpPort": 8080,
  "addresses": ["127.0.0.1", "192.168.1.20"],
  "mdns": true,
  "namespaces": ["example"],
  "domains": ["example.localhost"]
}
```

```sh
containers-anywhere gateway-configure --file gateway.json --state /path/to/cluster
containers-anywhere serve --state /path/to/cluster
containers-anywhere gateway-status --state /path/to/cluster
```

Replace the LAN address with this computer's address. Omit addresses to keep
the gateway loopback-only; omit mdns to disable discovery. The owner grants
namespace/domain publication before starting. Configuration requires the
exclusive lifecycle lock. Wildcard/public-IP binds and unknown JSON fields
are rejected. Port conflicts fail visibly instead of changing URLs.

After creating an `example` namespace and an HTTP Service named `web`, a workload
manager, Kite, or kubectl can publish it with:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: web
  namespace: example
spec:
  ingressClassName: containers-anywhere
  rules:
    - host: app.example.localhost
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: web
                port:
                  number: 8080
```

Open `https://app.example.localhost:8443`. Some resolvers do not implement
subdomains of localhost; an explicit hosts entry or curl --resolve is then
needed. Application `.local` names are accepted within the namespace convention;
automatic mDNS currently advertises only the built-in demo.local.

## Out-of-box demo and mDNS

The `containers-demo` namespace includes a preloaded unprivileged Caddy static
server, Service and Ingress for demo.local and demo.localhost. The namespace and
resource name reserve those exact hostnames. No external image download is
needed. The branded page is actually served by that pod, through Traefik and
native Caddy; it is not a launcher fallback page.

The host announces demo.local only after retrieving that page through Caddy's
HTTP listener. The responder probes for conflicting names, sends A/PTR/SRV/TXT
records on configured IPv4 interfaces, refreshes them, and sends goodbye records
on stop or failed health checks. It publishes no AAAA record. A second appliance
claiming demo.local is reported as a conflict, not silently renamed. Only one
such demo name can be used per multicast link. Network changes withdraw stale
advertisements; the owner must reconfigure changed addresses.

From a second computer with the launcher installed, run
`containers-anywhere demo-check`. It performs direct multicast A/SRV discovery
(bypassing hosts-file/DNS-cache shortcuts), rejects a self-test address, then
retrieves the expected pod page over the discovered host port. It does not
start a VM, install trust, or change firewall settings. This is a connectivity
check, not a cryptographic identity check; HTTP/mDNS can be spoofed on the LAN.

Open `http://demo.local` from a second device on the same network (include the
configured HTTP port if not 80). This non-sensitive static demo is the only
application allowed over HTTP. Other application requests redirect to HTTPS;
the exact public /root.crt bootstrap resource is also available over HTTP.
Phones can test the path before CA enrollment. HTTP bootstrap is not authenticated;
verify the public CA with the administrator before trusting it.

A successful local health probe does not prove a firewall permits LAN clients.
Windows must allow the configured TCP ports and UDP 5353 on the intended private
network; macOS must permit local-network discovery and the Caddy application;
Linux must permit these ports in its active firewall. This source does not
disable firewalls or silently change Windows network profiles. Wi-Fi client
isolation and multicast-blocking networks can prevent discovery/reachability.

Admission confines each namespace to `<name>.<namespace>.localhost` or `.local`,
requires explicit Service backends, and rejects catch-all routes, TLS blocks,
and Traefik annotations. Protected namespaces cannot publish. Host grants are a
second independent check. A namespace's authorized Ingress writers can publish
its permitted names; ordinary pods gain no permissions automatically. This is
a single-owner appliance, not isolation against cluster-admin/privileged pods
or users allowed to forge arbitrary Service endpoints.

Caddy discovers approved hosts every five seconds and issues certificates on
the first TLS handshake. No raw Caddy JSON, CA private keys, admin endpoint, or
host filesystem access is exposed inside Kubernetes. Transient discovery
failures retain the last approved host list and report degradation. Successful
discovery applies deletions. Existing WebSocket sessions are not forcibly
revoked by an Ingress deletion; stop the cluster for immediate session teardown.

## Trust and recovery

Install the **public** root certificate explicitly on the host/client:
`<state>/gateway/caddy/pki/authorities/local/root.crt`. Automatic trust-store
installation is disabled. Never distribute root.key or intermediate.key.

The host CA store is private and persists beneath `<state>/gateway/caddy`.
Caddy never uses an arbitrary default HOME storage location. A separate helper
supervisor holds a backup-exclusion lock and reaps Caddy on launcher pipe EOF.
Normal stop waits for that supervisor before releasing the lifecycle lock.
Hard termination can interrupt connections; reconnect is the client's job.

Cold backups now optionally include gateway.json and the Caddy store. Restore
still authenticates the complete archive before publication, rejects links,
duplicates/traversal and bounded metadata overflow, and requires an empty
destination. Old two-member archives remain readable; older launchers cannot
restore archives containing the new optional gateway members. Restore the CA
identity before starting, so previously trusted clients retain the same root.

Existing data still requires its exact original appliance. This guest-agent and
Traefik change does not implement an upgrade/migration path for earlier alphas.

## Diagnostics and limits

`gateway-status` distinguishes discovery errors from Caddy process exits and
lists approved URLs. Certificate trust is reported as manual, never inferred
from a running process. See gateway.log in desktop Diagnostics. A helper crash
is reported; restart the cluster to restart it. No automatic retry of clinical
POSTs is introduced. The relay limits concurrent guest connections to 128.

This slice supports HTTP/1.1 and WebSocket upgrades through the bridge. Caddy
accepts browser HTTP/2, but upstream gRPC/h2c and UDP/HTTP3 are not implemented.
There is no patient-data access logging by default. Demo readiness is exposed
as demoURL and discoveryError in gateway-status and the desktop panel.

CI additions validate the generated config with the packaged Caddy and exercise
trusted HTTPS through Traefik, management-route isolation, reboot, and (Linux
KVM) encrypted restore with identical CA and PVC markers. These tests must run
successfully before release. Physical LAN, certificate enrollment, long-lived
streams, upload stress, suspend/wake and network changes still require dedicated
acceptance; source changes and compile checks do not establish those results.

## Remaining product work

- Implement per-platform firewall/80/443 setup and network-profile enrollment.
- Add client trust workflows and general workload-name mDNS with attribution.
- Add Gateway API support and richer Kubernetes-visible publication status if
  needed; do not claim the Ingress implementation is Gateway API conformance.
- Add supervised automatic recovery of the edge and host startup integration.
- Prove streaming, upload/download, source-IP behavior and permission-denial
  handling on actual macOS, Windows WHPX and both Linux architectures.
