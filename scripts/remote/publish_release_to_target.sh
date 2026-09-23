#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage:
  scripts/remote/publish_release_to_target.sh \
    --release-dir PATH \
    --runtime-dir PATH \
    --payload-dir PATH \
    --target HOST \
    [--target-root PATH] \
    [--ssh-option OPTION] \
    [--sudo-mode none|password|passwordless] \
    [--sudo-password-fd FD]

Publishes a verified original EVA Release directory to a Remote Target.

Required release contents:
  release.yaml
  checksums.sha256
  eva-tool artifact
  eva-infra artifact
  eva-solution artifact
  (Remote Runtime and Target payload are supplied separately)

Default target root:
  /var/lib/eva/inbox/releases

Environment:
  SSH_CMD       SSH executable. Default: ssh
  RSYNC_CMD     rsync executable. Default: rsync
USAGE
}

release_dir=""
payload_dir=""
runtime_dir=""
target=""
target_root="/var/lib/eva/inbox/releases"
ssh_options=()
sudo_mode="none"
sudo_password_fd=""
sudo_password=""
remote_staging_prepared=false
remote_publish_committed=false

while (($#)); do
  case "$1" in
    --release-dir)
      release_dir="${2:-}"
      shift 2
      ;;
    --payload-dir)
      payload_dir="${2:-}"
      shift 2
      ;;
    --runtime-dir)
      runtime_dir="${2:-}"
      shift 2
      ;;
    --target)
      target="${2:-}"
      shift 2
      ;;
    --target-root)
      target_root="${2:-}"
      shift 2
      ;;
    --ssh-option)
      ssh_options+=("${2:-}")
      shift 2
      ;;
    --sudo-mode)
      sudo_mode="${2:-}"
      shift 2
      ;;
    --sudo-password-fd)
      sudo_password_fd="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "[ERROR] unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$release_dir" ]]; then
  echo "[ERROR] --release-dir is required" >&2
  exit 2
fi

if [[ -z "$payload_dir" ]]; then
  echo "[ERROR] --payload-dir is required" >&2
  exit 2
fi
if [[ -z "$runtime_dir" ]]; then
  echo "[ERROR] --runtime-dir is required" >&2
  exit 2
fi

if [[ -z "$target" ]]; then
  echo "[ERROR] --target is required" >&2
  exit 2
fi

if [[ "$target" == -* || "$target" =~ [[:space:]] ]]; then
  echo "[ERROR] invalid target: $target" >&2
  exit 2
fi

if [[ "$target_root" != /* || "$target_root" == "/" ]]; then
  echo "[ERROR] --target-root must be an absolute non-root path" >&2
  exit 2
fi

case "$sudo_mode" in
  none|password|passwordless)
    ;;
  *)
    echo "[ERROR] invalid --sudo-mode: $sudo_mode" >&2
    exit 2
    ;;
esac

if [[ "$sudo_mode" == password ]]; then
  if [[ ! "$sudo_password_fd" =~ ^[0-9]+$ ]] ||
    [[ "$sudo_password_fd" -lt 3 ]]
  then
    echo "[ERROR] password sudo requires --sudo-password-fd" >&2
    exit 2
  fi

  if ! IFS= read -r sudo_password <&"$sudo_password_fd"; then
    echo "[ERROR] Target sudo credential is unavailable" >&2
    exit 1
  fi

  if [[ -z "$sudo_password" ]]; then
    echo "[ERROR] Target sudo credential is unavailable" >&2
    exit 1
  fi
elif [[ -n "$sudo_password_fd" ]]; then
  echo "[ERROR] --sudo-password-fd requires password sudo mode" >&2
  exit 2
fi

SSH_CMD="${SSH_CMD:-ssh}"
RSYNC_CMD="${RSYNC_CMD:-rsync}"

command -v "$SSH_CMD" >/dev/null 2>&1 || {
  echo "[ERROR] SSH command not found: $SSH_CMD" >&2
  exit 1
}

command -v "$RSYNC_CMD" >/dev/null 2>&1 || {
  echo "[ERROR] rsync command not found: $RSYNC_CMD" >&2
  exit 1
}

release_dir="$(cd "$release_dir" && pwd)"
payload_dir="$(cd "$payload_dir" && pwd)"
runtime_dir="$(cd "$runtime_dir" && pwd)"

validate_runtime_artifact() {
  local directory="$1" prefix="$2" archive schema version platform release_version registry project descriptor_sha256 actual_descriptor_sha256
  for required_path in "$directory/manifest.yaml" "$directory/checksums.sha256"; do
    if [[ ! -f "$required_path" || -L "$required_path" ]]; then
      echo "[ERROR] ${prefix} Runtime artifact file is invalid: $required_path" >&2
      return 1
    fi
  done
  if find "$directory" -type l -print -quit | grep -q .; then
    echo "[ERROR] ${prefix} Runtime artifact contains a symbolic link" >&2; return 1
  fi
  archive="$(awk '/^  archive:[[:space:]]*/ { print $2; exit }' "$directory/manifest.yaml")"
  schema="$(awk '/^schema_version:[[:space:]]*/ { print $2; exit }' "$directory/manifest.yaml")"
  version="$(awk '/^  version:[[:space:]]*/ { print $2; exit }' "$directory/manifest.yaml")"
  platform="$(awk '/^  platform:[[:space:]]*/ { print $2; exit }' "$directory/manifest.yaml")"
  release_version="$(awk '/^release:/{ x=1; next } /^[^ ]/{x=0} x && /^  version:/{print $2; exit}' "$directory/manifest.yaml")"
  registry="$(awk '/^repository:/{ x=1; next } /^[^ ]/{x=0} x && /^  registry:/{print $2; exit}' "$directory/manifest.yaml")"
  project="$(awk '/^repository:/{ x=1; next } /^[^ ]/{x=0} x && /^  project:/{print $2; exit}' "$directory/manifest.yaml")"
  if [[ "$schema" != v1 || -z "$version" || "$platform" != linux/amd64 || -z "$release_version" || -z "$registry" || -z "$project" || ! "$archive" =~ ^[A-Za-z0-9._-]+$ || ! -f "$directory/$archive" || -L "$directory/$archive" ]]; then
    echo "[ERROR] ${prefix} Runtime artifact manifest is invalid" >&2; return 1
  fi
  if [[ "$(find "$directory" -mindepth 1 -maxdepth 1 -printf '%f\n' | wc -l)" -ne 3 ]] || ! (cd "$directory" && sha256sum --check --strict checksums.sha256 >/dev/null); then
    echo "[ERROR] ${prefix} Runtime artifact checksum validation failed" >&2; return 1
  fi
  if [[ "$(wc -l < "$directory/checksums.sha256")" -ne 1 ]]; then
    echo "[ERROR] ${prefix} Runtime artifact checksum manifest is invalid" >&2; return 1
  fi
  descriptor_sha256="$(awk '/^  descriptor_sha256:[[:space:]]*/ { print $2; exit }' "$directory/manifest.yaml")"
  actual_descriptor_sha256="$(tar -xOzf "$directory/$archive" runtime/runtime.yaml 2>/dev/null | sha256sum | awk '{print $1}')"
  if [[ ! "$descriptor_sha256" =~ ^[a-f0-9]{64}$ || "$descriptor_sha256" != "$actual_descriptor_sha256" ]]; then
    echo "[ERROR] ${prefix} Runtime descriptor digest is invalid" >&2; return 1
  fi
  while IFS= read -r entry; do
    case "$entry" in runtime|runtime/runtime.yaml|runtime/bin|runtime/bin/*|runtime/venv|runtime/venv/*) ;; *) echo "[ERROR] unsafe Runtime archive entry: $entry" >&2; return 1 ;; esac
    if [[ -z "$entry" || "$entry" == /* || "/$entry/" == */../* || "$entry" == *secret* || "$entry" == *credential* || "$entry" == *token* ]]; then
      echo "[ERROR] unsafe Runtime archive entry: $entry" >&2; return 1
    fi
  done < <(tar -tzf "$directory/$archive")
  if tar -tvzf "$directory/$archive" | awk '$1 !~ /^[-d]/ { exit 1 }'; then :; else
    echo "[ERROR] Runtime artifact contains a link or special file" >&2; return 1
  fi
}

validate_runtime_artifact "$runtime_dir" "source"
runtime_manifest_sha256="$(sha256sum "$runtime_dir/manifest.yaml" | awk '{print $1}')"

for required_path in \
  "$payload_dir/manifest.yaml" \
  "$payload_dir/checksums.sha256"; do
  if [[ ! -f "$required_path" || -L "$required_path" ]]; then
    echo "[ERROR] required target payload file is missing: $required_path" >&2
    exit 1
  fi
done
if find "$payload_dir" -type l -print -quit | grep -q .; then
  echo "[ERROR] target payload contains a symbolic link" >&2
  exit 1
fi
payload_archive="$(awk '/^archive:[[:space:]]*/ { print $2; exit }' "$payload_dir/manifest.yaml")"
payload_schema="$(awk '/^schema_version:[[:space:]]*/ { print $2; exit }' "$payload_dir/manifest.yaml")"
payload_identity="$(awk '/^identity:[[:space:]]*/ { print $2; exit }' "$payload_dir/manifest.yaml")"
payload_release_version="$(awk '/^release:/{ in_release=1; next } /^[^ ]/{ in_release=0 } in_release && /^  version:/{ print $2; exit }' "$payload_dir/manifest.yaml")"
payload_registry="$(awk '/^repository:/{ in_repository=1; next } /^[^ ]/{ in_repository=0 } in_repository && /^  registry:/{ print $2; exit }' "$payload_dir/manifest.yaml")"
payload_project="$(awk '/^repository:/{ in_repository=1; next } /^[^ ]/{ in_repository=0 } in_repository && /^  project:/{ print $2; exit }' "$payload_dir/manifest.yaml")"
if [[ "$payload_schema" != "v1" || ! "$payload_identity" =~ ^[a-f0-9]{32}$ || -z "$payload_release_version" || -z "$payload_registry" || -z "$payload_project" || ! "$payload_archive" =~ ^[A-Za-z0-9._-]+$ || ! -f "$payload_dir/$payload_archive" || -L "$payload_dir/$payload_archive" ]]; then
  echo "[ERROR] target payload archive is invalid" >&2
  exit 1
fi
while IFS= read -r payload_file; do
  case "$payload_file" in
    manifest.yaml|checksums.sha256|"$payload_archive") ;;
    *)
      echo "[ERROR] target payload has an unexpected file: $payload_file" >&2
      exit 1
      ;;
  esac
done < <(find "$payload_dir" -mindepth 1 -maxdepth 1 -printf '%f\n')
if [[ "$(wc -l < "$payload_dir/checksums.sha256")" -ne 1 || ! "$(awk 'NF == 2 { print $1 " " $2 }' "$payload_dir/checksums.sha256")" =~ ^[a-f0-9]{64}\ {1,2}${payload_archive}$ ]]; then
  echo "[ERROR] target payload checksum manifest is invalid" >&2
  exit 1
fi
if ! (cd "$payload_dir" && sha256sum --check --strict checksums.sha256 >/dev/null); then
  echo "[ERROR] target payload checksum validation failed" >&2
      exit 1
fi

for required_path in \
  "$release_dir/release.yaml" \
  "$release_dir/checksums.sha256"; do
  if [[ ! -f "$required_path" || -L "$required_path" ]]; then
    echo "[ERROR] required regular file is missing: $required_path" >&2
    exit 1
  fi
done

if find "$release_dir" -type l -print -quit | grep -q .; then
  echo "[ERROR] Release directory contains a symbolic link" >&2
  exit 1
fi

release_version="$(
  awk '
    /^version:[[:space:]]*/ {
      value = $0
      sub(/^version:[[:space:]]*/, "", value)
      gsub(/^["'\'']|["'\'']$/, "", value)
      print value
      exit
    }
  ' "$release_dir/release.yaml"
)"

if [[ ! "$release_version" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([+-][A-Za-z0-9.-]+)?$ ]]; then
  echo "[ERROR] invalid or missing Release version: $release_version" >&2
  exit 1
fi

mapfile -t artifact_entries < <(
  awk '
    /^[[:space:]]*-[[:space:]]+name:[[:space:]]*/ {
      name = $0
      sub(/^[[:space:]]*-[[:space:]]+name:[[:space:]]*/, "", name)
      gsub(/^["'\'']|["'\'']$/, "", name)
      next
    }
    /^[[:space:]]+file:[[:space:]]*/ {
      file = $0
      sub(/^[[:space:]]+file:[[:space:]]*/, "", file)
      gsub(/^["'\'']|["'\'']$/, "", file)
      if (name != "") {
        print name "|" file
        name = ""
      }
    }
  ' "$release_dir/release.yaml"
)

if ((${#artifact_entries[@]} == 0)); then
  echo "[ERROR] release.yaml has no artifacts" >&2
  exit 1
fi

required_artifacts=(
  eva-tool
  eva-tool-installer
  eva-infra
  eva-solution
)

for required_name in "${required_artifacts[@]}"; do
  found=false

  for artifact_entry in "${artifact_entries[@]}"; do
    artifact_name="${artifact_entry%%|*}"
    artifact_file="${artifact_entry#*|}"

    if [[ "$artifact_name" == "$required_name" ]]; then
      found=true

      if [[ "$artifact_file" == /* ||
            "$artifact_file" == ".." ||
            "$artifact_file" == ../* ||
            "$artifact_file" == */../* ]]; then
        echo "[ERROR] unsafe artifact path: $artifact_file" >&2
        exit 1
      fi

      if [[ ! -f "$release_dir/$artifact_file" ||
            -L "$release_dir/$artifact_file" ]]; then
        echo "[ERROR] artifact is missing: $artifact_name ($artifact_file)" >&2
        exit 1
      fi

      break
    fi
  done

  if [[ "$found" != true ]]; then
    echo "[ERROR] Remote Release is missing artifact: $required_name" >&2
    exit 1
  fi
done

manifest_files="$(
  awk '
    NF == 2 {
      name = $2
      sub(/^\*/, "", name)
      print name
    }
  ' "$release_dir/checksums.sha256"
)"

if [[ -z "$manifest_files" ]]; then
  echo "[ERROR] checksums.sha256 is empty" >&2
  exit 1
fi

while IFS= read -r manifest_file; do
  if [[ "$manifest_file" == /* ||
        "$manifest_file" == ".." ||
        "$manifest_file" == ../* ||
        "$manifest_file" == */../* ]]; then
    echo "[ERROR] unsafe checksum path: $manifest_file" >&2
    exit 1
  fi

  if [[ ! -f "$release_dir/$manifest_file" ||
        -L "$release_dir/$manifest_file" ]]; then
    echo "[ERROR] checksum input is missing: $manifest_file" >&2
    exit 1
  fi
done <<< "$manifest_files"

if (
  cd "$release_dir"
  sha256sum --check --strict checksums.sha256 >/dev/null
); then
  echo "[OK] Source Release checksum verified"
else
  echo "[ERROR] Source Release checksum validation failed" >&2
  exit 1
fi

release_manifest_sha256="$(
  sha256sum "$release_dir/release.yaml" |
    awk '{print $1}'
)"

checksum_manifest_sha256="$(
  sha256sum "$release_dir/checksums.sha256" |
    awk '{print $1}'
)"

transfer_id="${release_version}-${checksum_manifest_sha256:0:16}"
target_final="$target_root/$release_version"
target_staging="$target_root/.incoming-$transfer_id"

remote_shell=("$SSH_CMD")
remote_shell+=("${ssh_options[@]}")

remote_shell_text=""
printf -v remote_shell_text '%q ' "${remote_shell[@]}"
remote_shell_text="${remote_shell_text% }"

run_remote_root_script() {
  local remote_script

  remote_script="$(cat)"

  case "$sudo_mode" in
    password)
      {
        printf '%s\n' "$sudo_password"
        printf '%s\n' "$remote_script"
      } |
        "$SSH_CMD" "${ssh_options[@]}" "$target" \
          sudo -S -p '' bash -s -- "$@"
      ;;
    passwordless)
      printf '%s\n' "$remote_script" |
        "$SSH_CMD" "${ssh_options[@]}" "$target" \
          sudo -n bash -s -- "$@"
      ;;
    none)
      printf '%s\n' "$remote_script" |
        "$SSH_CMD" "${ssh_options[@]}" "$target" \
          bash -s -- "$@"
      ;;
  esac
}

cleanup_remote_staging() {
  local cleanup_status=0

  if [[ "$remote_staging_prepared" != true ]] ||
    [[ "$remote_publish_committed" == true ]]
  then
    return 0
  fi

  if ! run_remote_root_script "$target_staging" <<'REMOTE_CLEANUP'
set -euo pipefail
target_staging="$1"

if [[ -e "$target_staging" ]]; then
  rm -rf -- "$target_staging"
fi
REMOTE_CLEANUP
  then
    echo "[ERROR] Remote publish staging cleanup failed" >&2
    cleanup_status=1
  fi

  return "$cleanup_status"
}

on_transport_exit() {
  local status=$?

  trap - EXIT

  if ! cleanup_remote_staging; then
    echo "[ERROR] Remote publish cleanup was incomplete" >&2
  fi

  return "$status"
}

trap on_transport_exit EXIT

echo "[INFO] release_version=$release_version"
echo "[INFO] source=$release_dir"
echo "[INFO] target=$target"
echo "[INFO] target_path=$target_final"

run_remote_root_script \
  "$target_root" \
  "$target_staging" \
  "$target_final" \
  "$target" \
  "$sudo_mode" <<'REMOTE_PREPARE'
set -euo pipefail

target_root="$1"
target_staging="$2"
target_final="$3"
target_identity="$4"
sudo_mode="$5"
target_user="${target_identity%@*}"

case "$sudo_mode" in
  none|password|passwordless)
    ;;
  *)
    echo "[ERROR] invalid Remote sudo mode" >&2
    exit 1
    ;;
esac

umask 027
mkdir -p "$target_root"

if [[ -L "$target_root" ]]; then
  echo "[ERROR] target root must not be a symbolic link: $target_root" >&2
  exit 1
fi

if [[ -e "$target_staging" ]]; then
  rm -rf -- "$target_staging"
fi

mkdir -p "$target_staging"

if [[ "$target_user" == "$target_identity" ]] ||
  [[ -z "$target_user" ]]
then
  echo "[ERROR] Target SSH user is invalid" >&2
  exit 1
fi

if [[ "$sudo_mode" != none ]]; then
  target_group="$(id -gn "$target_user")"
  chown "$target_user:$target_group" "$target_staging"
fi

chmod 0700 "$target_staging"

if [[ -e "$target_final" && ! -d "$target_final" ]]; then
  echo "[ERROR] existing target Release is not a directory: $target_final" >&2
  exit 1
fi
REMOTE_PREPARE

remote_staging_prepared=true

"$RSYNC_CMD" \
  --archive \
  --delete \
  --delay-updates \
  --chmod=Du=rwx,Dgo=rx,Fu=rw,Fgo=r \
  --rsh="$remote_shell_text" \
  "$release_dir/" \
  "$target:$target_staging/"

"$RSYNC_CMD" \
  --archive --delete --delay-updates \
  --chmod=Du=rwx,Dgo=rx,Fu=rw,Fgo=r \
  --rsh="$remote_shell_text" \
  "$runtime_dir/" \
  "$target:$target_staging/remote-runtime/"

"$RSYNC_CMD" \
  --archive \
  --delete \
  --delay-updates \
  --chmod=Du=rwx,Dgo=rx,Fu=rw,Fgo=r \
  --rsh="$remote_shell_text" \
  "$payload_dir/" \
  "$target:$target_staging/remote-payload/"

run_remote_root_script \
    "$target_staging" \
    "$target_final" \
    "$release_version" \
    "$release_manifest_sha256" \
    "$checksum_manifest_sha256" \
    "$runtime_manifest_sha256" <<'REMOTE_PUBLISH'
set -euo pipefail

target_staging="$1"
target_final="$2"
release_version="$3"
expected_release_sha256="$4"
expected_checksums_sha256="$5"
expected_runtime_manifest_sha256="$6"

cleanup() {
  rm -rf -- "$target_staging"
}
trap cleanup EXIT

for command in awk sha256sum tar; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "[ERROR] required Target command not found: $command" >&2
    exit 1
  }
done

payload_dir="$target_staging/remote-payload"
runtime_dir="$target_staging/remote-runtime"
for required_path in "$runtime_dir/manifest.yaml" "$runtime_dir/checksums.sha256"; do
  if [[ ! -f "$required_path" || -L "$required_path" ]]; then echo "[ERROR] transferred Runtime artifact file is invalid" >&2; exit 1; fi
done
runtime_archive="$(awk '/^  archive:[[:space:]]*/ { print $2; exit }' "$runtime_dir/manifest.yaml")"
runtime_platform="$(awk '/^  platform:[[:space:]]*/ { print $2; exit }' "$runtime_dir/manifest.yaml")"
runtime_release_version="$(awk '/^release:/{ x=1; next } /^[^ ]/{x=0} x && /^  version:/{print $2; exit}' "$runtime_dir/manifest.yaml")"
runtime_registry="$(awk '/^repository:/{ x=1; next } /^[^ ]/{x=0} x && /^  registry:/{print $2; exit}' "$runtime_dir/manifest.yaml")"
runtime_project="$(awk '/^repository:/{ x=1; next } /^[^ ]/{x=0} x && /^  project:/{print $2; exit}' "$runtime_dir/manifest.yaml")"
runtime_descriptor_sha256="$(awk '/^  descriptor_sha256:[[:space:]]*/ { print $2; exit }' "$runtime_dir/manifest.yaml")"
if find "$runtime_dir" -type l -print -quit | grep -q . || [[ "$(find "$runtime_dir" -mindepth 1 -maxdepth 1 -printf '%f\n' | wc -l)" -ne 3 ]] || [[ "$(wc -l < "$runtime_dir/checksums.sha256")" -ne 1 ]] || [[ "$runtime_platform" != linux/amd64 || "$runtime_release_version" != "$release_version" || -z "$runtime_registry" || -z "$runtime_project" || ! "$runtime_archive" =~ ^[A-Za-z0-9._-]+$ || ! -f "$runtime_dir/$runtime_archive" || -L "$runtime_dir/$runtime_archive" ]] || ! (cd "$runtime_dir" && sha256sum --check --strict checksums.sha256 >/dev/null) || [[ ! "$runtime_descriptor_sha256" =~ ^[a-f0-9]{64}$ ]] || [[ "$(tar -xOzf "$runtime_dir/$runtime_archive" runtime/runtime.yaml 2>/dev/null | sha256sum | awk '{print $1}')" != "$runtime_descriptor_sha256" ]]; then echo "[ERROR] transferred Runtime artifact is invalid" >&2; exit 1; fi
while IFS= read -r runtime_entry; do
  case "$runtime_entry" in runtime|runtime/runtime.yaml|runtime/bin|runtime/bin/*|runtime/venv|runtime/venv/*) ;; *) echo "[ERROR] unsafe Runtime archive entry: $runtime_entry" >&2; exit 1 ;; esac
  if [[ -z "$runtime_entry" || "$runtime_entry" == /* || "/$runtime_entry/" == */../* || "$runtime_entry" == *secret* || "$runtime_entry" == *credential* || "$runtime_entry" == *token* ]]; then echo "[ERROR] unsafe Runtime archive entry: $runtime_entry" >&2; exit 1; fi
done < <(tar -tzf "$runtime_dir/$runtime_archive")
runtime_manifest_sha256="$(sha256sum "$runtime_dir/manifest.yaml" | awk '{print $1}')"
if [[ "$runtime_manifest_sha256" != "$expected_runtime_manifest_sha256" ]]; then echo "[ERROR] transferred Runtime artifact digest mismatch" >&2; exit 1; fi
for required_path in "$payload_dir/manifest.yaml" "$payload_dir/checksums.sha256"; do
  if [[ ! -f "$required_path" || -L "$required_path" ]]; then
    echo "[ERROR] transferred target payload file is invalid: $required_path" >&2
    exit 1
  fi
done
if find "$payload_dir" -type l -print -quit | grep -q .; then
  echo "[ERROR] transferred target payload contains a symbolic link" >&2
  exit 1
fi
payload_archive="$(awk '/^archive:[[:space:]]*/ { print $2; exit }' "$payload_dir/manifest.yaml")"
payload_schema="$(awk '/^schema_version:[[:space:]]*/ { print $2; exit }' "$payload_dir/manifest.yaml")"
payload_identity="$(awk '/^identity:[[:space:]]*/ { print $2; exit }' "$payload_dir/manifest.yaml")"
payload_release_version="$(awk '/^release:/{ in_release=1; next } /^[^ ]/{ in_release=0 } in_release && /^  version:/{ print $2; exit }' "$payload_dir/manifest.yaml")"
payload_release_digest="$(awk '/^release:/{ in_release=1; next } /^[^ ]/{ in_release=0 } in_release && /^  release_yaml_sha256:/{ print $2; exit }' "$payload_dir/manifest.yaml")"
payload_checksums_digest="$(awk '/^release:/{ in_release=1; next } /^[^ ]/{ in_release=0 } in_release && /^  checksums_sha256:/{ print $2; exit }' "$payload_dir/manifest.yaml")"
payload_registry="$(awk '/^repository:/{ in_repository=1; next } /^[^ ]/{ in_repository=0 } in_repository && /^  registry:/{ print $2; exit }' "$payload_dir/manifest.yaml")"
payload_project="$(awk '/^repository:/{ in_repository=1; next } /^[^ ]/{ in_repository=0 } in_repository && /^  project:/{ print $2; exit }' "$payload_dir/manifest.yaml")"
if [[ "$payload_schema" != "v1" || ! "$payload_identity" =~ ^[a-f0-9]{32}$ || -z "$payload_registry" || -z "$payload_project" || ! "$payload_archive" =~ ^[A-Za-z0-9._-]+$ || ! -f "$payload_dir/$payload_archive" || -L "$payload_dir/$payload_archive" ]]; then
  echo "[ERROR] transferred target payload archive is invalid" >&2
  exit 1
fi
while IFS= read -r payload_file; do
  case "$payload_file" in
    manifest.yaml|checksums.sha256|"$payload_archive") ;;
    *)
      echo "[ERROR] transferred target payload has an unexpected file: $payload_file" >&2
      exit 1
      ;;
  esac
done < <(find "$payload_dir" -mindepth 1 -maxdepth 1 -printf '%f\n')
if [[ "$payload_release_version" != "$release_version" || "$payload_release_digest" != "$expected_release_sha256" || "$payload_checksums_digest" != "$expected_checksums_sha256" ]]; then
  echo "[ERROR] transferred target payload identity does not match Release" >&2
  exit 1
fi
if [[ "$runtime_registry" != "$payload_registry" || "$runtime_project" != "$payload_project" ]]; then
  echo "[ERROR] transferred Runtime artifact repository identity does not match Target payload" >&2
  exit 1
fi
if ! (cd "$payload_dir" && sha256sum --check --strict checksums.sha256 >/dev/null); then
  echo "[ERROR] Target payload checksum validation failed" >&2
  exit 1
fi
if [[ "$(wc -l < "$payload_dir/checksums.sha256")" -ne 1 || ! "$(awk 'NF == 2 { print $1 " " $2 }' "$payload_dir/checksums.sha256")" =~ ^[a-f0-9]{64}\ {1,2}${payload_archive}$ ]]; then
  echo "[ERROR] Target payload checksum manifest is invalid" >&2
  exit 1
fi
while IFS= read -r payload_entry; do
  if [[ -z "$payload_entry" ||
        "$payload_entry" == /* ||
        "/$payload_entry/" == */../* ||
        "$payload_entry" != cache/* ||
        "$payload_entry" == cache/images/* ||
        "$payload_entry" == cache/qdrant-snapshots/* ]]; then
    echo "[ERROR] unsafe target payload archive entry: $payload_entry" >&2
    exit 1
  fi
done < <(tar -tzf "$payload_dir/$payload_archive")
if tar -tvzf "$payload_dir/$payload_archive" | awk '$1 !~ /^[-d]/ { exit 1 }'; then :; else
  echo "[ERROR] target payload archive contains a link or special file" >&2
  exit 1
fi
payload_manifest_sha256="$(sha256sum "$payload_dir/manifest.yaml" | awk '{print $1}')"

for required_path in \
  "$target_staging/release.yaml" \
  "$target_staging/checksums.sha256"; do
  if [[ ! -f "$required_path" || -L "$required_path" ]]; then
    echo "[ERROR] transferred Release file is invalid: $required_path" >&2
    exit 1
  fi
done

if find "$target_staging" -type l -print -quit | grep -q .; then
  echo "[ERROR] transferred Release contains a symbolic link" >&2
  exit 1
fi

actual_release_sha256="$(
  sha256sum "$target_staging/release.yaml" |
    awk '{print $1}'
)"

actual_checksums_sha256="$(
  sha256sum "$target_staging/checksums.sha256" |
    awk '{print $1}'
)"

if [[ "$actual_release_sha256" != "$expected_release_sha256" ]]; then
  echo "[ERROR] transferred release.yaml digest mismatch" >&2
  exit 1
fi

if [[ "$actual_checksums_sha256" != "$expected_checksums_sha256" ]]; then
  echo "[ERROR] transferred checksums.sha256 digest mismatch" >&2
  exit 1
fi

actual_version="$(
  awk '
    /^version:[[:space:]]*/ {
      value = $0
      sub(/^version:[[:space:]]*/, "", value)
      gsub(/^["'\'']|["'\'']$/, "", value)
      print value
      exit
    }
  ' "$target_staging/release.yaml"
)"

if [[ "$actual_version" != "$release_version" ]]; then
  echo \
    "[ERROR] transferred Release version mismatch: expected=$release_version actual=$actual_version" \
    >&2
  exit 1
fi

if ! (
  cd "$target_staging"
  sha256sum --check --strict checksums.sha256 >/dev/null
); then
  echo "[ERROR] Target Release checksum validation failed" >&2
  exit 1
fi

cat > "$target_staging/.eva-remote-release" <<MARKER
schema_version: v1
release_version: $release_version
release_yaml_sha256: $actual_release_sha256
checksums_sha256: $actual_checksums_sha256
payload_manifest_sha256: $payload_manifest_sha256
runtime_manifest_sha256: $runtime_manifest_sha256
registry: $payload_registry
project: $payload_project
MARKER

chmod 0644 "$target_staging/.eva-remote-release"

if [[ -d "$target_final" ]]; then
  existing_release_sha256=""

  if [[ -f "$target_final/.eva-remote-release" ]]; then
    existing_release_sha256="$(
      awk -F': *' '
        /^release_yaml_sha256:/ {
          print $2
          exit
        }
      ' "$target_final/.eva-remote-release"
    )"
  fi

  if [[ "$existing_release_sha256" == "$actual_release_sha256" ]] &&
     [[ "$(awk -F': *' '/^payload_manifest_sha256:/ { print $2; exit }' "$target_final/.eva-remote-release")" == "$payload_manifest_sha256" ]] &&
     [[ "$(awk -F': *' '/^runtime_manifest_sha256:/ { print $2; exit }' "$target_final/.eva-remote-release")" == "$runtime_manifest_sha256" ]] &&
     cmp -s \
       "$target_final/checksums.sha256" \
       "$target_staging/checksums.sha256"; then
    echo "[OK] Remote Release already published"
    echo "[INFO] path=$target_final"
    exit 0
  fi

  echo \
    "[ERROR] different Remote Release already exists: $target_final" \
    >&2
  exit 1
fi

chown -R root:root "$target_staging"
chmod 0755 "$target_staging"

mv -- "$target_staging" "$target_final"
trap - EXIT

echo "[OK] Remote Release published"
echo "[INFO] path=$target_final"
echo "[INFO] version=$release_version"
REMOTE_PUBLISH

remote_publish_committed=true
remote_staging_prepared=false
trap - EXIT
sudo_password=""
