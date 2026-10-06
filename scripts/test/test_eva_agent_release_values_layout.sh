#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ANSIBLE_PLAYBOOK="${ANSIBLE_PLAYBOOK:-ansible-playbook}"
TMP_ROOT="$(mktemp -d)"

cleanup() {
  rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

fail() {
  echo "[FAIL] $*" >&2
  exit 1
}

# shellcheck source=scripts/lib/load_versions.sh
source "$REPO_ROOT/scripts/lib/load_versions.sh"

for case in 3.1.0:legacy 3.0.4:legacy 3.2.0:source-split 3.2.1:source-split v3.2.0:source-split 4.0.0:source-split; do
  version="${case%%:*}"
  expected="${case#*:}"
  [[ "$(eva_agent_values_layout "$version")" == "$expected" ]] || fail "layout for $version is not $expected"
done
for invalid in '' 3.2 latest 3.2.0/../x; do
  if eva_agent_values_layout "$invalid" >/dev/null 2>&1; then
    fail "invalid Agent version accepted: '$invalid'"
  fi
done
for invalid in 'legacy eva-agent upstream' 'legacy eva-agent-vllm source H100x8' 'unknown eva-agent source' 'legacy eva-agent-init source'; do
  # shellcheck disable=SC2086
  if eva_agent_values_file $invalid >/dev/null 2>&1; then
    fail "invalid values file request accepted: $invalid"
  fi
done

expected_legacy='eva-agent/values-k3s.yaml
eva-agent-vllm/values-k3s.A6000x1.yaml
eva-agent-vllm/values-k3s.L40sx1.yaml
eva-agent-vllm/values-k3s.PRO5000x3.yaml
eva-agent-vllm/values-k3s.PRO6000-MIGx4.yaml'
expected_split='eva-agent/values-k3s.ecr.yaml
eva-agent/values-k3s.harbor.yaml
eva-agent-vllm/values-k3s.A6000x1.docker.yaml
eva-agent-vllm/values-k3s.A6000x1.harbor.yaml
eva-agent-vllm/values-k3s.L40sx1.docker.yaml
eva-agent-vllm/values-k3s.L40sx1.harbor.yaml
eva-agent-vllm/values-k3s.PRO5000x3.docker.yaml
eva-agent-vllm/values-k3s.PRO5000x3.harbor.yaml
eva-agent-vllm/values-k3s.PRO6000-MIGx4.docker.yaml
eva-agent-vllm/values-k3s.PRO6000-MIGx4.harbor.yaml'
[[ "$(eva_agent_release_values_paths legacy)" == "$expected_legacy" ]] || fail "legacy values paths differ"
[[ "$(eva_agent_release_values_paths source-split)" == "$expected_split" ]] || fail "source-split values paths differ"

# prepare-offline-assets must check exactly the files the selected Agent
# release publishes, and stop before any download when one is missing. curl
# and sudo are stubbed so the check runs without network or privileges.
stub_bin="$TMP_ROOT/bin"
mkdir -p "$stub_bin"
cat >"$stub_bin/curl" <<'STUB'
#!/usr/bin/env bash
url="${*: -1}"
if [[ " $* " != *" --write-out "* ]]; then
  echo "unexpected download before required asset validation: $url" >>"$STUB_LOG.downloads"
  exit 22
fi
echo "$url" >>"$STUB_LOG"
if [[ -n "${STUB_MISSING:-}" && "$url" == *"$STUB_MISSING" ]]; then
  printf '404'
else
  printf '200'
fi
STUB
cat >"$stub_bin/sudo" <<'STUB'
#!/usr/bin/env bash
[[ "$1" == -n ]] && shift
exec "$@"
STUB
chmod +x "$stub_bin/curl" "$stub_bin/sudo"

run_required_check() {
  local release="$1" missing="$2" log="$TMP_ROOT/required-$1.log"
  : >"$log"
  if PATH="$stub_bin:$PATH" STUB_LOG="$log" STUB_MISSING="$missing" \
    EVA_AGENT_RELEASE="$release" EVA_AGENT_QDRANT_SNAPSHOT_SOURCE=harbor \
    EVA_CACHE_ROOT="$TMP_ROOT/cache-$release" VERSIONS_QUIET=1 \
    bash "$REPO_ROOT/scripts/download/download_offline_assets.sh" >"$log.out" 2>&1; then
    fail "prepare-offline-assets accepted a missing required asset for $release"
  fi
  grep -Fq 'no preparation downloads were started.' "$log.out" || fail "prepare-offline-assets did not stop at validation for $release"
  [[ ! -e "$log.downloads" ]] || fail "prepare-offline-assets downloaded before validation for $release"
  sed "s#^https://raw.githubusercontent.com/mellerikat/eva-agent/chartmuseum/release/$release/##" "$log"
}

required_values() {
  grep -E '^eva-agent(-vllm)?/values-k3s' || true
}

split_required="$(run_required_check 3.2.0 install_eva_agent_dependencies.sh)"
[[ "$(required_values <<<"$split_required")" == "$expected_split" ]] || fail "3.2.0 required Agent values differ"
legacy_required="$(run_required_check 3.1.0 install_eva_agent_dependencies.sh)"
[[ "$(required_values <<<"$legacy_required")" == "$expected_legacy" ]] || fail "3.1.0 required Agent values differ"
for expected in eva-agent/values-secret.yaml eva-agent-init/values-k3s.yaml eva-agent-qdrant/values-k3s.harbor.yaml plugins/eva-agent-qdrant/post-renderer.sh plugins/eva-agent-qdrant/plugin.yaml; do
  grep -Fxq "$expected" <<<"$split_required" || fail "3.2.0 required assets omit $expected"
done
run_required_check 3.2.0 eva-agent-vllm/values-k3s.L40sx1.harbor.yaml >/dev/null
grep -Fq '[missing] status=404' "$TMP_ROOT/required-3.2.0.log.out" || fail "missing source-split asset was not reported"

# The Agent role must choose the same files: Harbor variants for Repository
# targets, upstream-source variants for Cloud targets.
if ! command -v "$ANSIBLE_PLAYBOOK" >/dev/null 2>&1 && [[ ! -x "$ANSIBLE_PLAYBOOK" ]]; then
  echo "ansible-playbook was not found: $ANSIBLE_PLAYBOOK" >&2
  exit 1
fi

values_dir="$TMP_ROOT/values"
mkdir -p "$values_dir"
cat >"$values_dir/agent.harbor.yaml" <<'EOF'
image:
  repository: "{{ harbor_url }}/eva/eva-agent"
persistence:
  s3Sync:
    image: "{{harbor_url}}/eva/aws-cli:2.33.8"
sharedPvcStorage:
  nfs:
    path: "{{ eva_agent_nfs_share_path }}/agent-cache"
EOF
cat >"$values_dir/qdrant.harbor.yaml" <<'EOF'
# Harbor k3s profile. Replace {{ harbor_url }} with the reachable Harbor
sidecarContainers:
  - name: qdrant-snapshot-sync
    image: "{{ harbor_url }}/eva/eva-agent-qdrant-snapshot-sync:0.1.0"
    env:
      - name: HARBOR_REGISTRY
        value: "{{ harbor_url }}"
EOF
cat >"$values_dir/unresolved.yaml" <<'EOF'
image:
  repository: "{{ harbor_registry }}/eva/eva-agent"
EOF

cat >"$TMP_ROOT/layout.yaml" <<EOF
---
- hosts: localhost
  gather_facts: false
  connection: local
  vars:
    eva_cache_root: $TMP_ROOT/cache
    eva_agent_vllm_profile: L40sx1
  tasks:
    - name: Load Agent values layout
      ansible.builtin.include_tasks: $REPO_ROOT/src/solution/roles/eva_agent/tasks/dependencies.yaml
      tags: always

    - name: Assert Agent values files
      ansible.builtin.assert:
        that:
          - eva_agent_values_file == expected_agent
          - eva_agent_vllm_values_file == expected_vllm
      tags: always
EOF

# set_fact results persist for the host, so each case runs in its own play.
for case in \
  '3.1.0 true values-k3s.yaml values-k3s.L40sx1.yaml' \
  '3.1.0 false values-k3s.yaml values-k3s.L40sx1.yaml' \
  '3.2.0 true values-k3s.harbor.yaml values-k3s.L40sx1.harbor.yaml' \
  '3.2.0 false values-k3s.ecr.yaml values-k3s.L40sx1.docker.yaml' \
  '3.2.1 true values-k3s.harbor.yaml values-k3s.L40sx1.harbor.yaml'; do
  read -r version airgap agent vllm <<<"$case"
  "$ANSIBLE_PLAYBOOK" --tags eva_agent_values_layout \
    -e "{\"eva_agent_deploy_version\": \"$version\", \"airgap_mode\": $airgap, \"expected_agent\": \"$agent\", \"expected_vllm\": \"$vllm\"}" \
    "$TMP_ROOT/layout.yaml" >"$TMP_ROOT/ansible.log" 2>&1 || {
    cat "$TMP_ROOT/ansible.log" >&2
    fail "Agent role values layout differs for $case"
  }
done

cat >"$TMP_ROOT/placeholders.yaml" <<EOF
---
- hosts: localhost
  gather_facts: false
  connection: local
  vars:
    repository_registry: 10.159.57.172:32080
    repository_project: eva-site
    eva_agent_nfs_share_path: /data001/share/eva-agent
  tasks:
    - name: Resolve Harbor placeholders
      ansible.builtin.include_tasks: $REPO_ROOT/src/solution/roles/eva_agent/tasks/resolve_release_values_placeholders.yaml
      vars:
        eva_agent_release_values_path: "$values_dir/{{ item }}"
      loop: [agent.harbor.yaml, qdrant.harbor.yaml]

    - name: Assert resolved Harbor values
      ansible.builtin.assert:
        that:
          - lookup('ansible.builtin.file', '$values_dir/agent.harbor.yaml') is search('repository. "10.159.57.172:32080/eva-site/eva-agent"')
          - lookup('ansible.builtin.file', '$values_dir/agent.harbor.yaml') is search('image. "10.159.57.172:32080/eva-site/aws-cli:2.33.8"')
          - lookup('ansible.builtin.file', '$values_dir/qdrant.harbor.yaml') is search('image. "10.159.57.172:32080/eva-site/eva-agent-qdrant-snapshot-sync:0.1.0"')
          - lookup('ansible.builtin.file', '$values_dir/agent.harbor.yaml') is search('path. "/data001/share/eva-agent/agent-cache"')
          - lookup('ansible.builtin.file', '$values_dir/qdrant.harbor.yaml') is search('value. "10.159.57.172:32080"')

    - name: Reject a Harbor placeholder that cannot be resolved
      block:
        - name: Resolve an unsupported placeholder form
          ansible.builtin.include_tasks: $REPO_ROOT/src/solution/roles/eva_agent/tasks/resolve_release_values_placeholders.yaml
          vars:
            eva_agent_release_values_path: "$values_dir/unresolved.yaml"
        - name: Record an accepted unresolved placeholder
          ansible.builtin.set_fact:
            eva_agent_unresolved_placeholder_accepted: true
      rescue:
        - name: Require the placeholder failure
          ansible.builtin.assert:
            that:
              - ansible_failed_result.msg is search('Unresolved placeholder')

    - name: Require the unresolved placeholder to fail closed
      ansible.builtin.assert:
        that:
          - not (eva_agent_unresolved_placeholder_accepted | default(false))
EOF

"$ANSIBLE_PLAYBOOK" "$TMP_ROOT/placeholders.yaml" >"$TMP_ROOT/ansible.log" 2>&1 || {
  cat "$TMP_ROOT/ansible.log" >&2
  fail "Harbor placeholder resolution contract failed"
}

echo 'EVA Agent release values layout contract tests passed.'
