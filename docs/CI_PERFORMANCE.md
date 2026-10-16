# Release and cache reconnaissance

Inspected on 2026-10-16. Baseline: successful [build 38889610837](https://github.com/tellmeY18/containers_anywhere/actions/runs/38889610837).
Verified result: [build 38909720435](https://github.com/tellmeY18/containers_anywhere/actions/runs/38909720435),
published as `containers-v0.1.0-mvp.49`. Change lists and predictions are not
benchmark results; only the two tables below are measured.

## Measured result: baseline versus verified build

| Work | Baseline (38889610837) | Verified (38909720435) |
| --- | ---: | ---: |
| Total wall time | 43m 47s | **31m 01s** (−29%) |
| Checks job | 0m 56s | 0m 36s |
| AMD64 appliance | 5m 23s | 4m 06s |
| ARM64 appliance | 20m 46s | **2m 57s** |
| macOS package | 4m 10s | 1m 29s |
| Linux ARM64 package | 1m 55s | 0m 49s |
| Linux AMD64 boot/restart/restore | 5m 31s | 6m 53s |
| Windows TCG boot acceptance | 21m 56s | 26m 06s |
| ARM64 compressed appliance | 877.2 MB | **789.2 MB** (−88.0 MB, −10.0%) |
| AMD64 compressed appliance | 1,091.6 MB | **1,002.3 MB** (−89.4 MB, −8.2%) |
| Guest runtime closure (ARM64/AMD64) | not measured | 1.50 GB / 1.31 GB, 372 paths each |

Removing EROFS block deduplication was expected to cost image size. Userspace
slimming more than paid for it: both appliances shrank while the ARM64 image build
dropped from 18m 09s to well under three minutes. Two changes landed together, so
their individual size contributions are not separated; the closure report makes
that measurable next time.

**Windows TCG acceptance is now 84% of the build.** It grew from 21m 56s to
26m 06s on identical hardware class, so treat TCG duration as variable rather than
regressing code. It, not the Nix build, is the next scheduling target.

## Baseline observations that motivated the change

Baseline build 38889610837, 43m 47s (10:48:10–11:31:57 UTC):

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
AMD64 was ready at 10:54:34 but Windows started only at 11:10:00.

The EROFS flags included `-Ededupe`. The pinned microvm source explicitly selects
single-threaded erofs-utils when this flag is present. Python's gzip archive
creation is also serial, but the ARM64 bundle-build-plus-archive tail was only
about 44 seconds, so the archive writer is not a bottleneck.

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
  The report prints human-readable sizes (kB/MB/GB) and, for the largest paths,
  the reference chain that pulled them in, so each reduction is attributable.
- Install only the native Linux QEMU target, without recommended extra packages.
- Cancel superseded PR checks; preserve in-flight release builds.
- Remove the guest's Python secret-generation dependency and unused userspace
  configuration described in [APPLIANCE.md](APPLIANCE.md).

## Windows WHPX coverage without WHPX hardware

GitHub-hosted Windows runners cannot use nested virtualization, so WHPX cannot be
exercised on hosted runners; Linux hosted runners can, which is why the Linux KVM
gate runs there today. A self-hosted Windows runner on nested-virt-capable hardware
(Azure Dv4/Dv5 or Ev5, or EC2 metal) remains the only way to test WHPX itself.

As an interim gate, `CONTAINERS_QEMU_MACHINE=pc` selects the i440fx machine while
keeping `-cpu host` under KVM, so the configuration WHPX depends on is booted,
deployed and rebooted on every build. This validates the machine/CPU pair that
caused the earlier WHPX CPU-model bug. It is **not** WHPX acceptance and must
never be reported as such.

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
3. **Accelerate Windows acceptance with actual WHPX hardware.** TCG is now 84% of
   the build. A suitable isolated self-hosted Windows runner can test hardware
   acceleration and reduce time, but first prove WHPX and retain the current release
   acceptance until its replacement passes. Do not silently skip the Windows test
   or count the `pc`-machine KVM gate as WHPX coverage.
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

Ranked candidates with measured sizes are in [APPLIANCE.md](APPLIANCE.md). The
largest is pruning the three disabled airgap images (76.1 MB), then trimming the
kernel module set (351.6 MB combined with kernel and initrd, which needs a kernel
build change and the full platform matrix). Perl and `gcc-lib` (78.2 MB) are
referenced directly by NixOS's own user-activation scripts, so they are not
removable without replacing user setup. Prove DNS, CNI, local-path helper pods and
offline restart/restore before pruning images. Removing the immutable preload copy
or lowering the sparse disk's logical capacity is not an equivalent safe
download-size optimization.

Still outstanding and not addressed by alpha 49: fresh and cached/offline startup
profiling, guest idle memory/CPU measurement, and physical macOS, Windows WHPX and
Linux ARM64 acceptance. Alpha 49 passed Go vet/race tests, Python tests, both Nix
builds, all four packages, Linux KVM fresh boot/workload/reboot/encrypted
backup/restore and Windows TCG boot acceptance. No reduced memory minimum, WHPX
boot result or further image-size saving is claimed.
