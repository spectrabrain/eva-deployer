#!/usr/bin/env bash
set -euo pipefail

repo_root="$(
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." &&
  pwd
)"

task_file="$repo_root/src/solution/roles/eva_agent/tasks/agent.yaml"
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

if [[ ! -x "$python_bin" ]]; then
  echo "[ERROR] Python interpreter is not executable: $python_bin"
  exit 1
fi

"$python_bin" - "$task_file" <<'PYTEST'
from pathlib import Path
import sys
import yaml


path = Path(sys.argv[1])
tasks = yaml.safe_load(path.read_text())

if not isinstance(tasks, list):
    raise SystemExit(
        "[ERROR] EVA Agent tasks are not a list"
    )

task_name = (
    "Wait until EVA Agent deployment pods "
    "are terminated before reinstall"
)

matches = [
    task
    for task in tasks
    if task.get("name") == task_name
]

if len(matches) != 1:
    raise SystemExit(
        "[ERROR] EVA Agent deployment wait task count mismatch: "
        f"actual={len(matches)}"
    )

task = matches[0]
command = task.get("ansible.builtin.command")

if not isinstance(command, str):
    raise SystemExit(
        "[ERROR] EVA Agent deployment wait command is absent"
    )

normalized = " ".join(command.split())

required = (
    "kubectl wait --for=delete pod",
    "-n {{ eva_agent_namespace }}",
    "-l app.kubernetes.io/name=eva-agent,pod-template-hash",
    "--timeout=180s",
)

for marker in required:
    if marker not in normalized:
        raise SystemExit(
            "[ERROR] EVA Agent deployment wait marker is absent: "
            + marker
        )

broad_selector = (
    "-l app.kubernetes.io/name=eva-agent "
    "--timeout=180s"
)

if broad_selector in normalized:
    raise SystemExit(
        "[ERROR] EVA Agent wait selector includes Job pods"
    )

conditions = task.get("when")

if not isinstance(conditions, list):
    raise SystemExit(
        "[ERROR] EVA Agent deployment wait conditions are absent"
    )

expected_conditions = (
    "not ansible_check_mode",
    "eva_agent_deployment_exists.rc == 0",
)

for condition in expected_conditions:
    if condition not in conditions:
        raise SystemExit(
            "[ERROR] EVA Agent wait condition is absent: "
            + condition
        )

if task.get("changed_when") is not False:
    raise SystemExit(
        "[ERROR] EVA Agent deployment wait must be read-only"
    )

failed_when = task.get("failed_when")

if not isinstance(failed_when, list):
    raise SystemExit(
        "[ERROR] EVA Agent deployment wait failure policy is absent"
    )

if "eva_agent_wait_delete.rc != 0" not in failed_when:
    raise SystemExit(
        "[ERROR] EVA Agent deployment wait rc guard is absent"
    )

print("[OK] EVA Agent wait selects Deployment pods only")
print("[OK] completed Job pods are excluded")
print("[OK] deployment wait remains bounded to 180 seconds")
print("[OK] deployment wait remains read-only")
PYTEST

echo 'EVA Agent reinstall wait contract tests passed.'
