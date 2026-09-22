#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
backend="$repo_root/scripts/publish/push_images_to_repository.sh"
registry="10.159.57.172:32080"
temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT

home="$temporary/home"
override="$temporary/override"
cache="$temporary/cache"
mkdir -p "$home/.docker" "$override" "$cache/images"
: > "$cache/images/images-pulled.txt"

write_config() {
  local directory="$1" key="$2"
  mkdir -p "$directory"
  printf '{"auths":{"%s":{}}}\n' "$key" > "$directory/config.json"
}

run_backend() {
  HOME="$home" EVA_CACHE_ROOT="$cache" IMAGE_LIST="$cache/images/images-pulled.txt" \
    REPOSITORY_REGISTRY="$registry" REPOSITORY_AUTO_LOGIN=true "$@" bash "$backend"
}

write_config "$override" "$registry"
output="$(run_backend env DOCKER_CONFIG="$override")"
[[ "$output" == *"using existing Docker credential"* ]] || { echo '[ERROR] DOCKER_CONFIG registry key was not used' >&2; exit 1; }

write_config "$override" "https://$registry"
output="$(run_backend env DOCKER_CONFIG="$override")"
[[ "$output" == *"using existing Docker credential"* ]] || { echo '[ERROR] DOCKER_CONFIG https registry key was not used' >&2; exit 1; }

write_config "$home/.docker" "$registry"
printf '{"auths":{}}\n' > "$override/config.json"
output="$(run_backend env DOCKER_CONFIG="$override")"
[[ "$output" != *"using existing Docker credential"* ]] || { echo '[ERROR] default Docker config incorrectly overrode DOCKER_CONFIG' >&2; exit 1; }

output="$(run_backend env DOCKER_CONFIG='')"
[[ "$output" == *"using existing Docker credential"* ]] || { echo '[ERROR] empty DOCKER_CONFIG did not use default Docker config' >&2; exit 1; }

printf '{malformed' > "$override/config.json"
output="$(run_backend env DOCKER_CONFIG="$override")"
[[ "$output" != *"using existing Docker credential"* ]] || { echo '[ERROR] malformed Docker config was accepted' >&2; exit 1; }

echo '[OK] Docker credential store contract'
