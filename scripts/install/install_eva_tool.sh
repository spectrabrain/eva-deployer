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
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
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

for command in awk find grep install mktemp mv readlink sha256sum tar; do
  command -v "$command" >/dev/null 2>&1 || { echo "[error] missing command: $command" >&2; exit 1; }
done
if [[ $EUID -ne 0 ]]; then
  echo "[error] system-wide installation requires root; use sudo" >&2
  exit 1
fi

if [[ -z "$artifact" ]]; then
  shopt -s nullglob
  tool_archives=("$script_dir"/eva-tool_*_linux_amd64.tar.gz)
  if [[ ${#tool_archives[@]} -ne 1 ]]; then
    echo "[error] expected exactly one EVA Tool archive beside this installer; found ${#tool_archives[@]}" >&2
    echo "        use --artifact PATH to select the archive explicitly" >&2
    exit 1
  fi
  artifact="${tool_archives[0]}"
  auto_artifact=true
fi
artifact="$(readlink -f "$artifact")"
if [[ ! -f "$artifact" ]]; then
  echo "[error] EVA Tool archive is not a regular file: $artifact" >&2
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

tool_dir="$eva_root/tool"
tool_binary="$tool_dir/bin/eva"
command_link="$bin_dir/eva"
if [[ -e "$command_link" || -L "$command_link" ]]; then
  existing_target="$(readlink -f "$command_link" 2>/dev/null || true)"
  if [[ "$existing_target" != "$tool_binary" && "$force" != true ]]; then
    echo "[error] existing command is not managed by EVA: $command_link (use --force to replace)" >&2
    exit 1
  fi
fi

staging_dir="$(mktemp -d "$eva_root/.eva-tool-XXXXXX")"
backup_dir=""
cleanup() {
  if [[ -d "$staging_dir" ]]; then
    rm -rf "$staging_dir"
  fi
}
trap cleanup EXIT
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
"$staging_dir/bin/eva" version >/dev/null

if [[ -e "$tool_dir" || -L "$tool_dir" ]]; then
  if [[ -L "$tool_dir" || ! -d "$tool_dir" ]]; then
    echo "[error] existing EVA tool path is not a directory: $tool_dir" >&2
    exit 1
  fi
  backup_dir="$eva_root/.eva-tool-backup-$(date +%s)"
  mv "$tool_dir" "$backup_dir"
fi
if ! mv "$staging_dir" "$tool_dir"; then
  [[ -n "$backup_dir" ]] && mv "$backup_dir" "$tool_dir"
  echo "[error] could not publish EVA Tool" >&2
  exit 1
fi

temporary_link="$bin_dir/.eva-$RANDOM"
ln -s "$tool_binary" "$temporary_link"
if ! mv -Tf "$temporary_link" "$command_link"; then
  rm -f "$temporary_link"
  rm -rf "$tool_dir"
  [[ -n "$backup_dir" ]] && mv "$backup_dir" "$tool_dir"
  echo "[error] could not publish EVA command link" >&2
  exit 1
fi
if [[ -n "$backup_dir" ]]; then
  rm -rf "$backup_dir"
fi

echo "[done] EVA Tool installed"
if resolved_command="$(command -v eva 2>/dev/null)"; then
  echo "[done] eva command: $resolved_command"
else
  echo "[done] eva command: $command_link"
fi
"$command_link" version
