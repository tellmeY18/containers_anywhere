from pathlib import Path
import tempfile
import unittest

from runtime import stage_linux_firmware


class FirmwareTest(unittest.TestCase):
    def fixture(self, root, ipxe=True):
        files = {"usr/share/qemu/qboot.rom": b"qboot",
                 "usr/share/seabios/bios-256k.bin": b"bios",
                 "usr/share/doc/seabios/copyright": b"seabios license"}
        if ipxe:
            files.update({"usr/lib/ipxe/qemu/efi-virtio.rom": b"virtio firmware",
                          "usr/share/doc/ipxe-qemu/copyright": b"ipxe license"})
        for name, data in files.items():
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)

    def test_standalone_runtime_includes_ipxe_and_licenses(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self.fixture(root / "source")
            stage_linux_firmware(root / "runtime", "amd64", root / "source")
            self.assertEqual((root / "runtime/share/qemu/efi-virtio.rom").read_bytes(), b"virtio firmware")
            self.assertEqual((root / "runtime/licenses/ipxe-qemu.copyright").read_bytes(), b"ipxe license")
            self.assertTrue((root / "runtime/share/qemu/bios-256k.bin").is_file())

    def test_missing_virtio_firmware_fails_packaging(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self.fixture(root / "source", ipxe=False)
            with self.assertRaisesRegex(ValueError, "efi-virtio.rom"):
                stage_linux_firmware(root / "runtime", "amd64", root / "source")


if __name__ == "__main__":
    unittest.main()
