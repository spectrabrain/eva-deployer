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
grep -Fq 'eva_app_effective_values.ingress.tls.hostPath | default(eva_app_tls_host_path)' "$app_tasks"
grep -Fq 'eva_iam_effective_user_values.ingress.tls.hostPath | default(eva_iam_tls_host_path)' "$iam_tasks"

# IAM-to-App handoff must follow the same public URL contract as the IAM chart:
# explicit publicUrl wins, otherwise the scheme follows effective ingress TLS.
grep -Fq 'eva_iam_effective_tls_enabled: >-' "$iam_tasks"
grep -Fq 'eva_iam_effective_user_values.ingress.tls.enabled' "$iam_tasks"
grep -Fq '| default(true)' "$iam_tasks"
grep -Fq "'https://' if (eva_iam_effective_tls_enabled | bool) else 'http://'" "$iam_tasks"
grep -Fq '(eva_iam_effective_public_url | regex_replace' "$iam_tasks"

grep -Fq 'eva-tls-for-traefik' "$tls_tasks"
grep -Fq 'kind: TLSStore' "$tls_tasks"
grep -Fq 'namespace: kube-system' "$tls_tasks"
grep -Fq -- '--cert={{ eva_traefik_tls_host_path }}/tls.crt' "$tls_tasks"
grep -Fq -- '--key={{ eva_traefik_tls_host_path }}/tls.key' "$tls_tasks"

mkdir -p "$TMP_ROOT/bin"
cat >"$TMP_ROOT/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${FAKE_KUBECTL_LOG:?}"
case "$*" in
  'create secret tls eva-tls-for-traefik '*) printf 'fake-secret-manifest\n' ;;
  'apply --filename=-')
    cat >>"${FAKE_KUBECTL_LOG}.stdin"
    printf '\n' >>"${FAKE_KUBECTL_LOG}.stdin"
    printf 'configured\n'
    ;;
  *) echo "Unexpected fixture kubectl command: $*" >&2; exit 99 ;;
esac
EOF
chmod 0755 "$TMP_ROOT/bin/kubectl"

# Preserve the production import_role task (including vars/when) and the facts
# it consumes. Importing only eva_traefik_tls would miss broken caller gating.
python3 - "$app_tasks" "$iam_tasks" "$TMP_ROOT" <<'PY_FIXTURES'
from copy import deepcopy
from pathlib import Path
import sys

import yaml

root = Path(sys.argv[3])
plays = []
check_plays = []
inventory = []
expected_calls = []


def find_one(tasks, predicate):
    matches = [task for task in tasks if predicate(task)]
    assert len(matches) == 1, matches
    return matches[0]


for component, path in (("app", sys.argv[1]), ("iam", sys.argv[2])):
    tasks = yaml.safe_load(Path(path).read_text())
    caller = find_one(tasks, lambda task: task.get("ansible.builtin.import_role", {}).get("name") == "eva_traefik_tls")
    fact_key = "eva_app_tls_job_enabled" if component == "app" else "eva_iam_effective_tls_enabled"
    facts = find_one(tasks, lambda task: fact_key in task.get("ansible.builtin.set_fact", {}))
    # Run the actual complete set_fact task. IAM's NodePort/public URL inputs
    # are harmless local variables; no Helm, API checks or deployment tasks run.
    cases = [
        ("https-ip", "192.0.2.10", {"tls": {"enabled": True, "create": True, "authType": "hostPath"}}, True, True, True, False),
        ("https-dns", "iam.example.test", {"tls": {"enabled": True, "create": True}}, False, True, True, False),
        ("http-ip", "192.0.2.10", {"tls": {"enabled": False, "create": True}}, False, False, False, False),
        ("http-dns", "iam.example.test", {"tls": {"enabled": False, "create": True}}, False, False, False, False),
        ("ingress-disabled", "192.0.2.10", {"enabled": False, "tls": {"enabled": True, "create": True}}, False, False, False, False),
        ("create-false", "192.0.2.10", {"tls": {"enabled": True, "create": False}}, False, False, False, False),
        ("secret-auth", "192.0.2.10", {"tls": {"enabled": True, "create": True, "authType": "secret"}}, False, False, True, False),
        ("defaults", "192.0.2.10", {}, True, True, True, False),
        ("string-false", "192.0.2.10", {"tls": {"enabled": "false", "create": True}}, False, False, False, False),
        ("check-mode", "192.0.2.10", {"tls": {"enabled": True, "create": True}}, False, False, True, True),
        ("https-missing-cert", "192.0.2.10", {"tls": {"enabled": True, "create": True}}, False, True, True, False),
    ]
    for suffix, host, ingress, certificate, call_role, job_enabled, check_mode in cases:
        name = f"{component}-{suffix}"
        case = root / name
        case.mkdir()
        cert_dir = case / "certs"
        if suffix != "defaults":
            ingress["tls"].setdefault("authType", "hostPath")
        ingress.setdefault("tls", {})["hostPath"] = str(cert_dir)
        if certificate:
            cert_dir.mkdir()
            (cert_dir / "tls.crt").write_text("fixture certificate\n")
            (cert_dir / "tls.key").write_text("fixture private key\n")
        kubeconfig = case / "kubeconfig"
        kubeconfig.touch()
        values = {"ingress": ingress, "app": {"backendHost": host}, "config": {"host": host}}
        variables = {
            "eva_app_effective_values": values,
            "eva_app_backend_host": host,
            "eva_app_tls_host_path": str(cert_dir),
            "eva_app_kubeconfig": str(kubeconfig),
            "eva_iam_effective_user_values": values,
            "eva_iam_host": host,
            "eva_iam_tls_host_path": str(cert_dir),
            "eva_iam_kubeconfig": str(kubeconfig),
            "eva_iam_db_nodeport": 32000,
            "eva_iam_redis_nodeport": 32001,
            "eva_iam_redis_external_enabled": False,
            "eva_iam_ingress_path": "/",
            "eva_iam_public_port": "",
            "eva_iam_public_url": "",
            "fixture_expected_call": call_role,
            "fixture_expected_job": job_enabled,
        }
        fixture_tasks = [deepcopy(facts)]
        if component == "app":
            fixture_tasks.append({
                "name": f"Assert real App TLS Job boolean: {name}",
                "ansible.builtin.assert": {"that": [
                    "eva_app_tls_job_enabled is boolean",
                    "eva_app_tls_job_enabled == fixture_expected_job",
                ]},
            })
        if suffix == "https-missing-cert":
            fixture_tasks.append({
                "name": f"Require missing HTTPS certificate failure: {name}",
                "block": [deepcopy(caller), {
                    "ansible.builtin.fail": {"msg": "unexpected missing certificate success"}
                }],
                "rescue": [{"ansible.builtin.assert": {"that": [
                    "ansible_failed_task.name.endswith('Require TLS certificate files for an IP-based ingress host')",
                    "'IP 기반 ingress host에는' in (ansible_failed_result | to_json(ensure_ascii=false))",
                ]}}],
            })
        else:
            fixture_tasks.extend([deepcopy(caller), {
                "name": f"Assert actual caller execution or skip: {name}",
                "ansible.builtin.assert": {"that": [
                    "(not (eva_traefik_tls_host_ip.skipped | default(false))) == fixture_expected_call",
                ]},
            }])
        alias = name.replace("-", "_")
        inventory.append(f"{alias} ansible_connection=local ansible_become=false")
        # ansible_check_mode reflects the CLI flag, not a play/task-level
        # check_mode override. Execute these cases with a real --check run.
        (check_plays if check_mode else plays).append({
            "name": name,
            "hosts": alias,
            "gather_facts": False,
            "vars": variables,
            "environment": {"FAKE_KUBECTL_LOG": str(case / "kubectl.log")},
            "tasks": [{
                "block": fixture_tasks,
                "rescue": [{"ansible.builtin.set_fact": {
                    "fixture_unexpected_failure": "{{ ansible_failed_task.name }}: {{ ansible_failed_result.msg | default('task failed') }}"
                }}],
            }],
        })
        if certificate and call_role:
            expected_calls.append(name)

print(f"[INFO] executing {len(plays) + len(check_plays)} actual App/IAM TLS caller fixtures")
for group in (plays, check_plays):
        group.append({
                "name": "Require every actual TLS caller fixture to pass",
                "hosts": ":".join(play["hosts"] for play in group),
                "gather_facts": False,
                "tasks": [{"ansible.builtin.assert": {
                        "that": ["fixture_unexpected_failure is not defined"],
                        "fail_msg": "{{ fixture_unexpected_failure | default('') }}",
                }}],
        })
(root / "inventory.ini").write_text("\n".join(inventory) + "\n")
(root / "playbook.yaml").write_text(yaml.safe_dump(plays, sort_keys=False))
(root / "check-playbook.yaml").write_text(yaml.safe_dump(check_plays, sort_keys=False))
(root / "expected-calls.txt").write_text("\n".join(expected_calls) + "\n")
PY_FIXTURES

fixture_status=0
for mode in normal check; do
    check_args=()
    playbook="$TMP_ROOT/playbook.yaml"
    if [[ "$mode" == check ]]; then
        check_args=(--check)
        playbook="$TMP_ROOT/check-playbook.yaml"
    fi
    if ! PATH="$TMP_ROOT/bin:$PATH" \
        ANSIBLE_ROLES_PATH="$REPO_ROOT/src/solution/roles" \
        "$ANSIBLE_PLAYBOOK" -i "$TMP_ROOT/inventory.ini" "$playbook" "${check_args[@]}" \
        >>"$TMP_ROOT/ansible.log" 2>&1; then
        fixture_status=1
    fi
done
if [[ "$fixture_status" != 0 ]]; then
  cat "$TMP_ROOT/ansible.log" >&2
  exit 1
fi

for case_root in "$TMP_ROOT"/app-* "$TMP_ROOT"/iam-*; do
  name="${case_root##*/}"
  if grep -Fxq "$name" "$TMP_ROOT/expected-calls.txt"; then
    grep -Fxq "create secret tls eva-tls-for-traefik --namespace=kube-system --cert=$case_root/certs/tls.crt --key=$case_root/certs/tls.key --dry-run=client --output=yaml" "$case_root/kubectl.log"
    [[ "$(grep -Fxc 'apply --filename=-' "$case_root/kubectl.log")" == 2 ]]
    [[ "$(wc -l <"$case_root/kubectl.log")" == 3 ]]
    grep -Fxq 'fake-secret-manifest' "$case_root/kubectl.log.stdin"
    grep -Fq 'kind: TLSStore' "$case_root/kubectl.log.stdin"
    grep -Fq 'namespace: kube-system' "$case_root/kubectl.log.stdin"
    grep -Fq 'secretName: eva-tls-for-traefik' "$case_root/kubectl.log.stdin"
  elif [[ -e "$case_root/kubectl.log" ]]; then
    echo "$name unexpectedly configured Traefik TLS resources" >&2
    exit 1
  fi
done

echo 'EVA Traefik TLS contract tests passed.'
