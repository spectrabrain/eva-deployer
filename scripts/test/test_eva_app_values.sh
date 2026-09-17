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

make_fixture() {
  local name="$1"
  local root="$TMP_ROOT/$name"

  mkdir -p "$root/bin" "$root/workspace/site-values" "$root/rendered" \
    "$root/app-work" "$root/certs" "$root/config/site/localhost" "$root/state/site/localhost"
  printf 'certificate\n' >"$root/certs/tls.crt"
  printf 'private-key\n' >"$root/certs/tls.key"

  cat >"$root/bin/helm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${FAKE_HELM_LOG:?}"
case "$1 $2" in
  "repo add"|"repo update") exit 0 ;;
  "show values") printf 'app:\n  sso:\n    baseUrl: https://chart.example\n    adminClientSecret: chart-default-secret\n' ;;
  "status eva-app") exit 1 ;;
  "template eva-app") exit 0 ;;
  "upgrade --install") echo 'has been installed' ;;
  "get values") printf 'app:\n  installed: true\n' ;;
  *) exit 0 ;;
esac
EOF
  cat >"$root/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${FAKE_KUBECTL_LOG:?}"
case "$1 $2" in
  "get deployment")
    if [[ "${FAKE_DEPLOYMENT_EXISTS:-false}" == true ]]; then
      case " $* " in
        *' -o jsonpath={.metadata.uid} '*) printf 'test-deployment-uid' ;;
        *' -o json '*) printf '%s\n' '{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"eva-app","namespace":"eva-app","uid":"test-deployment-uid"},"spec":{"replicas":1}}' ;;
      esac
      exit 0
    fi
    exit 1
    ;;
  "create --dry-run=client") exit 0 ;;
  "delete job")
    [[ "${FAKE_TLS_DELETE_FAIL:-false}" == true ]] && exit 1
    exit 0
    ;;
  "wait --for=condition=Complete")
    [[ "${FAKE_TLS_WAIT_FAIL:-false}" == true ]] && exit 1
    echo 'condition met'
    ;;
  "get all") echo resources ;;
  "get pods") echo eva-app-pod ;;
  "rollout status") echo 'rollout complete' ;;
  "patch deployment") echo patched ;;
  *) exit 0 ;;
esac
EOF
  chmod 0755 "$root/bin/helm" "$root/bin/kubectl"

  printf '%s\n' '[local]' 'localhost ansible_connection=local ansible_become=false' >"$root/inventory.ini"
  cat >"$root/playbook.yaml" <<EOF
---
- hosts: all
  gather_facts: false
  vars:
    ansible_facts:
      env:
        HOME: /tmp
    ansible_default_ipv4:
      address: 127.0.0.1
    eva_site_config_root: $root/config/site
    eva_site_state_root: $root/state/site
    eva_site_deploy_root: $root/rendered-root
    eva_site_values_root: $root/workspace/site-values
    eva_site_id: site
    eva_template_values_root: $REPO_ROOT/src/solution/values
    eva_cache_root: $root/cache
    eva_app_workdir: $root/app-work
    eva_app_deploy_values_dir: $root/rendered
    eva_app_chart_version: 3.1.8
    eva_app_deploy_version: 3.1.3-rc.3
    eva_app_kubeconfig: $root/kubeconfig
    eva_app_tls_host_path: $root/certs
    repository_mode: cloud_repository
  tasks:
    - ansible.builtin.import_role:
        name: eva_app
EOF

  printf '%s\n' "$root"
}

run_app() {
  local root="$1"
  local components="$2"
  local set_value="$3"

  PATH="$root/bin:$PATH" \
    FAKE_HELM_LOG="$root/helm.log" \
    FAKE_KUBECTL_LOG="$root/kubectl.log" \
    ANSIBLE_ROLES_PATH="$REPO_ROOT/src/solution/roles" \
    "$ANSIBLE_PLAYBOOK" -i "$root/inventory.ini" "$root/playbook.yaml" \
      -e "eva_enabled_components=$components" \
      -e "eva_cli_values_path=$root/cli.yaml" \
      -e "{\"eva_cli_helm_set\":[\"$set_value\"]}"
}

root="$(make_fixture generated)"
cat >"$root/workspace/site-values/app.yaml" <<'EOF'
localhost:
  app:
    browserTitleName: Workspace title
    sso:
      baseUrl: https://workspace.example
    license:
      api_key: workspace-key
  ingress:
    tls:
      create: true
      authType: hostPath
EOF
cat >"$root/config/site/localhost/eva.yaml" <<'EOF'
app:
  pipeline:
    detector:
      workerNum: 11
    perceptor:
      workerNum: 7
EOF
cat >"$root/state/site/localhost/eva-iam.yaml" <<'EOF'
schema_version: v1
site_id: site
target: localhost
iam_public_host: iam.example
generated_at: "2026-09-15T00:00:00Z"
sso:
  baseUrl: https://iam.example
  adminClientSecret: generated-secret
EOF
cat >"$root/cli.yaml" <<'EOF'
app:
  browserTitleName: CLI title
EOF
workspace_checksum_before="$(sha256sum "$root/workspace/site-values/app.yaml")"
run_app "$root" 'app,iam' 'app.browserTitleName=set-title' >"$root/ansible.log" 2>&1
workspace_checksum_after="$(sha256sum "$root/workspace/site-values/app.yaml")"

[[ "$workspace_checksum_before" == "$workspace_checksum_after" ]]
grep -Fq 'delete job eva-app-tls-job -n eva-app --ignore-not-found=true --wait=true --timeout=120s' "$root/kubectl.log"
grep -Fq 'wait --for=condition=Complete job/eva-app-tls-job -n eva-app --timeout=120s' "$root/kubectl.log"
grep -Fq 'create secret tls eva-tls-for-traefik -' "$root/kubectl.log"
grep -Fq 'apply --filename=-' "$root/kubectl.log"
[[ "$(stat -c '%a' "$root/rendered/effective-input-values.yaml")" == 600 ]]
[[ "$(stat -c '%a' "$root/rendered/resolved-values.yaml")" == 600 ]]
grep -Fq 'baseUrl: https://workspace.example' "$root/rendered/effective-input-values.yaml"
grep -Fq 'adminClientSecret: generated-secret' "$root/rendered/effective-input-values.yaml"
grep -Fq 'workerNum: 11' "$root/rendered/effective-input-values.yaml"
grep -Fq 'workerNum: 7' "$root/rendered/effective-input-values.yaml"
grep -Fq 'browserTitleName: CLI title' "$root/rendered/effective-input-values.yaml"
grep -Fq -- '--set app.browserTitleName=set-title' "$root/helm.log"
if grep -Fq 'generated-secret' "$root/ansible.log"; then
  echo 'Generated IAM secret leaked into Ansible log' >&2
  false
fi
if grep -Fq 'generated-secret' "$root/rendered/values-sources.yaml"; then
  echo 'Generated IAM secret leaked into values-sources.yaml' >&2
  false
fi

root="$(make_fixture missing-handoff)"
printf '%s\n' 'localhost:' '  app:' '    sso:' '      baseUrl: https://workspace.example' >"$root/workspace/site-values/app.yaml"
if run_app "$root" 'app,iam' 'app.replicaCount=2' >"$root/ansible.log" 2>&1; then
  echo 'App accepted a missing IAM handoff' >&2
  exit 1
fi
grep -Fq 'EVA App SSO 입력이 필요합니다' "$root/ansible.log"

root="$(make_fixture empty-handoff)"
printf '%s\n' 'localhost:' '  app: {}' >"$root/workspace/site-values/app.yaml"
printf '%s\n' 'schema_version: v1' 'site_id: site' 'target: localhost' 'iam_public_host: iam.example' 'generated_at: "2026-09-15T00:00:00Z"' 'sso:' '  baseUrl: https://iam.example' '  adminClientSecret: ""' >"$root/state/site/localhost/eva-iam.yaml"
if run_app "$root" 'app,iam' 'app.replicaCount=2' >"$root/ansible.log" 2>&1; then
  echo 'App accepted an empty IAM client secret' >&2
  exit 1
fi
grep -Fq 'EVA App SSO 입력이 필요합니다' "$root/ansible.log"

root="$(make_fixture app-only-workspace-sso)"
printf '%s\n' 'localhost:' '  app:' '    sso:' '      baseUrl: https://workspace.example' '      adminClientSecret: workspace-secret' >"$root/workspace/site-values/app.yaml"
printf '%s\n' 'schema_version: v1' 'site_id: another-site' 'target: another-target' 'iam_public_host: stale-iam.example' 'generated_at: "2026-09-15T00:00:00Z"' 'sso:' '  baseUrl: https://stale-iam.example' '  adminClientSecret: stale-secret' >"$root/state/site/localhost/eva-iam.yaml"
printf '%s\n' 'app: {}' >"$root/cli.yaml"
run_app "$root" 'app' 'app.replicaCount=2' >"$root/ansible.log" 2>&1
grep -Fq 'adminClientSecret: workspace-secret' "$root/rendered/effective-input-values.yaml"
if grep -Fq 'stale-iam.example' "$root/rendered/effective-input-values.yaml"; then
  echo 'Stale IAM base URL was applied to effective values' >&2
  false
fi
if grep -Fq 'stale-secret' "$root/rendered/effective-input-values.yaml"; then
  echo 'Stale IAM secret was applied to effective values' >&2
  false
fi
grep -Fq 'iam_handoff: false' "$root/rendered/values-sources.yaml"
if grep -Fq 'stale-secret' "$root/ansible.log"; then
  echo 'Stale IAM secret leaked into Ansible log' >&2
  false
fi

root="$(make_fixture app-only-matching-handoff)"
printf '%s\n' 'localhost:' '  app: {}' >"$root/workspace/site-values/app.yaml"
printf '%s\n' 'schema_version: v1' 'site_id: site' 'target: localhost' 'iam_public_host: iam.example' 'generated_at: "2026-09-15T00:00:00Z"' 'sso:' '  baseUrl: https://iam.example' '  adminClientSecret: matching-secret' >"$root/state/site/localhost/eva-iam.yaml"
printf '%s\n' 'app: {}' >"$root/cli.yaml"
run_app "$root" 'app' 'app.replicaCount=2' >"$root/ansible.log" 2>&1
grep -Fq 'baseUrl: https://iam.example' "$root/rendered/effective-input-values.yaml"
grep -Fq 'adminClientSecret: matching-secret' "$root/rendered/effective-input-values.yaml"
grep -Fq 'iam_handoff: true' "$root/rendered/values-sources.yaml"

root="$(make_fixture app-only-mismatched-handoff)"
printf '%s\n' 'localhost:' '  app: {}' >"$root/workspace/site-values/app.yaml"
printf '%s\n' 'schema_version: v1' 'site_id: another-site' 'target: localhost' 'iam_public_host: stale-iam.example' 'generated_at: "2026-09-15T00:00:00Z"' 'sso:' '  baseUrl: https://stale-iam.example' '  adminClientSecret: stale-secret' >"$root/state/site/localhost/eva-iam.yaml"
printf '%s\n' 'app: {}' >"$root/cli.yaml"
if run_app "$root" 'app' 'app.replicaCount=2' >"$root/ansible.log" 2>&1; then
  echo 'App accepted a handoff for another site' >&2
  exit 1
fi
grep -Fq 'EVA App SSO 입력이 필요합니다' "$root/ansible.log"

root="$(make_fixture app-only-wrong-target-handoff)"
printf '%s\n' 'localhost:' '  app: {}' >"$root/workspace/site-values/app.yaml"
printf '%s\n' 'schema_version: v1' 'site_id: site' 'target: another-target' 'iam_public_host: stale-iam.example' 'generated_at: "2026-09-15T00:00:00Z"' 'sso:' '  baseUrl: https://stale-iam.example' '  adminClientSecret: stale-secret' >"$root/state/site/localhost/eva-iam.yaml"
printf '%s\n' 'app: {}' >"$root/cli.yaml"
if run_app "$root" 'app' 'app.replicaCount=2' >"$root/ansible.log" 2>&1; then
  echo 'App accepted a handoff for another target' >&2
  exit 1
fi
grep -Fq 'EVA App SSO 입력이 필요합니다' "$root/ansible.log"

root="$(make_fixture app-only-partial-workspace-sso)"
cat >"$root/workspace/site-values/app.yaml" <<'EOF'
localhost:
  app:
    sso:
      redis:
        host: redis.workspace.example
EOF
printf '%s\n' 'schema_version: v1' 'site_id: site' 'target: localhost' 'iam_public_host: iam.example' 'generated_at: "2026-09-15T00:00:00Z"' 'sso:' '  baseUrl: https://iam.example' '  adminClientSecret: handoff-secret' >"$root/state/site/localhost/eva-iam.yaml"
printf '%s\n' 'app: {}' >"$root/cli.yaml"
run_app "$root" 'app' 'app.replicaCount=2' >"$root/ansible.log" 2>&1
grep -Fq 'baseUrl: https://iam.example' "$root/rendered/effective-input-values.yaml"
grep -Fq 'adminClientSecret: handoff-secret' "$root/rendered/effective-input-values.yaml"
grep -Fq 'host: redis.workspace.example' "$root/rendered/effective-input-values.yaml"

root="$(make_fixture app-only-no-handoff)"
printf '%s\n' 'localhost:' '  app: {}' >"$root/workspace/site-values/app.yaml"
printf '%s\n' 'app: {}' >"$root/cli.yaml"
if run_app "$root" 'app' 'app.replicaCount=2' >"$root/ansible.log" 2>&1; then
  echo 'App accepted chart defaults as implicit Workspace SSO' >&2
  exit 1
fi
grep -Fq 'EVA App SSO 입력이 필요합니다' "$root/ansible.log"

root="$(make_fixture tls-job-existing)"
printf '%s\n' \
  'localhost:' \
  '  app:' \
  '    sso:' \
  '      baseUrl: https://workspace.example' \
  '      adminClientSecret: workspace-secret' \
  '  ingress:' \
  '    tls:' \
  '      create: true' \
  '      authType: hostPath' \
  >"$root/workspace/site-values/app.yaml"
printf '%s\n' 'app: {}' >"$root/cli.yaml"
FAKE_DEPLOYMENT_EXISTS=true run_app "$root" 'app' 'app.replicaCount=2' >"$root/ansible.log" 2>&1
grep -Fq 'delete job eva-app-tls-job -n eva-app --ignore-not-found=true --wait=true --timeout=120s' "$root/kubectl.log"
grep -Fq 'wait --for=condition=Complete job/eva-app-tls-job -n eva-app --timeout=120s' "$root/kubectl.log"
delete_line="$(grep -n -F 'delete job eva-app-tls-job' "$root/kubectl.log" | head -1 | cut -d: -f1)"
scale_line="$(grep -n -F 'scale deployment eva-app' "$root/kubectl.log" | head -1 | cut -d: -f1)"
[[ "$delete_line" -lt "$scale_line" ]]

root="$(make_fixture tls-job-disabled)"
cat >"$root/workspace/site-values/app.yaml" <<'EOF'
localhost:
  app:
    sso:
      baseUrl: https://workspace.example
      adminClientSecret: workspace-secret
  ingress:
    tls:
      create: false
      authType: hostPath
EOF
printf '%s\n' 'app: {}' >"$root/cli.yaml"
run_app "$root" 'app' 'app.replicaCount=2' >"$root/ansible.log" 2>&1

if grep -Fq 'delete job eva-app-tls-job' "$root/kubectl.log"; then
  echo 'App deleted a TLS Job when TLS Job creation was disabled' >&2
  false
fi

if grep -Fq 'wait --for=condition=Complete job/eva-app-tls-job' "$root/kubectl.log"; then
  echo 'App waited for a TLS Job when TLS Job creation was disabled' >&2
  false
fi

root="$(make_fixture tls-job-delete-failure)"
printf '%s\n' \
  'localhost:' \
  '  app:' \
  '    sso:' \
  '      baseUrl: https://workspace.example' \
  '      adminClientSecret: workspace-secret' \
  '  ingress:' \
  '    tls:' \
  '      create: true' \
  '      authType: hostPath' \
  >"$root/workspace/site-values/app.yaml"
printf '%s\n' 'app: {}' >"$root/cli.yaml"
if FAKE_TLS_DELETE_FAIL=true run_app "$root" 'app' 'app.replicaCount=2' >"$root/ansible.log" 2>&1; then
  echo 'App accepted a failed TLS Job deletion' >&2
  exit 1
fi

root="$(make_fixture tls-job-completion-failure)"
printf '%s\n' \
  'localhost:' \
  '  app:' \
  '    sso:' \
  '      baseUrl: https://workspace.example' \
  '      adminClientSecret: workspace-secret' \
  '  ingress:' \
  '    tls:' \
  '      create: true' \
  '      authType: hostPath' \
  >"$root/workspace/site-values/app.yaml"
printf '%s\n' 'app: {}' >"$root/cli.yaml"
if FAKE_TLS_WAIT_FAIL=true run_app "$root" 'app' 'app.replicaCount=2' >"$root/ansible.log" 2>&1; then
  echo 'App accepted an incomplete TLS Job' >&2
  exit 1
fi

if grep -Eq 'kubectl delete (secret|pvc|deployment|statefulset)' "$REPO_ROOT/src/solution/roles/eva_app/tasks/main.yaml"; then
  echo 'EVA App TLS Job cleanup deletes a protected resource type' >&2
  exit 1
fi

grep -Fq 'eva_iam_app_handoff_path' "$REPO_ROOT/src/solution/roles/eva_iam/tasks/main.yaml"
grep -Fq 'eva_persistent_state_root' "$REPO_ROOT/src/solution/roles/eva_iam/tasks/main.yaml"
grep -Fq 'mode: "0700"' "$REPO_ROOT/src/solution/roles/eva_iam/tasks/main.yaml"
grep -Fq 'mode: "0600"' "$REPO_ROOT/src/solution/roles/eva_iam/tasks/main.yaml"
if grep -Fq "eva_site_config_root ~ '/' ~ eva_app_values_host ~ '/eva-iam.yaml'" "$REPO_ROOT/src/solution/roles/eva_app/tasks/main.yaml"; then
  echo 'EVA App still reads a Release-local IAM handoff path' >&2
  exit 1
fi

echo 'EVA App values contract tests passed.'
