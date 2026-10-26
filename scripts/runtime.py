#!/usr/bin/env python3
"""Stage QEMU and its native dependencies on CI, never on the user's computer."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import urllib.request


def download(url, dest, digest, algorithm="sha256"):
    urllib.request.urlretrieve(url, dest)
    with dest.open("rb") as f:
        if hashlib.file_digest(f, algorithm).hexdigest() != digest:
            raise ValueError(f"Checksum mismatch: {url}")


def stage_linux_firmware(dest, arch, source=Path("/")):
    firmware = dest / "share/qemu"
    shutil.copytree(source / "usr/share/qemu", firmware, symlinks=False)
    licenses = dest / "licenses"
    licenses.mkdir(exist_ok=True)
    # QEMU's distro search path includes these directories even with -L set.
    # Include them explicitly so a builder's host installation cannot hide a
    # missing firmware dependency in the downloadable runtime.
    for directory, package in [("usr/share/seabios", "seabios"), ("usr/lib/ipxe/qemu", "ipxe-qemu")]:
        path = source / directory
        if path.exists():
            shutil.copytree(path, firmware, dirs_exist_ok=True, symlinks=False)
            shutil.copy2(source / "usr/share/doc" / package / "copyright", licenses / (package + ".copyright"))
    if arch == "amd64":
        for name in ("bios-256k.bin", "efi-virtio.rom"):
            if not (firmware / name).is_file():
                raise ValueError("Incomplete standalone QEMU firmware: " + name)


def stage_runtime(dest, arch):
    dest.mkdir()
    if os.name == "nt":
        installer = dest.parent / "qemu-setup.exe"
        download("https://qemu.weilnetz.de/w64/2026/qemu-w64-setup-20260811.exe", installer,
                 "5bcf9eed634e8575a37b74f445af41a2fe4106da512d0c30c368301d4c105037fdfab40a5287367a28a957624cddebbc8c07e16c88ab6634f554cdf3d16bf543", "sha512")
        subprocess.run(["7z", "x", str(installer), "-o" + str(dest), "-y"], check=True)
        installer.unlink()
        # Keep the one host target we launch; retain DLLs, licenses and firmware.
        for p in dest.glob("qemu-system-*.exe"):
            if p.name != "qemu-system-x86_64.exe":
                p.unlink()
        # QEMU's Windows distribution keeps firmware at its top level.
        firmware = dest / "share/qemu"
        firmware.mkdir(parents=True, exist_ok=True)
        for pattern in ("*.bin", "*.rom", "*.dtb", "*.fd"):
            for p in dest.glob(pattern):
                shutil.copy2(p, firmware / p.name)
    else:
        name = "qemu-system-" + ("aarch64" if arch == "arm64" else "x86_64")
        binary = Path(shutil.which(name))
        shutil.copy2(binary, dest / "qemu")
        libs = dest / "lib"
        libs.mkdir()
        output = subprocess.check_output(["ldd", str(binary)], text=True)
        for path in re.findall(r"(/[^\s()]+)", output):
            shutil.copy2(path, libs / Path(path).name)
        loader = next(libs.glob("ld-linux*"))
        (dest / name).write_text(
            '#!/bin/sh\nHERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)\n'
            f'exec "$HERE/lib/{loader.name}" --library-path "$HERE/lib" "$HERE/qemu" "$@"\n')
        (dest / name).chmod(0o755)
        shutil.copytree("/usr/share/doc/qemu-system-common", dest / "licenses", symlinks=False)
        stage_linux_firmware(dest, arch)
        for path in re.findall(r"(/[^\s()]+)", output):
            result = subprocess.run(["dpkg-query", "-S", path], capture_output=True, text=True)
            if result.returncode == 0:
                package = result.stdout.split(":", 1)[0]
                notice = Path("/usr/share/doc") / package / "copyright"
                if notice.exists():
                    shutil.copy2(notice, dest / "licenses" / (package + ".copyright"))
    files = {}
    for p in sorted(dest.rglob("*")):
        if p.is_file():
            with p.open("rb") as f:
                files[p.relative_to(dest).as_posix()] = hashlib.file_digest(f, "sha256").hexdigest()
    (dest / "runtime.json").write_text(json.dumps(files, indent=2))
