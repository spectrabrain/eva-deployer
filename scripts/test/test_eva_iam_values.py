#!/usr/bin/env python3
"""Run real IAM role/artifact tasks with local fake Helm/Kubernetes commands."""

import copy
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml


REPO = Path(__file__).resolve().parents[2]
ROLES = REPO / "src/solution/roles"
TASKS = yaml.safe_load((ROLES / "eva_iam/tasks/main.yaml").read_text())


def task_named(name):
    return copy.deepcopy(next(task for task in TASKS if task.get("name") == name))


class IAMValuesTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.rendered = self.root / "rendered"
        self.defaults = {
            "config": {"host": "chart.example.test"},
            "keycloak": {"realmPatch": {"evaApp": {"clientId": "eva-app"}}},
            "ingress": {"tls": {"enabled": True}},
            "database": {"name": "keycloak"},
            "redis": {},
            "dispatcher": {},
            "imagePullSecrets": {},
            "persistence": {},
            "pv": {},
            "serviceAccount": {},
        }
        (self.root / "defaults.yaml").write_text(yaml.safe_dump(self.defaults))
        (self.root / "inventory.ini").write_text(
            "localhost ansible_connection=local ansible_become=false\n"
        )
        self.variables = {
            "ansible_facts": {"env": {"HOME": str(self.root)}},
            "ansible_default_ipv4": {"address": "192.0.2.10"},
            "eva_site_id": "fixture",
            "eva_site_values_root": str(self.root / "site-values"),
            "eva_site_deploy_root": str(self.root / "deploy"),
            "eva_site_state_root": str(self.root / "state"),
            "eva_cache_root": str(self.root / "cache"),
            "eva_template_values_root": str(REPO / "src/solution/values"),
            "eva_iam_workdir": str(self.root / "work"),
            "eva_iam_deploy_values_dir": str(self.rendered),
            "eva_iam_chart_version": "3.3.0-beta.1",
            "eva_iam_kubeconfig": str(self.root / "kubeconfig"),
            "eva_iam_host": "iam.example.test",
            "eva_iam_name": "custom-iam",
            "eva_iam_namespace": "custom-sso",
            "eva_iam_write_app_config": False,
            "eva_traefik_ports_manage": False,
            "deploy_versions": {"eva_iam_chart_version": "3.3.0-beta.1"},
        }
        self.write_command("helm", '''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys
import yaml
root = Path(os.environ["FIXTURE_ROOT"])
args = sys.argv[1:]
with (root / "helm.log").open("a") as log:
    log.write(json.dumps(args) + "\\n")
if args[:2] == ["list", "--namespace"]:
    print("[]")
elif args[:2] == ["show", "values"]:
    print((root / "defaults.yaml").read_text())
elif args[:2] == ["upgrade", "--install"]:
    if os.environ.get("FAIL_INSTALL"):
        sys.exit(42)
    values = {}
    def merge(left, right):
        for key, value in right.items():
            if isinstance(value, dict) and isinstance(left.get(key), dict):
                merge(left[key], value)
            else:
                left[key] = value
    for index, arg in enumerate(args):
        if arg == "-f":
            merge(values, yaml.safe_load(Path(args[index + 1]).read_text()) or {})
        elif arg == "--set":
            key, value = args[index + 1].split("=", 1)
            cursor = values
            keys = key.split(".")
            for part in keys[:-1]:
                cursor = cursor.setdefault(part, {})
            cursor[keys[-1]] = yaml.safe_load(value)
    (root / "installed.yaml").write_text(yaml.safe_dump(values))
    print("has been installed")
elif args[:2] == ["get", "values"]:
    if os.environ.get("FAIL_CAPTURE"):
        print("fixture read failed", file=sys.stderr)
        sys.exit(43)
    print((root / "installed.yaml").read_text())
''')
        self.write_command("kubectl", '''#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  "get nodes") echo fixture-node ;;
  "get svc") exit 0 ;;
  "rollout status") echo 'rollout complete' ;;
  "get all") echo resources ;;
  "exec -i") cat >/dev/null; echo fixture-generated-client-secret ;;
  *) echo "Unexpected fixture kubectl command" >&2; exit 99 ;;
esac
''')

    def write_command(self, name, content):
        path = self.bin / name
        path.write_text(content)
        path.chmod(0o755)

    def run_play(self, tasks, variables=None, *, check=False, env=None, succeeds=True):
        executable = os.environ.get("ANSIBLE_PLAYBOOK", "ansible-playbook")
        if not shutil.which(executable):
            self.fail(f"ansible-playbook not found: {executable}")
        play = [{
            "hosts": "all", "gather_facts": False,
            "vars": {**self.variables, **(variables or {})}, "tasks": tasks,
        }]
        path = self.root / "playbook.yaml"
        path.write_text(yaml.safe_dump(play, sort_keys=False))
        result = subprocess.run(
            [executable, "-i", str(self.root / "inventory.ini"), str(path)]
            + (["--check"] if check else []),
            env={**os.environ, "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
                 "ANSIBLE_ROLES_PATH": str(ROLES), "FIXTURE_ROOT": str(self.root),
                 **(env or {})},
            capture_output=True, text=True, timeout=180,
        )
        output = result.stdout + result.stderr
        self.assertEqual(result.returncode == 0, succeeds, output[-12000:])
        self.assertNotIn("fixture-generated-client-secret", output)
        self.assertNotIn("fixture-db-secret", output)
        return output

    def read_values(self, name):
        return yaml.safe_load((self.rendered / name).read_text())

    def assert_artifacts(self):
        for name in ("chart-defaults.yaml", "eva-iam-k3s-override.yaml",
                     "effective-input-values.yaml", "values-sources.yaml", "resolved-values.yaml"):
            self.assertEqual((self.rendered / name).stat().st_mode & 0o777, 0o600, name)
        self.assertEqual(self.read_values("chart-defaults.yaml"), self.defaults)
        sources = self.read_values("values-sources.yaml")
        for path in sources["rendered_artifacts"].values():
            self.assertTrue(Path(path).is_file(), path)
        self.assertNotIn("fixture-db-secret", (self.rendered / "values-sources.yaml").read_text())
        commands = [json.loads(line) for line in (self.root / "helm.log").read_text().splitlines()]
        self.assertIn(["get", "values", "custom-iam", "--namespace", "custom-sso",
                       "--all", "--output", "yaml"], commands)
        return commands

    def test_defaults_and_pinned_chart(self):
        self.run_play([{"ansible.builtin.import_role": {"name": "eva_iam"}}], {
            "eva_iam_values_file": str(self.root / "missing.yaml"),
        })
        commands = self.assert_artifacts()
        self.assertIn(["show", "values", "eva-iam/eva-iam", "--version", "3.3.0-beta.1"], commands)
        effective = self.read_values("effective-input-values.yaml")
        resolved = self.read_values("resolved-values.yaml")
        self.assertNotIn("name", effective["database"])
        self.assertEqual(resolved["database"]["name"], "keycloak")
        self.assertEqual(resolved["keycloak"]["realmPatch"]["evaApp"]["clientId"], "eva-app")
        sources = self.read_values("values-sources.yaml")
        self.assertFalse(sources["workspace_override"])
        self.assertFalse(sources["cli_override"])

    def test_workspace_cli_set_and_handoff(self):
        workspace = self.root / "iam.yaml"
        workspace.write_text(yaml.safe_dump({"localhost": {
            "eva_iam_deploy": {"db_nodeport": 32100},
            "config": {"host": "workspace.example.test"},
            "ingress": {"tls": {"enabled": False}},
            "database": {"internal": {"password": "fixture-db-secret"}},
            "keycloak": {"realmPatch": {"evaApp": {"clientId": "workspace-client"}}},
        }}))
        cli = self.root / "cli.yaml"
        cli.write_text(yaml.safe_dump({"config": {"host": "cli.example.test"}}))
        chart = self.root / "custom-chart.tgz"
        chart.touch()
        # Execute the real handoff copy without root ownership requirements.
        # The full role still performs the real Keycloak fetch/identity resolution.
        handoff = task_named("Write eva-iam.yaml (eva-app SSO hand-off) on control node")
        handoff["become"] = False
        handoff.pop("when")
        for field in ("owner", "group"):
            handoff["ansible.builtin.copy"].pop(field)
        (self.root / "state/localhost").mkdir(parents=True)
        self.run_play([
            {"ansible.builtin.import_role": {"name": "eva_iam"}}, handoff,
        ], {
            "eva_iam_values_file": str(workspace), "eva_cli_values_path": str(cli),
            "eva_cli_chart_path": str(chart),
            "eva_cli_helm_set": ["config.host=set.example.test", "keycloak.realmPatch.evaApp.clientId=set-client"],
        })
        commands = self.assert_artifacts()
        self.assertIn(["show", "values", str(self.root / "work/cli-chart.tgz")], commands)
        effective = self.read_values("effective-input-values.yaml")
        resolved = self.read_values("resolved-values.yaml")
        self.assertEqual(effective["config"]["host"], "cli.example.test")
        self.assertEqual(resolved["config"]["host"], "set.example.test")
        self.assertEqual(effective["database"]["internal"]["service"]["nodePort"], 32100)
        self.assertEqual(effective["database"]["internal"]["password"], "fixture-db-secret")
        self.assertNotIn("eva_iam_deploy", effective)
        sso = yaml.safe_load((self.root / "state/localhost/eva-iam.yaml").read_text())["sso"]
        self.assertEqual(sso["realm"], "eva-iam")
        self.assertEqual(sso["clientId"], "set-client")
        self.assertEqual(sso["adminClientId"], "eva-admin-api")
        self.assertEqual(sso["adminClientSecret"], "fixture-generated-client-secret")
        self.assertTrue(self.read_values("values-sources.yaml")["workspace_override"])
        self.assertTrue(self.read_values("values-sources.yaml")["cli_override"])

    def test_capture_failure_does_not_leave_stale_resolved(self):
        self.rendered.mkdir()
        path = self.rendered / "resolved-values.yaml"
        path.write_text("stale: true\n")
        self.run_play([
            task_named("Clear previous EVA IAM resolved values before deployment"),
            task_named("Capture effective EVA IAM Helm values"),
            task_named("Record private EVA IAM resolved values on control node"),
        ], env={"FAIL_CAPTURE": "1"}, succeeds=False)
        self.assertFalse(path.exists())

    def test_artifact_tasks_do_not_write_in_check_mode(self):
        self.rendered.mkdir()
        path = self.rendered / "resolved-values.yaml"
        path.write_text("previous: true\n")
        names = [
            "Clear previous EVA IAM resolved values before deployment",
            "Record EVA IAM chart defaults on control node",
            "Render eva-iam k3s override values on control node",
            "Resolve EVA IAM effective input values",
            "Record private EVA IAM effective input values on control node",
            "Record EVA IAM values source metadata on control node",
            "Capture effective EVA IAM Helm values",
            "Record private EVA IAM resolved values on control node",
            "Resolve deployed EVA IAM App client identity",
        ]
        self.run_play([task_named(name) for name in names], check=True)
        self.assertEqual(path.read_text(), "previous: true\n")
        self.assertEqual(list(self.rendered.iterdir()), [path])
        self.assertFalse((self.root / "helm.log").exists())

    def test_config_summary_host_uses_public_address_not_inventory_alias(self):
        tasks = yaml.safe_load((ROLES / "config/tasks/eva.yaml").read_text())
        builder = next(task for task in tasks if task.get("name") == "Build eva.yaml content")
        self.run_play([builder, {"ansible.builtin.assert": {"that": [
            "config_eva_backend_host == '192.0.2.10'",
        ]}}], {"config_effective_gpu_count": 0, "eva_site_config_root": str(self.root)})
        self.run_play([builder, {"ansible.builtin.assert": {"that": [
            "config_eva_backend_host == 'public.example.test'",
        ]}}], {"config_effective_gpu_count": 0, "eva_site_config_root": str(self.root),
               "eva_app_backend_host": "public.example.test"})


if __name__ == "__main__":
    unittest.main(verbosity=2)