import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest

from appliance_report import human_bytes, report


class ReportTest(unittest.TestCase):
    def test_human_bytes(self):
        for value, expected in [
            (0, "0 B"), (999, "999 B"), (1000, "1.0 kB"), (1_002_270_998, "1.00 GB"),
            (789_236_297, "789.2 MB"), (1_500_000_000, "1.50 GB"), (2_000_000_000_000, "2,000.00 GB"),
        ]:
            self.assertEqual(human_bytes(value), expected)

    def test_nix_json_formats_and_inclusion_paths(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name in ["kernel", "initrd", "system.img", "data.img", "appliance.tar.gz"]:
                (root / name).write_bytes(b"test")
            system = "/nix/store/aaaa-nixos-system-containers-anywhere-test"
            info = {
                system: {"narSize": 100, "references": ["/nix/store/bbbb-system-path"]},
                "/nix/store/bbbb-system-path": {"narSize": 40, "references": ["/nix/store/cccc-large"]},
                "/nix/store/cccc-large": {"narSize": 30, "references": []},
            }
            for data in [
                info,
                {"version": 2, "info": info},
                [{"path": path, **value} for path, value in info.items()],
            ]:
                closure = root / "closure.json"
                closure.write_text(json.dumps(data))
                output = io.StringIO()
                with contextlib.redirect_stdout(output):
                    report(root, root / "appliance.tar.gz", closure)
                text = output.getvalue()
                self.assertIn("170 B** across 3 store paths", text)
                self.assertIn("| system.img | 4 B |", text)
                self.assertIn("system-path -> large", text)
                self.assertIn("direct system reference", text)


if __name__ == "__main__":
    unittest.main()