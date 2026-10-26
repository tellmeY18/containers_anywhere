#!/usr/bin/env python3
"""Prepare a new draft from tested immutable artifacts; never rebuild or publish."""
import argparse
import base64
import json
import os
from pathlib import Path
import re
import shutil
import tarfile
import tempfile

from components import APPLIANCES, PLATFORMS, PUBLISHER, RUNTIMES, digest, gh, semver, validate_lock


def build_component(run):
    workflows = {".github/workflows/app.yml": "app",
                 ".github/workflows/appliance.yml": "appliance"}
    if (run["conclusion"] != "success" or run["head_branch"] != "develop"
            or run["path"] not in workflows or run["event"] != "workflow_dispatch"
            or run["run_attempt"] != 1
            or run["repository"]["full_name"] != PUBLISHER):
        raise ValueError("Select a successful first-attempt component dispatch on develop")
    return workflows[run["path"]]


def asset_names(component, version):
    if component == "appliance":
        return APPLIANCES | RUNTIMES
    return {f"Containers-Anywhere-{version}-{p}.{ext}" for p, ext in PLATFORMS.items()}


def verify_assets(root, names):
    for name in names:
        path = root / name
        sha, recorded = (root / (name + ".sha256")).read_text().split()
        if not path.is_file() or recorded != name or digest(path) != sha:
            raise ValueError("Checksum mismatch: " + name)


def appliance_contract(root, version):
    for arch in ("amd64", "arm64"):
        with tarfile.open(root / f"appliance-{arch}.tar.gz", "r:gz") as archive:
            member = archive.getmember("manifest.json")
            if not member.isfile() or member.size > 1 << 20:
                raise ValueError("Invalid appliance manifest")
            manifest = json.load(archive.extractfile(member))
        if (manifest.get("version") != version or manifest.get("protocol") != 1
                or manifest.get("format") != 3 or manifest.get("arch") != arch
                or manifest.get("product") != "containers-anywhere"):
            raise ValueError("Appliance artifact identity differs from tested source")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-id", required=True)
    args = parser.parse_args()
    if not re.fullmatch(r"[1-9][0-9]*", args.run_id):
        raise ValueError("Expected a numeric build run ID")
    repo = os.environ["GITHUB_REPOSITORY"]
    if repo != PUBLISHER:
        raise ValueError("Release publication is restricted to the approved publisher")
    run = json.loads(gh("api", f"repos/{repo}/actions/runs/{args.run_id}"))
    component = build_component(run)
    version_file = "VERSION" if component == "app" else "APPLIANCE_VERSION"
    source = json.loads(gh("api", f"repos/{repo}/contents/{version_file}?ref={run['head_sha']}"))
    version = semver(base64.b64decode(source["content"]).decode().strip())
    tag = component + "-v" + version
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        downloads, assets = root / "downloads", root / "assets"
        assets.mkdir()
        artifacts = [f"package-{p}" for p in PLATFORMS]
        if component == "appliance":
            artifacts += ["appliance-amd64", "appliance-arm64"]
        else:
            artifacts += ["dependencies"]
        for name in artifacts:
            gh("run", "download", args.run_id, "--repo", repo, "--name", name, "--dir", str(downloads / name))
        names = asset_names(component, version)
        for name in names | {n + ".sha256" for n in names}:
            matches = list(downloads.glob("*/" + name))
            if len(matches) != 1:
                raise ValueError("Missing or ambiguous release asset: " + name)
            shutil.copy2(matches[0], assets / name)
        verify_assets(assets, names)
        descriptor = {"schema": 1, "component": component, "version": version,
                      "source": run["head_sha"], "build": run["html_url"],
                      "files": {n: digest(assets / n) for n in sorted(names)}}
        if component == "appliance":
            appliance_contract(assets, version)
            descriptor["protocol"] = 1
        else:
            lock = json.loads((downloads / "dependencies/dependencies.json").read_text())
            validate_lock(lock)
            descriptor["dependencies"] = lock
        (assets / f"{component}-release.json").write_text(json.dumps(descriptor, indent=2) + "\n")
        # Atomic ref creation refuses any existing tag. Never --clobber assets,
        # move a tag, or recreate an identity after a partial promotion.
        gh("api", f"repos/{repo}/git/refs", "-f", "ref=refs/tags/" + tag, "-f", "sha=" + run["head_sha"])
        gh("release", "create", tag, "--repo", repo, "--verify-tag", "--draft", *(["--prerelease"] if "-" in version else []),
           "--title", f"Containers Anywhere {component} {version}",
           "--notes", f"Tested artifacts from {run['html_url']}; source {run['head_sha']}. "
           "Linux AMD64 KVM boot/reboot/encrypted restore; all four packages built. "
           "Physical macOS, Linux ARM64 KVM and Windows WHPX acceptance remains outstanding. "
           "Publisher-account trust, HTTPS and fixed hashes; no custom signing keys. "
           "This draft is not authorization to ship 0.1.0 before its remaining gates.")
        gh("release", "upload", tag, "--repo", repo, *[str(p) for p in sorted(assets.iterdir())])
        print(f"Complete draft: https://github.com/{repo}/releases/tag/{tag}")
        print("Nothing published. Do not publish final 0.1.0 until all agreed gates pass.")


if __name__ == "__main__":
    main()
