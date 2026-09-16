#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ANSIBLE_PLAYBOOK="${ANSIBLE_PLAYBOOK:-ansible-playbook}"
TMP_ROOT="$(mktemp -d)"

cleanup() {
  rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

if ! command -v "$ANSIBLE_PLAYBOOK" >/dev/null 2>&1 && [[ ! -x "$ANSIBLE_PLAYBOOK" ]]; then
  echo "ansible-playbook was not found: $ANSIBLE_PLAYBOOK" >&2
  exit 1
fi

run_loader() {
  local component="$1"
  local scenario="$2"
  local root="$TMP_ROOT/$component-$scenario"
  local deploy_key deploy_value

  mkdir -p "$root/site-values"
  case "$component" in
    agent)
      deploy_key='vllm_profile'
      deploy_value='L40sx1'
      ;;
    vision)
      deploy_key='rollout_timeout'
      deploy_value='2400s'
      ;;
  esac

  if [[ "$scenario" == selected ]]; then
    cat >"$root/site-values/$component.yaml" <<EOF
other-target:
  marker: other
  secret: other-secret
site-dev-196:
  eva_${component}_deploy:
    $deploy_key: $deploy_value
  marker: selected
  nested:
    workspace: true
  secret: selected-secret
EOF
  elif [[ "$scenario" == other-target ]]; then
    cat >"$root/site-values/$component.yaml" <<EOF
other-target:
  eva_${component}_deploy:
    $deploy_key: $deploy_value
  marker: other
EOF
  fi

  cat >"$root/playbook.yaml" <<EOF
---
- hosts: localhost
  gather_facts: false
  connection: local
  vars:
    eva_site_values_root: $root/site-values
    ansible_host: site-dev-196
  tasks:
    - ansible.builtin.include_tasks: $REPO_ROOT/src/solution/roles/eva_${component}/tasks/workspace_values.yaml
    - ansible.builtin.assert:
        that:
          - eva_${component}_workspace_values is mapping
          - eva_${component}_workspace_chart_values is mapping
EOF

  if [[ "$scenario" == selected ]]; then
    cat >>"$root/playbook.yaml" <<EOF
          - eva_${component}_workspace_chart_values.marker == 'selected'
          - eva_${component}_${deploy_key} == '$deploy_value'
          - eva_${component}_workspace_chart_values.nested.workspace | bool
EOF
  else
    cat >>"$root/playbook.yaml" <<EOF
          - eva_${component}_workspace_values | length == 0
          - eva_${component}_workspace_chart_values | length == 0
EOF
  fi

  "$ANSIBLE_PLAYBOOK" "$root/playbook.yaml" >"$root/ansible.log" 2>&1
  if grep -Fq 'selected-secret' "$root/ansible.log" || grep -Fq 'other-secret' "$root/ansible.log"; then
    echo "$component Workspace secret leaked into Ansible log" >&2
    exit 1
  fi
}

for component in agent vision; do
  run_loader "$component" selected
  run_loader "$component" other-target
  run_loader "$component" absent
done

for component in agent vision; do
  role="$REPO_ROOT/src/solution/roles/eva_${component}/tasks"
  rg -Fq "eva_${component}_workspace_chart_values" "$role"
  rg -Fq 'effective-input-values.yaml' "$role"
  rg -Fq 'resolved-values.yaml' "$role"
  rg -Fq 'mode: "0600"' "$role"
  rg -Fq 'values-sources.yaml' "$role"
done

rg -Fq 'combine(eva_agent_workspace_chart_values, recursive=true)' \
  "$REPO_ROOT/src/solution/roles/eva_agent/tasks/agent.yaml"
rg -Fq 'combine(eva_agent_cli_values_override | default({}, true), recursive=true)' \
  "$REPO_ROOT/src/solution/roles/eva_agent/tasks/agent.yaml"
rg -Fq 'combine(eva_vision_workspace_chart_values, recursive=true)' \
  "$REPO_ROOT/src/solution/roles/eva_vision/tasks/main.yaml"
rg -Fq 'combine(eva_vision_cli_values_override | default({}, true), recursive=true)' \
  "$REPO_ROOT/src/solution/roles/eva_vision/tasks/main.yaml"

echo 'EVA Agent/Vision Workspace values contract tests passed.'
