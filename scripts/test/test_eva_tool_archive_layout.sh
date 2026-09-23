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

release_root="$work_root/release #1"
mkdir -p "$release_root"
archive="$release_root/eva-tool_0.1.0-migration_linux_amd64.tar.gz"
(
  cd "$fixture_root"
  tar -czf "$archive" bin libexec
)
install -m 0755 "$installer" "$release_root/eva-tool-installer.sh"
printf 'infra fixture\n' > "$release_root/eva-infra_0.1.0-migration.tar.gz"
printf 'solution fixture\n' > "$release_root/eva-solution_0.1.0-migration.tar.gz"
cat > "$release_root/release.yaml" <<EOF
version: 0.1.0-migration
platform:
  os: linux
  arch: amd64
artifacts:
  - name: eva-tool
    file: $(basename "$archive")
    sha256: $(sha256sum "$archive" | awk '{print $1}')
  - name: eva-tool-installer
    file: eva-tool-installer.sh
    sha256: $(sha256sum "$release_root/eva-tool-installer.sh" | awk '{print $1}')
  - name: eva-infra
    file: eva-infra_0.1.0-migration.tar.gz
    sha256: $(sha256sum "$release_root/eva-infra_0.1.0-migration.tar.gz" | awk '{print $1}')
  - name: eva-solution
    file: eva-solution_0.1.0-migration.tar.gz
    sha256: $(sha256sum "$release_root/eva-solution_0.1.0-migration.tar.gz" | awk '{print $1}')
EOF
(
  cd "$release_root"
  sha256sum "$(basename "$archive")" eva-tool-installer.sh eva-infra_0.1.0-migration.tar.gz eva-solution_0.1.0-migration.tar.gz > checksums.sha256
)

install_root="$work_root/installed"
"${sudo_cmd[@]}" "$release_root/eva-tool-installer.sh" \
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
assert_mode "$install_root/opt/eva/tool/libexec/remote-root/scripts/install/install_docker.sh" 755
assert_mode "$install_root/opt/eva/tool/libexec/remote-root/scripts/install/setup_harbor.sh" 755
assert_mode "$install_root/opt/eva/tool/libexec/remote-root/src/infra/version.yaml" 644
assert_mode "$install_root/var/lib/eva/releases" 750
assert_mode "$install_root/var/lib/eva/releases/current.yaml" 640
assert_mode "$install_root/var/lib/eva/locks" 700
assert_mode "$install_root/var/lib/eva/locks/tool-installer.lock" 600
EVA_INTERNAL_CURRENT_RELEASE_RECEIPT_PATH="$install_root/var/lib/eva/releases/current.yaml" \
  "$install_root/bin/eva" internal validate-current-release --release "$release_root"
for directory in \
  "$install_root/opt/eva/tool" \
  "$install_root/opt/eva/tool/bin" \
  "$install_root/opt/eva/tool/libexec" \
  "$install_root/opt/eva/tool/libexec/remote-root" \
  "$install_root/opt/eva/tool/libexec/remote-root/scripts" \
  "$install_root/opt/eva/tool/libexec/remote-root/scripts/download" \
  "$install_root/opt/eva/tool/libexec/remote-root/scripts/install" \
  "$install_root/opt/eva/tool/libexec/remote-root/scripts/lib" \
  "$install_root/opt/eva/tool/libexec/remote-root/scripts/publish" \
  "$install_root/opt/eva/tool/libexec/remote-root/scripts/remote" \
  "$install_root/opt/eva/tool/libexec/remote-root/src" \
  "$install_root/opt/eva/tool/libexec/remote-root/src/infra" \
  "$install_root/opt/eva/tool/libexec/remote-root/src/solution"; do
  assert_mode "$directory" 755
  [[ "$("${sudo_cmd[@]}" stat -c '%U:%G' "$directory")" == root:root ]]
done
"$install_root/bin/eva" remote --help >/dev/null
"$install_root/opt/eva/tool/libexec/remote-root/scripts/remote/publish_release_to_target.sh" --help >/dev/null
VERSIONS_QUIET=1 bash -c '
  source "$1"
  load_deploy_versions
  [[ -n "$EVA_AGENT_RELEASE" && -n "$K3S_DEFAULT_VERSION" ]]
' bash "$install_root/opt/eva/tool/libexec/remote-root/scripts/lib/load_versions.sh"

# A failure after receipt publication must restore the prior Tool, command
# link, and receipt as one transaction. The hook is installer-test-only.
new_release_root="$work_root/release-new"
cp -a "$release_root" "$new_release_root"
new_version="0.1.1-migration"
new_fixture="$work_root/fixture-new"
cp -a "$fixture_root" "$new_fixture"
(
  cd "$repo_root/tools/eva"
  CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -ldflags "-X main.version=$new_version" \
    -o "$new_fixture/bin/eva" ./cmd/eva
)
new_archive="$new_release_root/eva-tool_${new_version}_linux_amd64.tar.gz"
rm -f "$new_release_root"/eva-tool_*_linux_amd64.tar.gz
(
  cd "$new_fixture"
  tar -czf "$new_archive" bin libexec
)
mv "$new_release_root/eva-infra_0.1.0-migration.tar.gz" "$new_release_root/eva-infra_${new_version}.tar.gz"
mv "$new_release_root/eva-solution_0.1.0-migration.tar.gz" "$new_release_root/eva-solution_${new_version}.tar.gz"
cat > "$new_release_root/release.yaml" <<EOF
version: $new_version
platform:
  os: linux
  arch: amd64
artifacts:
  - name: eva-tool
    file: $(basename "$new_archive")
    sha256: $(sha256sum "$new_archive" | awk '{print $1}')
  - name: eva-tool-installer
    file: eva-tool-installer.sh
    sha256: $(sha256sum "$new_release_root/eva-tool-installer.sh" | awk '{print $1}')
  - name: eva-infra
    file: eva-infra_${new_version}.tar.gz
    sha256: $(sha256sum "$new_release_root/eva-infra_${new_version}.tar.gz" | awk '{print $1}')
  - name: eva-solution
    file: eva-solution_${new_version}.tar.gz
    sha256: $(sha256sum "$new_release_root/eva-solution_${new_version}.tar.gz" | awk '{print $1}')
EOF
(
  cd "$new_release_root"
  sha256sum "$(basename "$new_archive")" eva-tool-installer.sh "eva-infra_${new_version}.tar.gz" "eva-solution_${new_version}.tar.gz" > checksums.sha256
)
if EVA_INSTALLER_TEST_FAIL_AFTER=receipt-published "${sudo_cmd[@]}" "$new_release_root/eva-tool-installer.sh" \
  --root "$install_root/opt/eva" \
  --bin-dir "$install_root/bin" \
  --state-root "$install_root/var/lib/eva" \
  --log-root "$install_root/var/log/eva" >/dev/null 2>&1; then
  echo "[error] installer accepted injected Current Release failure" >&2
  exit 1
fi
[[ "$("$install_root/bin/eva" version | awk 'NR == 1 { print $1; exit }')" == "0.1.0-migration" ]]
[[ "$(readlink -f "$install_root/bin/eva")" == "$install_root/opt/eva/tool/bin/eva" ]]
EVA_INTERNAL_CURRENT_RELEASE_RECEIPT_PATH="$install_root/var/lib/eva/releases/current.yaml" \
  "$install_root/bin/eva" internal validate-current-release --release "$release_root"
if find "$install_root/opt/eva" -mindepth 1 -maxdepth 1 -name '.eva-*' -print -quit | grep -q .; then
  echo "[error] installer left a Tool transaction backup after rollback" >&2
  exit 1
fi

first_failure_root="$work_root/first-failure"
if EVA_INSTALLER_TEST_FAIL_AFTER=register-current "${sudo_cmd[@]}" "$release_root/eva-tool-installer.sh" \
  --root "$first_failure_root/opt/eva" \
  --bin-dir "$first_failure_root/bin" \
  --state-root "$first_failure_root/var/lib/eva" \
  --log-root "$first_failure_root/var/log/eva" >/dev/null 2>&1; then
  echo "[error] first installer run accepted injected Current Release failure" >&2
  exit 1
fi
[[ ! -e "$first_failure_root/opt/eva/tool" && ! -L "$first_failure_root/opt/eva/tool" ]]
[[ ! -e "$first_failure_root/bin/eva" && ! -L "$first_failure_root/bin/eva" ]]
[[ ! -e "$first_failure_root/var/lib/eva/releases/current.yaml" && ! -L "$first_failure_root/var/lib/eva/releases/current.yaml" ]]

wait_for_hook() {
  local marker="$1" process="$2"
  while [[ ! -e "$marker" ]]; do
    if ! kill -0 "$process" 2>/dev/null; then
      echo "[error] installer exited before reaching hook: $marker" >&2
      exit 1
    fi
    sleep 0.05
  done
}

start_waiting_installer() {
  local release="$1" stage="$2" hook_dir="$3" output="$4"
  (
    # Non-interactive Bash backgrounds jobs with SIGINT ignored. Reset it so
    # the installer can exercise its INT trap under this isolated harness.
    trap - INT
    EVA_INSTALLER_TEST_WAIT_AFTER="$stage" \
      EVA_INSTALLER_TEST_HOOK_DIR="$hook_dir" \
      "${sudo_cmd[@]}" "$release/eva-tool-installer.sh" \
        --root "$install_root/opt/eva" \
        --bin-dir "$install_root/bin" \
        --state-root "$install_root/var/lib/eva" \
        --log-root "$install_root/var/log/eva"
  ) >"$output" 2>&1 &
  installer_pid=$!
}

assert_installer_lock_available() {
  local lock_path="$install_root/var/lib/eva/locks/tool-installer.lock"

  if ! "${sudo_cmd[@]}" flock -n "$lock_path" true; then
    echo "[error] EVA Tool installer lock remained held" >&2
    exit 1
  fi
}

assert_current_release() {
  assert_installer_lock_available
  local root="$1" expected_version="$2"
  [[ "$("$install_root/bin/eva" version | awk 'NR == 1 { print $1; exit }')" == "$expected_version" ]]
  EVA_INTERNAL_CURRENT_RELEASE_RECEIPT_PATH="$install_root/var/lib/eva/releases/current.yaml" \
    "$install_root/bin/eva" internal validate-current-release --release "$root"
}

# Lock contention must fail without touching the transaction held by A. A is
# then interrupted after Tool publish to prove SIGTERM rollback and lock reuse.
hook_root="$work_root/hooks-term"
mkdir -p "$hook_root"
start_waiting_installer "$new_release_root" tool-published "$hook_root" "$hook_root/a.log"
pid="$installer_pid"
wait_for_hook "$hook_root/tool-published.reached" "$pid"
if "${sudo_cmd[@]}" "$release_root/eva-tool-installer.sh" \
  --root "$install_root/opt/eva" \
  --bin-dir "$install_root/bin" \
  --state-root "$install_root/var/lib/eva" \
  --log-root "$install_root/var/log/eva" >"$hook_root/b.log" 2>&1; then
  echo "[error] concurrent installer unexpectedly acquired the lock" >&2
  exit 1
fi
grep -Fq 'another EVA Tool installer transaction is running' "$hook_root/b.log"
kill -TERM "$pid"
set +e
wait "$pid"
status=$?
set -e
[[ "$status" -eq 143 ]]
assert_current_release "$release_root" "0.1.0-migration"

# SIGINT after link publication and SIGHUP after receipt publication restore
# the pre-transaction Release without using timing-dependent sleeps.
for signal_stage in 'INT:link-published' 'HUP:receipt-published'; do
  signal_name="${signal_stage%%:*}"
  stage_name="${signal_stage#*:}"
  hook_root="$work_root/hooks-${signal_name,,}"
  mkdir -p "$hook_root"
  start_waiting_installer "$new_release_root" "$stage_name" "$hook_root" "$hook_root/installer.log"
  pid="$installer_pid"
  wait_for_hook "$hook_root/$stage_name.reached" "$pid"
  kill -"$signal_name" "$pid"
  set +e
  wait "$pid"
  status=$?
  set -e
  expected_status=130
  [[ "$signal_name" == HUP ]] && expected_status=129
  [[ "$status" -eq "$expected_status" ]]
  assert_current_release "$release_root" "0.1.0-migration"
done

# A stale lock file alone does not block the next transaction. After commit,
# interruption keeps the newly committed state and merely releases the lock.
touch "$install_root/var/lib/eva/locks/tool-installer.lock"
hook_root="$work_root/hooks-commit"
mkdir -p "$hook_root"
start_waiting_installer "$new_release_root" commit "$hook_root" "$hook_root/installer.log"
pid="$installer_pid"
wait_for_hook "$hook_root/commit.reached" "$pid"
kill -TERM "$pid"
set +e
wait "$pid"
status=$?
set -e
[[ "$status" -eq 143 ]]
assert_current_release "$new_release_root" "$new_version"

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
