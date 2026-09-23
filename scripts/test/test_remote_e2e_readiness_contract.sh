#!/usr/bin/env bash
set -euo pipefail

# Static cross-file contracts only. This test never evaluates a Release,
# credentials, a Main host, Harbor, or a Remote Target.

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

require_text() {
  local file="$1" text="$2" label="$3"
  if ! grep -Fq -- "$text" "$file"; then
    echo "[ERROR] missing Remote E2E static contract: $label" >&2
    exit 1
  fi
}

main_go="$repo_root/tools/eva/cmd/eva/main.go"
current_release_go="$repo_root/tools/eva/internal/release/current.go"
backend_go="$repo_root/tools/eva/internal/remote/backend.go"
bootstrap_go="$repo_root/tools/eva/internal/remote/bootstrap.go"
docker_registry_go="$repo_root/tools/eva/internal/remote/docker_registry.go"
prepare_go="$repo_root/tools/eva/internal/remote/prepare.go"
payload_go="$repo_root/tools/eva/internal/remote/payload.go"
manifest_go="$repo_root/tools/eva/internal/remote/manifest.go"
runtime_artifact_go="$repo_root/tools/eva/internal/remote/runtime_artifact.go"
preflight_go="$repo_root/tools/eva/internal/remote/preflight.go"
aws_credential_go="$repo_root/tools/eva/internal/remote/aws_credential.go"
verify_go="$repo_root/tools/eva/internal/remote/verify.go"
verify_test="$repo_root/tools/eva/internal/remote/verify_test.go"
installer="$repo_root/scripts/install/install_eva_tool.sh"
transport="$repo_root/scripts/remote/publish_release_to_target.sh"
remote_runbook="$repo_root/docs/installation/remote-repository-runbook.md"
cloud_runbook="$repo_root/docs/installation/cloud-repository-runbook.md"
image_publish_backend="$repo_root/scripts/publish/push_images_to_repository.sh"
qdrant_publish_backend="$repo_root/scripts/publish/push_qdrant_snapshots_to_harbor.sh"
installation_index="$repo_root/docs/installation/README.md"
root_readme="$repo_root/README.md"
pr_ci="$repo_root/.github/workflows/pr-ci.yaml"
tag_ci="$repo_root/.github/workflows/tag-release.yaml"
atomic_rename="mv -- \"\$target_staging\" \"\$target_final\""

for command in bash grep; do
  command -v "$command" >/dev/null 2>&1 || { echo "[ERROR] missing test dependency: $command" >&2; exit 1; }
done

for command_line in \
  'remote bootstrap [--registry HOST[:PORT]] --yes [--replace-registry]' \
  'remote prepare [RELEASE_PATH] [--registry HOST[:PORT]]' \
  'remote verify [RELEASE_PATH] [--registry HOST[:PORT]]' \
  'remote publish [RELEASE_PATH] [--registry HOST[:PORT]] --target USER@HOST [--target USER@HOST ...]'; do
  require_text "$main_go" "$command_line" "CLI $command_line"
done
require_text "$prepare_go" 'defaultRemoteProject = "eva"' 'default repository project'
require_text "$backend_go" 'no repository or working-directory fallback' 'no checkout or CWD fallback'
require_text "$backend_go" 'Remote backend must be a regular non-symlink file' 'backend symlink rejection'
require_text "$verify_go" 'performs no backend resolution, process invocation, network access' 'read-only verify boundary'
require_text "$verify_test" 'WithoutMutation' 'verify no-mutation regression'
require_text "$pr_ci" 'go test ./...' 'PR CI Go test coverage for verify regression'
require_text "$tag_ci" 'go test ./...' 'tag CI Go test coverage for verify regression'

for backend in \
  scripts/download/download_offline_assets.sh \
  scripts/download/download_eva_images.sh \
  scripts/download/download_infra_images.sh \
  scripts/download/download_eva_models.sh \
  scripts/download/download_qdrant_snapshots.sh \
  scripts/publish/push_images_to_repository.sh \
  scripts/publish/push_qdrant_snapshots_to_harbor.sh \
  scripts/install/install_docker.sh \
  scripts/install/setup_harbor.sh \
  scripts/lib/load_versions.sh \
  scripts/remote/publish_release_to_target.sh; do
  require_text "$installer" "$backend" "packaged backend $backend"
done

require_text "$prepare_go" 'EVA_AGENT_QDRANT_SNAPSHOT_SOURCE": "harbor"' 'Qdrant Harbor source'
require_text "$prepare_go" 'Name: "main-preflight"' 'Main preflight ordered step'
require_text "$prepare_go" 'Name: "build-runtime-artifact"' 'Runtime artifact ordered step'
require_text "$runtime_artifact_go" 'BootstrapTargetRuntime' 'Remote Runtime bootstrap contract'
require_text "$aws_credential_go" '"aws", "sts", "get-caller-identity"' 'AWS credential probe'
require_text "$preflight_go" 'Docker credential for registry is unavailable' 'Harbor credential fail-closed'
require_text "$bootstrap_go" 'func dockerLogin(' 'Managed Harbor Docker login'
require_text "$bootstrap_go" '"--password-stdin"' 'Managed Harbor password stdin'
require_text "$bootstrap_go" 'prepareCredential' 'Managed Harbor credential lifecycle'
require_text "$bootstrap_go" 'WriteHarborReceipt' 'receipt write after credential validation'
require_text "$bootstrap_go" 'EnsureRegistryTransport' 'Managed Harbor registry transport lifecycle'
require_text "$docker_registry_go" 'insecure-registries' 'Managed HTTP Harbor Docker transport'
require_text "$docker_registry_go" 'func validateDockerDaemonConfig(' 'Docker daemon config validation helper'
require_text "$docker_registry_go" '"--validate"' 'dockerd config validation option'
require_text "$docker_registry_go" 'previous configuration restored' 'Docker config rollback contract'
require_text "$bootstrap_go" 'RecoverManagedHarbor' 'Managed Harbor recovery lifecycle'
require_text "$docker_registry_go" 'func recoverManagedHarbor(' 'Managed Harbor compose recovery'
require_text "$docker_registry_go" '"compose"' 'Managed Harbor Docker Compose recovery'
require_text "$docker_registry_go" '"up"' 'Managed Harbor non-destructive startup'
require_text "$image_publish_backend" "os.environ.get('DOCKER_CONFIG', '').strip()" 'image publish DOCKER_CONFIG override'
require_text "$image_publish_backend" "Path(config_root) / 'config.json'" 'image publish Docker config path'
require_text "$image_publish_backend" "f'https://{registry}'" 'image publish HTTPS registry credential key'
require_text "$preflight_go" 'DefaultExternalSources' 'bounded external source contract'
require_text "$prepare_go" 'EVA_AGENT_QDRANT_VALUES_FILE": "values-k3s.harbor.yaml"' 'Qdrant Harbor values'
require_text "$prepare_go" 'repository-mapping-product.txt' 'product mapping report'
require_text "$prepare_go" 'repository-mapping-infra.txt' 'infra mapping report'
require_text "$manifest_go" 'DefaultRemoteCacheRoot = "/var/lib/eva/cache/remote"' 'shared Remote cache default'
require_text "$prepare_go" 'CacheRoot' 'PrepareService shared cache root'
require_text "$prepare_go" '"EVA_CACHE_ROOT": cacheRoot' 'backend shared cache environment'
require_text "$prepare_go" 'defaultManagedHarborConfig = "/opt/eva/harbor/harbor/harbor.yml"' 'Managed Harbor configuration path'
require_text "$prepare_go" '"LOCAL_HARBOR_YML":         harborConfigPath' 'Qdrant Managed Harbor config handoff'
require_text "$qdrant_publish_backend" 'registry == f'"'"'{hostname}:32080'"'"'' 'Qdrant Harbor registry identity check'
require_text "$qdrant_publish_backend" '--password-stdin' 'Qdrant Harbor password stdin'
require_text "$payload_go" 'func BuildTargetPayload(' 'payload compatibility entrypoint'
require_text "$payload_go" 'func BuildTargetPayloadWithProgress(' 'payload progress entrypoint'
require_text "$payload_go" 'cacheRoot string' 'payload explicit cache root'
require_text "$payload_go" 'progress io.Writer' 'payload progress writer'
require_text "$transport" "$atomic_rename" 'atomic Target publish'
require_text "$transport" 'different Remote Release already exists' 'different same-version Release rejection'
require_text "$current_release_go" 'DefaultCurrentReceiptPath = "/var/lib/eva/releases/current.yaml"' 'Current Release receipt path'
require_text "$current_release_go" 'SelectionCurrent' 'Current Release selector'
require_text "$current_release_go" 'func RestoreCurrentReceipt(' 'Current Release receipt rollback helper'
require_text "$main_go" 'register-current-release' 'installer Current Release registration command'
require_text "$installer" 'internal register-current-release' 'installer uses Go receipt registration'
require_text "$installer" 'rollback_installation()' 'installer transaction rollback helper'
require_text "$installer" 'internal validate-current-release' 'installer final Current Release validation'
require_text "$installer" 'tool-installer.lock' 'installer transaction lock path'
require_text "$installer" 'flock -n 9' 'installer non-blocking exclusive lock'
require_text "$installer" 'flock -u 9' 'installer explicit lock release'
require_text "$installer" 'exec 9>&-' 'installer lock descriptor close'
require_text "$installer" 'release_installer_lock' 'installer lock release helper'
require_text "$installer" 'another EVA Tool installer transaction is running' 'installer lock contention error'
require_text "$installer" 'transaction_started=false' 'installer transaction start state'
require_text "$installer" 'transaction_committed=false' 'installer transaction commit state'
require_text "$installer" 'trap on_exit EXIT' 'installer EXIT rollback handler'
require_text "$installer" "trap 'on_signal INT 130' INT" 'installer INT handler'
require_text "$installer" "trap 'on_signal TERM 143' TERM" 'installer TERM handler'
require_text "$installer" "trap 'on_signal HUP 129' HUP" 'installer HUP handler'
require_text "$installer" 'test_hook tool-published' 'installer tool publish failure hook'
require_text "$installer" 'test_hook commit' 'installer commit failure hook'
if grep -Fq 'release_root: %s' "$installer"; then
  echo '[ERROR] installer must not construct Current Release YAML directly' >&2
  exit 1
fi
require_text "$main_go" 'release_source=%s' 'selected Release source output'
require_text "$remote_runbook" '# EVA Remote Repository 설치 가이드' 'integrated Remote Runbook title'
require_text "$remote_runbook" 'eva-base-release-<version>.zip' 'Remote Base Release input'
require_text "$remote_runbook" 'sudo eva remote bootstrap' 'Remote bootstrap command'
require_text "$remote_runbook" 'sudo eva remote prepare' 'Remote prepare command'
require_text "$remote_runbook" 'sudo eva remote verify' 'Remote verify command'
require_text "$remote_runbook" 'sudo eva remote publish' 'Remote publish command'
require_text "$remote_runbook" '--replace-registry' 'registry replacement contract'
require_text "$remote_runbook" '### 여러 Target에 순차 게시' 'multi-target publish contract'
require_text "$remote_runbook" '[Main]' 'Main work location'
require_text "$remote_runbook" '[Target]' 'Target work location'
# shellcheck disable=SC2016 # Literal Runbook command contract.
require_text "$remote_runbook" 'sudo bash "$release_root/eva-tool-installer.sh"' 'Target installer registers Current Release'
require_text "$remote_runbook" 'sudo eva verify' 'Target Current Release verify command'
require_text "$remote_runbook" 'sudo eva install --release /var/lib/eva/inbox/releases/<another-version>' 'Target explicit Release override'
require_text "$cloud_runbook" 'sudo eva verify' 'Cloud Current Release verify command'
require_text "$cloud_runbook" 'sudo eva install --workspace /home/eva/site-dev-196 --yes' 'Cloud Current Release install command'
require_text "$cloud_runbook" 'sudo eva verify --release /path/to/another/release' 'Cloud explicit Release override'
require_text "$root_readme" 'sudo eva verify' 'README Current Release verify command'
require_text "$root_readme" 'Current Release' 'README Current Release contract'
# shellcheck disable=SC2016 # Literal documentation contract.
require_text "$remote_runbook" 'Remote Base Release에는 `eva-offline`이 필수가 아닙니다.' 'Remote Base Release offline contract'
require_text "$installation_index" '[Remote Repository Runbook](remote-repository-runbook.md)' 'Remote Runbook index'

bash -n "$transport"
echo '[OK] Remote E2E static readiness contracts passed'
echo '[INFO] Live Main, Harbor, Release, and Target readiness were not checked'
