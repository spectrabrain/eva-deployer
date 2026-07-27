#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="${BASE_DIR:-$SCRIPT_DIR}"
TARGET_DIR="${TARGET_DIR:-$BASE_DIR/display_mode_selector}"

S3_BUCKET="${S3_BUCKET:-s3-an2-spectrabrain-eva-deployer}"
S3_PREFIX="${S3_PREFIX:-display_mode_selector}"
AWS_REGION="${AWS_REGION:-ap-northeast-2}"
AWS_PROFILE="${AWS_PROFILE:-default}"

if ! command -v aws >/dev/null 2>&1; then
  echo "[ERROR] aws CLI not found"
  exit 1
fi

mkdir -p "$TARGET_DIR"

echo "[sync] s3://${S3_BUCKET}/${S3_PREFIX} -> ${TARGET_DIR}"
aws --region "$AWS_REGION" --profile "$AWS_PROFILE" s3 sync \
  "s3://${S3_BUCKET}/${S3_PREFIX}" \
  "$TARGET_DIR"

if [[ -f "$TARGET_DIR/mode_selector_1.72.0/linux/x64/displaymodeselector" ]]; then
  chmod +x "$TARGET_DIR/mode_selector_1.72.0/linux/x64/displaymodeselector" || true
fi

echo "[done] display_mode_selector downloaded"
