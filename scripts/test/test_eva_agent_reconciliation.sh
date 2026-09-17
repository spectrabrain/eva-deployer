#!/usr/bin/env bash
set -euo pipefail

repo_root="$(
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." &&
  pwd
)"

agent_file="$repo_root/src/solution/roles/eva_agent/tasks/agent.yaml"
reconcile_file="$repo_root/src/solution/roles/eva_agent/tasks/reconcile_agent.yaml"
python_bin="${PYTHON_BIN:-}"

if [[ -z "$python_bin" ]]; then
  if [[ -x "$repo_root/.venv/bin/python" ]]; then
    python_bin="$repo_root/.venv/bin/python"
  elif command -v python3 >/dev/null 2>&1; then
    python_bin="$(command -v python3)"
  else
    echo '[ERROR] Python interpreter is unavailable'
    exit 1
  fi
fi

"$python_bin" - "$agent_file" "$reconcile_file" <<'PY'
from pathlib import Path
import sys
import yaml


agent_path = Path(sys.argv[1])
reconcile_path = Path(sys.argv[2])

agent_tasks = yaml.safe_load(agent_path.read_text())
reconcile_tasks = yaml.safe_load(reconcile_path.read_text())

if not isinstance(agent_tasks, list):
    raise SystemExit("[ERROR] Agent tasks are not a list")

if not isinstance(reconcile_tasks, list) or len(reconcile_tasks) != 1:
    raise SystemExit(
        "[ERROR] expected exactly one Agent reconciliation task"
    )

names = [
    task.get("name")
    for task in agent_tasks
    if isinstance(task, dict)
]

wait_name = (
    "Wait until EVA Agent deployment pods "
    "are terminated before reinstall"
)
reconcile_name = (
    "Reconcile exact legacy Argo CD-managed "
    "EVA Agent resources"
)
install_name = "Install EVA Agent from online Helm repo"

for name in (wait_name, reconcile_name, install_name):
    if names.count(name) != 1:
        raise SystemExit(
            "[ERROR] Agent task count mismatch: " + name
        )

if not (
    names.index(wait_name)
    < names.index(reconcile_name)
    < names.index(install_name)
):
    raise SystemExit(
        "[ERROR] Agent reconciliation task order is invalid"
    )

include_task = next(
    task
    for task in agent_tasks
    if task.get("name") == reconcile_name
)

if (
    include_task.get("ansible.builtin.include_tasks")
    != "reconcile_agent.yaml"
):
    raise SystemExit(
        "[ERROR] Agent reconciliation include is incorrect"
    )

task = reconcile_tasks[0]
shell = task.get("ansible.builtin.shell")

if not isinstance(shell, str):
    raise SystemExit(
        "[ERROR] Agent reconciliation shell is absent"
    )

for delimiter in ("{{", "{%", "{#"):
    if delimiter in shell:
        raise SystemExit(
            "[ERROR] Agent reconciliation shell contains "
            "a Jinja delimiter: "
            + delimiter
        )

required = (
    'release=\'eva-agent\'',
    'namespace="$EVA_AGENT_NAMESPACE"',
    'chart_version="$EVA_AGENT_CHART_VERSION"',
    'values_file="$EVA_AGENT_VALUES_FILE"',
    'chart_source="$EVA_AGENT_CHART_SOURCE"',
    'use_chart_version="$EVA_AGENT_USE_CHART_VERSION"',
    'if [[ "$use_chart_version" == \'1\' ]]; then',
    'static_pv=\'eva-agent-model-pvc-storage\'',
    'static_pvc=\'eva-agent-models\'',
    'secret_name=\'eva-agent-secret\'',
    "kubectl create",
    "--dry-run=client",
    "rendered EVA Agent resource inventory is empty",
    "clean EVA Agent installation state",
    "deployed EVA Agent Helm release requires no legacy reconciliation",
    "partial EVA Agent persistent resource state detected",
    "EVA Agent PV/PVC binding identity mismatch",
    "EVA Agent Deployment pods exist during reconciliation",
    "legacy EVA Agent resource is not managed by rendered chart",
    "foreign Helm release ownership",
    "foreign Helm namespace ownership",
    'expected_group_kind="rbac.authorization.k8s.io/$kind"',
    'expected_group_kind="batch/$kind"',
    "live and rendered EVA Agent Secret data differ",
    "EVA Agent PV/PVC identity changed during legacy cleanup",
    "reconciled exact legacy EVA Agent resources",
)

for marker in required:
    if marker not in shell:
        raise SystemExit(
            "[ERROR] Agent reconciliation marker is absent: "
            + marker
        )

resource_query = (
    "serviceaccounts,configmaps,secrets,services,"
    "roles,rolebindings,deployments,statefulsets,"
    "daemonsets,cronjobs,poddisruptionbudgets"
)

if shell.count(resource_query) != 1:
    raise SystemExit(
        "[ERROR] Agent reconciliation resource query mismatch"
    )

queried_resources = set(resource_query.split(","))

for forbidden_resource in ("jobs", "pods"):
    if forbidden_resource in queried_resources:
        raise SystemExit(
            "[ERROR] historical resource type is queried: "
            + forbidden_resource
        )

for forbidden_mapping in (
    'resource="job.',
    'resource="pod/',
):
    if forbidden_mapping in shell:
        raise SystemExit(
            "[ERROR] historical resource deletion mapping is present: "
            + forbidden_mapping
        )

counter_contract = (
    (
        "resources_to_delete_count=0",
        1,
    ),
    (
        "resources_to_delete_count=$(("
        "resources_to_delete_count + 1))",
        1,
    ),
    (
        "count=$resources_to_delete_count",
        1,
    ),
)

for marker, expected_count in counter_contract:
    actual_count = shell.count(marker)

    if actual_count != expected_count:
        raise SystemExit(
            "[ERROR] Agent reconciliation deletion "
            "counter mismatch: "
            f"marker={marker} "
            f"expected={expected_count} "
            f"actual={actual_count}"
        )

if "${#resources_to_delete[@]}" in shell:
    raise SystemExit(
        "[ERROR] Bash array length conflicts with "
        "the Jinja comment delimiter"
    )

invalid_counter = """resources_to_delete_count=$(
        (
          resources_to_delete_count + 1
        )
      )"""

if invalid_counter in shell:
    raise SystemExit(
        "[ERROR] deletion counter uses command substitution "
        "instead of arithmetic expansion"
    )

persistent_adoption_markers = (
    "adopted persistent-only EVA Agent resources into Helm ownership",
    "EVA Agent PV/PVC identity changed during Helm adoption",
    "EVA Agent PV/PVC Helm ownership adoption failed",
    "app.kubernetes.io/managed-by=Helm",
    'meta.helm.sh/release-name="$release"',
    'meta.helm.sh/release-namespace="$namespace"',
    "kubectl label pv",
    "kubectl annotate pv",
    "kubectl label pvc",
    "kubectl annotate pvc",
)

for marker in persistent_adoption_markers:
    if marker not in shell:
        raise SystemExit(
            "[ERROR] Agent persistent adoption marker is absent: "
            + marker
        )

candidate_zero_marker = """if [[ "$candidate_count" == '0' ]]; then"""
persistent_zero_marker = """if [[ "$persistent_count" == '0' ]]; then"""
clean_state_marker = (
    'echo "[INFO] clean EVA Agent installation state"'
)
clean_exit_marker = """exit 0"""
persistent_state_marker = (
    'echo "[INFO] EVA Agent persistent-only installation state"'
)
persistent_adoption_marker = """kubectl label pv \\"""
persistent_success_marker = (
    'echo "[OK] adopted persistent-only EVA Agent resources '
    'into Helm ownership"'
)

candidate_zero_position = shell.find(
    candidate_zero_marker
)
persistent_zero_position = shell.find(
    persistent_zero_marker,
    candidate_zero_position,
)
clean_state_position = shell.find(
    clean_state_marker,
    persistent_zero_position,
)
clean_exit_position = shell.find(
    clean_exit_marker,
    clean_state_position,
)
persistent_state_position = shell.find(
    persistent_state_marker,
    clean_exit_position,
)
persistent_adoption_position = shell.find(
    persistent_adoption_marker,
    persistent_state_position,
)
persistent_success_position = shell.find(
    persistent_success_marker,
    persistent_adoption_position,
)

persistent_positions = (
    candidate_zero_position,
    persistent_zero_position,
    clean_state_position,
    clean_exit_position,
    persistent_state_position,
    persistent_adoption_position,
    persistent_success_position,
)

if min(persistent_positions) < 0:
    raise SystemExit(
        "[ERROR] persistent-only Agent flow is incomplete"
    )

if not (
    candidate_zero_position
    < persistent_zero_position
    < clean_state_position
    < clean_exit_position
    < persistent_state_position
    < persistent_adoption_position
    < persistent_success_position
):
    raise SystemExit(
        "[ERROR] persistent-only Agent flow order is invalid"
    )

candidate_branch_end = shell.find(
    "\nfi\n",
    persistent_state_position,
)

if candidate_branch_end < 0:
    raise SystemExit(
        "[ERROR] persistent-only candidate branch end is absent"
    )

persistent_only_tail = shell[
    persistent_state_position:candidate_branch_end
]

if "exit 0" in persistent_only_tail:
    raise SystemExit(
        "[ERROR] persistent-only Agent state exits before Helm adoption"
    )

if shell.count(
    'resource="role.rbac.authorization.k8s.io/$name"'
) != 1:
    raise SystemExit(
        "[ERROR] Agent Role mapping count mismatch"
    )

if shell.count(
    'resource="rolebinding.rbac.authorization.k8s.io/$name"'
) != 1:
    raise SystemExit(
        "[ERROR] Agent RoleBinding mapping count mismatch"
    )

environment = task.get("environment")

if not isinstance(environment, dict):
    raise SystemExit(
        "[ERROR] Agent reconciliation environment is absent"
    )

required_environment = (
    "KUBECONFIG",
    "HELM_PLUGINS",
    "EVA_AGENT_NAMESPACE",
    "EVA_AGENT_CHART_VERSION",
    "EVA_AGENT_VALUES_FILE",
    "EVA_AGENT_CHART_SOURCE",
    "EVA_AGENT_USE_CHART_VERSION",
)

for name in required_environment:
    if name not in environment:
        raise SystemExit(
            "[ERROR] Agent reconciliation environment "
            "variable is absent: "
            + name
        )

if "ternary('1', '0')" not in str(
    environment["EVA_AGENT_USE_CHART_VERSION"]
):
    raise SystemExit(
        "[ERROR] chart version selection is not rendered "
        "outside the shell body"
    )

changed_when = task.get("changed_when")

if not isinstance(changed_when, str):
    raise SystemExit(
        "[ERROR] Agent reconciliation changed_when is absent"
    )

for marker in (
    "[OK] reconciled exact legacy EVA Agent resources",
    "[OK] adopted persistent-only EVA Agent resources into Helm ownership",
):
    if changed_when.count(marker) != 1:
        raise SystemExit(
            "[ERROR] Agent mutation reporting mismatch: "
            + marker
        )

print("[OK] clean Agent installation remains mutation-free")
print("[OK] deployed Agent release bypasses legacy cleanup")
print("[OK] legacy resources require rendered chart membership")
print("[OK] Agent Secret data must match before cleanup")
print("[OK] Agent PV/PVC identity is protected")
print("[OK] Agent PV/PVC Helm ownership is adopted safely")
print("[OK] persistent-only retry state remains recoverable")
print("[OK] historical Job and Pod resources are excluded")
print("[OK] Agent reconciliation precedes Helm installation")
PY

echo 'EVA Agent reconciliation contract tests passed.'
