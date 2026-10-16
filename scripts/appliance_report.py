#!/usr/bin/env python3
"""Report shipped image sizes and the largest guest store paths with their cause."""
import collections
import json
from pathlib import Path
import sys

MEMBERS = ["kernel", "initrd", "system.img", "data.img"]
UNITS = ["B", "kB", "MB", "GB"]


def human_bytes(value):
    size = float(value)
    for unit in UNITS:
        if abs(size) < 1000 or unit == UNITS[-1]:
            if unit == "B":
                return f"{int(size)} B"
            return f"{size:,.2f} {unit}" if unit == "GB" else f"{size:,.1f} {unit}"
        size /= 1000


def read_closure(closure):
    paths = json.loads(closure.read_text())
    # Nix versions emit either a list, a path-keyed object, or a v2 info object.
    if isinstance(paths, dict):
        paths = paths.get("info", paths)
        return {path: dict(info, path=path) for path, info in paths.items()}
    return {info["path"]: info for info in paths}


def readable(path):
    """Drop the store hash so a chain stays readable."""
    return Path(path).name.split("-", 1)[-1]


def inclusion_paths(paths, largest=10, depth=3):
    """Shortest reference chain from the system closure to each large path."""
    roots = [p for p in paths if "nixos-system-" in p]
    if not roots:
        return {}
    root = max(roots, key=lambda p: paths[p]["narSize"])
    parents, queue = {root: None}, collections.deque([root])
    while queue:
        current = queue.popleft()
        for reference in paths[current].get("references", []):
            if reference in paths and reference not in parents:
                parents[reference] = current
                queue.append(reference)
    chains = {}
    for path in sorted(paths, key=lambda p: paths[p]["narSize"], reverse=True)[:largest]:
        chain, current = [], path
        while current is not None and current != root:
            chain.append(readable(current))
            current = parents.get(current)
        chain.reverse()
        if len(chain) > depth:
            chain = ["...", *chain[-depth:]]
        chains[path] = " -> ".join(chain) or "direct system reference"
    return chains


def report(bundle, archive, closure):
    paths = read_closure(closure)
    print(f"## {archive.name}\n")
    print("| File | Size |\n| --- | ---: |")
    for path in [archive, *(bundle / name for name in MEMBERS)]:
        print(f"| {path.name} | {human_bytes(path.stat().st_size)} |")
    total = sum(info["narSize"] for info in paths.values())
    print(f"\nGuest runtime closure: **{human_bytes(total)}** across {len(paths)} store paths.")
    print("Uncompressed Nix store bytes; not allocated disk or download size.\n")
    chains = inclusion_paths(paths)
    print("| Largest store path | Size | Included because |\n| --- | ---: | --- |")
    for path, chain in chains.items():
        name = Path(path).name
        print(f"| `{name[:70]}` | {human_bytes(paths[path]['narSize'])} | {chain[:150]} |")


if __name__ == "__main__":
    report(*(Path(argument) for argument in sys.argv[1:]))