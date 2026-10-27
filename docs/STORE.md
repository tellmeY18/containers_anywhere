# Store (proof of concept)

The desktop home screen lists deployments that publishers have made installable on Containers
Anywhere. Discovery works like Zigistry: there is no registry service. A repository opts in by
carrying the GitHub topic **`containers-anywhere`** and a Kubernetes manifest at a conventional path.

**This is a proof of concept with no trust mechanism.** Every entry is shown as *unverified*. Installing
one runs its publisher's code on your cluster with whatever access its manifest requests, which can be
full control of the cluster and everything in it. Do not use it with data you care about.

## Publishing

1. Add a manifest to the repository's default branch at the first of these paths that exists:
   `containers-anywhere.yaml`, `containers-anywhere.yml`, `manifests/containers-anywhere.yaml`,
   `manifests/<repo>-containers-anywhere.yaml`.
2. Add the topic `containers-anywhere`.

The manifest is ordinary multi-document Kubernetes YAML (at most 256 KiB, 64 resources). It may use the
platform's `WorkloadPublication`, `HostRequest` and `HostRegistryTrust` resources. Pin images by digest.

## What the launcher does

- Searches GitHub for the topic (at most 30 repositories), fetches each manifest from
  `raw.githubusercontent.com`, and caches the list for an hour in `<state>/store-cache.json`. It works
  offline with the last list. Set `CONTAINERS_GITHUB_TOKEN` to raise GitHub's rate limit.
- Summarises each manifest with a line-based scan (not a YAML parser, and **not a security boundary**):
  resource kinds, namespaces, images and risks (cluster-admin, privileged containers, host paths, host
  networking, exposed ports, webhooks, unpinned images, LAN publication).
- Install: the panel shows the summary and the manifest's SHA-256, asks for confirmation (high risk also
  requires typing the repository name), re-downloads the manifest, and refuses if its hash differs from
  what was reviewed. The guest agent applies it as the platform add-on `store-<repo>-<hash>`.
- Refused outright: namespaces `kube-*`, `kite-system`, `containers-gateway`, `containers-demo`.
- Installs are recorded in `<state>/store.json`. No appliance change is needed.

## Limits

- **No uninstall.** Removing an add-on would delete its volumes, so it needs a deliberate design. Remove
  workloads in Kite for now; the add-on file is re-applied on every K3s start.
- A manifest is fetched from the branch tip, so it can change between listing and review (the hash check
  covers review to install, not listing to review).
- Topics are self-asserted. Anyone can publish, impersonate a name, or squat a topic.

## Trust roadmap (required before this is more than a POC)

1. Pin to a commit or release tag instead of the branch tip.
2. Signed manifests and build provenance (for example Sigstore/GitHub attestations) tied to the publisher.
3. A curated or maintainer-signed allowlist, with unverified entries behind an explicit opt-in.
4. A permission policy: deny cluster-admin, privileged and host access by default, with per-install grants.
5. Uninstall, update notifications and re-review when a manifest changes.
6. Revocation: a way to delist a malicious repository and warn those who installed it.
