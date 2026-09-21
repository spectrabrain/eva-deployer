#!/usr/bin/env bash
set -euo pipefail

repo_root="$(
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." &&
    pwd
)"

transport="$repo_root/scripts/remote/publish_release_to_target.sh"

if [[ ! -f "$transport" ]]; then
  echo "[ERROR] Remote Release transport script is missing: $transport" >&2
  exit 1
fi

if [[ ! -x "$transport" ]]; then
  echo "[ERROR] Remote Release transport script is not executable" >&2
  exit 1
fi

bash -n "$transport"

# shellcheck disable=SC2016 # Literal shell text is the contract under test.
required_contracts=(
  '--release-dir'
	'--payload-dir'
  '--runtime-dir'
  '--target'
  '/var/lib/eva/inbox/releases'
  'sha256sum --check --strict checksums.sha256'
  '.eva-remote-release'
  '.incoming-'
  'mv -- "$target_staging" "$target_final"'
  'Remote Release already published'
  'different Remote Release already exists'
  'Release directory contains a symbolic link'
  'transferred Release contains a symbolic link'
	'transferred target payload identity does not match Release'
  'target payload archive contains a link or special file'
	'target payload has an unexpected file'
  'payload_manifest_sha256'
  'runtime_manifest_sha256'
  'remote-runtime'
  'Runtime artifact'
)

for required_contract in "${required_contracts[@]}"; do
  if grep -Fq -- "$required_contract" "$transport"; then
    echo "[OK] transport contract: $required_contract"
  else
    echo "[ERROR] missing transport contract: $required_contract" >&2
    exit 1
  fi
done

for forbidden_contract in \
  'aws_key.ini' \
  'credentials/aws' \
  'site-values/' \
  'inventory.ini'; do
  if grep -Fq -- "$forbidden_contract" "$transport"; then
    echo "[ERROR] transport includes forbidden site or credential input: $forbidden_contract" >&2
    exit 1
  fi
done

# shellcheck disable=SC2016 # The expression intentionally matches literal script text.
if grep -Eq 'rm[[:space:]]+-rf[[:space:]]+--?[[:space:]]*"\$target_final"' "$transport"; then
  echo "[ERROR] transport destructively removes an existing published Release" >&2
  exit 1
fi

echo '[OK] Remote Release transport contract tests passed'
