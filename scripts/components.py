#!/usr/bin/env python3
"""Resolve explicit publisher releases to a fixed, hash-pinned dependency lock."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import urllib.request

PUBLISHER = "tellmeY18/containers_anywhere"
PLATFORMS = {"linux-amd64": "AppImage", "linux-arm64": "AppImage",
             "macos-arm64": "dmg", "windows-amd64": "exe"}
APPLIANCES = {f"appliance-{a}.tar.gz" for a in ("amd64", "arm64")}
RUNTIMES = {f"runtime-{p}.tar.gz" for p in PLATFORMS if not p.startswith("macos")}


def semver(value):
    number = r"(?:0|[1-9][0-9]*)"
    identifier = rf"(?:{number}|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
    if not re.fullmatch(rf"{number}\.{number}\.{number}(?:-{identifier}(?:\.{identifier})*)?", value):
        raise ValueError("Expected SemVer without build metadata, e.g. 0.1.0 or 0.1.1-rc.1")
    return value


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def gh(*args):
    return subprocess.check_output(["gh", *args], text=True).strip()


def release_base(tag):
    if not tag.startswith(("appliance-v", "runtime-v")):
        raise ValueError("Dependencies must use explicit appliance-v or runtime-v tags")
    semver(tag.split("-v", 1)[1])
    return f"https://github.com/{PUBLISHER}/releases/download/{tag}"


def validate_descriptor(data, tag):
    component = tag.split("-v", 1)[0]
    release_base(tag)
    names = APPLIANCES | RUNTIMES if component == "appliance" else RUNTIMES
    if (data.get("schema") != 1 or data.get("component") != component
            or data.get("version") != tag.split("-v", 1)[1]
            or set(data.get("files", {})) != names
            or not re.fullmatch(r"[0-9a-f]{40}", data.get("source", ""))):
        raise ValueError("Invalid component release descriptor")
    if component == "appliance" and data.get("protocol") != 1:
        raise ValueError("Unsupported appliance protocol")
    for sha in data["files"].values():
        if not re.fullmatch(r"[0-9a-f]{64}", sha):
            raise ValueError("Missing fixed component hash")


def resolve(tag):
    release_base(tag)
    info = json.loads(gh("api", f"repos/{PUBLISHER}/releases/tags/{tag}"))
    if info["draft"] or info["tag_name"] != tag:
        raise ValueError("Select an already published, explicit component release")
    component = tag.split("-v", 1)[0]
    with tempfile.TemporaryDirectory() as tmp:
        name = f"{component}-release.json"
        gh("release", "download", tag, "--repo", PUBLISHER, "--pattern", name, "--dir", tmp)
        data = json.loads((Path(tmp) / name).read_text())
    validate_descriptor(data, tag)
    source = gh("api", f"repos/{PUBLISHER}/commits/{tag}", "--jq", ".sha")
    if source != data["source"]:
        raise ValueError("Component descriptor does not match its release tag")
    assets = {a["name"] for a in info["assets"]}
    if not set(data["files"]).issubset(assets):
        raise ValueError("Published component is missing assets")
    return {"tag": tag, "release_id": info["id"], "descriptor": data}


def validate_lock(lock, allow_candidate=False):
    if lock.get("schema") != 1 or lock.get("publisher") != PUBLISHER:
        raise ValueError("Unsupported publisher or dependency lock")
    candidate = lock.get("candidate")
    if "candidate" in lock:
        if (not allow_candidate or not isinstance(candidate, dict)
                or not re.fullmatch(r"[0-9a-f]{40}", candidate.get("source", ""))
                or not isinstance(candidate.get("run_id"), int) or candidate["run_id"] <= 0):
            raise ValueError("Candidate artifacts cannot be promoted as published dependencies")
    for key in ("appliance", "runtime"):
        item = lock[key]
        if key == "appliance" and not item["tag"].startswith("appliance-v"):
            raise ValueError("Appliance selection must be an appliance release")
        validate_descriptor(item["descriptor"], item["tag"])
        if candidate is not None and item["release_id"] == 0:
            if item["descriptor"]["source"] != candidate["source"]:
                raise ValueError("Mixed candidate sources")
        elif not isinstance(item["release_id"], int) or item["release_id"] <= 0:
            raise ValueError("Missing immutable release identity")


def fetch(lock, platform, arch, output, candidate_dir=None):
    validate_lock(lock, allow_candidate=candidate_dir is not None)
    if (candidate_dir is not None) != ("candidate" in lock):
        raise ValueError("Candidate and published dependency paths must not be mixed")
    if f"{platform}-{arch}" not in PLATFORMS:
        raise ValueError("Unsupported host platform")
    output.mkdir(parents=True, exist_ok=True)
    selected = [("appliance", f"appliance-{arch}.tar.gz")]
    if platform != "macos":
        selected.append(("runtime", f"runtime-{platform}-{arch}.tar.gz"))
    for component, name in selected:
        item = lock[component]
        target = output / name
        url = release_base(item["tag"]) + "/" + name
        # The publisher release descriptor is resolved once; all jobs consume
        # that same lock, and a changed asset fails its fixed hash.
        if candidate_dir is not None:
            with (candidate_dir / name).open("rb") as source, target.open("xb") as dest:
                shutil.copyfileobj(source, dest)
        else:
            with urllib.request.urlopen(url) as response, target.open("xb") as f:
                if not response.url.startswith("https://"):
                    raise ValueError("Insecure component redirect")
                while chunk := response.read(1 << 20):
                    f.write(chunk)
        if digest(target) != item["descriptor"]["files"][name]:
            raise ValueError("Component checksum mismatch: " + name)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    resolver = sub.add_parser("resolve")
    resolver.add_argument("--appliance-tag", required=True)
    resolver.add_argument("--runtime-tag")
    resolver.add_argument("--output", type=Path, required=True)
    downloader = sub.add_parser("fetch")
    downloader.add_argument("--lock", type=Path, required=True)
    downloader.add_argument("--platform", required=True)
    downloader.add_argument("--arch", required=True)
    downloader.add_argument("--output", type=Path, required=True)
    downloader.add_argument("--candidate-dir", type=Path, help="CI-only artifacts; forbidden in release promotion")
    args = parser.parse_args()
    if os.environ.get("GITHUB_REPOSITORY", PUBLISHER) != PUBLISHER:
        raise ValueError("Component releases must come from the approved publisher")
    if args.command == "resolve":
        appliance = resolve(args.appliance_tag)
        runtime = resolve(args.runtime_tag) if args.runtime_tag and args.runtime_tag != args.appliance_tag else appliance
        lock = {"schema": 1, "publisher": PUBLISHER, "appliance": appliance, "runtime": runtime}
        validate_lock(lock)
        args.output.write_text(json.dumps(lock, indent=2) + "\n")
    else:
        fetch(json.loads(args.lock.read_text()), args.platform, args.arch, args.output, args.candidate_dir)


if __name__ == "__main__":
    main()
