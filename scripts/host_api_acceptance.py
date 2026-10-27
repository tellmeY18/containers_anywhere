"""Host API acceptance (extended): requests, approval, immutability and registry trust.

Runs against a disposable VM through Kite (administrator) and the launcher control
API. It exercises the same paths a workload manager would, without handing any test
a host credential. The default smoke test only proves the bridge is alive; this is
the wider suite and runs with `smoke.py --extended`.
"""
import json
import urllib.error

GROUP = "host.containersanywhere.dev"
API = f"{GROUP}/v1alpha1"


def host_api_acceptance(c, browser, request, wait, process, state, timeout):
    app = c["AppURL"]

    def apply(resource, namespace="default"):
        return request(app + "/api/v1/resources/apply",
                       data={"yaml": json.dumps(resource), "namespace": namespace}, opener=browser)

    def get(path):
        return json.loads(request(app + "/api/v1/" + path, opener=browser))

    def host_request(name, spec):
        return {"apiVersion": API, "kind": "HostRequest", "metadata": {"name": name, "namespace": "default"}, "spec": spec}

    def status_of(plural, namespace, name):
        try:
            return get(f"{plural}.{GROUP}/{namespace}/{name}").get("status", {})
        except (urllib.error.URLError, ValueError):
            return {}

    def control(path, post=False):
        return json.loads(request(c["URL"] + path, c["Token"], data={} if post else None) or b"{}")

    def uid_of(name):
        return get(f"hostrequests.{GROUP}/default/{name}")["metadata"]["uid"]

    # 1. The bridge publishes real facts and never claims more than it knows.
    def status_written():
        host = control("/control/host")
        agent = host.get("agent", {})
        st = agent.get("status") or {}
        return agent.get("written") and st.get("host", {}).get("applianceVersion") and st.get("guest", {}).get("disk", {}).get("sizeBytes", 0) > 0 and host
    host = wait(status_written, process, timeout)
    assert not host["agent"].get("crdsMissing"), "The host API CRDs were not installed"
    print("PASS: HostStatus is written by the guest bridge with host and guest facts", flush=True)

    # 2. A harmless request is approved automatically and states its limits.
    apply(host_request("lan-check-1", {"type": "lan-check"}))
    wait(lambda: status_of("hostrequests", "default", "lan-check-1").get("phase") == "Applied", process, timeout)
    result = status_of("hostrequests", "default", "lan-check-1").get("result", {})
    assert "does not prove" in json.dumps(result), "The LAN check must state that it is not proof for remote clients"
    print("PASS: lan-check request is applied and states its limits", flush=True)

    # 3. A resize waits for a person, is refused when it would oversubscribe, and
    #    takes effect only at the next start.
    apply(host_request("resize-ok", {"type": "resources", "resources": {"cpus": 2, "memoryMiB": 3072}}))
    wait(lambda: status_of("hostrequests", "default", "resize-ok").get("phase") == "Pending", process, timeout)
    # The launcher refreshes its list every few seconds, so wait for it to catch up.
    wait(lambda: any(p["name"] == "resize-ok" for p in control("/control/host")["pending"]), process, timeout)
    assert not (state / "resources.json").exists(), "Nothing may change before approval"
    request(c["URL"] + f"/control/host/requests/{uid_of('resize-ok')}/approve", c["Token"], data={})
    wait(lambda: status_of("hostrequests", "default", "resize-ok").get("phase") == "Applied", process, timeout)
    assert json.loads((state / "resources.json").read_text())["memoryMiB"] == 3072
    apply(host_request("resize-huge", {"type": "resources", "resources": {"cpus": 64, "memoryMiB": 262144}}))
    wait(lambda: status_of("hostrequests", "default", "resize-huge").get("phase") == "Pending", process, timeout)
    request(c["URL"] + f"/control/host/requests/{uid_of('resize-huge')}/approve", c["Token"], data={})
    wait(lambda: status_of("hostrequests", "default", "resize-huge").get("phase") == "Rejected", process, timeout)
    print("PASS: resize needs approval, oversubscription is refused, size applies at next start", flush=True)

    # 4. What was approved is what is applied: a request cannot be edited.
    try:
        apply(host_request("resize-ok", {"type": "resources", "resources": {"cpus": 8, "memoryMiB": 16384}}))
        raise AssertionError("A host request spec was changed after creation")
    except urllib.error.HTTPError as e:
        assert e.code in (400, 403, 409, 422, 500), e.code
    print("PASS: host request specs are immutable", flush=True)

    # 5. Registry trust: only approved loopback/node endpoints, and a real pull.
    def trust(name, endpoint):
        return {"apiVersion": API, "kind": "HostRegistryTrust", "metadata": {"name": name},
                "spec": {"endpoint": endpoint, "plainHTTP": True}}
    try:
        apply(trust("remote", "registry.example.com:5000"))
        raise AssertionError("A remote registry name was accepted")
    except urllib.error.HTTPError:
        pass
    apply(trust("public-ip", "8.8.8.8:5000"))
    wait(lambda: status_of("hostregistrytrusts", "_all", "public-ip").get("phase") == "Rejected", process, timeout)

    labels = {"app": "registry"}
    apply({"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "registry", "namespace": "default"},
           "spec": {"replicas": 1, "selector": {"matchLabels": labels}, "template": {"metadata": {"labels": labels},
                    "spec": {"containers": [{"name": "registry", "image": "registry:2", "ports": [{"containerPort": 5000}]}]}}}})
    apply({"apiVersion": "v1", "kind": "Service", "metadata": {"name": "registry", "namespace": "default"},
           "spec": {"type": "NodePort", "selector": labels, "ports": [{"port": 5000, "targetPort": 5000, "nodePort": 30500}]}})
    apply(trust("local-registry", "127.0.0.1:30500"))
    wait(lambda: status_of("hostregistrytrusts", "_all", "local-registry").get("phase") == "Trusted", process, timeout)
    apply({"apiVersion": "batch/v1", "kind": "Job", "metadata": {"name": "push-image", "namespace": "default"},
           "spec": {"backoffLimit": 20, "template": {"spec": {"restartPolicy": "OnFailure", "containers": [{
               "name": "crane", "image": "gcr.io/go-containerregistry/crane:debug",
               "command": ["crane", "--insecure", "copy", "docker.io/library/busybox:1.37.0", "registry.default.svc:5000/busybox:1.37.0"]}]}}}})
    wait(lambda: get("jobs/default/push-image").get("status", {}).get("succeeded") == 1, process, timeout)
    apply({"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "pull-trusted", "namespace": "default"},
           "spec": {"replicas": 1, "selector": {"matchLabels": {"app": "pull-trusted"}}, "template": {
               "metadata": {"labels": {"app": "pull-trusted"}}, "spec": {"containers": [{
                   "name": "web", "image": "127.0.0.1:30500/busybox:1.37.0", "imagePullPolicy": "Always",
                   "command": ["httpd", "-f", "-p", "8080"]}]}}}})
    wait(lambda: get("deployments/default/pull-trusted").get("status", {}).get("readyReplicas") == 1, process, timeout)
    print("PASS: an approved in-cluster registry is pulled from, unapproved and remote ones are not trusted", flush=True)
