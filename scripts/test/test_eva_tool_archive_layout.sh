#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
installer="$repo_root/scripts/install/install_eva_tool.sh"

if [[ "$EUID" -eq 0 ]]; then
  sudo_cmd=()
elif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
  sudo_cmd=(sudo)
else
  echo "[skip] EVA Tool archive installer contract requires root or passwordless sudo" >&2
  exit 0
fi

for command in go install ln mktemp stat tar; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "[error] missing command: $command" >&2
    exit 1
  }
done

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

work_root="$(mktemp -d)"
cleanup() {
  "${sudo_cmd[@]}" rm -rf "$work_root"
}
trap cleanup EXIT

fixture_root="$work_root/fixture"
mkdir -p "$fixture_root/bin" "$fixture_root/libexec/remote-root"
(
  cd "$repo_root/tools/eva"
  CGO_ENABLED=0 go build -trimpath -buildvcs=false -o "$fixture_root/bin/eva" ./cmd/eva
)
chmod 0755 "$fixture_root/bin/eva"
for path in "${remote_backend_paths[@]}"; do
  install -D -m 0644 "$repo_root/$path" "$fixture_root/libexec/remote-root/$path"
done
for path in "${remote_backend_shell_paths[@]}"; do
  chmod 0755 "$fixture_root/libexec/remote-root/$path"
done

archive="$work_root/eva-tool.tar.gz"
(
  cd "$fixture_root"
  tar -czf "$archive" bin libexec
)

install_root="$work_root/installed"
"${sudo_cmd[@]}" "$installer" \
  --artifact "$archive" \
  --root "$install_root/opt/eva" \
  --bin-dir "$install_root/bin" \
  --state-root "$install_root/var/lib/eva" \
  --log-root "$install_root/var/log/eva" >/dev/null

assert_mode() {
  local path="$1" expected="$2" actual
  actual="$("${sudo_cmd[@]}" stat -c '%a' "$path")"
  [[ "$actual" == "$expected" ]] || {
    echo "[error] mode mismatch: $path is $actual, expected $expected" >&2
    exit 1
  }
}

assert_mode "$install_root/opt/eva/tool/libexec/remote-root" 755
assert_mode "$install_root/opt/eva/tool/libexec/remote-root/scripts/remote/publish_release_to_target.sh" 755
assert_mode "$install_root/opt/eva/tool/libexec/remote-root/src/infra/version.yaml" 644
for directory in \
  "$install_root/opt/eva/tool" \
  "$install_root/opt/eva/tool/bin" \
  "$install_root/opt/eva/tool/libexec" \
  "$install_root/opt/eva/tool/libexec/remote-root" \
  "$install_root/opt/eva/tool/libexec/remote-root/scripts" \
  "$install_root/opt/eva/tool/libexec/remote-root/scripts/download" \
  "$install_root/opt/eva/tool/libexec/remote-root/scripts/lib" \
  "$install_root/opt/eva/tool/libexec/remote-root/scripts/publish" \
  "$install_root/opt/eva/tool/libexec/remote-root/scripts/remote" \
  "$install_root/opt/eva/tool/libexec/remote-root/src" \
  "$install_root/opt/eva/tool/libexec/remote-root/src/infra" \
  "$install_root/opt/eva/tool/libexec/remote-root/src/solution"; do
  assert_mode "$directory" 755
  [[ "$("${sudo_cmd[@]}" stat -c '%U:%G' "$directory")" == root:root ]]
done
"$install_root/opt/eva/tool/libexec/remote-root/scripts/remote/publish_release_to_target.sh" --help >/dev/null
VERSIONS_QUIET=1 bash -c '
  source "$1"
  load_deploy_versions
  [[ -n "$EVA_AGENT_RELEASE" && -n "$K3S_DEFAULT_VERSION" ]]
' bash "$install_root/opt/eva/tool/libexec/remote-root/scripts/lib/load_versions.sh"

expect_rejected() {
  local candidate="$1"
  if "${sudo_cmd[@]}" "$installer" \
    --artifact "$candidate" \
    --root "$work_root/rejected/opt/eva" \
    --bin-dir "$work_root/rejected/bin" \
    --state-root "$work_root/rejected/var/lib/eva" \
    --log-root "$work_root/rejected/var/log/eva" >/dev/null 2>&1; then
    echo "[error] installer accepted unsafe archive: $candidate" >&2
    exit 1
  fi
}

missing_archive="$work_root/missing-backend.tar.gz"
tar -czf "$missing_archive" -C "$fixture_root" bin
expect_rejected "$missing_archive"

traversal_archive="$work_root/path-traversal.tar.gz"
(
  cd "$fixture_root"
  tar -czf "$traversal_archive" --transform='s,^bin/eva$,../escape,' bin/eva
)
expect_rejected "$traversal_archive"

link_root="$work_root/link-root"
cp -a "$fixture_root" "$link_root"
ln -s bin/eva "$link_root/unsafe-link"
link_archive="$work_root/symlink.tar.gz"
(
  cd "$link_root"
  tar -czf "$link_archive" bin libexec unsafe-link
)
expect_rejected "$link_archive"

hard_link_root="$work_root/hard-link-root"
cp -a "$fixture_root" "$hard_link_root"
ln "$hard_link_root/bin/eva" "$hard_link_root/unsafe-hard-link"
hard_link_archive="$work_root/hard-link.tar.gz"
(
  cd "$hard_link_root"
  tar -czf "$hard_link_archive" bin libexec unsafe-hard-link
)
expect_rejected "$hard_link_archive"

special_root="$work_root/special-root"
cp -a "$fixture_root" "$special_root"
mkfifo "$special_root/unsafe-fifo"
special_archive="$work_root/special-file.tar.gz"
(
  cd "$special_root"
  tar -czf "$special_archive" bin libexec unsafe-fifo
)
expect_rejected "$special_archive"

echo "[OK] EVA Tool archive layout and installer contract passed"
