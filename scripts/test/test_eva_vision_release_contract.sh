#!/usr/bin/env bash
# Published-chart image discovery contract. No network or real Docker/AWS calls.
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$REPO_ROOT/scripts/lib/load_versions.sh"
VERSIONS_QUIET=1 load_deploy_versions
[[ "$EVA_VISION_DEPLOY_VERSION" == 3.3.0 ]]
[[ "$EVA_VISION_CHART_VERSION" == 3.3.0 ]]
[[ "$(eva_vision_source chart 3.2.0)" == https://raw.githubusercontent.com/mellerikat/eva-vision/chartmuseum ]]
for version in 3.3.0 3.3.0-beta.1 4.0.0; do
  [[ "$(eva_vision_source chart "$version")" == https://raw.githubusercontent.com/spectrabrain/eva-vision-chart/chartmuseum ]]
  [[ "$(eva_vision_source image "$version")" == 211722900905.dkr.ecr.ap-northeast-2.amazonaws.com/spectrabrain/eva-vision ]]
done
if eva_vision_source chart invalid >/dev/null 2>&1; then
  echo '[ERROR] invalid Vision version was accepted' >&2
  exit 1
fi
archive="${EVA_VISION_TEST_CHART:-$REPO_ROOT/out/cache/eva-vision/eva-vision-3.3.0.tgz}"
[[ -f "$archive" ]] || { echo '[ERROR] set EVA_VISION_TEST_CHART to a local published 3.3.0 archive' >&2; exit 1; }
work="$(mktemp -d /tmp/eva-vision-release-contract.XXXXXXXX)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/cache/eva-vision"
cp "$archive" "$work/cache/eva-vision/eva-vision-3.3.0.tgz"
cat > "$work/bin/aws" <<'AWS'
#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == 'ecr get-login-password --region ap-northeast-2'* ]]
printf 'fixture-token\n'
AWS
cat > "$work/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  login) cat >/dev/null ;;
  pull) printf '%s\n' "${@: -1}" >> "$FIXTURE_PULL_LOG" ;;
  image)
    [[ "$2" == inspect ]]
    printf 'amd64/linux\n'
    ;;
  *) echo '[ERROR] unexpected mock Docker operation' >&2; exit 1 ;;
esac
DOCKER
chmod 0755 "$work/bin/aws" "$work/bin/docker"
PATH="$work/bin:$PATH" EVA_CACHE_ROOT="$work/cache" COMPONENTS=eva-vision \
  EVA_VISION_DEPLOY_VERSION=3.2.0 DOCKER_CMD="$work/bin/docker" FIXTURE_PULL_LOG="$work/pulls" \
  bash "$REPO_ROOT/scripts/download/download_eva_images.sh" > "$work/output"
expected='211722900905.dkr.ecr.ap-northeast-2.amazonaws.com/spectrabrain/eva-vision:3.3.0'
[[ "$(cat "$work/pulls")" == "$expected" ]]
[[ "$(cat "$work/cache/images/images-all.txt")" == "$expected" ]]
cmp "$work/cache/images/images-all.txt" "$work/cache/images/images-pulled.txt"
[[ ! -s "$work/cache/images/images-missing.txt" ]]
echo '[OK] Vision catalog, source boundary and published Chart image discovery use Spectrabrain 3.3.0'
