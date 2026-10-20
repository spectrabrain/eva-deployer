#!/usr/bin/env python3
"""Offline tests of the shipped helper and real Ansible callers; no cluster."""

import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch
from urllib.request import urlopen

import yaml

REAL_RUN = subprocess.run
sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
ROLES = ROOT / "src/solution/roles"
SPEC = importlib.util.spec_from_file_location(
    "helm_values_backup", ROLES / "eva_helm_values_backup/files/backup_user_values.py")
assert SPEC is not None and SPEC.loader is not None
module = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(module)
SECRET = "private-original-password-NOT-FOR-LOGS"
VALUES = ("config:\n  host: old.example\npassword: '" + SECRET
          + "'\nlist: [one, two]\nscript: |\n  first\n  second\n").encode()


def record(release="custom-iam", namespace="custom-ns", status="deployed", revision="4"):
    return {"name": release, "namespace": namespace, "revision": revision,
            "status": status, "chart": "eva-iam-3.1.8", "app_version": "3.1.8"}


class BackupTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="helm-backup-tests-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "backups with space"
        self.calls = []
        self.records = [record()]
        self.values = VALUES
        self.failure = None
        self.after = None
        self.lookups = 0
        runner = patch.object(module.subprocess, "run", side_effect=self.run_helm)
        runner.start()
        self.addCleanup(runner.stop)

    def run_helm(self, args, **kwargs):
        self.calls.append(args)
        self.assertEqual(args[0], "helm")
        self.assertEqual(kwargs["timeout"], 120)
        if args[1] == self.failure:
            return subprocess.CompletedProcess(args, 1, VALUES, VALUES)
        if args[1] == "list":
            records = self.records if self.lookups == 0 or self.after is None else self.after
            self.lookups += 1
            data = json.dumps(records).encode()
        else:
            self.assertEqual(args[1:3], ["get", "values"])
            self.assertNotIn("--all", args)
            self.assertNotIn("-a", args)
            data = self.values
        return subprocess.CompletedProcess(args, 0, data, b"")

    def backup(self, **overrides):
        args = dict(namespace="custom-ns", release="custom-iam", component="iam",
                    root=self.root, site="site-1")
        args.update(overrides)
        return module.backup(**args)

    def test_exact_original_values_revision_and_metadata(self):
        result = self.backup()
        path = Path(result["values_file"])
        self.assertEqual(path.read_bytes(), VALUES)
        self.assertEqual(path.name, "iam-user-values.yaml")
        metadata = json.loads((path.parent / "metadata.json").read_text())
        self.assertEqual(metadata["revision"], "4")
        self.assertEqual(metadata["sha256"], hashlib.sha256(VALUES).hexdigest())
        self.assertEqual(metadata["values_kind"], "user-supplied")
        self.assertTrue(metadata["sensitive"])
        self.assertNotIn(SECRET, json.dumps(result) + json.dumps(metadata))
        self.assertIn(["helm", "get", "values", "custom-iam", "--namespace", "custom-ns",
                       "--revision", "4", "--output", "yaml"], self.calls)
        self.assertIn("^custom\\-iam$", self.calls[0])

    def test_private_permissions(self):
        path = Path(self.backup()["values_file"])
        for directory in [path.parent, *path.parent.parents]:
            if directory == self.root.parent:
                break
            self.assertEqual(stat.S_IMODE(directory.stat().st_mode), 0o700)
        for file in path.parent.iterdir():
            self.assertEqual(stat.S_IMODE(file.stat().st_mode), 0o600)

    def test_absent_release_does_not_create_backups(self):
        self.records = []
        self.assertEqual(self.backup()["status"], "absent")
        self.assertFalse(self.root.exists())
        self.assertEqual(len(self.calls), 1)

    def test_failed_and_pending_releases_are_preserved(self):
        for status in ("failed", "pending-upgrade", "pending-install", "uninstalled"):
            with self.subTest(status=status):
                self.records = [record(status=status)]
                self.assertEqual(Path(self.backup()["values_file"]).read_bytes(), VALUES)
                self.assertNotIn("--all", self.calls[-3])
                for flag in ("--deployed", "--failed", "--pending", "--uninstalled",
                             "--uninstalling", "--superseded"):
                    self.assertIn(flag, self.calls[-3])

    def test_reruns_never_overwrite(self):
        first = self.backup()
        self.values = b"config:\n  host: newer.example\n"
        second = self.backup()
        self.assertNotEqual(first["backup_directory"], second["backup_directory"])
        self.assertEqual(Path(first["values_file"]).read_bytes(), VALUES)
        self.assertEqual(Path(second["values_file"]).read_bytes(), self.values)

    def test_null_is_valid_but_empty_output_fails(self):
        self.values = b"null\n"
        self.assertEqual(Path(self.backup()["values_file"]).read_bytes(), b"null\n")
        self.values = b"\n"
        with self.assertRaises(module.BackupError):
            self.backup()

    def test_helm_failures_are_not_fresh_installs(self):
        for command in ("list", "get"):
            with self.subTest(command=command):
                self.failure = command
                with self.assertRaises(module.BackupError) as error:
                    self.backup()
                self.assertNotIn(SECRET, str(error.exception))
        self.assertEqual(list(self.root.rglob("metadata.json")), [])

    def test_timeout_and_missing_helm_fail_closed(self):
        for error in (FileNotFoundError(SECRET), subprocess.TimeoutExpired("helm", 120, VALUES)):
            with self.subTest(error=type(error).__name__):
                with patch.object(module.subprocess, "run", side_effect=error):
                    with self.assertRaises(module.BackupError) as raised:
                        self.backup()
                    self.assertNotIn(SECRET, str(raised.exception))

    def test_safe_error_categories_identify_phase_without_leaking_diagnostics(self):
        failures = (
            (b"Error: unknown flag: --all", "unsupported-cli-option"),
            (b"secrets is forbidden", "kubernetes-rbac-denied"),
            (b"Unauthorized", "kubernetes-authentication-failed"),
            (b"x509: certificate signed by unknown authority", "kubernetes-tls-error"),
            (b"Kubernetes cluster unreachable: connection refused", "kubernetes-connection-failed"),
            (b"Kubernetes cluster unreachable", "kubernetes-unreachable"),
            (b"error loading config file", "kubeconfig-error"),
            (b"release: not found", "release-not-found-during-backup"),
            (b"unexpected error", "unclassified-helm-error"),
        )
        for command, phase in ((["list"], "release lookup"), (["get", "values"], "user Values export")):
            for diagnostic, reason in failures:
                with self.subTest(command=command, reason=reason):
                    failure = subprocess.CompletedProcess(command, 17, VALUES, diagnostic + b"\n" + VALUES)
                    with patch.object(module.subprocess, "run", return_value=failure):
                        with self.assertRaises(module.BackupError) as raised:
                            module.helm(command)
                    message = str(raised.exception)
                    self.assertIn(phase, message)
                    self.assertIn("exit=17", message)
                    self.assertIn("reason=" + reason, message)
                    self.assertNotIn(SECRET, message)

    def test_bad_lookup_fails_closed(self):
        for data in (b"", b"null", b"{}", b"invalid", b"[{}]"):
            with self.subTest(data=data), patch.object(module, "helm", return_value=data):
                with self.assertRaises(module.BackupError):
                    self.backup()

    def test_wrong_identity_revision_and_ambiguous_records(self):
        for records in ([record(release="another")], [record(namespace="another")],
                        [record(revision="0")], [record(revision="invalid")], [record(), record()]):
            with self.subTest(records=records):
                self.records = records
                with self.assertRaises(module.BackupError):
                    self.backup()

    def test_changed_or_deleted_release_does_not_publish_complete_backup(self):
        for records in ([record(revision="5")], [], [record(status="pending-upgrade")]):
            with self.subTest(records=records):
                self.lookups = 0
                self.after = records
                with self.assertRaises(module.BackupError):
                    self.backup()
        self.assertEqual(list(self.root.rglob("metadata.json")), [])
        self.assertEqual(list(self.root.rglob("*-user-values.yaml")), [])
        self.assertEqual(len(list(self.root.rglob("*.partial"))), 3)

    def test_unsafe_paths_fail(self):
        for kwargs in (dict(site=".."), dict(component="../iam"), dict(release="-invalid"),
                       dict(root="relative"), dict(root="/"), dict(root=self.root / ".." / "x")):
            with self.subTest(kwargs=kwargs):
                with self.assertRaises(module.BackupError):
                    self.backup(**kwargs)

    def test_incomplete_writes_never_publish_completion_metadata(self):
        for failure_at in (1, 2):
            effects = [None] * (failure_at - 1) + [OSError("simulated disk failure")]
            with self.subTest(failure_at=failure_at), patch.object(module.os, "fsync", side_effect=effects):
                with self.assertRaises(OSError):
                    self.backup()
            self.assertEqual(list(self.root.rglob("metadata.json")), [])

    def test_foreign_owned_backup_root_is_refused(self):
        with patch.object(module.os, "geteuid", return_value=os.geteuid() + 1):
            with self.assertRaises(module.BackupError):
                self.backup()
        self.assertEqual(list(self.root.iterdir()), [])

    def test_symlinks_at_root_parent_and_site_are_refused(self):
        target = Path(self.temp.name) / "target"
        target.mkdir()
        self.root.symlink_to(target, target_is_directory=True)
        with self.assertRaises(module.BackupError):
            self.backup()
        with self.assertRaises(module.BackupError):
            self.backup(root=self.root / "child")
        self.root.unlink()
        self.root.mkdir()
        (self.root / "site-1").symlink_to(target, target_is_directory=True)
        with self.assertRaises(module.BackupError):
            self.backup()
        self.assertEqual(list(target.iterdir()), [])


class RealHelmCLITests(unittest.TestCase):
    """Parse production argv using real Helm, with --help to avoid any API access."""

    def test_production_flags_supported_by_real_helm(self):
        with tempfile.TemporaryDirectory(prefix="helm-backup-cli-") as temporary:
            root = Path(temporary)
            binaries = [path for path in os.environ.get("EVA_TEST_HELM_BINARIES", "").split(os.pathsep) if path]
            if shutil.which("helm"):
                binaries.append(shutil.which("helm"))
            if os.environ.get("EVA_TEST_HELM_DOWNLOAD") == "1":
                # Read the exact runtime pin rather than guessing which Helm is deployed.
                source = (ROOT / "tools/eva/internal/runtime/bootstrap_online.go").read_text()
                artifact = re.search(r'Name:\s*"helm",\s*URL:\s*"([^"]+)",\s*SHA256:\s*"([0-9a-f]{64})"', source)
                self.assertIsNotNone(artifact, "Managed Helm runtime artifact must have a checksum pin")
                url, checksum = artifact.groups()
                archive = root / "helm.tar.gz"
                with urlopen(url, timeout=120) as response:
                    data = response.read()
                self.assertEqual(hashlib.sha256(data).hexdigest(), checksum)
                archive.write_bytes(data)
                with tarfile.open(archive) as tar:
                    member = tar.getmember("linux-amd64/helm")
                    self.assertTrue(member.isfile())
                    stream = tar.extractfile(member)
                    self.assertIsNotNone(stream)
                    with stream:
                        binary = root / "helm"
                        binary.write_bytes(stream.read())
                binary.chmod(0o700)
                binaries.append(str(binary))
            if not binaries:
                self.skipTest("No Helm binary; CI sets EVA_TEST_HELM_DOWNLOAD=1 to validate the runtime pin")
            for binary in dict.fromkeys(binaries):
                calls = []

                def validate(arguments):
                    calls.append(arguments)
                    result = REAL_RUN([binary, *arguments, "--help"], stdout=subprocess.PIPE,
                                      stderr=subprocess.PIPE, timeout=30, check=False)
                    self.assertEqual(result.returncode, 0,
                                     f"Real Helm rejected production flags: {arguments}: {result.stderr!r}")
                    return json.dumps([record()]).encode() if arguments[0] == "list" else VALUES

                with self.subTest(binary=binary), patch.object(module, "helm", side_effect=validate):
                    result = module.backup(namespace="custom-ns", release="custom-iam", component="iam",
                                           root=root / "backups", site="test")
                    self.assertEqual(Path(result["values_file"]).read_bytes(), VALUES)
                    self.assertEqual(len(calls), 3)


CALLERS = [
    ("eva_app/tasks/main.yaml", "Remove EVA App workspace if it exists", ["app"]),
    ("eva_iam/tasks/main.yaml", "Ensure EVA IAM workspace exists on target", ["iam"]),
    ("eva_vision/tasks/main.yaml", "Remove EVA Vision workspace if it exists", ["vision"]),
    ("eva_agent/tasks/agent.yaml", "Ensure EVA Agent workspace exists", ["agent"]),
    ("eva_agent/tasks/dependencies.yaml", "Detect local disk mount candidates from facts",
     ["agent-init", "qdrant", "vllm"]),
]


def backup_callers():
    callers = []
    for relative, mutation, _ in CALLERS:
        tasks = yaml.safe_load((ROLES / relative).read_text())
        matches = [task for task in tasks if task.get("ansible.builtin.include_role", {}).get("name")
                   == "eva_helm_values_backup"]
        assert len(matches) == 1, relative
        caller = matches[0]
        assert tasks.index(caller) < next(i for i, task in enumerate(tasks) if task.get("name") == mutation)
        callers.append(caller)
    return callers


class AnsibleIntegrationTests(unittest.TestCase):
    """Execute shipped script via the actual role and all seven caller bindings."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ansible-helm-backup-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        binary = self.root / "bin"
        binary.mkdir()
        self.log = self.root / "helm-calls.jsonl"
        fixture = binary / "helm"
        fixture.write_text("#!" + sys.executable + "\n" + '''
import json, os, sys
from pathlib import Path
args = sys.argv[1:]
with Path(os.environ['FAKE_HELM_LOG']).open('a') as f:
    f.write(json.dumps({'args': args, 'kubeconfig': os.environ.get('KUBECONFIG')}) + '\\n')
if args[0] == os.environ.get('FAKE_HELM_FAILURE'):
    print(os.environ['FAKE_HELM_SECRET'])
    print(os.environ['FAKE_HELM_SECRET'], file=sys.stderr)
    sys.exit(1)
namespace = args[args.index('--namespace') + 1]
if args[0] == 'list':
    if '--all' in args:
        print('Error: unknown flag: --all', file=sys.stderr)
        sys.exit(1)
    assert all(flag in args for flag in ('--deployed', '--failed', '--pending',
                                        '--uninstalled', '--uninstalling', '--superseded'))
    release = args[args.index('--filter') + 1][1:-1].replace('\\\\', '')
    records = [{'name': release, 'namespace': namespace, 'revision': '7',
                'chart': release + '-3.1.8', 'app_version': '3.1.8', 'status': 'deployed'}]
    print(json.dumps([] if os.environ.get('FAKE_HELM_ABSENT') else records))
elif args[:2] == ['get', 'values']:
    assert '--all' not in args and '-a' not in args
    assert args[args.index('--revision') + 1] == '7'
    sys.stdout.buffer.write(Path(os.environ['FAKE_HELM_VALUES']).read_bytes())
else:
    sys.exit(99)
''')
        fixture.chmod(0o700)
        values = self.root / "original.yaml"
        values.write_bytes(VALUES)
        self.environment = dict(os.environ, PATH=str(binary) + os.pathsep + os.environ["PATH"],
                                ANSIBLE_ROLES_PATH=str(ROLES), FAKE_HELM_LOG=str(self.log),
                                FAKE_HELM_SECRET=SECRET, FAKE_HELM_VALUES=str(values))
        self.ansible = shutil.which("ansible-playbook") or str(Path(sys.executable).parent / "ansible-playbook")
        variables = {"eva_site_id": "site-1", "eva_helm_values_backup_root": str(self.root / "backups"),
                     "ansible_facts": {"python": {"executable": sys.executable}},
                     "ansible_python_interpreter": sys.executable}
        for component in ("app", "iam", "vision", "agent"):
            variables["eva_" + component + "_namespace"] = "custom-" + component
            variables["eva_" + component + "_name"] = "release-" + component
            variables["eva_" + component + "_kubeconfig"] = str(self.root / (component + "-kubeconfig"))
        play = [{"hosts": "localhost", "gather_facts": False, "connection": "local", "vars": variables,
                 "tasks": [*backup_callers(), {"name": "Following install sentinel", "ansible.builtin.debug":
                                               {"msg": "INSTALL-SENTINEL-REACHED"}}]}]
        self.playbook = self.root / "play.yaml"
        self.playbook.write_text(yaml.safe_dump(play, sort_keys=False))

    def run_play(self, *arguments):
        result = subprocess.run([self.ansible, "-i", "localhost,", str(self.playbook), "-vv", *arguments],
                                env=self.environment, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, text=True, timeout=120)
        self.assertNotIn(SECRET, result.stdout)
        return result

    def test_all_actual_callers_backup_without_secret_logging(self):
        result = self.run_play()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("INSTALL-SENTINEL-REACHED", result.stdout)
        files = list((self.root / "backups").rglob("*-user-values.yaml"))
        self.assertEqual({file.name for file in files},
                         {name + "-user-values.yaml" for _, _, names in CALLERS for name in names})
        self.assertEqual(len(files), 7)
        for file in files:
            self.assertEqual(file.read_bytes(), VALUES)
            self.assertEqual(stat.S_IMODE(file.stat().st_mode), 0o600)
            self.assertEqual(stat.S_IMODE(file.parent.stat().st_mode), 0o700)
        calls = [json.loads(line) for line in self.log.read_text().splitlines()]
        self.assertEqual(len(calls), 21)
        for call in calls:
            namespace = call["args"][call["args"].index("--namespace") + 1]
            self.assertEqual(call["kubeconfig"], str(self.root / (namespace.removeprefix("custom-") + "-kubeconfig")))

    def test_absent_releases_skip_without_files(self):
        self.environment["FAKE_HELM_ABSENT"] = "1"
        result = self.run_play()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("INSTALL-SENTINEL-REACHED", result.stdout)
        self.assertFalse((self.root / "backups").exists())
        self.assertEqual(len(self.log.read_text().splitlines()), 7)

    def test_get_failure_stops_install_and_hides_diagnostics(self):
        self.environment["FAKE_HELM_FAILURE"] = "get"
        result = self.run_play()
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("INSTALL-SENTINEL-REACHED", result.stdout)
        self.assertEqual(list((self.root / "backups").rglob("metadata.json")), [])

    def test_lookup_failure_stops_install_not_absent(self):
        self.environment["FAKE_HELM_FAILURE"] = "list"
        result = self.run_play()
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("INSTALL-SENTINEL-REACHED", result.stdout)
        self.assertFalse((self.root / "backups").exists())

    def test_check_mode_queries_nothing_and_writes_nothing(self):
        result = self.run_play("--check")
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertFalse(self.log.exists())
        self.assertFalse((self.root / "backups").exists())


if __name__ == "__main__":
    unittest.main(verbosity=2)
