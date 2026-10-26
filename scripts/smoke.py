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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--bundle", type=Path, required=True)
    parser.add_argument("--state", type=Path, required=True)
    parser.add_argument("--restore", action="store_true")
    parser.add_argument("--gateway", action="store_true", help="accept packaged Caddy and preloaded Traefik")
    parser.add_argument("--replacement-binary", type=Path, help="restart automatically with a different app recommendation")
    parser.add_argument("--runtime-archive", type=Path, help="exact QEMU archive pinned by the replacement app")
    args = parser.parse_args()
    binary, bundle, state = args.binary.resolve(), args.bundle.resolve(), args.state.resolve()
    state.mkdir()  # Never use existing user data.
    gateway_root = None
    (state / "gateway.json").write_text(json.dumps({"port": 18443, "httpPort": 18080, "namespaces": ["default"], "domains": ["default.localhost"]}))
    admin = {"username": "mvptest", "password": secrets.token_urlsafe(24), "enableAnalytics": False}
    marker = None
    timeout = int(os.environ.get("CONTAINERS_SMOKE_BOOT_SECONDS", "600"))

    def boot(directory, fresh=False, replacement=False):
        label = "fresh" if fresh else "restart"
        log = (directory / (label + "-smoke.log")).open("w")
        command = [str(args.replacement_binary.resolve() if replacement else binary), "serve", "--state", str(directory), "--port", "0", "--no-open"]
        if not replacement:
            command += ["--bundle", str(bundle)]
        process = subprocess.Popen(command, stdout=log, stderr=log,
                                   env=dict(os.environ, CONTAINERS_NO_BROWSER="1"))
        c = None
        try:
            c = wait_for(lambda: json.loads((directory / "control.json").read_text()), process, timeout)
            def ready():
                status = json.loads(request(c["URL"]+"/control/status", c["Token"]))
                if "protocol" in status:
                    assert status["protocol"] == 1
                    assert {"status", "logs", "stop", "kite-proxy"}.issubset(status["capabilities"])
                return status.get("healthy")
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
                if args.gateway:
                    ingress = {"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": {"name": "mvp-web", "namespace": "default"},
                               "spec": {"ingressClassName": "containers-anywhere", "rules": [{"host": "app.default.localhost", "http": {"paths": [{"path": "/", "pathType": "Prefix", "backend": {"service": {"name": "mvp-web", "port": {"number": 8080}}}}]}}]}}
                    request(c["AppURL"]+"/api/v1/resources/apply", data={"yaml": json.dumps(ingress), "namespace": "default"}, opener=browser)
            nonlocal marker
            observed = wait_for(lambda: request(c["AppURL"]+"/api/v1/namespaces/default/services/mvp-web:8080/proxy/", opener=browser), process, timeout)
            if fresh:
                marker = observed
            else:
                assert observed == marker, "PVC contents changed after reboot/restore"
            if args.gateway:
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
                demo_command = ["curl", "--silent", "--show-error", "--fail", "--noproxy", "*", "--max-time", "20",
                                "--resolve", "demo.local:18080:127.0.0.1", "http://demo.local:18080/"]
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
                nonlocal gateway_root
                if gateway_root is None:
                    gateway_root = root.read_bytes()
                else:
                    assert root.read_bytes() == gateway_root, "Caddy CA changed across restart/restore"
                for path in ["/_agent/status", "/_agent/stop", "/control/stop"]:
                    result = subprocess.run(base + ["--cacert", str(root), target.rstrip("/")+path], capture_output=True)
                    assert result.returncode != 0, "Workload gateway exposed appliance management"
                print("PASS: Caddy trusted HTTPS, Traefik Ingress, management isolation and persistent CA", flush=True)
            print("PASS: Kite auth, local cluster, workload HTTP and persistent volume", flush=True)
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
        with args.runtime_archive.open("rb") as f:
            runtime_sha = hashlib.file_digest(f, "sha256").hexdigest()
        runtime = state / "runtimes" / runtime_sha / "qemu"
        shutil.copytree(bundle.parent / "qemu", runtime, copy_function=os.link)
        (runtime / ".archive-sha256").write_text(runtime_sha)
        boot(state, replacement=True)
        assert (state / "release.json").read_bytes() == original, "App replacement changed appliance identity"
        print("PASS: replacement app retains exact appliance, Kite identity and PVC with no appliance download", flush=True)
    else:
        boot(state)
    if args.restore:
        archive = state.parent / (state.name+".age")
        restored = state.parent / (state.name+"-restored")
        subprocess.run([str(binary), "backup", "--state", str(state), "--file", str(archive)], check=True)
        subprocess.run([str(binary), "restore", "--state", str(restored), "--file", str(archive), "--key", str(archive)+".key"], check=True)
        boot(restored)
        print("PASS: encrypted restore preserves Kite account, cluster and PVC", flush=True)


if __name__ == "__main__":
    main()
