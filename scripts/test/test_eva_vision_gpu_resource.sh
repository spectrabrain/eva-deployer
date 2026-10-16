#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ANSIBLE_PLAYBOOK="${ANSIBLE_PLAYBOOK:-ansible-playbook}"

if [[ -x "$REPO_ROOT/.venv/bin/python" ]]; then
  PYTHON_BIN="$REPO_ROOT/.venv/bin/python"
else
  PYTHON_BIN="$(command -v python3 || true)"
fi

if [[ -z "$PYTHON_BIN" || ! -x "$PYTHON_BIN" ]]; then
  echo "[ERROR] Python interpreter is unavailable" >&2
  false
fi

mkdir -p "$HOME/tmp"
TMP_ROOT="$(mktemp -d "$HOME/tmp/eva-vision-gpu-test.XXXXXX")"

cleanup() {
  rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

run_case() {
  local name="$1"
  local mig_enabled="$2"
  local resources="$3"
  local expected_resource="${4:-}"
  local root="$TMP_ROOT/$name"

  mkdir -p "$root/bin"
  cat >"$root/bin/kubectl" <<EOF
#!/usr/bin/env bash
printf '%s\n' '$resources'
EOF
  chmod 0755 "$root/bin/kubectl"

  cat >"$root/playbook.yaml" <<EOF
---
- hosts: localhost
  gather_facts: false
  connection: local
  vars:
    eva_vision_kubeconfig: $root/kubeconfig
    eva_vision_mig_enabled_from_config: $mig_enabled
    eva_vision_gpu_resource_name: ''
  tasks:
    - ansible.builtin.include_tasks: $REPO_ROOT/src/solution/roles/eva_vision/tasks/gpu_resource.yaml
EOF

  if [[ -n "$expected_resource" ]]; then
    cat >>"$root/playbook.yaml" <<EOF
    - ansible.builtin.assert:
        that:
          - eva_vision_gpu_resource_name == '$expected_resource'
EOF
    if ! PATH="$root/bin:$PATH" "$ANSIBLE_PLAYBOOK" "$root/playbook.yaml" >"$root/ansible.log" 2>&1; then
      cat "$root/ansible.log" >&2
      exit 1
    fi
  else
    if PATH="$root/bin:$PATH" "$ANSIBLE_PLAYBOOK" "$root/playbook.yaml" >"$root/ansible.log" 2>&1; then
      echo "$name unexpectedly selected an accelerator resource" >&2
      exit 1
    fi
    grep -Fq 'EVA Vision accelerator resource' "$root/ansible.log"
  fi
}

run_case single-mig true '{"items":[{"status":{"allocatable":{"nvidia.com/mig-2g.24gb":"2"}}}]}' 'nvidia.com/mig-2g.24gb'
run_case regular-gpu false '{"items":[{"status":{"allocatable":{"nvidia.com/gpu":"1"}}}]}' 'nvidia.com/gpu'
run_case mixed-mig true '{"items":[{"status":{"allocatable":{"nvidia.com/mig-1g.24gb":"2","nvidia.com/mig-2g.24gb":"1"}}}]}'
run_case missing-mig true '{"items":[{"status":{"allocatable":{"nvidia.com/gpu":"1"}}}]}'


# EVA Vision post-renderer Python boolean regression
post_renderer_template="$REPO_ROOT/src/solution/roles/eva_vision/templates/post-renderer/post-renderer.py.j2"
vision_tasks="$REPO_ROOT/src/solution/roles/eva_vision/tasks/main.yaml"
render_check_root="$(mktemp -d "$HOME/tmp/eva-vision-boolean-test.XXXXXX")"

cleanup_vision_boolean_test() {
  rm -rf "$render_check_root"
}

if "$PYTHON_BIN" - \
  "$post_renderer_template" \
  "$vision_tasks" \
  "$render_check_root" <<'PY_BOOL_TEST'
from pathlib import Path
import py_compile
import re
import sys

import yaml


template_path = Path(sys.argv[1])
tasks_path = Path(sys.argv[2])
output_root = Path(sys.argv[3])

template_content = template_path.read_text()

expression = (
    "{{ eva_vision_skip_startup_warmup "
    "| bool | ternary('True', 'False') }}"
)

if template_content.count(expression) != 1:
    raise SystemExit(
        "[ERROR] expected exactly one Python boolean "
        "Jinja expression"
    )

for input_name, literal in (
    ("true", "True"),
    ("false", "False"),
):
    rendered_content = template_content.replace(
        expression,
        literal,
        1,
    )

    expected_line = (
        f"SKIP_STARTUP_WARMUP = {literal}"
    )

    if rendered_content.count(expected_line) != 1:
        raise SystemExit(
            "[ERROR] rendered post-renderer is missing "
            f"{expected_line!r}"
        )

    if re.search(
        r"^SKIP_STARTUP_WARMUP\s*=\s*(true|false)\s*$",
        rendered_content,
        re.MULTILINE,
    ):
        raise SystemExit(
            "[ERROR] rendered post-renderer contains "
            "an invalid lowercase Python boolean"
        )

    output_path = output_root / (
        f"post-renderer-{input_name}.py"
    )
    output_path.write_text(rendered_content)

    py_compile.compile(
        str(output_path),
        doraise=True,
    )

tasks = yaml.safe_load(
    tasks_path.read_text()
)

target_name = (
    "Render and verify final EVA Vision "
    "Helm manifest before deployment"
)

matching_tasks = [
    task
    for task in tasks
    if isinstance(task, dict)
    and task.get("name") == target_name
]

if len(matching_tasks) != 1:
    raise SystemExit(
        "[ERROR] expected exactly one Vision final "
        "manifest verification task"
    )

no_log_value = matching_tasks[0].get("no_log")

if no_log_value is True:
    raise SystemExit(
        "[ERROR] Vision final manifest task still uses "
        "fixed no_log: true"
    )

no_log_text = str(no_log_value)

for required_name in (
    "eva_cli_values_path",
    "eva_cli_helm_set",
):
    if required_name not in no_log_text:
        raise SystemExit(
            "[ERROR] Vision conditional no_log is missing "
            f"{required_name}"
        )
PY_BOOL_TEST
then
  echo '[OK] EVA Vision post-renderer Python boolean regression'
else
  cleanup_vision_boolean_test
  echo '[ERROR] EVA Vision post-renderer Python boolean regression' >&2
  false
fi

cleanup_vision_boolean_test


echo 'EVA Vision GPU resource contract tests passed.'
