#!/usr/bin/env python3
"""Print shipped image sizes and the largest individual guest store paths."""
import json
from pathlib import Path
import sys


def report(bundle, archive, closure):
    paths = json.loads(closure.read_text())
    # Nix versions emit either a list, a path-keyed object, or a v2 info object.
    if isinstance(paths, dict):
        paths = paths.get("info", paths)
        paths = [dict(info, path=path) for path, info in paths.items()]
    total = sum(p["narSize"] for p in paths)
    print(f"## {archive.name}\n")
    print("| File | Bytes |\n| --- | ---: |")
    for path in [archive, *(bundle / n for n in ["kernel", "initrd", "system.img", "data.img"])]:
        print(f"| {path.name} | {path.stat().st_size:,} |")
    print(f"\nGuest runtime closure: **{total:,} NAR bytes**, {len(paths)} store paths.")
    print("This is not allocated disk usage or compressed download size.\n")
    print("| Largest store paths | NAR bytes |\n| --- | ---: |")
    for item in sorted(paths, key=lambda p: p["narSize"], reverse=True)[:30]:
        print(f"| `{Path(item['path']).name}` | {item['narSize']:,} |")


if __name__ == "__main__":
    report(*(Path(arg) for arg in sys.argv[1:]))
