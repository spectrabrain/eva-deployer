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

iam_tasks="$REPO_ROOT/src/solution/roles/eva_iam/tasks/main.yaml"
app_tasks="$REPO_ROOT/src/solution/roles/eva_app/tasks/main.yaml"
tls_tasks="$REPO_ROOT/src/solution/roles/eva_traefik_tls/tasks/main.yaml"
iam_include_line="$(grep -n -m1 'name: eva_traefik_tls' "$iam_tasks" | cut -d: -f1)"
iam_install_line="$(grep -n -m1 'Install/upgrade eva-iam from Helm repo or offline chart' "$iam_tasks" | cut -d: -f1)"
app_include_line="$(grep -n -m1 'name: eva_traefik_tls' "$app_tasks" | cut -d: -f1)"
app_install_line="$(grep -n -m1 'Install EVA App from online Helm repo' "$app_tasks" | cut -d: -f1)"
[[ -n "$iam_include_line" && -n "$iam_install_line" && "$iam_include_line" -lt "$iam_install_line" ]]
[[ -n "$app_include_line" && -n "$app_install_line" && "$app_include_line" -lt "$app_install_line" ]]
grep -Fq 'eva_app_effective_values.app.backendHost' "$app_tasks"
grep -Fq 'eva-tls-for-traefik' "$tls_tasks"
grep -Fq 'kind: TLSStore' "$tls_tasks"
grep -Fq 'namespace: kube-system' "$tls_tasks"
grep -Fq -- '--cert={{ eva_traefik_tls_host_path }}/tls.crt' "$tls_tasks"
grep -Fq -- '--key={{ eva_traefik_tls_host_path }}/tls.key' "$tls_tasks"

run_fixture() {
  local name="$1"
  local host="$2"
  local root="$TMP_ROOT/$name"

  mkdir -p "$root/bin" "$root/certs"
  cat >"$root/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${FAKE_KUBECTL_LOG:?}"
cat >/dev/null
printf 'configured\n'
EOF
  chmod 0755 "$root/bin/kubectl"
  : >"$root/kubeconfig"
  if [[ "$host" == 192.0.2.10 ]]; then
    printf 'certificate\n' >"$root/certs/tls.crt"
    printf 'private-key\n' >"$root/certs/tls.key"
  else
    rm -rf "$root/certs"
  fi

  cat >"$root/playbook.yaml" <<EOF
---
- hosts: localhost
  gather_facts: false
  connection: local
  vars:
    eva_traefik_tls_host: $host
    eva_traefik_tls_host_path: $root/certs
    eva_traefik_tls_kubeconfig: $root/kubeconfig
  tasks:
    - ansible.builtin.import_tasks: $tls_tasks
EOF

  PATH="$root/bin:$PATH" FAKE_KUBECTL_LOG="$root/kubectl.log" \
    "$ANSIBLE_PLAYBOOK" "$root/playbook.yaml" >"$root/ansible.log" 2>&1

  if [[ "$host" == 192.0.2.10 ]]; then
    grep -Fq "create secret tls eva-tls-for-traefik --namespace=kube-system --cert=$root/certs/tls.crt --key=$root/certs/tls.key --dry-run=client --output=yaml" "$root/kubectl.log"
    [[ "$(grep -Fc 'apply --filename=-' "$root/kubectl.log")" == 2 ]]
  elif [[ -e "$root/kubectl.log" ]]; then
    echo 'DNS IAM host unexpectedly configured Traefik TLS resources' >&2
    exit 1
  fi
}

run_fixture ip-host 192.0.2.10
run_fixture dns-host iam.example.test

missing_root="$TMP_ROOT/missing-certificate"
mkdir -p "$missing_root/bin" "$missing_root/certs"
cp "$TMP_ROOT/ip-host/bin/kubectl" "$missing_root/bin/kubectl"
chmod 0755 "$missing_root/bin/kubectl"
: >"$missing_root/kubeconfig"
cat >"$missing_root/playbook.yaml" <<EOF
---
- hosts: localhost
  gather_facts: false
  connection: local
  vars:
    eva_traefik_tls_host: 192.0.2.10
    eva_traefik_tls_host_path: $missing_root/certs
    eva_traefik_tls_kubeconfig: $missing_root/kubeconfig
  tasks:
    - ansible.builtin.import_tasks: $tls_tasks
EOF
if PATH="$missing_root/bin:$PATH" FAKE_KUBECTL_LOG="$missing_root/kubectl.log" \
  "$ANSIBLE_PLAYBOOK" "$missing_root/playbook.yaml" >"$missing_root/ansible.log" 2>&1; then
  echo 'IP IAM host accepted missing TLS certificate files' >&2
  exit 1
fi
grep -Fq 'IP 기반 ingress host에는' "$missing_root/ansible.log"
[[ ! -e "$missing_root/kubectl.log" ]]

echo 'EVA Traefik TLS contract tests passed.'
