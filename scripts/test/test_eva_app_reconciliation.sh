#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
main="$repo_root/src/solution/roles/eva_app/tasks/main.yaml"
reconcile="$repo_root/src/solution/roles/eva_app/tasks/reconcile_app.yaml"
verify="$repo_root/src/solution/roles/eva_app/tasks/verify_app_ownership.yaml"

python3 - "$main" "$reconcile" "$verify" <<'PY'
import copy
import json
import os
from pathlib import Path
import re
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
reconcile_shell = documents[reconcile_path][0].get("ansible.builtin.shell")
match = re.search(
    r"python3 - \"\$handoff_file\" \"\$EVA_APP_SITE_ID\" <<'PY'\n(.*?)\nPY",
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

def validates(receipt):
    with tempfile.TemporaryDirectory() as directory:
        receipt_path = Path(directory) / "argocd-handoff.yaml"
        receipt_path.write_text(yaml.safe_dump(receipt))
        result = subprocess.run(
            [sys.executable, "-", str(receipt_path), "site-a"],
            input=validator,
            text=True,
            capture_output=True,
            check=False,
        )
    return result

if validates(valid_receipt).stdout.strip() != "cluster-a-eva-app":
    raise SystemExit("[ERROR] valid Git-backed EVA App receipt was rejected")

invalid_receipts = {
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

if documents[reconcile_path][0]["environment"].get("EVA_APP_TLS_JOB_ENABLED") != "{{ eva_app_tls_job_enabled | bool | lower }}":
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
    if "," in args[1]:
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

def assess(name, *, status="deployed", old=None, live=None, rendered=None,
           enabled="false", receipt=valid_receipt, scan_extra=None, mode="cloud_repository",
           error=None):
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        for tool in ("helm", "kubectl"):
            executable = root / tool
            executable.write_text("#!" + sys.executable + "\n" + mock_cli)
            executable.chmod(0o755)
        fixture = root / "fixture.json"
        fixture.write_text(json.dumps({
            "status": status,
            "stored": stored if old is None else old,
            "live": stored if live is None else live,
            "rendered": [deployment] if rendered is None else rendered,
            "scan_extra": scan_extra or [],
        }))
        receipt_path = root / "handoff.yaml"
        if receipt is not None:
            receipt_path.write_text(yaml.safe_dump(receipt))
        env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"],
                   ASSESSMENT_FIXTURE=str(fixture), ASSESSMENT_LOG=str(root / "commands.log"),
                   EVA_APP_NAMESPACE="eva-app", EVA_APP_RELEASE="eva-app",
                   EVA_APP_CHART_SOURCE="eva-app/eva-app", EVA_APP_CHART_VERSION="3.3.0-beta.3",
                   EVA_APP_VALUES_FILE=str(root / "values.yaml"),
                   EVA_APP_EFFECTIVE_VALUES_FILE=str(root / "effective.yaml"),
                   EVA_APP_HANDOFF_FILE=str(receipt_path), EVA_APP_USE_CHART_VERSION="1",
                   EVA_APP_REPOSITORY_MODE=mode, EVA_APP_HELM_SET_JSON="[]", EVA_APP_SITE_ID="site-a")
        env.pop("EVA_APP_TLS_JOB_ENABLED", None)
        if enabled is not None:
            env["EVA_APP_TLS_JOB_ENABLED"] = enabled
        result = subprocess.run(["bash", "-c", reconcile_shell], env=env, text=True,
                                capture_output=True, check=False)
        output = result.stdout + result.stderr
        if error is None:
            if result.returncode != 0 or "take_ownership=true" not in result.stdout:
                raise SystemExit(f"[ERROR] {name}: expected verified reconciliation\n{output}")
        elif result.returncode == 0 or error not in output:
            raise SystemExit(f"[ERROR] {name}: expected rejection containing {error!r}\n{output}")
        commands = (root / "commands.log").read_text()
        if re.search(r"kubectl (delete|patch|apply)|helm upgrade", commands):
            raise SystemExit(f"[ERROR] {name}: assessment mutated cluster state")
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
