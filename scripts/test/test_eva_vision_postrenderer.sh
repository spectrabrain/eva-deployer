#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

python3 - "$REPO_ROOT" <<'PY'
from __future__ import annotations

from contextlib import redirect_stderr, redirect_stdout
from io import StringIO
from pathlib import Path
import sys

import yaml


repo_root = Path(sys.argv[1])
template_path = (
    repo_root
    / "src/solution/roles/eva_vision/templates/post-renderer/post-renderer.py.j2"
)
plugin_path = (
    repo_root
    / "src/solution/roles/eva_vision/templates/post-renderer/plugin.yaml.j2"
)
main_path = repo_root / "src/solution/roles/eva_vision/tasks/main.yaml"
reconcile_path = (
    repo_root / "src/solution/roles/eva_vision/tasks/reconcile_vision.yaml"
)

expected_command = [
    "/bin/bash",
    "-lc",
    "mv /app/scripts/warmup.py /app/scripts/warmup.py.disabled 2>/dev/null || true; exec uv run ./run.sh serve --log-profile prod --foreground",
]


def render_postrenderer(skip_startup_warmup: bool) -> str:
    source = template_path.read_text()
    source = source.replace(
        "{{ eva_vision_progress_deadline_seconds | int }}", "3600"
    )
    source = source.replace(
        "{{ eva_vision_skip_startup_warmup | bool | ternary('True', 'False') }}",
        str(skip_startup_warmup),
    )
    if "{{" in source or "}}" in source:
        raise SystemExit("[ERROR] post-renderer template rendering is incomplete")
    compile(source, str(template_path), "exec")
    return source


def deployment(command=None, deadline=None, container_name="eva-vision"):
    spec = {
        "template": {
            "spec": {
                "containers": [{"name": container_name, "image": "vision:test"}]
            }
        }
    }
    if command is not None:
        spec["template"]["spec"]["containers"][0]["command"] = command
    if deadline is not None:
        spec["progressDeadlineSeconds"] = deadline
    return {
        "apiVersion": "apps/v1",
        "kind": "Deployment",
        "metadata": {"name": "eva-vision"},
        "spec": spec,
    }


def manifest(*documents) -> str:
    return yaml.safe_dump_all(documents, explicit_start=True, sort_keys=False)


def run(source: str, input_manifest: str):
    namespace = {"__name__": "__main__"}
    original_stdin = sys.stdin
    stdout = StringIO()
    stderr = StringIO()
    try:
        sys.stdin = StringIO(input_manifest)
        with redirect_stdout(stdout), redirect_stderr(stderr):
            try:
                exec(source, namespace)
            except SystemExit as error:
                return error.code or 0, stdout.getvalue(), stderr.getvalue()
    finally:
        sys.stdin = original_stdin
    return 0, stdout.getvalue(), stderr.getvalue()


def assert_success(source: str, input_manifest: str):
    code, output, error = run(source, input_manifest)
    if code != 0:
        raise SystemExit(f"[ERROR] post-renderer unexpectedly failed: {error}")
    return list(yaml.safe_load_all(output))


def assert_failure(source: str, input_manifest: str, marker: str):
    code, _, error = run(source, input_manifest)
    if code == 0 or marker not in error:
        raise SystemExit(
            f"[ERROR] expected post-renderer failure marker={marker!r}: {error!r}"
        )


plugin = yaml.safe_load(plugin_path.read_text())
if plugin != {
    "apiVersion": "v1",
    "type": "postrenderer/v1",
    "name": "eva-vision-postrenderer",
    "version": "1.0.0",
    "runtime": "subprocess",
    "runtimeConfig": {
        "platformCommand": [
            {"command": "${HELM_PLUGIN_DIR}/post-renderer.py"}
        ]
    },
}:
    raise SystemExit("[ERROR] Helm 4 EVA Vision plugin contract mismatch")

enabled = render_postrenderer(True)
documents = [
    {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "vision"}},
    deployment(),
    {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "vision"}},
    {"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": "vision"}},
]
rendered = assert_success(enabled, manifest(*documents))
if rendered[0] != documents[0] or rendered[2] != documents[2] or rendered[3] != documents[3]:
    raise SystemExit("[ERROR] post-renderer changed a non-Deployment document")
target = rendered[1]
container = target["spec"]["template"]["spec"]["containers"][0]
if target["spec"].get("progressDeadlineSeconds") != 3600:
    raise SystemExit("[ERROR] post-renderer did not set progressDeadlineSeconds")
if container.get("command") != expected_command:
    raise SystemExit("[ERROR] post-renderer did not set the startup command")

rendered = assert_success(enabled, manifest(deployment(expected_command, 3600)))
if rendered[0]["spec"]["template"]["spec"]["containers"][0]["command"] != expected_command:
    raise SystemExit("[ERROR] expected startup command is not idempotent")

disabled = render_postrenderer(False)
original_command = ["/bin/sh", "-c", "exec custom-vision"]
rendered = assert_success(disabled, manifest(deployment(original_command)))
if rendered[0]["spec"]["template"]["spec"]["containers"][0]["command"] != original_command:
    raise SystemExit("[ERROR] warmup-disabled mode changed the existing command")

assert_failure(enabled, manifest(deployment(["unexpected"])), "unexpected existing")
assert_failure(enabled, manifest(deployment(None, 300)), "unexpected progressDeadlineSeconds")
assert_failure(enabled, manifest(), "manifest input is empty")
assert_failure(enabled, "not: [valid", "manifest parsing failed")
assert_failure(enabled, manifest({"apiVersion": "v1", "kind": "Service"}), "Deployment/eva-vision")
assert_failure(enabled, manifest(deployment(container_name="other")), "eva-vision container")

main_tasks = yaml.safe_load(main_path.read_text())
task_by_name = {
    task.get("name"): task
    for task in main_tasks
    if isinstance(task, dict) and task.get("name")
}
plugin_tasks = (
    "Ensure EVA Vision Helm post-renderer plugin directory exists",
    "Render EVA Vision Helm post-renderer plugin metadata",
    "Render EVA Vision Helm post-renderer",
    "Ensure EVA Vision Helm post-renderer is executable",
    "Verify Helm recognizes the EVA Vision post-renderer plugin",
    "Assert Helm recognizes the EVA Vision post-renderer plugin",
    "Render and verify final EVA Vision Helm manifest before deployment",
)
for name in plugin_tasks:
    if name not in task_by_name:
        raise SystemExit(f"[ERROR] post-renderer setup task is absent: {name}")

renderer_task = task_by_name["Render EVA Vision Helm post-renderer"]
if renderer_task.get("ansible.builtin.template") != {
    "src": "post-renderer/post-renderer.py.j2",
    "dest": "{{ eva_vision_workdir }}/plugin/eva-vision-postrenderer/post-renderer.py",
    "mode": "0755",
}:
    raise SystemExit("[ERROR] post-renderer template task does not preserve executable mode")

task_names = list(task_by_name)
if not (
    task_names.index(plugin_tasks[-1])
    < task_names.index("Reconcile exact legacy Argo CD-managed EVA Vision resources")
    < task_names.index("Deploy EVA Vision from online Helm repo")
    < task_names.index("Deploy EVA Vision from offline chart")
):
    raise SystemExit("[ERROR] post-renderer setup order is invalid")

for name in (
    "Deploy EVA Vision from online Helm repo",
    "Deploy EVA Vision from offline chart",
):
    task = next(task for task in main_tasks if task.get("name") == name)
    command = task.get("ansible.builtin.command", "")
    environment = task.get("environment", {})
    if "--post-renderer eva-vision-postrenderer" not in command:
        raise SystemExit(f"[ERROR] post-renderer is absent from {name}")
    if environment.get("HELM_PLUGINS") != "{{ eva_vision_workdir }}/plugin":
        raise SystemExit(f"[ERROR] HELM_PLUGINS is absent from {name}")

names = [task.get("name") for task in main_tasks if isinstance(task, dict)]
for forbidden in (
    "Extend EVA Vision deployment progress deadline",
    "Skip EVA Vision startup warmup during deployment",
):
    if forbidden in names:
        raise SystemExit(f"[ERROR] obsolete post-Helm task remains: {forbidden}")

reconcile = reconcile_path.read_text()
if "--post-renderer eva-vision-postrenderer" not in reconcile:
    raise SystemExit("[ERROR] reconciliation Helm template omits the post-renderer")
if 'HELM_PLUGINS: "{{ eva_vision_workdir }}/plugin"' not in reconcile:
    raise SystemExit("[ERROR] reconciliation omits HELM_PLUGINS")

for workflow_name in ("pr-ci.yaml", "tag-release.yaml"):
    workflow = (repo_root / ".github/workflows" / workflow_name).read_text()
    if "scripts/test/test_eva_vision_postrenderer.sh" not in workflow:
        raise SystemExit(f"[ERROR] post-renderer test is absent from {workflow_name}")

print("EVA Vision Helm post-renderer contract tests passed.")
PY
