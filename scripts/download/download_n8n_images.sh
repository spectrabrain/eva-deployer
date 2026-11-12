#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
EVA_CACHE_ROOT="${EVA_CACHE_ROOT:-$REPO_ROOT/out/cache}"
IMAGE_DIR="$EVA_CACHE_ROOT/images"
source "$REPO_ROOT/scripts/lib/load_versions.sh"
source "$REPO_ROOT/scripts/lib/docker_pull.sh"
load_deploy_versions

N8N_IMAGE="${N8N_IMAGE:?missing N8N_IMAGE (set in src/solution/version.yaml)}"
DOCKER_CMD="${DOCKER_CMD:-docker}"

read -r -a DOCKER_CMD_ARR <<< "${DOCKER_CMD}"

mkdir -p "${IMAGE_DIR}"

if ! command -v "${DOCKER_CMD_ARR[0]}" >/dev/null 2>&1; then
  echo "[ERROR] docker command not found: ${DOCKER_CMD_ARR[0]}"
  exit 1
fi

echo "[pull] ${N8N_IMAGE}"
eva_docker_pull_image "$N8N_IMAGE" --platform "${PULL_PLATFORM:-linux/amd64}"

echo "${N8N_IMAGE}" > "${IMAGE_DIR}/n8n-images.txt"

echo "[done] n8n image list: ${IMAGE_DIR}/n8n-images.txt"
echo "[done] push pulled image with: IMAGE_LIST=${IMAGE_DIR}/n8n-images.txt REPOSITORY_REGISTRY=<harbor-host> ./scripts/publish/push_images_to_repository.sh"
