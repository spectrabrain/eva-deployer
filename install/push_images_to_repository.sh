#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="${BASE_DIR:-$SCRIPT_DIR}"
IMAGE_DIR="$BASE_DIR/images"
IMAGE_LIST="${IMAGE_LIST:-$IMAGE_DIR/images-pulled.txt}"
REPOSITORY_REGISTRY="${REPOSITORY_REGISTRY:-${HARBOR_REGISTRY:-}}"
REPOSITORY_PROJECT="${REPOSITORY_PROJECT:-eva}"
DOCKER_CMD="${DOCKER_CMD:-docker}"
PULL_SOURCE_IMAGES="${PULL_SOURCE_IMAGES:-true}"
REPOSITORY_AUTO_LOGIN="${REPOSITORY_AUTO_LOGIN:-true}"
REPOSITORY_USERNAME="${REPOSITORY_USERNAME:-${HARBOR_ADMIN_USER:-admin}}"
REPOSITORY_PASSWORD="${REPOSITORY_PASSWORD:-${HARBOR_ADMIN_PASSWORD:-}}"
LOCAL_HARBOR_YML="${LOCAL_HARBOR_YML:-$HOME/.local/share/eva-harbor/harbor/harbor.yml}"
# 차트가 이미지 주소를 values 로 바꾸지 못하게 하드코딩해 둔 것들 (eva-iam 3.1.0 등).
# 주소 없는 이름은 kubelet 이 docker.io 로 해석하므로 k3s registries.yaml 의 docker.io
# mirror 로 Harbor 로 돌리는데, mirror 는 host 만 치환하고 경로는 그대로 보냅니다.
# 따라서 이 이미지들만은 평탄화(<project>/<name>) 하지 않고 Docker Hub 경로 그대로
# (library/busybox, bitnami/kubectl) push 해야 합니다.
# 지정하지 않으면 아래에서 IMAGE_LIST 를 훑어 자동으로 고릅니다 (레지스트리 주소가 없는 이미지).
MIRROR_PATH_IMAGES="${MIRROR_PATH_IMAGES:-}"
LEGACY_LOCAL_HARBOR_YML="$SCRIPT_DIR/harbor/harbor/harbor.yml"

if [[ -z "$REPOSITORY_REGISTRY" ]]; then
  echo "[ERROR] REPOSITORY_REGISTRY or HARBOR_REGISTRY is required"
  echo "        example: REPOSITORY_REGISTRY=harbor.main.local ./install/push_images_to_repository.sh"
  exit 1
fi

if [[ ! -f "$IMAGE_LIST" ]]; then
  echo "[ERROR] image list not found: $IMAGE_LIST"
  echo "        run a download_*_images.sh script first"
  exit 1
fi

REPOSITORY_REGISTRY="${REPOSITORY_REGISTRY#http://}"
REPOSITORY_REGISTRY="${REPOSITORY_REGISTRY#https://}"
REPOSITORY_REGISTRY="${REPOSITORY_REGISTRY%/}"
TARGET_PREFIX="${REPOSITORY_REGISTRY}/${REPOSITORY_PROJECT}"
MAPPING_FILE="$IMAGE_DIR/repository-mapping.txt"

read_local_harbor_password() {
  python3 - "$LOCAL_HARBOR_YML" "$LEGACY_LOCAL_HARBOR_YML" <<'PY'
import re
import sys
from pathlib import Path

for filename in sys.argv[1:]:
    path = Path(filename)
    if not path.is_file():
        continue

    for line in path.read_text().splitlines():
        match = re.match(r'^harbor_admin_password:\s*(.+?)\s*$', line)
        if match:
            print(match.group(1).strip().strip('"\''))
            raise SystemExit(0)
PY
}

has_docker_credential() {
  python3 - "$REPOSITORY_REGISTRY" <<'PY'
import json
import sys
from pathlib import Path

path = Path.home() / '.docker' / 'config.json'
try:
    config = json.loads(path.read_text())
except (FileNotFoundError, json.JSONDecodeError):
    raise SystemExit(1)

raise SystemExit(0 if sys.argv[1] in config.get('auths', {}) else 1)
PY
}

login_repository() {
  if [[ "$REPOSITORY_AUTO_LOGIN" != "true" ]]; then
    return
  fi

  if [[ "$REPOSITORY_REGISTRY" == "localhost:32080" && -z "$REPOSITORY_PASSWORD" ]] \
    && has_docker_credential; then
    echo "[login] using existing Docker credential for $REPOSITORY_REGISTRY"
    return
  fi

  if [[ -z "$REPOSITORY_PASSWORD" && "$REPOSITORY_REGISTRY" == "localhost:32080" ]]; then
    REPOSITORY_PASSWORD="$(read_local_harbor_password)"
  fi

  if [[ -z "$REPOSITORY_PASSWORD" ]]; then
    echo "[login] skipped: no registry password configured for $REPOSITORY_REGISTRY"
    echo "        Set REPOSITORY_PASSWORD or HARBOR_ADMIN_PASSWORD to log in automatically."
    return
  fi

  echo "[login] $REPOSITORY_REGISTRY -u $REPOSITORY_USERNAME"
  if ! printf '%s' "$REPOSITORY_PASSWORD" | "$DOCKER_CMD" login "$REPOSITORY_REGISTRY" \
    -u "$REPOSITORY_USERNAME" --password-stdin; then
    echo "[warn] automatic login failed; continuing with any existing Docker credential."
    echo "       If push fails, run docker login $REPOSITORY_REGISTRY with the current Harbor password."
  fi
}

mkdir -p "$IMAGE_DIR"
: > "$MAPPING_FILE"

login_repository

short_name() {
  local image_without_tag path name
  image_without_tag="${1%@*}"
  image_without_tag="${image_without_tag%:*}"
  path="${image_without_tag#*/}"
  name="${path##*/}"
  echo "$name"
}

image_tag() {
  local image="$1"
  if [[ "$image" == *@sha256:* ]]; then
    echo "sha256-${image##*@sha256:}"
  elif [[ "$image" == *:* && "${image##*/}" == *:* ]]; then
    echo "${image##*:}"
  else
    echo "latest"
  fi
}

while IFS= read -r source_image; do
  [[ -z "$source_image" ]] && continue

  target_image="${TARGET_PREFIX}/$(short_name "$source_image"):$(image_tag "$source_image")"
  echo "[map] $source_image -> $target_image"

  if [[ "$PULL_SOURCE_IMAGES" == "true" ]]; then
    echo "[pull] $source_image"
    "$DOCKER_CMD" pull "$source_image"
  fi

  echo "[tag] $target_image"
  "$DOCKER_CMD" tag "$source_image" "$target_image"

  echo "[push] $target_image"
  "$DOCKER_CMD" push "$target_image"

  echo "$source_image $target_image" >> "$MAPPING_FILE"
done < "$IMAGE_LIST"

# docker.io mirror 용 사본. 위 평탄화된 사본과 별개로, 원본 경로를 유지한 이름으로도 push 합니다.
dockerhub_path() {
  local repo="${1%:*}"
  # 단일 요소 이름(busybox)은 Docker Hub 공식 이미지이므로 library/ 를 붙입니다.
  if [[ "$repo" != */* ]]; then
    echo "library/$repo"
  else
    echo "$repo"
  fi
}

# 레지스트리 주소가 있는 이미지인지 판별합니다. 첫 요소에 '.' 이나 ':' 가 있거나 localhost 면
# 호스트로 봅니다 (registry.k8s.io/..., 339…amazonaws.com/..., localhost:32080/...).
has_registry_host() {
  [[ "$1" == */* ]] || return 1
  local first="${1%%/*}"
  [[ "$first" == *.* || "$first" == *:* || "$first" == "localhost" ]]
}

# 차트가 이미지를 주소 없이 하드코딩해 두면 kubelet 이 docker.io 로 해석하므로, k3s
# registries.yaml 의 docker.io mirror 를 타게 됩니다. mirror 는 host 만 치환하고 경로는
# 그대로 보내기 때문에 Harbor 에도 원본 경로가 필요합니다. 어느 차트가 그럴지 미리 알 수
# 없으므로, 목록에서 주소 없는 이미지를 전부 대상으로 삼습니다 (사본 몇 개가 늘 뿐입니다).
if [[ -z "$MIRROR_PATH_IMAGES" ]]; then
  while IFS= read -r image; do
    [[ -z "$image" ]] && continue
    has_registry_host "$image" || MIRROR_PATH_IMAGES+="$image "
  done < "$IMAGE_LIST"
  echo "[mirror] 자동 선택: ${MIRROR_PATH_IMAGES:-(없음)}"
fi

# Harbor 는 없는 project 로 push 하면 401 을 냅니다. bitnami/kubectl 처럼 project 가
# 새로 필요한 경로를 위해, 가능하면 미리 만들어 둡니다 (이미 있으면 409 로 조용히 넘어감).
ensure_harbor_project() {
  local project="$1" code
  [[ "$project" == "library" ]] && return 0

  # login_repository() 는 기존 docker 자격증명이 있으면 harbor.yml 을 읽지 않고 끝냅니다.
  # 그 경로로 왔으면 여기서 비밀번호를 채워야 API 호출이 됩니다 — 없으면 project 를 못 만들고
  # push 가 401 로 실패합니다.
  if [[ -z "${REPOSITORY_PASSWORD:-}" && "$REPOSITORY_REGISTRY" == "localhost:32080" ]]; then
    REPOSITORY_PASSWORD="$(read_local_harbor_password)"
  fi
  if [[ -z "${REPOSITORY_PASSWORD:-}" ]]; then
    echo "[warn] Harbor 비밀번호를 몰라 project 확인을 건너뜁니다: $project"
    echo "       REPOSITORY_PASSWORD 또는 HARBOR_ADMIN_PASSWORD 를 지정하세요."
    return 0
  fi
  code="$(curl -s -o /dev/null -w '%{http_code}' \
    -u "${REPOSITORY_USERNAME}:${REPOSITORY_PASSWORD}" \
    -X POST "http://${REPOSITORY_REGISTRY}/api/v2.0/projects" \
    -H 'Content-Type: application/json' \
    -d "{\"project_name\":\"${project}\",\"public\":true}" 2>/dev/null || true)"
  case "$code" in
    201) echo "[project] 생성: $project" ;;
    409) ;;
    *)   echo "[warn] project 확인 실패(HTTP ${code:-?}): $project — push 가 실패하면 수동 생성하세요" ;;
  esac
}

for source_image in $MIRROR_PATH_IMAGES; do
  [[ -z "$source_image" ]] && continue

  mirror_repo="$(dockerhub_path "$source_image")"
  target_image="${REPOSITORY_REGISTRY}/${mirror_repo}:$(image_tag "$source_image")"
  echo "[mirror] $source_image -> $target_image"

  ensure_harbor_project "${mirror_repo%%/*}"

  if [[ "$PULL_SOURCE_IMAGES" == "true" ]]; then
    "$DOCKER_CMD" pull "$source_image"
  fi

  "$DOCKER_CMD" tag "$source_image" "$target_image"
  if ! "$DOCKER_CMD" push "$target_image"; then
    echo "[warn] push 실패: $target_image"
    echo "       Harbor 에 '${mirror_repo%%/*}' project 가 없거나 권한이 없을 수 있습니다."
  fi

  echo "$source_image $target_image" >> "$MAPPING_FILE"
done

echo "[done] mapping: $MAPPING_FILE"
