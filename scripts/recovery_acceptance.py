"""Opt-in recovery acceptance against a real, disposable appliance.

The smoke runner owns the VM and Kite login. Call ``recovery_acceptance`` only
with --recovery, retain its return value, and call ``verify_restart`` after a
fresh login on subsequent boots. Cold operations let the persistent launcher
restart the guest automatically. No host credential is sent to a workload.

Markers are written by one-shot Jobs. Readers never create or repair data: an
empty, incorrectly restored, or reformatted volume therefore fails the drill.
Only successful runtime observations produce PASS messages or evidence entries.
"""
import hashlib
import json
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
from urllib.parse import quote


GROUP = "recovery.containersanywhere.dev"
API = GROUP + "/v1alpha1"
# The same multiarchitecture digest used by the shipped diagnostic demo.
IMAGE = "docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
SNAPSHOTS = "volumesnapshots.snapshot.storage.k8s.io"
REQUIRED_CASES = {"csi", "workload-crd", "workload-host-export", "workload-host-import",
                  "restart", "cold-system", "cold-full", "host-export", "host-verify"}


def require(value, message):
    # Acceptance must not disappear when Python is invoked with -O.
    if not value:
        raise AssertionError(message)


class RecoveryAcceptance:
    """Live Kite client and marker evidence, reusable after a launcher restart."""

    def __init__(self, c, browser, request, wait, process, state, timeout):
        self.request = request
        self.wait = wait
        self.state = state
        self.timeout = timeout
        self.namespace = "recovery-acceptance-" + secrets.token_hex(4)
        self.markers = {}
        self.pvc_uids = {}
        self.cold_points = {}
        self.evidence = {"version": 1, "namespace": self.namespace, "checks": []}
        self.connect(c, browser, process)

    def connect(self, c, browser, process):
        """Replace per-boot capabilities and the authenticated Kite browser."""
        self.c, self.browser, self.process = c, browser, process

    def apply(self, resource):
        namespace = resource.get("metadata", {}).get("namespace", "default")
        return self.request(self.c["AppURL"] + "/api/v1/resources/apply",
                            data={"yaml": json.dumps(resource), "namespace": namespace},
                            opener=self.browser)

    def get(self, plural, name, namespace=None):
        scope = self.namespace if namespace is None else namespace
        return json.loads(self.request(
            self.c["AppURL"] + f"/api/v1/{plural}/{scope}/{name}", opener=self.browser))

    def until(self, probe):
        return self.wait(probe, self.process, self.timeout)

    def record(self, case, check, **facts):
        self.evidence["checks"].append({"case": case, "check": check, "result": "passed", **facts})
        path = self.state / "recovery-evidence.json"
        temporary = path.with_suffix(".json.tmp")
        temporary.write_text(json.dumps(self.evidence, indent=2) + "\n")
        temporary.replace(path)
        print("PASS: " + check, flush=True)

    def resource(self, api, kind, name, spec):
        return {"apiVersion": api, "kind": kind,
                "metadata": {"name": name, "namespace": self.namespace}, "spec": spec}

    def prepare_namespace(self):
        self.apply({"apiVersion": "v1", "kind": "Namespace",
                    "metadata": {"name": self.namespace,
                                 "labels": {"containersanywhere.dev/acceptance": "recovery"}}})
        # All workload containers in this namespace are inert marker readers or
        # writers. Deny their outbound traffic as a second isolation boundary.
        # Image pulls are node operations and remain possible on first boot.
        self.apply(self.resource("networking.k8s.io/v1", "NetworkPolicy", "deny-egress",
                                 {"podSelector": {}, "policyTypes": ["Egress"], "egress": []}))

    def pvc(self, name, snapshot=None):
        spec = {"storageClassName": "btrfs", "accessModes": ["ReadWriteOnce"],
                "resources": {"requests": {"storage": "64Mi"}}}
        if snapshot:
            spec["dataSource"] = {"apiGroup": "snapshot.storage.k8s.io",
                                  "kind": "VolumeSnapshot", "name": snapshot}
        resource = self.resource("v1", "PersistentVolumeClaim", name, spec)
        if name == "source":
            resource["metadata"]["labels"] = {GROUP + "/acceptance-source": "true"}
        self.apply(resource)

    def reader(self, name, claim):
        labels = {"app": name}
        self.apply(self.resource("apps/v1", "Deployment", name, {
            "replicas": 1, "strategy": {"type": "Recreate"},
            "selector": {"matchLabels": labels}, "template": {
                "metadata": {"labels": labels}, "spec": {
                    "automountServiceAccountToken": False,
                    "containers": [{"name": "read", "image": IMAGE,
                                    "command": ["httpd", "-f", "-p", "8080", "-h", "/data"],
                                    "securityContext": {"allowPrivilegeEscalation": False},
                                    "ports": [{"containerPort": 8080}],
                                    "volumeMounts": [{"name": "data", "mountPath": "/data", "readOnly": True}]}],
                    "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": claim}}]}}}))
        self.apply(self.resource("v1", "Service", name, {
            "selector": labels, "ports": [{"port": 8080, "targetPort": 8080}]}))

    def write(self, claim, marker):
        require(marker.endswith(b"\n") and len(marker) == 49, "Unexpected acceptance marker")
        # A generated hexadecimal marker is safe as a shell literal. Never feed
        # workload-supplied data or secrets into this command.
        text = marker.decode().strip()
        require(all(c in "0123456789abcdef" for c in text), "Marker is not hexadecimal")
        name = "write-" + secrets.token_hex(4)
        command = f"set -eu; printf '%s\\n' '{text}' > /data/index.tmp; sync; mv /data/index.tmp /data/index.html; sync"
        self.apply(self.resource("batch/v1", "Job", name, {
            "backoffLimit": 0, "activeDeadlineSeconds": self.timeout,
            "template": {"spec": {"restartPolicy": "Never", "automountServiceAccountToken": False,
                "containers": [{"name": "write", "image": IMAGE, "command": ["sh", "-c", command],
                                "volumeMounts": [{"name": "data", "mountPath": "/data"}]}],
                "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": claim}}]}}}))

        def completed():
            status = self.get("jobs", name).get("status", {})
            require(not status.get("failed"), f"Marker write Job {name} failed")
            return status.get("succeeded") == 1
        self.until(completed)

    def read(self, service):
        return self.request(self.c["AppURL"] +
                            f"/api/v1/namespaces/{self.namespace}/services/{service}:8080/proxy/",
                            opener=self.browser)

    def marker_is(self, service, marker):
        self.until(lambda: self.read(service) == marker)

    def remember_pvc(self, name):
        obj = self.until(lambda: self.get("persistentvolumeclaims", name))
        require(obj["status"].get("phase") == "Bound", f"PVC {name} is not bound")
        self.pvc_uids[name] = obj["metadata"]["uid"]
        return obj

    def csi_roundtrip(self):
        """Real CSI snapshot -> mutate source -> new PVC -> compare both volumes."""
        started = time.monotonic()
        self.prepare_namespace()
        original = (secrets.token_hex(24) + "\n").encode()
        mutated = (secrets.token_hex(24) + "\n").encode()
        self.pvc("source")
        self.reader("source", "source")  # First consumer makes WFFC provisioning possible.
        self.write("source", original)
        self.marker_is("source", original)
        source = self.remember_pvc("source")
        self.apply(self.resource("snapshot.storage.k8s.io/v1", "VolumeSnapshot", "before-mutation", {
            "volumeSnapshotClassName": "btrfs", "source": {"persistentVolumeClaimName": "source"}}))

        def snapshot_ready():
            obj = self.get(SNAPSHOTS, "before-mutation")
            status = obj.get("status", {})
            require(not status.get("error"), "CSI snapshot failed: " + json.dumps(status.get("error")))
            return status.get("readyToUse") is True and status.get("boundVolumeSnapshotContentName") and obj
        snapshot = self.until(snapshot_ready)
        content = self.get("volumesnapshotcontents.snapshot.storage.k8s.io",
                           snapshot["status"]["boundVolumeSnapshotContentName"], "_all")
        require(content["spec"].get("driver") == "btrfs.csi.local", "Snapshot used the wrong CSI driver")
        require(content.get("status", {}).get("snapshotHandle"), "CSI did not publish a real snapshot handle")
        require(content["spec"]["volumeSnapshotRef"]["uid"] == snapshot["metadata"]["uid"],
                "Snapshot content is bound to another snapshot")
        self.write("source", mutated)
        self.marker_is("source", mutated)
        self.pvc("restored", "before-mutation")
        self.reader("restored", "restored")
        self.marker_is("restored", original)
        restored = self.remember_pvc("restored")
        require(restored["spec"]["volumeName"] != source["spec"]["volumeName"], "Restore reused the source PV")
        self.marker_is("source", mutated)
        require(self.get("persistentvolumeclaims", "source")["metadata"]["uid"] == self.pvc_uids["source"],
                "Restore replaced the source PVC")
        self.markers = {"source": mutated, "restored": original}
        self.record("csi", "CSI snapshot restores the old marker into a new PVC and preserves the mutated source",
                    elapsedSeconds=round(time.monotonic() - started, 3),
                    sourcePVCUID=self.pvc_uids["source"], restoredPVCUID=self.pvc_uids["restored"],
                    snapshotUID=snapshot["metadata"]["uid"],
                    originalMarkerSHA256=hashlib.sha256(original).hexdigest(),
                    mutatedMarkerSHA256=hashlib.sha256(mutated).hexdigest())

    def verify_restart(self, c, browser, process):
        """Call after a real stop/start and fresh Kite authentication."""
        self.connect(c, browser, process)
        require(self.markers, "The CSI drill has not completed")
        for name, marker in self.markers.items():
            self.marker_is(name, marker)
            require(self.get("persistentvolumeclaims", name)["metadata"]["uid"] == self.pvc_uids[name],
                    f"PVC {name} identity changed across restart")
        self.record("restart", "Restart preserves CSI source and restored PVC identities and markers")

    def completed_request(self, plural, name, namespace=None):
        def completed():
            obj = self.get(plural + "." + GROUP, name, namespace)
            status = obj.get("status", {})
            require(status.get("phase") not in ("Failed", "Rejected", "ResumeFailed"),
                    f"{plural}/{name} failed: {json.dumps(status)}")
            return (status.get("phase") == "Succeeded"
                    and status.get("observedGeneration") == obj["metadata"]["generation"] and obj)
        return self.until(completed)

    def workload_roundtrip(self):
        """The native controller must turn recovery CRDs into real CSI operations."""
        account = "recovery-workload"
        self.apply({"apiVersion": "v1", "kind": "ServiceAccount",
                    "metadata": {"name": account, "namespace": self.namespace}})
        self.apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
                    "metadata": {"name": "acceptance-recovery", "namespace": self.namespace},
                    "rules": [{"apiGroups": [""], "resources": ["persistentvolumeclaims", "configmaps"],
                               "verbs": ["get", "list", "create"]},
                              {"apiGroups": ["snapshot.storage.k8s.io"], "resources": ["volumesnapshots"],
                               "verbs": ["get", "list", "create"]}]})
        self.apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
                    "metadata": {"name": "acceptance-recovery", "namespace": self.namespace},
                    "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "acceptance-recovery"},
                    "subjects": [{"kind": "ServiceAccount", "name": account, "namespace": self.namespace}]})
        before = self.markers["source"]
        after = (secrets.token_hex(24) + "\n").encode()
        point_name = "workload-before-mutation"
        self.apply(self.resource(API, "RecoveryPoint", point_name, {"capture": {
            "selector": {"matchLabels": {GROUP + "/acceptance-source": "true"}},
            "expectedPVCs": ["source"], "consistency": "PerPVC", "timeoutSeconds": 300}}))
        point = self.completed_request("recoverypoints", point_name)
        members = point["status"].get("members", [])
        require(len(members) == 1 and members[0]["pvc"] == "source"
                and members[0]["uid"] == self.pvc_uids["source"] and members[0].get("ready"),
                "RecoveryPoint did not capture the exact source PVC identity")
        self.write("source", after)
        self.marker_is("source", after)
        self.apply(self.resource(API, "RestoreRequest", "workload-restore", {
            "recoveryPoint": point_name, "recoveryPointUID": point["metadata"]["uid"],
            "pvcNames": {"source": "crd-restored"}}))
        # A first consumer is necessary for WaitForFirstConsumer storage classes.
        # This reader never writes, including while its claim is still absent.
        self.reader("crd-restored", "crd-restored")
        restored_request = self.completed_request("restorerequests", "workload-restore")
        require(restored_request["status"].get("restoredPVCs", {}).get("source") == "crd-restored",
                "RestoreRequest did not report the requested new PVC")
        self.marker_is("crd-restored", before)
        self.marker_is("source", after)
        restored = self.remember_pvc("crd-restored")
        source = self.get("persistentvolumeclaims", "source")
        require(source["metadata"]["uid"] == self.pvc_uids["source"], "CRD restore replaced the source PVC")
        require(restored["spec"]["volumeName"] != source["spec"]["volumeName"], "CRD restore reused the source PV")
        self.markers.update({"source": after, "crd-restored": before})
        self.record("workload-crd", "RecoveryPoint and RestoreRequest restore real data into a new PVC without rewinding the source",
                    pointUID=point["metadata"]["uid"], restoreUID=restored_request["metadata"]["uid"],
                    restoredPVCUID=self.pvc_uids["crd-restored"], consistency="PerPVC")

    def backup_cli(self, binary, *args, reject=False):
        result = subprocess.run([str(Path(binary).resolve()), "recovery-backup", *map(str, args)],
                                capture_output=True, text=True, timeout=max(self.timeout, 1800))
        if reject:
            require(result.returncode != 0, "Recovery verification accepted a damaged encrypted backup")
            require(result.stderr.strip(), "Failed verification did not provide an error")
            return result
        require(result.returncode == 0,
                f"recovery-backup {args[0]} failed: {result.stderr[-4096:]}")
        return json.loads(result.stdout)

    def workload_host_roundtrip(self, binary):
        """Export through BackupDestination, remove the guest copy, import and populate.

        Run before cold_cases so every later full capture includes host-restored.
        This drills same-appliance workload export/import and a newly provisioned
        PVC. It does not claim replacement-host or lost-image recovery acceptance.
        """
        require(not self.cold_points, "Workload host recovery must run before the cold captures")
        require("source" in self.markers, "Workload host recovery needs the source marker drill")
        started = time.monotonic()
        workspace = self.state.resolve().with_name(self.state.name + "-workload-host-backup")
        workspace.mkdir(mode=0o700)
        repository, keys, staging, independent = (workspace / name for name in
                                                  ("repository", "keys", "staging", "unenrolled-state"))
        for directory in (repository, keys, staging, independent):
            directory.mkdir(mode=0o700)
        key = keys / "recovery.age.key"

        def cli(*args):
            return self.backup_cli(binary, *args)

        enrollment = cli("destination", "add", "--state", self.state.resolve(), "--name", "Workload recovery acceptance",
                         "--path", repository, "--key", key, "--keep", "8")
        require(enrollment.get("id") and enrollment.get("path") == str(repository) and key.is_file(),
                "Workload recovery destination enrollment did not produce the requested repository and separate key")
        destination_name = self.namespace + "-host"
        self.apply({"apiVersion": API, "kind": "BackupDestination", "metadata": {"name": destination_name},
                    "spec": {"provider": "HostDirectory", "hostDirectoryID": enrollment["id"],
                             "allowedNamespaces": [self.namespace]}})
        before = self.markers["source"]
        self.marker_is("source", before)
        point_name = "workload-host-export"
        self.apply(self.resource(API, "RecoveryPoint", point_name, {"destination": destination_name, "capture": {
            "selector": {"matchLabels": {GROUP + "/acceptance-source": "true"}},
            "expectedPVCs": ["source"], "consistency": "PerPVC", "timeoutSeconds": 300}}))
        point = self.completed_request("recoverypoints", point_name)

        def exported():
            current = self.get("recoverypoints." + GROUP, point_name)
            require(current["metadata"]["uid"] == point["metadata"]["uid"], "Export replaced its RecoveryPoint")
            status = current.get("status", {})
            export = status.get("export") or {}
            require(status.get("phase") not in ("Failed", "Rejected", "ResumeFailed"),
                    "Workload capture failed during export: " + json.dumps(status))
            require(export.get("phase") != "Failed", "Workload host export failed: " + json.dumps(export))
            return (status.get("phase") == "Succeeded" and export.get("phase") == "Succeeded"
                    and export.get("receipt") and not status.get("exportPending") and current)

        point = self.until(exported)
        members = point["status"].get("members", [])
        require(len(members) == 1 and members[0]["pvc"] == "source"
                and members[0]["uid"] == self.pvc_uids["source"] and members[0].get("ready"),
                "Exported workload point does not contain the exact source PVC")
        receipt = point["status"]["export"]["receipt"]
        entries = cli("list", "--repository", repository, "--state", independent)
        matches = [entry for entry in entries if entry.get("id") == receipt]
        require(len(matches) == 1, "Workload export receipt does not identify a unique repository entry")
        entry = matches[0]
        require(entry.get("scope") == "workload" and entry.get("mode") == "Full" and entry.get("pointID")
                and entry.get("plainBytes", 0) > 0 and entry.get("cipherBytes", 0) > 0,
                "Workload export did not register and export a nonempty guest workload point")
        point_id = entry["pointID"]
        verified = cli("verify", "--repository", repository, "--state", independent, "--id", receipt, "--key", key)
        require(verified.get("pointID") == point_id and verified.get("verifiedAt")
                and verified.get("sha256") == entry.get("sha256"), "Workload export did not authenticate independently")
        self.record("workload-host-export", "RecoveryPoint registers real CSI data and exports through its authorized BackupDestination",
                    pointUID=point["metadata"]["uid"], pointID=point_id, backupID=receipt,
                    sourcePVCUID=self.pvc_uids["source"], ciphertextSHA256=entry["sha256"])

        after = (secrets.token_hex(24) + "\n").encode()
        self.write("source", after)
        self.marker_is("source", after)
        self.markers["source"] = after
        point_url = self.c["URL"] + "/control/recovery/points/" + quote(point_id, safe="")
        deletion = urllib.request.Request(point_url, method="DELETE", headers={"Authorization": "Bearer " + self.c["Token"]})
        # Management stays on its authenticated loopback origin, without a proxy.
        with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(deletion, timeout=self.timeout) as response:
            require(json.loads(response.read()).get("deleted") is True, "Guest workload point deletion did not complete")
        try:
            self.request(point_url, self.c["Token"])
        except urllib.error.HTTPError as error:
            require(error.code == 404, f"Deleted workload point returned HTTP {error.code}, expected 404")
        else:
            raise AssertionError("Guest workload point still exists before repository import")
        snapshot = self.get(SNAPSHOTS, members[0]["snapshot"])
        require(snapshot["metadata"]["uid"] == members[0]["snapshotUID"]
                and snapshot.get("status", {}).get("readyToUse") is True,
                "Deleting the registered guest point removed or replaced the original CSI snapshot")
        imported = cli("import", "--state", self.state.resolve(), "--repository", repository,
                       "--id", receipt, "--key", key, "--staging-dir", staging)
        require(imported.get("id") == point_id and imported.get("scope") == "workload",
                "Repository import did not recreate the exported workload point")
        self.apply(self.resource(API, "RestoreRequest", "workload-host-restore", {
            "durablePointID": imported["id"], "pvcNames": {"source": "host-restored"}}))
        # Imported restore selects the node itself and fences consumers until the
        # guest has populated the new CSI volume. Do not start a reader early.
        restored_request = self.completed_request("restorerequests", "workload-host-restore")
        require(restored_request["status"].get("restoredPVCs", {}).get("source") == "host-restored",
                "Imported workload restore did not report the requested new PVC")
        restored = self.remember_pvc("host-restored")
        require(not restored["spec"].get("dataSource") and not restored["spec"].get("dataSourceRef"),
                "Imported workload restore used a CSI clone instead of populating a fresh volume")
        require(restored["metadata"].get("labels", {}).get(GROUP + "/restoring") != "true",
                "Imported restore reported success before releasing its consumer fence")
        source = self.get("persistentvolumeclaims", "source")
        require(source["metadata"]["uid"] == self.pvc_uids["source"], "Imported restore replaced the source PVC")
        require(restored["spec"]["volumeName"] != source["spec"]["volumeName"], "Imported restore reused the source PV")
        self.reader("host-restored", "host-restored")
        self.marker_is("host-restored", before)
        self.marker_is("source", after)
        self.markers["host-restored"] = before
        require(not list(staging.iterdir()), "Successful workload import retained plaintext staging files")
        require(not list(independent.iterdir()), "Workload repository verification used cluster state")
        self.record("workload-host-import", "Repository import after guest-point deletion populates a fresh PVC with the old marker and preserves the source",
                    pointID=point_id, backupID=receipt, restoreUID=restored_request["metadata"]["uid"],
                    restoredPVCUID=self.pvc_uids["host-restored"], originalGuestPointDeleted=True,
                    originalCSISnapshotRetained=True, replacementHostTested=False,
                    originalMarkerSHA256=hashlib.sha256(before).hexdigest(),
                    mutatedMarkerSHA256=hashlib.sha256(after).hexdigest(),
                    elapsedSeconds=round(time.monotonic() - started, 3))

    def cold_roundtrip(self, scope, capture_spec, restore_spec, complete):
        """Exercise cluster CRDs, a cold restart, datastore rewind and PVC scope.

        Bridge wiring supplies the finalized cluster spec and lifecycle interface:

        * ``capture_spec`` is the exact ClusterRecoveryPoint spec for this scope.
        * ``restore_spec(point, point_id)`` returns a ClusterRestoreRequest spec.
        * ``complete(cr)`` waits for the launcher's automatic graceful shutdown,
          guest restart and fresh Kite login. It returns ``(control, browser, process,
          operation)``; operation is the guest engine's durable Operation JSON.

        Complete must use the CR's Kubernetes UID, never invent a second operation
        ID. It must observe completion from the out-of-datastore journal: a restore
        may rewind away its own CR. The caller owns launcher process/log cleanup;
        the callback must not kill or replace the persistent launcher process.
        This intentionally has no guessed guest/control HTTP endpoint.
        """
        require(scope in ("system", "full"), "Cold scope must be system or full")
        started = time.monotonic()
        point_name = "cold-" + scope + "-" + secrets.token_hex(4)
        config_name = "cold-" + scope
        before = (secrets.token_hex(24) + "\n").encode()
        after = (secrets.token_hex(24) + "\n").encode()

        def config(marker):
            self.apply({"apiVersion": "v1", "kind": "ConfigMap",
                        "metadata": {"name": config_name, "namespace": self.namespace},
                        "data": {"marker": marker.decode().strip()}})

        def cluster_request(kind, plural, name, spec):
            self.apply({"apiVersion": API, "kind": kind, "metadata": {"name": name}, "spec": spec})
            return self.until(lambda: self.get(plural + "." + GROUP, name, "_all"))

        def finish(cr, action):
            c, browser, process, operation = complete(cr)
            require(process is self.process and process.poll() is None,
                    "Cold recovery must keep the original launcher process running")
            self.connect(c, browser, process)
            require(operation.get("phase") == "completed",
                    f"Cold {action} did not complete: {json.dumps(operation)}")
            request = operation.get("request", {})
            require(request.get("uid") == cr["metadata"]["uid"], "Cold operation UID differs from its CR")
            require(request.get("action") == action and request.get("scope") == scope,
                    "Cold operation action/scope differs from the requested drill")
            return operation

        self.write("source", before)
        self.marker_is("source", before)
        self.markers["source"] = before
        config(before)
        config_uid = self.get("configmaps", config_name)["metadata"]["uid"]
        point = cluster_request("ClusterRecoveryPoint", "clusterrecoverypoints", point_name, capture_spec)
        captured = finish(point, "capture")
        point_id = captured.get("pointID")
        require(point_id, "Cold capture returned no durable point ID")
        self.cold_points[scope] = captured
        # A cold capture must restart and retain the current workload state.
        self.verify_restart(self.c, self.browser, self.process)
        self.write("source", after)
        self.marker_is("source", after)
        config(after)
        require(self.get("configmaps", config_name)["data"]["marker"] == after.decode().strip(),
                "Datastore mutation was not observed before restore")
        restore = cluster_request("ClusterRestoreRequest", "clusterrestorerequests", point_name + "-restore",
                                  restore_spec(point, point_id))
        restored = finish(restore, "restore")
        require(restored.get("pointID") == point_id, "Cold restore completed for another recovery point")
        require(restored.get("safetyID"), "Cold restore did not retain a pre-restore safety point")
        self.until(lambda: self.get("configmaps", config_name).get("data", {}).get("marker") == before.decode().strip())
        require(self.get("configmaps", config_name)["metadata"]["uid"] == config_uid,
                "Datastore restore regenerated the ConfigMap instead of restoring its identity")
        # System scope restores K3s state but intentionally leaves CSI data live.
        # Full scope restores both system and the captured CSI volume contents.
        self.markers["source"] = before if scope == "full" else after
        self.verify_restart(self.c, self.browser, self.process)
        self.record("cold-" + scope, f"Cluster {scope} cold capture/restart/restore rewinds datastore and verifies PVC scope",
                    scope=scope, pointID=point_id, safetyID=restored["safetyID"],
                    captureUID=point["metadata"]["uid"], restoreUID=restore["metadata"]["uid"],
                    elapsedSeconds=round(time.monotonic() - started, 3))

    def operation_status(self, control, uid):
        return json.loads(self.request(control["URL"] + "/control/recovery/operations/" + quote(uid, safe=""),
                                       control["Token"]))

    def complete_cold(self, cr, admin, operation_status=None):
        """Wait for a real operation across guest restart in the same launcher.

        ``operation_status(control, uid)`` reads the durable guest Operation via
        the parent bridge's authenticated recovery-status endpoint. Kubernetes
        requests continue to go through Kite; this callback never needs kube or
        any general-purpose guest command endpoint.

        Updating the original control dictionary and reusing the browser opener
        also refreshes the smoke runner's credentials for its remaining checks
        and graceful shutdown. Passwords and capabilities are never recorded.
        """
        uid = cr["metadata"]["uid"]
        operation_status = operation_status or self.operation_status

        def completed():
            control = json.loads((self.state / "control.json").read_text())
            operation = operation_status(control, uid)
            require(operation.get("phase") != "failed",
                    "Cold operation failed: " + str(operation.get("error", "unspecified error")))
            if operation.get("phase") != "completed":
                return False
            require(operation.get("request", {}).get("uid") == uid,
                    "Recovery status endpoint returned another operation")
            status = json.loads(self.request(control["URL"] + "/control/status", control["Token"]))
            if not status.get("healthy") or not status.get("console"):
                return False
            # The original administrator must still authenticate after datastore
            # restore. Setup/create is deliberately never retried here.
            self.request(control["AppURL"] + "/_open?token=" + control["AppToken"], opener=self.browser)
            self.request(control["AppURL"] + "/api/auth/login/password", data=admin, opener=self.browser)
            bootstrap = json.loads(self.request(control["AppURL"] + "/api/v1/bootstrap", opener=self.browser))
            require(bootstrap.get("setup", {}).get("initialized"), "Cold restore lost the Kite administrator")
            self.c.clear()
            self.c.update(control)
            return operation

        operation = self.until(completed)
        return self.c, self.browser, self.process, operation

    def cold_cases(self, admin, operation_status=None, capture_spec=None, restore_spec=None):
        """Run both scopes using finalized cluster-schema builders from the bridge.

        Builders are ``capture_spec(scope)`` and
        ``restore_spec(scope, point_cr, point_id)``. The status reader is described
        in complete_cold. Defaults use the native v1alpha1 cluster specs and host
        control endpoint. No second VM or launcher process is started by this code.
        Follow with host_backup and require_complete for the full acceptance gate.
        """
        capture_spec = capture_spec or (lambda scope: {"scope": scope})
        restore_spec = restore_spec or (lambda scope, point, point_id: {"scope": scope, "durablePointID": point_id})
        for scope in ("system", "full"):
            self.cold_roundtrip(scope, capture_spec(scope),
                                lambda point, point_id: restore_spec(scope, point, point_id),
                                lambda cr: self.complete_cold(cr, admin, operation_status))

    def host_backup(self, binary):
        """Export a real full point, verify independently, reject damaged copies.

        Repository and key directories are new siblings of the disposable cluster
        state. Direct --repository verification uses an unrelated empty --state,
        proving that enrollment and a running guest are unnecessary to authenticate
        the archive. This is integrity acceptance, not replacement-host restore.

        Returns host-only repository/key/ID paths for a future separate-guest import
        drill. They must not be sent to a workload or included in uploaded evidence.
        """
        require("full" in self.cold_points, "Host export needs a completed full cold capture")
        point_id = self.cold_points["full"]["pointID"]
        binary = Path(binary).resolve()
        workspace = self.state.resolve().with_name(self.state.name + "-host-backup")
        workspace.mkdir(mode=0o700)  # Exclusive: never reuse an existing repository/key.
        repository, keys, independent = (workspace / name for name in ("repository", "keys", "unenrolled-state"))
        for directory in (repository, keys, independent):
            directory.mkdir(mode=0o700)
        key = keys / "recovery.age.key"
        backup_id = "acceptance-" + secrets.token_hex(8)
        def cli(*args, reject=False):
            return self.backup_cli(binary, *args, reject=reject)

        destination = cli("destination", "add", "--state", self.state.resolve(), "--name", "Recovery acceptance",
                          "--path", repository, "--key", key, "--keep", "8")
        require(destination.get("id") and destination.get("path") == str(repository),
                "Host destination did not enroll the requested repository")
        require(key.is_file(), "Enrollment did not create the separate recovery key")
        started = time.monotonic()
        entry = cli("export", "--state", self.state.resolve(), "--destination", destination["id"],
                    "--point", point_id, "--scope", "full", "--id", backup_id)
        require(entry.get("id") == backup_id and entry.get("pointID") == point_id
                and entry.get("scope") == "full" and entry.get("mode") == "Full",
                "Host export metadata does not identify the requested full point")
        require(entry.get("plainBytes", 0) > 0 and entry.get("cipherBytes", 0) > 0,
                "Host export produced an empty backup")
        archive = repository / (backup_id + ".age")
        require(archive.stat().st_size == entry["cipherBytes"], "Published ciphertext length differs from its catalog")
        with archive.open("rb") as stream:
            require(stream.readline(64) == b"age-encryption.org/v1\n", "Host recovery export is not age-encrypted")
            stream.seek(0)
            archive_hash = hashlib.file_digest(stream, "sha256").hexdigest()
        require(archive_hash == entry.get("sha256"), "Published ciphertext hash differs from its catalog")
        self.record("host-export", "Native host CLI exports a real full recovery point to an age-encrypted repository",
                    pointID=point_id, backupID=backup_id, cipherBytes=entry["cipherBytes"],
                    plainBytes=entry["plainBytes"], ciphertextSHA256=archive_hash,
                    elapsedSeconds=round(time.monotonic() - started, 3))

        def verify(repo, reject=False):
            return cli("verify", "--repository", repo, "--state", independent,
                       "--id", backup_id, "--key", key, reject=reject)

        def verified_entry(result):
            require(result.get("id") == backup_id and result.get("pointID") == point_id
                    and result.get("sha256") == archive_hash and result.get("verifiedAt"),
                    "Independent verification did not authenticate and record the exported point")

        verified_entry(verify(repository))
        listed = cli("list", "--repository", repository, "--state", independent)
        matches = [item for item in listed if item.get("id") == backup_id]
        require(len(matches) == 1, "Independent repository listing lost or duplicated the exported point")
        verified_entry(matches[0])
        rejected = []
        for damage in ("corrupted", "truncated"):
            # Never hardlink test ciphertext to the original. Only one disposable
            # copy exists at a time; large exports need not occupy three copies.
            with tempfile.TemporaryDirectory(prefix=damage + "-", dir=workspace) as temporary:
                damaged_repo = Path(temporary) / "repository"
                shutil.copytree(repository, damaged_repo, copy_function=shutil.copyfile)
                verified_entry(verify(damaged_repo))  # Prove this copy/path/key worked before tampering.
                damaged_archive = damaged_repo / archive.name
                size = damaged_archive.stat().st_size
                require(size > 64, "Encrypted export is too short for the integrity drill")
                with damaged_archive.open("r+b") as stream:
                    if damage == "corrupted":
                        stream.seek(-1, 2)
                        tail = stream.read(1)
                        stream.seek(-1, 2)
                        stream.write(bytes([tail[0] ^ 1]))
                    else:
                        stream.truncate(size - 1)
                # The public catalog is not an authenticity boundary. Recompute
                # its public size/hash so rejection must authenticate age's final
                # chunk instead of merely comparing an unauthenticated checksum.
                with damaged_archive.open("rb") as stream:
                    damaged_hash = hashlib.file_digest(stream, "sha256").hexdigest()
                catalog_path = damaged_repo / "catalog.json"
                catalog = json.loads(catalog_path.read_text())
                for item in catalog["entries"]:
                    if item["id"] == backup_id:
                        item["cipherBytes"] = damaged_archive.stat().st_size
                        item["sha256"] = damaged_hash
                        item.pop("verifiedAt", None)
                catalog_path.write_text(json.dumps(catalog) + "\n")
                before_catalog = catalog_path.read_bytes()
                verify(damaged_repo, reject=True)
                require(catalog_path.read_bytes() == before_catalog,
                        "Failed authentication changed the damaged repository catalog")
                rejected.append(damage)
        verified_entry(verify(repository))  # Original remains authentic after both rejection drills.
        require(not list(independent.iterdir()), "Direct repository verification used or created cluster state")
        self.record("host-verify", "Direct repository verification authenticates the full archive and rejects corrupted/truncated copies",
                    pointID=point_id, backupID=backup_id, rejectedCopies=rejected,
                    publicChecksumsRecomputed=True, originalStillVerifies=True,
                    enrollmentRequired=False, replacementImportTested=False)
        return {"repository": repository, "key": key, "id": backup_id, "pointID": point_id}

    def require_complete(self):
        """The opt-in suite cannot pass with missing cold or host-backup checks."""
        observed = {check["case"] for check in self.evidence["checks"] if check["result"] == "passed"}
        require(REQUIRED_CASES <= observed, "Recovery acceptance incomplete: " + ", ".join(sorted(REQUIRED_CASES - observed)))


def recovery_acceptance(c, browser, request, wait, process, state, timeout):
    """Explicit smoke --recovery entrypoint; returns restart-verification hooks."""
    harness = RecoveryAcceptance(c, browser, request, wait, process, state, timeout)
    harness.csi_roundtrip()
    harness.workload_roundtrip()
    return harness
