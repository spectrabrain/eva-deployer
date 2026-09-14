#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/release/build_release.sh --tag vX.Y.Z [options]

Builds a verified EVA Release from the exact Git tag at HEAD.

Options:
  --tag TAG             Required Git tag at the current HEAD, for example v3.2.0.
  --offline-root PATH   Prepared Offline payload root. It must contain runtime/
                        and may contain packages/, images/, charts/, models/,
                        snapshots/, and harbor/. Enables Offline and Airgap artifacts.
  --dist-dir PATH       Output directory. Default: <repo>/out/dist.
  -h, --help            Show this help.
EOF
}

tag=""
offline_root=""
dist_dir=""
while (($#)); do
  case "$1" in
    --tag)
      tag="${2:-}"
      shift 2
      ;;
    --offline-root)
      offline_root="${2:-}"
      shift 2
      ;;
    --dist-dir)
      dist_dir="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "[error] unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$tag" ]]; then
  echo "[error] --tag is required" >&2
  usage >&2
  exit 2
fi
if [[ ! "$tag" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([+-][A-Za-z0-9.]+)?$ ]]; then
  echo "[error] tag must use a semantic version, for example v3.2.0: $tag" >&2
  exit 2
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
cd "$repo_root"

if ! git show-ref --tags --verify --quiet "refs/tags/$tag"; then
  echo "[error] Git tag does not exist: $tag" >&2
  exit 1
fi
tag_commit="$(git rev-list -n 1 "$tag")"
head_commit="$(git rev-parse HEAD)"
if [[ "$tag_commit" != "$head_commit" ]]; then
  echo "[error] --tag must resolve to HEAD (tag=$tag_commit head=$head_commit)" >&2
  exit 1
fi
if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "[error] Release build requires a clean tracked worktree" >&2
  exit 1
fi
if [[ -n "$(git ls-files --others --exclude-standard)" ]]; then
  echo "[error] Release build requires no untracked files" >&2
  exit 1
fi

if [[ -z "$dist_dir" ]]; then
  dist_dir="$repo_root/out/dist"
fi
dist_dir="$(mkdir -p "$dist_dir" && cd "$dist_dir" && pwd)"
release_version="${tag#v}"
artifact_tag="v${release_version}"
source_date_epoch="$(git show -s --format=%ct "$tag_commit")"
commit_short="$(git rev-parse --short=12 "$tag_commit")"
build_date="$(git show -s --format=%cI "$tag_commit")"

for command in cmp find git go gzip sha256sum stat tar; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "[error] required command is unavailable: $command" >&2
    exit 1
  fi
done

assert_regular_tree() {
  local root="$1"
  local label="$2"
  if [[ ! -d "$root" ]]; then
    echo "[error] $label directory is missing: $root" >&2
    exit 1
  fi
  if find "$root" -type l -print -quit | grep -q .; then
    echo "[error] $label contains a symlink, which is not allowed in a Release artifact" >&2
    exit 1
  fi
  if find "$root" -xdev \( -type b -o -type c -o -type p -o -type s \) -print -quit | grep -q .; then
    echo "[error] $label contains a special file, which is not allowed in a Release artifact" >&2
    exit 1
  fi
}

assert_safe_offline_tree() {
  local root="$1"
  local path
  assert_regular_tree "$root" "Offline payload"
  if [[ ! -f "$root/runtime/runtime.yaml" ]]; then
    echo "[error] Offline payload must contain runtime/runtime.yaml" >&2
    exit 1
  fi
  while IFS= read -r path; do
    echo "[error] Offline payload contains a forbidden sensitive path: ${path#"$root"/}" >&2
    exit 1
  done < <(find "$root" \( -iname 'aws_key.ini' -o -iname '*.pem' -o -iname '*.key' -o -iname 'id_rsa*' -o -iname '.env' \) -print)
  while IFS= read -r path; do
    echo "[error] Offline payload contains a forbidden sensitive directory: ${path#"$root"/}" >&2
    exit 1
  done < <(find "$root" -type d \( -iname credentials -o -iname secrets \) -print)
}

make_archive() {
  local root="$1"
  local output="$2"
  shift 2
  (
    cd "$root"
    tar --sort=name --mtime="@$source_date_epoch" --owner=0 --group=0 --numeric-owner -cf - "$@" | gzip -n > "$output"
  )
}

checksum() {
  sha256sum "$1" | awk '{print $1}'
}

write_release_metadata() {
  local output="$1"
  local prefix="$2"
  shift 2
  {
    printf 'version: %s\n' "$release_version"
    printf 'platform:\n  os: linux\n  arch: amd64\n'
    printf 'artifacts:\n'
    local item name file
    for item in "$@"; do
      name="${item%%|*}"
      file="${item#*|}"
      printf '  - name: %s\n    file: %s%s\n    sha256: %s\n' "$name" "$prefix" "$file" "$(checksum "$staging_dist/$file")"
    done
  } > "$output"
}

write_checksums() {
  local output="$1"
  local prefix="$2"
  shift 2
  : > "$output"
  local item file
  for item in "$@"; do
    file="${item#*|}"
    printf '%s  %s%s\n' "$(checksum "$staging_dist/$file")" "$prefix" "$file" >> "$output"
  done
}

assert_regular_tree "$repo_root/src/infra" "Infra source"
assert_regular_tree "$repo_root/src/solution" "Solution source"
for file in ansible.cfg src/playbook-preflight.yaml src/playbook-vars.yaml; do
  if [[ ! -f "$repo_root/$file" ]]; then
    echo "[error] required Infra Release file is missing: $file" >&2
    exit 1
  fi
done
installer_source="$repo_root/scripts/install/install_eva_tool.sh"
if [[ ! -f "$installer_source" || -L "$installer_source" ]]; then
  echo "[error] required EVA Tool installer is missing or is not a regular file: $installer_source" >&2
  exit 1
fi

build_root="$(mktemp -d)"
trap 'rm -rf "$build_root"' EXIT
staging_dist="$build_root/dist"
build_output_dir="$repo_root/out/work/build"
tool_binary="$build_output_dir/eva"
mkdir -p "$staging_dist" "$build_root/tool/bin" "$build_output_dir"

echo "[info] downloading Go modules"
(
  cd "$repo_root/tools/eva"
  go mod download
  go test ./...
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid= -X main.version=$artifact_tag -X main.commit=$commit_short -X main.buildDate=$build_date" \
    -o "$tool_binary" ./cmd/eva
)
chmod 0755 "$tool_binary"
install -m 0755 "$tool_binary" "$build_root/tool/bin/eva"

tool_file="eva-tool_${artifact_tag}_linux_amd64.tar.gz"
installer_file="eva-tool-installer_${artifact_tag}.sh"
infra_file="eva-infra_${artifact_tag}.tar.gz"
solution_file="eva-solution_${artifact_tag}.tar.gz"
make_archive "$build_root/tool" "$staging_dist/$tool_file" bin/eva
install -m 0755 "$installer_source" "$staging_dist/$installer_file"
make_archive "$repo_root" "$staging_dist/$infra_file" ansible.cfg src/playbook-preflight.yaml src/playbook-vars.yaml src/infra
make_archive "$repo_root" "$staging_dist/$solution_file" src/solution

artifacts=(
  "eva-tool|$tool_file"
  "eva-tool-installer|$installer_file"
  "eva-infra|$infra_file"
  "eva-solution|$solution_file"
)

if [[ -n "$offline_root" ]]; then
  offline_root="$(cd "$offline_root" && pwd)"
  assert_safe_offline_tree "$offline_root"
  "$tool_binary" runtime validate --runtime-root "$offline_root/runtime" >/dev/null
  offline_file="eva-offline_${artifact_tag}_ubuntu24.04_amd64.tar.gz"
  mapfile -t offline_entries < <(cd "$offline_root" && find . -mindepth 1 -maxdepth 1 -printf '%P\n' | LC_ALL=C sort)
  if ((${#offline_entries[@]} == 0)); then
    echo "[error] Offline payload is empty: $offline_root" >&2
    exit 1
  fi
  make_archive "$offline_root" "$staging_dist/$offline_file" "${offline_entries[@]}"
  artifacts+=("eva-offline|$offline_file")
fi

write_release_metadata "$staging_dist/release.yaml" "" "${artifacts[@]}"
write_checksums "$staging_dist/checksums.sha256" "" "${artifacts[@]}"

"$tool_binary" verify "$staging_dist" >/dev/null
"$tool_binary" release prepare --release "$staging_dist" --install-root "$build_root/releases" >/dev/null

installer_smoke_root="$build_root/installer-smoke"
"$staging_dist/$installer_file" \
  --artifact "$staging_dist/$tool_file" \
  --sha256 "$(checksum "$staging_dist/$tool_file")" \
  --root "$installer_smoke_root/opt/eva" \
  --bin-dir "$installer_smoke_root/bin" \
  --state-root "$installer_smoke_root/var/lib/eva" \
  --log-root "$installer_smoke_root/var/log/eva" \
  --skip-group-management >/dev/null
"$installer_smoke_root/bin/eva" version >/dev/null

assert_mode() {
  local path="$1" expected="$2" actual
  actual="$(stat -c '%a' "$path")"
  if [[ "$actual" != "$expected" ]]; then
    echo "[error] installer mode mismatch: $path is $actual, expected $expected" >&2
    exit 1
  fi
}

assert_mode "$installer_smoke_root/opt/eva/tool" 755
assert_mode "$installer_smoke_root/opt/eva/tool/bin" 755
assert_mode "$installer_smoke_root/opt/eva/tool/bin/eva" 755
assert_mode "$installer_smoke_root/opt/eva/runtime" 2775
assert_mode "$installer_smoke_root/opt/eva/releases" 2775
assert_mode "$installer_smoke_root/var/lib/eva" 2770
assert_mode "$installer_smoke_root/var/lib/eva/artifacts" 2770
assert_mode "$installer_smoke_root/var/lib/eva/operations" 2770
assert_mode "$installer_smoke_root/var/lib/eva/state" 2770
assert_mode "$installer_smoke_root/var/log/eva" 2770
assert_mode "$installer_smoke_root/var/log/eva/operations" 2770

outputs=("${artifacts[@]}" "release-metadata|release.yaml" "checksum-manifest|checksums.sha256")
if [[ -n "$offline_root" ]]; then
  bundle_root="$build_root/bundle"
  mkdir -p "$bundle_root/artifacts"
  for artifact in "${artifacts[@]}"; do
    file="${artifact#*|}"
    cp "$staging_dist/$file" "$bundle_root/artifacts/$file"
  done
  write_release_metadata "$bundle_root/release.yaml" "artifacts/" "${artifacts[@]}"
  {
    printf 'EVA Airgap Bundle %s\n\n' "$artifact_tag"
    printf 'Verify before import: eva verify eva-airgap-bundle_%s_ubuntu24.04_amd64.tar.gz\n' "$artifact_tag"
  } > "$bundle_root/README.md"
  : > "$bundle_root/checksums.sha256"
  for artifact in "${artifacts[@]}"; do
    file="${artifact#*|}"
    printf '%s  artifacts/%s\n' "$(checksum "$staging_dist/$file")" "$file" >> "$bundle_root/checksums.sha256"
  done
  bundle_file="eva-airgap-bundle_${artifact_tag}_ubuntu24.04_amd64.tar.gz"
  make_archive "$bundle_root" "$staging_dist/$bundle_file" README.md artifacts checksums.sha256 release.yaml
  for artifact in "${artifacts[@]}"; do
    file="${artifact#*|}"
    if ! cmp -s "$staging_dist/$file" <(tar -xOzf "$staging_dist/$bundle_file" "artifacts/$file"); then
      echo "[error] Airgap Bundle artifact differs from the external artifact: $file" >&2
      exit 1
    fi
  done
  "$tool_binary" verify "$staging_dist/$bundle_file" >/dev/null
  outputs+=("airgap-bundle|$bundle_file")
  write_checksums "$staging_dist/checksums.sha256" "" "${artifacts[@]}" "airgap-bundle|$bundle_file"
fi

for output in "${outputs[@]}"; do
  file="${output#*|}"
  mode=0644
  if [[ "$file" == "$installer_file" ]]; then
    mode=0755
  fi
  install -m "$mode" "$staging_dist/$file" "$dist_dir/$file"
done

echo "[done] Release artifacts written to $dist_dir"
for output in "${outputs[@]}"; do
  file="${output#*|}"
  printf '  %s  %s\n' "$(checksum "$dist_dir/$file")" "$file"
done
