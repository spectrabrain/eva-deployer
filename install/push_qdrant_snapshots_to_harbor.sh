#!/usr/bin/env bash
set -euo pipefail

# Store each Qdrant snapshot as a single-file OCI artifact in Harbor.  The
# Harbor values profile consumes the same SNAPSHOT_SPECS block and pulls these
# artifacts with ORAS from the qdrant-snapshot-sync sidecar.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="${BASE_DIR:-$SCRIPT_DIR}"
SNAPSHOT_DIR="${SNAPSHOT_DIR:-$BASE_DIR/qdrant-snapshots}"
REPOSITORY_REGISTRY="${REPOSITORY_REGISTRY:-${HARBOR_REGISTRY:-}}"
REPOSITORY_PROJECT="${REPOSITORY_PROJECT:-eva}"
REPOSITORY_USERNAME="${REPOSITORY_USERNAME:-${HARBOR_ADMIN_USER:-admin}}"
REPOSITORY_PASSWORD="${REPOSITORY_PASSWORD:-${HARBOR_ADMIN_PASSWORD:-}}"
EVA_AGENT_QDRANT_VALUES_FILE="${EVA_AGENT_QDRANT_VALUES_FILE:-values-k3s.harbor.yaml}"
ORAS_CMD="${ORAS_CMD:-$BASE_DIR/tools/oras}"

source "$SCRIPT_DIR/load_versions.sh"
load_deploy_versions
EVA_AGENT_RELEASE="${EVA_AGENT_RELEASE:?missing EVA_AGENT_RELEASE (set in versions.json)}"
VALUES_FILE="$BASE_DIR/eva-agent/release/${EVA_AGENT_RELEASE}/eva-agent-qdrant/${EVA_AGENT_QDRANT_VALUES_FILE}"

if [[ ! -x "$ORAS_CMD" ]]; then
  ORAS_CMD="${ORAS_CMD_FALLBACK:-oras}"
fi
for bin in "$ORAS_CMD" awk; do
  command -v "$bin" >/dev/null 2>&1 || { echo "[ERROR] $bin not found" >&2; exit 1; }
done
[[ -n "$REPOSITORY_REGISTRY" ]] || { echo "[ERROR] REPOSITORY_REGISTRY or HARBOR_REGISTRY is required" >&2; exit 1; }
[[ -d "$SNAPSHOT_DIR" ]] || { echo "[ERROR] snapshot directory not found: $SNAPSHOT_DIR" >&2; exit 1; }
[[ -f "$VALUES_FILE" ]] || { echo "[ERROR] values file not found: $VALUES_FILE" >&2; exit 1; }

REPOSITORY_REGISTRY="${REPOSITORY_REGISTRY#http://}"
REPOSITORY_REGISTRY="${REPOSITORY_REGISTRY#https://}"
REPOSITORY_REGISTRY="${REPOSITORY_REGISTRY%/}"

if [[ -z "$REPOSITORY_PASSWORD" && "$REPOSITORY_REGISTRY" == "localhost:32080" ]]; then
  HARBOR_YML="${LOCAL_HARBOR_YML:-$HOME/.local/share/eva-harbor/harbor/harbor.yml}"
  if [[ -f "$HARBOR_YML" ]]; then
    # harbor.yml commonly quotes the password.  Do not pass those YAML quotes
    # to ORAS as part of the password (which turns a valid credential into 401).
    REPOSITORY_PASSWORD="$(python3 - "$HARBOR_YML" <<'PY'
import sys

path = sys.argv[1]
for line in open(path, encoding="utf-8"):
    if line.lstrip().startswith("harbor_admin_password:"):
        value = line.split(":", 1)[1].strip()
        if len(value) >= 2 and value[0] in "\"'" and value[-1] == value[0]:
            value = value[1:-1]
        else:
            value = value.split(" #", 1)[0].rstrip()
        print(value)
        break
PY
)"
  fi
fi
[[ -n "$REPOSITORY_PASSWORD" ]] || { echo "[ERROR] REPOSITORY_PASSWORD or HARBOR_ADMIN_PASSWORD is required" >&2; exit 1; }

snapshot_specs="$(awk '
  BEGIN { invalue = 0; keyindent = -1 }
  found && !invalue && $0 ~ /value:[ \t]*\|/ { invalue = 1; match($0, /^[ \t]*/); keyindent = RLENGTH; next }
  $0 ~ /- name:[ \t]*SNAPSHOT_SPECS[ \t]*$/ { found = 1; next }
  invalue {
    if ($0 !~ /[^ \t]/) { next }
    match($0, /^[ \t]*/)
    if (RLENGTH <= keyindent) { exit }
    line = $0; sub(/^[ \t]+/, "", line); print line
  }
' "$VALUES_FILE")"
[[ -n "$snapshot_specs" ]] || { echo "[ERROR] SNAPSHOT_SPECS not found in $VALUES_FILE" >&2; exit 1; }

# Keep credentials out of the user's Docker/ORAS config.  The artifact seed is
# often run as root from Ansible, so persisting them there is especially
# surprising and makes airgap verification harder to reproduce.
ORAS_REGISTRY_CONFIG="$(mktemp "${TMPDIR:-/tmp}/eva-oras-registry.XXXXXX")"
trap 'rm -f "$ORAS_REGISTRY_CONFIG"' EXIT
printf '%s' "$REPOSITORY_PASSWORD" | "$ORAS_CMD" login --plain-http "$REPOSITORY_REGISTRY" \
  --username "$REPOSITORY_USERNAME" --password-stdin \
  --registry-config "$ORAS_REGISTRY_CONFIG"

# The deployer bundle may be mounted read-only while testing an air-gapped VM.
# The manifest is only a local report, so use /tmp when the snapshot bundle is
# not writable (or let callers select a persistent output path explicitly).
manifest="${HARBOR_ARTIFACT_MANIFEST:-$SNAPSHOT_DIR/harbor-artifacts.txt}"
if ! touch "$manifest" 2>/dev/null; then
  manifest="${TMPDIR:-/tmp}/qdrant-harbor-artifacts.txt"
fi
: > "$manifest"
while IFS='|' read -r artifact_tag snapshot_file logical_collection ignored; do
  [[ -z "${artifact_tag}${snapshot_file}${logical_collection}" || "$artifact_tag" == \#* ]] && continue
  [[ -n "$artifact_tag" && -n "$snapshot_file" ]] || { echo "[ERROR] invalid SNAPSHOT_SPECS line" >&2; exit 1; }
  snapshot_path="$SNAPSHOT_DIR/$snapshot_file"
  [[ -s "$snapshot_path" ]] || { echo "[ERROR] snapshot missing or empty: $snapshot_path" >&2; exit 1; }
  artifact_ref="$REPOSITORY_REGISTRY/$REPOSITORY_PROJECT/qdrant-snapshots:$artifact_tag"
  if "$ORAS_CMD" manifest fetch --plain-http \
    --registry-config "$ORAS_REGISTRY_CONFIG" "$artifact_ref" >/dev/null 2>&1; then
    echo "[skip] $artifact_ref already exists"
  else
    echo "[push] $snapshot_path -> $artifact_ref"
    # Give ORAS a path relative to the snapshot directory.  An absolute source
    # path becomes the OCI layer title; ORAS pull intentionally refuses that
    # title as path traversal inside the pod.
    (
      cd "$SNAPSHOT_DIR"
      "$ORAS_CMD" push --plain-http --registry-config "$ORAS_REGISTRY_CONFIG" "$artifact_ref" \
        "$snapshot_file:application/vnd.mellerikat.qdrant.snapshot.v1"
    )
  fi
  printf '%s|%s|%s\n' "$artifact_ref" "$snapshot_file" "$logical_collection" >> "$manifest"
done <<< "$snapshot_specs"

echo "[done] Harbor artifact manifest: $manifest"
