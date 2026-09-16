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

run_agent_loader() {
  local scenario="$1"
  local root="$TMP_ROOT/agent-$scenario"
  mkdir -p "$root/site-values"

  if [[ "$scenario" == selected ]]; then
    cat >"$root/site-values/agent.yaml" <<'EOF'
other-target:
  marker: other
site-dev-196:
  image:
    pullPolicy: Always
  nested:
    workspace: true
  secret: selected-secret
EOF
  elif [[ "$scenario" == other-target ]]; then
    cat >"$root/site-values/agent.yaml" <<'EOF'
other-target:
  marker: other
EOF
  elif [[ "$scenario" == unkeyed ]]; then
    cat >"$root/site-values/agent.yaml" <<'EOF'
image:
  pullPolicy: Never
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
    - ansible.builtin.include_tasks: $REPO_ROOT/src/solution/roles/eva_agent/tasks/workspace_values.yaml
    - ansible.builtin.assert:
        that:
          - eva_agent_workspace_values is mapping
          - eva_agent_workspace_chart_values is mapping
EOF

  if [[ "$scenario" == selected ]]; then
    cat >>"$root/playbook.yaml" <<'EOF'
          - eva_agent_workspace_chart_values.image.pullPolicy == 'Always'
          - eva_agent_workspace_chart_values.nested.workspace | bool
EOF
  else
    cat >>"$root/playbook.yaml" <<'EOF'
          - eva_agent_workspace_values | length == 0
          - eva_agent_workspace_chart_values | length == 0
EOF
  fi

  "$ANSIBLE_PLAYBOOK" "$root/playbook.yaml" >"$root/ansible.log" 2>&1
  if grep -Fq 'selected-secret' "$root/ansible.log"; then
    echo 'Agent Workspace secret leaked into Ansible log' >&2
    exit 1
  fi
}

for scenario in selected other-target unkeyed absent; do
  run_agent_loader "$scenario"
done

run_vision_loader() {
  local scenario="$1"
  local root="$TMP_ROOT/vision-$scenario"
  mkdir -p "$root/site-values"

  if [[ "$scenario" == selected ]]; then
    cat >"$root/site-values/vision.yaml" <<'EOF'
other-target:
  image:
    pullPolicy: Never
site-dev-196:
  image:
    pullPolicy: Always
EOF
  elif [[ "$scenario" == other-target ]]; then
    cat >"$root/site-values/vision.yaml" <<'EOF'
other-target:
  image:
    pullPolicy: Never
EOF
  elif [[ "$scenario" == unkeyed ]]; then
    cat >"$root/site-values/vision.yaml" <<'EOF'
image:
  pullPolicy: Never
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
    - ansible.builtin.include_tasks: $REPO_ROOT/src/solution/roles/eva_vision/tasks/workspace_values.yaml
    - ansible.builtin.assert:
        that:
EOF

  if [[ "$scenario" == selected ]]; then
    cat >>"$root/playbook.yaml" <<'EOF'
          - eva_vision_workspace_values.image.pullPolicy == 'Always'
          - eva_vision_workspace_chart_values.image.pullPolicy == 'Always'
EOF
  else
    cat >>"$root/playbook.yaml" <<'EOF'
          - eva_vision_workspace_values | length == 0
          - eva_vision_workspace_chart_values | length == 0
EOF
  fi

  "$ANSIBLE_PLAYBOOK" "$root/playbook.yaml" >"$root/ansible.log" 2>&1
}

for scenario in selected other-target unkeyed absent; do
  run_vision_loader "$scenario"
done

for component in agent vision; do
  role="$REPO_ROOT/src/solution/roles/eva_${component}/tasks"
  grep -RFq "eva_${component}_workspace_chart_values" "$role"
  grep -RFq 'effective-input-values.yaml' "$role"
  grep -RFq 'resolved-values.yaml' "$role"
  grep -RFq 'mode: "0600"' "$role"
  grep -RFq 'values-sources.yaml' "$role"
done

agent_role="$REPO_ROOT/src/solution/roles/eva_agent/tasks"
grep -Fq 'combine(eva_agent_workspace_chart_values, recursive=true)' "$agent_role/agent.yaml"
grep -Fq 'combine(eva_agent_cli_values_override | default({}, true), recursive=true)' "$agent_role/agent.yaml"
if grep -Fq 'agent-vllm.yaml' "$agent_role/workspace_values.yaml" || grep -Fq 'agent-qdrant.yaml' "$agent_role/workspace_values.yaml"; then
  echo 'vLLM and Qdrant must not be Workspace inputs' >&2
  exit 1
fi
if grep -Fq 'eva_vision_deploy' "$REPO_ROOT/src/solution/roles/eva_vision/tasks/workspace_values.yaml"; then
  echo 'Vision Workspace values must remain chart overrides only' >&2
  exit 1
fi

echo 'EVA Agent/Vision optional values contract tests passed.'
