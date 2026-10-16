"""Stage the upstream Caddy binary; all archive hashes are release pins."""
import shutil
import tarfile
import zipfile

from runtime import download

VERSION = "2.11.7"
PINS = {
    ("linux", "amd64"): "727b91701a392de6ebc5027509f548bf39979e5216340d0faed8fa5e69c84f8b",
    ("linux", "arm64"): "d8fc6d179a5d283028a472a5618564f6ad8a86fed513e64f032b3b0b7cc45e42",
    ("macos", "arm64"): "cda3030e5d5b13eb9f0b6fb541037f633bddd958c070c5f561d7a5cccb581665",
    ("windows", "amd64"): "0a1edc0b799512051c57071ce0e798d3f2cf67dc98171366f1d2b326072e3b06",
}


def stage_caddy(platform, arch, directory, licenses=None):
    # macOS rejects unsigned non-code files placed in Contents/MacOS, so the
    # license goes to Resources while the helper stays adjacent to the launcher.
    licenses = directory if licenses is None else licenses
    os_name = "mac" if platform == "macos" else platform
    extension = "zip" if platform == "windows" else "tar.gz"
    name = "caddy.exe" if platform == "windows" else "caddy"
    archive = directory / f"caddy-{VERSION}.{extension}"
    download(f"https://github.com/caddyserver/caddy/releases/download/v{VERSION}/caddy_{VERSION}_{os_name}_{arch}.{extension}",
             archive, PINS[(platform, arch)])
    binary = directory / name
    # Extract only explicit members; no archive-selected destination paths.
    if extension == "zip":
        with zipfile.ZipFile(archive) as source:
            with source.open(name) as inp, binary.open("xb") as out:
                shutil.copyfileobj(inp, out)
            with source.open("LICENSE") as inp, (licenses / "CADDY_LICENSE.txt").open("xb") as out:
                shutil.copyfileobj(inp, out)
    else:
        with tarfile.open(archive) as source:
            member = source.getmember(name)
            if not member.isfile():
                raise ValueError("Caddy binary must be a regular archive member")
            with source.extractfile(member) as inp, binary.open("xb") as out:
                shutil.copyfileobj(inp, out)
            license_member = source.getmember("LICENSE")
            if not license_member.isfile():
                raise ValueError("Caddy license must be a regular archive member")
            with source.extractfile(license_member) as inp, (licenses / "CADDY_LICENSE.txt").open("xb") as out:
                shutil.copyfileobj(inp, out)
    archive.unlink()
    binary.chmod(0o755)
    return binary
