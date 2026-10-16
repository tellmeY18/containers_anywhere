# Release and cache reconnaissance

Inspected on 2026-10-16. Baseline: successful [build 38889610837](https://github.com/tellmeY18/containers_anywhere/actions/runs/38889610837),
plus its job logs and the live Actions cache inventory. Changes below require a
new CI run; predicted savings are not benchmark results.

## Where the time went

Total build wall time: **43m 47s** (10:48:10–11:31:57 UTC).

| Work | Measured duration | Observation |
| --- | ---: | --- |
| Checks job | 56s | Not the bottleneck |
| AMD64 appliance job | 5m 23s | EROFS derivation interval about 2m 06s |
| ARM64 appliance job | 20m 46s | EROFS derivation interval about 18m 09s |
| ARM64 Nix cache restore / post-save | 43s / 21s | Cache restored successfully |
| Windows package job | 21m 56s | Boot acceptance accounts for 19m 02s |
| Linux AMD64 boot/restart/restore | 5m 31s | Keep this acceptance gate |
| macOS package job | 4m 10s | Appliance download alone took 1m 12s |
| Draft preparation run 38894698972 | 3m 11s | Promotes tested artifacts; no rebuild |

The previous matrix dependency made **every package wait for both appliances**.
AMD64 was ready at 10:54:34 but Windows started only at 11:10:00. With the same job
durations and runner availability, independent architecture pipelines would finish
in roughly **28–29 minutes**, saving about **15 minutes** without removing tests.

The EROFS flags included `-Ededupe`. The pinned microvm source explicitly selects
single-threaded erofs-utils when this flag is present. ARM64's long image-build
interval is observed; dedupe's exact contribution versus runner CPU performance
still needs an A/B measurement. Python's gzip archive creation is also serial,
but the ARM64 bundle-build-plus-archive tail was only about 44 seconds in this run.

## Changes in this pass

- A reusable `build-architecture.yml` owns one appliance and its two packages.
  AMD64's Windows/Linux acceptance can overlap the ARM64 build. Artifact names and
  the single-run release gate remain compatible with `scripts/release.py`.
- Drop EROFS block deduplication to enable the upstream parallel builder while
  retaining LZ4HC, fragments and tail packing. Compare size as well as elapsed time.
- Root the current Nix bundle with an output link during post-job cache GC.
  Previously `--no-link` left it eligible for collection. Logs show old bundles
  and store images being collected, not proof that the current output was lost.
- Include `flake.nix` and nested Go sources in cache keys. The old key missed both,
  even though they can change derivations. Nix still prevented stale build outputs,
  but an exact immutable cache hit could prevent saving newly built paths.
- Separate build and archive timings; publish per-architecture runtime closure
  inventories and component/download sizes without adding them to release assets.
- Install only the native Linux QEMU target, without recommended extra packages.
- Cancel superseded PR checks; preserve in-flight release builds.
- Remove the guest's Python secret-generation dependency and unused userspace
  configuration described in [APPLIANCE.md](APPLIANCE.md).

## Cache findings and next priorities

1. **Stop rebuilding the appliance for host-only edits.** The guest agent currently
   builds the entire root Go application; its fileset includes every Go source,
   including tests. A desktop UI or launcher change consequently changes the agent,
   system closure and image. Extract a small `cmd/guest-agent` with only the shared
   transport/key helpers it needs, then key appliance reuse on those real inputs.
   Do not merely remove host files from the key while the derivation still uses them.
2. **Use a dedicated Nix binary cache if builds remain frequent.** At inspection,
   Actions held 10.09 GB across seven entries: current ARM64 3.56 GB, current AMD64
   2.73 GB, one older ARM64 3.56 GB, plus Go caches. Whole-store snapshots duplicate
   most paths and put pressure on the repository's configured quota. A path-based
   Cachix/Attic cache avoids repeatedly saving unchanged paths. Keep signed-cache
   verification and restrict publishing credentials to trusted builds. Inspect quota
   and eviction before increasing store-size limits; cache deletion is not needed
   for correctness.
3. **Accelerate Windows acceptance with actual WHPX hardware.** The 19-minute TCG
   test is intrinsically slow. A suitable isolated self-hosted Windows runner can
   test hardware acceleration and reduce time, but first prove WHPX and retain the
   current release acceptance until its replacement passes. Do not silently skip
   the Windows test or count TCG as WHPX coverage.
4. **Avoid duplicate full builds and documentation-only builds.** Push plus manual
   dispatch can queue the same work; two closely spaced runs were active/pending
   during inspection. Trigger one build per candidate. Add deliberate path filters
   if documentation-only builds are frequent, accounting for README/license content
   embedded in installers. Running push and dispatch in separate concurrency groups
   would spend more compute rather than eliminate duplication.
5. **Avoid full appliance transfers to packaging-only jobs.** macOS and Linux ARM64
   use the archive only to calculate the hash embedded in the launcher. Publish a
   small trusted hash/metadata artifact from the same build run and let those jobs
   consume it. Linux AMD64/Windows still need the actual appliance for acceptance.
   Retain digest verification and single-run provenance; never accept an arbitrary
   user-supplied checksum as a substitute for build provenance.
6. **Cache final exports only after controlling cache pressure.** An exact bundle
   cache can avoid repeated hashing/gzip on unchanged builds. This is secondary to
   EROFS and the pipeline dependency, and adding another ~2 GB Actions snapshot now
   could evict more useful Nix paths. Benchmark pigz against Python gzip before
   changing the archive writer; preserve its member allowlist and hash checks.

Go caching, native architecture builders, zero-compression uploads for already
compressed archives, and promoting one successful run rather than rebuilding for
release are already sensible. Per-platform Go race tests are cheap relative to boot
tests and cover different host implementations.

## Further VM-size work

Use the new closure report to rank actual byte savings. The next candidates are
disabled-component airgap images (Traefik/ServiceLB/metrics-server), Python/Perl
retained by transitive references, the generic kernel/module set, and duplicate
preload/containerd storage. Prove DNS, CNI, local-path helper pods and offline
restart/restore before pruning images. Kernel/initrd specialization requires macOS,
Linux ARM64/AMD64 and physical Windows acceptance. Removing the immutable preload
copy or lowering the sparse disk's logical capacity is not an equivalent safe
download-size optimization.

Required follow-up: CI Go vet/race tests, both Nix builds, all packages, Linux KVM
fresh boot/workload/reboot/backup/restore and Windows TCG acceptance; then fresh and
cached/offline startup plus the outstanding real-hardware matrix. Record before/
after compressed size, system image size, closure bytes and job times. No reduced
memory minimum, platform boot result or final image-size saving is claimed yet.

Local validation completed: actionlint (including shellcheck), gofmt,
nixpkgs-fmt, both bundle derivation evaluations, both configurations' assertions,
and the Python archive/report tests. The Go secret persistence/corruption tests
are added to the existing CI test suite; Go compilation/race tests and VM boot
acceptance run in Actions according to repository policy.
