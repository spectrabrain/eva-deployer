#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="${BASE_DIR:-$SCRIPT_DIR}"
SNAPSHOT_DIR="${BASE_DIR}/qdrant-snapshots"
AWS_PROFILE="${AWS_PROFILE:-default}"
AWS_REGION="${AWS_REGION:-ap-northeast-2}"
EVA_QDRANT_SNAPSHOT_BUCKET="${EVA_QDRANT_SNAPSHOT_BUCKET:-s3-an2-mellerikat-release-eva-agent}"

source "$SCRIPT_DIR/load_versions.sh"
load_deploy_versions

EVA_AGENT_RELEASE="${EVA_AGENT_RELEASE:?missing EVA_AGENT_RELEASE (set in versions.json)}"
EVA_AGENT_QDRANT_SNAPSHOT_SOURCE="${EVA_AGENT_QDRANT_SNAPSHOT_SOURCE:-local_pv}"
case "$EVA_AGENT_QDRANT_SNAPSHOT_SOURCE" in
  local_pv) qdrant_default_values_file="values-k3s.yaml" ;;
  harbor)   qdrant_default_values_file="values-k3s.harbor.yaml" ;;
  *)
    echo "[ERROR] EVA_AGENT_QDRANT_SNAPSHOT_SOURCE must be local_pv or harbor" >&2
    exit 1
    ;;
esac
EVA_AGENT_QDRANT_VALUES_FILE="${EVA_AGENT_QDRANT_VALUES_FILE:-$qdrant_default_values_file}"
echo "[info] Qdrant snapshot source=${EVA_AGENT_QDRANT_SNAPSHOT_SOURCE}, values=${EVA_AGENT_QDRANT_VALUES_FILE}"
VALUES_FILE="${BASE_DIR}/eva-agent/release/${EVA_AGENT_RELEASE}/eva-agent-qdrant/${EVA_AGENT_QDRANT_VALUES_FILE}"

if ! command -v aws >/dev/null 2>&1; then
  echo "[ERROR] aws CLI not found"
  exit 1
fi

if [[ ! -f "$VALUES_FILE" ]]; then
  echo "[ERROR] missing file: $VALUES_FILE"
  echo "[hint] 먼저 ./install/download_offline_assets.sh 실행"
  exit 1
fi

mkdir -p "$SNAPSHOT_DIR"

# Extract the SNAPSHOT_SPECS block scalar value (one "s3Directory|snapshotFile|logicalCollection"
# line per collection) from the eva-agent-qdrant chart values so this script and the deployed
# sidecar always agree on what "SNAPSHOT_SPECS" means - the chart values file stays the single
# source of truth.
snapshot_specs="$(awk '
  BEGIN { invalue = 0; keyindent = -1 }
  found && !invalue && $0 ~ /value:[ \t]*\|/ {
    invalue = 1
    match($0, /^[ \t]*/)
    keyindent = RLENGTH
    next
  }
  $0 ~ /- name:[ \t]*SNAPSHOT_SPECS[ \t]*$/ { found = 1; next }
  invalue {
    if ($0 !~ /[^ \t]/) { next }
    match($0, /^[ \t]*/)
    if (RLENGTH <= keyindent) { exit }
    line = $0
    sub(/^[ \t]+/, "", line)
    print line
  }
' "$VALUES_FILE")"

if [[ -z "$snapshot_specs" ]]; then
  echo "[ERROR] SNAPSHOT_SPECS를 $VALUES_FILE 에서 찾지 못했습니다"
  exit 1
fi

: > "$SNAPSHOT_DIR/manifest.txt"
{
  echo "Generated: $(date -Iseconds)"
  echo "bucket: ${EVA_QDRANT_SNAPSHOT_BUCKET}"
  echo "aws_profile: ${AWS_PROFILE}"
  echo "aws_region: ${AWS_REGION}"
  echo "snapshot_specs:"
  echo "${snapshot_specs}"
  echo "files:"
} >> "$SNAPSHOT_DIR/manifest.txt"

while IFS='|' read -r s3_directory snapshot_file logical_collection ignored; do
  [[ -z "${s3_directory}${snapshot_file}${logical_collection}" || "${s3_directory}" == \#* ]] && continue
  if [[ -z "${snapshot_file}" ]]; then
    echo "[WARN] skip spec without snapshotFile: ${s3_directory}|${snapshot_file}|${logical_collection}"
    continue
  fi
  if [[ -z "${s3_directory}" ]]; then
    echo "[WARN] skip spec with empty s3Directory (already offline?): ${snapshot_file}"
    continue
  fi

  dest="${SNAPSHOT_DIR}/${snapshot_file}"
  remote="s3://${EVA_QDRANT_SNAPSHOT_BUCKET}/agent/qdrant/${s3_directory}/${snapshot_file}"
  echo "[download] ${remote} -> ${dest}"
  aws --profile "${AWS_PROFILE}" --region "${AWS_REGION}" s3 cp "${remote}" "${dest}"
done <<< "$snapshot_specs"

find "$SNAPSHOT_DIR" -maxdepth 1 -type f ! -name manifest.txt | sort >> "$SNAPSHOT_DIR/manifest.txt"

echo "[done] Qdrant snapshot bootstrap files downloaded under ${SNAPSHOT_DIR}"
