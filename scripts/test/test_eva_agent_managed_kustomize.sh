#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." &&
    pwd
)"

TASK_FILE="$REPO_ROOT/src/solution/roles/eva_agent/tasks/dependencies.yaml"

if [[ ! -f "$TASK_FILE" ]]; then
  echo "[ERROR] EVA Agent dependency task file is missing"
  false
fi

if grep -Fq   '/usr/local/bin/kustomize'   "$TASK_FILE"
then
  echo "[ERROR] EVA Agent depends on system kustomize"
  false
fi

if grep -Fq   'install_kustomize.sh'   "$TASK_FILE"
then
  echo "[ERROR] EVA Agent downloads kustomize at apply time"
  false
fi

if ! grep -Fq   -- '- name: Validate EVA managed kustomize'   "$TASK_FILE"
then
  echo "[ERROR] managed kustomize validation task is missing"
  false
fi

if ! grep -Fq   'cmd: kustomize version'   "$TASK_FILE"
then
  echo "[ERROR] managed kustomize version check is missing"
  false
fi

if ! grep -Fq   -- '- eva_agent_managed_kustomize'   "$TASK_FILE"
then
  echo "[ERROR] managed kustomize validation tag is missing"
  false
fi

echo "EVA Agent managed kustomize contract test passed."
