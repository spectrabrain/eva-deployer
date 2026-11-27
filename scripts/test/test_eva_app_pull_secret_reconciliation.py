"""App pull-reference changes must not start a second App during installation."""

import copy
from contextlib import redirect_stderr, redirect_stdout
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

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
            assert set(patch["spec"]["template"]["spec"]) <= {"imagePullSecrets", "containers", "initContainers"}
            self.deployment["spec"]["template"]["spec"].update(patch["spec"]["template"]["spec"])
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

    def test_rescue_restores_original_credentials_only_for_unchanged_stopped_images(self):
        original = {"eva-app": "ecr.example/eva-app:old"}
        desired = [{"name": "ecr-registry"}]
        cluster = Cluster([])
        cluster.deployment["spec"]["template"]["spec"]["containers"] = [
            {"name": name, "image": image} for name, image in original.items()]
        result = MODULE.reconcile("eva-app", "eva-app", "app-uid", desired,
                                  run=cluster.run, expected_images=original)
        self.assertTrue(result["changed"])
        self.assertEqual(cluster.deployment["spec"]["template"]["spec"]["imagePullSecrets"], desired)
        self.assertEqual(cluster.deployment["spec"]["replicas"], 0)
        for scenario in ("new image", "running", "extra container", "replaced uid", "lingering pod"):
            with self.subTest(scenario=scenario):
                cluster = Cluster([])
                spec = cluster.deployment["spec"]
                spec["template"]["spec"]["containers"] = [{"name": "eva-app", "image": original["eva-app"]}]
                if scenario == "new image":
                    spec["template"]["spec"]["containers"][0]["image"] = "harbor.example/eva-app:new"
                elif scenario == "running":
                    spec["replicas"] = 1
                elif scenario == "extra container":
                    spec["template"]["spec"]["containers"].append({"name": "sidecar", "image": "other"})
                elif scenario == "replaced uid":
                    cluster.deployment["metadata"]["uid"] = "replacement-uid"
                else:
                    cluster.pods = [{"metadata": {"name": "terminating-app"}}]
                before = copy.deepcopy(cluster.deployment)
                if scenario in ("replaced uid", "lingering pod"):
                    with self.assertRaises(MODULE.UnsafeReconciliation):
                        MODULE.reconcile("eva-app", "eva-app", "app-uid", desired,
                                         run=cluster.run, expected_images=original)
                else:
                    result = MODULE.reconcile("eva-app", "eva-app", "app-uid", desired,
                                              run=cluster.run, expected_images=original)
                    self.assertFalse(result["changed"])
                self.assertEqual(cluster.deployment, before)
                self.assertFalse(any(c[1] == "patch" for c, _ in cluster.calls))

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

    def test_restore_cli_requires_image_guard_and_passes_original_references(self):
        prefix = [str(HELPER), "--namespace", "eva-app", "--release", "eva-app", "--expected-uid", "app-uid"]
        images = {"eva-app": "ecr.example/eva-app:old"}
        refs = [{"name": "ecr-registry"}]
        args = prefix + ["--restore-images", json.dumps(images), "--restore-references", json.dumps(refs)]
        with patch.object(sys, "argv", args), patch.object(MODULE, "reconcile", return_value={"changed": True}) as reconcile:
            with redirect_stdout(io.StringIO()):
                self.assertEqual(MODULE.main(), 0)
        reconcile.assert_called_once_with("eva-app", "eva-app", "app-uid", refs,
                          expected_images=images, required_registry=None)
        for value in ("null", "[]", "{}", "private-credential-not-json"):
            with self.subTest(value=value), patch.object(sys, "argv", prefix + [
                    "--restore-images", value, "--restore-references", json.dumps(refs)]):
                with patch.object(MODULE, "reconcile") as reconcile, redirect_stderr(io.StringIO()) as error:
                    self.assertEqual(MODULE.main(), 1)
                reconcile.assert_not_called()
                self.assertNotIn("private-credential", error.getvalue())

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
        reconcile = next(task for task in block if task["name"] ==
                         "Reconcile EVA App pull Secret references while existing App is stopped")
        preserve = next(task for task in block if task["name"] ==
                        "Preserve EVA App database identity and legacy local users before upgrade")
        self.assertLess(block.index(preserve), block.index(reconcile))
        relocation = next(task for task in block if task["name"] ==
                  "Relocate stopped EVA App to identical images in the selected Harbor")
        self.assertLess(block.index(relocation), block.index(preserve))
        verify = names.index("Verify existing EVA App image content in the selected Harbor before stopping workloads")
        self.assertLess(verify, names.index("Scale down existing EVA App deployment before reinstall"))
        for task in block:
            if task["name"].startswith("Install EVA App from"):
                self.assertLess(block.index(reconcile), block.index(task))
        self.assertIn("reconcile_pull_secrets.py", reconcile["ansible.builtin.command"]["argv"][1])
        self.assertIn("eva_app_deployment_exists.rc == 0", reconcile["when"])
        self.assertIn("db-target-manifest.yaml", reconcile["ansible.builtin.command"]["argv"][-1])
        self.assertIn("rescue", tasks[install])
        rescue = tasks[install]["rescue"]
        restore = next(task for task in rescue if task["name"] ==
                   "Restore prior EVA App pull references if the original image is still stopped")
        scale = next(task for task in rescue if task["name"] ==
                 "Restore EVA App replicas after Helm installation failure")
        self.assertLess(rescue.index(restore), rescue.index(scale))
        self.assertIn("--restore-images", restore["ansible.builtin.command"]["argv"])
        self.assertIn("--restore-references", restore["ansible.builtin.command"]["argv"])
        self.assertIn("--required-registry", restore["ansible.builtin.command"]["argv"])
        self.assertTrue(any("eva_app_pull_secret_restore.rc" in condition for condition in scale["when"]))
        for task in tasks[install + 1:]:
            self.assertNotIn("reconcile_pull_secrets.py", str(task))
            self.assertNotIn("eva_app_patch_pullsecret", str(task))
            argv = task.get("ansible.builtin.command", {})
            if isinstance(argv, dict):
                self.assertNotEqual(argv.get("argv", [])[:3], ["kubectl", "patch", "deployment"])


class HarborImageRelocationTests(unittest.TestCase):
    registry = "main.example:32080"
    digest = "sha256:" + "a" * 64
    image_id = "sha256:" + "b" * 64
    old_image = "private.ecr.example/old-project/eva-app:old-version"

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.manifest = Path(self.temp.name) / "rendered.yaml"
        self.cluster = Cluster([{"name": "old-ecr"}])
        self.cluster.deployment["spec"]["template"]["spec"]["containers"] = [
            {"name": "eva-app", "image": self.old_image, "imagePullPolicy": "Always",
             "env": [{"name": "PRIVATE", "value": "never-copy-to-probe"}]}]
        self.cluster.deployment["spec"]["replicas"] = 1
        self.proposed = {"kind": "Deployment", "metadata": {"name": "eva-app"}, "spec": {"template": {"spec": {
            "containers": [{"name": "eva-app", "image": self.registry + "/new-project/eva-app:new-version"}],
            "imagePullSecrets": [{"name": "main-harbor"}]}}}}
        self.manifest.write_text(yaml.safe_dump(self.proposed))
        self.commands = []
        self.probe = None
        self.fail_wait = False
        self.mismatch = False
        self.no_digest = False

    def run_command(self, args, input=None, **kwargs):
        self.commands.append(args)
        if args[:3] == ["k3s", "crictl", "inspecti"]:
            ref = args[3]
            status = {"id": self.image_id, "repoDigests": [MODULE.repository(self.old_image) + "@" + self.digest]}
            if self.no_digest:
                status["repoDigests"] = []
            if self.mismatch and ref != self.old_image:
                status["id"] = "sha256:" + "c" * 64
            return subprocess.CompletedProcess(args, 0, json.dumps({"status": status}), "")
        if args == ["kubectl", "get", "nodes", "-o", "json"]:
            return subprocess.CompletedProcess(args, 0, json.dumps({"items": [{"metadata": {"name": "node-1"}}]}), "")
        command = args[3:] if args[:3] == ["kubectl", "-n", "eva-app"] else []
        if command[:2] == ["get", "deployment"]:
            return subprocess.CompletedProcess(args, 0, json.dumps(self.cluster.deployment), "")
        if command[:1] == ["create"]:
            self.probe = json.loads(input)
            return subprocess.CompletedProcess(args, 0, "created", "")
        if command[:1] == ["wait"]:
            return subprocess.CompletedProcess(args, int(self.fail_wait), "", "private-registry-credential")
        if command[:1] == ["delete"]:
            return subprocess.CompletedProcess(args, 0, "deleted", "")
        raise AssertionError(args)

    def prepare(self):
        return MODULE.prepare_harbor("eva-app", "eva-app", "app-uid", str(self.manifest), self.registry, run=self.run_command)

    def test_existing_bytes_are_verified_and_relocated_through_main_harbor_only(self):
        plan = self.prepare()
        expected = self.registry + "/new-project/eva-app@" + self.digest
        self.assertEqual(plan["images"], {"eva-app": expected})
        container = self.probe["spec"]["containers"][0]
        self.assertEqual(container["image"], expected)
        self.assertEqual(container["imagePullPolicy"], "Always")
        self.assertNotIn("env", container)
        self.assertNotIn("volumes", self.probe["spec"])
        self.assertFalse(self.probe["spec"]["automountServiceAccountToken"])
        self.assertEqual(self.probe["spec"]["imagePullSecrets"], [{"name": "main-harbor"}])
        self.assertFalse(any("pull" in command for command in self.commands))
        self.assertTrue(any("delete" in command for command in self.commands))
        self.assertEqual(self.cluster.deployment["spec"]["replicas"], 1)
        self.cluster.deployment["spec"]["replicas"] = 0
        old_env = copy.deepcopy(self.cluster.deployment["spec"]["template"]["spec"]["containers"][0]["env"])
        result = MODULE.reconcile("eva-app", "eva-app", "app-uid", plan["pull_references"], run=self.cluster.run,
                                  expected_images=plan["original_images"], replacement_images=plan["images"], registry=self.registry)
        self.assertTrue(result["changed"])
        current = self.cluster.deployment["spec"]["template"]["spec"]
        self.assertEqual(current["containers"][0]["image"], expected)
        self.assertEqual(current["containers"][0]["env"], old_env)
        self.assertEqual(current["containers"][0]["imagePullPolicy"], "IfNotPresent")
        self.assertEqual(current["imagePullSecrets"], [{"name": "main-harbor"}])

    def test_missing_harbor_image_never_falls_back_to_ecr_or_stops_existing_app(self):
        self.fail_wait = True
        before = copy.deepcopy(self.cluster.deployment)
        with self.assertRaisesRegex(MODULE.UnsafeReconciliation, "No external-registry fallback") as error:
            self.prepare()
        self.assertNotIn("private-registry-credential", str(error.exception))
        self.assertEqual(self.cluster.deployment, before)
        self.assertTrue(any("delete" in command for command in self.commands))
        self.assertFalse(any("pull" in command or "scale" in command or "patch" in command for command in self.commands))

    def test_mismatched_or_unverifiable_image_blocks_before_relocation(self):
        for scenario in ("no digest", "different content", "wrong registry"):
            with self.subTest(scenario=scenario):
                self.no_digest = scenario == "no digest"
                self.mismatch = scenario == "different content"
                proposed = copy.deepcopy(self.proposed)
                if scenario == "wrong registry":
                    proposed["spec"]["template"]["spec"]["containers"][0]["image"] = "ecr.example/app:new"
                self.manifest.write_text(yaml.safe_dump(proposed))
                with self.assertRaises(MODULE.UnsafeReconciliation):
                    self.prepare()
                self.assertEqual(self.cluster.deployment["spec"]["replicas"], 1)

    def test_rescue_refuses_ecr_images_even_if_cached_credentials_exist(self):
        self.cluster.deployment["spec"]["replicas"] = 0
        before = copy.deepcopy(self.cluster.deployment)
        with self.assertRaisesRegex(MODULE.UnsafeReconciliation, "outside the selected Harbor"):
            MODULE.reconcile("eva-app", "eva-app", "app-uid", [{"name": "old-ecr"}], run=self.cluster.run,
                             expected_images={"eva-app": self.old_image}, required_registry=self.registry)
        self.assertEqual(self.cluster.deployment, before)
        self.assertFalse(any(command[1] == "patch" for command, _ in self.cluster.calls))


if __name__ == "__main__":
    unittest.main(verbosity=2)
