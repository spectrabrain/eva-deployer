#!/usr/bin/env bash
set -euo pipefail

repo_root="$(
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." &&
  pwd
)"

task_file="$repo_root/src/solution/roles/eva_agent/tasks/agent.yaml"
python_bin="${PYTHON_BIN:-}"

if [[ -z "$python_bin" ]]; then
  if [[ -x "$repo_root/.venv/bin/python" ]]; then
    python_bin="$repo_root/.venv/bin/python"
  elif command -v python3 >/dev/null 2>&1; then
    python_bin="$(command -v python3)"
  else
    echo '[ERROR] Python interpreter is unavailable'
    exit 1
  fi
fi

if [[ ! -x "$python_bin" ]]; then
  echo "[ERROR] Python interpreter is not executable: $python_bin"
  exit 1
fi

"$python_bin" - "$task_file" <<'PYTEST'
from pathlib import Path
from copy import deepcopy
from contextlib import redirect_stderr, redirect_stdout
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from io import StringIO
from types import SimpleNamespace
from unittest.mock import patch
import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import yaml


sys.dont_write_bytecode = True
path = Path(sys.argv[1])
tasks = yaml.safe_load(path.read_text())

if not isinstance(tasks, list):
    raise SystemExit(
        "[ERROR] EVA Agent tasks are not a list"
    )

task_name = (
    "Wait until EVA Agent deployment pods "
    "are terminated before reinstall"
)

matches = [
    task
    for task in tasks
    if task.get("name") == task_name
]

if len(matches) != 1:
    raise SystemExit(
        "[ERROR] EVA Agent deployment wait task count mismatch: "
        f"actual={len(matches)}"
    )

task = matches[0]
command = task.get("ansible.builtin.command")

if not isinstance(command, str):
    raise SystemExit(
        "[ERROR] EVA Agent deployment wait command is absent"
    )

normalized = " ".join(command.split())

required = (
    "kubectl wait --for=delete pod",
    "-n {{ eva_agent_namespace }}",
    "-l app.kubernetes.io/name=eva-agent,pod-template-hash",
    "--timeout=180s",
)

for marker in required:
    if marker not in normalized:
        raise SystemExit(
            "[ERROR] EVA Agent deployment wait marker is absent: "
            + marker
        )

broad_selector = (
    "-l app.kubernetes.io/name=eva-agent "
    "--timeout=180s"
)

if broad_selector in normalized:
    raise SystemExit(
        "[ERROR] EVA Agent wait selector includes Job pods"
    )

conditions = task.get("when")

if not isinstance(conditions, list):
    raise SystemExit(
        "[ERROR] EVA Agent deployment wait conditions are absent"
    )

expected_conditions = (
    "not ansible_check_mode",
    "eva_agent_deployment_exists.rc == 0",
)

for condition in expected_conditions:
    if condition not in conditions:
        raise SystemExit(
            "[ERROR] EVA Agent wait condition is absent: "
            + condition
        )

if task.get("changed_when") is not False:
    raise SystemExit(
        "[ERROR] EVA Agent deployment wait must be read-only"
    )

failed_when = task.get("failed_when")

if not isinstance(failed_when, list):
    raise SystemExit(
        "[ERROR] EVA Agent deployment wait failure policy is absent"
    )

if "eva_agent_wait_delete.rc != 0" not in failed_when:
    raise SystemExit(
        "[ERROR] EVA Agent deployment wait rc guard is absent"
    )

print("[OK] EVA Agent wait selects Deployment pods only")
print("[OK] completed Job pods are excluded")
print("[OK] deployment wait remains bounded to 180 seconds")
print("[OK] deployment wait remains read-only")

cleanup_name = "Clean up completed EVA Agent deployment pods before reinstall"
cleanup_tasks = [item for item in tasks if item.get("name") == cleanup_name]
assert len(cleanup_tasks) == 1
cleanup_task = cleanup_tasks[0]
assert all(condition in cleanup_task["when"] for condition in expected_conditions)
scale_index = next(index for index, item in enumerate(tasks)
                   if item.get("name") == "Scale down existing EVA Agent deployment before reinstall")
assert scale_index < tasks.index(cleanup_task) < tasks.index(task)
copy_task, run_task = cleanup_task["block"]
assert copy_task["ansible.builtin.copy"] == {
    "src": "cleanup_completed_pods.py",
    "dest": "{{ eva_agent_workdir }}/cleanup_completed_pods.py", "mode": "0700",
}
assert run_task["ansible.builtin.command"]["argv"] == [
    "python3", "{{ eva_agent_workdir }}/cleanup_completed_pods.py",
    "--namespace", "{{ eva_agent_namespace }}",
]
assert run_task["environment"]["KUBECONFIG"] == "{{ eva_agent_kubeconfig }}"
assert run_task["changed_when"] == (
    "eva_agent_completed_cleanup.rc == 0 and (eva_agent_completed_cleanup.stdout | from_json).changed")
assert "failed_when" not in run_task and "ignore_errors" not in run_task

helper_path = path.parent.parent / "files/cleanup_completed_pods.py"
spec = importlib.util.spec_from_file_location("completed_agent_pods", helper_path)
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)

NAMESPACE = "custom-agent"


def resource(kind, name, uid):
    return {"apiVersion": "v1" if kind == "Pod" else "apps/v1", "kind": kind,
            "metadata": {"name": name, "namespace": NAMESPACE, "uid": uid, "resourceVersion": "7"}}


def owner(kind, name, uid):
    return {"apiVersion": "apps/v1", "kind": kind, "name": name, "uid": uid, "controller": True}


class Cluster:
    def __init__(self):
        self.deployment = resource("Deployment", "eva-agent", "deployment-uid")
        self.deployment["spec"] = {"replicas": 0}
        self.replica_set = resource("ReplicaSet", "eva-agent-old", "rs-uid")
        self.replica_set["spec"] = {"replicas": 1}  # Scale-down reconciliation can lag.
        self.replica_set["metadata"]["ownerReferences"] = [owner("Deployment", "eva-agent", "deployment-uid")]
        self.pod = resource("Pod", "eva-agent-old-completed", "pod-uid")
        self.pod["status"] = {"phase": "Succeeded"}
        self.pod["metadata"].update(
            labels={"app.kubernetes.io/name": "eva-agent", "pod-template-hash": "old"},
            ownerReferences=[owner("ReplicaSet", "eva-agent-old", "rs-uid")],
        )
        self.listed = None
        self.current = None
        self.later_deployment = None
        self.reads = 0
        self.deletes = []
        self.error = None
        self.delete_error = False
        self.calls = []

    def run(self, command, **kwargs):
        self.calls.append(command)
        assert command[:2] == ["kubectl", "--request-timeout=30s"]
        assert kwargs["timeout"] == 40 and kwargs["text"] and kwargs["capture_output"]
        if self.error == "timeout":
            raise subprocess.TimeoutExpired(command, 40)
        if self.error == "os":
            raise OSError("not installed")
        if self.error:
            return SimpleNamespace(returncode=1 if self.error == "rc" else 0,
                                   stdout="sensitive-invalid-response")
        arguments = command[2:]
        if arguments[:1] == ["delete"]:
            assert arguments == ["delete", "--raw",
                                 f"/api/v1/namespaces/{NAMESPACE}/pods/eva-agent-old-completed", "-f", "-"]
            options = json.loads(kwargs["input"])
            assert options == {"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {
                "uid": "pod-uid", "resourceVersion": "7",
            }}  # No forced grace, finalizer patch, PVC or owner mutation.
            self.deletes.append(options)
            return SimpleNamespace(returncode=int(self.delete_error), stdout="{}")
        assert arguments[:3] == ["-n", NAMESPACE, "get"]
        if arguments[3] == "pods":
            assert arguments[4:] == ["-l", helper.SELECTOR, "-o", "json"]
            value = {"items": deepcopy(self.listed if self.listed is not None else [self.pod])}
        else:
            assert arguments[-3:] == ["--ignore-not-found", "-o", "json"]
            if arguments[3:5] == ["deployment", "eva-agent"]:
                self.reads += 1
                value = (self.later_deployment if self.reads > 1 and self.later_deployment is not None
                         else self.deployment)
            elif arguments[3:5] == ["replicaset", "eva-agent-old"]:
                value = self.replica_set
            elif arguments[3:5] == ["pod", "eva-agent-old-completed"]:
                value = self.current if self.current is not None else self.pod
            else:
                raise AssertionError(f"Unexpected resource access: {arguments}")
        return SimpleNamespace(returncode=0, stdout=json.dumps(value) if value else "")

    def cleanup(self):
        with patch.object(helper.subprocess, "run", side_effect=self.run):
            return helper.cleanup(NAMESPACE)


def unchanged(cluster):
    assert cluster.cleanup() == {"changed": False, "deleted_pods": []}
    assert not cluster.deletes


def rejected(cluster):
    try:
        cluster.cleanup()
    except helper.CleanupError:
        pass
    else:
        raise AssertionError("Unsafe or failed cleanup was not rejected")


cluster = Cluster()
assert cluster.cleanup() == {"changed": True, "deleted_pods": [cluster.pod["metadata"]["name"]]}
assert len(cluster.deletes) == 1 and cluster.reads == 2
cluster.deletes.clear()
cluster.listed = []
unchanged(cluster)

for phase in ("Running", "Pending", "Failed", "Unknown", None):
    cluster = Cluster()
    cluster.pod["status"]["phase"] = phase
    unchanged(cluster)

for key, value in (("deletionTimestamp", "2026-11-17T00:00:00Z"), ("finalizers", ["example.com/keep"]),
                   ("namespace", "foreign"), ("labels", {}), ("ownerReferences", []),
                   ("ownerReferences", [owner("Job", "eva-agent-old", "job-uid")]),
                   ("ownerReferences", [owner("ReplicaSet", "eva-agent-old", "rs-uid"),
                                        owner("ReplicaSet", "other", "other-uid")])):
    cluster = Cluster()
    cluster.pod["metadata"][key] = value
    unchanged(cluster)

for value in (False, None):
    cluster = Cluster()
    cluster.pod["metadata"]["ownerReferences"][0]["controller"] = value
    unchanged(cluster)

for key, value in (("uid", "foreign-rs"), ("ownerReferences", []),
                   ("ownerReferences", [owner("Deployment", "other", "deployment-uid")]),
                   ("ownerReferences", [owner("Deployment", "eva-agent", "replaced-deployment-uid")])):
    cluster = Cluster()
    cluster.replica_set["metadata"][key] = value
    unchanged(cluster)

for target in ("pod", "replica_set", "deployment"):
    for key in ("uid", "resourceVersion"):
        cluster = Cluster()
        cluster.__dict__[target]["metadata"].pop(key)
        rejected(cluster)
        assert not cluster.deletes

for replicas in (1, None, False, "0"):
    cluster = Cluster()
    cluster.deployment["spec"]["replicas"] = replicas
    rejected(cluster)
    assert not cluster.deletes

for change in ("replacement", "rescale", "deleting"):
    cluster = Cluster()
    cluster.later_deployment = deepcopy(cluster.deployment)
    if change == "replacement":
        cluster.later_deployment["metadata"]["uid"] = "new-deployment"
    elif change == "rescale":
        cluster.later_deployment["spec"]["replicas"] = 1
    else:
        cluster.later_deployment["metadata"]["deletionTimestamp"] = "now"
    rejected(cluster)
    assert not cluster.deletes

for change in ("phase", "finalizers", "owner", "replacement", "gone"):
    cluster = Cluster()
    cluster.current = deepcopy(cluster.pod)
    if change == "phase":
        cluster.current["status"]["phase"] = "Running"
    elif change == "finalizers":
        cluster.current["metadata"]["finalizers"] = ["example.com/keep"]
    elif change == "owner":
        cluster.current["metadata"]["ownerReferences"] = []
    elif change == "replacement":
        cluster.current["metadata"]["uid"] = "new-pod"
    else:
        cluster.current = {}
    if change == "replacement":
        rejected(cluster)
    else:
        unchanged(cluster)
    assert not cluster.deletes

cluster = Cluster()
cluster.replica_set = {}
unchanged(cluster)

for error in ("timeout", "os", "rc", "json"):
    cluster = Cluster()
    cluster.error = error
    rejected(cluster)
    assert not cluster.deletes

cluster = Cluster()
cluster.delete_error = True
rejected(cluster)
assert len(cluster.deletes) == 1  # A conflict is never retried or forced.

cluster = Cluster()
stdout, stderr = StringIO(), StringIO()
with patch.object(helper.subprocess, "run", side_effect=cluster.run), \
        patch.object(sys, "argv", [str(helper_path), "--namespace", NAMESPACE]), \
        redirect_stdout(stdout), redirect_stderr(stderr):
    assert helper.main() == 0
assert json.loads(stdout.getvalue())["changed"] is True and not stderr.getvalue()

cluster = Cluster()
cluster.error = "rc"
stdout, stderr = StringIO(), StringIO()
with patch.object(helper.subprocess, "run", side_effect=cluster.run), \
        patch.object(sys, "argv", [str(helper_path), "--namespace", NAMESPACE]), \
        redirect_stdout(stdout), redirect_stderr(stderr):
    assert helper.main() == 1
assert not stdout.getvalue() and "sensitive" not in stderr.getvalue()

print("[OK] only Succeeded Pods with verified Deployment/ReplicaSet/Pod UIDs are deleted")
print("[OK] active, deleting, finalized, Job and foreign-owned Pods are preserved")
print("[OK] scale-down races, replacements and deletion conflicts fail safely")
print("[OK] deletion uses UID/resourceVersion preconditions without force or storage changes")

# Exercise real kubectl's raw DELETE and Ansible's copy/command/changed_when
# against a loopback-only fake API. Never read or contact the user's cluster.
ansible = shutil.which("ansible-playbook") or str(path.parents[5] / ".venv/bin/ansible-playbook")
if shutil.which("kubectl") and Path(ansible).is_file():
    cluster = Cluster()
    requests = []
    state = {"present": True, "conflict": False}

    class API(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def send_json(self, value, code=200):
            raw = json.dumps(value).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def do_GET(self):
            url = self.path.split("?", 1)[0]
            requests.append(("GET", url))
            if url == "/api":
                value = {"kind": "APIVersions", "apiVersion": "v1", "versions": ["v1"]}
            elif url == "/apis":
                value = {"kind": "APIGroupList", "apiVersion": "v1", "groups": [{
                    "name": "apps", "versions": [{"groupVersion": "apps/v1", "version": "v1"}],
                    "preferredVersion": {"groupVersion": "apps/v1", "version": "v1"},
                }]}
            elif url in ("/api/v1", "/apis/apps/v1"):
                version = "v1" if url == "/api/v1" else "apps/v1"
                kinds = [("pods", "Pod")] if version == "v1" else [
                    ("deployments", "Deployment"), ("replicasets", "ReplicaSet")]
                value = {"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": version,
                         "resources": [{"name": name, "kind": kind, "namespaced": True,
                                        "singularName": "", "verbs": ["get", "list", "delete"]}
                                       for name, kind in kinds]}
            elif url == f"/apis/apps/v1/namespaces/{NAMESPACE}/deployments/eva-agent":
                value = cluster.deployment
            elif url == f"/apis/apps/v1/namespaces/{NAMESPACE}/replicasets/eva-agent-old":
                value = cluster.replica_set
            elif url == f"/api/v1/namespaces/{NAMESPACE}/pods":
                value = {"apiVersion": "v1", "kind": "PodList", "metadata": {"resourceVersion": "7"},
                         "items": [cluster.pod] if state["present"] else []}
            elif url == f"/api/v1/namespaces/{NAMESPACE}/pods/eva-agent-old-completed" and state["present"]:
                value = cluster.pod
            else:
                self.send_json({"kind": "Status", "apiVersion": "v1", "status": "Failure",
                                "reason": "NotFound", "code": 404}, 404)
                return
            self.send_json(value)

        def do_DELETE(self):
            # kubectl streams stdin with chunked transfer encoding on some versions.
            if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
                body = b""
                while True:
                    size = int(self.rfile.readline().strip(), 16)
                    if not size:
                        self.rfile.readline()
                        break
                    body += self.rfile.read(size)
                    self.rfile.read(2)
            else:
                body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
            requests.append(("DELETE", self.path, json.loads(body)))
            expected = {"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {
                "uid": "pod-uid", "resourceVersion": "7",
            }}
            if state["conflict"] or json.loads(body) != expected or self.path.split("?", 1)[0] != (
                    f"/api/v1/namespaces/{NAMESPACE}/pods/eva-agent-old-completed"):
                self.send_json({"kind": "Status", "apiVersion": "v1", "status": "Failure",
                                "reason": "Conflict", "code": 409}, 409)
                return
            state["present"] = False
            self.send_json({"apiVersion": "v1", "kind": "Status", "status": "Success"})

    server = ThreadingHTTPServer(("127.0.0.1", 0), API)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="eva-agent-completed-") as temporary:
            work = Path(temporary)
            config = work / "kubeconfig"
            config.write_text(yaml.safe_dump({
                "apiVersion": "v1", "kind": "Config", "current-context": "fixture", "users": [],
                "clusters": [{"name": "fixture", "cluster": {"server": f"http://127.0.0.1:{server.server_port}"}}],
                "contexts": [{"name": "fixture", "context": {"cluster": "fixture"}}],
            }))
            play = work / "play.yaml"
            fixture = deepcopy(cleanup_task)
            fixture["block"][0]["ansible.builtin.copy"]["src"] = str(helper_path)
            play.write_text(yaml.safe_dump([{
                "hosts": "localhost", "connection": "local", "gather_facts": False,
                "vars": {"eva_agent_workdir": str(work), "eva_agent_namespace": NAMESPACE,
                         "eva_agent_kubeconfig": str(config), "eva_agent_deployment_exists": {"rc": 0},
                         "ansible_python_interpreter": sys.executable},
                "tasks": [fixture, {"name": "Verify cleanup changed status", "ansible.builtin.assert": {
                    "that": ["eva_agent_completed_cleanup is changed == (expected_changed | bool)"]}}],
            }], sort_keys=False))
            environment = dict(os.environ, KUBECONFIG=str(config),
                               PATH=str(Path(ansible).parent) + os.pathsep + os.environ["PATH"])
            for expected in (True, False):
                result = subprocess.run([ansible, "-i", "localhost,", str(play),
                                         "-e", "expected_changed=" + str(expected).lower()],
                                        env=environment, text=True, capture_output=True, timeout=90)
                if result.returncode:
                    diagnostic = subprocess.run(["kubectl", "--request-timeout=5s", "-n", NAMESPACE,
                                                 "get", "deployment", "eva-agent", "-o", "json"],
                                                env=environment, text=True, capture_output=True, timeout=15)
                    raise AssertionError(result.stdout + result.stderr + repr(requests) + diagnostic.stderr)
            assert len([item for item in requests if item[0] == "DELETE"]) == 1
            count = len(requests)
            for extra in (["--check"], ["-e", '{"eva_agent_deployment_exists":{"rc":1}}']):
                result = subprocess.run([ansible, "-i", "localhost,", str(play), *extra,
                                         "-e", "expected_changed=false"],
                                        env=environment, text=True, capture_output=True, timeout=60)
                assert result.returncode == 0, result.stdout + result.stderr
                assert len(requests) == count  # Gates must not even inspect the cluster.
            state.update(present=True, conflict=True)
            result = subprocess.run([sys.executable, str(helper_path), "--namespace", NAMESPACE],
                                    env=environment, text=True, capture_output=True, timeout=60)
            assert result.returncode == 1 and not result.stdout and state["present"]
            assert len([item for item in requests if item[0] == "DELETE"]) == 2
    finally:
        server.shutdown()
        server.server_close()
        thread.join()
    print("[OK] real kubectl raw DELETE, Ansible changed/no-op/check-mode and 409 fail-stop verified offline")
else:
    print("[SKIP] real kubectl/Ansible integration (executables unavailable)")
PYTEST

echo 'EVA Agent reinstall wait contract tests passed.'
