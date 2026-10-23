#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
main="$repo_root/src/solution/roles/eva_app/tasks/main.yaml"
reconcile="$repo_root/src/solution/roles/eva_app/tasks/reconcile_app.yaml"
verify="$repo_root/src/solution/roles/eva_app/tasks/verify_app_ownership.yaml"

python3 - "$main" "$reconcile" "$verify" <<'PY'
import base64
import copy
import getpass
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import textwrap
import yaml

documents = {}
for raw_path in sys.argv[1:]:
    path = Path(raw_path)
    tasks = yaml.safe_load(path.read_text())
    if not isinstance(tasks, list):
        raise SystemExit(f"[ERROR] task file is not a YAML list: {path}")
    documents[path] = tasks

def find_task(tasks, name):
    for task in tasks:
        if not isinstance(task, dict):
            continue
        if task.get("name") == name:
            return task
        for block_name in ("block", "rescue", "always"):
            nested = task.get(block_name)
            if isinstance(nested, list):
                found = find_task(nested, name)
                if found is not None:
                    return found
    return None

main_tasks = documents[Path(sys.argv[1])]
conflict_fact = find_task(
    main_tasks,
    "Resolve EVA App Helm ownership adoption argument",
)
if (
    conflict_fact is None
    or "eva_app_server_side_conflict_args"
    not in str(conflict_fact.get("ansible.builtin.set_fact", {}))
    or "^force_conflicts=true$"
    not in str(conflict_fact.get("ansible.builtin.set_fact", {}))
):
    raise SystemExit("[ERROR] EVA App conflict argument fact is invalid")

for task_name in (
    "Validate EVA App Helm installation with server dry-run",
    "Install EVA App from online Helm repo",
    "Install EVA App from offline chart",
):
    task = find_task(main_tasks, task_name)
    command = "" if task is None else task.get("ansible.builtin.command", "")
    if "{{ eva_app_server_side_conflict_args }}" not in command:
        raise SystemExit(
            "[ERROR] EVA App conflict arguments are absent: " + task_name
        )

reconcile_path = Path(sys.argv[2])
reconcile_task = find_task(
    documents[reconcile_path], "Assess EVA App Helm ownership reconciliation"
)
internal_task = find_task(
    main_tasks, "Assess verified legacy EVA App internal Service preservation"
)
if reconcile_task is None or internal_task is None:
    raise SystemExit("[ERROR] production EVA App assessment tasks are absent")
reconcile_shell = reconcile_task.get("ansible.builtin.shell", "")
internal_shell = internal_task.get("ansible.builtin.shell", "")
for task in (internal_task, reconcile_task):
    if (
        task.get("environment", {}).get("EVA_APP_ARGOCD_HANDOFF_B64")
        != "{{ eva_app_argocd_handoff_raw.content | default('') }}"
        or "EVA_APP_HANDOFF_FILE" in task.get("environment", {})
        or "EVA_APP_HANDOFF_FILE" in task.get("ansible.builtin.shell", "")
        or "handoff_file" in task.get("ansible.builtin.shell", "")
    ):
        raise SystemExit(
            "[ERROR] assessment must consume controller receipt content only: "
            + task["name"]
        )
match = re.search(
    r"python3 - \"\$EVA_APP_SITE_ID\" <<'PY'\n(.*?)\nPY",
    reconcile_shell or "",
    re.DOTALL,
)
if match is None:
    raise SystemExit("[ERROR] EVA App handoff receipt validator is absent")

validator = textwrap.dedent(match.group(1))
valid_receipt = {
    "schema_version": "v1",
    "site_id": "site-a",
    "cluster_name": "cluster-a",
    "completed_at": "2026-09-18T00:00:00Z",
    "applications": ["cluster-a-eva-app", "cluster-a-eva-agent"],
    "registration_application": "registration",
    "registration_repository": "https://example.invalid/eva.git",
    "registration_branch": "main",
    "registration_manifest": "registration/clusters/cluster-a.yaml",
    "registration_commit": "a" * 40,
}

def encode_receipt(receipt):
    if receipt is None:
        return ""
    content = yaml.safe_dump(receipt) if isinstance(receipt, dict) else receipt
    return base64.b64encode(content.encode()).decode("ascii")

def validates(receipt, *, encoded=None):
    env = dict(os.environ, EVA_APP_ARGOCD_HANDOFF_B64=(
        encode_receipt(receipt) if encoded is None else encoded
    ))
    env.pop("EVA_APP_HANDOFF_FILE", None)
    return subprocess.run(
        [sys.executable, "-", "site-a"],
        input=validator,
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )

if validates(valid_receipt).stdout.strip() != "cluster-a-eva-app":
    raise SystemExit("[ERROR] valid Git-backed EVA App receipt was rejected")

invalid_receipts = {
    "site ID": {"site_id": "other"},
    "registration application": {"registration_application": "other"},
    "registration repository": {"registration_repository": ""},
    "registration branch": {"registration_branch": ""},
    "registration manifest": {"registration_manifest": "registration/clusters/other.yaml"},
    "registration commit": {"registration_commit": "A" * 40},
    "cluster name": {"cluster_name": ""},
    "EVA App application": {"applications": ["other-eva-app"]},
}
for name, changes in invalid_receipts.items():
    receipt = copy.deepcopy(valid_receipt)
    receipt.update(changes)
    if validates(receipt).returncode == 0:
        raise SystemExit(f"[ERROR] invalid {name} receipt was accepted")

for name, encoded in (
    ("missing content", ""),
    ("malformed base64", "not-base64!"),
    ("non-strict base64", encode_receipt(valid_receipt) + "!"),
    ("malformed YAML", encode_receipt("site_id: [")),
    ("non-mapping YAML", encode_receipt("[]")),
):
    if validates(None, encoded=encoded).returncode == 0:
        raise SystemExit(f"[ERROR] validator accepted {name}")

if reconcile_task["environment"].get("EVA_APP_TLS_JOB_ENABLED") != "{{ eva_app_tls_job_enabled | bool | lower }}":
    raise SystemExit("[ERROR] actual TLS Job gating fact is not passed to reconciliation")

# Execute the production assessment shell against local, read-only CLI mocks.
# In particular, old TLS helpers must not bypass the common ownership validator.
def resource(kind, name, api):
    group = api.split("/")[0] if "/" in api else ""
    return {
        "apiVersion": api,
        "kind": kind,
        "metadata": {
            "name": name,
            "namespace": "eva-app",
            "uid": "uid-" + name,
            "resourceVersion": "1",
            "labels": {"app.kubernetes.io/managed-by": "Helm"},
            "annotations": {
                "meta.helm.sh/release-name": "eva-app",
                "meta.helm.sh/release-namespace": "eva-app",
                "argocd.argoproj.io/tracking-id": f"cluster-a-eva-app:{group}/{kind}:eva-app/{name}",
            },
            "managedFields": [{"manager": "argocd-controller"}],
        },
    }

deployment = resource("Deployment", "eva-app", "apps/v1")
internal_service = resource("Service", "eva-app-internal", "v1")
internal_service["spec"] = {
    "type": "ClusterIP",
    "selector": {
        "app.kubernetes.io/instance": "eva-app",
        "app.kubernetes.io/name": "eva-app",
        "component": "eva-app",
    },
    "ports": [{"name": "http", "protocol": "TCP", "port": 32010, "targetPort": 32010}],
}
tls_helpers = [
    resource("Job", "eva-app-tls-job", "batch/v1"),
    resource("Role", "eva-app-tls-job-role", "rbac.authorization.k8s.io/v1"),
    resource("RoleBinding", "eva-app-tls-job-rolebinding", "rbac.authorization.k8s.io/v1"),
    resource("ServiceAccount", "eva-app-tls-job-sa", "v1"),
]
stored = [deployment, *tls_helpers]
mock_cli = '''
import json
import os
from pathlib import Path
import sys

fixture = json.loads(Path(os.environ["ASSESSMENT_FIXTURE"]).read_text())
tool, args = Path(sys.argv[0]).name, sys.argv[1:]
with Path(os.environ["ASSESSMENT_LOG"]).open("a") as stream:
    stream.write(tool + " " + " ".join(args) + "\\n")
if tool == "helm":
    if args[:2] == ["status", "eva-app"]:
        if fixture["status"] is None:
            sys.exit(1)
        print(json.dumps({"name": "eva-app", "namespace": "eva-app", "version": 1,
                          "info": {"status": fixture["status"]}}))
    elif args[:2] == ["get", "manifest"]:
        print(json.dumps({"kind": "List", "items": fixture["stored"]}))
    elif args[:2] == ["template", "eva-app"]:
        print(json.dumps({"kind": "List", "items": fixture["rendered"]}))
    else:
        raise SystemExit("unexpected Helm command")
elif args[:2] == ["create", "--dry-run=client"]:
    print(Path(args[args.index("-f") + 1]).read_text())
elif args[0] == "get":
    if args[1:3] == ["service", "eva-app-internal"]:
        if "-n" not in args or args[args.index("-n") + 1] != "eva-app":
            raise SystemExit("unexpected Service namespace")
        matches = [item for item in fixture["live"]
                   if item["kind"] == "Service"
                   and item["metadata"]["name"] == args[2]
                   and item["metadata"]["namespace"] == "eva-app"]
        if len(matches) != 1:
            sys.exit(1)
        print(json.dumps(matches[0]))
    elif "," in args[1]:
        print(json.dumps({"items": fixture["live"] + fixture["scan_extra"]}))
    elif args[1] == "secret" and "-l" in args:
        print(json.dumps({"items": [{"metadata": {"labels": {
            "owner": "helm", "name": "eva-app", "status": "failed"}}}]}))
    else:
        for item in fixture["live"]:
            if args[1] == item["kind"] + "/" + item["metadata"]["name"]:
                print(json.dumps(item))
                break
else:
    raise SystemExit("forbidden kubectl mutation or unexpected command")
'''

def install_mock_cli(root):
    root.mkdir(parents=True, exist_ok=True)
    for tool in ("helm", "kubectl"):
        executable = root / tool
        executable.write_text("#!" + sys.executable + "\n" + mock_cli)
        executable.chmod(0o755)

def assert_read_only(log, name):
    commands = log.read_text().splitlines() if log.exists() else []
    if not commands:
        raise SystemExit(f"[ERROR] {name}: assessment did not exercise CLI mocks")
    for command in commands:
        tool, *args = shlex.split(command)
        allowed = (
            tool == "helm"
            and (args[:2] in (["status", "eva-app"], ["get", "manifest"], ["template", "eva-app"]))
        ) or (
            tool == "kubectl"
            and (args[:1] == ["get"] or (
                args[:2] == ["create", "--dry-run=client"]
                and [arg for arg in args if arg.startswith("--dry-run")] == ["--dry-run=client"]
            ))
        )
        if not allowed:
            raise SystemExit(f"[ERROR] {name}: forbidden assessment command: {command}")
    return commands

def assess(name, *, status="deployed", old=None, live=None, rendered=None,
           enabled="false", receipt=valid_receipt, scan_extra=None, mode="online_repository",
           error=None, assessment="reconcile", encoded=None, target_receipt=None):
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        install_mock_cli(root)
        default_resources = [internal_service] if assessment == "internal" else stored
        fixture = root / "fixture.json"
        fixture.write_text(json.dumps({
            "status": status,
            "stored": default_resources if old is None else old,
            "live": default_resources if live is None else live,
            "rendered": [deployment] if rendered is None else rendered,
            "scan_extra": scan_extra or [],
        }))
        env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"],
                   ASSESSMENT_FIXTURE=str(fixture), ASSESSMENT_LOG=str(root / "commands.log"),
                   EVA_APP_NAMESPACE="eva-app", EVA_APP_RELEASE="eva-app",
                   EVA_APP_INTERNAL_SERVICE="eva-app-internal",
                   EVA_APP_CHART_SOURCE="eva-app/eva-app", EVA_APP_CHART_VERSION="3.3.0-beta.3",
                   EVA_APP_VALUES_FILE=str(root / "values.yaml"),
                   EVA_APP_EFFECTIVE_VALUES_FILE=str(root / "effective.yaml"),
                   EVA_APP_ARGOCD_HANDOFF_B64=(encode_receipt(receipt) if encoded is None else encoded),
                   EVA_APP_USE_CHART_VERSION="1",
                   EVA_APP_REPOSITORY_MODE=mode, EVA_APP_HELM_SET_JSON="[]", EVA_APP_SITE_ID="site-a")
        env.pop("EVA_APP_HANDOFF_FILE", None)
        # Only explicit fallback-attack cases create a target-local receipt.
        # Ordinary assessments have no receipt file anywhere in the fixture.
        if target_receipt is not None:
            target_path = root / "target-handoff.yaml"
            target_path.write_text(yaml.safe_dump(target_receipt))
            env["EVA_APP_HANDOFF_FILE"] = str(target_path)
        env.pop("EVA_APP_TLS_JOB_ENABLED", None)
        if enabled is not None:
            env["EVA_APP_TLS_JOB_ENABLED"] = enabled
        shell = internal_shell if assessment == "internal" else reconcile_shell
        expected = "preserve_internal_service=true" if assessment == "internal" else "take_ownership=true"
        result = subprocess.run(["bash", "-c", shell], env=env, text=True,
                                capture_output=True, check=False)
        output = result.stdout + result.stderr
        if error is None:
            if result.returncode != 0 or expected not in result.stdout:
                raise SystemExit(f"[ERROR] {name}: expected verified reconciliation\n{output}")
        elif result.returncode == 0 or error not in output:
            raise SystemExit(f"[ERROR] {name}: expected rejection containing {error!r}\n{output}")
        assert_read_only(root / "commands.log", name)
    print(f"[OK] App assessment: {name}")

assess("deployed TLS generation disabled")
assess("failed TLS generation disabled", status="failed")
assess("TTL-expired old TLS Job", live=[deployment, *tls_helpers[1:]])
assess("all old TLS helpers already absent", live=[deployment])
assess("unchanged TLS enabled", enabled="true", rendered=stored)
assess("rendered TLS helpers are not obsolete", rendered=stored)
assess("disabled helpers require receipt", receipt=None, error="require a verified Argo CD handoff receipt")
assess("invalid receipt", receipt={**valid_receipt, "site_id": "other"}, error="Git-backed EVA App identity contract")
assess("enabled TLS cannot lose helpers", enabled="true", error="manifest is incompatible")
assess("missing TLS fact fails closed", enabled=None, error="manifest is incompatible")
assess("no live rendered candidates", live=tls_helpers, error="no live rendered resource candidates")

for helper in tls_helpers:
    foreign = copy.deepcopy(stored)
    index = stored.index(helper)
    foreign[index]["metadata"]["annotations"]["meta.helm.sh/release-name"] = "other"
    assess("foreign " + helper["kind"], live=foreign, error="foreign Helm ownership")

for name, mutate, expected in (
    ("partial ownership", lambda m: m["annotations"].pop("meta.helm.sh/release-namespace"), "partial Helm ownership"),
    ("wrong tracking", lambda m: m["annotations"].update({"argocd.argoproj.io/tracking-id": "other:batch/Job:eva-app/eva-app-tls-job"}), "tracking identity mismatch"),
    ("controller owner", lambda m: m.update({"ownerReferences": [{"controller": True}]}), "unexpected controller ownership"),
    ("wrong namespace", lambda m: m.update({"namespace": "other"}), "unsafe EVA App legacy resource identity"),
    ("deployed helper lacks Helm owner", lambda m: [m["annotations"].pop(key) for key in ("meta.helm.sh/release-name", "meta.helm.sh/release-namespace")], "lacks current Helm ownership"),
):
    changed = copy.deepcopy(stored)
    mutate(changed[1]["metadata"])
    assess(name, live=changed, error=expected)

wrong_api = copy.deepcopy(stored)
wrong_api[1]["apiVersion"] = "other/v1"
assess("wrong live TLS API", live=wrong_api, error="unsafe live EVA App TLS helper identity")
assess("wrong stored TLS API", old=wrong_api, error="unsafe stored EVA App TLS helper identity")
unrelated = resource("Job", "eva-app-unrelated", "batch/v1")
assess("unrelated old Job remains blocked", old=[*stored, unrelated], error="manifest is incompatible")
assess("TLS helper not in deployed manifest", old=[deployment], error="absent from stored Helm manifest")
assess("late TLS helper appearance", live=[deployment], scan_extra=[tls_helpers[0]],
       error="obsolete TLS helper changed during assessment")
assess("unrendered legacy Secret remains blocked", scan_extra=[resource("Secret", "eva-app-secret-tls", "v1")],
       error="legacy EVA App resource is absent from rendered chart")
legacy = copy.deepcopy(stored)
for item in legacy:
    item["metadata"]["annotations"].pop("meta.helm.sh/release-name")
    item["metadata"]["annotations"].pop("meta.helm.sh/release-namespace")
assess("legacy Argo CD-only helpers validated", status=None, live=legacy)
ecr = resource("Job", "eva-app-ecr-job-once", "batch/v1")
assess("repository ECR cleanup contract preserved", old=[*stored, ecr], live=[*stored, ecr], mode="remote_repository")
assess("repository ECR-only cleanup preserved", old=[*stored, ecr], live=[ecr], mode="remote_repository")
assess("local repository ECR cleanup contract preserved", old=[*stored, ecr], live=[*stored, ecr], mode="local_repository")

legacy_service = copy.deepcopy(internal_service)
for key in ("meta.helm.sh/release-name", "meta.helm.sh/release-namespace"):
    legacy_service["metadata"]["annotations"].pop(key)
legacy_service["metadata"]["labels"] = {}
assess("unowned legacy internal Service uses controller receipt", assessment="internal",
    status=None, old=[], live=[legacy_service])
assess("unowned legacy internal Service requires controller receipt", assessment="internal",
    status=None, old=[], live=[legacy_service], receipt=None,
    error="requires an Argo CD handoff receipt")
current_service = copy.deepcopy(internal_service)
current_service["metadata"]["annotations"].pop("argocd.argoproj.io/tracking-id")
assess("current Helm internal Service without Argo tracking needs no receipt", assessment="internal",
    old=[current_service], live=[current_service], receipt=None)

for assessment, missing_error, site_error in (
    ("reconcile", "require a verified Argo CD handoff receipt", "Git-backed EVA App identity contract"),
    ("internal", "requires an Argo CD handoff receipt", "invalid Argo CD handoff receipt"),
):
    assess(assessment + " controller content without receipt file", assessment=assessment)
    assess(assessment + " missing receipt", assessment=assessment, receipt=None, error=missing_error)
    assess(assessment + " wrong site", assessment=assessment,
           receipt={**valid_receipt, "site_id": "other"}, error=site_error)
    for name, encoded in (
        ("malformed base64", "not-base64!"),
        ("non-strict base64", encode_receipt(valid_receipt) + "!"),
        ("malformed YAML", encode_receipt("site_id: [")),
        ("non-mapping YAML", encode_receipt("[]")),
    ):
        # Error wording may differ between decode/YAML failures. Both actual
        # shells must fail closed, not report a successful assessment.
        assess(assessment + " " + name, assessment=assessment, encoded=encoded, error="")
    tracked = copy.deepcopy([internal_service] if assessment == "internal" else stored)
    tracked[0]["metadata"]["annotations"]["argocd.argoproj.io/tracking-id"] = (
        "other:/Service:eva-app/eva-app-internal"
    )
    assess(assessment + " wrong tracking", assessment=assessment, live=tracked,
           error="tracking identity mismatch")
    assess(assessment + " invalid commit", assessment=assessment,
           receipt={**valid_receipt, "registration_commit": "A" * 40},
           error="registration commit" if assessment == "internal" else "Git-backed EVA App identity contract")
    assess(assessment + " hostile target file cannot override controller content",
           assessment=assessment, target_receipt={**valid_receipt, "site_id": "other"})
    assess(assessment + " target file cannot replace missing controller receipt",
           assessment=assessment, receipt=None, target_receipt=valid_receipt, error=missing_error)

# Exercise the real delegation/register/environment boundary, not a hand-built
# shell environment. Local target aliases deliberately share a filesystem: once
# the controller snapshot is removed, even that shared path cannot mask a target
# file read. Distinct sites/clusters also expose cross-host register leakage.
receipt_task_names = (
    "Check Argo CD handoff receipt on control node",
    "Read Argo CD handoff receipt on control node",
)
receipt_tasks = [find_task(main_tasks, name) for name in receipt_task_names]
for task, name, module, argument, register in zip(
    receipt_tasks,
    receipt_task_names,
    ("ansible.builtin.stat", "ansible.builtin.slurp"),
    ("path", "src"),
    ("eva_app_argocd_handoff_status", "eva_app_argocd_handoff_raw"),
):
    if task is None:
        raise SystemExit("[ERROR] missing production controller receipt task: " + name)
    conditions = task.get("when", [])
    conditions = [conditions] if isinstance(conditions, str) else conditions
    if (
        task.get(module, {}).get(argument) != "{{ eva_app_argocd_handoff_path }}"
        or task.get("delegate_to") != "localhost"
        or task.get("become") is not True
        or task.get("no_log") is not True
        or task.get("register") != register
        or not any("not" in str(condition) and "ansible_check_mode" in str(condition)
                   for condition in conditions)
        or (module == "ansible.builtin.slurp" and not any(
            "eva_app_argocd_handoff_status.stat.exists" in str(condition)
            for condition in conditions
        ))
    ):
        raise SystemExit("[ERROR] unsafe production controller receipt task: " + name)
main_names = [task.get("name") for task in main_tasks if isinstance(task, dict)]
if not (
    main_names.index(receipt_task_names[0])
    < main_names.index(receipt_task_names[1])
    < main_names.index(internal_task["name"])
):
    raise SystemExit("[ERROR] controller receipt must be read before internal Service assessment")

ansible_playbook = shutil.which("ansible-playbook")
if ansible_playbook is None:
    raise SystemExit("[ERROR] ansible-playbook is required for controller receipt boundary tests")

with tempfile.TemporaryDirectory(prefix="eva-app-receipt-boundary-") as directory:
    root = Path(directory)
    install_mock_cli(root / "bin")
    hosts = {}
    aliases = ("receipt-target-a", "receipt-target-b")
    for suffix, alias in zip(("a", "b"), aliases):
        host_root = root / alias
        controller = root / "controller" / alias / "argocd-handoff.yaml"
        controller.parent.mkdir(parents=True)
        host_root.mkdir()
        target = host_root / "target-handoff.yaml"
        receipt = {
            **valid_receipt,
            "site_id": "site-" + suffix,
            "cluster_name": "cluster-" + suffix,
            "applications": ["cluster-" + suffix + "-eva-app", "cluster-" + suffix + "-eva-agent"],
            "registration_manifest": "registration/clusters/cluster-" + suffix + ".yaml",
            "registration_commit": suffix * 40,
        }
        controller.write_text(yaml.safe_dump(receipt))
        controller.chmod(0o600)
        target.write_text(yaml.safe_dump({**receipt, "site_id": "hostile-target"}))
        resources = copy.deepcopy([deployment, *tls_helpers, internal_service])
        for item in resources:
            annotations = item["metadata"]["annotations"]
            annotations["argocd.argoproj.io/tracking-id"] = annotations[
                "argocd.argoproj.io/tracking-id"
            ].replace("cluster-a-eva-app:", "cluster-" + suffix + "-eva-app:", 1)
        fixture = host_root / "fixture.json"
        fixture.write_text(json.dumps({
            "status": "deployed",
            "stored": resources,
            "live": resources,
            "rendered": [resources[0], resources[-1]],
            "scan_extra": [],
        }))
        hosts[alias] = {
            "ansible_connection": "local",
            "ansible_python_interpreter": sys.executable,
            "ansible_remote_tmp": str(host_root / "ansible-tmp"),
            "eva_site_id": receipt["site_id"],
            "eva_app_argocd_handoff_path": str(controller),
            "eva_app_workdir": str(host_root),
            "eva_app_effective_values_dest": str(host_root / "effective.yaml"),
            "fixture_expected_receipt": encode_receipt(receipt),
            "fixture_json": str(fixture),
            "fixture_log": str(host_root / "commands.log"),
            "fixture_target_receipt": str(target),
        }
    inventory = root / "inventory.yaml"
    inventory.write_text(yaml.safe_dump({"all": {"children": {
        "receipt_targets": {"hosts": hosts},
        "receipt_controller": {"hosts": {"localhost": {
            "ansible_connection": "local", "ansible_python_interpreter": sys.executable,
        }}},
    }}}))

    # Preserve all production task settings (especially delegate_to, become,
    # no_log, register, when and environment). Only expected rejection copies
    # change failed_when so the play can assert their exit status and message.
    internal_rejected = copy.deepcopy(internal_task)
    reconcile_rejected = copy.deepcopy(reconcile_task)
    internal_rejected["failed_when"] = False
    reconcile_rejected["failed_when"] = False
    fixture_tasks = [
        *copy.deepcopy(receipt_tasks),
        {"name": "Assert delegated results belong to each target alias",
         "no_log": True,
         "ansible.builtin.assert": {"that": [
             "eva_app_argocd_handoff_status.stat.exists | bool",
             "eva_app_argocd_handoff_raw.content == fixture_expected_receipt",
             "hostvars[inventory_hostname].eva_app_argocd_handoff_raw.content == fixture_expected_receipt",
             "hostvars['localhost'].eva_app_argocd_handoff_raw is not defined",
             "hostvars['receipt-target-a'].eva_app_argocd_handoff_raw.content != hostvars['receipt-target-b'].eva_app_argocd_handoff_raw.content",
         ]}},
        {"name": "Remove controller receipt after actual delegated slurp",
         "ansible.builtin.file": {"path": "{{ eva_app_argocd_handoff_path }}", "state": "absent"},
         "delegate_to": "localhost", "become": False},
        {"name": "Probe deleted receipt path from target",
         "ansible.builtin.stat": {"path": "{{ eva_app_argocd_handoff_path }}"},
         "register": "fixture_target_probe"},
        {"name": "Assert target cannot read the snapshotted receipt file",
         "no_log": True,
         "ansible.builtin.assert": {"that": [
             "not fixture_target_probe.stat.exists",
             "hostvars[inventory_hostname].eva_app_argocd_handoff_raw.content == fixture_expected_receipt",
         ]}},
        copy.deepcopy(internal_task),
        copy.deepcopy(reconcile_task),
        {"name": "Assert both actual target shells accept controller snapshot",
         "ansible.builtin.assert": {"that": [
             "eva_app_internal_service_preservation.rc == 0",
             "'preserve_internal_service=true' in eva_app_internal_service_preservation.stdout_lines",
             "not eva_app_internal_service_preservation.changed",
             "eva_app_legacy_reconciliation.rc == 0",
             "'take_ownership=true' in eva_app_legacy_reconciliation.stdout_lines",
             "not eva_app_legacy_reconciliation.changed",
         ]}},
        {"name": "Make target-local fallback look valid before missing-controller assessment",
         "no_log": True,
         "ansible.builtin.copy": {
             "content": "{{ fixture_expected_receipt | b64decode }}",
             "dest": "{{ fixture_target_receipt }}", "mode": "0600",
         }},
        *copy.deepcopy(receipt_tasks),
        {"name": "Assert missing controller receipt clears earlier target snapshot",
         "ansible.builtin.assert": {"that": [
             "not eva_app_argocd_handoff_status.stat.exists",
             "eva_app_argocd_handoff_raw is skipped",
             "eva_app_argocd_handoff_raw.content is not defined",
         ]}},
        internal_rejected,
        reconcile_rejected,
        {"name": "Assert target-local file cannot bypass missing controller receipt",
         "ansible.builtin.assert": {"that": [
             "eva_app_internal_service_preservation.rc != 0",
             "'requires an Argo CD handoff receipt' in (eva_app_internal_service_preservation.stdout + eva_app_internal_service_preservation.stderr)",
             "eva_app_legacy_reconciliation.rc != 0",
             "'require a verified Argo CD handoff receipt' in (eva_app_legacy_reconciliation.stdout + eva_app_legacy_reconciliation.stderr)",
         ]}},
        {"name": "Report verified controller snapshot boundary",
         "ansible.builtin.debug": {"msg": "controller snapshot boundary verified for {{ inventory_hostname }}"}},
    ]
    playbook = root / "playbook.yaml"
    playbook.write_text(yaml.safe_dump([{
        "name": "Exercise production controller receipt transfer and target assessments",
        "hosts": "receipt_targets", "gather_facts": False, "become": False,
        "strategy": "linear",
        "vars": {
            # Keep real production become:true tasks runnable without requiring
            # sudo/root in a local fixture: controller files belong to this user.
            "ansible_become_user": getpass.getuser(),
            "eva_app_namespace": "eva-app", "eva_app_name": "eva-app",
            "eva_app_kubeconfig": str(root / "unused-kubeconfig"),
            "eva_app_chart_source": "eva-app/eva-app", "eva_app_chart_version": "3.3.0-beta.3",
            "eva_app_tls_job_enabled": False, "repository_mode": "online_repository",
            "eva_cli_helm_set": [], "eva_cli_chart_path": "",
            "eva_app_traefik_args": "", "eva_app_traefik_env": {},
        },
        "environment": {
            "PATH": str(root / "bin") + os.pathsep + str(Path(sys.executable).parent)
                    + os.pathsep + os.environ["PATH"],
            "ASSESSMENT_FIXTURE": "{{ fixture_json }}", "ASSESSMENT_LOG": "{{ fixture_log }}",
            # Deliberately injected legacy input must be ignored, never read.
            "EVA_APP_HANDOFF_FILE": "{{ fixture_target_receipt }}",
        },
        "tasks": fixture_tasks,
    }], sort_keys=False))
    config = root / "ansible.cfg"
    config.write_text("[defaults]\nretry_files_enabled = False\nstdout_callback = default\n")
    env = dict(os.environ, ANSIBLE_CONFIG=str(config), ANSIBLE_NOCOLOR="1",
               ANSIBLE_STDOUT_CALLBACK="default", ANSIBLE_BECOME_ALLOW_SAME_USER="False")
    env.pop("EVA_APP_HANDOFF_FILE", None)
    env.pop("EVA_APP_ARGOCD_HANDOFF_B64", None)
    result = subprocess.run(
        [ansible_playbook, "-i", str(inventory), str(playbook)],
        env=env, text=True, capture_output=True, check=False,
    )
    if result.returncode != 0 or result.stdout.count("controller snapshot boundary verified for ") != len(aliases):
        raise SystemExit(
            "[ERROR] actual Ansible controller receipt boundary fixture failed\n"
            + result.stdout + result.stderr
        )
    for alias in aliases:
        commands = assert_read_only(Path(hosts[alias]["fixture_log"]), alias)
        if not (
            any(command.startswith("kubectl get service eva-app-internal -n eva-app") for command in commands)
            and any(command.startswith("helm template eva-app") for command in commands)
        ):
            raise SystemExit("[ERROR] both production shells were not exercised for " + alias)
        if Path(hosts[alias]["eva_app_argocd_handoff_path"]).exists():
            raise SystemExit("[ERROR] controller receipt survived boundary fixture for " + alias)
    print("[OK] actual delegated stat/slurp: two isolated aliases, deleted controller receipts, no target fallback, read-only CLIs")
PY

for required in \
  'Reconcile verified Argo CD legacy EVA App resources' \
  'Validate EVA App Helm installation with server dry-run' \
  'Record existing EVA App deployment identity and replicas' \
  'Install EVA App and restore prior replicas if Helm fails' \
  'Verify successful EVA App legacy Helm adoption'; do
  grep -Fq "$required" "$main"
done

dry_run_line="$(grep -n -m1 'Validate EVA App Helm installation with server dry-run' "$main" | cut -d: -f1)"
scale_line="$(grep -n -m1 'Scale down existing EVA App deployment before reinstall' "$main" | cut -d: -f1)"
[[ "$dry_run_line" -lt "$scale_line" ]]

for required in \
  --dry-run=server \
  --hide-secret \
  '{{ eva_app_take_ownership_arg }}' \
  'take_ownership=true' \
  'take_ownership=false' \
  'force_conflicts=true' \
  'force_conflicts=false' \
  'failed_release_recovery=true' \
  'failed_release_recovery=false' \
  'failed EVA App Helm release will be recovered by verified Helm upgrade' \
  'failed EVA App release manifest is incompatible with current rendered chart' \
  'exact current release Helm ownership accepted' \
  'partial Helm ownership' \
  'foreign Helm ownership' \
  '--server-side=true' \
  '--force-conflicts' \
  'eva_app_server_side_conflict_args' \
  'Git-backed EVA App identity contract' \
  'registration_application' \
  'registration_repository' \
  'registration_branch' \
  'registration_manifest' \
  'registration_commit' \
  'cluster_name' \
  "cluster_name + '-eva-app'" \
  'EVA_APP_SITE_ID' \
  'eva_cli_helm_set' \
  'unsafe EVA App Helm release status' \
  'unsupported managed-by ownership' \
  'unexpected controller ownership' \
  'EVA App tracking identity mismatch' \
  'legacy EVA App resource is absent from rendered chart' \
  'persistent resource cannot be adopted' \
  'adopted EVA App resources are owned by the deployed Helm release' \
  'meta.helm.sh/release-name' \
  'meta.helm.sh/release-namespace'; do
  grep -Fq -- "$required" "$main" "$reconcile" "$verify"
done

if grep -Eq 'kubectl (delete|patch)' "$reconcile" "$verify"; then
  echo 'EVA App reconciliation performs a forbidden kubectl mutation' >&2
  exit 1
fi

if grep -Eq -- '(^|[[:space:]])--force([[:space:]]|$)' "$main" "$reconcile" "$verify"; then
  echo 'EVA App reconciliation uses forbidden Helm --force' >&2
  exit 1
fi

for workflow in pr-ci.yaml tag-release.yaml; do
  grep -Fq 'scripts/test/test_eva_app_reconciliation.sh' "$repo_root/.github/workflows/$workflow"
done

# Verified legacy internal Service preservation contract
app_main="$repo_root/src/solution/roles/eva_app/tasks/main.yaml"

python3 - \
  "$app_main" <<'PY_APP_INTERNAL_CONTRACT'
from pathlib import Path
import sys

import yaml


tasks = yaml.safe_load(
    Path(sys.argv[1]).read_text()
)

task_by_name = {
    task.get("name"): task
    for task in tasks
    if isinstance(task, dict)
    and isinstance(task.get("name"), str)
}

required_names = (
    "Assess verified legacy EVA App internal Service preservation",
    "Build verified legacy EVA App internal Service values",
    "Preserve verified legacy EVA App internal Service",
    "Merge Workspace EVA App values",
    "Merge CLI EVA App values",
    "Record private EVA App effective input values on control node",
    "Reconcile verified Argo CD legacy EVA App resources",
)

for name in required_names:
    if name not in task_by_name:
        raise SystemExit(
            f"[ERROR] missing EVA App task: {name}"
        )

ordered_names = [
    task.get("name")
    for task in tasks
    if isinstance(task, dict)
]

positions = {
    name: ordered_names.index(name)
    for name in required_names
}

if not (
    positions[
        "Assess verified legacy EVA App internal Service preservation"
    ]
    < positions[
        "Build verified legacy EVA App internal Service values"
    ]
    < positions[
        "Preserve verified legacy EVA App internal Service"
    ]
    < positions["Merge Workspace EVA App values"]
    < positions["Merge CLI EVA App values"]
    < positions[
        "Record private EVA App effective input values on control node"
    ]
    < positions[
        "Reconcile verified Argo CD legacy EVA App resources"
    ]
):
    raise SystemExit(
        "[ERROR] EVA App legacy Service preservation "
        "task ordering is invalid"
    )

assessment = task_by_name[
    "Assess verified legacy EVA App internal Service preservation"
]

shell = assessment.get(
    "ansible.builtin.shell",
    "",
)

required_shell_contracts = (
    "preserve_internal_service=false",
    "preserve_internal_service=true",
    "argocd.argoproj.io/tracking-id",
    "meta.helm.sh/release-name",
    "meta.helm.sh/release-namespace",
    "app.kubernetes.io/managed-by",
    "ownerReferences",
    "ClusterIP",
    "targetPort",
    "registration_commit",
)

for contract in required_shell_contracts:
    if contract not in shell:
        raise SystemExit(
            "[ERROR] legacy Service assessment is missing "
            f"contract: {contract}"
        )

build_values = str(
    task_by_name[
        "Build verified legacy EVA App internal Service values"
    ].get(
        "ansible.builtin.set_fact",
        {},
    )
)

if (
    "eva_app_legacy_service_values" not in build_values
    or "'service'" not in build_values
    or "'internal'" not in build_values
    or "'enabled': true" not in build_values
):
    raise SystemExit(
        "[ERROR] legacy internal Service values override "
        "is incomplete"
    )

preserve_values = str(
    task_by_name[
        "Preserve verified legacy EVA App internal Service"
    ].get(
        "ansible.builtin.set_fact",
        {},
    )
)

if (
    "eva_app_generated_values" not in preserve_values
    or "eva_app_legacy_service_values" not in preserve_values
    or "recursive=true" not in preserve_values
):
    raise SystemExit(
        "[ERROR] verified legacy Service values are not "
        "merged recursively"
    )

metadata_task = task_by_name[
    "Record EVA App values source metadata on control node"
]

metadata_content = str(
    metadata_task.get(
        "ansible.builtin.copy",
        {},
    ).get(
        "content",
        "",
    )
)

if "legacy_internal_service:" not in metadata_content:
    raise SystemExit(
        "[ERROR] values source metadata omits "
        "legacy internal Service preservation"
    )

print(
    "[OK] verified legacy EVA App internal "
    "Service preservation contract"
)
PY_APP_INTERNAL_CONTRACT


echo 'EVA App reconciliation contract tests passed.'
