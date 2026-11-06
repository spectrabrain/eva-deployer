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

run_case() {
  local name="$1"
  local mode="$2"
  local expected_status="$3"
  local expected_apps="$4"
  local root="$TMP_ROOT/$name"
  mkdir -p "$root/bin"
  : >"$root/kubeconfig"

  cat >"$root/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [[ "${KUBECTL_MODE:?}" == "unavailable" ]]; then
  exit 10
fi
if [[ "${KUBECTL_MODE:?}" == "error" ]]; then
  echo 'target API unavailable' >&2
  exit 1
fi
if [[ "${KUBECTL_MODE:?}" == "no-cluster" ]]; then
  echo 'The connection to the server 127.0.0.1:6443 was refused' >&2
  exit 1
fi
cat <<'OUTPUT'
label=lge-shee-magok-d-eva-app
tracking=lge-shee-magok-d-eva-agent:apps/Deployment:eva-agent/agent
label=unrelated-site-app
tracking=lge-shee-magok-d-eva-app:v1/Service:eva-app/app
OUTPUT
EOF
  chmod 0755 "$root/bin/kubectl"

  cat >"$root/playbook.yaml" <<EOF
---
- hosts: localhost
  gather_facts: false
  connection: local
  vars:
    eva_site_id: lge-shee-magok-d
    precondition_argocd_kubeconfig_override: $root/kubeconfig
  tasks:
    - ansible.builtin.include_tasks: $REPO_ROOT/src/infra/roles/precondition/tasks/argocd_tracking.yaml
    - ansible.builtin.assert:
        that:
          - precondition_argocd_tracking_status == '$expected_status'
          - precondition_argocd_applications == $expected_apps
EOF

  PATH="$root/bin:$PATH" KUBECTL_MODE="$mode" \
    "$ANSIBLE_PLAYBOOK" "$root/playbook.yaml" >"$root/ansible.log" 2>&1 || {
      cat "$root/ansible.log" >&2
      exit 1
    }
}

run_case tracked ok ok '["lge-shee-magok-d-eva-agent", "lge-shee-magok-d-eva-app"]'
run_case api-error error error '[]'
run_case kubectl-unavailable unavailable unavailable '[]'
run_case cluster-unavailable no-cluster unavailable '[]'
