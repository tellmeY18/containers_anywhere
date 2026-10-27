#!/usr/bin/env python3
"""Boot a disposable VM; exercise Kite, a real workload, restart and cold restore."""
import argparse
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import time
import urllib.error
import urllib.request

from publication_acceptance import publication_acceptance


def request(url, token=None, data=None, opener=None):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, headers=headers, data=None if data is None else json.dumps(data).encode())
    with (opener or urllib.request.build_opener()).open(req, timeout=30) as r:
        return r.read()


def wait_for(fn, process, seconds=600):
    deadline = time.monotonic() + seconds
    error = None
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"Launcher exited: {process.returncode}")
        try:
            value = fn()
            if value:
                return value
        except (OSError, ValueError) as e:
            error = e
        time.sleep(2)
    raise TimeoutError(f"Appliance did not become ready: {error}")


def launch_command(binary, state, bundle=None):
    command = [str(binary), "serve", "--state", str(state), "--port", "0", "--no-open"]
    if bundle is not None:
        command += ["--bundle", str(bundle)]
    return command


def cache_runtime(state, bundle, archive):
    with archive.open("rb") as f:
        digest = hashlib.file_digest(f, "sha256").hexdigest()
    target = state / "runtimes" / digest / "qemu"
    if not target.exists():
        shutil.copytree(bundle.parent / "qemu", target, copy_function=os.link)
        (target / ".archive-sha256").write_text(digest)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--bundle", type=Path, required=True)
    parser.add_argument("--state", type=Path, required=True)
    parser.add_argument("--restore", action="store_true")
    parser.add_argument("--gateway", action="store_true", help="accept packaged Caddy and preloaded Traefik")
    parser.add_argument("--replacement-binary", type=Path, help="restart automatically with a different app recommendation")
    parser.add_argument("--runtime-archive", type=Path, help="exact QEMU archive pinned by the replacement app")
    parser.add_argument("--download", action="store_true", help="fresh boot through the packaged launcher's actual public download pins")
    parser.add_argument("--update-bundle", type=Path, help="distinct current data-contract appliance for explicit KVM update/recovery drill")
    args = parser.parse_args()
    binary, bundle, state = args.binary.resolve(), args.bundle.resolve(), args.state.resolve()
    state.mkdir()  # Never use existing user data.
    gateway_root = None
    gateway_config = json.dumps({"port": 18443, "httpPort": 18080, "addresses": ["127.0.0.1"]}).encode()
    (state / "gateway.json").write_bytes(gateway_config)
    admin = {"username": "mvptest", "password": secrets.token_urlsafe(24), "enableAnalytics": False}
    marker = None
    publication_identity = None
    timeout = int(os.environ.get("CONTAINERS_SMOKE_BOOT_SECONDS", "600"))

    def boot(directory, fresh=False, replacement=False, update=False, automatic=False, recovery_bundle=None,
             commit=True, write_update_marker=False):
        label = "fresh" if fresh else "restart"
        log = (directory / (label + "-smoke.log")).open("w")
        selected_binary = args.replacement_binary.resolve() if replacement else binary
        if recovery_bundle is not None:
            selected_binary = recovery_bundle.parent / ("launcher.exe" if os.name == "nt" else "launcher")
        selected_bundle = None if automatic or replacement or (fresh and args.download) else (recovery_bundle or bundle)
        command = launch_command(selected_binary, directory, selected_bundle)
        if update:
            command = [str(binary), "update", "--operation", "activate", "--accept-external-effects",
                       "--state", str(directory), "--port", "0", "--no-open"]
        process = subprocess.Popen(command, stdout=log, stderr=log,
                                   env=dict(os.environ, CONTAINERS_NO_BROWSER="1"))
        c = None
        try:
            c = wait_for(lambda: json.loads((directory / "control.json").read_text()), process, timeout)
            if fresh and args.download:
                selected = json.loads((directory / "release.json").read_text())
                expected = json.loads((bundle / "manifest.json").read_text())
                for key, value in selected.items():
                    assert expected.get(key) == value, f"Public bootstrap selected the wrong appliance: {key}"
                assert (directory / "appliance.json").is_file(), "Public bootstrap did not persist its download receipt"
            def ready():
                status = json.loads(request(c["URL"]+"/control/status", c["Token"]))
                assert status["protocol"] == 1
                assert {"status", "logs", "stop", "console-proxy", "platform-apply-v1", "workload-publication-v1"}.issubset(status["capabilities"])
                return status.get("healthy") and status.get("console")
            wait_for(ready, process, timeout)
            try:
                request(c["AppURL"]+"/api/v1/bootstrap")
                raise AssertionError("Kite bootstrap exposed without host capability")
            except urllib.error.HTTPError as e:
                assert e.code == 401
            jar = http.cookiejar.CookieJar()
            browser = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
            request(c["AppURL"]+"/_open?token="+c["AppToken"], opener=browser)
            endpoint = "/api/auth/setup/create_super_user" if fresh else "/api/auth/login/password"
            request(c["AppURL"]+endpoint, data=admin, opener=browser)
            assert json.loads(request(c["AppURL"]+"/api/v1/bootstrap", opener=browser))["setup"]["initialized"]
            assert b"local" in request(c["AppURL"]+"/api/v1/clusters", opener=browser)
            if fresh:
                for namespace in ["cncm-system", "care"]:
                    try:
                        request(c["AppURL"]+"/api/v1/namespaces/_all/"+namespace, opener=browser)
                        raise AssertionError(f"Fresh universal appliance preinstalled {namespace}")
                    except urllib.error.HTTPError as e:
                        assert e.code == 404, f"Cannot inspect fresh namespaces: {e}"
                # This is a real additional workload, with a PVC whose contents
                # must survive both reboot and an encrypted backup round trip.
                resources = [
                    {"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": {"name": "mvp-data", "namespace": "default"},
                     "spec": {"accessModes": ["ReadWriteOnce"], "resources": {"requests": {"storage": "64Mi"}}}},
                    # Graceful node shutdown terminates Pods. A controller must
                    # recreate them after reboot; a bare Pod is not a service.
                    {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "mvp-web", "namespace": "default"},
                     "spec": {"replicas": 1, "strategy": {"type": "Recreate"}, "selector": {"matchLabels": {"app": "mvp-web"}},
                              "template": {"metadata": {"labels": {"app": "mvp-web"}},
                                           "spec": {"containers": [{"name": "web", "image": "busybox:1.37.0", "command": ["sh", "-c", "test -f /www/index.html || cat /proc/sys/kernel/random/uuid > /www/index.html; exec httpd -f -p 8080 -h /www"],
                                                                    "ports": [{"containerPort": 8080}], "volumeMounts": [{"name": "data", "mountPath": "/www"}]}],
                                                    "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "mvp-data"}}]}}}},
                    {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "mvp-web", "namespace": "default"},
                     "spec": {"selector": {"app": "mvp-web"}, "ports": [{"port": 8080, "targetPort": 8080}]}},
                ]
                for resource in resources:
                    request(c["AppURL"]+"/api/v1/resources/apply", data={"yaml": json.dumps(resource), "namespace": "default"}, opener=browser)
            assert (directory / "gateway.json").read_bytes() == gateway_config, "Listener settings were overwritten"
            nonlocal marker
            observed = wait_for(lambda: request(c["AppURL"]+"/api/v1/namespaces/default/services/mvp-web:8080/proxy/", opener=browser), process, timeout)
            if fresh:
                marker = observed
            else:
                assert observed == marker, "PVC contents changed after reboot/restore"
            if args.gateway:
                if fresh:
                    publication_acceptance(c["AppURL"], browser, request, wait_for, process,
                                           directory / "gateway/caddy/pki/authorities/local/root.crt",
                                           marker, timeout)
                obj = wait_for(lambda: json.loads(request(
                    c["AppURL"] + "/api/v1/workloadpublications.networking.containersanywhere.dev/default/mvp-web",
                    opener=browser)), process, timeout)
                observed_identity = (obj["metadata"]["uid"], obj["metadata"]["generation"], obj["spec"])
                nonlocal publication_identity
                if fresh:
                    publication_identity = observed_identity
                else:
                    assert observed_identity == publication_identity, "WorkloadPublication identity/spec changed across update/retry/recovery"
                wait_for(lambda: "https://app.default.localhost:18443" in json.loads(request(c["URL"]+"/control/gateway", c["Token"])).get("urls", []), process, timeout)
                # curl uses --resolve so .localhost DNS support is not assumed.
                # First contact triggers certificate issuance, without trusting it.
                target = "https://app.default.localhost:18443/"
                base = ["curl", "--silent", "--show-error", "--fail", "--noproxy", "*", "--max-time", "20", "--resolve", "app.default.localhost:18443:127.0.0.1"]
                root = directory / "gateway/caddy/pki/authorities/local/root.crt"

                def gateway_ready():
                    # Fetching the root from local protected storage is the trust
                    # bootstrap; no OS trust store is changed by the smoke test.
                    if not root.exists():
                        subprocess.run(base + [target], capture_output=True)
                        return False
                    result = subprocess.run(base + ["--cacert", str(root), target], capture_output=True)
                    return result.returncode == 0 and result.stdout == marker

                wait_for(gateway_ready, process, timeout)
                def publication_ready():
                    status = json.loads(request(c["URL"]+"/control/gateway", c["Token"]))
                    return any(e["url"] == target and e["ready"] and not e["advertised"] for e in status.get("endpoints", []))
                wait_for(publication_ready, process, timeout)
                # The shipped demo is LocalCA: HTTPS verified against the persisted
                # appliance root, and plain HTTP redirects to HTTPS.
                demo_base = ["curl", "--silent", "--show-error", "--noproxy", "*", "--max-time", "20",
                             "--resolve", "demo.local:18443:127.0.0.1", "--resolve", "demo.local:18080:127.0.0.1"]
                demo_command = demo_base + ["--fail", "--cacert", str(root), "https://demo.local:18443/"]
                redirect_command = demo_base + ["--output", os.devnull, "--write-out", "%{http_code} %{redirect_url}", "http://demo.local:18080/"]
                last = []

                def demo_ready():
                    # After a restore every pod restarts, so the demo may lag the workload.
                    last[:] = [subprocess.run(demo_command, capture_output=True)]
                    return last[0].returncode == 0 and b"Containers Anywhere works." in last[0].stdout

                try:
                    wait_for(demo_ready, process, 300)
                except TimeoutError:
                    demo = last[0]
                    raise AssertionError(f"Preloaded demo did not traverse Caddy/Traefik: rc={demo.returncode} stdout={demo.stdout[:200]!r} stderr={demo.stderr[:200]!r}")
                redirect = subprocess.run(redirect_command, capture_output=True)
                assert redirect.returncode == 0 and redirect.stdout == b"308 https://demo.local:18443/", f"Demo HTTP did not redirect to HTTPS: {redirect.stdout!r} {redirect.stderr[:200]!r}"

                def demo_status_ready():
                    status = json.loads(request(c["URL"]+"/control/gateway", c["Token"]))
                    demo = [e for e in status.get("endpoints", []) if e["url"] == "https://demo.local:18443/"]
                    return demo and demo[0]["ready"] and demo[0]["certificateReady"]
                wait_for(demo_status_ready, process, timeout)
                nonlocal gateway_root
                if gateway_root is None:
                    gateway_root = root.read_bytes()
                else:
                    assert root.read_bytes() == gateway_root, "Caddy CA changed across restart/restore"
                for path in ["/_agent/status", "/_agent/stop", "/control/stop"]:
                    result = subprocess.run(base + ["--cacert", str(root), target.rstrip("/")+path], capture_output=True)
                    assert result.returncode != 0, "Workload gateway exposed appliance management"
                print("PASS: Caddy trusted HTTPS, Traefik Ingress, management isolation and persistent CA", flush=True)
            print("PASS: Kite auth, current protocol, generic workload/PVC and persistent listener settings", flush=True)
            if fresh and args.download:
                print("PASS: actual public pinned appliance/runtime bootstrap into fresh state", flush=True)
            if write_update_marker:
                marker = (secrets.token_hex(24) + "\n").encode()
                job = {"apiVersion": "batch/v1", "kind": "Job",
                       "metadata": {"name": "update-write", "namespace": "default"},
                       "spec": {"template": {"spec": {"restartPolicy": "Never", "containers": [
                           {"name": "write", "image": "busybox:1.37.0",
                            "command": ["sh", "-c", "printf '%s\\n' '" + marker.decode().strip() + "' > /data/index.html"],
                            "volumeMounts": [{"name": "data", "mountPath": "/data"}]}],
                           "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "mvp-data"}}]}}}}
                request(c["AppURL"]+"/api/v1/resources/apply", data={"yaml": json.dumps(job), "namespace": "default"}, opener=browser)
                wait_for(lambda: request(c["AppURL"]+"/api/v1/namespaces/default/services/mvp-web:8080/proxy/",
                                         opener=browser) == marker, process, timeout)
            if update and commit:
                # Login above verifies the original administrator; the HTTP
                # marker verifies real PVC data, not only launcher/K3s health.
                subprocess.run([str(binary), "update", "--operation", "commit",
                                "--accept-workload-validation", "--state", str(directory)], check=True)
                journal = json.loads((directory / "update.json").read_text())
                assert journal["phase"] == "committed"
                assert journal["validated_at"]
        finally:
            if c and process.poll() is None:
                try:
                    journal = request(c["URL"]+"/control/logs", c["Token"])
                    (directory / (label + "-guest.log")).write_bytes(journal)
                    request(c["URL"]+"/control/stop", c["Token"], {})
                    process.wait(timeout=180)
                except Exception as e:
                    print("Graceful shutdown failed:", e, flush=True)
                    process.terminate()
                    process.wait(timeout=180)
            console = directory / "console.log"
            if console.exists():
                (directory / (label + "-console.log")).write_bytes(console.read_bytes())
            log.close()
            if process.poll() not in (None, 0):
                for path in (directory / (label + "-smoke.log"), console):
                    if path.exists():
                        print(f"Launcher failure ({path.name}):\n{path.read_bytes()[-8192:].decode(errors='replace')}", flush=True)
        assert process.returncode == 0

    boot(state, fresh=True)
    if args.replacement_binary:
        if not args.runtime_archive:
            raise ValueError("Replacement acceptance needs its exact runtime archive")
        original = (state / "release.json").read_bytes()
        shutil.copytree(bundle, state / "appliances/acceptance/bundle", copy_function=os.link)
        if not args.download:
            cache_runtime(state, bundle, args.runtime_archive)
        boot(state, replacement=True)
        assert (state / "release.json").read_bytes() == original, "App replacement changed appliance identity"
        print("PASS: replacement app retains exact appliance, Kite identity and PVC with no appliance download", flush=True)
    else:
        boot(state)
    if args.update_bundle:
        if not args.runtime_archive:
            raise ValueError("Update restart acceptance needs the app's exact pinned runtime archive")
        cache_runtime(state, bundle, args.runtime_archive)
        candidate = args.update_bundle.resolve()
        original_identity = json.loads((state / "release.json").read_text())
        candidate_identity = json.loads((candidate / "manifest.json").read_text())
        assert candidate_identity["system"] != original_identity["system"], "Update drill needs a distinct built root image"
        subprocess.run([str(binary), "update", "--operation", "prepare", "--state", str(state),
                        "--bundle", str(candidate), "--original-bundle", str(bundle)], check=True)
        assert json.loads((state / "release.json").read_text()) == original_identity
        cold_marker = marker
        boot(state, update=True, commit=False, write_update_marker=True)
        assert json.loads((state / "update.json").read_text())["phase"] == "candidate-running"
        boot(state, update=True)  # Must retain the new marker, never rewind.
        assert json.loads((state / "release.json").read_text())["system"] == candidate_identity["system"]
        boot(state, automatic=True)
        recovered = state.parent / (state.name + "-recovered")
        subprocess.run([str(binary), "update", "--operation", "recover", "--state", str(state),
                        "--destination", str(recovered), "--accept-data-loss"], check=True)
        # These are disposable acceptance workloads with no external services.
        # Never automatically do this for arbitrary real workloads.
        marker = cold_marker  # Deliberate cold recovery excludes candidate writes.
        boot(recovered, recovery_bundle=recovered / "recovery/bundle")
        assert json.loads((recovered / "release.json").read_text()) == original_identity
        assert (state / "data.img").exists(), "Recovery removed current candidate data"
        print("PASS: distinct current-contract update/retry preserves candidate PVC writes, WorkloadPublication UID/spec/routing, login and CA; explicit cold recovery preserves originals", flush=True)
    if args.restore:
        archive = state.parent / (state.name+".age")
        restored = state.parent / (state.name+"-restored")
        subprocess.run([str(binary), "backup", "--state", str(state), "--file", str(archive)], check=True)
        subprocess.run([str(binary), "restore", "--state", str(restored), "--file", str(archive), "--key", str(archive)+".key"], check=True)
        boot(restored)
        print("PASS: encrypted restore preserves Kite account, cluster and PVC", flush=True)


if __name__ == "__main__":
    main()
