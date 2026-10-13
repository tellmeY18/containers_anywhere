#!/usr/bin/env python3
"""Archive exactly the verified appliance members, never a runtime state disk."""
import json
from pathlib import Path
import sys
import tarfile

from package import checksum, digest


def pack_bundle(root, output):
    m = json.loads((root / "manifest.json").read_text())
    names = {"kernel", "initrd", "system.img", "data.img"}
    if m.get("format") != 3 or m.get("product") != "containers-anywhere" or set(m["files"]) != names:
        raise ValueError("Invalid appliance manifest")
    for name in names:
        path = root / name
        if path.is_symlink() or not path.is_file() or digest(path) != m["files"][name]:
            raise ValueError(f"Invalid appliance member: {name}")
    with tarfile.open(output, "w:gz") as archive:
        for name in ["manifest.json", *sorted(names)]:
            archive.add(root / name, arcname=name, recursive=False)
    checksum(output)


if __name__ == "__main__":
    pack_bundle(Path(sys.argv[1]), Path(sys.argv[2]))
