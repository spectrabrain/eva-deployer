#!/usr/bin/env bash
set -euo pipefail

repo_root="$(
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." &&
  pwd
)"

task_file="$repo_root/src/solution/roles/eva_agent/tasks/reconcile_qdrant.yaml"
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

if [[ ! -f "$task_file" ]]; then
  echo "[ERROR] Qdrant reconciliation task is absent: $task_file"
  exit 1
fi

"$python_bin" - "$task_file" <<'PY'
from pathlib import Path
import sys
import yaml


path = Path(sys.argv[1])
content = yaml.safe_load(path.read_text())

if not isinstance(content, list) or len(content) != 1:
    raise SystemExit(
        "[ERROR] expected exactly one Qdrant reconciliation task"
    )

task = content[0]
shell = task.get("ansible.builtin.shell")

if not isinstance(shell, str) or not shell.strip():
    raise SystemExit(
        "[ERROR] Qdrant reconciliation shell is absent"
    )

if 'RS="---"' in shell:
    raise SystemExit(
        "[ERROR] Qdrant reconciliation parses Helm YAML manually"
    )

required_markers = (
    "parse_manifest_resources() {",
    'local manifest_file="$1"',
    "validate_no_standalone_persistent_resources() {",
    "kubectl create",
    "--dry-run=client",
    'jq -s -r',
    '.kind == "List"',
    '(.metadata.name // "") != ""',
    "| @tsv",
    "failed to build rendered Qdrant resource inventory",
    "failed to build failed-release Qdrant resource inventory",
    "rendered Qdrant resource inventory is empty",
    "failed-release Qdrant resource inventory is empty",
    "deployed Qdrant Helm release requires no reconciliation",
    "clean Qdrant installation state",
    "Qdrant PVC-only reinstall state validated",
    "removed failed Qdrant Helm release with PVC identities preserved",
    "reconciled exact legacy Qdrant resources",
)

for marker in required_markers:
    if marker not in shell:
        raise SystemExit(
            "[ERROR] Qdrant reconciliation marker is absent: "
            + marker
        )

if shell.count("parse_manifest_resources() {") != 1:
    raise SystemExit(
        "[ERROR] canonical parser function count mismatch"
    )

if shell.count(
    "validate_no_standalone_persistent_resources() {"
) != 1:
    raise SystemExit(
        "[ERROR] persistent validator function count mismatch"
    )

if shell.count("parse_manifest_resources \\") != 2:
    raise SystemExit(
        "[ERROR] canonical parser invocation count mismatch"
    )

if shell.count(
    "validate_no_standalone_persistent_resources \\"
) != 2:
    raise SystemExit(
        "[ERROR] persistent validator invocation count mismatch"
    )

function_position = shell.find(
    "parse_manifest_resources() {"
)

normal_position = shell.find(
    "failed to build rendered Qdrant resource inventory"
)

deployed_position = shell.find(
    "deployed Qdrant Helm release requires no reconciliation"
)

failed_position = shell.find(
    "failed to build failed-release Qdrant resource inventory"
)

clean_position = shell.find(
    "clean Qdrant installation state"
)

legacy_position = shell.find(
    "reconciled exact legacy Qdrant resources"
)

positions = (
    function_position,
    normal_position,
    deployed_position,
    failed_position,
    clean_position,
    legacy_position,
)

if min(positions) < 0:
    raise SystemExit(
        "[ERROR] Qdrant reconciliation branch ordering is incomplete"
    )

if not (
    function_position
    < normal_position
    < deployed_position
    < failed_position
    < clean_position
    < legacy_position
):
    raise SystemExit(
        "[ERROR] Qdrant reconciliation branch ordering changed"
    )

changed_when = task.get("changed_when")

if not isinstance(changed_when, str):
    raise SystemExit(
        "[ERROR] Qdrant reconciliation changed_when is absent"
    )

required_change_markers = (
    (
        "[OK] reconciled exact legacy Qdrant resources",
        1,
    ),
    (
        "[OK] removed failed Qdrant Helm release",
        1,
    ),
    (
        "eva_agent_qdrant_reconciliation.stdout",
        2,
    ),
)

for marker, expected_count in required_change_markers:
    actual_count = changed_when.count(marker)

    if actual_count != expected_count:
        raise SystemExit(
            "[ERROR] Qdrant changed_when marker mismatch: "
            f"marker={marker} "
            f"expected={expected_count} "
            f"actual={actual_count}"
        )

if " or " not in " ".join(changed_when.split()):
    raise SystemExit(
        "[ERROR] Qdrant changed_when mutation conditions are not combined"
    )

for read_only_marker in (
    "clean Qdrant installation state",
    "deployed Qdrant Helm release requires no reconciliation",
    "Qdrant PVC-only reinstall state validated",
):
    if read_only_marker in changed_when:
        raise SystemExit(
            "[ERROR] read-only Qdrant state reports changed: "
            + read_only_marker
        )

print("[OK] Qdrant mutation-only change reporting")
print("[OK] Qdrant parser uses Kubernetes JSON truth")
print("[OK] normal and failed release inventories share one parser")
print("[OK] clean installation branch remains present")
print("[OK] deployed upgrade branch remains present")
print("[OK] failed release cleanup branch remains present")
print("[OK] legacy migration branch remains present")
PY

fixture="$(mktemp)"
actual="$(mktemp)"
expected="$(mktemp)"

cleanup() {
  rm -f "$fixture" "$actual" "$expected"
}

trap cleanup EXIT

cat > "$fixture" <<'JSON'
{
  "apiVersion": "v1",
  "kind": "ServiceAccount",
  "metadata": {
    "name": "eva-agent-qdrant"
  }
}
{
  "apiVersion": "v1",
  "kind": "List",
  "items": [
    {
      "apiVersion": "v1",
      "kind": "ConfigMap",
      "metadata": {
        "name": "eva-agent-qdrant"
      }
    },
    {
      "apiVersion": "v1",
      "kind": "Service",
      "metadata": {
        "name": "eva-agent-qdrant"
      }
    },
    {
      "apiVersion": "v1",
      "kind": "Service",
      "metadata": {
        "name": "eva-agent-qdrant-headless"
      }
    },
    {
      "apiVersion": "apps/v1",
      "kind": "StatefulSet",
      "metadata": {
        "name": "eva-agent-qdrant"
      }
    }
  ]
}
JSON

cat > "$expected" <<'EOF'
ConfigMap	eva-agent-qdrant
Service	eva-agent-qdrant
Service	eva-agent-qdrant-headless
ServiceAccount	eva-agent-qdrant
StatefulSet	eva-agent-qdrant
EOF

jq -s -r '
  .[]
  | if .kind == "List"
    then .items[]
    else .
    end
  | select(
      (.kind // "") != ""
      and
      (.metadata.name // "") != ""
    )
  | [
      .kind,
      .metadata.name
    ]
  | @tsv
' "$fixture" |
  LC_ALL=C sort -u > "$actual"

LC_ALL=C sort -o "$expected" "$expected"

if cmp -s "$expected" "$actual"; then
  echo '[OK] concatenated JSON and List inventory'
else
  echo '[ERROR] canonical Qdrant inventory mismatch'
  diff -u "$expected" "$actual" || true
  exit 1
fi

cat > "$fixture" <<'JSON'
[
  {
    "kind": "ConfigMap",
    "metadata": {
      "name": "eva-agent-qdrant"
    }
  },
  {
    "kind": "PersistentVolumeClaim",
    "metadata": {
      "name": "unexpected-qdrant-pvc"
    }
  }
]
JSON

persistent_count="$(
  jq -r '
    .[]
    | select(
        .kind == "PersistentVolume"
        or
        .kind == "PersistentVolumeClaim"
        or
        .kind == "StorageClass"
      )
    | [
        .kind,
        .metadata.name
      ]
    | @tsv
  ' "$fixture" |
    wc -l |
    tr -d ' '
)"

if [[ "$persistent_count" == '1' ]]; then
  echo '[OK] standalone persistent resource detection'
else
  echo "[ERROR] persistent resource detection count=$persistent_count"
  exit 1
fi

echo 'EVA Qdrant reconciliation contract tests passed.'
