#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TASK_FILE="$REPO_ROOT/src/infra/roles/precondition/tasks/tls_san.yaml"

TMP_ROOT="$(mktemp -d)"
cleanup() {
  rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

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

echo 'TLS SAN validation contract tests passed.'
