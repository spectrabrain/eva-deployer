#!/usr/bin/env python3
"""Inspect NKS and existing storage; installer Values remain authoritative."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys

import yaml


def run(command):
    result = subprocess.run(command, capture_output=True, text=True, timeout=90)
    if result.returncode:
        raise RuntimeError("Cluster inspection failed: " + " ".join(command[:4]))
    return result.stdout


def get(resource, namespace=None):
    args = ["kubectl", "--request-timeout=20s"]
    args += ["-n", namespace] if namespace else []
    return json.loads(run(args + ["get", resource, "-o", "json"]))["items"]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--workspace", required=True)
    parser.add_argument("--host", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    components = [name for name in os.environ["EVA_CLUSTER_COMPONENTS"].split(",") if name in {"app", "iam", "agent", "vision"}]
    context = run(["kubectl", "config", "current-context"]).strip()
    nodes = get("nodes")
    ready = [n for n in nodes if any(c.get("type") == "Ready" and c.get("status") == "True" for c in n.get("status", {}).get("conditions", []))]
    if not ready:
        raise RuntimeError("The selected Kubernetes cluster has no Ready nodes")
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=True, mode=0o700)
    facts = {"eva_cluster_values": {}, "eva_cluster_iam_node_selectors": {}, "eva_cluster_gpu_count": 0, "eva_cluster_mig_enabled": False}
    for n in ready:
        for key, count in n["status"].get("allocatable", {}).items():
            if key == "nvidia.com/gpu" or key.startswith("nvidia.com/mig-"):
                facts["eva_cluster_gpu_count"] += int(count)
                facts["eva_cluster_mig_enabled"] |= key.startswith("nvidia.com/mig-") and int(count) > 0
    host_file = output / "host-nodes.json"
    host_records = json.loads(host_file.read_text()) if host_file.exists() else {}
    for component in components:
        ns = release = "eva-" + component
        installed = json.loads(run(["helm", "list", "-n", ns, "--deployed", "--failed", "--pending", "--uninstalled", "--uninstalling", "--superseded", "-o", "json"]))
        if component == "agent":
            for dependency in ("vllm", "qdrant"):
                dep_release = "eva-agent-" + dependency
                status = next((r.get("status") for r in installed if r["name"] == dep_release), None)
                dep_path = output / (dependency + "-previous.yaml")
                if status is None:
                    dep_path.unlink(missing_ok=True)
                elif status not in {"deployed", "failed"}:
                    raise RuntimeError("Resolve the pending Helm operation before installing " + dep_release)
                else:
                    dep_path.write_text(run(["helm", "get", "manifest", dep_release, "-n", ns]))
                    dep_path.chmod(0o600)
        installed = [r for r in installed if r["name"] == release]
        source = Path(args.workspace) / "site-values" / (component + ".yaml")
        explicit = (yaml.safe_load(source.read_text()) or {}).get(args.host, {}) if source.exists() else {}
        if not isinstance(explicit, dict) or not explicit:
            raise RuntimeError(release + " requires installer Values in targets.controller.values." + component)
        manifest = []
        if installed:
            if installed[0].get("status") not in {"deployed", "failed"}:
                raise RuntimeError("Resolve the pending Helm operation before installing " + release)
            manifest = list(yaml.safe_load_all(run(["helm", "get", "manifest", release, "-n", ns])))
            path = output / (component + "-previous.yaml")
            path.write_text(yaml.safe_dump_all(manifest))
            path.chmod(0o600)
            pods = get("pods", ns)
            deployments = get("deployments", ns)
            for deployment in deployments:
                annotations = deployment["metadata"].get("annotations", {})
                if annotations.get("argocd.argoproj.io/tracking-id") or deployment["metadata"].get("labels", {}).get("argocd.argoproj.io/instance"):
                    raise RuntimeError("Argo CD tracking must be cleared before updating " + release)
                if annotations.get("meta.helm.sh/release-name") != release:
                    continue
                podspec = deployment["spec"]["template"]["spec"]
                if component == "iam" and explicit.get("persistence", {}).get("type") == "hostPath" and any("hostPath" in v for v in podspec.get("volumes", [])):
                    labels = deployment["spec"]["selector"].get("matchLabels", {})
                    hosts = {p["spec"].get("nodeName") for p in pods if all(p["metadata"].get("labels", {}).get(k) == v for k, v in labels.items()) and p["spec"].get("nodeName")}
                    record_key = ns + "/" + deployment["metadata"]["name"]
                    saved = host_records.get(record_key, {})
                    if not hosts and saved.get("uid") == deployment["metadata"]["uid"]:
                        hosts = {saved["node"]}
                    if len(hosts) != 1:
                        raise RuntimeError("Cannot preserve the data node of hostPath workload " + deployment["metadata"]["name"])
                    host = hosts.pop()
                    host_records[record_key] = {"uid": deployment["metadata"]["uid"], "node": host}
                    node = next((n for n in ready if n["metadata"]["name"] == host), None)
                    if node is None or node.get("spec", {}).get("unschedulable"):
                        raise RuntimeError("Existing hostPath data node is not Ready")
                    selector = {"kubernetes.io/hostname": node["metadata"]["labels"]["kubernetes.io/hostname"]}
                    name = deployment["metadata"]["name"]
                    block = "database" if name.endswith("-postgres") else name.removeprefix("eva-iam-")
                    facts["eva_cluster_iam_node_selectors"][block] = selector
        else:
            (output / (component + "-previous.yaml")).unlink(missing_ok=True)
        facts["eva_cluster_values"][component] = explicit
    host_file.write_text(json.dumps(host_records))
    host_file.chmod(0o600)
    path = output / "context.yaml"
    path.write_text(yaml.safe_dump(facts))
    path.chmod(0o600)
    print(json.dumps({"context": context, "ready_nodes": len(ready), "components": components}))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Never relay parsing errors, CLI stderr or Secret/Values contents.
        print(str(error) if isinstance(error, RuntimeError) else "Cannot inspect the selected Kubernetes cluster; check API authentication, permissions and tools", file=sys.stderr)
        sys.exit(1)
