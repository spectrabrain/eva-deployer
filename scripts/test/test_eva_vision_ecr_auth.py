#!/usr/bin/env python3
"""ECR preparation and renewal tests with local AWS/kubectl/systemctl fixtures."""

import base64
import copy
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import yaml


sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
ROLE = ROOT / "src/solution/roles/eva_vision"
HELPER = ROLE / "files/ecr_pull_secret.py"
REGISTRY = "211722900905.dkr.ecr.ap-northeast-2.amazonaws.com"
REPOSITORY = REGISTRY + "/spectrabrain/eva-vision"
TOKEN = "fixture-private-ecr-token-DO-NOT-PRINT"

spec = importlib.util.spec_from_file_location("vision_ecr", HELPER)
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)


def config_token(token):
    return {"auth": base64.b64encode(("AWS:" + token).encode()).decode()}


def secret_fixture(name="ecr-registry", registry=REGISTRY, managed=False):
    annotations = {"operator.example/note": "preserve"}
    if managed:
        annotations[helper.MANAGED_REGISTRY] = registry
    config = {"auths": {registry: config_token("old-token"), "other.example": {"auth": "preserve"}},
              "extra": {"preserve": True}}
    return {"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
            "metadata": {"name": name, "namespace": "eva-vision", "resourceVersion": "1", "annotations": annotations},
            "data": {".dockerconfigjson": base64.b64encode(json.dumps(config).encode()).decode(), "extra": "cHJlc2VydmU="}}


FAKE_TOOL = r'''
import json, os, pathlib, sys
state = pathlib.Path(os.environ["ECR_FIXTURE"])
args = sys.argv[1:]
tool = pathlib.Path(sys.argv[0]).name
with (state / "calls.jsonl").open("a") as output:
    output.write(json.dumps({"tool": tool, "args": args}) + "\n")
assert os.environ["ECR_TOKEN"] not in " ".join(args), "Token must never enter process arguments"
if tool == "getent":
    assert args == ["passwd", "vision-user"]
    print("vision-user:x:1000:1000:fixture:" + str(state / "home") + ":/bin/bash")
elif tool == "aws":
    assert args[:3] == ["ecr", "get-login-password", "--region"], args
    assert args[3] == os.environ.get("ECR_REGION", "ap-northeast-2")
    assert args[4:] == ["--profile", "default"], args
    assert os.environ["HOME"] == str(state / "home")
    assert os.environ["AWS_SHARED_CREDENTIALS_FILE"] == str(state / "home/.aws/credentials")
    if os.environ.get("ECR_AWS_FAIL") == "1":
        raise SystemExit(os.environ["ECR_TOKEN"])
    print(os.environ["ECR_TOKEN"])
elif tool == "kubectl":
    secret = state / "secret.json"
    if args[:3] == ["-n", "eva-vision", "get"]:
        assert args[3:] == ["secret", os.environ["ECR_SECRET"], "--ignore-not-found=true", "-o", "json"], args
        if secret.exists():
            print(secret.read_text())
    elif args == ["apply", "-f", "-"]:
        assert json.load(sys.stdin)["kind"] == "Namespace"
        print("namespace/eva-vision unchanged")
    elif args == ["create", "-f", "-"]:
        data = json.load(sys.stdin)
        assert data["metadata"]["namespace"] == "eva-vision"
        assert data["metadata"]["name"] == os.environ["ECR_SECRET"]
        if os.environ.get("ECR_CREATE_RACE") == "1":
            secret.write_text('{"concurrent":"preserve"}')
            raise SystemExit("Concurrent Secret creator")
        assert not secret.exists()
        data["metadata"]["resourceVersion"] = "1"
        secret.write_text(json.dumps(data))
        print("secret/ecr-registry created")
    elif args[:3] == ["-n", "eva-vision", "patch"]:
        assert args[3:] == ["secret", os.environ["ECR_SECRET"], "--type=merge", "--patch-file=/dev/stdin"], args
        data = json.load(sys.stdin)
        live = json.loads(secret.read_text())
        assert data["metadata"]["resourceVersion"] == live["metadata"]["resourceVersion"]
        live["data"].update(data["data"])
        live["metadata"]["annotations"].update(data["metadata"]["annotations"])
        live["metadata"]["resourceVersion"] = "2"
        secret.write_text(json.dumps(live))
        print("secret/ecr-registry patched")
    else:
        raise SystemExit("Unexpected Kubernetes mutation: " + str(args))
elif tool == "systemctl":
    command = args[0]
    if command == "show":
        if os.environ.get("ECR_OLD_TIMER") == "1":
            print("LoadState=loaded\nActiveState=active\nUnitFileState=enabled\nCanReload=no")
        else:
            print("LoadState=loaded\nActiveState=inactive\nUnitFileState=disabled\nCanReload=no")
    elif command == "is-enabled":
        if os.environ.get("ECR_OLD_TIMER") == "1":
            print("enabled")
            raise SystemExit(0)
        print("disabled")
        raise SystemExit(1)
    elif command not in {"daemon-reload", "enable", "disable", "start", "stop"}:
        raise SystemExit("Unexpected systemctl operation: " + str(args))
else:
    raise SystemExit("Unexpected fixture tool")
'''


class ECRTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="eva-vision-ecr-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        (self.root / "home").mkdir()
        (self.root / "units").mkdir()
        self.env = {**os.environ, "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
                    "ECR_FIXTURE": str(self.root), "ECR_TOKEN": TOKEN, "ECR_SECRET": "ecr-registry",
                    "HOME": str(self.root / "home"), "AWS_SHARED_CREDENTIALS_FILE": str(self.root / "home/.aws/credentials"),
                    "AWS_PROFILE": "default",
                    "ANSIBLE_NOCOLOR": "1", "ANSIBLE_STDOUT_CALLBACK": "default", "PYTHONDONTWRITEBYTECODE": "1"}
        for tool in ("aws", "kubectl", "getent", "systemctl"):
            executable = self.bin / tool
            executable.write_text("#!" + sys.executable + "\n" + FAKE_TOOL)
            executable.chmod(0o700)

    def existing(self, name="ecr-registry", **kwargs):
        self.env["ECR_SECRET"] = name
        document = secret_fixture(name, **kwargs)
        (self.root / "secret.json").write_text(json.dumps(document))
        return copy.deepcopy(document)

    def calls(self, tool=None):
        path = self.root / "calls.jsonl"
        calls = [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []
        return [item for item in calls if tool is None or item["tool"] == tool]

    def invoke(self, *, ok=True, repository=REPOSITORY, renew_only=False):
        result = subprocess.run([sys.executable, str(HELPER), "--namespace", "eva-vision", "--secret", self.env["ECR_SECRET"],
                                 "--repository", repository] + (["--renew-only"] if renew_only else []),
                                env=self.env, capture_output=True, text=True, timeout=15)
        self.assertEqual(result.returncode == 0, ok, result.stdout + result.stderr)
        self.assertNotIn(TOKEN, result.stdout + result.stderr)
        self.assertNotIn("Traceback", result.stderr)
        return json.loads(result.stdout) if ok else result

    def assert_auth(self):
        document = json.loads((self.root / "secret.json").read_text())
        config = helper.read_config(document)
        self.assertEqual(config["auths"][REGISTRY], config_token(TOKEN))
        self.assertEqual(document["metadata"]["annotations"][helper.MANAGED_REGISTRY], REGISTRY)
        return document, config

    def test_fresh_secret_is_created_without_tokens_in_argv(self):
        self.assertTrue(self.invoke()["managed"])
        self.assert_auth()
        self.assertEqual(len(self.calls("aws")), 1)
        self.assertFalse(any(item["args"][:3] == ["-n", "eva-vision", "patch"] for item in self.calls()))

    def test_canonical_secret_without_ownership_is_renewed_and_preserves_other_data(self):
        before = self.existing()
        self.assertTrue(self.invoke()["changed"])
        after, config = self.assert_auth()
        self.assertEqual(after["data"]["extra"], before["data"]["extra"])
        self.assertEqual(after["metadata"]["annotations"]["operator.example/note"], "preserve")
        self.assertEqual(config["auths"]["other.example"], {"auth": "preserve"})
        self.assertEqual(config["extra"], {"preserve": True})

    def test_renewal_is_idempotent_for_the_same_token(self):
        self.invoke()
        self.assertFalse(self.invoke()["changed"])

    def test_periodic_renewal_never_recreates_a_deleted_secret(self):
        self.assertFalse(self.invoke(renew_only=True)["managed"])
        self.assertEqual(self.calls("aws"), [])
        self.assertFalse((self.root / "secret.json").exists())
        self.assertEqual(len(self.calls("kubectl")), 1)

    def test_periodic_renewal_preserves_operator_replacements(self):
        before = self.existing()
        self.assertFalse(self.invoke(renew_only=True)["managed"])
        self.assertEqual(json.loads((self.root / "secret.json").read_text()), before)
        self.assertEqual(self.calls("aws"), [])

    def test_periodic_renewal_updates_only_the_recorded_registry(self):
        self.existing(managed=True)
        self.assertTrue(self.invoke(renew_only=True)["changed"])
        self.assert_auth()

    def test_custom_external_secret_is_reused_without_aws_or_writes(self):
        before = self.existing("operator-pull-secret")
        self.assertFalse(self.invoke()["managed"])
        self.assertEqual(self.calls("aws"), [])
        self.assertEqual(json.loads((self.root / "secret.json").read_text()), before)
        self.assertEqual(len(self.calls("kubectl")), 1)

    def test_missing_custom_secret_is_not_created(self):
        self.env["ECR_SECRET"] = "operator-pull-secret"
        self.invoke(ok=False)
        self.assertEqual(self.calls("aws"), [])
        self.assertFalse((self.root / "secret.json").exists())

    def test_repository_account_and_region_follow_the_effective_image(self):
        self.env["ECR_REGION"] = "us-east-1"
        self.invoke(repository="123456789012.dkr.ecr.us-east-1.amazonaws.com/custom/vision")
        document = json.loads((self.root / "secret.json").read_text())
        self.assertTrue(helper.has_credentials(helper.read_config(document), "123456789012.dkr.ecr.us-east-1.amazonaws.com"))

    def test_aws_failure_preserves_existing_credentials_and_sanitizes_output(self):
        before = self.existing()
        self.env["ECR_AWS_FAIL"] = "1"
        self.invoke(ok=False)
        self.assertEqual(json.loads((self.root / "secret.json").read_text()), before)
        self.assertEqual(len(self.calls("kubectl")), 1)

    def test_create_race_does_not_overwrite_another_writer(self):
        self.env["ECR_CREATE_RACE"] = "1"
        self.invoke(ok=False)
        self.assertEqual(json.loads((self.root / "secret.json").read_text()), {"concurrent": "preserve"})

    def test_invalid_secret_documents_are_rejected_without_aws(self):
        for document in ([], {"type": "Opaque"}, {"type": "kubernetes.io/dockerconfigjson", "data": {}},
                         {"type": "kubernetes.io/dockerconfigjson", "data": {".dockerconfigjson": "invalid"}}):
            with self.subTest(document=document):
                (self.root / "secret.json").write_text(json.dumps(document))
                self.invoke(ok=False)
        self.assertEqual(self.calls("aws"), [])

    def run_role(self, mode="online_repository", *, enabled=True, create=False, repository=REPOSITORY, ok=True, old_timer=False):
        if old_timer:
            self.env["ECR_OLD_TIMER"] = "1"
            (self.root / "units/eva-vision-ecr-eva-vision-eva-vision.timer").write_text("fixture previous timer")
        playbook = self.root / "play.yaml"
        variables = {"repository_mode": mode, "ansible_user": "vision-user", "eva_vision_namespace": "eva-vision",
                     "eva_vision_name": "eva-vision", "eva_vision_kubeconfig": str(self.root / "kubeconfig"),
                     "eva_vision_ecr_refresh_dir": str(self.root / "persistent"), "eva_vision_ecr_systemd_dir": str(self.root / "units"),
                     "eva_vision_runtime_values": {"image": {"repository": repository},
                         "imagePullSecrets": {"enabled": enabled, "create": create, "ExistingSecret": self.env["ECR_SECRET"]}}}
        playbook.write_text(yaml.safe_dump([{"hosts": "localhost", "connection": "local", "gather_facts": False,
            "vars": variables, "tasks": [{"ansible.builtin.include_role": {"name": "eva_vision", "tasks_from": "image_pull_secret.yaml"}}]}]))
        ansible = (os.environ.get("ANSIBLE_PLAYBOOK") or shutil.which("ansible-playbook")
                   or str(Path(sys.executable).with_name("ansible-playbook")))
        # pip --user installs belong to the controller HOME; the role sets the target AWS HOME.
        env = {**self.env, "HOME": os.environ.get("HOME", str(Path.home())),
               "ANSIBLE_ROLES_PATH": str(ROLE.parent)}
        result = subprocess.run([ansible, "-i", "localhost,", str(playbook)], env=env, capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode == 0, ok, result.stdout + result.stderr)
        self.assertNotIn(TOKEN, result.stdout + result.stderr)
        return result

    def test_online_role_creates_secret_and_installs_six_hour_renewal(self):
        self.run_role()
        self.assert_auth()
        service = (self.root / "units/eva-vision-ecr-eva-vision-eva-vision.service").read_text()
        timer = (self.root / "units/eva-vision-ecr-eva-vision-eva-vision.timer").read_text()
        self.assertIn("OnUnitActiveSec=6h", timer)
        self.assertIn("Restart=on-failure", service)
        self.assertIn("--renew-only", service)
        self.assertIn(str(self.bin / "kubectl"), service)
        self.assertIn(str(self.root / "home/.aws/credentials"), service)
        self.assertNotIn(TOKEN, service + timer)
        self.assertTrue(any(item["args"][0] == "start" for item in self.calls("systemctl")))
        if shutil.which("systemd-analyze"):
            result = subprocess.run(["systemd-analyze", "verify", str(self.root / "units/eva-vision-ecr-eva-vision-eva-vision.service"),
                                     str(self.root / "units/eva-vision-ecr-eva-vision-eva-vision.timer")], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_role_preserves_controller_home_and_target_aws_home(self):
        ansible = (os.environ.get("ANSIBLE_PLAYBOOK") or shutil.which("ansible-playbook")
                   or str(Path(sys.executable).with_name("ansible-playbook")))
        controller_home = os.environ.get("HOME", str(Path.home()))
        launcher = self.root / "ansible-controller-home"
        launcher.write_text(
            "#!" + sys.executable + "\nimport os, sys\n"
            f"assert os.environ['HOME'] == {controller_home!r}, 'Ansible must retain the controller HOME'\n"
            f"os.execvp({ansible!r}, [{ansible!r}, *sys.argv[1:]])\n"
        )
        launcher.chmod(0o700)
        with patch.dict(os.environ, {"ANSIBLE_PLAYBOOK": str(launcher)}):
            self.run_role()
        self.assert_auth()
        self.assertEqual(self.env["HOME"], str(self.root / "home"))
        self.assertEqual(len(self.calls("aws")), 1)

    def test_online_custom_secret_does_not_install_renewal(self):
        before = self.existing("operator-pull-secret")
        self.run_role()
        self.assertEqual(self.calls("aws"), [])
        self.assertEqual(self.calls("systemctl"), [])
        self.assertEqual(json.loads((self.root / "secret.json").read_text()), before)
        self.assertFalse(list((self.root / "units").iterdir()))

    def test_cloud_alias_renews_existing_secret(self):
        self.existing()
        self.run_role("cloud_repository")
        self.assert_auth()

    def test_switching_to_custom_secret_stops_previous_default_renewal(self):
        before = self.existing("operator-pull-secret")
        self.run_role(old_timer=True)
        self.assertEqual(self.calls("aws"), [])
        self.assertEqual(json.loads((self.root / "secret.json").read_text()), before)
        self.assertTrue(any(item["args"][0] == "disable" for item in self.calls("systemctl")))
        self.assertFalse(any(item["args"][0] == "start" for item in self.calls("systemctl")))

    def test_remote_mode_disables_previous_online_renewal_without_accessing_aws(self):
        self.run_role("remote_repository", old_timer=True, enabled=False)
        self.assertEqual(self.calls("aws"), [])
        self.assertEqual(self.calls("kubectl"), [])
        self.assertFalse((self.root / "persistent").exists())
        self.assertTrue(any(item["args"][0] == "disable" for item in self.calls("systemctl")))

    def test_local_disabled_chart_created_and_non_ecr_paths_do_not_require_aws(self):
        for mode, enabled, create, repository in (("local_repository", False, False, REPOSITORY),
                                                  ("online_repository", False, False, REPOSITORY),
                                                  ("online_repository", True, True, REPOSITORY),
                                                  ("online_repository", True, False, "harbor.example/eva/vision")):
            with self.subTest(mode=mode, enabled=enabled, create=create, repository=repository):
                self.run_role(mode, enabled=enabled, create=create, repository=repository)
        self.assertEqual(self.calls(), [])

    def test_failed_initial_ecr_login_never_reaches_renewal_setup(self):
        self.env["ECR_AWS_FAIL"] = "1"
        result = self.run_role(ok=False)
        self.assertIn("Cannot obtain an ECR login token", result.stdout)
        self.assertFalse((self.root / "secret.json").exists())
        self.assertFalse(list((self.root / "units").iterdir()))
        self.assertEqual(self.calls("systemctl"), [])

    def test_failed_login_keeps_previous_renewal_and_credentials_unchanged(self):
        before = self.existing()
        self.env["ECR_AWS_FAIL"] = "1"
        self.run_role(ok=False, old_timer=True)
        self.assertEqual(self.calls("systemctl"), [])
        self.assertEqual(json.loads((self.root / "secret.json").read_text()), before)

    def test_authentication_precedes_workload_shutdown(self):
        names = [task["name"] for task in yaml.safe_load((ROLE / "tasks/main.yaml").read_text())]
        self.assertLess(names.index("Prepare EVA Vision external model runtime"), names.index("Prepare EVA Vision image pull authentication"))
        self.assertLess(names.index("Prepare EVA Vision image pull authentication"), names.index("Check existing EVA Vision deployment"))


if __name__ == "__main__":
    unittest.main()
