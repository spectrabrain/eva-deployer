#!/usr/bin/env bash
set -euo pipefail

repo_root="$(
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." &&
  pwd
)"

task_file="$repo_root/src/infra/roles/nfs/tasks/mount_share_path.yaml"
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

"$python_bin" - "$task_file" <<'PY'
from pathlib import Path
import sys
import yaml


path = Path(sys.argv[1])
tasks = yaml.safe_load(path.read_text())

if not isinstance(tasks, list):
    raise SystemExit(
        "[ERROR] NFS task file is not a list"
    )

task_names = [
    task.get("name")
    for task in tasks
    if isinstance(task, dict)
]

export_name = (
    "Allow localhost-only NFS export "
    "for EVA Agent share"
)
service_name = "Ensure NFS server is running"
reload_name = (
    "Reload NFS exports when configuration changed"
)
showmount_name = "Show NFS exports on localhost"

for name in (
    export_name,
    service_name,
    reload_name,
    showmount_name,
):
    actual_count = task_names.count(name)

    if actual_count != 1:
        raise SystemExit(
            "[ERROR] NFS task count mismatch: "
            f"name={name} actual={actual_count}"
        )

if not (
    task_names.index(export_name)
    < task_names.index(service_name)
    < task_names.index(reload_name)
    < task_names.index(showmount_name)
):
    raise SystemExit(
        "[ERROR] NFS task ordering is invalid"
    )

export_task = next(
    task
    for task in tasks
    if task.get("name") == export_name
)

lineinfile = export_task.get(
    "ansible.builtin.lineinfile"
)

if not isinstance(lineinfile, dict):
    raise SystemExit(
        "[ERROR] NFS export lineinfile is absent"
    )

expected_regexp = (
    "^{{ (nfs_share_path | regex_escape) }}"
    "\\s+.*$"
)

expected_line = (
    "{{ nfs_share_path }} "
    "127.0.0.1("
    "rw,sync,no_subtree_check,root_squash,"
    "anonuid=10001,anongid=10001)"
)

if lineinfile.get("regexp") != expected_regexp:
    raise SystemExit(
        "[ERROR] NFS export regexp mismatch"
    )

if lineinfile.get("line") != expected_line:
    raise SystemExit(
        "[ERROR] NFS export line mismatch"
    )

if lineinfile.get("state") == "absent":
    raise SystemExit(
        "[ERROR] NFS export is deleted before re-adding"
    )

if lineinfile.get("create") is not True:
    raise SystemExit(
        "[ERROR] NFS exports file creation is not enabled"
    )

if lineinfile.get("mode") != "0644":
    raise SystemExit(
        "[ERROR] NFS exports file mode mismatch"
    )

if export_task.get("register") != "nfs_exports_line":
    raise SystemExit(
        "[ERROR] NFS export change result is not registered"
    )

service_task = next(
    task
    for task in tasks
    if task.get("name") == service_name
)

systemd = service_task.get(
    "ansible.builtin.systemd"
)

if not isinstance(systemd, dict):
    raise SystemExit(
        "[ERROR] NFS service configuration is absent"
    )

if systemd.get("name") != "nfs-kernel-server":
    raise SystemExit(
        "[ERROR] NFS service name mismatch"
    )

if systemd.get("state") != "started":
    raise SystemExit(
        "[ERROR] NFS service is not managed idempotently"
    )

if systemd.get("enabled") is not True:
    raise SystemExit(
        "[ERROR] NFS service is not enabled"
    )

for task in tasks:
    systemd_config = task.get(
        "ansible.builtin.systemd"
    )

    if (
        isinstance(systemd_config, dict)
        and
        systemd_config.get("state") == "restarted"
    ):
        raise SystemExit(
            "[ERROR] disruptive NFS restart remains"
        )

    line_config = task.get(
        "ansible.builtin.lineinfile"
    )

    if (
        isinstance(line_config, dict)
        and
        line_config.get("path") == "/etc/exports"
        and
        line_config.get("state") == "absent"
    ):
        raise SystemExit(
            "[ERROR] NFS export delete task remains"
        )

reload_task = next(
    task
    for task in tasks
    if task.get("name") == reload_name
)

if (
    reload_task.get("ansible.builtin.command")
    != "exportfs -ra"
):
    raise SystemExit(
        "[ERROR] NFS export reload command mismatch"
    )

conditions = reload_task.get("when")

if not isinstance(conditions, list):
    raise SystemExit(
        "[ERROR] NFS reload conditions are absent"
    )

for condition in (
    "not ansible_check_mode",
    "nfs_exports_line.changed",
):
    if condition not in conditions:
        raise SystemExit(
            "[ERROR] NFS reload condition is absent: "
            + condition
        )

if reload_task.get("changed_when") is not True:
    raise SystemExit(
        "[ERROR] executed export reload is not reported changed"
    )

print("[OK] NFS export is managed by one task")
print("[OK] export options are normalized by share path")
print("[OK] NFS service is started without restart")
print("[OK] export reload runs only after a real change")
print("[OK] downstream showmount validation is preserved")
PY

echo 'NFS share idempotency contract tests passed.'
