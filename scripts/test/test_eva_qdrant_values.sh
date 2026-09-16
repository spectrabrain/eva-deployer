#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ROLE="$REPO_ROOT/src/solution/roles/eva_agent/tasks/dependencies.yaml"
BASE='https://raw.githubusercontent.com/mellerikat/eva-agent/chartmuseum/release/3.1.0'

grep -Fq "eva_agent_qdrant_values_file: \"{{ eva_agent_qdrant_values_file | default('values-k3s.s3.yaml') }}\"" "$ROLE"
grep -Fq "values-k3s.s3.yaml' and eva_agent_qdrant_snapshot_source == 'local_pv'" "$ROLE"
grep -Fq "values-k3s.harbor.yaml' and eva_agent_qdrant_snapshot_source == 'harbor'" "$ROLE"
grep -Fq 'eva_agent_qdrant_post_renderer_arg' "$ROLE"
grep -Fq '{{ eva_agent_qdrant_post_renderer_arg }}' "$ROLE"

cloud_values="$(curl -fsSL "$BASE/eva-agent-qdrant/values-k3s.s3.yaml")"
plugin="$(curl -fsSL "$BASE/plugins/eva-agent-qdrant/plugin.yaml")"
printf '%s\n' "$cloud_values" | grep -Fq 'S3_BUCKET'
printf '%s\n' "$cloud_values" | grep -Fq 'qdrant-snapshot-sync'
printf '%s\n' "$plugin" | grep -Fq 'eva-agent-qdrant-postrenderer'

echo 'EVA Qdrant Cloud values contract tests passed.'
