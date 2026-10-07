#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
backend="$repo_root/scripts/publish/push_images_to_repository.sh"

bash -n "$backend"
# shellcheck disable=SC2016 # These checks intentionally match literal backend text.
grep -Fq 'MAPPING_FILE="${REPOSITORY_MAPPING_FILE:-$IMAGE_DIR/repository-mapping.txt}"' "$backend"
# shellcheck disable=SC2016 # These checks intentionally match literal backend text.
grep -Fq 'REPOSITORY_MIRROR_PATH_IMAGES="${REPOSITORY_MIRROR_PATH_IMAGES:-true}"' "$backend"

echo "[OK] Remote prepare publish backend contract"
