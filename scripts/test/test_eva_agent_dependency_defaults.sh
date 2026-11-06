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

cat >"$TMP_ROOT/playbook.yaml" <<EOF
---
- hosts: localhost
  gather_facts: false
  connection: local
  vars:
    eva_cache_root: $TMP_ROOT/cache
    eva_site_deploy_root: $TMP_ROOT/deploy
    eva_agent_deploy_version: '3.2.0'
    qdrant_chart_version: '1.17.1'
    eva_agent_vllm_chart_version: '0.0.1'
    eva_agent_vllm_profile: L40sx1
    ansible_facts:
      env:
        HOME: $TMP_ROOT/home
  tasks:
    # Load inputs below extra-var precedence, so set_fact must preserve
    # explicit snapshot/values overrides rather than being shadowed by -e.
    - name: Load dependency test case inputs
      ansible.builtin.include_vars: "{{ defaults_case_vars }}"
      tags: always

    - name: Load EVA Agent dependency defaults
      ansible.builtin.include_tasks: $REPO_ROOT/src/solution/roles/eva_agent/tasks/dependencies.yaml
      tags: always

    - name: Assert EVA Agent dependency paths and Qdrant snapshot defaults
      ansible.builtin.assert:
        that:
          - eva_agent_cache_root == '$TMP_ROOT/cache'
          - eva_agent_offline_dir == '$TMP_ROOT/cache/eva-agent'
          - eva_agent_offline_release_dir == '$TMP_ROOT/cache/eva-agent/release/3.2.0'
          - eva_agent_offline_agent_model_dir == '$TMP_ROOT/cache/models/agent/hf'
          - eva_agent_offline_vllm_model_dir == '$TMP_ROOT/cache/models/vllm/hf'
          - eva_agent_qdrant_values_file == expected_values
          - eva_agent_qdrant_snapshot_source == expected_source
          - eva_agent_qdrant_snapshot_source is string
          - repository_mode == expected_mode
          - airgap_mode == (repository_mode not in ['online_repository', 'cloud_repository'])
          - (eva_agent_sync_aws_credentials | int) == ((repository_mode in ['online_repository', 'cloud_repository']) | ternary(1, 0))
      tags:
        - eva_agent_dependency_defaults
EOF

# Each invocation gets a fresh host's set_fact state. Exercise the default
# used by eva install, not only the explicitly supplied Harbor source used
# by preparation and older direct-Ansible examples.
write_case() {
  local mode="$1" values="$2" source="$3" expected_values="$4" expected_source="$5"
  local expected_mode="$mode"
  if [[ "$mode" == default ]]; then
    expected_mode=online_repository
  fi
  cat >"$TMP_ROOT/case.yaml" <<EOF
expected_mode: $expected_mode
expected_values: $expected_values
expected_source: $expected_source
EOF
  if [[ "$mode" != default ]]; then
    printf 'repository_mode: %s\n' "$mode" >>"$TMP_ROOT/case.yaml"
  fi
  if [[ "$mode" == remote_repository || "$mode" == local_repository ]]; then
    printf 'repository_registry: 10.159.57.172:32080\n' >>"$TMP_ROOT/case.yaml"
  fi
  if [[ "$values" != default ]]; then
    printf 'eva_agent_qdrant_values_file: %s\n' "$values" >>"$TMP_ROOT/case.yaml"
  fi
  if [[ "$source" != default ]]; then
    printf 'eva_agent_qdrant_snapshot_source: %s\n' "$source" >>"$TMP_ROOT/case.yaml"
  fi
}

run_case() {
  "$ANSIBLE_PLAYBOOK" \
    --tags eva_agent_dependency_defaults \
    -e "defaults_case_vars=$TMP_ROOT/case.yaml" \
    "$TMP_ROOT/playbook.yaml" >"$TMP_ROOT/ansible.log" 2>&1
}

for case in \
  'default default default values-k3s.s3.yaml local_pv' \
  'online_repository default default values-k3s.s3.yaml local_pv' \
  'cloud_repository default default values-k3s.s3.yaml local_pv' \
  'remote_repository default default values-k3s.harbor.yaml harbor' \
  'local_repository default default values-k3s.harbor.yaml harbor' \
  'online_repository values-k3s.harbor.yaml default values-k3s.harbor.yaml harbor' \
  'cloud_repository values-k3s.harbor.yaml default values-k3s.harbor.yaml harbor' \
  'remote_repository values-k3s.s3.yaml default values-k3s.s3.yaml local_pv' \
  'remote_repository default harbor values-k3s.harbor.yaml harbor' \
  'online_repository default local_pv values-k3s.s3.yaml local_pv' \
  'cloud_repository default local_pv values-k3s.s3.yaml local_pv'; do
  read -r mode values source expected_values expected_source <<<"$case"
  write_case "$mode" "$values" "$source" "$expected_values" "$expected_source"
  if ! run_case; then
    cat "$TMP_ROOT/ansible.log" >&2
    echo "[FAIL] EVA Agent dependency defaults differ for: $case" >&2
    exit 1
  fi
done

# Invalid or mismatched inputs must still fail closed; fixing defaults must
# not relax the source contract or silently override an operator's input.
for case in \
  'remote_repository default local_pv' \
  'online_repository default harbor' \
  'cloud_repository default harbor' \
  'remote_repository default oci'; do
  read -r mode values source <<<"$case"
  write_case "$mode" "$values" "$source" unused unused
  if run_case; then
    echo "[FAIL] EVA Agent accepted invalid Qdrant snapshot inputs: $case" >&2
    exit 1
  fi
  if ! grep -Fq 'Harbor snapshots require values-k3s.harbor.yaml with snapshot source harbor.' "$TMP_ROOT/ansible.log"; then
    cat "$TMP_ROOT/ansible.log" >&2
    echo "[FAIL] EVA Agent failed before snapshot validation: $case" >&2
    exit 1
  fi
done

echo 'EVA Agent dependency defaults contract tests passed (11 valid, 4 invalid cases).'
