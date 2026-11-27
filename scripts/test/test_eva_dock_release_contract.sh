#!/usr/bin/env bash
# Published EVA-Dock chart image discovery contract. No network or real Docker/AWS calls.
# EVA_DOCK_TEST_CHART may select an explicit local archive; this test never downloads charts.
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$REPO_ROOT/scripts/lib/load_versions.sh"
VERSIONS_QUIET=1 load_deploy_versions
[[ -n "$EVA_DOCK_CHART_VERSION" ]]
# 오프라인 번들 경로·다운로드 URL 은 roles/eva_dock 이 읽는 위치와 같아야 합니다.
grep -Fq 'https://raw.githubusercontent.com/spectrabrain/eva-dock-chart/main' "$REPO_ROOT/scripts/download/download_offline_assets.sh"
# shellcheck disable=SC2016 # literal source text is matched on purpose
grep -Fq '"$EVA_CACHE_ROOT/eva-dock/eva-dock-${EVA_DOCK_CHART_VERSION}.tgz"' "$REPO_ROOT/scripts/download/download_offline_assets.sh"
# shellcheck disable=SC2016
grep -Fq 'eva_dock_chart: ${EVA_DOCK_CHART_VERSION}' "$REPO_ROOT/scripts/download/download_offline_assets.sh"

archive="${EVA_DOCK_TEST_CHART:-$REPO_ROOT/out/cache/eva-dock/eva-dock-${EVA_DOCK_CHART_VERSION}.tgz}"
[[ -f "$archive" ]] || { echo "[ERROR] set EVA_DOCK_TEST_CHART to a local published ${EVA_DOCK_CHART_VERSION} archive" >&2; exit 1; }
work="$(mktemp -d /tmp/eva-dock-release-contract.XXXXXXXX)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/cache/eva-dock"
cp "$archive" "$work/cache/eva-dock/eva-dock-${EVA_DOCK_CHART_VERSION}.tgz"
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

app_version="$(tar -xzOf "$archive" eva-dock/Chart.yaml | awk '/^appVersion:/ { gsub(/["\047]/, "", $2); print $2 }')"
[[ -n "$app_version" ]]
repository='211722900905.dkr.ecr.ap-northeast-2.amazonaws.com/spectrabrain/eva-dock'

run_discovery() {
  rm -f "$work/pulls"
  PATH="$work/bin:$PATH" EVA_CACHE_ROOT="$work/cache" COMPONENTS=eva-dock \
    DOCKER_CMD="$work/bin/docker" FIXTURE_PULL_LOG="$work/pulls" "$@" \
    bash "$REPO_ROOT/scripts/download/download_eva_images.sh" > "$work/output"
}

# 카탈로그의 image tag 가 그대로 렌더되고, Ingress TLS Secret Job 의 kubectl 이미지도 포함됩니다.
# imagePullSecret 을 끈 airgap 렌더이므로 ECR refresh CronJob 이미지(aws-cli)는 없어야 합니다.
expected_tag="${EVA_DOCK_IMAGE_TAG:-$app_version}"
run_discovery env
expected="$(printf '%s\n' "bitnami/kubectl:latest" "${repository}:${expected_tag}" | sort)"
[[ "$(cat "$work/cache/images/images-all.txt")" == "$expected" ]]
[[ "$(sort "$work/pulls")" == "$expected" ]]
cmp "$work/cache/images/images-all.txt" "$work/cache/images/images-pulled.txt"
[[ ! -s "$work/cache/images/images-missing.txt" ]]
if grep -q 'aws-cli' "$work/cache/images/images-all.txt"; then
  echo '[ERROR] airgap Dock render included the ECR refresh image' >&2
  exit 1
fi
echo "[OK] Dock chart ${EVA_DOCK_CHART_VERSION} discovers ${repository}:${expected_tag} and the TLS kubectl image"

run_discovery env EVA_DOCK_IMAGE_TAG=3.4.0-contract-test
grep -Fxq "${repository}:3.4.0-contract-test" "$work/cache/images/images-all.txt"
echo '[OK] Dock image tag override is rendered into the discovered image'

sed '/^eva_dock_image_tag:/d' "$REPO_ROOT/src/solution/version.yaml" > "$work/legacy-version.yaml"
run_discovery env -u EVA_DOCK_IMAGE_TAG EVA_SOLUTION_VERSION_FILE="$work/legacy-version.yaml"
grep -Fxq "${repository}:${app_version}" "$work/cache/images/images-all.txt"
echo '[OK] Catalogs without a Dock image tag fall back to the chart appVersion'

if PATH="$work/bin:$PATH" EVA_CACHE_ROOT="$work/empty" COMPONENTS=eva-dock DOCKER_CMD="$work/bin/docker" \
    bash "$REPO_ROOT/scripts/download/download_eva_images.sh" > "$work/output" 2>&1; then
  echo '[ERROR] missing Dock chart was accepted' >&2
  exit 1
fi
grep -Fq "eva-dock/eva-dock-${EVA_DOCK_CHART_VERSION}.tgz" "$work/output"
echo '[OK] Dock image discovery requires the offline chart'
