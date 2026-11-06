#!/usr/bin/env bash
set -euo pipefail

# Runs the Remote publish backend end to end with a Release, Runtime artifact
# and Target payload built by the real EVA Tool preparation code, the same
# --ssh-option values a managed Target connection passes, and an ssh stand-in
# that parses arguments like OpenSSH and runs the remote command locally.
#
# As on a real Target, the SSH session is an unprivileged user (nobody): the
# rsync receiver runs as that user and only `sudo ...` commands run as root,
# while the target root sits under a root-only 0700 directory like
# /var/lib/eva. That needs real root, so this runs as root or through
# passwordless sudo, like the installer contract test. Without a usable
# unprivileged user it still runs, as root, and says so.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
transport="$repo_root/scripts/remote/publish_release_to_target.sh"

if [[ "$EUID" -eq 0 ]]; then
  sudo_cmd=()
elif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
  sudo_cmd=(sudo --preserve-env=PATH)
else
  echo "[skip] Remote publish end-to-end test requires root or passwordless sudo" >&2
  exit 0
fi

for command in go rsync tar sha256sum; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "[error] missing command: $command" >&2
    exit 1
  }
done

work_root="$(mktemp -d)"
cleanup() {
  "${sudo_cmd[@]}" rm -rf -- "$work_root"
}
trap cleanup EXIT

fixture="$work_root/fixture"
mkdir -p "$fixture"
(
  cd "$repo_root/tools/eva"
  EVA_REMOTE_PUBLISH_FIXTURE_DIR="$fixture" \
    go test ./internal/remote -run '^TestWriteRemotePublishFixture$' -count=1 >/dev/null
)

ssh_user=root
if "${sudo_cmd[@]}" setpriv --reuid=nobody --regid="$(id -g nobody 2>/dev/null || echo 65534)" --clear-groups true 2>/dev/null; then
  ssh_user=nobody
else
  echo "[warn] cannot switch to an unprivileged SSH user; the Target permission boundary is not exercised" >&2
fi

bin="$work_root/bin"
mkdir -p "$bin"
cat >"$bin/ssh" <<'STUB'
#!/usr/bin/env bash
# Parse like OpenSSH: options, then the destination, then a command that is
# joined with spaces and run by the remote shell as the session user. Only
# the session's own sudo raises privileges.
user=""
while (($#)); do
  case "$1" in
    -o)
      [[ "${2:-}" =~ ^[A-Za-z]+=.+$ ]] || { echo "ssh: bad -o option: ${2:-}" >&2; exit 255; }
      shift 2
      ;;
    -l) user="${2:-}"; shift 2 ;;
    -p|-i|-F|-J) shift 2 ;;
    -*) shift ;;
    *) break ;;
  esac
done
[[ "${1:-}" =~ ^(([A-Za-z0-9._-]+)@)?[A-Za-z0-9._-]+$ ]] || {
  echo "ssh: Could not resolve hostname ${1:-}: Name or service not known" >&2
  exit 255
}
[[ -n "${BASH_REMATCH[2]}" ]] && user="${BASH_REMATCH[2]}"
shift
command="$*"
if [[ "$command" == sudo\ * || "$user" == root || -z "$user" ]]; then
  exec sh -c "$command"
fi
exec setpriv --reuid="$user" --regid="$(id -g "$user")" --clear-groups sh -c "$command"
STUB
cat >"$bin/sudo" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == -n ]]; then shift; exec "$@"; fi
[[ "$1" == -S && "$2" == -p && "$3" == '' ]] || { echo "sudo: usage error" >&2; exit 1; }
shift 3
IFS= read -r password
[[ "$password" == "$EVA_TEST_SUDO_PASSWORD" ]] || { echo "sudo: authentication failed" >&2; exit 1; }
exec "$@"
STUB
chmod +x "$bin/ssh" "$bin/sudo"

ssh_arguments=()
while IFS= read -r option; do
  [[ -n "$option" ]] && ssh_arguments+=(--ssh-option "$option")
done <"$fixture/ssh-options"
((${#ssh_arguments[@]} > 0)) || { echo "[error] fixture has no ssh options" >&2; exit 1; }

# A root-only state directory, like /var/lib/eva on a Target with the EVA Tool
# installed. Everything above it stays traversable, as /var/lib is.
chmod 0755 "$work_root"
state_root="$work_root/target/var/lib/eva"
staging_root="$work_root/target/var/lib/eva-staging"
target_root="$state_root/inbox/releases"
mkdir -p "$target_root"
chmod 0755 "$work_root/target" "$work_root/target/var" "$work_root/target/var/lib"
chmod 0700 "$state_root"
publish() {
  "${sudo_cmd[@]}" env PATH="$bin:$PATH" SSH_CMD="$bin/ssh" EVA_TEST_SUDO_PASSWORD="it's a \"sudo\" secret" \
    bash "$transport" \
    --release-dir "$fixture/release" \
    --runtime-dir "$fixture/runtime" \
    --payload-dir "$fixture/payload" \
    --target "$ssh_user@target" \
    --target-root "$target_root" \
    --staging-root "$staging_root" \
    "${ssh_arguments[@]}" \
    --sudo-mode password \
    --sudo-password-fd 3 \
    3<<<"it's a \"sudo\" secret"
}

publish >"$work_root/publish.log" 2>&1 || {
  cat "$work_root/publish.log" >&2
  echo "[error] Remote publish backend rejected EVA Tool artifacts" >&2
  exit 1
}
grep -Fq '[OK] Remote Release published' "$work_root/publish.log"
version="$(awk '/^version:/ { print $2; exit }' "$fixture/release/release.yaml")"
for path in release.yaml checksums.sha256 .eva-remote-release remote-runtime/manifest.yaml remote-payload/manifest.yaml; do
  "${sudo_cmd[@]}" test -f "$target_root/$version/$path" || { echo "[error] published Release lacks $path" >&2; exit 1; }
done
if "${sudo_cmd[@]}" find "$target_root" "$staging_root" -maxdepth 1 -name '.incoming-*' -print -quit | grep -q .; then
  echo "[error] publish left a Target staging directory" >&2
  exit 1
fi

publish >"$work_root/republish.log" 2>&1 || {
  cat "$work_root/republish.log" >&2
  echo "[error] republishing the same Release failed" >&2
  exit 1
}
grep -Fq '[OK] Remote Release already published' "$work_root/republish.log"

owner="$("${sudo_cmd[@]}" stat -c %U "$target_root/$version")"
[[ "$owner" == root ]] || { echo "[error] published Release is owned by $owner" >&2; exit 1; }

if [[ "$ssh_user" != root ]]; then
  echo '[OK] Remote publish end-to-end test passed with an unprivileged SSH user'
fi
echo '[OK] Remote publish end-to-end test passed'
