#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="${BASE_DIR:-$SCRIPT_DIR}"
IMAGE_DIR="${BASE_DIR}/images"
source "$SCRIPT_DIR/load_versions.sh"
load_deploy_versions

N8N_IMAGE="${N8N_IMAGE:-docker.n8n.io/n8nio/n8n:1.103.2}"
DOCKER_CMD="${DOCKER_CMD:-docker}"

read -r -a DOCKER_CMD_ARR <<< "${DOCKER_CMD}"

mkdir -p "${IMAGE_DIR}"

if ! command -v "${DOCKER_CMD_ARR[0]}" >/dev/null 2>&1; then
  echo "[ERROR] docker command not found: ${DOCKER_CMD_ARR[0]}"
  exit 1
fi

echo "[pull] ${N8N_IMAGE}"
"${DOCKER_CMD_ARR[@]}" pull "${N8N_IMAGE}"

echo "${N8N_IMAGE}" > "${IMAGE_DIR}/n8n-images.txt"

echo "[done] n8n image list: ${IMAGE_DIR}/n8n-images.txt"
echo "[done] push pulled image with: IMAGE_LIST=${IMAGE_DIR}/n8n-images.txt REPOSITORY_REGISTRY=<harbor-host> ./install/push_images_to_repository.sh"
