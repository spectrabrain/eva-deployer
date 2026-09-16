#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ANSIBLE_PLAYBOOK="${ANSIBLE_PLAYBOOK:-ansible-playbook}"
TMP_ROOT="$(mktemp -d)"

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

echo 'EVA Vision GPU resource contract tests passed.'
