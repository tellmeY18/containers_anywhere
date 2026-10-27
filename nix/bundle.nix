{ pkgs, guest }:
let cfg = guest.config;
in pkgs.runCommand "containers-anywhere-bundle" { nativeBuildInputs = [ pkgs.btrfs-progs pkgs.python3 ]; } ''
  mkdir -p $out
  cp ${if pkgs.stdenv.hostPlatform.isAarch64 then "${cfg.microvm.kernel}/Image" else "${cfg.microvm.kernel.dev}/vmlinux"} $out/kernel
  cp ${cfg.microvm.initrdPath} $out/initrd
  cp ${cfg.microvm.storeDisk} $out/system.img
  # The data disk is a btrfs created once, here, with its top-level subvolumes. The
  # launcher grows the file and the guest grows the filesystem at boot. Features are
  # pinned so the on-disk format does not change with the tool version.
  mkdir -p tree/@system tree/@snapshots
  truncate -s 256M $out/data.img
  mkfs.btrfs -q -f -L ca-data -O extref,skinny-metadata,no-holes,free-space-tree \
    -r tree --subvol @system --subvol @snapshots $out/data.img
  export OUT=$out
  python - <<'PY'
  import os, json, hashlib, pathlib
  root = pathlib.Path(os.environ['OUT'])
  files = {}
  for name in ['kernel', 'initrd', 'system.img', 'data.img']:
      with (root/name).open('rb') as f:
          files[name] = hashlib.file_digest(f, 'sha256').hexdigest()
  (root/'manifest.json').write_text(json.dumps(dict(
      product='containers-anywhere', version='${pkgs.lib.removeSuffix "\n" (builtins.readFile ../APPLIANCE_VERSION)}', format=3, protocol=1,
      capabilities=['status', 'logs', 'stop', 'console-proxy', 'platform-apply-v1', 'workload-publication-v1', 'host-api-v1', 'storage-v1'],
      data_contract='var-lib-btrfs-v1',
      data_bytes=8*1024**3, arch='${if pkgs.stdenv.hostPlatform.isAarch64 then "arm64" else "amd64"}',
      kernel='kernel', initrd='initrd', system='${cfg.system.build.toplevel}', files=files,
      k3s='${cfg.services.k3s.package.version}'), indent=2))
  PY
''
