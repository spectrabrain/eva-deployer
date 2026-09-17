#!/usr/bin/env bash
set -euo pipefail

repo_root="$(
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." &&
  pwd
)"

main_file="$repo_root/src/solution/roles/eva_vision/tasks/main.yaml"
reconcile_file="$repo_root/src/solution/roles/eva_vision/tasks/reconcile_vision.yaml"
python_bin="${PYTHON_BIN:-}"

if [[ -z "$python_bin" ]]; then
  if [[ -x "$repo_root/.venv/bin/python" ]]; then
    python_bin="$repo_root/.venv/bin/python"
  elif command -v python3 >/dev/null 2>&1; then
    python_bin="$(command -v python3)"
  else
    echo "[ERROR] Python interpreter is unavailable"
    false
  fi
fi

"$python_bin" - \
  "$main_file" \
  "$reconcile_file" <<'PY'
from pathlib import Path
import sys
import yaml


main_path = Path(sys.argv[1])
reconcile_path = Path(sys.argv[2])

main_tasks = yaml.safe_load(main_path.read_text())
reconcile_tasks = yaml.safe_load(reconcile_path.read_text())

if not isinstance(main_tasks, list):
    raise SystemExit(
        "[ERROR] EVA Vision main tasks are not a list"
    )

if not isinstance(reconcile_tasks, list):
    raise SystemExit(
        "[ERROR] EVA Vision reconciliation tasks are not a list"
    )

if len(reconcile_tasks) != 1:
    raise SystemExit(
        "[ERROR] expected one EVA Vision reconciliation task"
    )

task_names = [
    task.get("name")
    for task in main_tasks
    if isinstance(task, dict)
]

wait_name = (
    "Wait until EVA Vision pods are terminated before reinstall"
)
reconcile_name = (
    "Reconcile exact legacy Argo CD-managed EVA Vision resources"
)
online_name = "Deploy EVA Vision from online Helm repo"
offline_name = "Deploy EVA Vision from offline chart"

for name in (
    wait_name,
    reconcile_name,
    online_name,
    offline_name,
):
    actual_count = task_names.count(name)

    if actual_count != 1:
        raise SystemExit(
            "[ERROR] EVA Vision task count mismatch: "
            f"name={name} actual={actual_count}"
        )

if not (
    task_names.index(wait_name)
    < task_names.index(reconcile_name)
    < task_names.index(online_name)
    < task_names.index(offline_name)
):
    raise SystemExit(
        "[ERROR] EVA Vision reconciliation ordering is invalid"
    )

include_task = next(
    task
    for task in main_tasks
    if task.get("name") == reconcile_name
)

if (
    include_task.get("ansible.builtin.include_tasks")
    != "reconcile_vision.yaml"
):
    raise SystemExit(
        "[ERROR] EVA Vision reconciliation include mismatch"
    )

task = reconcile_tasks[0]
shell = task.get("ansible.builtin.shell")

if not isinstance(shell, str):
    raise SystemExit(
        "[ERROR] EVA Vision reconciliation shell is absent"
    )

required_markers = (
    "rendered EVA Vision resource inventory is unsupported",
    "clean EVA Vision installation state",
    "deployed EVA Vision Helm release requires no legacy reconciliation",
    "failed EVA Vision Helm release will be recovered by Helm upgrade",
    "unsafe EVA Vision Helm release status",
    "live EVA Vision resource is absent from rendered chart",
    "partial legacy EVA Vision resource set detected",
    "foreign or partial Helm ownership",
    "EVA Vision resource has no Argo CD tracking ID",
    "EVA Vision tracking identity mismatch",
    "EVA Vision pods remain before legacy reconciliation",
    "reconciled exact legacy EVA Vision resources",
)

for marker in required_markers:
    if marker not in shell:
        raise SystemExit(
            "[ERROR] EVA Vision reconciliation marker is absent: "
            + marker
        )

expected_inventory = (
    "ConfigMap\teva-vision-config",
    "Deployment\teva-vision",
    "Service\teva-vision",
    "ServiceAccount\teva-vision-sa",
)

for marker in expected_inventory:
    if marker not in shell:
        raise SystemExit(
            "[ERROR] EVA Vision rendered inventory is incomplete: "
            + marker
        )

if 'if [[ "$candidate_count" != \'4\' ]]; then' not in shell:
    raise SystemExit(
        "[ERROR] partial legacy EVA Vision resource guard is absent"
    )

deletion_order = (
    '"deployment/eva-vision"',
    '"service/eva-vision"',
    '"configmap/eva-vision-config"',
    '"serviceaccount/eva-vision-sa"',
)

deletion_positions = [
    shell.find(marker)
    for marker in deletion_order
]

if min(deletion_positions) < 0:
    raise SystemExit(
        "[ERROR] EVA Vision deletion inventory is incomplete"
    )

if deletion_positions != sorted(deletion_positions):
    raise SystemExit(
        "[ERROR] EVA Vision deletion order is invalid"
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
        "remaining_count=0",
        1,
    ),
    (
        "remaining_count=$((remaining_count + 1))",
        1,
    ),
)

for marker, expected_count in counter_contract:
    actual_count = shell.count(marker)

    if actual_count != expected_count:
        raise SystemExit(
            "[ERROR] EVA Vision counter contract mismatch: "
            f"marker={marker} "
            f"expected={expected_count} "
            f"actual={actual_count}"
        )

invalid_counter_markers = (
    "resources_to_delete_count=$(\n",
    "remaining_count=$(\n",
)

for marker in invalid_counter_markers:
    if marker in shell:
        raise SystemExit(
            "[ERROR] EVA Vision counter uses command substitution"
        )

for forbidden in (
    "persistentvolume",
    "persistentvolumeclaim",
    "storageclass",
    "kubectl annotate",
    "kubectl label",
):
    if forbidden in shell.lower():
        raise SystemExit(
            "[ERROR] unsupported Vision adoption behavior is present: "
            + forbidden
        )

changed_when = task.get("changed_when")

if (
    not isinstance(changed_when, str)
    or
    changed_when.count(
        "[OK] reconciled exact legacy EVA Vision resources"
    ) != 1
):
    raise SystemExit(
        "[ERROR] EVA Vision mutation reporting mismatch"
    )

environment = task.get("environment")

if not isinstance(environment, dict):
    raise SystemExit(
        "[ERROR] EVA Vision reconciliation environment is absent"
    )

for name in (
    "EVA_VISION_NAMESPACE",
    "EVA_VISION_RELEASE",
    "EVA_VISION_CHART_VERSION",
    "EVA_VISION_VALUES_FILE",
    "EVA_VISION_CHART_SOURCE",
    "EVA_VISION_USE_CHART_VERSION",
):
    if name not in environment:
        raise SystemExit(
            "[ERROR] EVA Vision environment is absent: "
            + name
        )

print("[OK] clean Vision installation remains mutation-free")
print("[OK] deployed and failed Vision releases bypass legacy cleanup")
print("[OK] exact four-resource Vision inventory is enforced")
print("[OK] legacy resources require rendered chart membership")
print("[OK] foreign Helm ownership is rejected")
print("[OK] exact Argo CD tracking identities are required")
print("[OK] Vision pods must be absent before cleanup")
print("[OK] only non-persistent Vision resources are recreated")
print("[OK] Vision deletion counters use arithmetic expansion")
print("[OK] Vision reconciliation precedes Helm deployment")
PY

echo 'EVA Vision reconciliation contract tests passed.'
