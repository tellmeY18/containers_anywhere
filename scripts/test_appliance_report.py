import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest

from appliance_report import report


class ReportTest(unittest.TestCase):
    def test_nix_json_formats(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name in ["kernel", "initrd", "system.img", "data.img", "appliance.tar.gz"]:
                (root / name).write_bytes(b"test")
            info = {"/nix/store/small": {"narSize": 10}, "/nix/store/large": {"narSize": 20}}
            for data in [info, {"version": 2, "info": info}, [dict(value, path=key) for key, value in info.items()]]:
                closure = root / "closure.json"
                closure.write_text(json.dumps(data))
                output = io.StringIO()
                with contextlib.redirect_stdout(output):
                    report(root, root / "appliance.tar.gz", closure)
                text = output.getvalue()
                self.assertIn("30 NAR bytes", text)
                self.assertLess(text.index("`large`"), text.index("`small`"))
                self.assertIn("| system.img | 4 |", text)
