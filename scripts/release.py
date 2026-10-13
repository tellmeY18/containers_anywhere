#!/usr/bin/env python3
"""Promote one successful Containers Anywhere build without rebuilding artifacts."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile

from package import digest


def gh(*args):
    return subprocess.check_output(["gh", *args], text=True).strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-id", required=True)
    args = parser.parse_args()
    repo = os.environ["GITHUB_REPOSITORY"]
    run = json.loads(gh("api", f"repos/{repo}/actions/runs/{args.run_id}"))
    if run["conclusion"] != "success" or run["head_branch"] != "containers-anywhere" or run["path"] != ".github/workflows/alpha.yml" or run["event"] not in {"push", "workflow_dispatch"}:
        raise ValueError("Select a successful containers-anywhere MVP build")
    # Source VERSION is read from the tested commit, not the publisher checkout.
    import base64
    source = json.loads(gh("api", f"repos/{repo}/contents/VERSION?ref={run['head_sha']}"))
    version = base64.b64decode(source["content"]).decode().strip() + f"-mvp.{run['run_number']}"
    tag = "containers-v" + version
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        for name in ["appliance-amd64", "appliance-arm64", "package-linux-amd64", "package-linux-arm64", "package-macos-arm64", "package-windows-amd64"]:
            gh("run", "download", args.run_id, "--repo", repo, "--name", name, "--dir", tmp)
        names = {f"appliance-{arch}.tar.gz" for arch in ["arm64", "amd64"]}
        names |= {f"runtime-{platform}.tar.gz" for platform in ["linux-amd64", "linux-arm64", "windows-amd64"]}
        names |= {f"Containers-Anywhere-{version}-{p}.{ext}" for p, ext in [("linux-amd64", "AppImage"), ("linux-arm64", "AppImage"), ("macos-arm64", "dmg"), ("windows-amd64", "exe")]}
        if {p.name for p in root.iterdir()} != names | {n+".sha256" for n in names}:
            raise ValueError("Incomplete release asset set")
        for name in names:
            sha, recorded = (root / (name+".sha256")).read_text().split()
            if recorded != name or digest(root/name) != sha:
                raise ValueError("Checksum mismatch: " + name)
        gh("release", "create", tag, "--repo", repo, "--target", run["head_sha"], "--draft", "--prerelease", "--title", "Containers Anywhere " + version,
           "--notes", f"K3s + Kite v0.16.0 MVP. Build: {run['html_url']}. VM and QEMU download on first launch. See README for platform acceptance boundaries.")
        gh("release", "upload", tag, "--repo", repo, *[str(root / n) for n in sorted(names | {n+".sha256" for n in names})])
        print(f"Draft prepared: https://github.com/{repo}/releases/tag/{tag}")
        print("Publish the complete draft to make its pinned first-run download URLs reachable.")


if __name__ == "__main__":
    main()
