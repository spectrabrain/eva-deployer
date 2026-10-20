#!/usr/bin/env python3
"""Offline production-helper and real Ansible caller tests. Never access a cluster."""

import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch
from urllib.request import urlopen

import yaml


# Set BEFORE loading the packaged role; tests must not package __pycache__.
sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
ROLE = ROOT / "src/solution/roles/eva_traefik_ports"
HELPER = ROLE / "files/traefik_ports.py"
spec = importlib.util.spec_from_file_location("traefik_ports", HELPER)
ports = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ports)


def setUpModule():
    # CI must exercise the real managed Helm (including its post-renderer API),
    # not rely on whichever Helm happens to be preinstalled on a runner.
    if os.environ.get("EVA_TEST_HELM_DOWNLOAD") != "1":
        return
    temporary = tempfile.TemporaryDirectory(prefix="eva-traefik-helm-")
    unittest.addModuleCleanup(temporary.cleanup)
    source = (ROOT / "tools/eva/internal/runtime/bootstrap_online.go").read_text()
    artifact = re.search(r'Name:\s*"helm",\s*URL:\s*"([^"]+)",\s*SHA256:\s*"([0-9a-f]{64})"', source)
    if artifact is None:
        raise AssertionError("Managed Helm runtime artifact must have a checksum pin")
    url, checksum = artifact.groups()
    with urlopen(url, timeout=120) as response:
        data = response.read()
    if hashlib.sha256(data).hexdigest() != checksum:
        raise AssertionError("Managed Helm archive checksum mismatch")
    with tarfile.open(fileobj=io.BytesIO(data)) as archive:
        member = archive.getmember("linux-amd64/helm")
        if not member.isfile():
            raise AssertionError("Managed Helm binary is not a regular archive member")
        with archive.extractfile(member) as stream:
            binary = Path(temporary.name) / "helm"
            binary.write_bytes(stream.read())
    binary.chmod(0o700)
    environment = patch.dict(os.environ, PATH=temporary.name + os.pathsep + os.environ.get("PATH", ""))
    environment.start()
    unittest.addModuleCleanup(environment.stop)


def defaults(modern=True):
    return {"ports": {
        "traefik": {"port": 9000, "expose": {"default": False} if modern else False},
        "web": {"port": 8000, "exposedPort": 80, "expose": {"default": True} if modern else True},
        "websecure": {"port": 8443, "exposedPort": 443, "expose": {"default": True} if modern else True}},
        "service": {"type": "LoadBalancer"}}


def objects(component="app", number=32010, secure=False, host="10.159.56.197", path="/"):
    name = "eva-" + component
    data = ({"BACKEND_HOST": host, "BACKEND_PUBLIC_PORT": str(number), "BACKEND_SECURE": str(secure).lower(),
             "BACKEND_SECURE_PORT": str(number), "BACKEND_PORT": "9999"} if component == "app" else
            {"ISSUER": f"{'https' if secure else 'http'}://{host}:{number}/realms/eva-iam"})
    ingress = {"kind": "Ingress", "apiVersion": "networking.k8s.io/v1", "metadata": {"name": name, "namespace": name,
               "uid": name + "-uid", "resourceVersion": "1", "labels": {"app.kubernetes.io/managed-by": "Helm"},
               "annotations": {"meta.helm.sh/release-name": name, "meta.helm.sh/release-namespace": name}},
               "spec": {"ingressClassName": "traefik", "rules": [{"http": {"paths": [{"path": path}]}}]}}
    if secure:
        ingress["spec"]["tls"] = [{"hosts": [host], "secretName": "tls"}]
    metadata = copy.deepcopy(ingress["metadata"])
    metadata.update(name=name + ("-env" if component == "app" else ""), uid=name + "-config-uid")
    return [{"kind": "ConfigMap", "metadata": metadata, "data": data}, ingress]


def endpoint(component="app", number=32010, secure=False, path="/"):
    return ports.endpoint(objects(component, number, secure, path=path), component, "eva-" + component, "eva-" + component)


class FakeCluster:
    def __init__(self, directory, modern=True):
        self.directory = str(directory)
        self.defaults = defaults(modern)
        content = io.BytesIO()
        with tarfile.open(fileobj=content, mode="w:gz") as archive:
            raw = yaml.safe_dump(self.defaults).encode()
            member = tarfile.TarInfo("traefik/values.yaml")
            member.size = len(raw)
            archive.addfile(member, io.BytesIO(raw))
        import base64
        self.chart = {"metadata": {"name": "traefik", "namespace": "kube-system", "resourceVersion": "1"},
                      "spec": {"chartContent": base64.b64encode(content.getvalue()).decode()}}
        self.config = None
        self.live = []
        self.history = {}
        self.calls = []
        self.stale = False
        self.failed = False
        self.race = False
        self.stale_pod = False
        self.wrong_target = False
        self.bad_args = False
        self.bad_owner = False
        self.service_type = "LoadBalancer"
        self.no_lb = False
        self.ingress_race = False
        self.pod_args_stale = False
        self.pod_not_ready = False
        self.pod_foreign_owner = False
        self.extra_args = False
        self.duplicate_args = False

    def install(self, manifest):
        name = manifest[-1]["metadata"]["namespace"]
        self.live = [obj for obj in self.live if obj["metadata"].get("namespace") != name] + copy.deepcopy(manifest)
        self.history[name] = copy.deepcopy(manifest)

    def controller_args(self):
        effective = ports.merge(self.defaults, yaml.safe_load((self.config or {}).get("spec", {}).get("valuesContent", "")) or {})
        args = [f"--entrypoints.{name}.address=:{item['port']}" for name, item in effective["ports"].items()]
        if self.extra_args:
            args.append("--entrypoints.foreign.address=:12345")
        if self.duplicate_args:
            args.append(args[0])
        return [] if self.bad_args else args

    def state_ports(self):
        values = yaml.safe_load((self.config or {}).get("spec", {}).get("valuesContent", "")) or {}
        effective = ports.merge(self.defaults, values)
        if self.stale:
            effective = self.defaults
        return {name: {"port": item.get("exposedPort", item["port"]), "targetPort": name, "protocol": "TCP"}
                for name, item in effective["ports"].items() if ports.enabled(item)}

    def get(self, resource, namespace="kube-system", name=None, optional=False):
        self.calls.append(("get", resource))
        active = self.state_ports()
        hostports = [{"hostPort": item["port"]} for item in active.values()]
        labels = {"svccontroller.k3s.cattle.io/svcname": "traefik", "svccontroller.k3s.cattle.io/svcnamespace": "kube-system"}
        if resource == ports.CHART:
            result = copy.deepcopy(self.chart)
            if self.failed:
                result["status"] = {"conditions": [{"type": "Failed", "status": "True"}]}
            return result
        if resource == ports.RESOURCE:
            return copy.deepcopy(self.config)
        if resource == "ingresses":
            return {"items": [obj for obj in self.live if obj["kind"] == "Ingress"]}
        if resource == "configmaps":
            return {"items": [obj for obj in self.live if obj["kind"] == "ConfigMap"]}
        if resource == "services":
            if namespace is None:
                return {"items": []}
            result = {"metadata": {"uid": "abcdef01-rest", "annotations": {"meta.helm.sh/release-name": "traefik", "meta.helm.sh/release-namespace": "kube-system"}},
                      "spec": {"type": self.service_type, "ports": [dict(name=name, **item) for name, item in active.items()]}}
            if self.wrong_target:
                result["spec"]["ports"][0]["targetPort"] = "wrong"
            return result
        if resource == "daemonsets":
            return {"items": [{"metadata": {"name": "svclb-traefik-abcdef01", "uid": "ds-1", "generation": 2, "labels": labels},
                              "spec": {"template": {"spec": {"containers": [{"ports": hostports}]}}},
                              "status": {"desiredNumberScheduled": 1, "numberReady": 1, "updatedNumberScheduled": 1, "observedGeneration": 2}}]}
        if resource == "pods":
            if self.no_lb:
                return {"items": []}
            if self.stale_pod:
                hostports = [{"hostPort": 80}, {"hostPort": 443}]
            return {"items": [{"metadata": {"namespace": "kube-system", "labels": labels,
                                             "ownerReferences": [{"kind": "DaemonSet", "uid": "foreign" if self.bad_owner else "ds-1"}]},
                               "spec": {"containers": [{"ports": hostports}]},
                               "status": {"conditions": [{"type": "Ready", "status": "True"}]}},
                              {"metadata": {"namespace": "kube-system", "name": "traefik-pod", "ownerReferences": [
                                  {"kind": "ReplicaSet", "uid": "foreign" if self.pod_foreign_owner else "rs-1", "controller": True}]},
                               "spec": {"containers": [{"name": "traefik", "args": [] if self.pod_args_stale else self.controller_args()}]},
                               "status": {"conditions": [{"type": "Ready", "status": "False" if self.pod_not_ready else "True"}]}}]}
        if resource == "replicasets":
            return {"items": [{"metadata": {"uid": "rs-1", "ownerReferences": [{"kind": "Deployment", "uid": "traefik-uid", "controller": True}]}}]}
        if resource == "deployments":
            return {"items": [{"metadata": {"name": "traefik", "namespace": "kube-system", "uid": "traefik-uid", "generation": 2,
                                           "labels": {"app.kubernetes.io/managed-by": "Helm"},
                                           "annotations": {"meta.helm.sh/release-name": "traefik", "meta.helm.sh/release-namespace": "kube-system"}},
                              "spec": {"replicas": 1, "template": {"spec": {"containers": [{"name": "traefik", "args": self.controller_args()}]}}},
                              "status": {"observedGeneration": 2, "replicas": 1, "readyReplicas": 1, "updatedReplicas": 1, "availableReplicas": 1}}]}
        raise AssertionError((resource, namespace, name))

    def run(self, args, stdin=None):
        self.calls.append(tuple(args[:3]))
        if args[:2] == ["helm", "version"]:
            return "v4.2.4"
        if args[:3] == ["helm", "get", "manifest"]:
            if args[3] not in self.history:
                raise ports.Unsafe("Helm history absent")
            return yaml.safe_dump_all(self.history[args[3]])
        if args[:2] == ["kubectl", "api-resources"]:
            return ""
        if args[:2] == ["kubectl", "create"]:
            if self.race or self.config:
                raise ports.Unsafe("AlreadyExists")
            self.config = json.loads(stdin)
            self.config["metadata"]["resourceVersion"] = "1"
            return "created"
        if args[:2] == ["kubectl", "patch"]:
            if args[2] == "ingresses":
                ingress = next(obj for obj in self.live if obj["kind"] == "Ingress" and obj["metadata"]["name"] == args[3])
                if self.ingress_race:
                    ingress["metadata"]["annotations"]["operator-race"] = "keep"
                    ingress["metadata"]["resourceVersion"] = "99"
                operations = json.loads(stdin)
                for op in operations:
                    if op["op"] == "test" and ingress["metadata"].get(op["path"].split("/")[-1]) != op["value"]:
                        raise ports.Unsafe("Injected CAS conflict")
                assert operations[1] == {"op": "test", "path": "/metadata/uid", "value": ingress["metadata"]["uid"]}
                for op in operations[2:]:
                    key = op["path"].split("/")[-1].replace("~1", "/").replace("~0", "~")
                    assert key in (ports.ENTRY, ports.TLS) and op["op"] == "add"
                    ingress["metadata"]["annotations"][key] = op["value"]
                ingress["metadata"]["resourceVersion"] = str(int(ingress["metadata"]["resourceVersion"]) + 1)
                return "patched"
            if self.race:
                raise ports.Unsafe("Conflict")
            operations = json.loads(stdin)
            assert operations[0] == {"op": "test", "path": "/metadata/resourceVersion", "value": self.config["metadata"]["resourceVersion"]}
            assert all(op["path"] != "/spec" for op in operations)
            for op in operations[1:]:
                if op["path"] == "/spec/valuesContent":
                    self.config["spec"]["valuesContent"] = op["value"]
                elif op["path"] == "/metadata/annotations":
                    self.config["metadata"]["annotations"] = op["value"]
                else:
                    self.config["metadata"].setdefault("annotations", {})[ports.STATE] = op["value"]
            self.config["metadata"]["resourceVersion"] = str(int(self.config["metadata"]["resourceVersion"]) + 1)
            return "patched"
        raise AssertionError(args)


class TraefikPortsTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.cluster = FakeCluster(self.directory)
        self.request = {"backup_root": str(self.directory / "backups"), "timeout": 0}

    def reconcile(self, planned=None):
        return ports.reconcile(self.cluster, self.request, planned or endpoint())

    def test_http_32010_releases_80_and_unused_443_and_is_idempotent(self):
        first = self.reconcile()
        self.assertTrue(first["changed"])
        self.assertEqual(self.cluster.state_ports(), {"web": {"port": 32010, "targetPort": "web", "protocol": "TCP"}})
        self.assertFalse(self.reconcile()["changed"])
        backup = Path(first["backup"])
        self.assertEqual(backup.stat().st_mode & 0o777, 0o600)
        self.assertEqual(backup.parent.stat().st_mode & 0o777, 0o700)
        self.assertIsNone(json.loads(backup.read_text())["helmchartconfig"])

    def test_preserve_full_values_metadata_and_spec(self):
        values = {"resources": {"limits": {"memory": "384Mi"}}, "ports": {"web": {"forwardedHeaders": {"trustedIPs": ["10.0.0.0/8"]}}}}
        self.cluster.config = {"metadata": {"resourceVersion": "9", "labels": {"operator": "keep"}, "annotations": {"unrelated": "keep"}},
                               "spec": {"valuesContent": yaml.safe_dump(values), "failurePolicy": "abort"}}
        self.reconcile()
        after = yaml.safe_load(self.cluster.config["spec"]["valuesContent"])
        self.assertEqual(after["resources"], values["resources"])
        self.assertEqual(after["ports"]["web"]["forwardedHeaders"], values["ports"]["web"]["forwardedHeaders"])
        self.assertEqual(self.cluster.config["metadata"]["labels"], {"operator": "keep"})
        self.assertEqual(self.cluster.config["metadata"]["annotations"]["unrelated"], "keep")
        self.assertEqual(self.cluster.config["spec"]["failurePolicy"], "abort")

    def test_create_and_update_races_never_adopt_or_retry(self):
        for existing in (False, True):
            with self.subTest(existing=existing):
                self.cluster = FakeCluster(self.directory)
                if existing:
                    self.cluster.config = {"metadata": {"resourceVersion": "1"}, "spec": {}}
                original = copy.deepcopy(self.cluster.config)
                self.cluster.race = True
                with self.assertRaisesRegex(ports.Unsafe, "CAS migration stopped"):
                    self.reconcile()
                self.assertEqual(self.cluster.config, original)

    def test_http_and_https_different_ports_and_router_isolation(self):
        for secure in (False, True):
            with self.subTest(secure=secure):
                self.cluster = FakeCluster(self.directory)
                app = endpoint(number=32010, secure=secure)
                first = self.reconcile(app)
                self.cluster.install(ports.isolate(objects(number=32010, secure=secure), first["endpoint"]))
                second = self.reconcile(endpoint("iam", 32011, secure))
                self.assertEqual({item["port"] for item in self.cluster.state_ports().values()}, {32010, 32011})
                self.assertNotEqual(first["endpoint"]["entrypoint"], second["endpoint"]["entrypoint"])
                self.assertFalse(self.reconcile(endpoint("iam", 32011, secure))["changed"])

    def test_mixed_protocol_ports(self):
        first = self.reconcile()
        self.cluster.install(ports.isolate(objects(), first["endpoint"]))
        self.reconcile(endpoint("iam", 32443, True))
        self.assertEqual(set(self.cluster.state_ports()), {"web", "eva-s-32443"})

    def test_shared_port_different_paths_allowed_same_paths_rejected(self):
        selected = {"app": endpoint(path="/app"), "iam": endpoint("iam", 32010, path="/iam")}
        _, state, _ = ports.plan(defaults(), self.cluster.chart, None, selected, [])
        self.assertEqual(state["app"]["entrypoint"], state["iam"]["entrypoint"])
        selected["iam"]["paths"] = ["/app"]
        with self.assertRaisesRegex(ports.Unsafe, "conflicting public port/path"):
            ports.plan(defaults(), self.cluster.chart, None, selected, [])

    def test_both_ip_unisolated_bootstrap_and_sequential_upgrades(self):
        for first in ("app", "iam"):
            for secure in (False, True):
                with self.subTest(first=first, secure=secure):
                    self.cluster = FakeCluster(self.directory)
                    for component, number in (("app", 32010), ("iam", 32011)):
                        manifest = objects(component, number, secure)
                        manifest[-1]["metadata"]["annotations"]["unrelated-private"] = "keep-secret"
                        self.cluster.install(manifest)
                    original = copy.deepcopy(self.cluster.live)
                    other = "iam" if first == "app" else "app"
                    selected = endpoint(first, 32010 if first == "app" else 32011, secure)
                    result = self.reconcile(selected)
                    self.assertTrue(result["changed"])
                    self.assertEqual({item["port"] for item in self.cluster.state_ports().values()}, {32010, 32011})
                    self.assertNotIn("web", self.cluster.state_ports())
                    self.assertNotIn("websecure", self.cluster.state_ports())
                    snapshot = json.loads(Path(result["backup"]).read_text())
                    self.assertEqual(snapshot["ingresses"], [obj for obj in original if obj["kind"] == "Ingress"])
                    for before, after in zip(original, self.cluster.live):
                        if before["kind"] == "ConfigMap":
                            self.assertEqual(before, after)
                            continue
                        self.assertEqual(before["spec"], after["spec"])
                        self.assertEqual(after["metadata"]["annotations"]["unrelated-private"], "keep-secret")
                        self.assertEqual(after["metadata"]["annotations"][ports.TLS], str(secure).lower())
                    writes = [call for call in self.cluster.calls if call[:2] in (("kubectl", "patch"), ("kubectl", "create"))]
                    self.assertEqual(writes[:2], [("kubectl", "patch", "ingresses")] * 2)
                    self.assertEqual(writes[2], ("kubectl", "create", "-f"))
                    self.cluster.install(ports.isolate(objects(first, selected["port"], secure), result["endpoint"]))
                    second = self.reconcile(endpoint(other, 32011 if other == "iam" else 32010, secure))
                    self.assertFalse(second["changed"])
                    self.cluster.install(ports.isolate(objects(other, second["endpoint"]["port"], secure), second["endpoint"]))
                    self.assertFalse(self.reconcile(selected)["changed"])
                    # A later public-port change also never reuses another port.
                    upgrade = self.reconcile(endpoint(first, 32123, secure))
                    self.assertTrue(upgrade["changed"])
                    self.assertEqual({item["port"] for item in self.cluster.state_ports().values()},
                                     {32123, 32011 if other == "iam" else 32010})

    def test_unrelated_ingress_stops_relocation_and_keeps_default_ports(self):
        self.cluster.live = [{"kind": "Ingress", "metadata": {"name": "foreign", "namespace": "other"}, "spec": {"rules": [{"host": "other.example"}]}}]
        with self.assertRaisesRegex(ports.Unsafe, "Unrelated"):
            self.reconcile()
        self.assertIsNone(self.cluster.config)
        result = self.reconcile(endpoint(number=80))
        self.assertTrue(result["isolated"])
        self.assertEqual({item["port"] for item in self.cluster.state_ports().values()}, {80, 443})

    def test_ingress_cas_conflict_keeps_hcc_and_operator_changes(self):
        for existing in (False, True):
            for race_field in ("resourceVersion", "uid"):
                with self.subTest(existing=existing, race_field=race_field):
                    self.cluster = FakeCluster(self.directory)
                    if existing:
                        self.cluster.config = {"metadata": {"resourceVersion": "7"}, "spec": {"failurePolicy": "abort"}}
                    self.cluster.install(objects("iam", 32011))
                    original_config = copy.deepcopy(self.cluster.config)
                    original = copy.deepcopy(self.cluster.live[-1])
                    run = self.cluster.run

                    def race(args, stdin=None):
                        if args[:3] == ["kubectl", "patch", "ingresses"]:
                            backups = list((self.directory / "backups").glob("before-*.json"))
                            self.assertTrue(any(original in json.loads(path.read_text())["ingresses"] for path in backups))
                            live = self.cluster.live[-1]
                            live["metadata"][race_field] = "operator-changed"
                            live["metadata"]["annotations"]["operator-race"] = "must-survive"
                        return run(args, stdin)

                    with patch.object(self.cluster, "run", side_effect=race):
                        with self.assertRaisesRegex(ports.Unsafe, "private original backup") as error:
                            self.reconcile()
                    self.assertTrue(Path(str(error.exception).split(": ")[-1]).is_file())
                    self.assertEqual(self.cluster.config, original_config)
                    self.assertEqual(self.cluster.live[-1]["metadata"]["annotations"]["operator-race"], "must-survive")
                    self.assertFalse(any(call[:2] == ("kubectl", "create") or call[:3] == ("kubectl", "patch", ports.RESOURCE)
                                         for call in self.cluster.calls))

    def test_all_originals_saved_before_first_patch_and_partial_failure_no_rollback(self):
        self.cluster.install(objects())
        self.cluster.install(objects("iam", 32011))
        original = copy.deepcopy(self.cluster.live)
        run = self.cluster.run
        patches = []

        def fail_second(args, stdin=None):
            if args[:3] == ["kubectl", "patch", "ingresses"]:
                snapshots = [json.loads(path.read_text()) for path in (self.directory / "backups").glob("*.json")]
                self.assertEqual(snapshots[0]["ingresses"], [obj for obj in original if obj["kind"] == "Ingress"])
                patches.append(args[3])
                if len(patches) == 2:
                    raise ports.Unsafe("Injected second patch failure")
            return run(args, stdin)

        with patch.object(self.cluster, "run", side_effect=fail_second):
            with self.assertRaisesRegex(ports.Unsafe, "no blind retry or rollback"):
                self.reconcile()
        self.assertEqual(patches, ["eva-app", "eva-iam"])
        self.assertIn(ports.ENTRY, self.cluster.live[1]["metadata"]["annotations"])
        self.assertEqual(self.cluster.live[-1], original[-1])
        self.assertIsNone(self.cluster.config)

    def test_ownership_mismatch_and_no_helm_history_refused_without_mutation(self):
        for kind in ("Ingress", "ConfigMap"):
            for field in ("release-name", "release-namespace", "managed-by", "namespace", "history"):
                with self.subTest(kind=kind, field=field):
                    self.cluster = FakeCluster(self.directory)
                    self.cluster.install(objects("iam", 32011))
                    obj = next(obj for obj in self.cluster.live if obj["kind"] == kind)
                    if field in ("release-name", "release-namespace"):
                        obj["metadata"]["annotations"]["meta.helm.sh/" + field] = "foreign"
                    elif field == "managed-by":
                        obj["metadata"]["labels"]["app.kubernetes.io/managed-by"] = "ArgoCD"
                    elif field == "namespace":
                        obj["metadata"]["namespace"] = "foreign"
                    else:
                        self.cluster.history.clear()
                    before = copy.deepcopy(self.cluster.live)
                    with self.assertRaises(ports.Unsafe):
                        self.reconcile()
                    self.assertIsNone(self.cluster.config)
                    self.assertEqual(before, self.cluster.live)

    def test_custom_entrypoint_refused_but_legacy_web_list_is_migrated(self):
        for entry in ("private-listener", "web,private-listener", "eva-h-32011", "web,websecure", None):
            with self.subTest(entry=entry):
                self.cluster = FakeCluster(self.directory)
                self.cluster.install(objects("iam", 32011))
                if entry is not None:
                    self.cluster.live[-1]["metadata"]["annotations"][ports.ENTRY] = entry
                if entry in ("web,websecure", None):
                    self.reconcile()
                    self.assertEqual(self.cluster.live[-1]["metadata"]["annotations"][ports.ENTRY], "eva-h-32011")
                else:
                    with self.assertRaisesRegex(ports.Unsafe, "foreign explicit entrypoint"):
                        self.reconcile()
                    self.assertIsNone(self.cluster.config)

    def test_old_endpoint_facts_and_history_drift_refused(self):
        for history in (False, True):
            self.cluster = FakeCluster(self.directory)
            first = self.reconcile()
            self.cluster.install(ports.isolate(objects(), first["endpoint"]))
            self.cluster.live[0]["data"]["BACKEND_PUBLIC_PORT"] = "32199"
            if history:
                self.cluster.history["eva-app"] = copy.deepcopy(self.cluster.live)
            before = copy.deepcopy(self.cluster.config)
            with self.assertRaisesRegex(ports.Unsafe, "endpoint|Endpoint"):
                self.reconcile(endpoint("iam", 32011))
            self.assertEqual(self.cluster.config, before)

    def test_current_upgrade_retry_before_helm_upgrade_is_not_a_new_deadlock(self):
        self.cluster.install(objects())
        self.cluster.install(objects("iam", 32011))
        first = self.reconcile()
        self.cluster.install(ports.isolate(objects(), first["endpoint"]))
        desired = endpoint(number=32123)
        upgrade = self.reconcile(desired)
        self.assertTrue(upgrade["changed"])
        # The subsequent App Helm upgrade did not run (e.g. DB preflight failed).
        # Current release may retry its exact requested endpoint; the OTHER
        # release must still match its installed endpoint/history.
        retry = self.reconcile(desired)
        self.assertFalse(retry["changed"])
        self.assertEqual(retry["endpoint"], upgrade["endpoint"])
        app = next(obj for obj in self.cluster.live if obj["kind"] == "ConfigMap" and obj["metadata"]["namespace"] == "eva-app")
        self.assertEqual(app["data"]["BACKEND_PUBLIC_PORT"], "32010")

    def test_snapshot_failure_before_mutations(self):
        self.cluster.install(objects("iam", 32011))
        before = copy.deepcopy(self.cluster.live)
        with patch.object(ports.os, "fsync", side_effect=OSError("private-file-value")):
            with self.assertRaises(OSError):
                self.reconcile()
        self.assertEqual(before, self.cluster.live)
        self.assertIsNone(self.cluster.config)

    def test_endpoint_configmap_race_stops_before_hcc_without_rollback(self):
        for after_patch in (False, True):
            with self.subTest(after_patch=after_patch):
                self.cluster = FakeCluster(self.directory)
                self.cluster.install(objects("iam", 32011))
                run = self.cluster.run
                snapshot = ports.snapshot

                def changed():
                    self.cluster.live[0]["data"]["ISSUER"] = "http://10.159.56.197:32199/realms/eva-iam"

                def after_snapshot(*args):
                    result = snapshot(*args)
                    if not after_patch:
                        changed()
                    return result

                def after_router(args, stdin=None):
                    result = run(args, stdin)
                    if after_patch and args[:3] == ["kubectl", "patch", "ingresses"]:
                        changed()
                    return result

                with patch.object(ports, "snapshot", side_effect=after_snapshot), patch.object(self.cluster, "run", side_effect=after_router):
                    with self.assertRaisesRegex(ports.Unsafe, "private original backup"):
                        self.reconcile()
                self.assertIsNone(self.cluster.config)
                self.assertIn(":32199/", self.cluster.live[0]["data"]["ISSUER"])
                self.assertEqual(ports.ENTRY in self.cluster.live[-1]["metadata"]["annotations"], after_patch)

    def test_operator_ingress_edit_after_cas_prevents_hcc_without_clobber(self):
        self.cluster.install(objects("iam", 32011))
        run = self.cluster.run

        def operator_edit(args, stdin=None):
            result = run(args, stdin)
            if args[:3] == ["kubectl", "patch", "ingresses"]:
                self.cluster.live[-1]["metadata"]["annotations"]["operator-after-cas"] = "preserve"
            return result

        with patch.object(self.cluster, "run", side_effect=operator_edit):
            with self.assertRaisesRegex(ports.Unsafe, "no blind retry or rollback"):
                self.reconcile()
        self.assertIsNone(self.cluster.config)
        self.assertEqual(self.cluster.live[-1]["metadata"]["annotations"]["operator-after-cas"], "preserve")

    def test_k3s_safe_spec_set_and_failure_policy_preserved(self):
        self.cluster.chart["spec"]["set"] = {"global.systemDefaultRegistry": "registry.internal", "global.clusterCIDR": "10.42.0.0/16"}
        self.cluster.config = {"metadata": {"resourceVersion": "7"}, "spec": {"failurePolicy": "abort"}}
        self.reconcile()
        self.assertEqual(self.cluster.config["spec"]["failurePolicy"], "abort")
        self.assertEqual(self.cluster.chart["spec"]["set"]["global.clusterCIDR"], "10.42.0.0/16")

    def test_unrelated_ingress_unchanged_when_shared_defaults_preserved(self):
        foreign = {"kind": "Ingress", "metadata": {"name": "foreign", "namespace": "other", "annotations": {"private": "unchanged"}},
                   "spec": {"rules": [{"host": "other.example"}]}}
        self.cluster.live = [foreign]
        before = copy.deepcopy(foreign)
        self.reconcile(endpoint(number=80))
        self.assertEqual(self.cluster.live, [before])
        self.assertFalse(any(call[:3] == ("kubectl", "patch", "ingresses") for call in self.cluster.calls))

    def test_error_boundary_never_prints_private_exception_or_command_values(self):
        secret = "PRIVATE-YAML-COMMAND-SECRET"
        for error in (ValueError(secret), KeyError(secret), TypeError(secret), OSError(secret),
                      RuntimeError(secret), yaml.YAMLError(secret), subprocess.TimeoutExpired(["helm", secret], 1)):
            with self.subTest(error=type(error).__name__), patch.object(ports, "main", side_effect=error):
                stdout, stderr = io.StringIO(), io.StringIO()
                with patch.object(sys, "stdout", stdout), patch.object(sys, "stderr", stderr):
                    self.assertEqual(ports.cli(), 1)
                self.assertEqual(stdout.getvalue(), "")
                self.assertNotIn(secret, stderr.getvalue())
                self.assertNotIn("Traceback", stderr.getvalue())
        with patch.object(ports.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, stdout=secret, stderr=secret)):
            with self.assertRaises(ports.Unsafe) as caught:
                ports.Cluster().run(["helm", "template", secret, "--set", secret])
            self.assertNotIn(secret, str(caught.exception))

    def test_failed_actual_role_keeps_safe_error_not_from_json_failure(self):
        self.run_ansible("app", "helper-failure")

    def test_override_and_unknown_schema_guards(self):
        for override in ({"set": {"ports.web.exposedPort": "80"}}, {"set": {"ports": "foreign"}}, {"valuesSecrets": [{"name": "secret"}]}):
            with self.subTest(override=override):
                chart = ports.merge(self.cluster.chart, {"spec": override})
                with self.assertRaises(ports.Unsafe):
                    ports.plan(defaults(), chart, None, {"app": endpoint()}, [])
        unknown = defaults()
        unknown["ports"]["web"]["expose"] = "true"
        with self.assertRaisesRegex(ports.Unsafe, "schema"):
            ports.plan(unknown, self.cluster.chart, None, {"app": endpoint()}, [])
        for field in ("hostPort", "nodePort"):
            values = defaults()
            values["ports"]["web"][field] = 80
            with self.assertRaises(ports.Unsafe):
                ports.plan(values, self.cluster.chart, None, {"app": endpoint()}, [])

    def test_legacy_boolean_expose(self):
        self.cluster = FakeCluster(self.directory, modern=False)
        self.reconcile()
        config = yaml.safe_load(self.cluster.config["spec"]["valuesContent"])
        self.assertIs(config["ports"]["web"]["expose"], True)
        self.assertIs(config["ports"]["websecure"]["expose"], False)

    def test_stale_ready_controller_or_pods_never_succeed(self):
        for field in ("stale", "stale_pod", "wrong_target", "bad_args", "bad_owner", "pod_args_stale",
                  "pod_not_ready", "pod_foreign_owner", "extra_args", "duplicate_args"):
            with self.subTest(field=field):
                self.cluster = FakeCluster(self.directory)
                setattr(self.cluster, field, True)
                with self.assertRaisesRegex(ports.Unsafe, "Timeout"):
                    self.reconcile()

    def test_controller_errors_are_explicit(self):
        self.cluster.failed = True
        with self.assertRaisesRegex(ports.Unsafe, "controller failed"):
            self.reconcile()

    def test_nodeport_external_lb_and_custom_chart_refused(self):
        self.cluster.service_type = "NodePort"
        with self.assertRaisesRegex(ports.Unsafe, "NodePort"):
            self.reconcile()
        self.assertIsNone(self.cluster.config)
        self.cluster.service_type = "LoadBalancer"
        self.cluster.no_lb = True
        with self.assertRaisesRegex(ports.Unsafe, "external LB"):
            self.reconcile()
        with self.assertRaisesRegex(ports.Unsafe, "remote/custom"):
            ports.chart_defaults({"metadata": self.cluster.chart["metadata"], "spec": {"chart": "https://foreign/chart.tgz"}})

    def test_endpoint_uses_public_configmap_not_backend_service_port(self):
        self.assertEqual(endpoint()["port"], 32010)
        self.assertEqual(endpoint(secure=True)["scheme"], "https")
        manifest = objects()
        del manifest[0]["data"]["BACKEND_PUBLIC_PORT"]
        with self.assertRaisesRegex(ports.Unsafe, "public port"):
            ports.endpoint(manifest, "app", "eva-app", "eva-app")
        self.assertIsNone(ports.endpoint(objects(host="app.example"), "app", "eva-app", "eva-app"))
        self.assertIsNone(ports.endpoint(objects()[:1], "app", "eva-app", "eva-app"))
        manifest = objects(secure=True)
        manifest[-1]["spec"].pop("tls")
        with self.assertRaisesRegex(ports.Unsafe, "TLS disagree"):
            ports.endpoint(manifest, "app", "eva-app", "eva-app")

    def test_postrenderer_overrides_cli_annotations_last_preserves_other_objects(self):
        manifest = objects("iam")
        manifest[-1]["metadata"]["annotations"].update({ports.ENTRY: "wrong", ports.TLS: "true", "keep": "yes"})
        selected = dict(endpoint("iam"), entrypoint="eva-h-32010")
        rendered = ports.isolate(manifest, selected)
        self.assertEqual(rendered[-1]["metadata"]["annotations"][ports.ENTRY], "eva-h-32010")
        self.assertEqual(rendered[-1]["metadata"]["annotations"][ports.TLS], "false")
        self.assertEqual(rendered[0], manifest[0])
        self.assertEqual(rendered[-1]["metadata"]["annotations"]["keep"], "yes")

    def test_actual_ansible_app_iam_callers_dns_ip_and_opt_out(self):
        for component in ("app", "iam"):
            for case in ("dns", "ip", "opt-out"):
                with self.subTest(component=component, case=case):
                    self.run_ansible(component, case)

    def run_ansible(self, component, case):
        directory = self.directory / (component + "-" + case)
        directory.mkdir()
        binary = directory / "bin"
        binary.mkdir()
        # This process handles kubectl only through the serialized fake cluster.
        kubectl = binary / "kubectl"
        kubectl.write_text(f"#!{sys.executable}\nimport runpy, sys\nsys.argv = [{str(Path(__file__).resolve())!r}, '--fake-kubectl'] + sys.argv[1:]\nrunpy.run_path(sys.argv[0], run_name='__main__')\n")
        kubectl.chmod(0o700)
        fake = FakeCluster(directory)
        fake.bad_args = case == "helper-failure"
        state = directory / "cluster.json"
        state.write_text(json.dumps(fake.__dict__))
        chart = directory / "chart"
        (chart / "templates").mkdir(parents=True)
        (chart / "Chart.yaml").write_text("apiVersion: v2\nname: eva-fixture\nversion: 1.0.0\n")
        (chart / "values.yaml").write_text("{}\n")
        manifest = objects(component, host="app.example" if case == "dns" else "10.159.56.197")
        (chart / "templates/resources.yaml").write_text(yaml.safe_dump_all(manifest))
        (directory / "values.yaml").write_text("{}\n")
        (directory / "eva-iam-k3s-override.yaml").write_text("{}\n")
        source = ROOT / f"src/solution/roles/eva_{component}/tasks/main.yaml"
        tasks = yaml.safe_load(source.read_text())
        caller = next(task for task in tasks if task.get("ansible.builtin.include_role", {}).get("name") == "eva_traefik_ports")
        result = next(task for task in tasks if "eva_" + component + "_traefik_args" in task.get("ansible.builtin.set_fact", {}))
        variables = {"eva_traefik_ports_manage": case != "opt-out", "eva_traefik_ports_backup_root": str(directory / "backup"),
                     "eva_traefik_ports_timeout": 0, "eva_cli_chart_path": "override", "eva_cli_helm_set": [], "repository_mode": "cloud_repository",
                     f"eva_{component}_namespace": "eva-" + component, f"eva_{component}_name": "eva-" + component,
                     f"eva_{component}_workdir": str(directory), f"eva_{component}_chart_source": str(chart),
                     f"eva_{component}_chart_version": "1.0.0", f"eva_{component}_kubeconfig": str(directory / "absent-kubeconfig"),
                     "eva_app_effective_values_dest": str(directory / "values.yaml"), "eva_iam_chart_version_arg": "",
                     "eva_iam_app_sso_base_url": "http://10.159.56.197:32010",
                     "eva_iam_effective_user_values": {}, "eva_iam_values_dest": str(directory / "values.yaml")}
        assertion = {"ansible.builtin.assert": {"that": ["eva_traefik_ports_result.isolated == " + ("true" if case == "ip" else "false")]}}
        play = [{"hosts": "localhost", "gather_facts": False, "vars": variables, "tasks": [caller, result, assertion],
                 "environment": {"PATH": str(binary) + ":" + os.environ["PATH"], "FAKE_TRAEFIK_STATE": str(state)}}]
        playbook = directory / "play.yaml"
        playbook.write_text(yaml.safe_dump(play))
        process = subprocess.run(["ansible-playbook", "-i", "localhost,", "-c", "local", str(playbook)],
                                 env=dict(os.environ, ANSIBLE_ROLES_PATH=str(ROLE.parent), ANSIBLE_NOCOLOR="1"), capture_output=True, text=True, timeout=90)
        if case == "helper-failure":
            self.assertNotEqual(process.returncode, 0)
            self.assertIn("Timeout waiting for exact Traefik", process.stdout + process.stderr)
            self.assertNotIn("from_json", process.stdout + process.stderr)
            self.assertNotIn("Expecting value", process.stdout + process.stderr)
            self.assertIn('"changed": false', process.stdout)
            return
        self.assertEqual(process.returncode, 0, process.stdout + process.stderr)
        saved = json.loads(state.read_text())
        self.assertEqual(saved["config"] is not None, case == "ip")


class PublishedChartTests(unittest.TestCase):
    """Optional real archived-chart proof; missing archives are explicit skips."""
    def test_exact_offline_charts_and_installed_helm_postrenderer(self):
        charts = {
            "app": Path(os.environ.get("EVA_TEST_APP_CHART", "/mnt/storage/home/sunghyuns.park/eva-app-3.3.0-beta.3-airgap/charts/eva-app-3.3.0-beta.3.tgz")),
            "iam": Path(os.environ.get("EVA_TEST_IAM_CHART", "/mnt/storage/home/sunghyuns.park/eva-app-iam-3.3.0-beta.1-airgap/charts/eva-iam-3.3.0-beta.1.tgz")),
        }
        if not all(path.is_file() for path in charts.values()) or not shutil.which("helm"):
            self.skipTest("Provide EVA_TEST_APP_CHART/EVA_TEST_IAM_CHART and helm for real chart proof")
        for component, chart in charts.items():
            for secure in (False, True):
                with self.subTest(component=component, secure=secure), tempfile.TemporaryDirectory() as temp:
                    directory = Path(temp)
                    values = ({"app": {"backendHost": "10.159.56.197", "backendSecure": secure, "backendPublicPort": 12345, "backendSecurePort": 12346}, "service": {"port": 9999}}
                              if component == "app" else {"config": {"host": "10.159.56.197", "publicPort": "12345"}})
                    values["ingress"] = {"tls": {"enabled": secure}}
                    path = directory / "values.yaml"
                    path.write_text(yaml.safe_dump(values))
                    site = directory / "site.yaml"
                    site.write_text(yaml.safe_dump({"app": {"backendPublicPort": 23456, "backendSecurePort": 23457}}
                                                   if component == "app" else {"config": {"publicPort": "23456"}}))
                    cli = "app.backendSecurePort=32443" if component == "app" and secure else "app.backendPublicPort=32010" if component == "app" else "config.publicPort=32443" if secure else "config.publicPort=32010"
                    command = ["helm", "template", "eva-" + component, str(chart), "-n", "eva-" + component, "-f", str(path), "-f", str(site)]
                    site_render = subprocess.run(command, check=True, text=True, capture_output=True).stdout
                    site_endpoint = ports.endpoint(ports.documents(site_render), component, "eva-" + component, "eva-" + component)
                    self.assertEqual(site_endpoint["port"], 23457 if component == "app" and secure else 23456)
                    command += ["--set", cli]
                    rendered = subprocess.run(command, check=True, text=True, capture_output=True).stdout
                    selected = ports.endpoint(ports.documents(rendered), component, "eva-" + component, "eva-" + component)
                    self.assertEqual(selected["port"], 32443 if secure else 32010)
                    tls = ports.tls_job_input(ports.documents(rendered), selected["ingress"])
                    self.assertEqual(tls["tls_job_enabled"], secure)
                    self.assertEqual(bool(tls["tls_host_path"]), secure)
                    selected["entrypoint"] = "websecure" if secure else "web"
                    router = directory / "router.json"
                    router.write_text(json.dumps(selected))
                    wrapper = directory / "traefik-post-renderer"
                    wrapper.write_text(f'#!/bin/sh\nexec "{sys.executable}" "{HELPER}" --post-render "$1"\n')
                    wrapper.chmod(0o700)
                    renderer, env = ports.prepare_renderer(ports.Cluster(), directory)
                    installed = subprocess.run(command + ["--post-renderer", renderer, "--post-renderer-args", str(router)],
                                               env=dict(os.environ, **env), check=True, capture_output=True, text=True).stdout
                    ingress = next(obj for obj in ports.documents(installed) if obj.get("kind") == "Ingress")
                    self.assertEqual(ingress["metadata"]["annotations"][ports.ENTRY], selected["entrypoint"])
                    self.assertEqual(ingress["metadata"]["annotations"][ports.TLS], str(secure).lower())


def fake_kubectl():
    path = Path(os.environ["FAKE_TRAEFIK_STATE"])
    cluster = FakeCluster(path.parent)
    cluster.__dict__.update(json.loads(path.read_text()))
    args = sys.argv[2:]
    if args[0] == "get":
        namespace = None if "-A" in args else args[args.index("-n") + 1]
        name = args[2] if len(args) > 2 and not args[2].startswith("-") else None
        result = cluster.get(args[1], namespace, name, "--ignore-not-found=true" in args)
        if result is not None:
            print(json.dumps(result))
    else:
        print(cluster.run(["kubectl"] + args, sys.stdin.read() if "-f" in args or "--patch-file=/dev/stdin" in args else None))
    path.write_text(json.dumps(cluster.__dict__))


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--fake-kubectl":
        fake_kubectl()
    else:
        unittest.main(verbosity=2)
