#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TASK_FILE="$REPO_ROOT/src/infra/roles/precondition/tasks/tls_san.yaml"
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

VALIDATOR_FILE="$TMP_ROOT/validator.py"

python3 - "$TASK_FILE" "$VALIDATOR_FILE" <<'PY_EXTRACT'
from pathlib import Path
import sys

import yaml


task_path = Path(sys.argv[1])
output_path = Path(sys.argv[2])

tasks = yaml.safe_load(task_path.read_text())

matches = [
    task
    for task in tasks
    if isinstance(task, dict)
    and task.get("name")
    == "Validate selected ingress certificate SANs"
]

if len(matches) != 1:
    raise SystemExit(
        "[ERROR] expected exactly one TLS SAN validation task"
    )

command = matches[0].get("ansible.builtin.command", {})
argv = command.get("argv", [])

if (
    not isinstance(argv, list)
    or len(argv) < 3
    or argv[0] != "python3"
    or argv[1] != "-c"
):
    raise SystemExit(
        "[ERROR] TLS SAN validator command contract is invalid"
    )

validator = argv[2]

required_contracts = (
    "import ipaddress",
    "subjectAltName",
    "matches_dns",
    'pattern_labels[0] == "*"',
    "len(pattern_labels) == len(hostname_labels)",
    '"IP Address"',
)

for contract in required_contracts:
    if contract not in validator:
        raise SystemExit(
            "[ERROR] TLS SAN validator is missing contract: "
            + contract
        )

compile(
    validator,
    "tls-san-validator.py",
    "exec",
)

output_path.write_text(validator)
PY_EXTRACT

make_certificate() {
  local name="$1"
  local san="${2:-}"
  local key="$TMP_ROOT/$name.key"
  local certificate="$TMP_ROOT/$name.crt"

  if [[ -n "$san" ]]; then
    openssl req \
      -x509 \
      -newkey rsa:2048 \
      -nodes \
      -days 1 \
      -subj "/CN=unused.example.invalid" \
      -addext "subjectAltName=$san" \
      -keyout "$key" \
      -out "$certificate" \
      >"$TMP_ROOT/$name.log" 2>&1
  else
    openssl req \
      -x509 \
      -newkey rsa:2048 \
      -nodes \
      -days 1 \
      -subj "/CN=unused.example.invalid" \
      -keyout "$key" \
      -out "$certificate" \
      >"$TMP_ROOT/$name.log" 2>&1
  fi

  printf '%s\n' "$certificate"
}

expect_success() {
  local name="$1"
  local host="$2"
  local san="$3"
  local certificate

  certificate="$(make_certificate "$name" "$san")"

  if python3 "$VALIDATOR_FILE" "$host" "$certificate"; then
    echo "[OK] $name"
  else
    echo "[ERROR] expected SAN validation success: $name" >&2
    return 1
  fi
}

expect_failure() {
  local name="$1"
  local host="$2"
  local san="${3:-}"
  local certificate

  certificate="$(make_certificate "$name" "$san")"

  if python3 "$VALIDATOR_FILE" "$host" "$certificate"; then
    echo "[ERROR] expected SAN validation failure: $name" >&2
    return 1
  fi

  echo "[OK] $name"
}

expect_success \
  exact-dns \
  app196.eva-dev.lge.com \
  "DNS:app196.eva-dev.lge.com"

expect_success \
  wildcard-dns \
  app196.eva-dev.lge.com \
  "DNS:*.eva-dev.lge.com"

expect_success \
  case-insensitive-dns \
  APP196.EVA-DEV.LGE.COM \
  "DNS:*.eva-dev.lge.com"

expect_success \
  trailing-dot-dns \
  app196.eva-dev.lge.com. \
  "DNS:*.eva-dev.lge.com"

expect_failure \
  wildcard-multiple-labels \
  nested.app196.eva-dev.lge.com \
  "DNS:*.eva-dev.lge.com"

expect_failure \
  wildcard-wrong-suffix \
  app196.eva-dev.lge.com \
  "DNS:*.example.com"

expect_failure \
  partial-wildcard \
  app196.eva-dev.lge.com \
  "DNS:app*.eva-dev.lge.com"

expect_failure \
  multiple-wildcards \
  app196.eva-dev.lge.com \
  "DNS:*.*.lge.com"

expect_success \
  exact-ipv4 \
  10.159.56.196 \
  "IP:10.159.56.196"

expect_success \
  exact-ipv6 \
  2001:db8::196 \
  "IP:2001:db8::196"

expect_failure \
  mismatched-ip \
  10.159.56.196 \
  "IP:10.159.56.197"

expect_failure \
  ip-in-dns-san \
  10.159.56.196 \
  "DNS:10.159.56.196"

expect_failure \
  missing-san \
  app196.eva-dev.lge.com

# Execute the real precondition tasks, not a test-side copy of the selection
# expression. Each local inventory alias has isolated facts/include_vars state.
make_certificate fixture-ip 'IP:192.0.2.10' >/dev/null
make_certificate fixture-dns 'DNS:ingress.example.test' >/dev/null
mkdir -p "$TMP_ROOT/bin"
cat >"$TMP_ROOT/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${FAKE_KUBECTL_LOG:?}"
echo 'SAN preconditions must not invoke kubectl' >&2
exit 99
EOF
chmod 0755 "$TMP_ROOT/bin/kubectl"

python3 - "$TASK_FILE" "$TMP_ROOT" <<'PY_FIXTURES'
from pathlib import Path
import shutil
import sys

import yaml

task_file = sys.argv[1]
root = Path(sys.argv[2])
plays = []
inventory = []


def add_case(name, settings, selected=None, expected=(), failure=None):
  case = root / name
  case.mkdir()
  target = "192.0.2.100"
  if selected is None:
    selected = ",".join(settings)
  for component, (host, ingress, cert) in settings.items():
    cert_dir = case / component / "certs"
    values = {"ingress": ingress}
    values["config" if component == "iam" else "app"] = {
      "host" if component == "iam" else "backendHost": host
    }
    # No certificate directory exists in skip/missing cases. Even stat/SAN
    # results must stay empty, so a silently ignored failed read cannot pass.
    ingress.setdefault("tls", {})["hostPath"] = str(cert_dir)
    if cert:
      cert_dir.mkdir(parents=True)
      shutil.copyfile(root / f"fixture-{cert}.crt", cert_dir / "tls.crt")
    (case / f"{component}.yaml").write_text(
      yaml.safe_dump({target: values})
    )

  imported = {"ansible.builtin.import_tasks": task_file}
  tasks = []
  if failure:
    tasks.append({
      "name": f"Require expected certificate failure: {name}",
      "block": [imported, {
        "ansible.builtin.fail": {"msg": "unexpected TLS validation success"}
      }],
      "rescue": [{"ansible.builtin.assert": {"that": [
        f"ansible_failed_task.name == '{failure}'",
        "'App' in (ansible_failed_result | to_json)",
      ]}}],
    })
  else:
    tasks.extend([imported, {
      "name": f"Assert actual selected certificate and SAN checks: {name}",
      "ansible.builtin.assert": {"that": [
        "(precondition_tls_checks | map(attribute='component') | list) == fixture_expected_components",
        "(precondition_tls_certificate_files.results | length) == (fixture_expected_components | length)",
        "(precondition_tls_san_results.results | length) == (fixture_expected_components | length)",
        "(precondition_tls_san_results.results | map(attribute='rc') | reject('equalto', 0) | list | length) == 0",
      ]},
    }])
  alias = name.replace("-", "_")
  inventory.append(f"{alias} ansible_connection=local ansible_become=false")
  plays.append({
    "name": name,
    "hosts": alias,
    "gather_facts": False,
    "vars": {
      "ansible_host": target,
      "eva_site_values_root": str(case),
      "eva_enabled_components": selected,
      "fixture_expected_components": list(expected),
    },
    "tasks": [{
      "block": tasks,
      "rescue": [{"ansible.builtin.set_fact": {
        "fixture_unexpected_failure": "{{ ansible_failed_task.name }}: {{ ansible_failed_result.msg | default('task failed') }}"
      }}],
    }],
  })


for component in ("app", "iam"):
  label = "App" if component == "app" else "IAM"
  for kind, host in (("ip", "192.0.2.10"), ("dns", "ingress.example.test")):
    add_case(f"{component}-http-{kind}", {
      component: (host, {"tls": {"enabled": False, "create": True}}, None)
    })
    add_case(f"{component}-https-{kind}", {
      component: (host, {"tls": {"enabled": True, "create": True, "authType": "hostPath"}}, kind)
    }, expected=(label,))
  for name, ingress in (
    ("ingress-disabled", {"enabled": False, "tls": {"enabled": True, "create": True}}),
    ("create-false", {"tls": {"enabled": True, "create": False}}),
    ("secret-auth", {"tls": {"enabled": True, "create": True, "authType": "secret"}}),
    ("string-false", {"tls": {"enabled": "false", "create": True}}),
  ):
    add_case(f"{component}-{name}", {component: ("192.0.2.10", ingress, None)})
  add_case(f"{component}-not-selected", {
    component: ("192.0.2.10", {"tls": {"enabled": True, "create": True}}, None)
  }, selected="agent")
  # Omitted ingress.enabled/tls.enabled/tls.create/authType all default on.
  add_case(f"{component}-defaults", {
    component: ("192.0.2.10", {}, "ip")
  }, expected=(label,))

add_case("no-selected-components-or-values", {}, selected="")
add_case("mixed-app-http-iam-https", {
  "app": ("192.0.2.10", {"tls": {"enabled": False, "create": True}}, None),
  "iam": ("ingress.example.test", {"tls": {"enabled": True}}, "dns"),
}, expected=("IAM",))
add_case("mixed-app-https-iam-http", {
  "app": ("192.0.2.10", {"tls": {"enabled": True}}, "ip"),
  "iam": ("ingress.example.test", {"tls": {"enabled": False, "create": True}}, None),
}, expected=("App",))
for kind, host in (("ip", "192.0.2.10"), ("dns", "ingress.example.test")):
  add_case(f"https-{kind}-missing-cert", {
    "app": (host, {"tls": {"enabled": True}}, None)
  }, selected="app", failure="Require selected ingress certificate files")
add_case("https-mismatched-san", {
  "app": ("other.example.test", {"tls": {"enabled": True}}, "dns")
}, selected="app", failure="Require selected ingress addresses in certificate SANs")

print(f"[INFO] executing {len(plays)} real TLS SAN precondition fixtures")
plays.append({
  "name": "Require every TLS SAN fixture to pass",
  "hosts": "all",
  "gather_facts": False,
  "tasks": [{"ansible.builtin.assert": {
    "that": ["fixture_unexpected_failure is not defined"],
    "fail_msg": "{{ fixture_unexpected_failure | default('') }}",
  }}],
})
(root / "inventory.ini").write_text("\n".join(inventory) + "\n")
(root / "playbook.yaml").write_text(yaml.safe_dump(plays, sort_keys=False))
PY_FIXTURES

if ! PATH="$TMP_ROOT/bin:$PATH" FAKE_KUBECTL_LOG="$TMP_ROOT/kubectl.log" \
  "$ANSIBLE_PLAYBOOK" -i "$TMP_ROOT/inventory.ini" "$TMP_ROOT/playbook.yaml" \
  >"$TMP_ROOT/ansible.log" 2>&1; then
  cat "$TMP_ROOT/ansible.log" >&2
  exit 1
fi
if [[ -e "$TMP_ROOT/kubectl.log" ]]; then
  echo 'TLS SAN preconditions unexpectedly invoked kubectl' >&2
  exit 1
fi

echo 'TLS SAN validation contract tests passed.'
