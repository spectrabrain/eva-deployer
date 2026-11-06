#!/usr/bin/env bash
set -euo pipefail

# The shared playbook preflight pins every derived path to the Release root.
# eva install on a Remote Target supplies the payload it materialized from the
# published Release as eva_cache_root; that managed path, and only that one,
# must pass alongside the Release's own out/cache.

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

release_root="$TMP_ROOT/release"
mkdir -p "$release_root/src/infra/playbooks" "$TMP_ROOT/workspace/site-values"
cp "$REPO_ROOT/src/playbook-vars.yaml" "$REPO_ROOT/src/playbook-preflight.yaml" "$release_root/src/"
cp "$REPO_ROOT/src/infra/version.yaml" "$release_root/src/infra/version.yaml"
cat >"$release_root/src/infra/playbooks/contract.yaml" <<'EOF'
---
- hosts: localhost
  gather_facts: false
  connection: local
  vars_files:
    - ../../playbook-vars.yaml
  tasks:
    - name: Run the shared playbook preflight
      ansible.builtin.import_tasks: ../../playbook-preflight.yaml
EOF

run_contract() {
  local cache_root="$1"
  local -a extra=()
  [[ -n "$cache_root" ]] && extra=(-e "eva_cache_root=$cache_root")
  EVA_REPO_ROOT="$release_root" EVA_SITE_ID=site-mg-d EVA_WORKSPACE_ROOT="$TMP_ROOT/workspace" \
    "$ANSIBLE_PLAYBOOK" "${extra[@]}" "$release_root/src/infra/playbooks/contract.yaml" >"$TMP_ROOT/run.log" 2>&1
}

run_contract "" || { cat "$TMP_ROOT/run.log" >&2; echo "[FAIL] default Release cache was rejected" >&2; exit 1; }
run_contract "/var/lib/eva/artifacts/remote/0.1.0-ci.107/0123456789abcdef0123456789abcdef/cache" || {
  cat "$TMP_ROOT/run.log" >&2
  echo "[FAIL] materialized Remote Target payload cache was rejected" >&2
  exit 1
}

for rejected in \
  /tmp/cache \
  /var/lib/eva/artifacts/remote/0.1.0-ci.107/0123456789abcdef0123456789abcdef \
  /var/lib/eva/artifacts/remote/0.1.0-ci.107/not-an-identity/cache \
  /var/lib/eva/artifacts/remote/../../../etc/0123456789abcdef0123456789abcdef/cache \
  relative/out/cache; do
  if run_contract "$rejected"; then
    echo "[FAIL] path contract accepted cache root: $rejected" >&2
    exit 1
  fi
  grep -Fq "eva_cache_root=$rejected" "$TMP_ROOT/run.log" || {
    cat "$TMP_ROOT/run.log" >&2
    echo "[FAIL] path contract failure does not name the rejected cache root" >&2
    exit 1
  }
done

echo 'EVA playbook path contract tests passed.'
