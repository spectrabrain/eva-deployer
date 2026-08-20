#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="${BASE_DIR:-$SCRIPT_DIR}"
IMAGE_DIR="$BASE_DIR/images"
RENDER_DIR="$IMAGE_DIR/rendered"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
source "$SCRIPT_DIR/load_versions.sh"
load_deploy_versions

EVA_AGENT_RELEASE="${EVA_AGENT_RELEASE:?missing EVA_AGENT_RELEASE (set in versions.json)}"
EVA_APP_CHART_VERSION="${EVA_APP_CHART_VERSION:?missing EVA_APP_CHART_VERSION (set in versions.json)}"
EVA_VISION_CHART_VERSION="${EVA_VISION_CHART_VERSION:?missing EVA_VISION_CHART_VERSION (set in versions.json)}"
EVA_AGENT_CHART_VERSION="${EVA_AGENT_CHART_VERSION:?missing EVA_AGENT_CHART_VERSION (set in versions.json)}"
EVA_AGENT_VLLM_CHART_VERSION="${EVA_AGENT_VLLM_CHART_VERSION:?missing EVA_AGENT_VLLM_CHART_VERSION (set in versions.json)}"
EVA_AGENT_INIT_CHART_VERSION="${EVA_AGENT_INIT_CHART_VERSION:?missing EVA_AGENT_INIT_CHART_VERSION (set in versions.json)}"
QDRANT_CHART_VERSION="${QDRANT_CHART_VERSION:?missing QDRANT_CHART_VERSION (set in versions.json)}"
EVA_IAM_CHART_VERSION="${EVA_IAM_CHART_VERSION:?missing EVA_IAM_CHART_VERSION (set in versions.json)}"
EVA_AGENT_VLLM_VALUES_FILE="${EVA_AGENT_VLLM_VALUES_FILE:-values-k3s.PRO6000-MIGx4.yaml}"

# 받을 컴포넌트. 기본은 전부. 일부만 설치할 때는 그 컴포넌트만 지정하면 required 검사·렌더·
# pull 이 모두 좁혀집니다. vllm 이미지가 커서 app/iam 만 볼 때는 차이가 큽니다.
#   COMPONENTS="eva-app eva-iam" ./install/download_eva_images.sh
COMPONENTS="${COMPONENTS:-all}"
ALL_COMPONENTS="eva-app eva-vision eva-agent eva-agent-init eva-agent-vllm eva-agent-qdrant eva-iam"

want() {
  [[ "$COMPONENTS" == "all" ]] && return 0
  local c
  for c in $COMPONENTS; do
    [[ "$c" == "$1" ]] && return 0
  done
  return 1
}

# 오타가 조용히 "아무것도 안 받음"으로 이어지지 않게 검증합니다.
if [[ "$COMPONENTS" != "all" ]]; then
  for c in $COMPONENTS; do
    # shellcheck disable=SC2076
    [[ " $ALL_COMPONENTS " == *" $c "* ]] || {
      echo "[ERROR] 알 수 없는 컴포넌트: $c"
      echo "        사용 가능: all | $ALL_COMPONENTS"
      exit 1
    }
  done
  echo "[info] COMPONENTS=$COMPONENTS (전체가 아닙니다)"
fi

for bin in helm docker; do
  command -v "$bin" >/dev/null 2>&1 || { echo "[ERROR] $bin not found"; exit 1; }
done
DOCKER_CMD="${DOCKER_CMD:-docker}"

mkdir -p "$IMAGE_DIR" "$RENDER_DIR"

APP_CHART="$BASE_DIR/eva-app/eva-app-${EVA_APP_CHART_VERSION}.tgz"
VISION_CHART="$BASE_DIR/eva-vision/eva-vision-${EVA_VISION_CHART_VERSION}.tgz"
AGENT_CHART="$BASE_DIR/eva-agent/eva-agent-${EVA_AGENT_CHART_VERSION}.tgz"
VLLM_CHART="$BASE_DIR/eva-agent/eva-agent-vllm-${EVA_AGENT_VLLM_CHART_VERSION}.tgz"
INIT_CHART="$BASE_DIR/eva-agent/eva-agent-init-${EVA_AGENT_INIT_CHART_VERSION}.tgz"
QDRANT_CHART="$BASE_DIR/qdrant/qdrant-${QDRANT_CHART_VERSION}.tgz"
IAM_CHART="$BASE_DIR/eva-iam/eva-iam-${EVA_IAM_CHART_VERSION}.tgz"
RELEASE_DIR="$BASE_DIR/eva-agent/release/${EVA_AGENT_RELEASE}"

required_files=()
want eva-app          && required_files+=("$APP_CHART")
want eva-vision       && required_files+=("$VISION_CHART")
want eva-iam          && required_files+=("$IAM_CHART")
want eva-agent-init   && required_files+=("$INIT_CHART" "$RELEASE_DIR/eva-agent-init/values-k3s.yaml")
want eva-agent-qdrant && required_files+=("$QDRANT_CHART" "$RELEASE_DIR/eva-agent-qdrant/values-k3s.yaml")
want eva-agent-vllm   && required_files+=("$VLLM_CHART" "$RELEASE_DIR/eva-agent-vllm/${EVA_AGENT_VLLM_VALUES_FILE}")
want eva-agent        && required_files+=("$AGENT_CHART" "$RELEASE_DIR/eva-agent/values-k3s.yaml" "$RELEASE_DIR/eva-agent/values-secret.yaml")

for f in "${required_files[@]}"; do
  [[ -f "$f" ]] || { echo "[ERROR] missing file: $f"; echo "[hint] 먼저 ./install/download_offline_assets.sh 실행"; exit 1; }
done

echo "[info] Rendering charts to discover image list..."
# 이전 실행이 남긴 렌더 결과가 섞이면 안 받은 컴포넌트의 이미지까지 목록에 들어옵니다.
rm -f "$RENDER_DIR"/*.yaml
want eva-app          && helm template eva-app "$APP_CHART" > "$RENDER_DIR/eva-app.yaml"
want eva-vision       && helm template eva-vision "$VISION_CHART" > "$RENDER_DIR/eva-vision.yaml"
want eva-agent-init   && helm template eva-agent-init "$INIT_CHART" -f "$RELEASE_DIR/eva-agent-init/values-k3s.yaml" > "$RENDER_DIR/eva-agent-init.yaml"
want eva-agent-qdrant && helm template eva-agent-qdrant "$QDRANT_CHART" -f "$RELEASE_DIR/eva-agent-qdrant/values-k3s.yaml" > "$RENDER_DIR/eva-agent-qdrant.yaml"
want eva-agent-vllm   && helm template eva-agent-vllm "$VLLM_CHART" -f "$RELEASE_DIR/eva-agent-vllm/${EVA_AGENT_VLLM_VALUES_FILE}" > "$RENDER_DIR/eva-agent-vllm.yaml"
want eva-agent        && helm template eva-agent "$AGENT_CHART" -f "$RELEASE_DIR/eva-agent/values-k3s.yaml" -f "$RELEASE_DIR/eva-agent/values-secret.yaml" > "$RENDER_DIR/eva-agent.yaml"
# imagePullSecrets off = airgap 렌더. 켜두면 ECR 로그인 cronjob 의 amazon/aws-cli 까지
# 목록에 들어오는데, airgap 에서는 쓰이지 않습니다.
want eva-iam          && helm template eva-iam "$IAM_CHART" \
  --set imagePullSecrets.enabled=false --set imagePullSecrets.create=false > "$RENDER_DIR/eva-iam.yaml"

awk '
  /^[[:space:]]*image:[[:space:]]*/ {
    val=$2
    gsub(/"/,"",val)
    gsub(/\047/,"",val)
    if (val != "" && val !~ /\{\{/ && val !~ /^$/) print val
  }
' "$RENDER_DIR"/*.yaml | sort -u > "$IMAGE_DIR/images-all.txt"

image_count="$(wc -l < "$IMAGE_DIR/images-all.txt" | tr -d ' ')"
echo "[info] Found $image_count images"

if command -v aws >/dev/null 2>&1; then
  awk -F/ '/\.dkr\.ecr\..*\.amazonaws\.com\//{print $1}' "$IMAGE_DIR/images-all.txt" | sort -u > "$TMP_DIR/ecr-hosts.txt"
  while IFS= read -r host; do
    [[ -z "$host" ]] && continue
    region="$(echo "$host" | sed -E 's#^[0-9]+\.dkr\.ecr\.([^.]+)\.amazonaws\.com$#\1#')"
    echo "[auth] aws ecr login: $host ($region)"
    aws ecr get-login-password --region "$region" --profile "${AWS_PROFILE:-default}" | $DOCKER_CMD login --username AWS --password-stdin "$host" || true
  done < "$TMP_DIR/ecr-hosts.txt"
fi

: > "$IMAGE_DIR/images-pulled.txt"
: > "$IMAGE_DIR/images-missing.txt"

while IFS= read -r image; do
  [[ -z "$image" ]] && continue
  echo "[pull] $image"
  if $DOCKER_CMD pull "$image"; then
    echo "$image" >> "$IMAGE_DIR/images-pulled.txt"
  else
    echo "$image" >> "$IMAGE_DIR/images-missing.txt"
  fi
done < "$IMAGE_DIR/images-all.txt"

pulled_count="$(wc -l < "$IMAGE_DIR/images-pulled.txt" | tr -d ' ')"
missing_count="$(wc -l < "$IMAGE_DIR/images-missing.txt" | tr -d ' ')"
echo "[info] pulled=$pulled_count missing=$missing_count"

echo "[done] image list: $IMAGE_DIR/images-all.txt"
echo "[done] pulled list: $IMAGE_DIR/images-pulled.txt"
echo "[done] missing list: $IMAGE_DIR/images-missing.txt"
echo "[done] push pulled images with: REPOSITORY_REGISTRY=<harbor-host> ./install/push_images_to_repository.sh"
