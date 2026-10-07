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
  '--sudo-mode'
  '--sudo-password-fd'
  'sudo -S -p'
  'sudo -n $remote_command'
  'printf -v remote_command'
  'if [[ "$sudo_mode" != none ]]'
  'sudo_mode="$5"'
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
  '"$payload_entry" != cache/*'
  '"$payload_entry" == cache/images/*'
  '"$payload_entry" == cache/qdrant-snapshots/*'
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

work_root="$(mktemp -d)"
trap 'rm -rf "$work_root"' EXIT
fail() {
  echo "[ERROR] $*" >&2
  exit 1
}

# Manifests are written by the EVA Tool with yaml.v3, which indents nested
# mappings by four spaces. The reader must not assume a width, and each key
# must come from its own section (release and runtime both carry version).
# shellcheck source=/dev/null
source <(sed -n '/^manifest_value() {$/,/^}$/p' "$transport")
digest_a="$(printf 'a%.0s' {1..64})"
digest_d="$(printf 'd%.0s' {1..64})"
for indent in '    ' '  '; do
  manifest="$work_root/manifest-${#indent}.yaml"
  {
    echo 'schema_version: v1'
    echo 'release:'
    echo "${indent}version: 0.1.0-ci.104"
    echo "${indent}platform: linux/amd64"
    echo "${indent}release_yaml_sha256: $digest_a"
    echo 'repository:'
    echo "${indent}registry: 10.159.57.172:32080"
    echo "${indent}project: eva"
    echo 'runtime:'
    echo "${indent}version: 1.0.1"
    echo "${indent}platform: linux/amd64"
    echo "${indent}archive: eva-runtime_1.0.1_linux_amd64.tar.gz"
    echo "${indent}descriptor_sha256: $digest_d"
  } >"$manifest"
  [[ "$(manifest_value "$manifest" "" schema_version)" == v1 ]] || fail "top-level field (indent ${#indent})"
  [[ "$(manifest_value "$manifest" release version)" == 0.1.0-ci.104 ]] || fail "release version (indent ${#indent})"
  [[ "$(manifest_value "$manifest" runtime version)" == 1.0.1 ]] || fail "runtime version (indent ${#indent})"
  [[ "$(manifest_value "$manifest" runtime archive)" == eva-runtime_1.0.1_linux_amd64.tar.gz ]] || fail "runtime archive (indent ${#indent})"
  [[ "$(manifest_value "$manifest" repository registry)" == 10.159.57.172:32080 ]] || fail "repository registry (indent ${#indent})"
  [[ "$(manifest_value "$manifest" release release_yaml_sha256)" == "$digest_a" ]] || fail "release digest (indent ${#indent})"
  [[ "$(manifest_value "$manifest" runtime descriptor_sha256)" == "$digest_d" ]] || fail "runtime digest (indent ${#indent})"
  [[ -z "$(manifest_value "$manifest" repository version)" ]] || fail "key leaked across sections (indent ${#indent})"
done
echo '[OK] transport manifest reader accepts EVA Tool manifests'

# The Runtime archive allowlist must match the EVA Tool builder: Ansible
# collections ship, and third-party module names such as secretsmanager_secret
# are legitimate there, while unsafe paths and secret-named bin entries are not.
# shellcheck source=/dev/null
source <(sed -n '/^runtime_archive_entry_is_safe() {$/,/^}$/p' "$transport")
for entry in runtime/ runtime/runtime.yaml runtime/bin/kubectl runtime/venv/bin/ansible-playbook \
  runtime/collections/ansible_collections/ansible/posix/MANIFEST.json \
  runtime/venv/lib/python3/site-packages/ansible_collections/amazon/aws/plugins/lookup/secretsmanager_secret.py; do
  runtime_archive_entry_is_safe "$entry" || fail "Runtime archive entry rejected: $entry"
done
for entry in '' /etc/passwd runtime/../escape runtime/extra other/file runtime/bin/aws_token runtime/bin/credentials; do
  if runtime_archive_entry_is_safe "$entry"; then fail "Runtime archive entry accepted: '$entry'"; fi
done
echo '[OK] transport Runtime archive allowlist matches the EVA Tool builder'

# ssh joins remote arguments with spaces. The remote root command must survive
# that join: an empty sudo prompt and every script argument must arrive intact.
bin="$work_root/bin"
mkdir -p "$bin"
cat >"$bin/fake-ssh" <<'STUB'
#!/usr/bin/env bash
while [[ "$1" != eva@target ]]; do shift; done
shift
exec sh -c "$*"
STUB
cat >"$bin/sudo" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == -n ]]; then shift; exec "$@"; fi
[[ "$1" == -S && "$2" == -p && "$3" == '' ]] || { echo "sudo usage" >&2; exit 1; }
shift 3
IFS= read -r password
[[ "$password" == "$EXPECTED_SUDO_PASSWORD" ]] || exit 1
exec "$@"
STUB
chmod +x "$bin/fake-ssh" "$bin/sudo"
# shellcheck source=/dev/null
source <(sed -n '/^run_remote_root_script() {$/,/^}$/p' "$transport")
# These are the globals run_remote_root_script reads.
# shellcheck disable=SC2034
SSH_CMD="$bin/fake-ssh"
# shellcheck disable=SC2034
ssh_options=(-o BatchMode=yes)
# shellcheck disable=SC2034
target=eva@target
export EXPECTED_SUDO_PASSWORD="it's \"secret\""
# shellcheck disable=SC2034
sudo_password="$EXPECTED_SUDO_PASSWORD"
expected="2|/var/lib/eva/inbox/releases/.incoming-x|it's two words"$'\n'"manifest_value"$'\n'"runtime_archive_entry_is_safe"
for sudo_mode in password passwordless none; do
  # shellcheck disable=SC2016 # The remote script expands its own arguments.
  output="$(PATH="$bin:$PATH" run_remote_root_script "/var/lib/eva/inbox/releases/.incoming-x" "it's two words" <<<'printf "%s|%s|%s\n" "$#" "$1" "$2"; declare -F manifest_value runtime_archive_entry_is_safe')" ||
    fail "remote root script failed with sudo mode $sudo_mode"
  [[ "$output" == "$expected" ]] || fail "remote root script arguments differ with sudo mode $sudo_mode: $output"
done
echo '[OK] transport remote root command survives ssh argument joining'

echo '[OK] Remote Release transport contract tests passed'
