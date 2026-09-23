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
  tasks:
    - name: Load EVA Agent dependency defaults
      ansible.builtin.include_tasks: $REPO_ROOT/src/solution/roles/eva_agent/tasks/dependencies.yaml
      tags: always

    - name: Assert EVA Agent default-only dependency paths
      ansible.builtin.assert:
        that:
          - eva_agent_cache_root == '$TMP_ROOT/cache'
          - eva_agent_offline_dir == '$TMP_ROOT/cache/eva-agent'
      tags:
        - eva_agent_dependency_defaults
EOF

"$ANSIBLE_PLAYBOOK" \
  --tags eva_agent_dependency_defaults \
  "$TMP_ROOT/playbook.yaml"

echo 'EVA Agent default-only dependency defaults contract test passed.'
