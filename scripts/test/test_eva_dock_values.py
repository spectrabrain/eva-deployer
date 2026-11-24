#!/usr/bin/env python3
"""Run the real EVA Dock role against the published chart with a fake cluster.

Helm rendering is real (helm template on the selected chart archive); only the
Kubernetes API and Helm release storage are replaced. Provide the chart with
EVA_DOCK_TEST_CHART or prepare out/cache/eva-dock/eva-dock-<version>.tgz.
"""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

import yaml


REPO = Path(__file__).resolve().parents[2]
ROLES = REPO / "src/solution/roles"
CATALOG = yaml.safe_load((REPO / "src/solution/version.yaml").read_text())
CHART = Path(os.environ.get("EVA_DOCK_TEST_CHART") or
             REPO / "out/cache/eva-dock" / f"eva-dock-{CATALOG['eva_dock_chart_version']}.tgz")
sys.path.insert(0, str(ROLES / "eva_traefik_ports/files"))
import traefik_ports  # noqa: E402


FAKE_HELM = r'''#!/usr/bin/env python3
import json, os, subprocess, sys
from pathlib import Path
root = Path(os.environ["FIXTURE_ROOT"])
real, chart = os.environ["REAL_HELM"], os.environ["FIXTURE_CHART"]
args = sys.argv[1:]
with (root / "helm.log").open("a") as log:
    log.write(json.dumps(args) + "\n")
def real_helm(arguments):
    result = subprocess.run([real] + arguments, capture_output=True, text=True)
    sys.stderr.write(result.stderr)
    if result.returncode:
        sys.exit(result.returncode)
    return result.stdout
if args[:1] == ["list"]:
    print("[]")
elif args[:1] == ["repo"]:
    print("ok")
elif args[:2] == ["show", "values"]:
    print(real_helm(["show", "values", chart]))
elif args[:2] == ["upgrade", "--install"]:
    name, namespace = args[2], args[args.index("-n") + 1]
    options = []
    for index, arg in enumerate(args):
        if arg in ("-f", "--set"):
            options += [arg, args[index + 1]]
    (root / "rendered.yaml").write_text(real_helm(["template", name, chart, "-n", namespace] + options))
    (root / "installed-values.json").write_text(json.dumps(options))
    print("Release has been installed")
elif args[:2] == ["get", "values"]:
    print("installed: true")
else:
    print("unexpected fake helm command", file=sys.stderr)
    sys.exit(99)
'''

FAKE_KUBECTL = r'''#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${FIXTURE_ROOT:?}/kubectl.log"
case "$1 $2" in
  "get nodes") echo fixture-node ;;
  "get svc") exit 0 ;;
  "rollout restart"|"rollout status") echo ok ;;
  "get all,ingress") echo resources ;;
  "create secret") echo "apiVersion: v1" ;;
  "apply --filename=-") cat >/dev/null; echo created ;;
  *) echo "unexpected fake kubectl command: $*" >&2; exit 99 ;;
esac
'''


@unittest.skipUnless(CHART.is_file(), f"EVA Dock chart is unavailable: {CHART}")
@unittest.skipUnless(shutil.which("helm") and shutil.which(os.environ.get("ANSIBLE_PLAYBOOK", "ansible-playbook")),
                     "helm and ansible-playbook are required")
class DockRoleTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        for name, content in (("helm", FAKE_HELM), ("kubectl", FAKE_KUBECTL)):
            (self.bin / name).write_text(content)
            (self.bin / name).chmod(0o755)
        self.certs = self.root / "certs"
        self.certs.mkdir()
        for name in ("tls.crt", "tls.key"):
            (self.certs / name).write_text("fixture\n")
        (self.root / "inventory.ini").write_text("localhost ansible_connection=local ansible_become=false\n")
        self.rendered = self.root / "deploy/localhost/dock"
        self.variables = {
            "ansible_facts": {"env": {"HOME": str(self.root)}, "default_ipv4": {"address": "192.0.2.10"},
                              "python": {"executable": sys.executable}},
            "ansible_user": "eva",
            "eva_site_id": "fixture",
            "eva_site_values_root": str(self.root / "site-values"),
            "eva_site_deploy_root": str(self.root / "deploy"),
            "eva_cache_root": str(self.root / "cache"),
            "eva_template_values_root": str(REPO / "src/solution/values"),
            "eva_helm_values_backup_root": str(self.root / "backups"),
            "eva_traefik_ports_manage": False,
            "eva_dock_workdir": str(self.root / "work"),
            "eva_dock_kubeconfig": str(self.root / "kubeconfig"),
            "eva_dock_chart_version": CATALOG["eva_dock_chart_version"],
            "eva_dock_image_tag": CATALOG["eva_dock_image_tag"],
        }

    def run_role(self, values=None, variables=None, succeeds=True):
        if values is not None:
            (self.root / "site-values").mkdir(exist_ok=True)
            (self.root / "site-values/dock.yaml").write_text(yaml.safe_dump({"localhost": values}))
        play = [{"hosts": "all", "gather_facts": False, "vars": {**self.variables, **(variables or {})},
                 "tasks": [{"ansible.builtin.import_role": {"name": "eva_dock"}}]}]
        path = self.root / "playbook.yaml"
        path.write_text(yaml.safe_dump(play, sort_keys=False))
        result = subprocess.run(
            [os.environ.get("ANSIBLE_PLAYBOOK", "ansible-playbook"), "-i", str(self.root / "inventory.ini"), str(path)],
            env={**os.environ, "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
                 "ANSIBLE_ROLES_PATH": str(ROLES), "FIXTURE_ROOT": str(self.root),
                 "REAL_HELM": shutil.which("helm"), "FIXTURE_CHART": str(CHART)},
            capture_output=True, text=True, timeout=300)
        output = result.stdout + result.stderr
        self.assertEqual(result.returncode == 0, succeeds, output[-12000:])
        self.assertNotIn("fixture-dock-password", output)
        return output

    def manifest(self):
        return [item for item in yaml.safe_load_all((self.root / "rendered.yaml").read_text()) if item]

    def kind(self, manifest, kind, suffix=""):
        return [item for item in manifest if item["kind"] == kind and item["metadata"]["name"].endswith(suffix)]

    def helm_commands(self):
        log = self.root / "helm.log"
        return [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []

    def test_online_ip_https_uses_ecr_dedicated_port_and_tls(self):
        output = self.run_role({
            "ingress": {"host": "192.0.2.10", "tls": {"enabled": True, "create": True, "hostPath": str(self.certs)}},
            "config": {"dockPublicUrl": "https://192.0.2.10:32020"},
            "secrets": {"dockAccessPassword": "fixture-dock-password"},
        })
        manifest = self.manifest()
        deployment = self.kind(manifest, "Deployment")[0]
        container = deployment["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(container["image"], "211722900905.dkr.ecr.ap-northeast-2.amazonaws.com/spectrabrain/eva-dock:"
                         + CATALOG["eva_dock_image_tag"])
        self.assertEqual(container["imagePullPolicy"], "Always")
        self.assertEqual(deployment["spec"]["template"]["spec"]["volumes"][0]["hostPath"]["path"], "/eva-dock")
        self.assertEqual(self.kind(manifest, "ConfigMap")[0]["data"]["DOCK_PUBLIC_URL"], "https://192.0.2.10:32020")
        self.assertTrue(self.kind(manifest, "CronJob", "-ecr-cronjob"))
        cronjob = self.kind(manifest, "CronJob", "-ecr-cronjob")[0]
        volumes = cronjob["spec"]["jobTemplate"]["spec"]["template"]["spec"]["volumes"]
        self.assertIn({"name": "aws-credentials", "hostPath": {"path": "/home/eva/.aws", "type": "Directory"}}, volumes)
        endpoint = traefik_ports.endpoint(manifest, "dock", "eva-dock", "eva-dock")
        self.assertEqual((endpoint["host"], endpoint["scheme"], endpoint["port"]), ("192.0.2.10", "https", 32020))
        ingress = self.kind(manifest, "Ingress")[0]
        self.assertEqual(ingress["spec"]["tls"][0]["secretName"], "eva-dock-tls")
        self.assertEqual(traefik_ports.tls_job_input(manifest, ingress["metadata"]["name"]),
                         {"tls_job_enabled": True, "tls_host_path": str(self.certs)})
        secret = self.kind(manifest, "Secret", "-secrets")[0]
        self.assertEqual(secret["stringData"]["DOCK_ACCESS_PASSWORD"], "fixture-dock-password")
        self.assertIn(["show", "values", "eva-dock/eva-dock", "--version", CATALOG["eva_dock_chart_version"]], self.helm_commands())
        for name in ("chart-defaults.yaml", "dock-k3s-override.yaml", "effective-input-values.yaml",
                     "values-sources.yaml", "resolved-values.yaml"):
            self.assertEqual((self.rendered / name).stat().st_mode & 0o777, 0o600, name)
        self.assertIn("deployment/eva-dock-eva-dock", (self.root / "kubectl.log").read_text())
        self.assertIn("https://192.0.2.10:32020", output)

    def test_local_repository_uses_offline_chart_and_harbor_images(self):
        offline = self.root / "cache/eva-dock" / f"eva-dock-{CATALOG['eva_dock_chart_version']}.tgz"
        offline.parent.mkdir(parents=True)
        shutil.copy(CHART, offline)
        self.run_role({
            "ingress": {"host": "192.0.2.10", "tls": {"enabled": True, "create": True, "hostPath": str(self.certs)}},
            "secrets": {"dockAccessPassword": "fixture-dock-password"},
        }, {"repository_mode": "local_repository", "repository_registry": "192.0.2.20:32080"})
        manifest = self.manifest()
        container = self.kind(manifest, "Deployment")[0]["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(container["image"], "192.0.2.20:32080/eva/eva-dock:" + CATALOG["eva_dock_image_tag"])
        job = self.kind(manifest, "Job", "-tls-job")[0]
        self.assertTrue(job["spec"]["template"]["spec"]["containers"][0]["image"].startswith("192.0.2.20:32080/eva/kubectl:"))
        self.assertFalse(self.kind(manifest, "CronJob"))
        # The derived public URL keeps Dock off App's default listener.
        self.assertEqual(self.kind(manifest, "ConfigMap")[0]["data"]["DOCK_PUBLIC_URL"], "https://192.0.2.10:32020")
        self.assertIn(["show", "values", str(offline)], self.helm_commands())

    def test_domain_url_is_derived_without_port(self):
        self.run_role({
            "ingress": {"host": "dock.example.com", "tls": {"enabled": False, "create": False}},
            "secrets": {"dockAccessPassword": "fixture-dock-password"},
        })
        manifest = self.manifest()
        self.assertEqual(self.kind(manifest, "ConfigMap")[0]["data"]["DOCK_PUBLIC_URL"], "http://dock.example.com")
        self.assertEqual(self.kind(manifest, "Ingress")[0]["spec"]["rules"][0]["host"], "dock.example.com")
        self.assertIsNone(traefik_ports.endpoint(manifest, "dock", "eva-dock", "eva-dock"))

    def test_invalid_inputs_stop_before_helm_changes(self):
        for values, message in (
            ({"ingress": {"host": "192.0.2.10"}}, "EVA Dock 접속 비밀번호가 없습니다"),
            ({"ingress": {"host": "192.0.2.10:32020"}, "secrets": {"dockAccessPassword": "fixture-dock-password"}},
             "EVA Dock 공개 주소가 올바르지 않습니다"),
            ({"ingress": {"host": "192.0.2.10", "tls": {"enabled": True}}, "config": {"dockPublicUrl": "http://192.0.2.10:32020"},
              "secrets": {"dockAccessPassword": "fixture-dock-password"}}, "EVA Dock 공개 주소가 올바르지 않습니다"),
            ({"unknownKey": True, "secrets": {"dockAccessPassword": "fixture-dock-password"}}, "모르는 최상위 키"),
        ):
            with self.subTest(message=message):
                output = self.run_role(values, succeeds=False)
                self.assertIn(message, output)
                self.assertFalse(any(command[:2] == ["upgrade", "--install"] for command in self.helm_commands()))


if __name__ == "__main__":
    unittest.main()
