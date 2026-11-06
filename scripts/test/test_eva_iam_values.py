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
    def find(tasks):
        for task in tasks:
            if task.get("name") == name:
                return task
            for section in ("block", "rescue", "always"):
                found = find(task.get(section, []))
                if found is not None:
                    return found
        return None

    task = find(TASKS)
    if task is None:
        raise AssertionError(f"task not found: {name}")
    return copy.deepcopy(task)


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
            "ansible_facts": {
                "env": {"HOME": str(self.root)},
                "default_ipv4": {"address": "192.0.2.10"},
            },
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
            "eva_iam_client_secret_retries": 2,
            "eva_iam_client_secret_retry_delay": 0,
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
printf '%s|%s\\n' "${KUBECONFIG:?}" "$*" >>"${FIXTURE_ROOT:?}/kubectl.log"
case "$1 $2" in
  "get nodes") echo fixture-node ;;
  "get svc") exit 0 ;;
    "rollout restart") echo 'restarted' ;;
  "rollout status") echo 'rollout complete' ;;
  "get all") echo resources ;;
    "exec -i")
        cat >/dev/null
        count=0
        if [[ -f "$FIXTURE_ROOT/fetch-count" ]]; then
            read -r count <"$FIXTURE_ROOT/fetch-count"
        fi
        count=$((count + 1))
        printf '%s\\n' "$count" >"$FIXTURE_ROOT/fetch-count"
        if [[ "$count" -le "${FETCH_FAILURES:-0}" ]]; then
            echo fixture-sensitive-fetch-error >&2
            exit 1
        fi
        if [[ "$count" -le "${FETCH_EMPTY:-0}" ]]; then
            printf '%s\\n' "${FETCH_EMPTY_OUTPUT-   }"
            exit 0
        fi
        echo fixture-generated-client-secret
        ;;
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
        self.assertNotIn("fixture-sensitive-fetch-error", output)
        self.assertNotIn("fixture-db-secret", output)
        return output

    def handoff_writer(self):
        # Run the real publication task without requiring root in the fixture.
        handoff = task_named("Write eva-iam.yaml (eva-app SSO hand-off) on control node")
        handoff["become"] = False
        handoff.pop("when")
        for field in ("owner", "group"):
            handoff["ansible.builtin.copy"].pop(field)
        return handoff

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
        kubectl = [line.split("|", 1) for line in (self.root / "kubectl.log").read_text().splitlines()]
        self.assertTrue(all(config == self.variables["eva_iam_kubeconfig"] for config, _ in kubectl))
        rollouts_and_fetch = ["exec" if command.startswith("exec ") else command
                              for _, command in kubectl
                              if command.startswith(("rollout ", "exec "))]
        installs = sum(command[:2] == ["upgrade", "--install"] for command in commands)
        self.assertEqual(rollouts_and_fetch, [
            "rollout restart deployment/custom-iam-keycloak deployment/custom-iam-dispatcher --namespace custom-sso",
            "rollout status deployment/custom-iam-keycloak -n custom-sso --timeout=600s",
            "rollout status deployment/custom-iam-dispatcher -n custom-sso --timeout=600s",
            "exec",
        ] * installs)
        return commands

    def test_defaults_and_pinned_chart(self):
        # Identical reinstall still replaces both workloads before secret fetch.
        for _ in range(2):
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
        handoff = self.handoff_writer()
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

    def test_client_secret_fetch_retries_startup_failures(self):
        handoff = self.root / "state/localhost/eva-iam.yaml"
        handoff.parent.mkdir(parents=True)
        self.run_play(
            [{"ansible.builtin.import_role": {"name": "eva_iam"}}, self.handoff_writer()],
            env={"FETCH_FAILURES": "2"},
        )
        self.assertEqual((self.root / "fetch-count").read_text().strip(), "3")
        self.assertEqual(yaml.safe_load(handoff.read_text())["sso"]["adminClientSecret"],
                         "fixture-generated-client-secret")

    def test_client_secret_fetch_retries_empty_success(self):
        for empty in ("", "   "):
            with self.subTest(output=repr(empty)):
                (self.root / "fetch-count").unlink(missing_ok=True)
                self.run_play(
                    [{"ansible.builtin.import_role": {"name": "eva_iam"}}],
                    env={"FETCH_EMPTY": "2", "FETCH_EMPTY_OUTPUT": empty},
                )
                self.assertEqual((self.root / "fetch-count").read_text().strip(), "3")

    def test_client_secret_failure_preserves_handoff_and_stops_iam(self):
        handoff = self.root / "state/localhost/eva-iam.yaml"
        handoff.parent.mkdir(parents=True)
        existing = "existing-private-handoff\n"
        handoff.write_text(existing)
        handoff.chmod(0o600)
        output = self.run_play([
            {"ansible.builtin.import_role": {"name": "eva_iam"}}, self.handoff_writer(),
        ], env={"FETCH_FAILURES": "99"}, succeeds=False)
        self.assertEqual((self.root / "fetch-count").read_text().strip(), "3")
        self.assertEqual(handoff.read_text(), existing)
        self.assertEqual(handoff.stat().st_mode & 0o777, 0o600)
        self.assertIn("IAM client Secret 조회에 실패", output)
        self.assertNotIn("Show eva-iam deployment summary", output)

    def test_empty_client_secret_stops_before_new_handoff(self):
        handoff = self.root / "state/localhost/eva-iam.yaml"
        handoff.parent.mkdir(parents=True)
        output = self.run_play([
            {"ansible.builtin.import_role": {"name": "eva_iam"}}, self.handoff_writer(),
        ], env={"FETCH_EMPTY": "99", "FETCH_EMPTY_OUTPUT": ""}, succeeds=False)
        self.assertEqual((self.root / "fetch-count").read_text().strip(), "3")
        self.assertFalse(handoff.exists())
        self.assertIn("IAM client Secret 조회에 실패", output)

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
            "Restart EVA IAM deployments to load installed configuration",
            "Wait for keycloak rollout",
            "Wait for dispatcher rollout",
            "Wait for Keycloak Admin API and read the generated client secret",
            "Ensure persistent IAM handoff directories exist on control node",
            "Write eva-iam.yaml (eva-app SSO hand-off) on control node",
        ]
        self.run_play([task_named(name) for name in names], check=True)
        self.assertEqual(path.read_text(), "previous: true\n")
        self.assertEqual(list(self.rendered.iterdir()), [path])
        self.assertFalse((self.root / "helm.log").exists())
        self.assertFalse((self.root / "kubectl.log").exists())

    def test_public_host_defaults_without_fact_injection(self):
        cases = [
            ("missing-ipv4", {}, {}, "127.0.0.1"),
            ("missing-address", {"default_ipv4": {}}, {}, "127.0.0.1"),
            ("detected-ipv4", {"default_ipv4": {"address": "192.0.2.10"}}, {}, "192.0.2.10"),
            ("explicit-host", {"default_ipv4": {"address": "192.0.2.10"}},
             {"eva_app_backend_host": "public.example.test"}, "public.example.test"),
            ("explicit-host-without-ipv4", {},
             {"eva_app_backend_host": "public.example.test"}, "public.example.test"),
        ]
        for role, source, name, host_fact in (
            ("config", "config/tasks/eva.yaml", "Build eva.yaml content", "config_eva_backend_host"),
            ("app", "eva_app/tasks/main.yaml", "Set EVA App install defaults (on-premise)",
             "eva_app_backend_host"),
        ):
            tasks = yaml.safe_load((ROLES / source).read_text())
            builder = next(task for task in tasks if task.get("name") == name)
            for scenario, facts, overrides, expected in cases:
                with self.subTest(role=role, scenario=scenario):
                    variables = {
                        "ansible_facts": {"env": {"HOME": str(self.root)}, **facts},
                        "config_effective_gpu_count": 0,
                        "eva_site_config_root": str(self.root),
                        "eva_app_chart_version": "3.3.0-beta.9",
                        "eva_app_deploy_version": "3.3.0-beta.9",
                        **overrides,
                    }
                    output = self.run_play([
                        builder,
                        {"ansible.builtin.assert": {"that": [f"{host_fact} == '{expected}'"]}},
                    ], variables, env={"ANSIBLE_INJECT_FACT_VARS": "False"})
                    self.assertNotIn("INJECT_FACTS_AS_VARS", output)


if __name__ == "__main__":
    unittest.main(verbosity=2)
