from pathlib import Path
import unittest

from smoke import launch_command


class SmokeLaunchTest(unittest.TestCase):
    def test_public_bootstrap_does_not_bypass_launcher_downloads(self):
        command = launch_command(Path("/app"), Path("/fresh-state"))
        self.assertEqual(command, ["/app", "serve", "--state", "/fresh-state", "--port", "0", "--no-open"])
        self.assertNotIn("--bundle", command)

    def test_candidate_and_restore_use_explicit_verified_bundle(self):
        command = launch_command(Path("/app"), Path("/state"), Path("/verified-bundle"))
        self.assertEqual(command[-2:], ["--bundle", "/verified-bundle"])


if __name__ == "__main__":
    unittest.main()
