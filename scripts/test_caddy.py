import ast
import io
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile

from caddy import stage_caddy


class CaddyPackagingTests(unittest.TestCase):
    def test_extracts_only_binary_and_license(self):
        for platform, arch in [("windows", "amd64"), ("linux", "arm64"), ("macos", "arm64")]:
            with self.subTest(platform=platform), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)

                def download(url, dest, digest):
                    self.assertEqual(len(digest), 64)
                    members = {"caddy.exe" if platform == "windows" else "caddy": b"binary", "LICENSE": b"license", "../escape": b"bad"}
                    if platform == "windows":
                        with zipfile.ZipFile(dest, "w") as archive:
                            for name, data in members.items():
                                archive.writestr(name, data)
                    else:
                        with tarfile.open(dest, "w:gz") as archive:
                            for name, data in members.items():
                                entry = tarfile.TarInfo(name)
                                entry.size = len(data)
                                archive.addfile(entry, io.BytesIO(data))

                with patch("caddy.download", download):
                    binary = stage_caddy(platform, arch, root)
                self.assertEqual(binary.read_bytes(), b"binary")
                self.assertEqual((root / "CADDY_LICENSE.txt").read_bytes(), b"license")
                self.assertEqual(len(list(root.iterdir())), 2)

    def test_license_can_be_staged_outside_the_executable_directory(self):
        # macOS strict signing rejects stray non-code files in Contents/MacOS.
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            macos, resources = root / "MacOS", root / "Resources"
            macos.mkdir()
            resources.mkdir()

            def download(url, dest, digest):
                with tarfile.open(dest, "w:gz") as archive:
                    for name, data in {"caddy": b"binary", "LICENSE": b"license"}.items():
                        entry = tarfile.TarInfo(name)
                        entry.size = len(data)
                        archive.addfile(entry, io.BytesIO(data))

            with patch("caddy.download", download):
                binary = stage_caddy("macos", "arm64", macos, resources)
            self.assertEqual((macos / "caddy").read_bytes(), b"binary")
            self.assertEqual((resources / "CADDY_LICENSE.txt").read_bytes(), b"license")
            self.assertEqual(sorted(p.name for p in macos.iterdir()), ["caddy"])

    def test_smoke_script_parses(self):
        ast.parse(Path(__file__).with_name("smoke.py").read_text())
