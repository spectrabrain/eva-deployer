#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage:
  scripts/remote/publish_release_to_target.sh \
    --release-dir PATH \
    --target HOST \
    [--target-root PATH] \
    [--ssh-option OPTION]

Publishes a verified original EVA Release directory to a Remote Target.

Required release contents:
  release.yaml
  checksums.sha256
  eva-tool artifact
  eva-infra artifact
  eva-solution artifact
  eva-offline artifact

Default target root:
  /var/lib/eva/inbox/releases

Environment:
  SSH_CMD       SSH executable. Default: ssh
  RSYNC_CMD     rsync executable. Default: rsync
USAGE
}

release_dir=""
target=""
target_root="/var/lib/eva/inbox/releases"
ssh_options=()

while (($#)); do
  case "$1" in
    --release-dir)
      release_dir="${2:-}"
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
    -h|--help)
      usage
      return 0 2>/dev/null || true
      ;;
    *)
      echo "[ERROR] unknown option: $1" >&2
      usage >&2
      return 2 2>/dev/null || true
      ;;
  esac
done

if [[ -z "$release_dir" ]]; then
  echo "[ERROR] --release-dir is required" >&2
  return 2 2>/dev/null || true
fi

if [[ -z "$target" ]]; then
  echo "[ERROR] --target is required" >&2
  return 2 2>/dev/null || true
fi

if [[ "$target" == -* || "$target" =~ [[:space:]] ]]; then
  echo "[ERROR] invalid target: $target" >&2
  return 2 2>/dev/null || true
fi

if [[ "$target_root" != /* || "$target_root" == "/" ]]; then
  echo "[ERROR] --target-root must be an absolute non-root path" >&2
  return 2 2>/dev/null || true
fi

SSH_CMD="${SSH_CMD:-ssh}"
RSYNC_CMD="${RSYNC_CMD:-rsync}"

command -v "$SSH_CMD" >/dev/null 2>&1 || {
  echo "[ERROR] SSH command not found: $SSH_CMD" >&2
  return 1 2>/dev/null || true
}

command -v "$RSYNC_CMD" >/dev/null 2>&1 || {
  echo "[ERROR] rsync command not found: $RSYNC_CMD" >&2
  return 1 2>/dev/null || true
}

release_dir="$(cd "$release_dir" && pwd)"

for required_path in \
  "$release_dir/release.yaml" \
  "$release_dir/checksums.sha256"; do
  if [[ ! -f "$required_path" || -L "$required_path" ]]; then
    echo "[ERROR] required regular file is missing: $required_path" >&2
    return 1 2>/dev/null || true
  fi
done

if find "$release_dir" -type l -print -quit | grep -q .; then
  echo "[ERROR] Release directory contains a symbolic link" >&2
  return 1 2>/dev/null || true
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
  return 1 2>/dev/null || true
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
  return 1 2>/dev/null || true
fi

required_artifacts=(
  eva-tool
  eva-infra
  eva-solution
  eva-offline
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
        return 1 2>/dev/null || true
      fi

      if [[ ! -f "$release_dir/$artifact_file" ||
            -L "$release_dir/$artifact_file" ]]; then
        echo "[ERROR] artifact is missing: $artifact_name ($artifact_file)" >&2
        return 1 2>/dev/null || true
      fi

      break
    fi
  done

  if [[ "$found" != true ]]; then
    echo "[ERROR] Remote Release is missing artifact: $required_name" >&2
    return 1 2>/dev/null || true
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
  return 1 2>/dev/null || true
fi

while IFS= read -r manifest_file; do
  if [[ "$manifest_file" == /* ||
        "$manifest_file" == ".." ||
        "$manifest_file" == ../* ||
        "$manifest_file" == */../* ]]; then
    echo "[ERROR] unsafe checksum path: $manifest_file" >&2
    return 1 2>/dev/null || true
  fi

  if [[ ! -f "$release_dir/$manifest_file" ||
        -L "$release_dir/$manifest_file" ]]; then
    echo "[ERROR] checksum input is missing: $manifest_file" >&2
    return 1 2>/dev/null || true
  fi
done <<< "$manifest_files"

if (
  cd "$release_dir"
  sha256sum --check --strict checksums.sha256 >/dev/null
); then
  echo "[OK] Source Release checksum verified"
else
  echo "[ERROR] Source Release checksum validation failed" >&2
  return 1 2>/dev/null || true
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

echo "[INFO] release_version=$release_version"
echo "[INFO] source=$release_dir"
echo "[INFO] target=$target"
echo "[INFO] target_path=$target_final"

"$SSH_CMD" "${ssh_options[@]}" "$target" \
  bash -s -- "$target_root" "$target_staging" "$target_final" <<'REMOTE_PREPARE'
set -euo pipefail

target_root="$1"
target_staging="$2"
target_final="$3"

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

if [[ -e "$target_final" && ! -d "$target_final" ]]; then
  echo "[ERROR] existing target Release is not a directory: $target_final" >&2
  exit 1
fi
REMOTE_PREPARE

"$RSYNC_CMD" \
  --archive \
  --delete \
  --delay-updates \
  --chmod=Du=rwx,Dgo=rx,Fu=rw,Fgo=r \
  --rsh="$remote_shell_text" \
  "$release_dir/" \
  "$target:$target_staging/"

"$SSH_CMD" "${ssh_options[@]}" "$target" \
  bash -s -- \
    "$target_staging" \
    "$target_final" \
    "$release_version" \
    "$release_manifest_sha256" \
    "$checksum_manifest_sha256" <<'REMOTE_PUBLISH'
set -euo pipefail

target_staging="$1"
target_final="$2"
release_version="$3"
expected_release_sha256="$4"
expected_checksums_sha256="$5"

cleanup() {
  rm -rf -- "$target_staging"
}
trap cleanup EXIT

for command in awk sha256sum; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "[ERROR] required Target command not found: $command" >&2
    exit 1
  }
done

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

offline_count="$(
  awk '
    /^[[:space:]]*-[[:space:]]+name:[[:space:]]*eva-offline[[:space:]]*$/ {
      count++
    }
    END {
      print count + 0
    }
  ' "$target_staging/release.yaml"
)"

if [[ "$offline_count" -ne 1 ]]; then
  echo \
    "[ERROR] Target Remote Release requires exactly one eva-offline artifact: count=$offline_count" \
    >&2
  exit 1
fi

cat > "$target_staging/.eva-remote-release" <<MARKER
schema_version: v1
release_version: $release_version
release_yaml_sha256: $actual_release_sha256
checksums_sha256: $actual_checksums_sha256
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

mv -- "$target_staging" "$target_final"
trap - EXIT

echo "[OK] Remote Release published"
echo "[INFO] path=$target_final"
echo "[INFO] version=$release_version"
REMOTE_PUBLISH
