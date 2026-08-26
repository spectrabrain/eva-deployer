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
MIRROR_PATH_IMAGES="${MIRROR_PATH_IMAGES:-busybox:latest bitnami/kubectl:latest}"
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

for source_image in $MIRROR_PATH_IMAGES; do
  [[ -z "$source_image" ]] && continue

  target_image="${REPOSITORY_REGISTRY}/$(dockerhub_path "$source_image"):$(image_tag "$source_image")"
  echo "[mirror] $source_image -> $target_image"

  if [[ "$PULL_SOURCE_IMAGES" == "true" ]]; then
    "$DOCKER_CMD" pull "$source_image"
  fi

  "$DOCKER_CMD" tag "$source_image" "$target_image"
  if ! "$DOCKER_CMD" push "$target_image"; then
    echo "[warn] push 실패: $target_image"
    echo "       Harbor 에 '$(dockerhub_path "$source_image" | cut -d/ -f1)' project 가 없을 수 있습니다."
    echo "       Harbor UI → Projects → New Project 로 만든 뒤 다시 실행하세요."
  fi

  echo "$source_image $target_image" >> "$MAPPING_FILE"
done

echo "[done] mapping: $MAPPING_FILE"
