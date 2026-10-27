{ pkgs, guest }:
let cfg = guest.config;
in pkgs.runCommand "containers-anywhere-bundle" { nativeBuildInputs = [ pkgs.e2fsprogs pkgs.python3 ]; } ''
  mkdir -p $out
  cp ${if pkgs.stdenv.hostPlatform.isAarch64 then "${cfg.microvm.kernel}/Image" else "${cfg.microvm.kernel.dev}/vmlinux"} $out/kernel
  cp ${cfg.microvm.initrdPath} $out/initrd
  cp ${cfg.microvm.storeDisk} $out/system.img
  truncate -s 64M $out/data.img
  mkfs.ext4 -q -F -L ca-data $out/data.img
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
      capabilities=['status', 'logs', 'stop', 'console-proxy', 'platform-apply-v1', 'workload-publication-v1'],
      data_contract='var-lib-v1',
      data_bytes=16*1024**3, arch='${if pkgs.stdenv.hostPlatform.isAarch64 then "arm64" else "amd64"}',
      kernel='kernel', initrd='initrd', system='${cfg.system.build.toplevel}', files=files,
      k3s='${cfg.services.k3s.package.version}'), indent=2))
  PY
''
