# Kubernetes-controlled workload publication

Status: implemented in source, pending CI build/boot/restore and physical LAN
acceptance. Native Caddy, Traefik, the preloaded demo and IPv4 mDNS are wired
together. Automatic firewall setup, privileged port setup, OS trust enrollment,
Gateway API conformance and physical-platform acceptance remain outstanding.

Desired publication lives exclusively in the namespaced
`networking.containersanywhere.dev/v1alpha1` **WorkloadPublication** CRD.
An explicitly authorized workload service account can manage its Service,
Ingress and publication, including host Caddy TLS and mDNS, without host-file
edits or panel grants. CNCM is an optional consumer of this same contract; it is
not preinstalled or special-cased. Container privilege is not authorization.

Fresh packaged launchers bind workload listeners to **0.0.0.0**, preferring
HTTP 80/HTTPS 443. Permission denial selects 8080/8443; occupied ports fail
visibly rather than changing URLs. Wildcard binding can expose published
workloads on unintended interfaces; use the host firewall appropriately.
Management, Kite, Kubernetes and Caddy administration remain private.
Never run the VM launcher as root to obtain low ports.

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

## Workload API and authorization

An administrator creates a namespace and a named service account, then binds
the shipped `containers-workload-publisher` ClusterRole using a **RoleBinding in
that namespace**. No default service account receives a binding. The role grants
CRUD of publications, standard Ingresses and Services, not publication status,
Secrets, privileged pods, nodes, or host sockets. Installing workloads may need
separate Deployment/PVC permissions. Giving permission to modify a referenced
Ingress/Service also gives control of its published content.

See [the complete example](examples/workload-publication.yaml), including
namespace, service account, RoleBinding, Service, Ingress and publication.
Its Service selects an already-installed workload labelled `app: web`.
Apply the authorization objects as the administrator; the workload manager
creates/manages the last three resources with its own identity.

```yaml
apiVersion: networking.containersanywhere.dev/v1alpha1
kind: WorkloadPublication
metadata:
  name: web
  namespace: example
spec:
  ingressRef: web
  hostname: app.example.local
  enabled: true
  lan: true
  mdns: true
  tls:
    mode: LocalCA
```

| Field | Contract |
| --- | --- |
| `ingressRef` | Required name of a same-namespace `containers-anywhere` Ingress. No namespace field or cross-namespace references. |
| `hostname` | Required exact rule hostname under `<namespace>.local` or `<namespace>.localhost`. No wildcards/IPs/catch-all routes. |
| `enabled` | Defaults true. False withdraws routing and advertisements. |
| `lan` | Defaults true. False restricts workload requests to loopback clients, using Caddy's authenticated forwarding metadata. |
| `mdns` | Defaults false. True requires LAN and `.local`; advertisements require successful probes. |
| `tls.mode` | Defaults `LocalCA`: host-managed local-CA HTTPS and HTTP redirect. `HTTP` explicitly serves unencrypted HTTP and denies HTTPS workload requests. No ACME, uploaded certificates or automatic trust. |

The structural schema and admission policies are in `nix/publication.yaml`.
Unsupported modes and malformed settings are rejected by Kubernetes without
replacing the last valid object. Semantic errors such as a deleted/missing
backend withdraw only that publication with `Accepted=False`; they are not
silently treated as success. Services must be selector-backed non-headless
ClusterIP Services, with a matching TCP port and no external IPs.

The guest controller reconciles approximately every three seconds, producing
controller-owned `ca-pub-<UID>` Ingresses with class
`containers-anywhere-published`. Traefik reads only that class, not workload input
Ingresses. Admission reserves derived routes to the appliance's local K3s
administrator (`system:admin`), including updates, deletes and class changes.
It copies only the selected host's standard Service paths, never arbitrary
annotations/Caddy configuration. Management namespaces and demo names are
reserved. This is not isolation against cluster administrators, privileged pods,
or users independently authorized to forge endpoint data.

Every competing valid claim to one hostname is rejected with
`HostnameConflict`; no list-order winner is selected. Removing a competing claim
allows reconciliation again. Namespace suffix ownership prevents a different
namespace claiming your hostname. A maximum of 128 publications is supported.

The host polls authenticated snapshots over vsock/per-boot mTLS, updates its
request allowlist, probes the resulting routes, and sends observations back over
the same private transport. Status patches test UID and resourceVersion; stale
host observations cannot report readiness for a replacement or changed object.
Restart reconstructs routing from current Kubernetes state and preserves the CA.

```sh
kubectl get workloadpublications -n example
kubectl get wp web -n example -o json
kubectl patch wp web -n example --type=merge -p '{"spec":{"enabled":false}}'
kubectl delete wp web -n example
```

`status.url` includes the selected host port. `status.observedGeneration` and
each condition's `observedGeneration` identify the desired version evaluated.
`Accepted` means validation succeeded; `Programmed` means the host applied the
snapshot; `Ready` means the local workload probe responded; `CertificateReady`
means the TLS chain and hostname verified against this appliance's CA;
`Advertised` means local mDNS publication succeeded. None asserts remote-client
trust or firewall passage. Missing host feedback becomes Unknown after 60 seconds
while the controller is running. A stopped controller cannot refresh persisted
status; do not interpret stored conditions as a continuous health guarantee.

For a hostname change, update the referenced Ingress rule and the publication.
An intermediate mismatch explicitly withdraws the affected route. Set
`tls.mode: HTTP` only for non-sensitive traffic. Delete or disable a publication
to revoke it: removing an RBAC binding prevents future edits but, like other
Kubernetes objects, does not delete objects already created.

## Appliance-wide listener settings

There are no host publication grants. Optional `gateway.json` configures only
global listener addresses and HTTP/HTTPS ports; it is not a per-workload API:

```json
{"port": 8443, "httpPort": 8080, "addresses": ["127.0.0.1", "192.168.1.20"]}
```

Use `gateway-configure --file <file> --state <state>` on a stopped cluster only
if those host-wide defaults need changing. Explicit settings require loopback
plus optional private IPv4 addresses, or a sole `0.0.0.0`; omitted addresses
mean loopback. Unknown fields, including removed `namespaces`, `domains` and
`mdns` grants, fail explicitly. There is no alpha migration/dual-authority path
and no automatic deletion or rewriting of incompatible state. Use a matching
current appliance with a new empty state directory rather than bypassing exact
appliance identity checks. The packaged, hash-verified Caddy helper is required.

## Out-of-box demo and mDNS

The `containers-demo` namespace includes a preloaded unprivileged Caddy static
server, Service and Ingress for demo.local and demo.localhost. The namespace and
resource name reserve those exact hostnames. No external image download is
needed. The branded page is actually served by that pod, through Traefik and
native Caddy; it is not a launcher fallback page.

Two ordinary publication objects enable the demo: `demo` for demo.local
(LAN/mDNS/LocalCA) and `demo-localhost` for demo.localhost (loopback/LocalCA).
Both serve HTTPS with certificates from the appliance's local CA; plain HTTP on
the HTTP port returns a 308 redirect to HTTPS (plus the /root.crt bootstrap
resource). Browsers warn until the public root certificate is trusted.
The packaged demo is the only preinstalled application. Names are reserved to
those exact namespace/resource pairs.

The host announces each approved `.local` name only after a probe traverses
Caddy, the authenticated VM bridge and Traefik to a responding workload on every
selected address. LocalCA applications are probed at `GET /` over HTTPS, using the
persisted Caddy root CA and the correct TLS hostname; no certificate verification
is bypassed. HTTP redirects from Caddy alone do not establish readiness.
Application 2xx/3xx/401/403 responses count as responding; redirects are not
followed and no application credentials are sent. Other statuses (including
404/5xx), connection failures and invalid TLS are reported, not advertised.
Applications serving only a non-root path remain routable but will not be marked
responding or advertised until their root responds. This is endpoint availability,
not application health, login success or a production readiness guarantee.

The demo is probed over HTTPS like any LocalCA publication and must also
return its expected page.
The responder probes for conflicting names, sends A/PTR/SRV/TXT
records on configured IPv4 interfaces, refreshes them, and sends goodbye records
on stop, removed approval or failed endpoint checks. LocalCA SRV records use
`_https._tcp` and the configured HTTPS port; HTTP publications use `_http._tcp`
and the HTTP port. No CARE/CNCM names are synthesized and no AAAA records are published.
Name conflicts are reported, not silently renamed; a conflict conservatively
withdraws the current advertisement set. Only one owner of a name can be used
per multicast link. Network changes withdraw stale
advertisements; the owner must reconfigure changed addresses.

From a second computer with the launcher installed, run
`containers-anywhere demo-check`. It performs direct multicast A/SRV discovery
(bypassing hosts-file/DNS-cache shortcuts), rejects a self-test address, then
retrieves the expected pod page over HTTPS (SNI demo.local) on the discovered
host port. It does not start a VM, install trust, or change firewall settings.
Without arguments it does not verify the certificate, so it is a connectivity
check, not an identity check; mDNS can be spoofed on the LAN. Pass the
appliance's public root certificate (`containers-anywhere demo-check root.crt`)
to also verify the certificate chain and hostname.

Open `https://demo.local` from a second device (use `:8443` if low ports were
unavailable). LocalCA publications redirect HTTP requests to HTTPS;
the exact public /root.crt bootstrap resource is also available over HTTP.
Phones can test the path before CA enrollment. HTTP bootstrap is not authenticated;
verify the public CA with the administrator before trusting it.

A successful local health probe does not prove a firewall permits LAN clients.
Windows must allow the configured TCP ports and UDP 5353 on the intended private
network; macOS must permit local-network discovery and the Caddy application;
Linux must permit these ports in its active firewall. This source does not
disable firewalls or silently change Windows network profiles. Wi-Fi client
isolation and multicast-blocking networks can prevent discovery/reachability.

Caddy discovers publications every five seconds and issues certificates on
the first TLS handshake. No raw Caddy JSON, CA private keys, admin endpoint, or
host filesystem access is exposed inside Kubernetes. Transient discovery
failures retain the last approved host list and report degradation. Successful
discovery applies deletions. Existing WebSocket sessions are not forcibly
revoked by publication deletion; stop the cluster for immediate session teardown.
Cached TLS certificates are not erased on removal, but the bridge denies new
workload requests regardless of Caddy's certificate cache.

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
destination. Restore the CA identity before starting, so explicitly enrolled
clients retain the same root.

Existing data still requires its exact original appliance. This guest-agent and
Traefik change does not implement an upgrade/migration path for earlier alphas.

## Diagnostics and limits

`gateway-status` distinguishes discovery errors from Caddy process exits.
`urls` lists programmed HTTP/HTTPS routes, not a health assertion. `endpoints` reports
each probe URL, `ready` (responding through the gateway), `certificateReady`, `error`, and
`advertised` (mDNS announced). The desktop shows responding workload links and
individual probe errors without automatically opening any application.
Probe cycles are bounded to 15 seconds plus multicast setup; no-response apps
can delay the next cycle. A failure withdraws advertisements on the next check.
Certificate trust is reported as manual, never inferred
from a running process. See gateway.log in desktop Diagnostics. A helper crash
is reported; restart the cluster to restart it. No automatic retry of application
POSTs is introduced. The relay limits concurrent guest connections to 128.

This slice supports HTTP/1.1 and WebSocket upgrades through the bridge. Caddy
accepts browser HTTP/2, but upstream gRPC/h2c and UDP/HTTP3 are not implemented.
There is no patient-data access logging by default. Demo readiness is exposed
as demoURL (the responding `https://demo.local` endpoint) and discoveryError in gateway-status and the desktop panel.

CI validates the generated config with the packaged Caddy. The KVM smoke test
uses projected service-account tokens from workload pods to create Service,
Ingress and publication objects; checks unauthorized/RBAC, unsupported TLS,
reserved names, cross-namespace references, derived-route and status-write
denials; exercises conflicts, hostname/TLS changes, disable/delete and recreation.
It then exercises trusted HTTPS, management isolation, reboot and encrypted
restore with identical CA and PVC markers. The test harness pulls curl/busybox
workload images, without installing runtime packages. These tests must run
successfully before release. Physical LAN, certificate enrollment, long-lived
streams, upload stress, suspend/wake and network changes still require dedicated
acceptance; source changes and compile checks do not establish those results.

## Client onboarding, certificate trust and firewalls

Read `HostStatus.status.host.gateway` (see [HOST_API.md](HOST_API.md)) for the ports, bound addresses, the
root-CA URL and its SHA-256 fingerprint. It reports whether the 8080/8443 fallback is in use. A `lan-check`
request dials this computer's own LAN addresses and reports whether the HTTPS port accepts connections;
it is **not** proof that another device can reach the gateway.

**Trusting the CA on a client.** Download the public root from `http://<lan-ip>:<http-port>/root.crt`, then
compare its SHA-256 fingerprint with the value the launcher or your manager shows over a *different* channel.
Fetching the certificate over HTTP only is trust on first use; it does not authenticate the certificate. Install
only the public `root.crt`, never a key. Applications use HTTPS; the demo is also published over HTTP only so a new
device can test connectivity before enrolling.

**Firewalls.** None of the following has been verified on every platform; treat it as what was observed:

- *Ports.* The gateway prefers 80/443 and falls back to 8080/8443 when they cannot be bound. On Linux, binding below
  1024 needs `CAP_NET_BIND_SERVICE` or `net.ipv4.ip_unprivileged_port_start`. A Windows 11 machine and macOS both
  bound 80/443 as an ordinary user.
- *Windows.* Defender Firewall prompts on the first inbound listener; allow it on **Private** networks. No firewall
  rule is created automatically.
- *macOS.* The Application Firewall may prompt for the bundled Caddy helper. More importantly,
  **System Settings > Privacy & Security > Local Network** must allow Containers Anywhere. If it was
  denied (or the prompt was dismissed), macOS silently drops inbound LAN connections and multicast: the
  gateway looks healthy and everything works from the Mac itself, but no other device can reach it. The app is
  ad-hoc signed, so a new build is a new identity and macOS may ask again; after changing the setting, quit and
  relaunch the app so the gateway restarts under the new permission.
- *Management stays private.* Kubernetes (6443), the guest agent (8080 inside the VM), Kite and launcher control were
  not reachable from the LAN in testing; only the workload gateway ports were.

**When a device cannot resolve the name.** The gateway is name-based, so `https://<ip>` always fails; use the name.
The public root can be downloaded by address (`http://<lan-ip>:<http-port>/root.crt`; before 0.3.1 only when the
`Host` header is `demo.local`). If `.local` does not resolve on a device (Android apps and many Linux setups
without `avahi`/`nss-mdns`; any network that filters multicast), add `<lan-ip> demo.local` to that device's hosts
file after trusting the root, or use a name you control.

**mDNS (`.local` names).** Announcements need multicast on UDP 5353. Wi-Fi client isolation and multicast filtering
silently break them, and a machine running Apple Bonjour (`mDNSResponder`) also holds 5353. The demo uses the fixed
names `demo.local` / `demo.localhost`, so **two appliances on one LAN claim the same name and conflict**; per-instance
demo names would fix that.

**Windows clients.** Tools built on Schannel (for example `curl.exe`) fail against a private CA with "revocation status
is unknown" unless run with `--ssl-no-revoke`; browsers behave once the root is trusted.

## Remaining product work

- Implement per-platform firewall/80/443 setup and network-profile enrollment.
- Add client trust workflows and configurable application health paths.
- Add Gateway API support only if needed; this CRD/Ingress contract is not
  Gateway API conformance.
- Add supervised automatic recovery of the edge and host startup integration.
- Prove streaming, upload/download, source-IP behavior and permission-denial
  handling on actual macOS, Windows WHPX and both Linux architectures.
