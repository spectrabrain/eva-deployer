#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." &&
    pwd
)"
ANSIBLE_PLAYBOOK="${ANSIBLE_PLAYBOOK:-ansible-playbook}"
TMP_ROOT="$(mktemp -d)"

cleanup() {
  rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

if ! command -v "$ANSIBLE_PLAYBOOK" >/dev/null 2>&1 &&
   [[ ! -x "$ANSIBLE_PLAYBOOK" ]]
then
  echo "[ERROR] ansible-playbook was not found: $ANSIBLE_PLAYBOOK"
  false
fi

mkdir -p "$TMP_ROOT/bin"

cat >"$TMP_ROOT/bin/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail

resource="${3:-}"

if [[ "${1:-}" == "get" ]] &&
   [[ "${2:-}" == "storageclass" ]] &&
   [[ "${4:-}" == "-o" ]] &&
   [[ "${5:-}" == "json" ]]
then
  if [[ "$resource" == "eva-agent-sc-bs" ]]; then
    cat <<'JSON'
{
  "metadata": {
    "annotations": {
      "meta.helm.sh/release-name": "eva-agent-init",
      "meta.helm.sh/release-namespace": "eva-agent"
    },
    "labels": {
      "app.kubernetes.io/managed-by": "Helm",
      "app.kubernetes.io/instance": "eva-agent-init",
      "app.kubernetes.io/name": "eva-agent-init",
      "helm.sh/chart": "eva-agent-init-1.0.0"
    }
  },
  "parameters": {
    "fsType": "ext4",
    "type": "local"
  },
  "provisioner": "rancher.io/local-path",
  "reclaimPolicy": "Retain",
  "volumeBindingMode": "WaitForFirstConsumer"
}
JSON
    exit 0
  fi

  if [[ "$resource" == "eva-agent-sc-fs" ]]; then
    cat <<'JSON'
{
  "metadata": {
    "annotations": {
      "argocd.argoproj.io/tracking-id": "legacy-site-eva-agent-init:storage.k8s.io/StorageClass:eva-agent-init/eva-agent-sc-fs"
    },
    "labels": {
      "app.kubernetes.io/managed-by": "Helm",
      "app.kubernetes.io/instance": "eva-agent-init",
      "app.kubernetes.io/name": "eva-agent-init",
      "helm.sh/chart": "eva-agent-init-1.0.0"
    }
  },
  "parameters": {
    "server": "localhost",
    "share": "/share/eva-agent"
  },
  "provisioner": "nfs.csi.k8s.io",
  "reclaimPolicy": "Retain",
  "volumeBindingMode": "WaitForFirstConsumer"
}
JSON
    exit 0
  fi
fi

if [[ "${1:-}" == "patch" ]] &&
   [[ "${2:-}" == "storageclass" ]]
then
  printf '%s\n' "$*" >>"${KUBECTL_PATCH_LOG:?}"
  printf 'storageclass.storage.k8s.io/%s patched\n' "$resource"
  exit 0
fi

printf '[ERROR] unexpected kubectl command: %s\n' "$*" >&2
exit 1
SH

chmod 0755 "$TMP_ROOT/bin/kubectl"

cat >"$TMP_ROOT/playbook.yaml" <<EOF
---
- hosts: localhost
  gather_facts: false
  connection: local
  vars:
    eva_agent_namespace: eva-agent
    eva_agent_kubeconfig: $TMP_ROOT/kubeconfig
  tasks:
    - name: Load EVA Agent ownership migration
      ansible.builtin.include_tasks: $REPO_ROOT/src/solution/roles/eva_agent/tasks/dependencies.yaml
      tags:
        - always
EOF

PATH="$TMP_ROOT/bin:$PATH" \
KUBECTL_PATCH_LOG="$TMP_ROOT/patch.log" \
"$ANSIBLE_PLAYBOOK" \
  --tags eva_agent_init_storageclass_migration \
  "$TMP_ROOT/playbook.yaml"

if [[ ! -f "$TMP_ROOT/patch.log" ]]; then
  echo "[ERROR] StorageClass ownership patch was not executed"
  false
fi

if grep -Fq \
  'patch storageclass eva-agent-sc-bs' \
  "$TMP_ROOT/patch.log"
then
  echo "[ERROR] already-owned block StorageClass was patched"
  false
fi

if [[ "$(
  grep -Fc \
    'patch storageclass eva-agent-sc-fs' \
    "$TMP_ROOT/patch.log"
)" -ne 1 ]]
then
  echo "[ERROR] legacy NFS StorageClass was not patched exactly once"
  false
fi

for expected in \
  'meta.helm.sh/release-name' \
  'eva-agent-init' \
  'meta.helm.sh/release-namespace' \
  'eva-agent' \
  'argocd.argoproj.io/tracking-id' \
  'null'
do
  if ! grep -Fq "$expected" "$TMP_ROOT/patch.log"; then
    echo "[ERROR] ownership patch is missing: $expected"
    false
  fi
done

echo 'EVA Agent init StorageClass mixed-state migration contract test passed.'
