#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="${BASE_DIR:-$SCRIPT_DIR}"
IMAGE_DIR="$BASE_DIR/images"

DOCKER_CMD="${DOCKER_CMD:-docker}"
INFRA_IMAGES=(
  "${K3S_PAUSE_IMAGE:-rancher/mirrored-pause:3.6}"
  "${NVIDIA_DEVICE_PLUGIN_IMAGE:-nvcr.io/nvidia/k8s-device-plugin:v0.18.0}"
  "${NVIDIA_CUDA_SAMPLE_IMAGE:-nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda12.5.0}"
  "${NVIDIA_CUDA_VALIDATION_IMAGE:-nvcr.io/nvidia/cuda:12.5.0-base-ubuntu22.04}"
  "${NFS_CSI_NFS_IMAGE:-registry.k8s.io/sig-storage/nfsplugin:v4.11.0}"
  "${NFS_CSI_PROVISIONER_IMAGE:-registry.k8s.io/sig-storage/csi-provisioner:v5.2.0}"
  "${NFS_CSI_RESIZER_IMAGE:-registry.k8s.io/sig-storage/csi-resizer:v1.13.1}"
  "${NFS_CSI_SNAPSHOTTER_IMAGE:-registry.k8s.io/sig-storage/csi-snapshotter:v8.2.0}"
  "${NFS_CSI_LIVENESS_PROBE_IMAGE:-registry.k8s.io/sig-storage/livenessprobe:v2.15.0}"
  "${NFS_CSI_NODE_REGISTRAR_IMAGE:-registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.13.0}"
  "${NFS_CSI_SNAPSHOT_CONTROLLER_IMAGE:-registry.k8s.io/sig-storage/snapshot-controller:v8.2.0}"
)

command -v docker >/dev/null 2>&1 || { echo "[ERROR] docker not found"; exit 1; }

mkdir -p "$IMAGE_DIR"

: > "$IMAGE_DIR/infra-images-all.txt"
for image in "${INFRA_IMAGES[@]}"; do
  [[ -n "$image" ]] && echo "$image" >> "$IMAGE_DIR/infra-images-all.txt"
done
sort -u "$IMAGE_DIR/infra-images-all.txt" -o "$IMAGE_DIR/infra-images-all.txt"

: > "$IMAGE_DIR/infra-images-pulled.txt"
: > "$IMAGE_DIR/infra-images-missing.txt"

while IFS= read -r image; do
  [[ -z "$image" ]] && continue
  echo "[pull] $image"
  if $DOCKER_CMD pull "$image"; then
    echo "$image" >> "$IMAGE_DIR/infra-images-pulled.txt"
  else
    echo "$image" >> "$IMAGE_DIR/infra-images-missing.txt"
  fi
done < "$IMAGE_DIR/infra-images-all.txt"

if [[ -n "${K3S_PAUSE_IMAGE:-}" || -f "$IMAGE_DIR/infra-images-pulled.txt" ]]; then
  pause_image="${K3S_PAUSE_IMAGE:-rancher/mirrored-pause:3.6}"
  if grep -Fxq "$pause_image" "$IMAGE_DIR/infra-images-pulled.txt"; then
    echo "[save] k3s pause image -> $IMAGE_DIR/k3s-images.tar"
    "$DOCKER_CMD" save -o "$IMAGE_DIR/k3s-images.tar" "$pause_image"
  fi
fi

pulled_count="$(wc -l < "$IMAGE_DIR/infra-images-pulled.txt" | tr -d ' ')"
missing_count="$(wc -l < "$IMAGE_DIR/infra-images-missing.txt" | tr -d ' ')"
echo "[info] infra pulled=$pulled_count missing=$missing_count"

echo "[done] image list: $IMAGE_DIR/infra-images-all.txt"
echo "[done] pulled list: $IMAGE_DIR/infra-images-pulled.txt"
echo "[done] missing list: $IMAGE_DIR/infra-images-missing.txt"
echo "[done] push pulled images with: IMAGE_LIST=$IMAGE_DIR/infra-images-pulled.txt REPOSITORY_REGISTRY=<harbor-host> ./install/push_images_to_repository.sh"
