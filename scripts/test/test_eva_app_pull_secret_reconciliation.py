"""App pull-reference changes must not start a second App during installation."""

import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import yaml


sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
HELPER = ROOT / "src/solution/roles/eva_app/files/reconcile_pull_secrets.py"
SPEC = importlib.util.spec_from_file_location("app_pull_reconciliation", HELPER)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class Cluster:
    def __init__(self, refs):
        self.deployment = {"metadata": {"uid": "app-uid", "resourceVersion": "17"}, "spec": {
            "replicas": 0, "selector": {"matchLabels": {"app": "selected-app"}},
            "template": {"spec": {"imagePullSecrets": refs}}}}
        self.pods = []
        self.calls = []
        self.reject_patch = False

    def run(self, args, input=None, **kwargs):
        self.calls.append((args, input))
        assert args[-2:] == ["-n", "eva-app"]
        command = args[1:-2]
        if command[:2] == ["get", "deployment"]:
            return subprocess.CompletedProcess(args, 0, json.dumps(self.deployment), "")
        if command[:2] == ["get", "pods"]:
            assert command[2:] == ["-l", "app=selected-app", "-o", "json"]
            return subprocess.CompletedProcess(args, 0, json.dumps({"items": self.pods}), "")
        if command[:2] == ["patch", "deployment"]:
            if self.reject_patch:
                return subprocess.CompletedProcess(args, 1, "", "private-credential conflict")
            patch = json.loads(input)
            assert patch["metadata"] == self.deployment["metadata"]
            assert patch["spec"]["replicas"] == self.deployment["spec"]["replicas"] == 0
            assert set(patch["spec"]) == {"replicas", "template"}
            assert set(patch["spec"]["template"]["spec"]) == {"imagePullSecrets"}
            self.deployment["spec"]["template"]["spec"]["imagePullSecrets"] = patch["spec"]["template"]["spec"]["imagePullSecrets"]
            self.deployment["metadata"]["resourceVersion"] = "18"
            return subprocess.CompletedProcess(args, 0, "patched", "")
        raise AssertionError(args)


class PullSecretReconciliationTests(unittest.TestCase):
    def test_legacy_reference_is_removed_without_starting_app(self):
        desired = [{"name": "custom-pull-secret"}]
        cluster = Cluster([{"name": "old-reference"}] + desired)
        result = MODULE.reconcile("eva-app", "eva-app", "app-uid", desired, run=cluster.run)
        self.assertTrue(result["changed"])
        self.assertEqual(cluster.deployment["spec"]["template"]["spec"]["imagePullSecrets"], desired)
        self.assertEqual(cluster.deployment["spec"]["replicas"], 0)
        self.assertEqual(len([c for c, _ in cluster.calls if c[1] == "patch"]), 1)

    def test_matching_references_do_not_mutate_the_pod_template(self):
        for desired in [[], [{"name": "custom"}], [{"name": "custom"}, {"name": "shared"}]]:
            with self.subTest(desired=desired):
                cluster = Cluster(copy.deepcopy(desired))
                before = copy.deepcopy(cluster.deployment)
                result = MODULE.reconcile("eva-app", "eva-app", "app-uid", desired, run=cluster.run)
                self.assertFalse(result["changed"])
                self.assertEqual(cluster.deployment, before)
                self.assertFalse(any(c[1] == "patch" for c, _ in cluster.calls))

    def test_disabled_pull_secrets_clear_legacy_references_while_stopped(self):
        cluster = Cluster([{"name": "legacy"}])
        result = MODULE.reconcile("eva-app", "eva-app", "app-uid", [], run=cluster.run)
        self.assertTrue(result["changed"])
        self.assertEqual(cluster.deployment["spec"]["template"]["spec"]["imagePullSecrets"], [])

    def test_live_or_replaced_workloads_are_never_patched(self):
        for scenario in ["running", "lingering pod", "new uid", "missing version", "missing selector"]:
            with self.subTest(scenario=scenario):
                cluster = Cluster([{"name": "legacy"}])
                if scenario == "running":
                    cluster.deployment["spec"]["replicas"] = 1
                elif scenario == "lingering pod":
                    cluster.pods = [{"metadata": {"deletionTimestamp": "2026-11-06T05:34:04Z"}}]
                elif scenario == "new uid":
                    cluster.deployment["metadata"]["uid"] = "replacement-uid"
                elif scenario == "missing version":
                    del cluster.deployment["metadata"]["resourceVersion"]
                else:
                    del cluster.deployment["spec"]["selector"]
                with self.assertRaises(MODULE.UnsafeReconciliation):
                    MODULE.reconcile("eva-app", "eva-app", "app-uid", [], run=cluster.run)
                self.assertFalse(any(c[1] == "patch" for c, _ in cluster.calls))

    def test_concurrent_kubernetes_change_fails_without_exposing_diagnostics(self):
        cluster = Cluster([{"name": "legacy"}])
        cluster.reject_patch = True
        before = copy.deepcopy(cluster.deployment)
        with self.assertRaises(MODULE.UnsafeReconciliation) as failure:
            MODULE.reconcile("eva-app", "eva-app", "app-uid", [], run=cluster.run)
        self.assertNotIn("private-credential", str(failure.exception))
        self.assertEqual(cluster.deployment, before)

    def test_final_rendered_manifest_controls_the_exact_reference_list(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "rendered.yaml"
            for desired in [[], [{"name": "helm-set-secret"}], [{"name": "custom"}, {"name": "shared"}]]:
                deployment = {"kind": "Deployment", "metadata": {"name": "eva-app", "namespace": "eva-app"},
                              "spec": {"template": {"spec": {"imagePullSecrets": desired}}}}
                for resources in [[deployment], [{"kind": "List", "items": [deployment]}]]:
                    path.write_text(yaml.safe_dump_all(resources))
                    self.assertEqual(MODULE.desired_references(path, "eva-app", "eva-app"), desired)
            path.write_text(yaml.safe_dump_all([deployment, deployment]))
            with self.assertRaises(MODULE.UnsafeReconciliation):
                MODULE.desired_references(path, "eva-app", "eva-app")
            path.write_text("password: private-credential\ninvalid: [\n")
            with self.assertRaises(MODULE.UnsafeReconciliation) as failure:
                MODULE.desired_references(path, "eva-app", "eva-app")
            self.assertNotIn("private-credential", str(failure.exception))

    def test_role_never_reconciles_pull_references_after_helm_starts_app(self):
        tasks = yaml.safe_load((ROOT / "src/solution/roles/eva_app/tasks/main.yaml").read_text())
        names = [task.get("name") for task in tasks]
        install = names.index("Install EVA App and restore prior replicas if Helm fails")
        stopped = names.index("Wait until EVA App pods are terminated before reinstall")
        self.assertLess(stopped, install)
        block = tasks[install]["block"]
        self.assertIn("reconcile_pull_secrets.py", block[0]["ansible.builtin.command"]["argv"][1])
        self.assertIn("eva_app_deployment_exists.rc == 0", block[0]["when"])
        self.assertIn("db-target-manifest.yaml", block[0]["ansible.builtin.command"]["argv"][-1])
        self.assertIn("rescue", tasks[install])
        for task in tasks[install + 1:]:
            self.assertNotIn("reconcile_pull_secrets.py", str(task))
            self.assertNotIn("eva_app_patch_pullsecret", str(task))
            argv = task.get("ansible.builtin.command", {})
            if isinstance(argv, dict):
                self.assertNotEqual(argv.get("argv", [])[:3], ["kubectl", "patch", "deployment"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
