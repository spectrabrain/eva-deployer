#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
main="$repo_root/src/solution/roles/eva_app/tasks/main.yaml"
reconcile="$repo_root/src/solution/roles/eva_app/tasks/reconcile_app.yaml"
verify="$repo_root/src/solution/roles/eva_app/tasks/verify_app_ownership.yaml"

python3 - "$main" "$reconcile" "$verify" <<'PY'
import copy
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import textwrap
import yaml

documents = {}
for raw_path in sys.argv[1:]:
    path = Path(raw_path)
    tasks = yaml.safe_load(path.read_text())
    if not isinstance(tasks, list):
        raise SystemExit(f"[ERROR] task file is not a YAML list: {path}")
    documents[path] = tasks

reconcile_path = Path(sys.argv[2])
reconcile_shell = documents[reconcile_path][0].get("ansible.builtin.shell")
match = re.search(
    r"python3 - \"\$handoff_file\" \"\$EVA_APP_SITE_ID\" <<'PY'\n(.*?)\nPY",
    reconcile_shell or "",
    re.DOTALL,
)
if match is None:
    raise SystemExit("[ERROR] EVA App handoff receipt validator is absent")

validator = textwrap.dedent(match.group(1))
valid_receipt = {
    "schema_version": "v1",
    "site_id": "site-a",
    "cluster_name": "cluster-a",
    "completed_at": "2026-09-18T00:00:00Z",
    "applications": ["cluster-a-eva-app", "cluster-a-eva-agent"],
    "registration_application": "registration",
    "registration_repository": "https://example.invalid/eva.git",
    "registration_branch": "main",
    "registration_manifest": "registration/clusters/cluster-a.yaml",
    "registration_commit": "a" * 40,
}

def validates(receipt):
    with tempfile.TemporaryDirectory() as directory:
        receipt_path = Path(directory) / "argocd-handoff.yaml"
        receipt_path.write_text(yaml.safe_dump(receipt))
        result = subprocess.run(
            [sys.executable, "-", str(receipt_path), "site-a"],
            input=validator,
            text=True,
            capture_output=True,
            check=False,
        )
    return result

if validates(valid_receipt).stdout.strip() != "cluster-a-eva-app":
    raise SystemExit("[ERROR] valid Git-backed EVA App receipt was rejected")

invalid_receipts = {
    "registration application": {"registration_application": "other"},
    "registration repository": {"registration_repository": ""},
    "registration branch": {"registration_branch": ""},
    "registration manifest": {"registration_manifest": "registration/clusters/other.yaml"},
    "registration commit": {"registration_commit": "A" * 40},
    "cluster name": {"cluster_name": ""},
    "EVA App application": {"applications": ["other-eva-app"]},
}
for name, changes in invalid_receipts.items():
    receipt = copy.deepcopy(valid_receipt)
    receipt.update(changes)
    if validates(receipt).returncode == 0:
        raise SystemExit(f"[ERROR] invalid {name} receipt was accepted")
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
  'Git-backed EVA App identity contract' \
  'registration_application' \
  'registration_repository' \
  'registration_branch' \
  'registration_manifest' \
  'registration_commit' \
  'cluster_name' \
  "cluster_name + '-eva-app'" \
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
