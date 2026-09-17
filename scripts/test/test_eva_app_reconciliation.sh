#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
main="$repo_root/src/solution/roles/eva_app/tasks/main.yaml"
reconcile="$repo_root/src/solution/roles/eva_app/tasks/reconcile_app.yaml"
verify="$repo_root/src/solution/roles/eva_app/tasks/verify_app_ownership.yaml"

python3 - "$main" "$reconcile" "$verify" <<'PY'
from pathlib import Path
import sys
import yaml

for raw_path in sys.argv[1:]:
    path = Path(raw_path)
    tasks = yaml.safe_load(path.read_text())
    if not isinstance(tasks, list):
        raise SystemExit(f"[ERROR] task file is not a YAML list: {path}")
PY

for required in \
  'Reconcile verified Argo CD legacy EVA App resources' \
  'Validate EVA App Helm installation with server dry-run' \
  'Record existing EVA App deployment identity and replicas' \
  'Install EVA App and restore prior replicas if Helm fails' \
  'Verify successful EVA App legacy Helm adoption'; do
  grep -Fq "$required" "$main"
done

dry_run_line="$(grep -n -m1 'Validate EVA App Helm installation with server dry-run' "$main" | cut -d: -f1)"
scale_line="$(grep -n -m1 'Scale down existing EVA App deployment before reinstall' "$main" | cut -d: -f1)"
[[ "$dry_run_line" -lt "$scale_line" ]]

for required in \
  --dry-run=server \
  --hide-secret \
  '{{ eva_app_take_ownership_arg }}' \
  'take_ownership=true' \
  'take_ownership=false' \
  'exactly one EVA App Application identity' \
  'EVA_APP_SITE_ID' \
  'eva_cli_helm_set' \
  'unsafe EVA App Helm release status' \
  'foreign or partial Helm ownership' \
  'unsupported managed-by ownership' \
  'unexpected controller ownership' \
  'EVA App tracking identity mismatch' \
  'legacy EVA App resource is absent from rendered chart' \
  'persistent resource cannot be adopted' \
  'adopted EVA App resources are owned by the deployed Helm release' \
  'meta.helm.sh/release-name' \
  'meta.helm.sh/release-namespace'; do
  grep -Fq -- "$required" "$main" "$reconcile" "$verify"
done

if grep -Eq 'kubectl (delete|patch)|--force' "$reconcile" "$verify"; then
  echo 'EVA App reconciliation performs a forbidden resource mutation' >&2
  exit 1
fi

for workflow in pr-ci.yaml tag-release.yaml; do
  grep -Fq 'scripts/test/test_eva_app_reconciliation.sh' "$repo_root/.github/workflows/$workflow"
done

echo 'EVA App reconciliation contract tests passed.'
