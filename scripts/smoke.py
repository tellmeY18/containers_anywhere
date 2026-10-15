#!/usr/bin/env python3
"""Boot a disposable VM; exercise Kite, a real workload, restart and cold restore."""
import argparse
import http.cookiejar
import json
import os
from pathlib import Path
import secrets
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
    args = parser.parse_args()
    binary, bundle, state = args.binary.resolve(), args.bundle.resolve(), args.state.resolve()
    state.mkdir()  # Never use existing user data.
    admin = {"username": "mvptest", "password": secrets.token_urlsafe(24), "enableAnalytics": False}
    marker = None
    timeout = int(os.environ.get("CONTAINERS_SMOKE_BOOT_SECONDS", "600"))

    def boot(directory, fresh=False):
        label = "fresh" if fresh else "restart"
        log = (directory / (label + "-smoke.log")).open("w")
        process = subprocess.Popen([str(binary), "serve", "--state", str(directory), "--bundle", str(bundle), "--port", "0", "--no-open"], stdout=log, stderr=log,
                                   env=dict(os.environ, CONTAINERS_NO_BROWSER="1"))
        c = None
        try:
            c = wait_for(lambda: json.loads((directory / "control.json").read_text()), process, timeout)
            wait_for(lambda: json.loads(request(c["URL"]+"/control/status", c["Token"])).get("healthy"), process, timeout)
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
            nonlocal marker
            observed = wait_for(lambda: request(c["AppURL"]+"/api/v1/namespaces/default/services/mvp-web:8080/proxy/", opener=browser), process, timeout)
            if fresh:
                marker = observed
            else:
                assert observed == marker, "PVC contents changed after reboot/restore"
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
        assert process.returncode == 0

    boot(state, fresh=True)
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
