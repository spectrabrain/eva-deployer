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
TRAEFIK_CHARTS = [Path(path) for path in os.environ.get("EVA_TEST_TRAEFIK_CHARTS", "").split(os.pathsep) if path]


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
    # Exercise actual schemas and Helm merge semantics, not just fake rendering.
    for version, checksum in (
        ("27.0.2", "271298d9b089b502eeb93552034ee6767b3529af57c8f9e60665c3a61c15c974"),
        ("34.4.1", "8c3ec98f82eb38d84aa330e2e4b067f2889595b566e130ab4bb7545d51978015"),
        ("40.1.0", "328e20a46bf2f0546c0761f0df3c6bd5b99c2ada180be4d3e18287d14e90d1fc"),
    ):
        with urlopen(f"https://traefik.github.io/charts/traefik/traefik-{version}.tgz", timeout=120) as response:
            data = response.read()
        if hashlib.sha256(data).hexdigest() != checksum:
            raise AssertionError("Traefik chart archive checksum mismatch")
        archive = Path(temporary.name) / f"traefik-{version}.tgz"
        archive.write_bytes(data)
        TRAEFIK_CHARTS.append(archive)


def defaults(modern=True):
    return {"ports": {
        "traefik": {"port": 9000, "expose": {"default": False} if modern else False},
        "web": {"port": 8000, "exposedPort": 80, "expose": {"default": True} if modern else True,
                **({"redirections": {"entryPoint": {}}} if modern else {})},
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
        self.stale_redirect = False
        self.stale_pod_redirect = False

    def install(self, manifest):
        name = manifest[-1]["metadata"]["namespace"]
        self.live = [obj for obj in self.live if obj["metadata"].get("namespace") != name] + copy.deepcopy(manifest)
        self.history[name] = copy.deepcopy(manifest)

    def effective_values(self):
        base = yaml.safe_load(self.chart.get("spec", {}).get("valuesContent", "")) or {}
        overrides = yaml.safe_load((self.config or {}).get("spec", {}).get("valuesContent", "")) or {}
        return ports.merge(ports.merge(self.defaults, base), overrides)

    def controller_args(self):
        effective = self.effective_values()
        args = [f"--entrypoints.{name}.address=:{item['port']}" for name, item in effective["ports"].items()]
        for name, item in effective["ports"].items():
            redirects = [item.get("redirectTo"), (item.get("redirections") or {}).get("entryPoint"),
                         ((item.get("http") or {}).get("redirections") or {}).get("entryPoint")]
            if any(redirects):
                args.append(f"--entryPoints.{name}.http.redirections.entryPoint.to=:443")
        args += effective.get("additionalArguments", []) + effective.get("globalArguments", [])
        if self.stale_redirect:
            args.append("--entryPoints.web.http.redirections.entryPoint.to=:443")
        if self.extra_args:
            args.append("--entrypoints.foreign.address=:12345")
        if self.duplicate_args:
            args.append(args[0])
        return [] if self.bad_args else args

    def state_ports(self):
        effective = self.effective_values()
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
                               "spec": {"containers": [{"name": "traefik", "args": [] if self.pod_args_stale else self.controller_args() + (
                                   ["--entryPoints.web.http.redirections.entryPoint.to=:443"] if self.stale_pod_redirect else [])}]},
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
                    key = op["path"].split("/")[-1].replace("~1", "/").replace("~0", "~")
                    assert key in (ports.STATE, ports.OWNED)
                    self.config["metadata"].setdefault("annotations", {})[key] = op["value"]
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

    def test_owned_listener_can_be_reused_after_port_change(self):
        for secure in (False, True):
            for modern in (False, True):
                with self.subTest(secure=secure, modern=modern):
                    self.cluster = FakeCluster(self.directory, modern)
                    self.cluster.install(objects(secure=secure))
                    self.cluster.install(objects("iam", 32011, secure))
                    first = self.reconcile(endpoint(secure=secure))
                    self.cluster.install(ports.isolate(objects(secure=secure), first["endpoint"]))
                    original = self.reconcile(endpoint("iam", 32011, secure))["endpoint"]
                    # Migrate an existing installation with only the old endpoint annotation.
                    self.cluster.config["metadata"]["annotations"].pop(ports.OWNED)
                    changed = self.reconcile(endpoint("iam", 32111, secure))
                    self.cluster.install(ports.isolate(objects("iam", 32111, secure), changed["endpoint"]))
                    self.assertIn(original["entrypoint"], json.loads(self.cluster.config["metadata"]["annotations"][ports.OWNED]))
                    self.assertFalse(ports.enabled(self.cluster.effective_values()["ports"][original["entrypoint"]]))
                    restored = self.reconcile(endpoint("iam", 32011, secure))
                    self.assertEqual(restored["endpoint"], original)
                    self.assertEqual({item["port"] for item in self.cluster.state_ports().values()}, {32010, 32011})
                    self.assertFalse(self.reconcile(endpoint("iam", 32011, secure))["changed"])

    def test_owned_http_listener_survives_https_switch_and_reinstall(self):
        self.cluster.install(objects())
        self.cluster.install(objects("iam", 32011))
        first = self.reconcile()
        self.cluster.install(ports.isolate(objects(), first["endpoint"]))
        original = first["endpoint"]["entrypoint"]
        for component, path in (("app", "/"), ("iam", "/iam")):
            changed = self.reconcile(endpoint(component, 443, True, path))
            self.cluster.install(ports.isolate(objects(component, 443, True, path=path), changed["endpoint"]))
        state = json.loads(self.cluster.config["metadata"]["annotations"][ports.STATE])
        self.assertTrue(all(item["entrypoint"] == "websecure" for item in state.values()))
        self.assertFalse(ports.enabled(self.cluster.effective_values()["ports"][original]))
        # Deleting releases drops endpoint state on the next reconciliation,
        # but must not orphan their retained port definitions.
        self.cluster.live.clear()
        self.cluster.history.clear()
        installed = self.reconcile(endpoint("iam", 443, True, "/iam"))
        self.cluster.install(ports.isolate(objects("iam", 443, True, path="/iam"), installed["endpoint"]))
        restored = self.reconcile()
        self.assertEqual(restored["endpoint"]["entrypoint"], original)
        self.assertEqual({item["port"] for item in self.cluster.state_ports().values()}, {32010, 443})
        self.assertFalse(self.reconcile()["changed"])

    def test_eva_listener_without_ownership_is_recovered(self):
        for modern in (False, True):
            for secure in (False, True):
                for exposed in (False, True):
                    for source in ("chart", "config"):
                        with self.subTest(modern=modern, secure=secure, exposed=exposed, source=source):
                            self.cluster = FakeCluster(self.directory, modern)
                            name = f"eva-{'s' if secure else 'h'}-32010"
                            values = {"resources": {"limits": {"memory": "384Mi"}}, "ports": {
                                name: {"port": 32010, "exposedPort": 32010,
                                       "expose": {"default": exposed} if modern else exposed},
                                "private-listener": {"port": 12345, "expose": {"default": False} if modern else False}}}
                            self.cluster.install(objects("iam", 443, True, path="/iam"))
                            if source == "chart":
                                self.cluster.chart["spec"]["valuesContent"] = yaml.safe_dump(values)
                            else:
                                self.cluster.config = {"metadata": {"resourceVersion": "1", "annotations": {"operator": "keep"}},
                                                       "spec": {"valuesContent": yaml.safe_dump(values)}}
                            before = copy.deepcopy(self.cluster.config)
                            chart = copy.deepcopy(self.cluster.chart)
                            desired = endpoint(secure=secure)
                            result = self.reconcile(desired)
                            self.assertEqual(result["endpoint"]["entrypoint"], name)
                            self.assertEqual({item["port"] for item in self.cluster.state_ports().values()}, {32010, 443})
                            self.assertEqual(json.loads(self.cluster.config["metadata"]["annotations"][ports.OWNED]), [name])
                            effective = self.cluster.effective_values()
                            self.assertEqual(effective["resources"], values["resources"])
                            self.assertEqual(effective["ports"]["private-listener"], values["ports"]["private-listener"])
                            self.assertEqual(json.loads(Path(result["backup"]).read_text())["helmchartconfig"], before)
                            self.assertEqual(self.cluster.chart, chart)
                            if source == "config":
                                self.assertEqual(self.cluster.config["metadata"]["annotations"]["operator"], "keep")
                            self.assertFalse(self.reconcile(desired)["changed"])

    def test_reported_https_state_with_orphan_http_listener_installs_without_cleanup(self):
        state = {}
        for component, path in (("app", "/"), ("iam", "/iam")):
            selected = dict(endpoint(component, 443, True, path), entrypoint="websecure")
            self.cluster.install(ports.isolate(objects(component, 443, True, path=path), selected))
            state[f"eva-{component}/eva-{component}"] = selected
        self.cluster.config = {"metadata": {"resourceVersion": "1", "annotations": {ports.STATE: json.dumps(state)}},
                               "spec": {"valuesContent": yaml.safe_dump({"ports": {
                                   "eva-h-32010": {"port": 32010, "exposedPort": 32010, "expose": {"default": False}},
                                   "web": {"exposedPort": 80, "expose": {"default": False}},
                                   "websecure": {"exposedPort": 443, "expose": {"default": True}}}})}}
        before = copy.deepcopy(self.cluster.config)
        app = copy.deepcopy([obj for obj in self.cluster.live if obj["metadata"]["namespace"] == "eva-app"])
        desired = endpoint("iam", 32010, path="/iam")
        result = self.reconcile(desired)
        self.assertEqual(result["endpoint"]["entrypoint"], "eva-h-32010")
        self.assertEqual({item["port"] for item in self.cluster.state_ports().values()}, {32010, 443})
        self.assertEqual([obj for obj in self.cluster.live if obj["metadata"]["namespace"] == "eva-app"], app)
        self.assertEqual(json.loads(Path(result["backup"]).read_text())["helmchartconfig"], before)
        self.assertIn("eva-h-32010", json.loads(self.cluster.config["metadata"]["annotations"][ports.OWNED]))
        self.assertFalse(self.reconcile(desired)["changed"])

    def test_missing_endpoint_and_listener_records_restore_installed_eva_routers(self):
        for secure in (False, True):
            with self.subTest(secure=secure):
                self.cluster = FakeCluster(self.directory)
                for component, number in (("app", 32010), ("iam", 32011)):
                    self.cluster.install(objects(component, number, secure))
                first = self.reconcile(endpoint(secure=secure))
                self.cluster.install(ports.isolate(objects(secure=secure), first["endpoint"]))
                second = self.reconcile(endpoint("iam", 32011, secure))
                self.cluster.install(ports.isolate(objects("iam", 32011, secure), second["endpoint"]))
                self.cluster.config["metadata"]["annotations"].pop(ports.STATE)
                self.cluster.config["metadata"]["annotations"].pop(ports.OWNED)
                live = copy.deepcopy(self.cluster.live)
                restored = self.reconcile(endpoint("iam", 32011, secure))
                self.assertEqual(restored["endpoint"], second["endpoint"])
                self.assertEqual(self.cluster.live, live)
                self.assertEqual({item["port"] for item in self.cluster.state_ports().values()}, {32010, 32011})
                self.assertEqual(len(json.loads(self.cluster.config["metadata"]["annotations"][ports.OWNED])), 2)
                self.assertFalse(self.reconcile(endpoint("iam", 32011, secure))["changed"])

    def test_unrecorded_extra_listener_is_disabled_when_primary_is_reused(self):
        for source in ("chart", "config"):
            with self.subTest(source=source):
                self.cluster = FakeCluster(self.directory)
                values = {"ports": {"eva-h-32010": {"port": 32010, "exposedPort": 32010, "expose": {"default": True}}}}
                if source == "chart":
                    self.cluster.chart["spec"]["valuesContent"] = yaml.safe_dump(values)
                else:
                    self.cluster.config = {"metadata": {"resourceVersion": "1"}, "spec": {"valuesContent": yaml.safe_dump(values)}}
                result = self.reconcile()
                self.assertEqual(result["endpoint"]["entrypoint"], "web")
                self.assertEqual(set(self.cluster.state_ports()), {"web"})
                self.assertFalse(ports.enabled(self.cluster.effective_values()["ports"]["eva-h-32010"]))
                self.assertIn("eva-h-32010", json.loads(self.cluster.config["metadata"]["annotations"][ports.OWNED]))
                self.assertFalse(self.reconcile()["changed"])

    def test_recovered_http_listeners_clear_redirects_without_ownership_records(self):
        self.cluster.install(objects("iam", 443, True, path="/iam"))
        redirect = {"http": {"redirections": {"entryPoint": {"to": "websecure", "scheme": "https"}}}}
        values = {"ports": {f"eva-h-{number}": {"port": number, "exposedPort": number,
                                               "expose": {"default": False}, **redirect} for number in (32010, 32011)},
                  "additionalArguments": ["--entryPoints.eva-h-32011.http.redirections.entryPoint.to=websecure"]}
        self.cluster.config = {"metadata": {"resourceVersion": "1"}, "spec": {"valuesContent": yaml.safe_dump(values)}}
        self.reconcile()
        effective = self.cluster.effective_values()
        self.assertFalse(ports.check_redirects(effective["ports"]["eva-h-32010"]))
        self.assertFalse(ports.check_redirects(effective["ports"]["eva-h-32011"]))
        self.assertFalse(ports.enabled(effective["ports"]["eva-h-32011"]))
        self.assertFalse(any(".http.redirections." in arg.lower() for arg in self.cluster.controller_args()))
        self.assertEqual(json.loads(self.cluster.config["metadata"]["annotations"][ports.OWNED]), ["eva-h-32010", "eva-h-32011"])
        self.assertFalse(self.reconcile()["changed"])

    def test_incompatible_eva_listener_configuration_is_not_overwritten(self):
        for incompatible in ({"port": 32123}, {"exposedPort": 32123}, {"protocol": "UDP"}, {"targetPort": "foreign"}):
            with self.subTest(incompatible=incompatible):
                self.cluster = FakeCluster(self.directory)
                self.cluster.install(objects("iam", 443, True, path="/iam"))
                listener = {"port": 32010, "exposedPort": 32010, "expose": {"default": False}, **incompatible}
                self.cluster.config = {"metadata": {"resourceVersion": "1"},
                                       "spec": {"valuesContent": yaml.safe_dump({"ports": {"eva-h-32010": listener}})}}
                before = copy.deepcopy(self.cluster.config)
                live = copy.deepcopy(self.cluster.live)
                with self.assertRaisesRegex(ports.Unsafe, "eva-h-32010 has incompatible listener configuration"):
                    self.reconcile()
                self.assertEqual(self.cluster.config, before)
                self.assertEqual(self.cluster.live, live)
                self.assertFalse(any(call[:2] in (("kubectl", "patch"), ("kubectl", "create")) for call in self.cluster.calls))

    def test_unrelated_router_prevents_automatic_listener_recovery(self):
        for number in (80, 32010):
            with self.subTest(number=number):
                self.cluster = FakeCluster(self.directory)
                self.cluster.live = [{"kind": "Ingress", "metadata": {"name": "foreign", "namespace": "other"},
                                      "spec": {"rules": [{"host": "other.example"}]}}]
                listener = {"port": 32010, "exposedPort": 32010, "expose": {"default": False}}
                self.cluster.config = {"metadata": {"resourceVersion": "1"},
                                       "spec": {"valuesContent": yaml.safe_dump({"ports": {"eva-h-32010": listener}})}}
                before = copy.deepcopy(self.cluster.config)
                if number == 80:
                    self.reconcile(endpoint(number=number))
                    self.assertEqual(json.loads(self.cluster.config["metadata"]["annotations"][ports.OWNED]), [])
                    self.assertEqual(self.cluster.effective_values()["ports"]["eva-h-32010"], listener)
                else:
                    with self.assertRaisesRegex(ports.Unsafe, "Unrelated Traefik ingresses prevent relocation"):
                        self.reconcile()
                    self.assertEqual(self.cluster.config, before)
                    self.assertFalse(any(call[:2] in (("kubectl", "patch"), ("kubectl", "create")) for call in self.cluster.calls))

    def test_invalid_entrypoint_ownership_refused_before_mutation(self):
        for recorded in ({}, ["web"], ["foreign"], [123], ["eva-h-invalid"]):
            with self.subTest(recorded=recorded):
                self.cluster = FakeCluster(self.directory)
                self.cluster.config = {"metadata": {"resourceVersion": "1", "annotations": {ports.OWNED: json.dumps(recorded)}},
                                       "spec": {}}
                before = copy.deepcopy(self.cluster.config)
                with self.assertRaisesRegex(ports.Unsafe, "Invalid EVA entrypoint ownership annotation"):
                    self.reconcile()
                self.assertEqual(self.cluster.config, before)
                self.assertFalse(any(call[:2] in (("kubectl", "patch"), ("kubectl", "create")) for call in self.cluster.calls))

    def test_ownership_annotation_change_during_convergence_is_not_overwritten(self):
        self.cluster.install(objects())
        self.cluster.install(objects("iam", 32011))
        original_get = self.cluster.get

        def operator_edit(resource, namespace="kube-system", name=None, optional=False):
            if resource == ports.RESOURCE and self.cluster.config:
                self.cluster.config["metadata"]["annotations"][ports.OWNED] = "[]"
            return original_get(resource, namespace, name, optional)

        with patch.object(self.cluster, "get", side_effect=operator_edit):
            with self.assertRaisesRegex(ports.Unsafe, "HelmChartConfig changed during controller convergence"):
                self.reconcile()
        self.assertEqual(self.cluster.config["metadata"]["annotations"][ports.OWNED], "[]")
        self.assertTrue(list((self.directory / "backups").glob("before-*.json")))

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
        for entry in ("private-listener", "web,private-listener", "eva-h-32111", "eva-s-32011", "eva-h-32011", "web,websecure", None):
            with self.subTest(entry=entry):
                self.cluster = FakeCluster(self.directory)
                self.cluster.install(objects("iam", 32011))
                if entry is not None:
                    self.cluster.live[-1]["metadata"]["annotations"][ports.ENTRY] = entry
                if entry in ("eva-h-32011", "web,websecure", None):
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

    def test_empty_redirect_defaults_are_inactive_and_preserved(self):
        # Official Traefik chart v34 uses ports.web.redirections.entryPoint;
        # v40 nests it under http. Both gate argument generation on the inner
        # entryPoint map, not the truthiness of its outer redirections map.
        idle_values = [
            {"redirectTo": None}, {"redirectTo": ""}, {"redirectTo": {}},
            {"redirections": None}, {"redirections": {}},
            {"redirections": {"entryPoint": None}},
            {"redirections": {"entryPoint": {}}},
            {"http": None}, {"http": {}},
            {"http": {"redirections": None}},
            {"http": {"redirections": {}}},
            {"http": {"redirections": {"entryPoint": None}}},
            {"http": {"redirections": {"entryPoint": {}}}},
        ]
        for source in ("defaults", "chart", "config"):
            for idle in idle_values:
                with self.subTest(source=source, idle=idle):
                    self.cluster = FakeCluster(self.directory)
                    values = {"ports": {"web": idle}}
                    if source == "defaults":
                        # Embedded archive is read by reconcile, not the fake
                        # controller defaults. Supply the same parsed defaults.
                        self.cluster.defaults = ports.merge(self.cluster.defaults, values)
                    elif source == "chart":
                        self.cluster.chart["spec"]["valuesContent"] = yaml.safe_dump(values)
                    else:
                        self.cluster.config = {"metadata": {"resourceVersion": "7"},
                                               "spec": {"valuesContent": yaml.safe_dump(values)}}
                    before_chart = copy.deepcopy(self.cluster.chart)
                    planned = endpoint("iam", 32011)
                    result = ports.reconcile(self.cluster, self.request, planned, self.cluster.defaults)
                    self.assertTrue(result["isolated"])
                    self.assertEqual({item["port"] for item in self.cluster.state_ports().values()}, {32011})
                    after = yaml.safe_load(self.cluster.config["spec"]["valuesContent"])
                    self.assertEqual(self.cluster.chart, before_chart)
                    for key, value in idle.items():
                        if source == "config":
                            self.assertEqual(after["ports"]["web"][key], value)
                        else:
                            self.assertNotIn(key, after["ports"]["web"])
                    self.assertFalse(ports.reconcile(self.cluster, self.request, planned, self.cluster.defaults)["changed"])

    def test_http_install_removes_redirects_from_all_values_layers(self):
        redirects = [
            {"redirectTo": "websecure"}, {"redirectTo": {"port": "websecure"}},
            {"redirectTo": {"priority": 10}},
            {"redirections": {"entryPoint": {"to": "websecure", "scheme": "https"}}},
            {"http": {"redirections": {"entryPoint": {"to": "websecure", "scheme": "https", "permanent": True}}}},
            {"redirections": {"entryPoint": {"to": ""}}},
            {"http": {"redirections": {"entryPoint": {"permanent": False}}}},
        ]
        for source in ("defaults", "chart", "config"):
            for redirect in redirects:
                for installed in (False, True):
                    with self.subTest(source=source, redirect=redirect, installed=installed):
                        self.cluster = FakeCluster(self.directory)
                        values = {"ports": {"web": redirect}}
                        if source == "defaults":
                            self.cluster.defaults = ports.merge(self.cluster.defaults, values)
                        elif source == "chart":
                            self.cluster.chart["spec"]["valuesContent"] = yaml.safe_dump(values)
                        else:
                            self.cluster.config = {"metadata": {"resourceVersion": "7"},
                                                   "spec": {"valuesContent": yaml.safe_dump(values)}}
                        if installed:
                            self.cluster.install(objects())
                        before_chart = copy.deepcopy(self.cluster.chart)
                        before_config = copy.deepcopy(self.cluster.config)
                        result = ports.reconcile(self.cluster, self.request, endpoint(), self.cluster.defaults)
                        self.assertTrue(result["isolated"])
                        self.assertFalse(any(".http.redirections." in arg.lower() for arg in self.cluster.controller_args()))
                        self.assertEqual(self.cluster.chart, before_chart)
                        backup = json.loads(Path(result["backup"]).read_text())
                        self.assertEqual(backup["helmchartconfig"], before_config)
                        self.assertEqual(backup["helmchart"], before_chart)
                        self.assertFalse(ports.reconcile(self.cluster, self.request, endpoint(), self.cluster.defaults)["changed"])

    def test_unknown_redirects_fail_before_any_mutation(self):
        unsafe_values = [
            {"redirectTo": ["websecure"]}, {"redirectTo": True},
            {"redirections": {"entryPoint": "websecure"}},
            {"redirections": {"futureRedirect": {}}},
            {"http": {"redirections": {"entrypoint": {}}}},
            {"http": {"redirections": []}},
            {"http": {"redirections": False}},
        ]
        for source in ("defaults", "chart", "config"):
            for name in ("web", "websecure", "foreign"):
                for unsafe in unsafe_values:
                    with self.subTest(source=source, port=name, unsafe=unsafe):
                        self.cluster = FakeCluster(self.directory)
                        if name == "foreign":
                            self.cluster.defaults["ports"][name] = {"port": 12345, "exposedPort": 12345,
                                                                   "expose": {"default": False}}
                        values = {"ports": {name: unsafe}}
                        if source == "defaults":
                            self.cluster.defaults = ports.merge(self.cluster.defaults, values)
                        elif source == "chart":
                            self.cluster.chart["spec"]["valuesContent"] = yaml.safe_dump(values)
                        else:
                            self.cluster.config = {"metadata": {"resourceVersion": "7"},
                                                   "spec": {"valuesContent": yaml.safe_dump(values)}}
                        self.cluster.install(objects("iam", 32011))
                        before = copy.deepcopy(self.cluster.config)
                        live = copy.deepcopy(self.cluster.live)
                        with self.assertRaisesRegex(ports.Unsafe, "redirect"):
                            ports.reconcile(self.cluster, self.request, endpoint(), self.cluster.defaults)
                        self.assertEqual(self.cluster.config, before)
                        self.assertEqual(self.cluster.live, live)
                        self.assertFalse(any(call[:2] in (("kubectl", "patch"), ("kubectl", "create"))
                                             for call in self.cluster.calls))
        self.assertFalse((self.directory / "backups").exists())

    def test_https_install_preserves_redirects_and_listener_exposure(self):
        redirects = [
            {"redirectTo": "websecure"}, {"redirectTo": {"port": "websecure"}},
            {"redirections": {"entryPoint": {"to": "websecure", "scheme": "https"}}},
            {"http": {"redirections": {"entryPoint": {"to": "websecure", "scheme": "https", "permanent": True}}}},
        ]
        for source in ("defaults", "chart", "config"):
            for redirect in redirects:
                with self.subTest(source=source, redirect=redirect):
                    self.cluster = FakeCluster(self.directory)
                    values = {"ports": {"web": redirect}}
                    if source == "defaults":
                        self.cluster.defaults = ports.merge(self.cluster.defaults, values)
                    elif source == "chart":
                        self.cluster.chart["spec"]["valuesContent"] = yaml.safe_dump(values)
                    else:
                        self.cluster.config = {"metadata": {"resourceVersion": "7"},
                                               "spec": {"valuesContent": yaml.safe_dump(values)}}
                    before_chart = copy.deepcopy(self.cluster.chart)
                    before_config = copy.deepcopy(self.cluster.config)
                    result = ports.reconcile(self.cluster, self.request, endpoint(number=32443, secure=True), self.cluster.defaults)
                    effective = self.cluster.effective_values()
                    for key, value in redirect.items():
                        self.assertEqual(effective["ports"]["web"][key], value)
                    self.assertEqual({item["port"] for item in self.cluster.state_ports().values()}, {80, 32443})
                    self.assertTrue(any(".http.redirections." in arg.lower() for arg in self.cluster.controller_args()))
                    self.assertEqual(self.cluster.chart, before_chart)
                    self.assertEqual(json.loads(Path(result["backup"]).read_text())["helmchartconfig"], before_config)
                    self.assertFalse(ports.reconcile(self.cluster, self.request, endpoint(number=32443, secure=True),
                                                     self.cluster.defaults)["changed"])

    def test_redirects_without_https_or_http_ownership_remain_guarded(self):
        for name in ("websecure", "foreign"):
            with self.subTest(name=name):
                self.cluster = FakeCluster(self.directory)
                if name == "foreign":
                    self.cluster.defaults["ports"][name] = {"port": 12345, "expose": {"default": False}}
                self.cluster.chart["spec"]["valuesContent"] = yaml.safe_dump({
                    "ports": {name: {"http": {"redirections": {"entryPoint": {"to": "websecure"}}}}}})
                with self.assertRaisesRegex(ports.Unsafe, "redirect"):
                    self.reconcile()
                self.assertIsNone(self.cluster.config)

    def test_https_install_preserves_explicit_redirect_arguments(self):
        for field in ("additionalArguments", "globalArguments"):
            with self.subTest(field=field):
                self.cluster = FakeCluster(self.directory)
                arguments = ["--log.level=INFO", "--entryPoints.web.http.redirections.entryPoint.to=:443",
                             "--entrypoints.web.http.redirections.entrypoint.scheme=https"]
                self.cluster.chart["spec"]["valuesContent"] = yaml.safe_dump({field: arguments})
                self.reconcile(endpoint(number=443, secure=True))
                self.assertEqual(self.cluster.effective_values()[field], arguments)
                self.assertEqual({item["port"] for item in self.cluster.state_ports().values()}, {80, 443})

    def test_mixed_install_clears_http_and_preserves_other_redirects(self):
        first = self.reconcile(endpoint(number=443, secure=True))
        self.cluster.install(ports.isolate(objects(number=443, secure=True), first["endpoint"]))
        redirect = {"http": {"redirections": {"entryPoint": {"to": "websecure", "scheme": "https"}}}}
        values = {"ports": {"web": redirect,
                            "foreign-http": {"port": 12345, "expose": {"default": False}, **redirect}}}
        self.cluster.chart["spec"]["valuesContent"] = yaml.safe_dump(values)
        self.reconcile(endpoint("iam", 32011))
        after = self.cluster.effective_values()
        self.assertEqual(after["ports"]["foreign-http"]["http"], redirect["http"])
        self.assertFalse(ports.check_redirects(after["ports"]["web"]))
        self.assertTrue(any("--entryPoints.foreign-http.http.redirections." in arg for arg in self.cluster.controller_args()))
        self.assertEqual({item["port"] for item in self.cluster.state_ports().values()}, {443, 32011})

    def test_https_existing_redirect_source_and_target_remain_exposed(self):
        self.cluster.install(objects(number=443, secure=True))
        self.cluster.chart["spec"]["valuesContent"] = yaml.safe_dump({
            "ports": {"web": {"http": {"redirections": {"entryPoint": {"to": "websecure", "scheme": "https"}}}}}})
        self.reconcile(endpoint(number=32443, secure=True))
        self.assertEqual({item["port"] for item in self.cluster.state_ports().values()}, {80, 443, 32443})
        self.assertFalse(self.reconcile(endpoint(number=32443, secure=True))["changed"])

    def test_http_redirect_arguments_removed_without_losing_other_settings(self):
        for field in ("additionalArguments", "globalArguments"):
            with self.subTest(field=field):
                self.cluster = FakeCluster(self.directory)
                values = {field: ["--log.level=INFO", "--entryPoints.web.http.redirections.entryPoint.to=websecure",
                                  "--entrypoints.web.http.redirections.entrypoint.scheme=https"],
                          "ports": {"web": {"http": {"middlewares": ["auth@kubernetescrd"],
                                                    "redirections": {"entryPoint": {"to": "websecure"}}},
                                            "forwardedHeaders": {"trustedIPs": ["10.0.0.0/8"]}}}}
                self.cluster.chart["spec"]["valuesContent"] = yaml.safe_dump(values)
                self.reconcile()
                effective = self.cluster.effective_values()
                self.assertEqual(effective[field], ["--log.level=INFO"])
                self.assertEqual(effective["ports"]["web"]["http"]["middlewares"], ["auth@kubernetescrd"])
                self.assertEqual(effective["ports"]["web"]["forwardedHeaders"], values["ports"]["web"]["forwardedHeaders"])
                self.assertFalse(any(".http.redirections." in arg.lower() for arg in self.cluster.controller_args()))

    def test_stale_redirect_arguments_never_succeed(self):
        for field in ("stale_redirect", "stale_pod_redirect"):
            with self.subTest(field=field):
                self.cluster = FakeCluster(self.directory)
                setattr(self.cluster, field, True)
                with self.assertRaisesRegex(ports.Unsafe, "Timeout"):
                    self.reconcile()

    def test_shared_listener_redirect_policy_is_not_silently_changed(self):
        for values in ({"ports": {"web": {"http": {"redirections": {"entryPoint": {"to": "websecure"}}}}}},
                       {"additionalArguments": ["--entryPoints.web.http.redirections.entryPoint.to=websecure"]}):
            with self.subTest(values=values):
                self.cluster = FakeCluster(self.directory)
                self.cluster.live = [{"kind": "Ingress", "metadata": {"name": "foreign", "namespace": "other"},
                                      "spec": {"rules": [{"host": "other.example"}]}}]
                self.cluster.chart["spec"]["valuesContent"] = yaml.safe_dump(values)
                before = copy.deepcopy(self.cluster.live)
                with self.assertRaisesRegex(ports.Unsafe, "shared listener redirects"):
                    self.reconcile(endpoint(number=80))
                self.assertIsNone(self.cluster.config)
                self.assertEqual(self.cluster.live, before)
                self.assertFalse(any(call[:2] in (("kubectl", "patch"), ("kubectl", "create")) for call in self.cluster.calls))

    def test_http_cleanup_preserves_https_and_owned_extra_listeners(self):
        first = self.reconcile(endpoint(number=32443, secure=True))
        self.cluster.install(ports.isolate(objects(number=32443, secure=True), first["endpoint"]))
        before = yaml.safe_load(self.cluster.config["spec"]["valuesContent"])
        before["ports"]["web"]["http"] = {"redirections": {"entryPoint": {"to": "websecure", "scheme": "https"}}}
        before["ports"]["websecure"]["http"] = {"tls": {"enabled": True, "options": "keep"}}
        self.cluster.config["spec"]["valuesContent"] = yaml.safe_dump(before)
        second = self.reconcile(endpoint("iam", 32011))
        self.assertEqual(second["endpoint"]["entrypoint"], "eva-h-32011")
        self.assertEqual({item["port"] for item in self.cluster.state_ports().values()}, {32011, 32443})
        values = yaml.safe_load(self.cluster.config["spec"]["valuesContent"])
        self.assertEqual(values["ports"]["websecure"]["http"], before["ports"]["websecure"]["http"])
        values["ports"]["eva-h-32011"]["http"] = {"redirections": {"entryPoint": {"to": "websecure"}}}
        self.cluster.config["spec"]["valuesContent"] = yaml.safe_dump(values)
        self.reconcile(endpoint("iam", 32011))
        self.assertFalse(any(".http.redirections." in arg.lower() for arg in self.cluster.controller_args()))

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
            for case in ("dns", "ip", "ip-nested-default", "ip-redirect", "ip-https-redirect", "opt-out"):
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
        if case in ("ip-nested-default", "ip-redirect", "ip-https-redirect"):
            fake.chart["spec"]["valuesContent"] = yaml.safe_dump({
                "ports": {"web": {"http": {"redirections": {"entryPoint": (
                    {"to": "websecure", "scheme": "https"} if case != "ip-nested-default" else {})}}}},
            })
        fake.bad_args = case == "helper-failure"
        state = directory / "cluster.json"
        state.write_text(json.dumps(fake.__dict__))
        chart = directory / "chart"
        (chart / "templates").mkdir(parents=True)
        (chart / "Chart.yaml").write_text("apiVersion: v2\nname: eva-fixture\nversion: 1.0.0\n")
        (chart / "values.yaml").write_text("{}\n")
        manifest = objects(component, secure=case == "ip-https-redirect",
                   host="app.example" if case == "dns" else "10.159.56.197")
        (chart / "templates/resources.yaml").write_text(yaml.safe_dump_all(manifest))
        (directory / "values.yaml").write_text("{}\n")
        (directory / "eva-iam-k3s-override.yaml").write_text("{}\n")
        source = ROOT / f"src/solution/roles/eva_{component}/tasks/main.yaml"
        tasks = yaml.safe_load(source.read_text())
        caller = next(task for task in tasks if task.get("ansible.builtin.include_role", {}).get("name") == "eva_traefik_ports")
        result = next(task for task in tasks if "eva_" + component + "_traefik_args" in task.get("ansible.builtin.set_fact", {}))
        variables = {"eva_traefik_ports_manage": case != "opt-out", "eva_traefik_ports_backup_root": str(directory / "backup"),
                     "eva_traefik_ports_timeout": 0, "eva_cli_chart_path": "override", "eva_cli_helm_set": [], "repository_mode": "online_repository",
                     f"eva_{component}_namespace": "eva-" + component, f"eva_{component}_name": "eva-" + component,
                     f"eva_{component}_workdir": str(directory), f"eva_{component}_chart_source": str(chart),
                     f"eva_{component}_chart_version": "1.0.0", f"eva_{component}_kubeconfig": str(directory / "absent-kubeconfig"),
                     "eva_app_effective_values_dest": str(directory / "values.yaml"), "eva_iam_chart_version_arg": "",
                     "eva_iam_app_sso_base_url": "http://10.159.56.197:32010",
                     "eva_iam_effective_user_values": {}, "eva_iam_values_dest": str(directory / "values.yaml")}
        isolated = case in ("ip", "ip-nested-default", "ip-redirect", "ip-https-redirect")
        assertion = {"ansible.builtin.assert": {"that": ["eva_traefik_ports_result.isolated == " + ("true" if isolated else "false")]}}
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
        self.assertEqual(saved["config"] is not None, isolated)
        if case == "ip-redirect":
            fake.__dict__.update(saved)
            self.assertFalse(any(".http.redirections." in arg.lower() for arg in fake.controller_args()))
        elif case == "ip-https-redirect":
            fake.__dict__.update(saved)
            self.assertTrue(any(".http.redirections." in arg.lower() for arg in fake.controller_args()))
            self.assertIn(80, {item["port"] for item in fake.state_ports().values()})


class PublishedChartTests(unittest.TestCase):
    """Optional real archived-chart proof; missing archives are explicit skips."""
    def test_traefik_redirect_policy_with_actual_helm_schema_and_templates(self):
        if not TRAEFIK_CHARTS or not shutil.which("helm"):
            self.skipTest("Provide EVA_TEST_TRAEFIK_CHARTS or EVA_TEST_HELM_DOWNLOAD=1 for real Traefik renders")
        for archive in TRAEFIK_CHARTS:
            with tarfile.open(archive) as tar:
                original_defaults = yaml.safe_load(tar.extractfile("traefik/values.yaml").read())
            web = original_defaults["ports"]["web"]
            redirect = ({"http": {"redirections": {"entryPoint": {"to": "websecure", "scheme": "https"}}}}
                        if "http" in web else {"redirections": {"entryPoint": {"to": "websecure", "scheme": "https"}}}
                        if "redirections" in web else {"redirectTo": {"port": "websecure"}})
            for source, secure in ((source, secure) for source in ("defaults", "chart", "config", "owned-extra") for secure in (False, True)):
                with self.subTest(chart=archive.name, source=source, secure=secure), tempfile.TemporaryDirectory() as temp:
                    root = Path(temp)
                    chart_defaults = copy.deepcopy(original_defaults)
                    chart = {"spec": {}}
                    values = {"ports": {"web": redirect}}
                    config = None
                    if source == "defaults":
                        chart_defaults = ports.merge(chart_defaults, values)
                    elif source == "chart":
                        chart["spec"]["valuesContent"] = yaml.safe_dump(values)
                    else:
                        if source == "owned-extra":
                            extra = {"port": 32010, "exposedPort": 32010,
                                     "expose": copy.deepcopy(web["expose"]), **redirect}
                            values["ports"]["eva-h-32010"] = extra
                        config = {"spec": {"valuesContent": yaml.safe_dump(values)}}
                    if source == "owned-extra":
                        config["metadata"] = {"annotations": {ports.STATE: json.dumps({"app": {"entrypoint": "eva-h-32010"}})}}
                    desired, _, expected = ports.plan(chart_defaults, chart, config, {"app": endpoint(number=32443 if secure else 32010, secure=secure)}, [],
                                                      preserve_primary=source == "owned-extra")
                    base = root / "base.yaml"
                    overrides = root / "hcc.yaml"
                    # Passing modified defaults as a values layer reproduces
                    # inherited Chart leaves without rewriting its archive.
                    inherited = ports.merge(chart_defaults, yaml.safe_load(chart["spec"].get("valuesContent", "")) or {})
                    base.write_text(yaml.safe_dump(inherited))
                    overrides.write_text(yaml.safe_dump(desired))
                    command = ["helm", "template", "traefik", str(archive), "-n", "kube-system", "-f", str(base)]
                    before_values = root / "before.yaml"
                    before_values.write_text(yaml.safe_dump(values if config else {}))
                    before = subprocess.run(command + ["-f", str(before_values)], capture_output=True, text=True)
                    self.assertEqual(before.returncode, 0, before.stderr)
                    self.assertIn("http.redirections.entryPoint.to", before.stdout)
                    rendered = subprocess.run(command + ["-f", str(overrides)], capture_output=True, text=True)
                    self.assertEqual(rendered.returncode, 0, rendered.stderr)
                    manifests = ports.documents(rendered.stdout)
                    deployment = next(obj for obj in manifests if obj["kind"] == "Deployment")
                    args = deployment["spec"]["template"]["spec"]["containers"][0]["args"]
                    self.assertEqual(any(".http.redirections." in arg.lower() for arg in args), secure)
                    if secure and source != "owned-extra":
                        self.assertTrue(any(".http.redirections.entrypoint.to=:32443" in arg.lower() for arg in args))
                    service = next(obj for obj in manifests if obj["kind"] == "Service" and obj["metadata"]["name"] == "traefik")
                    actual = {item["name"]: {key: item[key] for key in ("port", "targetPort", "protocol")}
                              for item in service["spec"]["ports"]}
                    self.assertEqual(actual, expected)

    def test_exact_offline_charts_and_installed_helm_postrenderer(self):
        charts = {
            "app": Path(os.environ.get("EVA_TEST_APP_CHART", ROOT / "out/cache/eva-app/eva-app-3.3.0-beta.9.tgz")),
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
