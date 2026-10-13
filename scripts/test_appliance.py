import json
from pathlib import Path
import tarfile
import tempfile
import unittest

from appliance import pack_bundle
from package import digest


class ArchiveTest(unittest.TestCase):
    def test_bundle_allowlist_and_corruption(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            files = {}
            for name in ["kernel", "initrd", "system.img", "data.img"]:
                (root / name).write_bytes(name.encode())
                files[name] = digest(root / name)
            (root / "secret.key").write_text("must not ship")
            (root / "manifest.json").write_text(json.dumps({"product": "containers-anywhere", "format": 3, "files": files}))
            output = root / "appliance.tar.gz"
            pack_bundle(root, output)
            with tarfile.open(output) as archive:
                self.assertEqual(set(archive.getnames()), {"manifest.json", *files})
            (root / "kernel").write_text("tampered")
            with self.assertRaises(ValueError):
                pack_bundle(root, output)
