#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: sudo bash ./eva-tool-installer_vX.Y.Z.sh --artifact PATH [options]

Options:
  --artifact PATH             Required eva-tool archive.
  --sha256 DIGEST             Optional expected SHA-256 for the archive.
  --operator USER             Add this user to eva-operators. Defaults to SUDO_USER.
  --group NAME                Operator group. Default: eva-operators.
  --root PATH                 EVA root. Default: /opt/eva.
  --bin-dir PATH              EVA command directory. Default: /usr/local/bin.
  --state-root PATH           State root. Default: /var/lib/eva.
  --log-root PATH             Log root. Default: /var/log/eva.
  --force                     Replace a non-EVA existing command link.
  --skip-group-management     Do not create/change groups or ownership. Test only.
  -h, --help                  Show this help.
EOF
}

artifact=""
expected_sha256=""
operator="${SUDO_USER:-}"
operator_group="eva-operators"
eva_root="/opt/eva"
bin_dir="/usr/local/bin"
state_root="/var/lib/eva"
log_root="/var/log/eva"
force=false
skip_group_management=false

while (($#)); do
  case "$1" in
    --artifact) artifact="${2:-}"; shift 2 ;;
    --sha256) expected_sha256="${2:-}"; shift 2 ;;
    --operator) operator="${2:-}"; shift 2 ;;
    --group) operator_group="${2:-}"; shift 2 ;;
    --root) eva_root="${2:-}"; shift 2 ;;
    --bin-dir) bin_dir="${2:-}"; shift 2 ;;
    --state-root) state_root="${2:-}"; shift 2 ;;
    --log-root) log_root="${2:-}"; shift 2 ;;
    --force) force=true; shift ;;
    --skip-group-management) skip_group_management=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "[error] unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ -z "$artifact" ]]; then
  echo "[error] --artifact is required" >&2
  exit 2
fi
for command in awk install mktemp mv readlink sha256sum tar; do
  command -v "$command" >/dev/null 2>&1 || { echo "[error] missing command: $command" >&2; exit 1; }
done
if [[ "$skip_group_management" != true && $EUID -ne 0 ]]; then
  echo "[error] system-wide installation requires root; use sudo" >&2
  exit 1
fi

artifact="$(readlink -f "$artifact")"
if [[ ! -f "$artifact" ]]; then
  echo "[error] EVA Tool archive is not a regular file: $artifact" >&2
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

mapfile -t archive_entries < <(tar -tzf "$artifact")
if [[ ${#archive_entries[@]} -ne 1 || "${archive_entries[0]}" != "bin/eva" ]]; then
  echo "[error] EVA Tool archive must contain exactly bin/eva" >&2
  exit 1
fi
if ! tar -tvzf "$artifact" | awk 'NR == 1 { valid = ($1 ~ /^-/ && $NF == "bin/eva") } END { exit !(NR == 1 && valid) }'; then
  echo "[error] EVA Tool archive bin/eva must be a regular file" >&2
  exit 1
fi

if [[ "$skip_group_management" != true ]]; then
  getent group "$operator_group" >/dev/null || groupadd --system "$operator_group"
  if [[ -n "$operator" && "$operator" != root ]]; then
    id "$operator" >/dev/null
    usermod -aG "$operator_group" "$operator"
  fi
fi

setup_directory() {
  local path="$1" mode="$2"
  if [[ "$skip_group_management" == true ]]; then
    install -d -m "$mode" "$path"
  else
    install -d -o root -g "$operator_group" -m "$mode" "$path"
  fi
}

eva_root="$(mkdir -p "$eva_root" && cd "$eva_root" && pwd)"
bin_dir="$(mkdir -p "$bin_dir" && cd "$bin_dir" && pwd)"
state_root="$(mkdir -p "$state_root" && cd "$state_root" && pwd)"
log_root="$(mkdir -p "$log_root" && cd "$log_root" && pwd)"
setup_directory "$eva_root" 2775
setup_directory "$eva_root/runtime" 2775
setup_directory "$eva_root/releases" 2775
setup_directory "$state_root" 2770
setup_directory "$state_root/artifacts" 2770
setup_directory "$state_root/operations" 2770
setup_directory "$state_root/state" 2770
setup_directory "$log_root" 2770
setup_directory "$log_root/operations" 2770

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
if [[ ! -f "$staging_dir/bin/eva" || -L "$staging_dir/bin/eva" || ! -x "$staging_dir/bin/eva" ]]; then
  echo "[error] extracted EVA Tool is not an executable regular file" >&2
  exit 1
fi
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

echo "[done] EVA Tool installed: $command_link"
"$command_link" version
if [[ -n "$operator" && "$operator" != root && "$skip_group_management" != true ]]; then
  echo "[info] $operator was added to $operator_group; start a new login session before using EVA."
fi
