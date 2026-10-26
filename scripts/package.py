#!/usr/bin/env python3
"""Package a small launcher with build-pinned downloads; no VM in the installer."""
import argparse
import os
from pathlib import Path
import plistlib
import shutil
import subprocess
import tarfile
from urllib.parse import urlparse

from runtime import stage_runtime, download
from caddy import stage_caddy
from components import digest, semver

ROOT = Path(__file__).resolve().parent.parent


def run(*args, **kwargs):
    subprocess.run(args, check=True, **kwargs)


def checksum(path):
    path.with_name(path.name + ".sha256").write_text(f"{digest(path)}  {path.name}\n")


def pack(directory, destination):
    # Explicit regular files only: runtime symlinks are dereferenced at build time.
    with tarfile.open(destination, "w:gz", dereference=True) as archive:
        for path in sorted(directory.rglob("*")):
            if path.is_file():
                archive.add(path, arcname=path.relative_to(directory).as_posix(), recursive=False)
    checksum(destination)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("platform", choices=["macos", "linux", "windows"])
    parser.add_argument("--appliance", type=Path, required=True, help="exact appliance archive to pin")
    parser.add_argument("--arch", choices=["arm64", "amd64"], required=True)
    parser.add_argument("--base-url", required=True, help="immutable appliance release asset directory")
    parser.add_argument("--runtime", type=Path, help="reuse an already verified QEMU archive instead of building one")
    parser.add_argument("--runtime-base-url", help="independent immutable runtime release directory")
    parser.add_argument("--version", required=True)
    parser.add_argument("--output", type=Path, default=ROOT / "dist/releases")
    args = parser.parse_args()
    semver(args.version)
    for address in [args.base_url, args.runtime_base_url or args.base_url]:
        url = urlparse(address)
        if url.scheme != "https" or not url.netloc or url.username or url.query or url.fragment or any(c.isspace() for c in address):
            raise ValueError("Expected an HTTPS release directory without credentials, query or spaces")
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=True)
    stage = out / f"stage-{args.platform}-{args.arch}"
    stage.mkdir()
    flags = ["-s", "-w", "-X", f"main.appVersion={args.version}",
             "-X", f"main.applianceURL={args.base_url.rstrip('/')}/appliance-{args.arch}.tar.gz",
             "-X", f"main.applianceSHA256={digest(args.appliance)}"]
    if args.platform != "macos":
        runtime_archive = args.runtime
        if runtime_archive is None:
            runtime_dir = stage / "qemu"
            stage_runtime(runtime_dir, args.arch)
            runtime_archive = out / f"runtime-{args.platform}-{args.arch}.tar.gz"
            pack(runtime_dir, runtime_archive)
            shutil.rmtree(runtime_dir)
        if runtime_archive.name != f"runtime-{args.platform}-{args.arch}.tar.gz":
            raise ValueError("QEMU archive does not match the host platform")
        flags += ["-X", f"main.runtimeURL={(args.runtime_base_url or args.base_url).rstrip('/')}/{runtime_archive.name}",
                  "-X", f"main.runtimeSHA256={digest(runtime_archive)}"]
    if args.platform == "macos":
        if args.arch != "arm64":
            raise ValueError("macOS MVP supports Apple Silicon")
        app = stage / "Containers Anywhere.app"
        contents = app / "Contents"
        binary = contents / "MacOS/containers-anywhere"
        resources = contents / "Resources"
        binary.parent.mkdir(parents=True)
        resources.mkdir()
        (contents / "Info.plist").write_bytes(plistlib.dumps({
            "CFBundleName": "Containers Anywhere", "CFBundleDisplayName": "Containers Anywhere",
            "CFBundleIdentifier": "org.containers-anywhere.launcher", "CFBundleVersion": os.environ.get("GITHUB_RUN_NUMBER", "1"),
            "CFBundleShortVersionString": args.version.split("-")[0], "CFBundleExecutable": binary.name,
            "CFBundlePackageType": "APPL", "LSMinimumSystemVersion": "13.0", "LSUIElement": True,
            "NSHighResolutionCapable": True, "LSApplicationCategoryType": "public.app-category.developer-tools",
            "NSLocalNetworkUsageDescription": "Publish the appliance demo and approved workloads on your local network.",
            "NSBonjourServices": ["_http._tcp", "_https._tcp"],
        }))
        (stage / "Applications").symlink_to("/Applications")
    else:
        app = stage / "containers-anywhere"
        app.mkdir()
        resources = app
        binary = app / ("containers-anywhere.exe" if args.platform == "windows" else "containers-anywhere")
        if args.platform == "windows":
            flags += ["-H=windowsgui"]
    helper = stage_caddy(args.platform, args.arch, binary.parent, resources if args.platform == "macos" else None)
    if args.platform == "macos":
        run("codesign", "--force", "--sign", os.environ.get("MACOS_SIGN_IDENTITY", "-"), "--options", "runtime", str(helper))
    flags += ["-X", f"main.caddySHA256={digest(helper)}"]
    env = dict(os.environ, GOARCH=args.arch)
    if args.platform != "macos":
        env.update(GOOS=args.platform, CGO_ENABLED="0")
    run("go", "build", "-trimpath", "-ldflags=" + " ".join(flags), "-o", str(binary), ".", cwd=ROOT, env=env)
    for name in ["LICENSE", "THIRD_PARTY_NOTICES.md", "README.md", "flake.lock"]:
        shutil.copy2(ROOT / name, resources / name)
    prefix = f"Containers-Anywhere-{args.version}-{args.platform}-{args.arch}"
    if args.platform == "macos":
        identity = os.environ.get("MACOS_SIGN_IDENTITY", "-")
        run("codesign", "--force", "--sign", identity, "--options", "runtime", "--entitlements", str(ROOT / "entitlements.plist"), str(app))
        run("codesign", "--verify", "--deep", "--strict", str(app))
        artifact = out / (prefix + ".dmg")
        run("hdiutil", "create", "-volname", "Containers Anywhere", "-srcfolder", str(stage), "-format", "ULMO", str(artifact))
        run("hdiutil", "verify", str(artifact))
        if identity != "-" and os.environ.get("MACOS_NOTARY_PROFILE"):
            run("xcrun", "notarytool", "submit", str(artifact), "--keychain-profile", os.environ["MACOS_NOTARY_PROFILE"], "--wait")
            run("xcrun", "stapler", "staple", str(artifact))
    elif args.platform == "linux":
        (app / "AppRun").write_text('#!/bin/sh\nHERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)\nexec "$HERE/containers-anywhere" "$@"\n')
        (app / "AppRun").chmod(0o755)
        (app / "containers-anywhere.desktop").write_text('[Desktop Entry]\nType=Application\nName=Containers Anywhere\nExec=containers-anywhere\nIcon=containers-anywhere\nCategories=Development;\nTerminal=false\n')
        shutil.copy2(ROOT / "scripts/containers-anywhere.svg", app / "containers-anywhere.svg")
        tool = out / "appimagetool"
        arch = "aarch64" if args.arch == "arm64" else "x86_64"
        sha = "f0837e7448a0c1e4e650a93bb3e85802546e60654ef287576f46c71c126a9158" if args.arch == "arm64" else "ed4ce84f0d9caff66f50bcca6ff6f35aae54ce8135408b3fa33abfc3cb384eb0"
        download(f"https://github.com/AppImage/appimagetool/releases/download/1.9.1/appimagetool-{arch}.AppImage", tool, sha)
        tool.chmod(0o755)
        artifact = out / (prefix + ".AppImage")
        run(str(tool), "--appimage-extract-and-run", "--no-appstream", str(app), str(artifact), env=dict(os.environ, ARCH=arch))
    else:
        compiler = shutil.which("ISCC.exe") or r"C:\Program Files (x86)\Inno Setup 6\ISCC.exe"
        run(compiler, f"/DAppSource={app}", f"/DOutputDir={out}", f"/DAppVersion={args.version}", str(ROOT / "scripts/windows.iss"))
        artifact = out / (prefix + ".exe")
    checksum(artifact)
    print(artifact)


if __name__ == "__main__":
    main()
