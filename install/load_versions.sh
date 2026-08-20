#!/usr/bin/env bash
set -euo pipefail

load_deploy_versions() {
  local script_dir repo_root versions_file
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  repo_root="$(cd "$script_dir/.." && pwd)"
  versions_file="${VERSIONS_FILE:-$repo_root/versions.json}"

  if [[ ! -f "$versions_file" ]]; then
    return 0
  fi

  if ! command -v python3 >/dev/null 2>&1; then
    echo "[warn] python3 not found. skip loading versions.json: $versions_file"
    return 0
  fi

  # Emit shell-safe assignments only for vars not already provided by env.
  eval "$(
    python3 - "$versions_file" <<'PY'
import json
import shlex
import sys

path = sys.argv[1]
with open(path, "r", encoding="utf-8") as f:
    data = json.load(f)

mapping = {
    "EVA_APP_DEPLOY_VERSION": ["eva_app_deploy_version"],
    "EVA_APP_CHART_VERSION": ["eva_app_chart_version"],
    "EVA_APP_ALEMBIC_VERSION": ["eva_app_alembic_version"],
    "EVA_VISION_DEPLOY_VERSION": ["eva_vision_deploy_version"],
    "EVA_VISION_CHART_VERSION": ["eva_vision_chart_version"],
    "EVA_AGENT_RELEASE": ["eva_agent_release", "eva_agent_deploy_version"],
    "EVA_AGENT_DEPLOY_VERSION": ["eva_agent_deploy_version"],
    "EVA_AGENT_CHART_VERSION": ["eva_agent_chart_version"],
    "EVA_AGENT_VLLM_CHART_VERSION": ["eva_agent_vllm_chart_version"],
    "EVA_AGENT_INIT_CHART_VERSION": ["eva_agent_init_chart_version"],
    "EVA_IAM_CHART_VERSION": ["eva_iam_chart_version"],
    "QDRANT_CHART_VERSION": ["qdrant_chart_version"],
    "KUSTOMIZE_VERSION": ["kustomize_version"],
    "K3S_DEFAULT_VERSION": ["k3s_default_version"],
    "N8N_IMAGE": ["n8n_image"],
}

for env_name, candidates in mapping.items():
    value = None
    for key in candidates:
        if key in data and data[key] is not None:
            value = str(data[key])
            break
    if value is None:
        continue
    quoted = shlex.quote(value)
    print(f'if [[ -z "${{{env_name}:-}}" ]]; then {env_name}={quoted}; fi')
PY
  )"

  export EVA_APP_DEPLOY_VERSION EVA_APP_CHART_VERSION EVA_APP_ALEMBIC_VERSION
  export EVA_VISION_DEPLOY_VERSION EVA_VISION_CHART_VERSION
  export EVA_AGENT_RELEASE EVA_AGENT_DEPLOY_VERSION EVA_AGENT_CHART_VERSION
  export EVA_AGENT_VLLM_CHART_VERSION EVA_AGENT_INIT_CHART_VERSION EVA_IAM_CHART_VERSION
  export QDRANT_CHART_VERSION KUSTOMIZE_VERSION K3S_DEFAULT_VERSION N8N_IMAGE
}
