#!/usr/bin/env bash
set -euo pipefail

repo_root="$(
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." &&
  pwd
)"

migration="$repo_root/src/solution/roles/eva_agent/tasks/migrate_legacy_vllm.yaml"
verification="$repo_root/src/solution/roles/eva_agent/tasks/verify_vllm_reconciliation.yaml"
dependencies="$repo_root/src/solution/roles/eva_agent/tasks/dependencies.yaml"
python_bin="${PYTHON_BIN:-}"

if [[ -z "$python_bin" ]]; then
  if [[ -x "$repo_root/.venv/bin/python" ]]; then
    python_bin="$repo_root/.venv/bin/python"
  elif command -v python3 >/dev/null 2>&1; then
    python_bin="$(command -v python3)"
  else
    echo "[ERROR] Python interpreter is unavailable"
    exit 1
  fi
fi

for path in \
  "$migration" \
  "$verification" \
  "$dependencies"
do
  if [[ ! -f "$path" ]]; then
    echo "[ERROR] required file is absent: $path"
    exit 1
  fi
done

if [[ ! -x "$python_bin" ]]; then
  echo "[ERROR] Python interpreter is unavailable: $python_bin"
  exit 1
fi

"$python_bin" - \
  "$migration" \
  "$verification" \
  "$dependencies" <<'PY'
from pathlib import Path
import re
import sys

import yaml

migration_path = Path(sys.argv[1])
verification_path = Path(sys.argv[2])
dependencies_path = Path(sys.argv[3])

migration_content = yaml.safe_load(
    migration_path.read_text()
)

if (
    not isinstance(migration_content, list)
    or len(migration_content) != 1
):
    raise SystemExit(
        "[ERROR] expected exactly one VLLM migration task"
    )

migration_shell = migration_content[0].get(
    "ansible.builtin.shell"
)

if not isinstance(migration_shell, str):
    raise SystemExit(
        "[ERROR] VLLM migration shell is absent"
    )

absent_branch_match = re.search(
    r"""
    if[ \t]+\(\(
    [ \t]*static_pv_exists[ \t]*==[ \t]*0
    [ \t]*&&[ \t]*
    claim_exists[ \t]*==[ \t]*0
    [ \t]*\)\);[ \t]*then
    (?P<body>.*?)
    elif[ \t]+\(\(
    [ \t]*static_pv_exists[ \t]*!=[ \t]*1
    [ \t]*\|\|[ \t]*
    claim_exists[ \t]*!=[ \t]*1
    [ \t]*\)\);[ \t]*then
    """,
    migration_shell,
    re.DOTALL | re.VERBOSE,
)

if absent_branch_match is None:
    raise SystemExit(
        "[ERROR] clean-install branch is absent"
    )

absent_branch = absent_branch_match.group("body")

for marker in (
    '[[ -f "$snapshot_file" ]]',
    (
        "VLLM persistent storage migration "
        "retry state validated"
    ),
    '[[ "$legacy_candidate_count" == \'0\' ]]',
    'echo "[INFO] clean VLLM installation state"',
):
    if marker not in absent_branch:
        raise SystemExit(
            "[ERROR] clean/retry branch is missing: "
            + marker
        )

for mutation in (
    "kubectl delete pvc",
    "kubectl delete pv",
):
    if mutation in absent_branch:
        raise SystemExit(
            "[ERROR] clean/retry branch contains mutation: "
            + mutation
        )

legacy_guard = (
    "VLLM persistent drift exists without "
    "exact legacy tracking evidence"
)

legacy_guard_position = migration_shell.find(
    legacy_guard
)

pvc_delete_position = migration_shell.find(
    "kubectl delete pvc"
)

dynamic_delete_match = re.search(
    r'kubectl[ \t]+delete[ \t]+pv'
    r'(?:[ \t]*\\?[ \t]*\n[ \t]*)*'
    r'"\$dynamic_pv"',
    migration_shell,
)

static_delete_match = re.search(
    r'kubectl[ \t]+delete[ \t]+pv'
    r'(?:[ \t]*\\?[ \t]*\n[ \t]*)*'
    r'"\$static_pv"',
    migration_shell,
)

if legacy_guard_position < 0:
    raise SystemExit(
        "[ERROR] legacy evidence guard is absent"
    )

if pvc_delete_position < 0:
    raise SystemExit(
        "[ERROR] expected PVC deletion is absent"
    )

if dynamic_delete_match is None:
    raise SystemExit(
        "[ERROR] expected dynamic PV deletion is absent"
    )

if static_delete_match is None:
    raise SystemExit(
        "[ERROR] expected static PV deletion is absent"
    )

if not (
    legacy_guard_position
    < pvc_delete_position
    < dynamic_delete_match.start()
    < static_delete_match.start()
):
    raise SystemExit(
        "[ERROR] destructive reconciliation order is invalid"
    )

if migration_shell.count("kubectl delete pvc") != 1:
    raise SystemExit(
        "[ERROR] PVC deletion command count is not one"
    )

if 'rm -f "$snapshot_file"' in migration_shell:
    raise SystemExit(
        "[ERROR] migration snapshot is removed before retry"
    )

persistent_snapshot = (
    'snapshot_file="$cache_path/'
    '.eva-vllm-storage-migration.tsv"'
)

if migration_shell.count(persistent_snapshot) != 1:
    raise SystemExit(
        "[ERROR] migration does not use persistent cache marker"
    )

if "storage-migration-before.tsv" in migration_shell:
    raise SystemExit(
        "[ERROR] migration uses ephemeral workspace state"
    )

recovery_position = migration_shell.find(
    "storage_recovery_pending=1"
)
candidate_position = migration_shell.find(
    'if [[ ! -s "$expected_resources_file" ]]; then'
)
record_position = migration_shell.find(
    "recorded recovered VLLM storage migration state"
)
resource_delete_match = re.search(
    (
        r'kubectl[ \t]+delete'
        r'(?:[ \t]*\\?[ \t]*\n[ \t]*)+'
        r'"\$resource"'
    ),
    migration_shell,
)

if resource_delete_match is None:
    raise SystemExit(
        "[ERROR] interrupted migration resource deletion is absent"
    )

delete_position = resource_delete_match.start()

if min(
    recovery_position,
    candidate_position,
    record_position,
    delete_position,
) < 0:
    raise SystemExit(
        "[ERROR] interrupted migration recovery is incomplete"
    )

if not (
    recovery_position
    < candidate_position
    < record_position
    < delete_position
):
    raise SystemExit(
        "[ERROR] interrupted migration recovery order is unsafe"
    )

if ".s3-sync.done" in migration_shell:
    raise SystemExit(
        "[ERROR] environment-specific cache marker is mandatory"
    )

rendered_json_inventory = (
    '\' "$rendered_json_file" |',
    'sort -u > "$rendered_resources_file"',
    'rendered eva-agent-vllm resource inventory is empty',
)

for marker in rendered_json_inventory:
    if migration_shell.count(marker) != 1:
        raise SystemExit(
            "[ERROR] rendered JSON inventory marker mismatch: "
            + marker
        )

if 'RS="---"' in migration_shell:
    raise SystemExit(
        "[ERROR] rendered inventory parses Helm YAML manually"
    )

json_source_position = migration_shell.find(
    '\' "$rendered_json_file" |'
)
inventory_sort_position = migration_shell.find(
    'sort -u > "$rendered_resources_file"',
    json_source_position,
)
candidate_source_position = migration_shell.find(
    'join("\\u001f")',
    inventory_sort_position,
)

if min(
    json_source_position,
    inventory_sort_position,
    candidate_source_position,
) < 0:
    raise SystemExit(
        "[ERROR] rendered JSON inventory is incomplete"
    )

if not (
    json_source_position
    < inventory_sort_position
    < candidate_source_position
):
    raise SystemExit(
        "[ERROR] rendered JSON inventory order is invalid"
    )

for marker in (
    "partial VLLM persistent resource state detected",
    "legacy VLLM static PV contract mismatch",
    "legacy VLLM PVC contract mismatch",
    "drifted dynamic VLLM PV contract mismatch",
    "retained dynamic VLLM cache path was removed",
    "canonical VLLM cache path became empty",
):
    if migration_shell.count(marker) != 1:
        raise SystemExit(
            "[ERROR] migration guard count mismatch: "
            + marker
        )

verification_content = yaml.safe_load(
    verification_path.read_text()
)

if (
    not isinstance(verification_content, list)
    or len(verification_content) != 1
):
    raise SystemExit(
        "[ERROR] expected exactly one VLLM verification task"
    )

verification_task = verification_content[0]
verification_shell = verification_task.get(
    "ansible.builtin.shell"
)

if not isinstance(verification_shell, str):
    raise SystemExit(
        "[ERROR] VLLM verification shell is absent"
    )

if verification_shell.count(persistent_snapshot) != 1:
    raise SystemExit(
        "[ERROR] verifier does not use persistent cache marker"
    )

if "storage-migration-before.tsv" in verification_shell:
    raise SystemExit(
        "[ERROR] verifier uses ephemeral workspace state"
    )

if verification_task.get("changed_when") is not False:
    raise SystemExit(
        "[ERROR] VLLM verification must be read-only"
    )

if "kubectl delete" in verification_shell:
    raise SystemExit(
        "[ERROR] VLLM verification deletes Kubernetes resources"
    )

for marker in (
    "binding_verified=0",
    "for attempt in $(seq 1 25)",
    "binding did not become ready within 120 seconds",
    (
        "VLLM cache content validation skipped "
        "for non-migration installation"
    ),
    "migrated VLLM cache content was preserved",
):
    if verification_shell.count(marker) != 1:
        raise SystemExit(
            "[ERROR] VLLM verifier state marker count mismatch: "
            + marker
        )

snapshot_position = verification_shell.find(
    'if [[ -f "$snapshot_file" ]]; then'
)
hub_position = verification_shell.find(
    'if [[ ! -d "$cache_path/hub" ]]; then'
)
empty_position = verification_shell.find(
    'find "$cache_path" -mindepth 1 -print -quit'
)
remove_position = verification_shell.find(
    'rm -f "$snapshot_file"'
)

if min(
    snapshot_position,
    hub_position,
    empty_position,
    remove_position,
) < 0:
    raise SystemExit(
        "[ERROR] migration-only cache verification is incomplete"
    )

if not (
    snapshot_position
    < hub_position
    < empty_position
    < remove_position
):
    raise SystemExit(
        "[ERROR] migration cache verification order is invalid"
    )

clean_install_prefix = verification_shell[:snapshot_position]

if '[[ ! -d "$cache_path/hub" ]]' in clean_install_prefix:
    raise SystemExit(
        "[ERROR] clean installation requires pre-populated model hub"
    )

if ".s3-sync.done" in verification_shell:
    raise SystemExit(
        "[ERROR] verifier requires an environment-specific cache marker"
    )

for marker in (
    "eva-agent-vllm Helm release is not deployed",
    "installed VLLM static PV contract mismatch",
    "installed VLLM PVC contract mismatch",
    "installed VLLM PV and PVC UID binding mismatch",
    "no installed VLLM workload references the expected PVC",
    "eva-agent-vllm static PV and PVC binding verified",
):
    if verification_shell.count(marker) != 1:
        raise SystemExit(
            "[ERROR] verification guard count mismatch: "
            + marker
        )

dependencies = dependencies_path.read_text()

ordered_markers = (
    (
        "- name: Reconcile exact legacy Argo CD-managed "
        "eva-agent-vllm resources"
    ),
    "- name: Install/Upgrade eva-agent-vllm (online)",
    "- name: Install/Upgrade eva-agent-vllm (airgap)",
    (
        "- name: Verify eva-agent-vllm reconciliation "
        "after Helm installation"
    ),
    (
        "- name: Capture effective Qdrant "
        "and vLLM Helm values"
    ),
)

positions = []

for marker in ordered_markers:
    if dependencies.count(marker) != 1:
        raise SystemExit(
            "[ERROR] dependency task count mismatch: "
            + marker
        )

    positions.append(dependencies.find(marker))

if positions != sorted(positions):
    raise SystemExit(
        "[ERROR] VLLM dependency execution order is invalid"
    )

include_line = (
    "  ansible.builtin.include_tasks: "
    "verify_vllm_reconciliation.yaml"
)

if dependencies.count(include_line) != 1:
    raise SystemExit(
        "[ERROR] VLLM verification include count is not one"
    )

print("[OK] clean installation is mutation-free")
print("[OK] retry requires persistent migration evidence")
print("[OK] legacy migration is fail-closed")
print("[OK] persistent deletion order is deterministic")
print("[OK] post-install verification is read-only")
print("[OK] VLLM task execution order is valid")
PY

echo "EVA vLLM reconciliation contract tests passed."
