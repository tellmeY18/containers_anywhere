import json
from pathlib import Path
import subprocess
import unittest

from publication_acceptance import publication_acceptance


class ScaffoldingReady(Exception):
    pass


class PublicationHarnessTests(unittest.TestCase):
    def test_projected_identity_and_shell_syntax(self):
        resources = []

        def request(url, data, opener):
            resources.append(json.loads(data["yaml"]))

        def wait(*args):
            raise ScaffoldingReady()

        with self.assertRaises(ScaffoldingReady):
            publication_acceptance("http://kite", None, request, wait, None, Path("/ca"), b"marker", 10)
        deployments = [r for r in resources if r["kind"] == "Deployment"]
        self.assertEqual(len(deployments), 2)
        accounts = set()
        for deployment in deployments:
            pod = deployment["spec"]["template"]["spec"]
            accounts.add(pod["serviceAccountName"])
            script = pod["containers"][0]["command"][2]
            subprocess.run(["sh", "-n"], input=script.encode(), check=True)
            self.assertIn("--cacert", script)
            self.assertNotIn("--insecure", script)
            self.assertNotIn("hostNetwork", pod)
            self.assertTrue(all("hostPath" not in v for v in pod["volumes"]))
        self.assertEqual(accounts, {"publication-publisher", "publication-unprivileged"})
        bindings = [r for r in resources if r["kind"] == "RoleBinding"]
        self.assertTrue(all(s["name"] == "publication-publisher" for r in bindings for s in r["subjects"]))


if __name__ == "__main__":
    unittest.main()
