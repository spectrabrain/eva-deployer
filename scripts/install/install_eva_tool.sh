#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: sudo bash ./eva-tool-installer.sh [options]

Options:
  --artifact PATH             EVA Tool archive. Defaults to the single sibling
                              eva-tool_*_linux_amd64.tar.gz archive.
  --sha256 DIGEST             Expected archive SHA-256. Defaults to the matching
                              sibling checksums.sha256 entry.
  --root PATH                 EVA root. Default: /opt/eva.
  --bin-dir PATH              EVA command directory. Default: /usr/local/bin.
  --state-root PATH           State root. Default: /var/lib/eva.
  --log-root PATH             Log root. Default: /var/log/eva.
  --force                     Replace a non-EVA existing command link.
  -h, --help                  Show this help.
EOF
}

artifact=""
expected_sha256=""
eva_root="/opt/eva"
bin_dir="/usr/local/bin"
state_root="/var/lib/eva"
log_root="/var/log/eva"
force=false
installer_path="$(readlink -f "${BASH_SOURCE[0]}")"
script_dir="$(dirname "$installer_path")"
auto_artifact=false

while (($#)); do
  case "$1" in
    --artifact) artifact="${2:-}"; shift 2 ;;
    --sha256) expected_sha256="${2:-}"; shift 2 ;;
    --root) eva_root="${2:-}"; shift 2 ;;
    --bin-dir) bin_dir="${2:-}"; shift 2 ;;
    --state-root) state_root="${2:-}"; shift 2 ;;
    --log-root) log_root="${2:-}"; shift 2 ;;
    --force) force=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "[error] unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

for command in awk cp date dirname find flock grep install mkdir mktemp mv readlink sha256sum sleep sync tar; do
  command -v "$command" >/dev/null 2>&1 || { echo "[error] missing command: $command" >&2; exit 1; }
done
if [[ $EUID -ne 0 ]]; then
  echo "[error] system-wide installation requires root; use sudo" >&2
  exit 1
fi

# The installer itself is the release-context authority. Resolve its real
# parent once; a launcher symlink is fine, but the resulting Release root and
# its immutable descriptors must be regular filesystem objects.
release_root="$script_dir"
if [[ -L "$release_root" || ! -d "$release_root" ]]; then
  echo "[error] Release root is not a non-symlink directory: $release_root" >&2
  exit 1
fi
for release_file in release.yaml checksums.sha256; do
  if [[ ! -f "$release_root/$release_file" || -L "$release_root/$release_file" ]]; then
    echo "[error] Release is missing a regular $release_file: $release_root/$release_file" >&2
    exit 1
  fi
done
if [[ "$installer_path" != "$release_root/eva-tool-installer.sh" ]]; then
  echo "[error] installer must be eva-tool-installer.sh directly inside its Release root" >&2
  exit 1
fi
if ! awk '
  NF != 2 { exit 1 }
  $1 !~ /^[a-fA-F0-9]{64}$/ { exit 1 }
  {
    name = $2
    sub(/^\*/, "", name)
    if (name == "" || name ~ /^\// || name ~ /(^|\/)\.\.($|\/)/) exit 1
  }
  END { if (NR == 0) exit 1 }
' "$release_root/checksums.sha256"; then
  echo "[error] checksums.sha256 has an unsafe or invalid entry" >&2
  exit 1
fi
if ! (cd "$release_root" && sha256sum --check --strict checksums.sha256 >/dev/null); then
  echo "[error] Release checksum verification failed" >&2
  exit 1
fi
release_version="$(awk '/^version:[[:space:]]*/ { value=$0; sub(/^[^:]*:[[:space:]]*/, "", value); gsub(/[[:space:]]+$/, "", value); print value; exit }' "$release_root/release.yaml")"
if [[ ! "$release_version" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.]+)?$ ]]; then
  echo "[error] release.yaml has no valid semantic version" >&2
  exit 1
fi

shopt -s nullglob
tool_archives=("$script_dir"/eva-tool_*_linux_amd64.tar.gz)
if [[ ${#tool_archives[@]} -ne 1 ]]; then
  echo "[error] expected exactly one EVA Tool archive beside this installer; found ${#tool_archives[@]}" >&2
  exit 1
fi
if [[ -z "$artifact" ]]; then
  artifact="${tool_archives[0]}"
  auto_artifact=true
fi
if [[ -L "$artifact" ]]; then
  echo "[error] EVA Tool archive must not be a symlink: $artifact" >&2
  exit 1
fi
artifact="$(readlink -f "$artifact")"
if [[ ! -f "$artifact" ]]; then
  echo "[error] EVA Tool archive is not a regular file: $artifact" >&2
  exit 1
fi
if [[ "$(dirname "$artifact")" != "$release_root" ]]; then
  echo "[error] EVA Tool archive must be directly inside the installer Release root" >&2
  exit 1
fi
if [[ -z "$expected_sha256" && -f "$script_dir/checksums.sha256" ]]; then
  archive_name="${artifact##*/}"
  expected_sha256="$(awk -v file="$archive_name" '$2 == file || $2 == "*" file { print $1; exit }' "$script_dir/checksums.sha256")"
fi
if [[ "$auto_artifact" == true && -z "$expected_sha256" ]]; then
  echo "[error] checksums.sha256 has no digest for ${artifact##*/}" >&2
  echo "        use --sha256 DIGEST only when an external verified digest is available" >&2
  exit 1
fi
if [[ -n "$expected_sha256" ]]; then
  [[ "$expected_sha256" =~ ^[a-fA-F0-9]{64}$ ]] || { echo "[error] --sha256 must be a SHA-256 digest" >&2; exit 2; }
  actual_sha256="$(sha256sum "$artifact" | awk '{print $1}')"
  if [[ "${actual_sha256,,}" != "${expected_sha256,,}" ]]; then
    echo "[error] EVA Tool archive checksum mismatch" >&2
    exit 1
  fi
fi

if ! awk -v file="${artifact##*/}" '$2 == file || $2 == "*" file { matches += 1 } END { exit matches != 1 }' "$release_root/checksums.sha256"; then
  echo "[error] checksums.sha256 must contain exactly one EVA Tool archive digest" >&2
  exit 1
fi

remote_backend_paths=(
  scripts/download/build_nvidia_driver_repo.sh
  scripts/download/download_ansible_wheels.sh
  scripts/download/download_display_mode_selector.sh
  scripts/download/download_eva_images.sh
  scripts/download/download_eva_models.sh
  scripts/download/download_infra_images.sh
  scripts/download/download_n8n_images.sh
  scripts/download/download_offline_assets.sh
  scripts/download/download_python_venv_debs.sh
  scripts/download/download_qdrant_snapshots.sh
  scripts/lib/load_versions.sh
  scripts/publish/push_images_to_repository.sh
  scripts/publish/push_qdrant_snapshots_to_harbor.sh
  scripts/remote/publish_release_to_target.sh
  scripts/install/install_docker.sh
  scripts/install/setup_harbor.sh
  scripts/install/requirements-airgap.txt
  src/infra/version.yaml
  src/solution/version.yaml
)

remote_backend_shell_paths=(
  scripts/download/build_nvidia_driver_repo.sh
  scripts/download/download_ansible_wheels.sh
  scripts/download/download_display_mode_selector.sh
  scripts/download/download_eva_images.sh
  scripts/download/download_eva_models.sh
  scripts/download/download_infra_images.sh
  scripts/download/download_n8n_images.sh
  scripts/download/download_offline_assets.sh
  scripts/download/download_python_venv_debs.sh
  scripts/download/download_qdrant_snapshots.sh
  scripts/lib/load_versions.sh
  scripts/publish/push_images_to_repository.sh
  scripts/publish/push_qdrant_snapshots_to_harbor.sh
  scripts/remote/publish_release_to_target.sh
  scripts/install/install_docker.sh
  scripts/install/setup_harbor.sh
)

archive_contains_exactly_one() {
  local expected="$1" entry count=0
  for entry in "${archive_entries[@]}"; do
    [[ "$entry" == "$expected" ]] && ((count += 1))
  done
  [[ "$count" -eq 1 ]]
}

archive_member_is_executable() {
  local expected="$1"
  tar -tvzf "$artifact" | awk -v expected="$expected" '
    $NF == expected {
      matches += 1
      executable = (substr($1, 1, 1) == "-" && substr($1, 4, 1) == "x")
    }
    END { exit !(matches == 1 && executable) }
  '
}

validate_archive_layout() {
  local entry path type

  mapfile -t archive_entries < <(tar -tzf "$artifact")
  if ((${#archive_entries[@]} == 0)); then
    echo "[error] EVA Tool archive is empty" >&2
    exit 1
  fi
  for entry in "${archive_entries[@]}"; do
    if [[ -z "$entry" || "$entry" == /* || "/$entry/" == */../* ]]; then
      echo "[error] EVA Tool archive contains an unsafe path: $entry" >&2
      exit 1
    fi
  done
  if ! archive_contains_exactly_one "bin/eva"; then
    echo "[error] EVA Tool archive must contain exactly one bin/eva" >&2
    exit 1
  fi
  if ! archive_contains_exactly_one "libexec/remote-root/"; then
    echo "[error] EVA Tool archive must contain libexec/remote-root" >&2
    exit 1
  fi
  for path in "${remote_backend_paths[@]}"; do
    if ! archive_contains_exactly_one "libexec/remote-root/$path"; then
      echo "[error] EVA Tool archive is missing required Remote backend file: $path" >&2
      exit 1
    fi
  done
  if ! archive_member_is_executable "bin/eva"; then
    echo "[error] EVA Tool archive bin/eva must be an executable regular file" >&2
    exit 1
  fi
  for path in "${remote_backend_shell_paths[@]}"; do
    if ! archive_member_is_executable "libexec/remote-root/$path"; then
      echo "[error] EVA Tool archive backend script must be executable: $path" >&2
      exit 1
    fi
  done
  while IFS= read -r type; do
    case "${type:0:1}" in
      -|d) ;;
      *)
        echo "[error] EVA Tool archive contains a link or special file" >&2
        exit 1
        ;;
    esac
  done < <(tar -tvzf "$artifact")
}

validate_archive_layout

setup_directory() {
  local path="$1" mode="$2"
  install -d -o root -g root -m "$mode" "$path"
}

eva_root="$(mkdir -p "$eva_root" && cd "$eva_root" && pwd)"
bin_dir="$(mkdir -p "$bin_dir" && cd "$bin_dir" && pwd)"
state_root="$(mkdir -p "$state_root" && cd "$state_root" && pwd)"
log_root="$(mkdir -p "$log_root" && cd "$log_root" && pwd)"
setup_directory "$eva_root" 0755
setup_directory "$eva_root/runtime" 0755
setup_directory "$eva_root/releases" 0755
setup_directory "$state_root" 0700
setup_directory "$state_root/artifacts" 0700
setup_directory "$state_root/operations" 0700
setup_directory "$state_root/state" 0700
setup_directory "$log_root" 0700
setup_directory "$log_root/operations" 0700

lock_dir="$state_root/locks"
installer_lock="$lock_dir/tool-installer.lock"
if [[ -L "$lock_dir" || ( -e "$lock_dir" && ! -d "$lock_dir" ) ]]; then
  echo "[error] EVA Tool installer lock root is not a directory: $lock_dir" >&2
  exit 1
fi
setup_directory "$lock_dir" 0700
if [[ -L "$installer_lock" || ( -e "$installer_lock" && ! -f "$installer_lock" ) ]]; then
  echo "[error] EVA Tool installer lock is not a regular file: $installer_lock" >&2
  exit 1
fi
installer_lock_acquired=false

exec 9>"$installer_lock"
chmod 0600 "$installer_lock"
chown root:root "$installer_lock"

if ! flock -n 9; then
  echo "[error] another EVA Tool installer transaction is running" >&2
  exit 1
fi

installer_lock_acquired=true

tool_dir="$eva_root/tool"
tool_binary="$tool_dir/bin/eva"
command_link="$bin_dir/eva"
current_receipt="$state_root/releases/current.yaml"
if [[ -e "$command_link" || -L "$command_link" ]]; then
  existing_target="$(readlink -f "$command_link" 2>/dev/null || true)"
  if [[ "$existing_target" != "$tool_binary" && "$force" != true ]]; then
    echo "[error] existing command is not managed by EVA: $command_link (use --force to replace)" >&2
    exit 1
  fi
fi

staging_dir="$(mktemp -d "$eva_root/.eva-tool-XXXXXX")"
transaction_started=false
old_tool_backed_up=false
new_tool_published=false
old_link_backed_up=false
new_link_published=false
receipt_registration_started=false
transaction_committed=false
rollback_started=false
rollback_completed=false
old_tool_backup_dir=""
old_tool_backup=""
old_link_backup=""
receipt_backup_dir=""
receipt_backup=""
had_current_receipt=false
temporary_link=""

release_installer_lock() {
  local unlock_error=0

  if [[ "$installer_lock_acquired" != true ]]; then
    return 0
  fi

  if ! flock -u 9; then
    echo "[error] could not release EVA Tool installer lock" >&2
    unlock_error=1
  fi

  exec 9>&-
  installer_lock_acquired=false

  return "$unlock_error"
}

cleanup() {
  if [[ -n "$temporary_link" ]]; then rm -f "$temporary_link" || true; fi
  if [[ -d "$staging_dir" ]]; then rm -rf "$staging_dir" || true; fi
  if [[ -d "$old_tool_backup_dir" ]]; then rm -rf "$old_tool_backup_dir" || true; fi
  if [[ -d "$receipt_backup_dir" ]]; then rm -rf "$receipt_backup_dir" || true; fi
  if [[ -n "$old_link_backup" ]]; then rm -f "$old_link_backup" || true; fi
}

test_hook() {
  local stage="$1" marker continue_marker
  if [[ -n "${EVA_INSTALLER_TEST_FAIL_AFTER:-}" && "${EVA_INSTALLER_TEST_FAIL_AFTER}" != tool-published && "${EVA_INSTALLER_TEST_FAIL_AFTER}" != link-published && "${EVA_INSTALLER_TEST_FAIL_AFTER}" != register-current && "${EVA_INSTALLER_TEST_FAIL_AFTER}" != receipt-published && "${EVA_INSTALLER_TEST_FAIL_AFTER}" != commit ]]; then
    echo "[error] invalid EVA_INSTALLER_TEST_FAIL_AFTER stage" >&2
    return 1
  fi
  if [[ "${EVA_INSTALLER_TEST_FAIL_AFTER:-}" == "$stage" ]]; then
    echo "[error] installer test failure after $stage" >&2
    return 1
  fi
  if [[ "${EVA_INSTALLER_TEST_WAIT_AFTER:-}" == "$stage" ]]; then
    [[ -n "${EVA_INSTALLER_TEST_HOOK_DIR:-}" ]] || return 1
    marker="$EVA_INSTALLER_TEST_HOOK_DIR/$stage.reached"
    continue_marker="$EVA_INSTALLER_TEST_HOOK_DIR/$stage.continue"
    mkdir -p "$EVA_INSTALLER_TEST_HOOK_DIR"
    : > "$marker"
    while [[ ! -e "$continue_marker" ]]; do
      sleep 0.05
    done
  fi
}

restore_receipt() {
  local receipt_error=0
  if [[ "$receipt_registration_started" != true ]]; then
    return 0
  fi
  if [[ "$had_current_receipt" == true ]]; then
    if ! EVA_INTERNAL_CURRENT_RELEASE_RECEIPT_PATH="$current_receipt" "$tool_binary" internal restore-current-release --backup "$receipt_backup"; then
      echo "[error] rollback could not restore Current Release receipt" >&2
      receipt_error=1
    fi
  elif ! EVA_INTERNAL_CURRENT_RELEASE_RECEIPT_PATH="$current_receipt" "$tool_binary" internal clear-current-release; then
    echo "[error] rollback could not remove Current Release receipt" >&2
    receipt_error=1
  fi
  return "$receipt_error"
}

rollback_installation() {
  local rollback_error=0
  restore_receipt || rollback_error=1
  if [[ "$new_link_published" == true ]] && ! rm -f "$command_link"; then
    echo "[error] rollback could not remove new EVA command link" >&2
    rollback_error=1
  fi
  if [[ "$old_link_backed_up" == true ]] && ! mv "$old_link_backup" "$command_link"; then
    echo "[error] rollback could not restore previous EVA command link" >&2
    rollback_error=1
  fi
  if [[ "$new_tool_published" == true ]] && ! rm -rf "$tool_dir"; then
    echo "[error] rollback could not remove new EVA Tool" >&2
    rollback_error=1
  fi
  if [[ "$old_tool_backed_up" == true ]] && ! mv "$old_tool_backup" "$tool_dir"; then
    echo "[error] rollback could not restore previous EVA Tool" >&2
    rollback_error=1
  fi
  return "$rollback_error"
}

fail_transaction() {
  local message="$1"
  echo "[error] $message" >&2
  exit 1
}

on_exit() {
  local status=$?
  trap - EXIT INT TERM HUP
  if [[ "$transaction_started" == true && "$transaction_committed" != true && "$rollback_started" != true ]]; then
    rollback_started=true
    if rollback_installation; then
      rollback_completed=true
    fi
  fi
  if [[ "$rollback_started" == true && "$rollback_completed" != true ]]; then
    echo "[error] installer rollback was incomplete" >&2
  fi
  cleanup

  if ! release_installer_lock; then
    echo "[error] EVA Tool installer lock release was incomplete" >&2
  fi

  return "$status"
}

on_signal() {
  local signal_name="$1" status="$2"
  echo "[error] EVA Tool installer interrupted by $signal_name" >&2
  exit "$status"
}

trap on_exit EXIT
trap 'on_signal INT 130' INT
trap 'on_signal TERM 143' TERM
trap 'on_signal HUP 129' HUP
mkdir -p "$staging_dir/bin"
tar -xzf "$artifact" -C "$staging_dir" --no-same-owner --no-same-permissions
if find "$staging_dir" -xdev \( -type l -o -type b -o -type c -o -type p -o -type s \) -print -quit | grep -q .; then
  echo "[error] extracted EVA Tool contains a link or special file" >&2
  exit 1
fi
if find "$staging_dir" -xdev -type f -links +1 -print -quit | grep -q .; then
  echo "[error] extracted EVA Tool contains a hard-linked file" >&2
  exit 1
fi
if find "$staging_dir" -xdev ! -type f ! -type d -print -quit | grep -q .; then
  echo "[error] extracted EVA Tool contains an unsupported file type" >&2
  exit 1
fi
if [[ ! -f "$staging_dir/bin/eva" || -L "$staging_dir/bin/eva" ]]; then
  echo "[error] extracted EVA Tool is not an executable regular file" >&2
  exit 1
fi
for path in "${remote_backend_paths[@]}"; do
  if [[ ! -f "$staging_dir/libexec/remote-root/$path" || -L "$staging_dir/libexec/remote-root/$path" ]]; then
    echo "[error] extracted EVA Tool is missing required Remote backend file: $path" >&2
    exit 1
  fi
done
find "$staging_dir" -xdev -type d -exec chmod 0755 {} +
find "$staging_dir" -xdev -type f -exec chmod 0644 {} +
chmod 0755 "$staging_dir/bin/eva"
for path in "${remote_backend_shell_paths[@]}"; do
  chmod 0755 "$staging_dir/libexec/remote-root/$path"
done
chown -R root:root "$staging_dir"
if [[ ! -x "$staging_dir/bin/eva" ]]; then
  echo "[error] extracted EVA Tool is not an executable regular file" >&2
  exit 1
fi
for path in "${remote_backend_shell_paths[@]}"; do
  if [[ ! -x "$staging_dir/libexec/remote-root/$path" ]]; then
    echo "[error] extracted Remote backend script is not executable: $path" >&2
    exit 1
  fi
done
staged_tool_version="$("$staging_dir/bin/eva" version | awk 'NR == 1 { print $1; exit }')"
if [[ "$staged_tool_version" != "$release_version" ]]; then
  echo "[error] EVA Tool version $staged_tool_version does not match Release version $release_version" >&2
  exit 1
fi
if ! "$staging_dir/bin/eva" verify "$release_root" >/dev/null; then
  echo "[error] EVA Tool could not validate its installer Release" >&2
  exit 1
fi

transaction_started=true
if [[ -e "$current_receipt" || -L "$current_receipt" ]]; then
  if [[ -L "$current_receipt" || ! -f "$current_receipt" ]]; then
    echo "[error] existing Current Release receipt is not a regular file" >&2
    exit 1
  fi
  receipt_backup_dir="$(mktemp -d "$eva_root/.eva-current-receipt-XXXXXX")"
  receipt_backup="$receipt_backup_dir/current.yaml"
  if ! cp -p "$current_receipt" "$receipt_backup"; then
    echo "[error] could not back up Current Release receipt" >&2
    exit 1
  fi
  if ! EVA_INTERNAL_CURRENT_RELEASE_RECEIPT_PATH="$receipt_backup" "$staging_dir/bin/eva" internal validate-current-release >/dev/null 2>&1; then
    echo "[error] existing Current Release receipt cannot be validated" >&2
    exit 1
  fi
  had_current_receipt=true
fi

if [[ -e "$tool_dir" || -L "$tool_dir" ]]; then
  if [[ -L "$tool_dir" || ! -d "$tool_dir" ]]; then
    echo "[error] existing EVA tool path is not a directory: $tool_dir" >&2
    exit 1
  fi
  old_tool_backup_dir="$(mktemp -d "$eva_root/.eva-tool-backup-XXXXXX")"
  old_tool_backup="$old_tool_backup_dir/tool"
  if ! mv "$tool_dir" "$old_tool_backup"; then
    echo "[error] could not back up existing EVA Tool" >&2
    exit 1
  fi
  old_tool_backed_up=true
fi
if [[ -e "$command_link" || -L "$command_link" ]]; then
  old_link_backup="$(mktemp "$bin_dir/.eva-backup-XXXXXX")"
  rm -f "$old_link_backup"
  if ! mv "$command_link" "$old_link_backup"; then
    fail_transaction "could not back up existing EVA command link"
  fi
  old_link_backed_up=true
fi
if ! mv "$staging_dir" "$tool_dir"; then
  fail_transaction "could not publish EVA Tool"
fi
new_tool_published=true
if ! test_hook tool-published; then
  exit 1
fi

temporary_link="$bin_dir/.eva-$RANDOM"
if ! ln -s "$tool_binary" "$temporary_link" || ! mv -Tf "$temporary_link" "$command_link"; then
  fail_transaction "could not publish EVA command link"
fi
temporary_link=""
new_link_published=true
if ! test_hook link-published; then
  exit 1
fi

if ! installed_tool_version="$("$command_link" version | awk 'NR == 1 { print $1; exit }')" || [[ "$installed_tool_version" != "$release_version" ]]; then
  fail_transaction "installed EVA Tool version does not match Release version"
fi

receipt_registration_started=true
if ! test_hook register-current || ! EVA_INTERNAL_CURRENT_RELEASE_RECEIPT_PATH="$current_receipt" "$tool_binary" internal register-current-release --release "$release_root" --selected-by eva-tool-installer; then
  fail_transaction "could not register Current Release"
fi
if ! test_hook receipt-published; then
  exit 1
fi
if ! EVA_INTERNAL_CURRENT_RELEASE_RECEIPT_PATH="$current_receipt" "$tool_binary" internal validate-current-release --release "$release_root"; then
  fail_transaction "could not validate Current Release"
fi
if ! installed_tool_version="$("$command_link" version | awk 'NR == 1 { print $1; exit }')" || [[ "$installed_tool_version" != "$release_version" ]]; then
  fail_transaction "installed EVA Tool and Current Release versions do not match"
fi

transaction_committed=true
if [[ "$transaction_committed" != true ]]; then
  fail_transaction "installer transaction did not commit"
fi
if ! test_hook commit; then
  exit 1
fi
rm -rf "$old_tool_backup_dir" "$receipt_backup_dir"
old_tool_backup_dir=""
receipt_backup_dir=""
rm -f "$old_link_backup"
old_link_backup=""

echo "[done] EVA Tool installed"
if resolved_command="$(command -v eva 2>/dev/null)"; then
  echo "[done] eva command: $resolved_command"
else
  echo "[done] eva command: $command_link"
fi
echo "[done] current release: $release_version"
echo "[done] release root: $release_root"
