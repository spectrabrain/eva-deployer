#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
RCLONE_REMOTE="${RCLONE_REMOTE:-gdrive:}"
RCLONE_DRIVE_ROOT_FOLDER_ID="${RCLONE_DRIVE_ROOT_FOLDER_ID:-1PFmDAIxj-JzlEWgnWqLFKLfqz4ni5Xha}"
RCLONE_DESTINATION="${RCLONE_DESTINATION:-$REPO_ROOT}"
RCLONE_DRY_RUN="${RCLONE_DRY_RUN:-true}"

command -v rclone >/dev/null 2>&1 || {
  echo "[ERROR] rclone is required. Configure a remote first with: rclone config" >&2
  exit 1
}

case "$RCLONE_DRY_RUN" in
  true) dry_run_args=(--dry-run) ;;
  false) dry_run_args=() ;;
  *)
    echo "[ERROR] RCLONE_DRY_RUN must be true or false: $RCLONE_DRY_RUN" >&2
    exit 1
    ;;
esac

echo "[sync] $RCLONE_REMOTE -> $RCLONE_DESTINATION"
rclone copy "$RCLONE_REMOTE" "$RCLONE_DESTINATION" \
  --drive-root-folder-id "$RCLONE_DRIVE_ROOT_FOLDER_ID" \
  --progress \
  "${dry_run_args[@]}"
