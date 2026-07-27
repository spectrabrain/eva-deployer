#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="${BASE_DIR:-$SCRIPT_DIR}"
MODEL_DIR="${BASE_DIR}/models"
AWS_PROFILE="${AWS_PROFILE:-default}"
AWS_REGION="${AWS_REGION:-ap-northeast-2}"
EVA_MODEL_BUCKET="${EVA_MODEL_BUCKET:-s3-an2-mellerikat-release-eva-agent}"

AGENT_PREFIX="${AGENT_PREFIX:-agent/hf}"
VLLM_PREFIX="${VLLM_PREFIX:-vllm/hf}"

if ! command -v aws >/dev/null 2>&1; then
  echo "[ERROR] aws CLI not found"
  exit 1
fi

mkdir -p "${MODEL_DIR}/agent/hf" "${MODEL_DIR}/vllm/hf"

echo "[sync] s3://${EVA_MODEL_BUCKET}/${AGENT_PREFIX} -> ${MODEL_DIR}/agent/hf"
aws --profile "${AWS_PROFILE}" --region "${AWS_REGION}" s3 sync \
  "s3://${EVA_MODEL_BUCKET}/${AGENT_PREFIX}" \
  "${MODEL_DIR}/agent/hf"

echo "[sync] s3://${EVA_MODEL_BUCKET}/${VLLM_PREFIX} -> ${MODEL_DIR}/vllm/hf"
aws --profile "${AWS_PROFILE}" --region "${AWS_REGION}" s3 sync \
  "s3://${EVA_MODEL_BUCKET}/${VLLM_PREFIX}" \
  "${MODEL_DIR}/vllm/hf"

cat > "${MODEL_DIR}/manifest.txt" <<MANIFEST
Generated: $(date -Iseconds)
bucket: ${EVA_MODEL_BUCKET}
agent_prefix: ${AGENT_PREFIX}
vllm_prefix: ${VLLM_PREFIX}
aws_profile: ${AWS_PROFILE}
aws_region: ${AWS_REGION}
files:
$(find "${MODEL_DIR}" -type f | sort)
MANIFEST

echo "[done] EVA model cache downloaded under ${MODEL_DIR}"
