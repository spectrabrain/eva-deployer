#!/usr/bin/env bash
set -euo pipefail

_eva_versions_strip_quotes() {
  local value="$1"

  if [[ ${#value} -ge 2 ]]; then
    if [[ ${value:0:1} == '"' && ${value: -1} == '"' ]]; then
      value="${value:1:${#value}-2}"
    elif [[ ${value:0:1} == "'" && ${value: -1} == "'" ]]; then
      value="${value:1:${#value}-2}"
    fi
  fi

  printf '%s' "$value"
}

_eva_versions_parse_catalog() {
  local file="$1"

  awk -v file="$file" '
    function trim(s) {
      sub(/^[ \t]+/, "", s)
      sub(/[ \t]+$/, "", s)
      return s
    }

    /^[ \t]*($|#)/ { next }

    /^[^ \t#][A-Za-z0-9_]*:[ \t]*/ {
      line = $0
      key = line
      sub(/:.*/, "", key)
      value = line
      sub(/^[^:]+:[ \t]*/, "", value)
      value = trim(value)
      sub(/[ \t]+#.*$/, "", value)
      value = trim(value)

      if (key == "schemaVersion" || key == "kind") {
        next
      }

      if (value == "" || value ~ /^[|>]/) {
        printf "[ERROR] unsupported non-scalar version catalog entry in %s: %s\n", file, line > "/dev/stderr"
        exit 1
      }

      printf "%s\t%s\n", key, value
      next
    }

    /^[ \t]+[^#]/ {
      printf "[ERROR] unsupported nested YAML content in %s: %s\n", file, $0 > "/dev/stderr"
      exit 1
    }

    {
      printf "[ERROR] unsupported YAML line in %s: %s\n", file, $0 > "/dev/stderr"
      exit 1
    }
  ' "$file"
}

_eva_versions_load_catalog_file() {
  local file="$1"
  local catalog_text line key value

  [[ -f "$file" ]] || return 0

  catalog_text="$(_eva_versions_parse_catalog "$file")" || return 1
  [[ -n "$catalog_text" ]] || return 0

  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    key="${line%%$'\t'*}"
    value="${line#*$'\t'}"
    value="$(_eva_versions_strip_quotes "$value")"

    if [[ -n "${EVA_VERSION_SOURCES[$key]+x}" ]]; then
      echo "[ERROR] duplicate version key '$key' found in ${EVA_VERSION_SOURCES[$key]} and $file" >&2
      return 1
    fi

    EVA_VERSION_VALUES[$key]="$value"
    EVA_VERSION_SOURCES[$key]="$file"
  done <<< "$catalog_text"
}

_eva_versions_catalog_value() {
  local key="$1"
  printf '%s' "${EVA_VERSION_VALUES[$key]:-}"
}

_eva_versions_catalog_source() {
  local key="$1"
  printf '%s' "${EVA_VERSION_SOURCES[$key]:-}"
}

load_deploy_versions() {
  local script_dir repo_root infra_version_file solution_version_file
  local -a export_names=(
    EVA_APP_DEPLOY_VERSION
    EVA_APP_CHART_VERSION
    EVA_APP_ALEMBIC_VERSION
    EVA_VISION_DEPLOY_VERSION
    EVA_VISION_CHART_VERSION
    EVA_AGENT_RELEASE
    EVA_AGENT_DEPLOY_VERSION
    EVA_AGENT_CHART_VERSION
    EVA_AGENT_VLLM_CHART_VERSION
    EVA_AGENT_INIT_CHART_VERSION
    EVA_IAM_CHART_VERSION
    QDRANT_CHART_VERSION
    KUSTOMIZE_VERSION
    K3S_DEFAULT_VERSION
    N8N_IMAGE
  )
  local env_name key selected_value selected_source
  local -A env_candidates=(
    [EVA_APP_DEPLOY_VERSION]="eva_app_deploy_version"
    [EVA_APP_CHART_VERSION]="eva_app_chart_version"
    [EVA_APP_ALEMBIC_VERSION]="eva_app_alembic_version"
    [EVA_VISION_DEPLOY_VERSION]="eva_vision_deploy_version"
    [EVA_VISION_CHART_VERSION]="eva_vision_chart_version"
    [EVA_AGENT_RELEASE]="eva_agent_release eva_agent_deploy_version"
    [EVA_AGENT_DEPLOY_VERSION]="eva_agent_deploy_version"
    [EVA_AGENT_CHART_VERSION]="eva_agent_chart_version"
    [EVA_AGENT_VLLM_CHART_VERSION]="eva_agent_vllm_chart_version"
    [EVA_AGENT_INIT_CHART_VERSION]="eva_agent_init_chart_version"
    [EVA_IAM_CHART_VERSION]="eva_iam_chart_version"
    [QDRANT_CHART_VERSION]="qdrant_chart_version"
    [KUSTOMIZE_VERSION]="kustomize_version"
    [K3S_DEFAULT_VERSION]="k3s_default_version"
    [N8N_IMAGE]="n8n_image"
  )

  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  repo_root="$(cd "$script_dir/../.." && pwd)"
  infra_version_file="${EVA_INFRA_VERSION_FILE:-$repo_root/src/infra/version.yaml}"
  solution_version_file="${EVA_SOLUTION_VERSION_FILE:-$repo_root/src/solution/version.yaml}"

  declare -gA EVA_VERSION_VALUES EVA_VERSION_SOURCES
  EVA_VERSION_VALUES=()
  EVA_VERSION_SOURCES=()

  _eva_versions_load_catalog_file "$infra_version_file"
  _eva_versions_load_catalog_file "$solution_version_file"

  for env_name in "${export_names[@]}"; do
    selected_value=""
    selected_source=""

    for key in ${env_candidates[$env_name]}; do
      if [[ -n "${EVA_VERSION_VALUES[$key]+x}" ]]; then
        selected_value="$(_eva_versions_catalog_value "$key")"
        selected_source="$(_eva_versions_catalog_source "$key")"
        break
      fi
    done

    if [[ -n "$selected_source" && ! ${!env_name+x} ]]; then
      printf -v "$env_name" '%s' "$selected_value"
    fi
    export "$env_name"
  done

  if [[ -z "${VERSIONS_QUIET:-}" ]]; then
    echo "[versions] infra catalog: $infra_version_file"
    echo "[versions] solution catalog: $solution_version_file"
    for env_name in "${export_names[@]}"; do
      selected_value=""
      selected_source=""
      for key in ${env_candidates[$env_name]}; do
        if [[ -n "${EVA_VERSION_VALUES[$key]+x}" ]]; then
          selected_value="$(_eva_versions_catalog_value "$key")"
          selected_source="$(_eva_versions_catalog_source "$key")"
          break
        fi
      done

      [[ -n "$selected_source" ]] || continue

      if [[ ${!env_name+x} && "${!env_name}" != "$selected_value" ]]; then
        printf '[versions]   %-26s %-16s <- env (catalog %s: %s)\n' \
          "$env_name" "${!env_name:-(없음)}" "$selected_source" "$selected_value"
      else
        printf '[versions]   %-26s %s\n' "$env_name" "${!env_name:-(없음)}"
      fi
    done
  fi
}
