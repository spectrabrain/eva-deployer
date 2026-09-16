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
  local name="$1" models="$2" gpu_count="$3" mig_enabled="$4" mig_count="$5" resources="$6" expected="${7:-}"
  local root="$TMP_ROOT/$name"
  mkdir -p "$root/bin"

  cat >"$root/bin/nvidia-smi" <<EOF
#!/usr/bin/env bash
printf '%s\\n' '$models'
EOF
  cat >"$root/bin/kubectl" <<EOF
#!/usr/bin/env bash
printf '%s\\n' '$resources'
EOF
  chmod 0755 "$root/bin/nvidia-smi" "$root/bin/kubectl"

  cat >"$root/playbook.yaml" <<EOF
---
- hosts: localhost
  gather_facts: false
  connection: local
  vars:
    eva_infra_root: $REPO_ROOT/src/infra
    eva_enabled_components: agent
    config_detected_gpu_count: $gpu_count
    config_mig_enabled: $mig_enabled
    config_mig_instance_count: $mig_count
  tasks:
    - ansible.builtin.include_tasks: $REPO_ROOT/src/solution/roles/config/tasks/vllm_profile.yaml
EOF

  if [[ -n "$expected" ]]; then
    cat >>"$root/playbook.yaml" <<EOF
    - ansible.builtin.assert:
        that:
          - config_eva_vllm_profile == '$expected'
EOF
    PATH="$root/bin:$PATH" "$ANSIBLE_PLAYBOOK" "$root/playbook.yaml" >"$root/ansible.log" 2>&1
  else
    if PATH="$root/bin:$PATH" "$ANSIBLE_PLAYBOOK" "$root/playbook.yaml" >"$root/ansible.log" 2>&1; then
      echo "$name unexpectedly resolved a vLLM profile" >&2
      exit 1
    fi
    grep -Fq 'vLLM profile cannot be determined' "$root/ansible.log"
  fi
}

run_case a6000 'NVIDIA RTX A6000' 1 false 0 '{"items":[{"status":{"allocatable":{"nvidia.com/gpu":"1"}}}]}' A6000x1
run_case l40s 'NVIDIA L40S' 1 false 0 '{"items":[{"status":{"allocatable":{"nvidia.com/gpu":"1"}}}]}' L40sx1
run_case pro5000 'NVIDIA RTX PRO 5000 Blackwell' 3 false 0 '{"items":[{"status":{"allocatable":{"nvidia.com/gpu":"3"}}}]}' PRO5000x3
run_case pro6000-mig 'NVIDIA RTX PRO 6000 Blackwell Server Edition' 1 true 4 '{"items":[{"status":{"allocatable":{"nvidia.com/gpu":"1","nvidia.com/mig-1g.24gb":"4"}}}]}' PRO6000-MIGx4
run_case unsupported 'NVIDIA H100' 1 false 0 '{"items":[{"status":{"allocatable":{"nvidia.com/gpu":"1"}}}]}'
run_case wrong-count 'NVIDIA RTX A6000' 2 false 0 '{"items":[{"status":{"allocatable":{"nvidia.com/gpu":"2"}}}]}'
run_case multiple-mig 'NVIDIA RTX PRO 6000 Blackwell Server Edition' 1 true 4 '{"items":[{"status":{"allocatable":{"nvidia.com/mig-1g.24gb":"2","nvidia.com/mig-2g.24gb":"2"}}}]}'

if grep -Fq "default('PRO6000-MIGx4')" "$REPO_ROOT/src/solution/roles/eva_agent/tasks/dependencies.yaml"; then
  echo 'PRO6000-MIGx4 fallback must not remain in the Agent role' >&2
  exit 1
fi

grep -Fq 'vllm_profile: {{ config_eva_vllm_profile | to_json }}' \
  "$REPO_ROOT/src/solution/roles/config/templates/eva.yaml.j2"
grep -Fq 'eva_agent_vllm_profile: "{{ eva_agent_generated_config.vllm_profile | mandatory }}"' \
  "$REPO_ROOT/src/solution/playbooks/site_eva_agent.yaml"
grep -Fq 'eva_agent_vllm_profile: "{{ eva_generated_config.vllm_profile | mandatory }}"' \
  "$REPO_ROOT/src/solution/playbooks/site_eva.yaml"
grep -Fq 'Verify selected Agent Release values files exist' \
  "$REPO_ROOT/src/solution/roles/eva_agent/tasks/dependencies.yaml"

echo 'EVA vLLM profile resolver contract tests passed.'
