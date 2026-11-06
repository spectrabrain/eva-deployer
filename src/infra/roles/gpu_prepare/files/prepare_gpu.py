#!/usr/bin/env python3
"""Prepare supported EVA GPU layouts without replacing existing partitions.

Used by both eva install and eva preflight gpu, and by the boot restoration unit.
All device commands use argv, and only the selected physical GPU is mutated.
"""
import argparse
import csv
import json
import pathlib
import re
import socket
import subprocess
import sys
import time


class PreparationError(RuntimeError):
    pass


def run(*args, allow_failure=False):
    result = subprocess.run(args, text=True, capture_output=True, timeout=90)
    if result.returncode and not allow_failure:
        raise PreparationError("Command failed: " + " ".join(args) + "\n" + result.stdout + result.stderr)
    return result


def rows(text):
    return [[field.strip() for field in row] for row in csv.reader(text.splitlines()) if row]


def inventory():
    output = run("nvidia-smi", "--query-gpu=index,uuid,name,mig.mode.current,display_mode", "--format=csv,noheader").stdout
    result = []
    for row in rows(output):
        if len(row) != 5 or not row[0].isdigit() or not re.fullmatch(r"GPU-[a-zA-Z0-9-]+", row[1]):
            raise PreparationError("Cannot parse NVIDIA GPU inventory")
        result.append(dict(zip(("index", "uuid", "name", "mode", "display"), row)))
    if not result:
        raise PreparationError("nvidia-smi reported no GPUs")
    return result


def instances(gpu):
    gi = run("nvidia-smi", "mig", "-i", gpu, "-lgi", allow_failure=True)
    ci = run("nvidia-smi", "mig", "-i", gpu, "-lci", allow_failure=True)
    # nvidia-smi returns a nonzero status for an empty GI/CI table on some drivers.
    for result in (gi, ci):
        if result.returncode and "No " not in result.stdout + result.stderr:
            raise PreparationError("Cannot inspect MIG instances: " + result.stdout + result.stderr)
    gpu_instances = []
    compute_instances = []
    for line in gi.stdout.splitlines():
        match = re.fullmatch(r"\|\s*(\d+)\s+MIG\s+\S+\s+(\d+)\s+(\d+)\s+(\d+):(\d+)\s*\|", line.strip())
        if match and match[1] == gpu:
            gpu_instances.append({"profile": match[2], "id": match[3]})
        elif re.match(r"\|\s*\d+\s+MIG", line):
            raise PreparationError("Cannot parse MIG GPU instance row: " + line)
    for line in ci.stdout.splitlines():
        match = re.fullmatch(r"\|\s*(\d+)\s+(\d+)\s+MIG\s+\S+\s+(\d+)\s+(\d+)\s+(\d+):(\d+)\s*\|", line.strip())
        if match and match[1] == gpu:
            compute_instances.append({"gi": match[2], "profile": match[3], "id": match[4]})
        elif re.match(r"\|\s*\d+\s+\d+\s+MIG", line):
            raise PreparationError("Cannot parse MIG compute instance row: " + line)
    return gpu_instances, compute_instances


def inspect(model_map, agent=False, expected_uuid=""):
    gpus = inventory()
    names = [gpu["name"] for gpu in gpus]
    model = model_map.get(names[0], {}) if len(gpus) == 1 else {}
    needs_mig = int(model.get("gpu_mig_mig_instance_count", 0)) == 4
    supported_non_mig = ((names == ["NVIDIA RTX A6000"])
                         or (names == ["NVIDIA L40S"])
                         or (len(names) == 3 and len(set(names)) == 1
                             and names[0] in ("PRO 5000 Blackwell", "NVIDIA RTX PRO 5000 Blackwell")))
    if agent and not (needs_mig or supported_non_mig):
        raise PreparationError("Unsupported Agent GPU layout. Expected A6000x1, L40sx1, PRO5000x3 or PRO6000-MIGx4; detected " + str(names))
    if not needs_mig:
        if supported_non_mig and any(gpu["mode"] == "Enabled" for gpu in gpus):
            raise PreparationError("This EVA profile uses whole GPUs, but MIG is enabled. Existing MIG configuration was preserved; resolve the profile mismatch before installing.")
        return {"status": "whole_gpu_ready", "needs_mig": False, "ready": True, "changed": False, "gpus": gpus}
    gpu = gpus[0]
    if expected_uuid and gpu["uuid"] != expected_uuid:
        raise PreparationError("GPU UUID changed; refusing to apply the saved MIG configuration to another GPU")
    profile = str(model["gpu_mig_mig_profile_id"])
    if not profile.isdigit():
        raise PreparationError("Invalid MIG profile in the Release model map")
    gi, ci = instances(gpu["index"]) if gpu["mode"] == "Enabled" else ([], [])
    # A fresh GPU has zero instances. Partial/different GI layouts require an
    # operator decision rather than deleting or replacing running partitions.
    if gi and (len(gi) != 4 or any(item["profile"] != profile for item in gi)):
        raise PreparationError("Existing MIG layout differs from PRO6000-MIGx4 (profile " + profile + " x4). Existing partitions were preserved.")
    gi_ids = {item["id"] for item in gi}
    if any(item["gi"] not in gi_ids or item["profile"] != "0" for item in ci) or len({item["gi"] for item in ci}) != len(ci):
        raise PreparationError("Existing MIG compute instances differ from the required full compute slices; existing partitions were preserved.")
    ready = gpu["mode"] == "Enabled" and len(gi) == len(ci) == 4
    return {"status": "mig_ready" if ready else "mig_configuration_needed", "needs_mig": True,
            "ready": ready, "changed": False, "needs_selector": gpu["mode"] not in ("Enabled", "Disabled") or gpu["display"] == "Enabled",
            "gpu": gpu, "profile": profile, "gpu_instances": gi, "compute_instances": ci}


def apply(model_map, selector="", agent=False, expected_uuid=""):
    state = inspect(model_map, agent, expected_uuid)
    if state["ready"]:
        return state
    gpu = state["gpu"]
    processes = run("nvidia-smi", "--query-compute-apps=gpu_uuid,pid", "--format=csv,noheader")
    if any(row[0] == gpu["uuid"] for row in rows(processes.stdout)):
        raise PreparationError("GPU workloads are running. Stop those workloads and rerun eva preflight gpu; no running processes were killed.")
    if state["needs_selector"]:
        if not selector or not pathlib.Path(selector).is_file():
            raise PreparationError("Display Mode Selector is required. Include it when preparing the Release assets and rerun eva preflight gpu.")
        if gpu["display"] == "Enabled":
            # A PRO6000 MIG server uses compute mode. Stop the display manager
            # gracefully only for this transition, never kill GPU clients.
            stopped = run("systemctl", "stop", "display-manager.service", allow_failure=True)
            if stopped.returncode and "not loaded" not in stopped.stderr and "not found" not in stopped.stderr:
                raise PreparationError("Cannot stop the display manager for Compute Mode conversion: " + stopped.stderr)
        # Auto is scoped to the single physical GPU layout validated above.
        run(selector, "--gpumode", "compute", "--auto")
        gpu = inventory()[0]
        if gpu["mode"] not in ("Enabled", "Disabled"):
            raise PreparationError("EVA_GPU_REBOOT_REQUIRED: Compute mode was requested. Reboot this target and rerun the same eva preflight gpu or eva install command.")
    if gpu["mode"] != "Enabled":
        enabled = run("nvidia-smi", "-i", gpu["index"], "-mig", "1", allow_failure=True)
        current = inventory()[0]
        if current["mode"] != "Enabled":
            raise PreparationError("EVA_GPU_REBOOT_REQUIRED: MIG enable did not become active. Stop GPU clients, reboot this target if requested by the driver, and rerun the same command. " + enabled.stdout + enabled.stderr)
    # Reinspect after mode changes, so a conflicting layout can never be reset.
    state = inspect(model_map, agent, expected_uuid)
    if not state["gpu_instances"]:
        run("nvidia-smi", "mig", "-i", gpu["index"], "-cgi", ",".join([state["profile"]] * 4), "-C")
    else:
        existing = {item["gi"] for item in state["compute_instances"]}
        for item in state["gpu_instances"]:
            if item["id"] not in existing:
                run("nvidia-smi", "mig", "-i", gpu["index"], "-gi", item["id"], "-cci", "0")
    final = inspect(model_map, agent, expected_uuid)
    if not final["ready"]:
        raise PreparationError("MIG configuration is incomplete. Inspect the target GPU and rerun eva preflight gpu; existing instances were preserved.")
    final["changed"] = True
    return final


def reconcile_k3s(required=False):
    if not pathlib.Path("/etc/rancher/k3s/k3s.yaml").is_file():
        if required:
            raise PreparationError("k3s is not ready; install Infra before deploying Agent/Vision")
        return {"status": "hardware_ready_k3s_pending", "changed": False}
    def kubectl(*args):
        return run("kubectl", "--kubeconfig", "/etc/rancher/k3s/k3s.yaml", *args)
    machine_id = pathlib.Path("/etc/machine-id").read_text().strip()
    nodes = json.loads(kubectl("get", "nodes", "-o", "json").stdout)["items"]
    local = [node for node in nodes if node["status"].get("nodeInfo", {}).get("machineID") == machine_id]
    if not local:
        local = [node for node in nodes if node["metadata"]["name"] == socket.gethostname()]
    if len(local) != 1:
        raise PreparationError("Cannot uniquely identify this GPU's Kubernetes node; no plugin pods were restarted")
    name = local[0]["metadata"]["name"]
    def ready(node):
        resources = {key: int(value) for key, value in node["status"].get("allocatable", {}).items()
                     if key.startswith("nvidia.com/mig-") and int(value) > 0}
        return len(resources) == 1 and list(resources.values()) == [4]
    if ready(local[0]):
        return {"status": "k3s_mig_ready", "changed": False, "node": name}
    ds = json.loads(kubectl("-n", "kube-system", "get", "ds", "nvidia-device-plugin-daemonset", "-o", "json").stdout)
    containers = ds["spec"]["template"]["spec"]["containers"]
    env = {entry["name"]: entry.get("value") for container in containers for entry in container.get("env", [])}
    if env.get("MIG_STRATEGY") != "mixed" or env.get("DEVICE_DISCOVERY_STRATEGY") != "nvml":
        kubectl("-n", "kube-system", "set", "env", "ds/nvidia-device-plugin-daemonset", "MIG_STRATEGY=mixed", "DEVICE_DISCOVERY_STRATEGY=nvml")
    else:
        # Restart only this node's device plugin, never unrelated GPU workloads.
        pods = json.loads(kubectl("-n", "kube-system", "get", "pods", "--field-selector", "spec.nodeName=" + name, "-o", "json").stdout)["items"]
        for pod in pods:
            if any(owner.get("uid") == ds["metadata"]["uid"] for owner in pod["metadata"].get("ownerReferences", [])):
                kubectl("-n", "kube-system", "delete", "pod", pod["metadata"]["name"], "--wait=false")
    for _ in range(30):
        node = json.loads(kubectl("get", "node", name, "-o", "json").stdout)
        if ready(node):
            return {"status": "k3s_mig_ready", "changed": True, "node": name}
        time.sleep(5)
    raise PreparationError("k3s did not expose four MIG resources on node " + name + "; check NVIDIA Device Plugin readiness and rerun eva preflight gpu")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--model-map", required=True)
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--agent", action="store_true")
    parser.add_argument("--selector", default="")
    parser.add_argument("--gpu-uuid", default="")
    parser.add_argument("--reconcile-k3s", action="store_true")
    parser.add_argument("--require-k3s", action="store_true")
    args = parser.parse_args()
    try:
        if args.reconcile_k3s:
            result = reconcile_k3s(args.require_k3s)
        else:
            model_map = json.loads(pathlib.Path(args.model_map).read_text())
            result = apply(model_map, args.selector, args.agent, args.gpu_uuid) if args.apply else inspect(model_map, args.agent, args.gpu_uuid)
        print(json.dumps(result))
    except (PreparationError, OSError, ValueError, KeyError, subprocess.TimeoutExpired) as error:
        print(str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
