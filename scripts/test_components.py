from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from components import APPLIANCES, RUNTIMES, PUBLISHER, digest, fetch, semver, validate_descriptor, validate_lock
from release import asset_names, build_component, verify_assets
import package


def descriptor():
    return {"schema": 1, "component": "appliance", "version": "0.1.0", "source": "a" * 40,
            "protocol": 1, "files": {n: "a" * 64 for n in APPLIANCES | RUNTIMES}}


class ComponentsTest(unittest.TestCase):
    def test_semver(self):
        for value in ("0.1.0", "0.1.1", "1.2.3-rc.1", "0.1.0-mvp.49"):
            self.assertEqual(semver(value), value)
        for value in ("01.1.0", "0.1", "latest", "0.1.0-01", "0.1.0+mutable", "../0.1.0"):
            with self.assertRaises(ValueError):
                semver(value)

    def test_descriptor_requires_exact_version_protocol_and_assets(self):
        validate_descriptor(descriptor(), "appliance-v0.1.0")
        for field, value in (("protocol", 2), ("version", "0.1.1"), ("files", {}),
                             ("source", "main"), ("component", "app")):
            bad = descriptor()
            bad[field] = value
            with self.assertRaises(ValueError):
                validate_descriptor(bad, "appliance-v0.1.0")

    def test_lock_pins_publisher_and_separate_runtime_identity(self):
        appliance = {"tag": "appliance-v0.1.0", "release_id": 42, "descriptor": descriptor()}
        runtime_descriptor = dict(descriptor(), component="runtime", version="0.2.0",
                                  files={n: "b" * 64 for n in RUNTIMES})
        runtime = {"tag": "runtime-v0.2.0", "release_id": 43, "descriptor": runtime_descriptor}
        lock = {"schema": 1, "publisher": PUBLISHER, "appliance": appliance, "runtime": runtime}
        validate_lock(lock)
        lock["publisher"] = "untrusted/repo"
        with self.assertRaises(ValueError):
            validate_lock(lock)

    def test_candidate_fetch_shares_hash_checks_but_cannot_be_promoted(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            artifacts = root / "candidates"
            artifacts.mkdir()
            data = descriptor()
            for name in APPLIANCES | RUNTIMES:
                (artifacts / name).write_bytes(name.encode())
                data["files"][name] = digest(artifacts / name)
            dependency = {"tag": "appliance-v0.1.0", "release_id": 0, "descriptor": data}
            lock = {"schema": 1, "publisher": PUBLISHER, "appliance": dependency, "runtime": dependency,
                    "candidate": {"source": "a" * 40, "run_id": 123}}
            with self.assertRaises(ValueError):
                validate_lock(lock)
            with patch("components.urllib.request.urlopen") as network:
                fetch(lock, "linux", "amd64", root / "fetched", artifacts)
                network.assert_not_called()
            self.assertEqual((root / "fetched/appliance-amd64.tar.gz").read_bytes(),
                             (artifacts / "appliance-amd64.tar.gz").read_bytes())
            (artifacts / "runtime-linux-amd64.tar.gz").write_bytes(b"tampered")
            with self.assertRaisesRegex(ValueError, "checksum mismatch"):
                fetch(lock, "linux", "amd64", root / "tampered", artifacts)

    def test_no_release_of_pr_legacy_or_unaccepted_run(self):
        run = {"conclusion": "success", "head_branch": "develop", "path": ".github/workflows/app.yml",
               "event": "workflow_dispatch", "run_attempt": 1, "repository": {"full_name": PUBLISHER}}
        self.assertEqual(build_component(run), "app")
        for field, value in (("conclusion", "failure"), ("event", "pull_request"),
                             ("head_branch", "care-mvp"), ("run_attempt", 2),
                             ("path", ".github/workflows/alpha.yml")):
            with self.assertRaises(ValueError):
                build_component(dict(run, **{field: value}))

    def test_independent_asset_sets(self):
        self.assertEqual(asset_names("appliance", "0.1.0"), APPLIANCES | RUNTIMES)
        apps = asset_names("app", "0.1.1")
        self.assertEqual(len(apps), 4)
        self.assertTrue(all(n.startswith("Containers-Anywhere-0.1.1-") for n in apps))
        self.assertFalse(apps & (APPLIANCES | RUNTIMES))

    def test_promotion_refuses_changed_bytes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "asset").write_bytes(b"changed after acceptance")
            (root / "asset.sha256").write_text("a" * 64 + "  asset\n")
            with self.assertRaises(ValueError):
                verify_assets(root, {"asset"})

    def test_app_packaging_reuses_runtime_without_staging_it(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            appliance = root / "appliance-amd64.tar.gz"
            runtime = root / "runtime-linux-amd64.tar.gz"
            helper = root / "caddy"
            for path in (appliance, runtime, helper):
                path.write_bytes(path.name.encode())
            args = ["package.py", "linux", "--arch", "amd64", "--version", "0.1.2",
                    "--appliance", str(appliance), "--runtime", str(runtime),
                    "--base-url", f"https://github.com/{PUBLISHER}/releases/download/appliance-v0.1.1",
                    "--runtime-base-url", f"https://github.com/{PUBLISHER}/releases/download/appliance-v0.1.0",
                    "--output", str(root / "out")]
            def download_tool(url, dest, sha):
                dest.write_bytes(b"test appimage tool")
            with patch("sys.argv", args), patch.object(package, "stage_runtime") as stage_runtime, \
                    patch.object(package, "stage_caddy", return_value=helper), \
                    patch.object(package, "download", side_effect=download_tool), \
                    patch.object(package, "checksum"), patch.object(package, "run") as run:
                package.main()
            stage_runtime.assert_not_called()
            command = run.call_args_list[0].args
            flags = next(arg for arg in command if arg.startswith("-ldflags="))
            self.assertIn("main.appVersion=0.1.2", flags)
            self.assertIn("/appliance-v0.1.1/appliance-amd64.tar.gz", flags)
            self.assertIn("/appliance-v0.1.0/runtime-linux-amd64.tar.gz", flags)
            self.assertIn("main.runtimeSHA256=" + package.digest(runtime), flags)


if __name__ == "__main__":
    unittest.main()
