#!/usr/bin/env bash
set -euo pipefail

# Store each Qdrant snapshot as a single-file OCI artifact in Harbor.  The
# Harbor values profile consumes the same SNAPSHOT_SPECS block and pulls these
# artifacts with ORAS from the qdrant-snapshot-sync sidecar.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
EVA_CACHE_ROOT="${EVA_CACHE_ROOT:-$REPO_ROOT/out/cache}"
SNAPSHOT_DIR="${SNAPSHOT_DIR:-$EVA_CACHE_ROOT/qdrant-snapshots}"
REPOSITORY_REGISTRY="${REPOSITORY_REGISTRY:-${HARBOR_REGISTRY:-}}"
REPOSITORY_PROJECT="${REPOSITORY_PROJECT:-eva}"
REPOSITORY_USERNAME="${REPOSITORY_USERNAME:-${HARBOR_ADMIN_USER:-admin}}"
REPOSITORY_PASSWORD="${REPOSITORY_PASSWORD:-${HARBOR_ADMIN_PASSWORD:-}}"
LOCAL_HARBOR_YML="${LOCAL_HARBOR_YML:-$HOME/.local/share/eva-harbor/harbor/harbor.yml}"
EVA_AGENT_QDRANT_VALUES_FILE="${EVA_AGENT_QDRANT_VALUES_FILE:-values-k3s.harbor.yaml}"
ORAS_CMD="${ORAS_CMD:-$EVA_CACHE_ROOT/tools/oras}"

source "$REPO_ROOT/scripts/lib/load_versions.sh"
load_deploy_versions
EVA_AGENT_RELEASE="${EVA_AGENT_RELEASE:?missing EVA_AGENT_RELEASE (set in src/solution/version.yaml)}"
VALUES_FILE="$EVA_CACHE_ROOT/eva-agent/release/${EVA_AGENT_RELEASE}/eva-agent-qdrant/${EVA_AGENT_QDRANT_VALUES_FILE}"

if [[ "$ORAS_CMD" == */* ]]; then
  [[ -x "$ORAS_CMD" ]] || { echo "[ERROR] ORAS command not found: $ORAS_CMD" >&2; exit 1; }
else
  command -v "$ORAS_CMD" >/dev/null 2>&1 || { echo "[ERROR] ORAS command not found: $ORAS_CMD" >&2; exit 1; }
fi
command -v awk >/dev/null 2>&1 || { echo "[ERROR] awk not found" >&2; exit 1; }
[[ -n "$REPOSITORY_REGISTRY" ]] || { echo "[ERROR] REPOSITORY_REGISTRY or HARBOR_REGISTRY is required" >&2; exit 1; }
[[ -d "$SNAPSHOT_DIR" ]] || { echo "[ERROR] snapshot directory not found: $SNAPSHOT_DIR" >&2; exit 1; }
[[ -f "$VALUES_FILE" ]] || { echo "[ERROR] values file not found: $VALUES_FILE" >&2; exit 1; }

REPOSITORY_REGISTRY="${REPOSITORY_REGISTRY#http://}"
REPOSITORY_REGISTRY="${REPOSITORY_REGISTRY#https://}"
REPOSITORY_REGISTRY="${REPOSITORY_REGISTRY%/}"

if [[ -z "$REPOSITORY_PASSWORD" ]]; then
  # Only use the local installation's password for the endpoint configured in
  # its harbor.yml. This preserves automatic Local Harbor seeding without
  # sending that password to an unrelated remote registry.
  REPOSITORY_PASSWORD="$(python3 - "$LOCAL_HARBOR_YML" "$REPOSITORY_REGISTRY" <<'PY'
import re
import sys
from pathlib import Path

path = Path(sys.argv[1])
registry = sys.argv[2]
if not path.is_file():
    raise SystemExit(0)

hostname = None
password = None
for line in path.read_text().splitlines():
    hostname_match = re.match(r'^hostname:\s*(.+?)\s*$', line)
    password_match = re.match(r'^harbor_admin_password:\s*(.+?)\s*$', line)
    if hostname_match:
        hostname = hostname_match.group(1).split(' #', 1)[0].strip().strip('"\'')
    if password_match:
        password = password_match.group(1).split(' #', 1)[0].strip().strip('"\'')

if hostname and password and registry == f'{hostname}:32080':
    print(password)
PY
)"
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
