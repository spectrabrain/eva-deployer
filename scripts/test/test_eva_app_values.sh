#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ANSIBLE_PLAYBOOK="${ANSIBLE_PLAYBOOK:-ansible-playbook}"
TMP_ROOT="$(mktemp -d)"

cleanup() {
  local status=$?
  if (( status != 0 )) && [[ -n "${root:-}" && -f "$root/ansible.log" ]]; then
    printf '\n[ERROR] App fixture failed; last Ansible diagnostics:\n' >&2
    tail -60 "$root/ansible.log" >&2
  fi
  rm -rf "$TMP_ROOT"
  return "$status"
}
trap cleanup EXIT

if ! command -v "$ANSIBLE_PLAYBOOK" >/dev/null 2>&1 && [[ ! -x "$ANSIBLE_PLAYBOOK" ]]; then
  echo "ansible-playbook was not found: $ANSIBLE_PLAYBOOK" >&2
  exit 1
fi

python3 - "$REPO_ROOT/src/solution/roles/eva_app/tasks/main.yaml" <<'PY'
import copy
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import textwrap
import yaml
from jinja2 import Environment, StrictUndefined

tasks = yaml.safe_load(Path(sys.argv[1]).read_text())
task = next(
    item
    for item in tasks
    if item.get("name") == "Assess verified legacy EVA App internal Service preservation"
)
shell = task.get("ansible.builtin.shell", "")
if r"[expected_application\]" in shell:
    raise SystemExit("[ERROR] internal Service validator contains an escaped closing bracket")
match = re.search(
    r"<<'PY_INTERNAL_SERVICE'\n(.*?)\nPY_INTERNAL_SERVICE",
    shell,
    re.DOTALL,
)
if match is None:
    raise SystemExit("[ERROR] internal Service Python validator is absent")
compile(textwrap.dedent(match.group(1)), "internal-service-validator", "exec")

# Exercise the actual post-rollout shell, not a reimplementation of its jq.
# Fake time advances by five seconds per observation; no real sleeps or extra
# full-role Ansible fixtures are needed for these failure cases.
health_task = next(t for t in tasks if t.get("name") == "Verify EVA App pod remains healthy after rollout")
assert health_task.get("when") == "not ansible_check_mode"
assert not health_task.get("failed_when", False)
rendered_shell = Environment(undefined=StrictUndefined).from_string(health_task["ansible.builtin.shell"]).render(
  eva_app_namespace="eva-app", eva_app_name="eva-app", eva_app_rollout_stabilization_seconds=10,
)
subprocess.run(["bash", "-n"], input=rendered_shell, text=True, check=True)
healthy = {"items": [{"metadata": {"name": "eva-app-pod", "uid": "pod-1"}, "status": {
  "phase": "Running", "conditions": [{"type": "Ready", "status": "True"}],
  "containerStatuses": [{"name": "eva-app", "containerID": "container-1", "ready": True,
               "restartCount": 0, "state": {"running": {}}}],
}}]}

def pod_with(status_changes=None, container_changes=None):
  result = copy.deepcopy(healthy)
  status = result["items"][0]["status"]
  status.update(status_changes or {})
  if container_changes:
    status["containerStatuses"][0].update(container_changes)
  return result

terminated = pod_with(container_changes={"ready": False, "restartCount": 4,
  "state": {"terminated": {"reason": "Error", "exitCode": 3}}})
restarted = pod_with(container_changes={"restartCount": 1, "containerID": "container-2"})
backoff = pod_with(container_changes={"ready": False, "restartCount": 4,
  "state": {"waiting": {"reason": "CrashLoopBackOff"}}})
pulling = pod_with(container_changes={"ready": False, "state": {"waiting": {"reason": "ImagePullBackOff"}}})

def check_health(name, responses, succeeds, expected=""):
  with tempfile.TemporaryDirectory() as directory:
    root = Path(directory)
    for index, response in enumerate(responses):
      (root / f"pods-{index}.json").write_text(json.dumps(response))
    scripts = {
      "kubectl": '''#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> "$TEST_ROOT/commands"
index=0
[[ ! -f "$TEST_ROOT/count" ]] || index="$(cat "$TEST_ROOT/count")"
printf '%s' "$((index + 1))" > "$TEST_ROOT/count"
(( index < TEST_RESPONSES_COUNT )) || index="$((TEST_RESPONSES_COUNT - 1))"
cat "$TEST_ROOT/pods-$index.json"
''',
      "date": '''#!/usr/bin/env bash
set -euo pipefail
now=0
[[ ! -f "$TEST_ROOT/time" ]] || now="$(cat "$TEST_ROOT/time")"
printf '%s' "$((now + 5))" > "$TEST_ROOT/time"
printf '%s\\n' "$now"
''',
      "sleep": "#!/usr/bin/env bash\ntrue\n",
    }
    for tool, content in scripts.items():
      path = root / tool
      path.write_text(content)
      path.chmod(0o755)
    result = subprocess.run(["bash", "-c", rendered_shell], text=True, capture_output=True, timeout=10,
      env=dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"],
           TEST_ROOT=str(root), TEST_RESPONSES_COUNT=str(len(responses))))
    output = result.stdout + result.stderr
    assert (result.returncode == 0) == succeeds, (name, result.returncode, output)
    assert expected in output, (name, expected, output)
    commands = (root / "commands").read_text().splitlines()
    assert all(command.startswith("get pods -n eva-app -l app.kubernetes.io/instance=eva-app,component=eva-app -o json")
           for command in commands), commands
  print("[OK] App rollout health: " + name)

check_health("stable ready pod", [healthy], True)
check_health("Running phase but Terminated Error exit 3", [terminated], False, "exit_code=3")
check_health("initially ready then exit 3", [healthy, terminated], False, "exit_code=3")
check_health("CrashLoopBackOff", [backoff], False, "CrashLoopBackOff")
check_health("restart during observation", [healthy, restarted], False, "restarted-or-replaced")
check_health("historical restart already recovered", [restarted], True)
check_health("image pull transient recovered", [pulling, healthy], True)
check_health("missing pods", [{"items": []}], False, "pods: none")
check_health("missing container statuses", [pod_with({"containerStatuses": []})], False, "not-ready")
check_health("missing Ready condition", [pod_with({"conditions": []})], False, "not-ready")
check_health("Ready flag without running state", [pod_with(container_changes={"state": {}})], False, "not-ready")
PY

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
  "list --namespace") printf '[]\n' ;;
  "show values")
    printf 'database:\n  name: %s\napp:\n  sso:\n    baseUrl: https://chart.example\n    adminClientSecret: chart-default-secret\n' "${FAKE_CHART_DB_NAME:-eva_v3_0}"
    ;;
  "status eva-app") exit 1 ;;
  "template eva-app")
    # Production DB preparation resolves the intended DB from the rendered
    # App container, including the Helm --set layer. Other mocks have no DB.
    printf '%s\n' '{"kind":"Deployment","metadata":{"name":"eva-app"},"spec":{"template":{"spec":{"containers":[{"name":"eva-app","env":[{"name":"MYSQL_NAME","value":"eva_v3_0"}]}]}}}}'
    printf '%s\n' '---' '{"kind":"Deployment","metadata":{"name":"eva-app-mysql"}}'
    ;;
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
    # These Values fixtures model no existing database for the transition
    # helper's optional lookups. DB migration has its own stateful tests.
    if [[ " $* " == *' --ignore-not-found=true '* ]]; then
      exit 0
    fi
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
  "get pods")
    case " $* " in
      *' -o jsonpath={.items[0].metadata.name} '*)
        printf 'eva-app-pod'
        ;;
      *' -o json '*)
        printf '%s\n' '{"items":[{"metadata":{"name":"eva-app-pod","uid":"pod-1"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"eva-app","ready":true,"restartCount":0,"containerID":"container-1","state":{"running":{}}}]}}]}'
        ;;
      *)
        echo eva-app-pod
        ;;
    esac
    ;;
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
    eva_app_rollout_stabilization_seconds: 0
    repository_mode: cloud_repository
  tasks:
    - ansible.builtin.import_role:
        name: eva_app
    - name: Assert actual App TLS Job gating fact
      ansible.builtin.assert:
        that:
          - eva_app_tls_job_enabled is boolean
          - eva_app_tls_job_enabled == fixture_expected_tls_job_enabled
      when: fixture_expected_tls_job_enabled is defined
EOF

  printf '%s\n' "$root"
}

run_app() {
  local root="$1"
  local components="$2"
  local set_value="$3"
  shift 3

  PATH="$root/bin:$PATH" \
    FAKE_HELM_LOG="$root/helm.log" \
    FAKE_KUBECTL_LOG="$root/kubectl.log" \
    ANSIBLE_ROLES_PATH="$REPO_ROOT/src/solution/roles" \
    "$ANSIBLE_PLAYBOOK" -i "$root/inventory.ini" "$root/playbook.yaml" \
      -e "eva_enabled_components=$components" \
      -e "eva_cli_values_path=$root/cli.yaml" \
      -e "{\"eva_cli_helm_set\":[\"$set_value\"]}" "$@"
}

assert_database_values() {
  local root="$1"
  local chart_default="$2"
  local role_name="$3"
  local effective_name="$4"

  python3 - "$root/rendered" "$chart_default" "$role_name" "$effective_name" <<'PY'
from pathlib import Path
import sys
import yaml

root = Path(sys.argv[1])
defaults = yaml.safe_load((root / "chart-defaults.yaml").read_text())
override = yaml.safe_load((root / "app-k3s-override.yaml").read_text())
effective = yaml.safe_load((root / "effective-input-values.yaml").read_text())
assert defaults["database"]["name"] == sys.argv[2], defaults["database"]
assert override["database"]["type"] == "internal", override["database"]
for values, expected in ((override, sys.argv[3]), (effective, sys.argv[4])):
    if expected:
        assert values["database"]["name"] == expected, values["database"]
    else:
        assert "name" not in values["database"], values["database"]
PY
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
assert_database_values "$root" eva_v3_0 '' ''
grep -Fq 'show values eva-app/eva-app --version 3.1.8' "$root/helm.log"
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
grep -Fq 'activation_mode: online' "$root/rendered/effective-input-values.yaml"
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

for scenario in remote-default local-default remote-online local-online remote-role-online; do
  root="$(make_fixture "license-$scenario")"
  mode=remote_repository
  [[ "$scenario" == local-* ]] && mode=local_repository
  expected=offline
  cat >"$root/workspace/site-values/app.yaml" <<'EOF'
localhost:
  app:
    sso:
      baseUrl: https://workspace.example
      adminClientSecret: workspace-secret
EOF
  if [[ "$scenario" == remote-online || "$scenario" == local-online ]]; then
    printf '%s\n' '    license:' '      activation_mode: online' >>"$root/workspace/site-values/app.yaml"
    expected=online
  fi
  args=()
  if [[ "$scenario" == remote-role-online ]]; then
    args+=(-e eva_app_license_activation_mode=online)
    expected=online
  fi
  printf '%s\n' 'app: {}' >"$root/cli.yaml"
  printf 'fake chart\n' >"$root/custom-chart.tgz"
  if ! run_app "$root" 'app' 'app.replicaCount=2' \
    -e "repository_mode=$mode" -e repository_registry=harbor.example.test:32080 \
    -e "eva_cli_chart_path=$root/custom-chart.tgz" "${args[@]}" >"$root/ansible.log" 2>&1; then
    cat "$root/ansible.log" >&2
    exit 1
  fi
  grep -Fq "activation_mode: $expected" "$root/rendered/effective-input-values.yaml"
  echo "[OK] App license $scenario ($expected)"
done

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
printf '%s\n' 'localhost:' '  database:' '    name: eva' '  app:' '    sso:' '      baseUrl: https://workspace.example' '      adminClientSecret: workspace-secret' >"$root/workspace/site-values/app.yaml"
printf '%s\n' 'schema_version: v1' 'site_id: another-site' 'target: another-target' 'iam_public_host: stale-iam.example' 'generated_at: "2026-09-15T00:00:00Z"' 'sso:' '  baseUrl: https://stale-iam.example' '  adminClientSecret: stale-secret' >"$root/state/site/localhost/eva-iam.yaml"
printf '%s\n' 'app: {}' >"$root/cli.yaml"
run_app "$root" 'app' 'app.replicaCount=2' -e eva_app_db_name=role_db >"$root/ansible.log" 2>&1
assert_database_values "$root" eva_v3_0 role_db eva
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
printf '%s\n' 'localhost:' '  database:' '    name: workspace_db' '  app: {}' >"$root/workspace/site-values/app.yaml"
printf '%s\n' 'schema_version: v1' 'site_id: site' 'target: localhost' 'iam_public_host: iam.example' 'generated_at: "2026-09-15T00:00:00Z"' 'sso:' '  baseUrl: https://iam.example' '  adminClientSecret: matching-secret' >"$root/state/site/localhost/eva-iam.yaml"
printf '%s\n' 'database:' '  name: cli_db' >"$root/cli.yaml"
run_app "$root" 'app' 'database.name=set_db' -e eva_app_db_name=role_db >"$root/ansible.log" 2>&1
assert_database_values "$root" eva_v3_0 role_db cli_db
grep -Fq -- '--set database.name=set_db' "$root/helm.log"
grep -Fq 'baseUrl: https://iam.example' "$root/rendered/effective-input-values.yaml"
grep -Fq 'adminClientSecret: matching-secret' "$root/rendered/effective-input-values.yaml"
grep -Fq 'iam_handoff: true' "$root/rendered/values-sources.yaml"

for scenario in explicit-role-db empty-role-db; do
  root="$(make_fixture "$scenario")"
  printf '%s\n' 'localhost:' '  app:' '    sso:' '      baseUrl: https://workspace.example' '      adminClientSecret: workspace-secret' >"$root/workspace/site-values/app.yaml"
  printf '%s\n' 'app: {}' >"$root/cli.yaml"
  if [[ "$scenario" == explicit-role-db ]]; then
    # A CLI Chart supplies its own defaults; the catalog version must not select another Chart.
    printf 'fake chart\n' >"$root/custom-chart.tgz"
    run_app "$root" 'app' 'app.replicaCount=2' \
      -e eva_app_db_name=custom_db -e "eva_cli_chart_path=$root/custom-chart.tgz" >"$root/ansible.log" 2>&1
    assert_database_values "$root" eva_v3_0 custom_db custom_db
    grep -Fxq "show values $root/app-work/cli-chart.tgz" "$root/helm.log"
  else
    # Both an empty role variable and a changed Chart default must remain Chart-driven.
    FAKE_CHART_DB_NAME=eva_next run_app "$root" 'app' 'app.replicaCount=2' \
      -e eva_app_db_name= >"$root/ansible.log" 2>&1
    assert_database_values "$root" eva_next '' ''
  fi
done

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

# Exercise the full production role with no host certificate directory. Keeping
# create:true is intentional: HTTP must override it, while secret auth must not
# disable the chart's TLS Job (only the hostPath Traefik caller).
for scenario in http-ip http-dns https-secret-auth; do
  root="$(make_fixture "$scenario")"
  rm -rf "$root/certs"
  host=192.0.2.10
  tls_enabled=false
  tls_auth=hostPath
  expected_job=false
  if [[ "$scenario" == http-dns ]]; then
    host=app.example.test
  elif [[ "$scenario" == https-secret-auth ]]; then
    tls_enabled=true
    tls_auth=secret
    expected_job=true
  fi
  cat >"$root/workspace/site-values/app.yaml" <<EOF
localhost:
  app:
    backendHost: $host
    sso:
      baseUrl: http://iam.example.test
      adminClientSecret: workspace-secret
  ingress:
    enabled: true
    tls:
      enabled: $tls_enabled
      create: true
      authType: $tls_auth
      hostPath: $root/certs
EOF
  printf '%s\n' 'app: {}' >"$root/cli.yaml"
  if ! run_app "$root" 'app' 'app.replicaCount=2' \
    -e "{\"fixture_expected_tls_job_enabled\":$expected_job}" >"$root/ansible.log" 2>&1; then
    cat "$root/ansible.log" >&2
    exit 1
  fi
  [[ ! -e "$root/certs" ]]
  grep -Fq 'upgrade --install eva-app' "$root/helm.log"
  if grep -Eq 'create secret tls eva-tls-for-traefik|apply --filename=-' "$root/kubectl.log"; then
    echo "$scenario unexpectedly configured hostPath Traefik TLS resources" >&2
    exit 1
  fi
  if [[ "$expected_job" == true ]]; then
    grep -Fq 'delete job eva-app-tls-job' "$root/kubectl.log"
    grep -Fq 'wait --for=condition=Complete job/eva-app-tls-job' "$root/kubectl.log"
  elif grep -Eq 'delete job eva-app-tls-job|wait --for=condition=Complete job/eva-app-tls-job' "$root/kubectl.log"; then
    echo "$scenario deleted or waited for a disabled TLS Job" >&2
    exit 1
  fi
  python3 - "$root/rendered/effective-input-values.yaml" "$tls_enabled" <<'PY'
from pathlib import Path
import sys
import yaml

values = yaml.safe_load(Path(sys.argv[1]).read_text())
assert values["ingress"]["enabled"] is True, values["ingress"]
assert values["ingress"]["tls"]["enabled"] is (sys.argv[2] == "true"), values["ingress"]
assert values["ingress"]["tls"]["create"] is True, values["ingress"]
PY
  echo "[OK] actual App role $scenario (TLS Job enabled=$expected_job)"
done

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

grep -Fq 'Verify EVA App pod remains healthy after rollout' "$REPO_ROOT/src/solution/roles/eva_app/tasks/main.yaml"
grep -Fq 'app.kubernetes.io/instance={{ eva_app_name }},component=eva-app' "$REPO_ROOT/src/solution/roles/eva_app/tasks/main.yaml"

grep -Fq 'eva_iam_app_handoff_path' "$REPO_ROOT/src/solution/roles/eva_iam/tasks/main.yaml"
grep -Fq 'eva_persistent_state_root' "$REPO_ROOT/src/solution/roles/eva_iam/tasks/main.yaml"
grep -Fq 'mode: "0700"' "$REPO_ROOT/src/solution/roles/eva_iam/tasks/main.yaml"
grep -Fq 'mode: "0600"' "$REPO_ROOT/src/solution/roles/eva_iam/tasks/main.yaml"
if grep -Fq "eva_site_config_root ~ '/' ~ eva_app_values_host ~ '/eva-iam.yaml'" "$REPO_ROOT/src/solution/roles/eva_app/tasks/main.yaml"; then
  echo 'EVA App still reads a Release-local IAM handoff path' >&2
  exit 1
fi

echo 'EVA App values contract tests passed.'
