#!/usr/bin/env python3
"""Existing k3s connection and legacy App public-port transition regression tests."""

import copy
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import yaml

from test_traefik_ports import FakeCluster, ROOT, endpoint, objects, ports


class LegacyCluster(FakeCluster):
    def __init__(self, directory):
        super().__init__(directory)
        configmap = objects()[0]
        configmap["apiVersion"] = "v1"
        configmap["data"].pop("BACKEND_PUBLIC_PORT")
        configmap["data"]["BACKEND_HOST"] = "192.1.70.21"
        metadata = copy.deepcopy(configmap["metadata"])
        metadata.update(name="eva-app", uid="original-service")
        service = {"apiVersion": "v1", "kind": "Service", "metadata": metadata, "spec": {
            "type": "NodePort", "clusterIP": "10.43.19.219", "clusterIPs": ["10.43.19.219"],
            "ipFamilies": ["IPv4"], "ipFamilyPolicy": "SingleStack", "sessionAffinity": "None",
            "internalTrafficPolicy": "Cluster", "externalTrafficPolicy": "Cluster",
            "ports": [{"name": "http", "protocol": "TCP", "port": 32010, "targetPort": 32010, "nodePort": 32010}],
            "selector": {"app.kubernetes.io/name": "eva-app", "app.kubernetes.io/instance": "eva-app"}}}
        internal = copy.deepcopy(service)
        internal["metadata"].update(name="eva-app-internal", uid="internal-service")
        internal["spec"]["type"] = "ClusterIP"
        internal["spec"]["ports"][0].pop("nodePort")
        self.live = [configmap, service, internal]
        self.history["eva-app"] = copy.deepcopy([configmap, service, internal])
        for obj in self.history["eva-app"]:
            if obj["kind"] == "Service":
                for key in ("clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy", "internalTrafficPolicy", "sessionAffinity"):
                    obj["spec"].pop(key, None)
        self.status = "deployed"
        self.service_race = False
        self.serial = 0

    def get(self, resource, namespace="kube-system", name=None, optional=False):
        if resource in ("services", "configmaps", "ingresses") and name != "traefik":
            kind = {"services": "Service", "configmaps": "ConfigMap", "ingresses": "Ingress"}[resource]
            return {"items": copy.deepcopy([obj for obj in self.live if obj["kind"] == kind
                                            and (namespace is None or obj["metadata"]["namespace"] == namespace)])}
        return super().get(resource, namespace, name, optional)

    def run(self, args, stdin=None):
        if args[:2] == ["helm", "status"]:
            return json.dumps({"name": "eva-app", "namespace": "eva-app", "version": 2, "info": {"status": self.status}})
        if args[:2] == ["kubectl", "create"]:
            obj = json.loads(stdin)
            if obj["kind"] == "Ingress":
                self.calls.append(tuple(args))
                receipt = json.loads(Path(obj["metadata"]["annotations"][ports.LEGACY]).read_text())
                assert receipt["legacy"]["service"]["spec"]["type"] == "NodePort"
                assert not any(item["kind"] == "Ingress" for item in self.live)
                obj["metadata"].update(uid="created-bridge", resourceVersion="1", creationTimestamp="test")
                self.live.append(obj)
                return json.dumps(obj)
            output = super().run(args, stdin)
            self.config["metadata"]["uid"] = "created-config"
            return json.dumps(self.config) if args[-2:] == ["-o", "json"] else output
        if args[:2] == ["kubectl", "patch"] and args[-2:] == ["-o", "json"]:
            self.calls.append(tuple(args))
            resource = args[2]
            obj = self.config if resource == ports.RESOURCE else next(
                obj for obj in self.live if obj["kind"] == ("Service" if resource == "services" else "Ingress")
                and obj["metadata"]["name"] == args[3])
            if resource == "services" and self.service_race:
                obj["metadata"]["resourceVersion"] = "99"
                obj["spec"]["selector"]["operator"] = "keep"
            for op in json.loads(stdin):
                keys = [key.replace("~1", "/").replace("~0", "~") for key in op["path"].strip("/").split("/")]
                target = obj
                for key in keys[:-1]:
                    target = target.setdefault(key, {})
                if op["op"] == "test":
                    ports.require(target.get(keys[-1]) == op["value"], "Test CAS conflict")
                else:
                    target[keys[-1]] = op["value"]
            obj["metadata"]["resourceVersion"] = str(int(obj["metadata"]["resourceVersion"]) + 1)
            return json.dumps(obj)
        if args[:3] == ["kubectl", "delete", "--raw"]:
            self.calls.append(tuple(args))
            config = "/helmchartconfigs/" in args[3]
            obj = self.config if config else next(obj for obj in self.live if obj["kind"] == "Ingress")
            meta = obj["metadata"]
            assert json.loads(stdin)["preconditions"] == {"uid": meta["uid"], "resourceVersion": meta["resourceVersion"]}
            if config:
                self.config = None
            else:
                self.live.remove(obj)
            return "deleted"
        return super().run(args, stdin)


class LegacyAppTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.cluster = LegacyCluster(self.temp.name)
        self.request = {"backup_root": str(Path(self.temp.name) / "backups"), "timeout": 0, "legacy_app": {
            "enabled": True, "ingress_enabled": True, "namespace": "eva-app", "release": "eva-app",
            "host": "222.108.131.192", "port": 32010, "secure": False, "path": "/"}}
        self.original = copy.deepcopy(self.cluster.live)

    def reconcile(self, component="iam"):
        planned = ports.endpoint(objects(component, host="222.108.131.192", path="/iam" if component == "iam" else "/"),
                                 component, "eva-" + component, "eva-" + component)
        return ports.reconcile(self.cluster, self.request, planned)

    def app_service(self):
        return next(obj for obj in self.cluster.live if obj["kind"] == "Service" and obj["metadata"]["name"] == "eva-app")

    def test_iam_first_bridges_existing_app_and_retries_without_helm_ingress_history(self):
        result = self.reconcile()
        self.assertTrue(result["changed"])
        service = self.app_service()
        self.assertEqual(service["spec"]["type"], "ClusterIP")
        self.assertNotIn("nodePort", service["spec"]["ports"][0])
        self.assertEqual(service["spec"]["clusterIP"], self.original[1]["spec"]["clusterIP"])
        self.assertEqual(self.cluster.live[0], self.original[0])
        self.assertEqual(self.cluster.live[2], self.original[2])
        self.assertEqual({p["port"] for p in self.cluster.state_ports().values()}, {32010})
        self.cluster.install(ports.isolate(objects("iam", host="222.108.131.192", path="/iam"), result["endpoint"]))
        self.assertFalse(self.reconcile()["changed"])
        app = self.reconcile("app")
        self.assertFalse(app["changed"])
        bridge = self.cluster.live[-1]
        self.assertEqual(bridge["metadata"]["annotations"][ports.ENTRY], app["endpoint"]["entrypoint"])
        self.assertEqual(json.loads(Path(result["backup"]).read_text())["legacy"]["phase"], "active")
        # After the real App Helm upgrade, normal history takes precedence.
        updated = ports.isolate(objects(host="222.108.131.192"), app["endpoint"])
        self.cluster.live = [obj for obj in self.cluster.live if obj["metadata"]["namespace"] != "eva-app"
                             or obj["kind"] not in ("ConfigMap", "Ingress")] + updated
        self.cluster.history["eva-app"] = copy.deepcopy([obj for obj in self.cluster.live if obj["metadata"]["namespace"] == "eva-app"])
        self.assertFalse(self.reconcile("app")["changed"])
        writes = [call for call in self.cluster.calls if call[:2] in (("kubectl", "patch"), ("kubectl", "create"), ("kubectl", "delete"))]
        self.assertTrue(all(call[2] in ("services", "ingresses", ports.RESOURCE, "-f", "--raw") for call in writes))

    def test_app_only_upgrade_can_migrate_its_own_nodeport(self):
        self.request.pop("legacy_app")
        self.assertTrue(self.reconcile("app")["changed"])

    def test_unselected_app_and_foreign_port_occupant_remain_blocked(self):
        self.request["legacy_app"]["enabled"] = False
        with self.assertRaisesRegex(ports.Unsafe, "conflicts"):
            self.reconcile()
        self.assertEqual(self.cluster.live, self.original)

    def test_bad_ownership_history_and_active_argo_refused_before_writes(self):
        for change in ("owner", "history", "selector", "argo", "pending"):
            with self.subTest(change=change):
                self.cluster = LegacyCluster(self.temp.name)
                service = self.app_service()
                if change == "owner":
                    service["metadata"]["annotations"]["meta.helm.sh/release-name"] = "foreign"
                elif change == "history":
                    self.cluster.history["eva-app"] = []
                elif change == "selector":
                    service["spec"]["selector"]["other"] = "foreign"
                elif change == "argo":
                    service["metadata"]["annotations"]["argocd.argoproj.io/tracking-id"] = "active"
                else:
                    self.cluster.status = "pending-upgrade"
                before = copy.deepcopy(self.cluster.live)
                with self.assertRaises(ports.Unsafe):
                    self.reconcile()
                self.assertEqual(self.cluster.live, before)
                self.assertFalse(any(call[:2] in (("kubectl", "patch"), ("kubectl", "create")) for call in self.cluster.calls))

    def test_convergence_failure_restores_nodeport_after_listener_release(self):
        self.cluster.wrong_target = True
        with self.assertRaisesRegex(ports.Unsafe, "original App NodePort restored"):
            self.reconcile()
        self.assertEqual(ports.stable(self.app_service()), ports.stable(self.original[1]))
        self.assertIsNone(self.cluster.config)
        self.assertFalse(any(obj["kind"] == "Ingress" for obj in self.cluster.live))
        commands = [call for call in self.cluster.calls if call[:2] in (("kubectl", "patch"), ("kubectl", "delete"))]
        deleted = next(i for i, call in enumerate(commands) if call[:3] == ("kubectl", "delete", "--raw"))
        restored = max(i for i, call in enumerate(commands) if call[:3] == ("kubectl", "patch", "services"))
        self.assertLess(deleted, restored)

    def test_history_mismatch_names_fields_without_exposing_configmap_values(self):
        service = self.app_service()
        service["spec"]["ports"][0]["targetPort"] = 9999
        service["spec"]["selector"]["operator"] = "custom-selector"
        configmap = self.cluster.live[0]
        configmap["data"]["BACKEND_HOST"] = "private-live-endpoint"
        self.cluster.history["eva-app"][0]["data"]["BACKEND_HOST"] = "private-history-endpoint"
        configmap["data"]["JWT_SECRET"] = "private-live-credential"
        self.cluster.history["eva-app"][0]["data"]["JWT_SECRET"] = "private-history-credential"
        before = copy.deepcopy(self.cluster.live)
        with self.assertRaises(ports.Unsafe) as failure:
            self.reconcile()
        message = str(failure.exception)
        for field in ("Service.spec.ports[0].targetPort", "Service.spec.selector", "ConfigMap.data.BACKEND_HOST"):
            self.assertIn(field, message)
        for value in ("private-live-credential", "private-history-credential", "custom-selector",
                      "private-live-endpoint", "private-history-endpoint", "JWT_SECRET"):
            self.assertNotIn(value, message)
        self.assertEqual(self.cluster.live, before)
        self.assertIsNone(self.cluster.config)
        self.assertFalse(any(call[:2] in (("kubectl", "patch"), ("kubectl", "create")) for call in self.cluster.calls))

    def test_unrelated_manual_configmap_edits_are_preserved_through_transition_and_retry(self):
        configmap = self.cluster.live[0]
        configmap["data"].update(INGESTER_ASSIGNER="manual-assigner", JWT_SECRET="private-runtime-value")
        self.cluster.history["eva-app"][0]["data"]["JWT_SECRET"] = "old-runtime-value"
        self.cluster.history["eva-app"][0]["data"]["OLD_RUNTIME_SETTING"] = "removed-manually"
        expected = copy.deepcopy(configmap)
        result = self.reconcile()
        self.assertEqual(self.cluster.live[0], expected)
        receipt = json.loads(Path(result["backup"]).read_text())
        self.assertEqual(receipt["legacy"]["configmap"], expected)
        self.cluster.install(ports.isolate(objects("iam", host="222.108.131.192", path="/iam"), result["endpoint"]))
        self.assertFalse(self.reconcile()["changed"])
        self.assertFalse(self.reconcile("app")["changed"])
        self.assertEqual(self.cluster.live[0], expected)

    def test_public_endpoint_edits_remain_blocked_before_any_writes(self):
        for field in ports.LEGACY_ENDPOINT_KEYS:
            with self.subTest(field=field):
                self.cluster = LegacyCluster(self.temp.name)
                self.cluster.live[0]["data"][field] = "changed-endpoint"
                before = copy.deepcopy(self.cluster.live)
                with self.assertRaisesRegex(ports.Unsafe, "ConfigMap.data." + field):
                    self.reconcile()
                self.assertEqual(self.cluster.live, before)
                self.assertIsNone(self.cluster.config)
                self.assertFalse(any(call[:2] in (("kubectl", "patch"), ("kubectl", "create"))
                                     for call in self.cluster.calls))

    def test_removed_public_endpoint_fields_remain_blocked(self):
        for field in ports.LEGACY_ENDPOINT_KEYS:
            with self.subTest(field=field):
                self.cluster = LegacyCluster(self.temp.name)
                self.cluster.history["eva-app"][0]["data"][field] = "recorded-endpoint"
                self.cluster.live[0]["data"].pop(field, None)
                before = copy.deepcopy(self.cluster.live)
                with self.assertRaises(ports.Unsafe):
                    self.reconcile()
                self.assertEqual(self.cluster.live, before)
                self.assertIsNone(self.cluster.config)

    def test_unrelated_configmap_edits_after_assessment_or_transition_remain_blocked(self):
        for during_transition in (False, True):
            with self.subTest(during_transition=during_transition):
                self.cluster = LegacyCluster(self.temp.name)
                self.cluster.live[0]["data"]["INGESTER_ASSIGNER"] = "manual-assigner"
                if during_transition:
                    snapshot = ports.snapshot

                    def concurrent_edit(*args, **kwargs):
                        backup = snapshot(*args, **kwargs)
                        self.cluster.live[0]["data"]["INGESTER_ASSIGNER"] = "concurrent-edit"
                        return backup

                    with patch.object(ports, "snapshot", side_effect=concurrent_edit):
                        with self.assertRaises(ports.Unsafe) as failure:
                            self.reconcile()
                    self.assertIn("ConfigMap changed during router migration", str(failure.exception.__cause__))
                    self.assertEqual(ports.stable(self.app_service()), ports.stable(self.original[1]))
                    self.assertIsNone(self.cluster.config)
                    self.assertFalse(any(call[:2] in (("kubectl", "patch"), ("kubectl", "create"))
                                         for call in self.cluster.calls))
                else:
                    self.reconcile()
                    self.cluster.live[0]["data"]["INGESTER_ASSIGNER"] = "concurrent-edit"
                    before = copy.deepcopy(self.cluster.live)
                    with self.assertRaisesRegex(ports.Unsafe, "ConfigMap changed after transition"):
                        self.reconcile("app")
                    self.assertEqual(self.cluster.live, before)
                self.assertEqual(self.cluster.live[0]["data"]["INGESTER_ASSIGNER"], "concurrent-edit")

    def test_failure_restores_existing_operator_traefik_configuration(self):
        self.cluster.config = {"apiVersion": "helm.cattle.io/v1", "kind": "HelmChartConfig", "metadata": {
            "name": "traefik", "namespace": "kube-system", "uid": "existing-config", "resourceVersion": "7",
            "annotations": {"operator": "preserve"}, "labels": {"owner": "operator"}},
            "spec": {"valuesContent": yaml.safe_dump({"resources": {"limits": {"memory": "384Mi"}}}), "failurePolicy": "abort"}}
        original = copy.deepcopy(self.cluster.config)
        self.cluster.wrong_target = True
        with self.assertRaisesRegex(ports.Unsafe, "original App NodePort restored"):
            self.reconcile()
        self.assertEqual(ports.stable(self.cluster.config), ports.stable(original))
        self.assertEqual(ports.stable(self.app_service()), ports.stable(self.original[1]))

    def test_concurrent_operator_config_and_service_are_never_overwritten(self):
        for field in ("config", "service", "bridge"):
            with self.subTest(field=field):
                self.cluster = LegacyCluster(self.temp.name)

                def race(*args, **kwargs):
                    if field == "config":
                        self.cluster.config["metadata"]["annotations"]["operator"] = "keep"
                    elif field == "service":
                        self.app_service()["spec"]["selector"]["operator"] = "keep"
                    else:
                        self.cluster.live[-1]["metadata"]["annotations"]["operator"] = "keep"
                    return False

                with patch.object(ports, "ready", side_effect=race):
                    with self.assertRaisesRegex(ports.Unsafe, "restoration stopped"):
                        self.reconcile()
                target = (self.cluster.config["metadata"]["annotations"] if field == "config" else
                          self.app_service()["spec"]["selector"] if field == "service" else
                          self.cluster.live[-1]["metadata"]["annotations"])
                self.assertEqual(target["operator"], "keep")

    def test_tampered_receipt_or_changed_bridge_and_service_are_refused(self):
        for field in ("receipt", "bridge", "service", "uid"):
            with self.subTest(field=field):
                self.cluster = LegacyCluster(self.temp.name)
                result = self.reconcile()
                if field == "receipt":
                    Path(result["backup"]).chmod(0o644)
                elif field == "bridge":
                    self.cluster.live[-1]["spec"]["rules"][0]["http"]["paths"][0]["backend"]["service"]["name"] = "foreign"
                elif field == "uid":
                    self.cluster.live[-1]["metadata"]["uid"] = "recreated"
                else:
                    self.app_service()["spec"]["selector"]["foreign"] = "other"
                before = copy.deepcopy(self.cluster.live)
                with self.assertRaises(ports.Unsafe):
                    self.reconcile("app")
                self.assertEqual(self.cluster.live, before)

    def test_interruption_after_bridge_create_or_service_conversion_can_retry(self):
        for after_conversion in (False, True):
            with self.subTest(after_conversion=after_conversion):
                self.cluster = LegacyCluster(self.temp.name)
                run = self.cluster.run

                def interrupt(args, stdin=None):
                    result = run(args, stdin)
                    if after_conversion and args[:3] == ["kubectl", "patch", "services"]:
                        raise KeyboardInterrupt()
                    if not after_conversion and args[:2] == ["kubectl", "create"] and json.loads(stdin)["kind"] == "Ingress":
                        raise KeyboardInterrupt()
                    return result

                with patch.object(self.cluster, "run", side_effect=interrupt):
                    with self.assertRaises(KeyboardInterrupt):
                        self.reconcile()
                self.assertTrue(self.reconcile()["isolated"])
                self.assertEqual(self.app_service()["spec"]["type"], "ClusterIP")


@unittest.skipUnless(shutil.which("ansible-playbook"), "ansible-playbook required")
class HostKubeconfigTests(unittest.TestCase):
    def test_companion_app_routing_uses_each_target_values_and_selection(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "app.yaml").write_text(yaml.safe_dump({
                "127.0.0.1": {"app": {"backendHost": "222.108.131.192", "backendPublicPort": 32010,
                                       "backendSecure": False, "jwtSecret": "private-test-input"}, "ingress": {"path": "/"}},
                "localhost": {"app": {"backendHost": "192.0.2.20", "backendPublicPort": 32100}}}))
            inventory = root / "inventory.yaml"
            inventory.write_text(yaml.safe_dump({"all": {"hosts": {
                "server-a": {"ansible_host": "127.0.0.1", "eva_enabled_components": "iam,app"},
                "server-b": {"ansible_host": "localhost", "eva_enabled_components": "iam"}}}}))
            role = ROOT / "src/solution/roles/eva_traefik_ports/tasks/main.yaml"
            tasks = yaml.safe_load(role.read_text())[1]["block"][:3]
            tasks.append({"name": "Verify private companion routing", "no_log": True, "ansible.builtin.assert": {"that": [
                "eva_traefik_legacy_app.enabled == (inventory_hostname == 'server-a')",
                "eva_traefik_legacy_app.port == (32010 if inventory_hostname == 'server-a' else 32100)",
                "eva_traefik_legacy_app.host == ('222.108.131.192' if inventory_hostname == 'server-a' else '192.0.2.20')",
                "eva_traefik_legacy_app.ingress_enabled", "not eva_traefik_legacy_app.secure"]}})
            play = [{"hosts": "all", "connection": "local", "gather_facts": False,
                     "vars": {"eva_site_values_root": str(root)}, "tasks": tasks}]
            path = root / "test.yaml"
            path.write_text(yaml.safe_dump(play))
            result = subprocess.run(["ansible-playbook", "-i", str(inventory), str(path)], capture_output=True, text=True,
                                    env=dict(os.environ, ANSIBLE_NOCOLOR="1"), timeout=45)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertNotIn("private-test-input", result.stdout + result.stderr)

    def test_copy_missing_only_and_preserve_existing_or_explicit_configuration(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "k3s.yaml"
            source.write_text("apiVersion: v1\nclusters: []\n")
            for case in ("missing", "existing", "environment", "no-k3s", "managed"):
                with self.subTest(case=case):
                    home = root / case
                    home.mkdir()
                    target = home / ".kube/config"
                    if case == "existing":
                        target.parent.mkdir()
                        target.write_text("keep existing configuration\n")
                    play = [{"hosts": "localhost", "connection": "local", "gather_facts": False,
                             "vars": {"eva_k3s_kubeconfig_source": str(source if case != "no-k3s" else root / "absent"),
                                      "eva_kubeconfig_home": str(home), "ansible_facts": {"env": {
                                          "HOME": str(home), "KUBECONFIG": str(root / "explicit") if case == "environment" else ""}}},
                             "tasks": [{"ansible.builtin.import_tasks": str(ROOT / "src/host-kubeconfig.yaml"),
                                        "when": case != "managed"}]}]
                    path = root / "test.yaml"
                    path.write_text(yaml.safe_dump(play))
                    result = subprocess.run(["ansible-playbook", "-i", "localhost,", str(path)], capture_output=True, text=True,
                                            env=dict(os.environ, ANSIBLE_NOCOLOR="1"), timeout=45)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    if case == "missing":
                        self.assertEqual(target.read_text(), source.read_text())
                        self.assertEqual(target.stat().st_mode & 0o777, 0o600)
                        self.assertEqual(target.parent.stat().st_mode & 0o777, 0o700)
                    elif case == "existing":
                        self.assertEqual(target.read_text(), "keep existing configuration\n")
                    else:
                        self.assertFalse(target.exists())


if __name__ == "__main__":
    unittest.main(verbosity=2)
