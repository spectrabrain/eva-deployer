#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
EVA_CACHE_ROOT="${EVA_CACHE_ROOT:-$REPO_ROOT/out/cache}"
MODEL_DIR="${EVA_CACHE_ROOT}/models"
AWS_PROFILE="${AWS_PROFILE:-default}"
AWS_REGION="${AWS_REGION:-ap-northeast-2}"
source "$REPO_ROOT/scripts/lib/load_versions.sh"
load_deploy_versions
EVA_AGENT_RELEASE="${EVA_AGENT_RELEASE:?missing EVA_AGENT_RELEASE}"
EVA_MODEL_BUCKET="${EVA_MODEL_BUCKET:-$(eva_agent_source bucket "$EVA_AGENT_RELEASE")}"
AGENT_MODELS_REQUIRED="$(eva_agent_source models-required "$EVA_AGENT_RELEASE")"

AWS_ARGS=(--region "${AWS_REGION}")
if [[ -z "${AWS_ACCESS_KEY_ID:-}" ||
      -z "${AWS_SECRET_ACCESS_KEY:-}" ]]; then
  AWS_ARGS+=(--profile "${AWS_PROFILE}")
fi

AGENT_PREFIX="${AGENT_PREFIX:-agent/hf}"
VLLM_PREFIX="${VLLM_PREFIX:-vllm/hf}"

if ! command -v aws >/dev/null 2>&1; then
  echo "[ERROR] aws CLI not found"
  exit 1
fi

model_trees=("${MODEL_DIR}/vllm/hf")
if [[ "$AGENT_MODELS_REQUIRED" == "true" ]]; then
  model_trees+=("${MODEL_DIR}/agent/hf")
fi
mkdir -p "${model_trees[@]}"

if [[ "$AGENT_MODELS_REQUIRED" == "true" ]]; then
  echo "[sync] s3://${EVA_MODEL_BUCKET}/${AGENT_PREFIX} -> ${MODEL_DIR}/agent/hf"
  aws "${AWS_ARGS[@]}" s3 sync \
    "s3://${EVA_MODEL_BUCKET}/${AGENT_PREFIX}" \
    "${MODEL_DIR}/agent/hf"
fi

echo "[sync] s3://${EVA_MODEL_BUCKET}/${VLLM_PREFIX} -> ${MODEL_DIR}/vllm/hf"
aws "${AWS_ARGS[@]}" s3 sync \
  "s3://${EVA_MODEL_BUCKET}/${VLLM_PREFIX}" \
  "${MODEL_DIR}/vllm/hf"

manifest_path="${MODEL_DIR}/manifest.txt"
manifest_temp="$(mktemp "${MODEL_DIR}/.manifest.XXXXXX")"

cleanup_manifest_temp() {
  rm -f "${manifest_temp}"
}
trap cleanup_manifest_temp EXIT

mapfile -t model_files < <(
  find "${model_trees[@]}" -type f -size +0c -print | sort
)

if [[ "${#model_files[@]}" -eq 0 ]]; then
  echo "[ERROR] EVA model cache has no non-empty files" >&2
  exit 1
fi

{
  echo "Generated: $(date -Iseconds)"
  echo "bucket: ${EVA_MODEL_BUCKET}"
  echo "agent_models_required: ${AGENT_MODELS_REQUIRED}"
  echo "agent_prefix: ${AGENT_PREFIX}"
  echo "vllm_prefix: ${VLLM_PREFIX}"
  echo "files:"
  printf '%s\n' "${model_files[@]}"
} > "${manifest_temp}"

chmod 0640 "${manifest_temp}"
mv -f "${manifest_temp}" "${manifest_path}"
trap - EXIT

echo "[done] EVA model cache downloaded under ${MODEL_DIR}"
