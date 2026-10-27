"""Workload-identity publication acceptance, driven through Kite on a disposable VM.

Only test scaffolding is installed as the administrator. All publication/route/
Service operations run inside a curl pod with its projected service-account
token and Kubernetes CA. No host kubeconfig, gateway edits or socket access.
"""
import copy
import json
import shlex
import subprocess
import urllib.error

GROUP = "networking.containersanywhere.dev"
PUBS = f"/apis/{GROUP}/v1alpha1/namespaces/default/workloadpublications"
INGRESSES = "/apis/networking.k8s.io/v1/namespaces/default/ingresses"
SERVICES = "/api/v1/namespaces/default/services"


def publication_acceptance(app_url, browser, request, wait, process, root, marker, timeout, quick=False):
    """quick: a workload service account creates a publication that serves traffic.

    The full suite (RBAC/admission denials, conflicts, rename, TLS modes, disable,
    delete and recreation) runs only with extended acceptance.
    """
    def apply(resource):
        request(app_url + "/api/v1/resources/apply",
                data={"yaml": json.dumps(resource), "namespace": "default"}, opener=browser)

    def api_step(method, path, body=None, codes=(200, 201)):
        data = "" if body is None else " --data " + shlex.quote(json.dumps(body))
        content_type = "application/merge-patch+json" if method == "PATCH" else "application/json"
        expected = "|".join(str(code) for code in codes)
        return (
            f'code=$(api -X {method} -H "Content-Type: {content_type}"'
            f'{data} "$API{path}" -o /tmp/response -w "%{{http_code}}") || fail transport\n'
            f'case "$code" in {expected}) ;; *) cat /tmp/response; fail "HTTP $code: {method} {path}";; esac\n'
        )

    service = {"apiVersion": "v1", "kind": "Service",
               "metadata": {"name": "published-web", "namespace": "default"},
               "spec": {"selector": {"app": "mvp-web"}, "ports": [{"port": 8080, "targetPort": 8080}]}}
    ingress = {"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
               "metadata": {"name": "mvp-web", "namespace": "default"},
               "spec": {"ingressClassName": "containers-anywhere", "rules": [
                   {"host": "app.default.localhost", "http": {"paths": [
                       {"path": "/", "pathType": "Prefix", "backend": {
                           "service": {"name": "published-web", "port": {"number": 8080}}}}]}}]}}
    publication = {"apiVersion": GROUP + "/v1alpha1", "kind": "WorkloadPublication",
                   "metadata": {"name": "mvp-web", "namespace": "default"},
                   "spec": {"ingressRef": "mvp-web", "hostname": "app.default.localhost",
                            "enabled": True, "lan": False, "mdns": False, "tls": {"mode": "LocalCA"}}}
    apex = copy.deepcopy(publication)
    apex["metadata"]["name"] = "mvp-apex"
    apex["spec"]["hostname"] = "default.localhost"
    foreign_apex = copy.deepcopy(apex)
    foreign_apex["spec"]["hostname"] = "other.localhost"
    conflict = copy.deepcopy(publication)
    conflict["metadata"]["name"] = "mvp-conflict"
    derived = copy.deepcopy(ingress)
    derived["metadata"]["name"] = "ca-pub-forged"
    derived["spec"]["ingressClassName"] = "containers-anywhere-published"
    renamed = copy.deepcopy(ingress["spec"])
    renamed["rules"][0]["host"] = "changed.default.local"
    stages = {
        1: api_step("POST", SERVICES, service) + api_step("POST", INGRESSES, ingress) + api_step("POST", PUBS, publication),
        2: api_step("PATCH", PUBS + "/mvp-web", {"spec": {"tls": {"mode": "ACME"}}}, (422,))
           + api_step("PATCH", PUBS + "/mvp-web", {"spec": {"hostname": "demo.local"}}, (403, 422))
           + api_step("POST", PUBS + "?dryRun=All", apex, (201,))
           + api_step("POST", PUBS + "?dryRun=All", foreign_apex, (403, 422))
           + api_step("PATCH", PUBS + "/mvp-web", {"spec": {"ingressRef": "other/web"}}, (422,))
           + api_step("POST", INGRESSES, derived, (403, 422))
           + api_step("PATCH", PUBS + "/mvp-web/status", {"status": {"url": "forged"}}, (403,))
           + api_step("GET", "/api/v1/namespaces/kite-system/secrets", codes=(403,))
           + api_step("POST", PUBS.replace("/default/", "/kite-system/"), publication, (403,)),
        3: api_step("POST", PUBS, conflict),
        4: api_step("DELETE", PUBS + "/mvp-conflict"),
        5: api_step("PATCH", INGRESSES + "/mvp-web", {"spec": renamed})
           + api_step("PATCH", PUBS + "/mvp-web", {"spec": {"hostname": "changed.default.local", "lan": True, "mdns": True}}),
        6: api_step("PATCH", PUBS + "/mvp-web", {"spec": {"tls": {"mode": "HTTP"}}}),
        7: api_step("PATCH", PUBS + "/mvp-web", {"spec": {"enabled": False}}),
        8: api_step("DELETE", PUBS + "/mvp-web"),
        9: api_step("PATCH", INGRESSES + "/mvp-web", {"spec": ingress["spec"]})
           + api_step("POST", PUBS, publication),
    }
    if quick:
        stages = {1: stages[1]}
    common = r'''
set -eu
API=https://kubernetes.default.svc
TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
CA=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt
api() { curl --silent --show-error --max-time 20 --cacert "$CA" -H "Authorization: Bearer $TOKEN" "$@"; }
fail() { printf '%s\n' "$*" > /results/error; exit 1; }
'''

    probes = {}

    def deploy_probe(name, account, script):
        deployment = {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": name, "namespace": "default"},
               "spec": {"replicas": 1, "selector": {"matchLabels": {"app": name}}, "template": {
                   "metadata": {"labels": {"app": name}}, "spec": {
                       "serviceAccountName": account,
                       "securityContext": {"runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000},
                       "containers": [
                           {"name": "client", "image": "curlimages/curl:8.12.1",
                            "command": ["sh", "-c", common + script],
                            "volumeMounts": [{"name": "results", "mountPath": "/results"}]},
                           {"name": "results", "image": "busybox:1.37.0",
                            "command": ["httpd", "-f", "-p", "8080", "-h", "/results"],
                            "volumeMounts": [{"name": "results", "mountPath": "/results"}]}],
                       "volumes": [{"name": "results", "emptyDir": {}}]}}}}
        probes[name] = deployment
        apply(deployment)
        apply({"apiVersion": "v1", "kind": "Service", "metadata": {"name": name, "namespace": "default"},
               "spec": {"selector": {"app": name}, "ports": [{"port": 8080, "targetPort": 8080}]}})

    for name in ["publication-publisher", "publication-unprivileged"]:
        apply({"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": name, "namespace": "default"}})
    apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
           "metadata": {"name": "publication-acceptance-publisher", "namespace": "default"},
           "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "containers-workload-publisher"},
           "subjects": [{"kind": "ServiceAccount", "name": "publication-publisher", "namespace": "default"}]})
    apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
           "metadata": {"name": "publication-acceptance-stage", "namespace": "default"},
           "rules": [{"apiGroups": [""], "resources": ["configmaps"], "resourceNames": ["publication-acceptance-stage"], "verbs": ["get"]}]})
    apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
           "metadata": {"name": "publication-acceptance-stage", "namespace": "default"},
           "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "publication-acceptance-stage"},
           "subjects": [{"kind": "ServiceAccount", "name": "publication-publisher", "namespace": "default"}]})

    def stage(number):
        apply({"apiVersion": "v1", "kind": "ConfigMap",
               "metadata": {"name": "publication-acceptance-stage", "namespace": "default"},
               "data": {"stage": str(number)}})

    stage(0)
    if not quick:
        denied = api_step("POST", PUBS, publication, (403,)) + api_step("POST", INGRESSES, ingress, (403,))
        deploy_probe("publication-denied", "publication-unprivileged",
                     denied + 'echo PASS > /results/done\nwhile :; do sleep 3600; done\n')
    script = "last=0\nwhile :; do\n"
    script += 'api "$API/api/v1/namespaces/default/configmaps/publication-acceptance-stage" > /tmp/stage || fail stage\n'
    for number, commands in stages.items():
        script += f'''if [ "$last" -lt {number} ] && grep -Eq '"stage"[[:space:]]*:[[:space:]]*"{number}"' /tmp/stage; then
{commands}last={number}
echo {number} > /results/done.tmp
mv /results/done.tmp /results/done
fi
'''
    script += f'api "$API{PUBS}/mvp-web" > /results/wp.tmp || fail observation\nmv /results/wp.tmp /results/wp.json\nsleep 1\ndone\n'
    deploy_probe("publication-allowed", "publication-publisher", script)

    def result(name, path):
        try:
            return request(app_url + f"/api/v1/namespaces/default/services/{name}:8080/proxy/{path}", opener=browser)
        except urllib.error.HTTPError as error:
            if error.code in (404, 502, 503):
                return b""
            raise

    def done(name, expected):
        error = result(name, "error")
        assert not error, error.decode()
        return result(name, "done").strip() == expected

    if not quick:
        wait(lambda: done("publication-denied", b"PASS"), process, timeout)

    def status(condition, expected="True", reason=None):
        data = result("publication-allowed", "wp.json")
        if not data:
            return False
        obj = json.loads(data)
        for entry in obj.get("status", {}).get("conditions", []):
            if entry["type"] == condition:
                return (entry["status"] == expected
                        and entry["observedGeneration"] == obj["metadata"]["generation"]
                        and (reason is None or entry["reason"] == reason))
        return False

    def fetch(host, tls=True):
        scheme, port = ("https", 18443) if tls else ("http", 18080)
        command = ["curl", "--silent", "--show-error", "--fail", "--noproxy", "*",
                   "--max-time", "5", "--resolve", f"{host}:{port}:127.0.0.1"]
        if tls:
            command += ["--cacert", str(root)]
        return subprocess.run(command + [f"{scheme}://{host}:{port}/"], capture_output=True)

    def serves(host, tls=True):
        response = fetch(host, tls)
        return response.returncode == 0 and response.stdout == marker

    for number in stages:
        stage(number)
        wait(lambda: done("publication-allowed", str(number).encode()), process, timeout)
        if number in (1, 2, 4, 9):
            wait(lambda: status("Ready") and status("CertificateReady"), process, timeout)
            wait(lambda: serves("app.default.localhost"), process, timeout)
        elif number == 3:
            wait(lambda: status("Accepted", "False", "HostnameConflict"), process, timeout)
            wait(lambda: fetch("app.default.localhost").returncode != 0, process, timeout)
        elif number in (5, 6):
            wait(lambda: status("Ready"), process, timeout)
            wait(lambda: serves("changed.default.local", tls=number == 5), process, timeout)
            wait(lambda: fetch("app.default.localhost").returncode != 0, process, timeout)
            # The CI host listener is loopback-only: do not claim LAN/mDNS acceptance.
            wait(lambda: status("Advertised", "False"), process, timeout)
            if number == 6:
                wait(lambda: status("CertificateReady", "False", "NotRequested"), process, timeout)
                wait(lambda: fetch("changed.default.local").returncode != 0, process, timeout)
        elif number == 7:
            wait(lambda: status("Accepted", "False", "Disabled"), process, timeout)
            wait(lambda: fetch("changed.default.local", tls=False).returncode != 0, process, timeout)
        elif number == 8:
            wait(lambda: json.loads(result("publication-allowed", "wp.json") or b"{}").get("code") == 404, process, timeout)
            wait(lambda: fetch("changed.default.local", tls=False).returncode != 0, process, timeout)
    # Stop the harness clients before the reboot/restore drill. Keep the final
    # workload publication, standard routes and service as persistent state.
    for name in probes:
        probes[name]["spec"]["replicas"] = 0
        apply(probes[name])
    if quick:
        print("PASS: workload SA publication create/status and routed traffic", flush=True)
    else:
        print("PASS: workload SA publication create/status, admission/RBAC denials, conflicts, rename, TLS, disable/delete and recreation", flush=True)
