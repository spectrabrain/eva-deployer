#!/usr/bin/env python3
"""Guard exact shared-role Helm inputs; never install or change cluster objects."""
import argparse
import json
from pathlib import Path
import subprocess
import sys

import yaml


def run(command):
    r = subprocess.run(command, text=True, capture_output=True, timeout=180)
    if r.returncode:
        raise RuntimeError("Cluster validation failed: " + " ".join(command[:3]))
    return r.stdout


def items(kind, namespace=None):
    command = ["kubectl", "--request-timeout=20s", "get", kind, "-o", "json"]
    command += ["-n", namespace] if namespace else ["-A"]
    return json.loads(run(command))["items"]


def workload(resource):
    if resource.get("kind") in {"Deployment", "StatefulSet", "DaemonSet", "Job"}:
        return resource["spec"]["template"]["spec"]
    if resource.get("kind") == "CronJob":
        return resource["spec"]["jobTemplate"]["spec"]["template"]["spec"]
    return None


def identity(resource, ns):
    meta = resource.get("metadata", {})
    return resource.get("kind"), meta.get("namespace", ns), meta.get("name")


def gpu_requests(pod):
    total = {}
    for c in pod.get("containers", []):
        for name, amount in c.get("resources", {}).get("limits", {}).items():
            if name.startswith("nvidia.com/"):
                total[name] = total.get(name, 0) + int(amount)
    for c in pod.get("initContainers", []):
        for name, amount in c.get("resources", {}).get("limits", {}).items():
            if name.startswith("nvidia.com/"):
                total[name] = max(total.get(name, 0), int(amount))
    return total


def permits(node, pod):
    labels = node["metadata"].get("labels", {})
    if not all(labels.get(k) == v for k, v in pod.get("nodeSelector", {}).items()):
        return False
    for taint in node.get("spec", {}).get("taints", []):
        if taint.get("effect") not in {"NoSchedule", "NoExecute"}:
            continue
        if not any((not t.get("effect") or t["effect"] == taint["effect"])
                   and ((t.get("operator") == "Exists" and (not t.get("key") or t["key"] == taint["key"]))
                        or (t.get("operator", "Equal") == "Equal" and t.get("key") == taint["key"] and t.get("value", "") == taint.get("value", "")))
                   for t in pod.get("tolerations", [])):
            return False
    terms = pod.get("affinity", {}).get("nodeAffinity", {}).get("requiredDuringSchedulingIgnoredDuringExecution", {}).get("nodeSelectorTerms")
    if terms is None:
        return True
    def matches(expression, values):
        actual = values.get(expression["key"])
        expected = expression.get("values", [])
        operator = expression["operator"]
        if operator == "In": return actual in expected
        if operator == "NotIn": return actual not in expected
        if operator == "Exists": return actual is not None
        if operator == "DoesNotExist": return actual is None
        if operator in {"Gt", "Lt"}:
            try:
                return int(actual) > int(expected[0]) if operator == "Gt" else int(actual) < int(expected[0])
            except (ValueError, TypeError, IndexError):
                return False
        return False
    return any((term.get("matchExpressions") or term.get("matchFields"))
               and all(matches(e, labels) for e in term.get("matchExpressions", []))
               and all(matches(e, {"metadata.name": node["metadata"]["name"]}) for e in term.get("matchFields", []))
               for term in terms)


def validate(request, docs, old, query=items):
    ns = request.get("namespace", "eva-" + request["component"])
    release = request.get("release", "eva-" + request["component"])
    rendered = {identity(d, ns): d for d in docs if isinstance(d, dict)}
    previous = {identity(d, ns): d for d in old if isinstance(d, dict)}
    live = {}
    for kind in ["secrets", "configmaps", "persistentvolumeclaims", "serviceaccounts", "services"]:
        for obj in query(kind):
            live[identity(obj, ns)] = obj
    # Only compare resources owned by this release. External ALB/Ingress routes
    # are outside the chart when ingress.enabled=false and remain untouched.
    for key, obj in previous.items():
        current = rendered.get(key)
        if key[0] in {"PersistentVolume", "PersistentVolumeClaim"}:
            if current is None:
                if request["component"] != "agent":
                    raise RuntimeError("The upgrade would remove existing storage " + str(key))
                continue  # Agent role retains obsolete model PV/PVC before 3.3 migration.
            if obj.get("spec") != current.get("spec"):
                raise RuntimeError("Existing PV/PVC changes require a separate migration: " + str(key))
        if current is None:
            continue
        if key[0] == "StatefulSet" and obj["spec"].get("volumeClaimTemplates") != current["spec"].get("volumeClaimTemplates"):
            raise RuntimeError("Existing StatefulSet claim templates require a separate migration: " + str(key))
        old_pod, new_pod = workload(obj), workload(current)
        if old_pod is not None and new_pod is not None:
            old_mounts = {v["name"]: v for v in old_pod.get("volumes", []) if "hostPath" in v or "persistentVolumeClaim" in v}
            new_mounts = {v["name"]: v for v in new_pod.get("volumes", [])}
            for name, volume in old_mounts.items():
                if new_mounts.get(name) != volume and request["component"] != "agent":
                    raise RuntimeError("Persistent workload volume changes require a separate migration: " + name)
            for container in old_pod.get("containers", []):
                after = next((c for c in new_pod.get("containers", []) if c["name"] == container["name"]), {})
                for mount in container.get("volumeMounts", []):
                    if mount["name"] in old_mounts and mount not in after.get("volumeMounts", []) and request["component"] != "agent":
                        raise RuntimeError("Existing persistent mountPath/subPath would change")
    class_names = {c["metadata"]["name"] for c in query("storageclasses")}
    runtime_names = {c["metadata"]["name"] for c in query("runtimeclasses")}
    ingress_names = {c["metadata"]["name"] for c in query("ingressclasses")}
    for key, obj in rendered.items():
        if key[0] == "StatefulSet":
            for claim in obj["spec"].get("volumeClaimTemplates", []):
                selected = claim.get("spec", {}).get("storageClassName")
                if selected and selected not in class_names:
                    raise RuntimeError("Unknown cluster StorageClass: " + selected)
                for ordinal in range(obj["spec"].get("replicas", 1)):
                    name = claim["metadata"]["name"] + "-" + key[2] + "-" + str(ordinal)
                    existing = live.get(("PersistentVolumeClaim", key[1], name))
                    if existing and existing.get("status", {}).get("phase") != "Bound":
                        raise RuntimeError("Existing PVC is not Bound: " + name)
        if key[0] == "PersistentVolumeClaim":
            selected = obj.get("spec", {}).get("storageClassName")
            if selected and selected not in class_names:
                raise RuntimeError("Unknown cluster StorageClass: " + selected)
        if key[0] == "Ingress":
            selected = obj.get("spec", {}).get("ingressClassName")
            if selected and selected not in ingress_names:
                raise RuntimeError("Unknown cluster IngressClass: " + selected)
        if key[0] == "Service":
            ports = {p.get("nodePort") for p in obj.get("spec", {}).get("ports", []) if p.get("nodePort")}
            for other_key, other in live.items():
                if other_key[0] == "Service" and other_key != key and ports & {p.get("nodePort") for p in other["spec"].get("ports", [])}:
                    raise RuntimeError("NodePort collision with " + str(other_key))
        pod = workload(obj)
        if pod is None:
            continue
        old_pod = workload(previous[key]) if key in previous else None
        old_paths = {v["hostPath"]["path"] for v in (old_pod or {}).get("volumes", []) if "hostPath" in v}
        for volume in pod.get("volumes", []):
            path = volume.get("hostPath", {}).get("path")
            if path and path not in old_paths and path not in {"/tmp/nvidia-mps", "/tmp/nvidia-mps-log"}:
                raise RuntimeError("New Cloud workloads require PVC/Secret instead of worker hostPath: " + path)
        if pod.get("runtimeClassName") and pod["runtimeClassName"] not in runtime_names:
            raise RuntimeError("Unknown cluster RuntimeClass: " + pod["runtimeClassName"])
        refs = []
        refs += [("Secret", p["name"]) for p in pod.get("imagePullSecrets", [])]
        refs += [("ServiceAccount", pod["serviceAccountName"])] if pod.get("serviceAccountName") else []
        for v in pod.get("volumes", []):
            for field, kind, name_key in [("secret", "Secret", "secretName"), ("configMap", "ConfigMap", "name"), ("persistentVolumeClaim", "PersistentVolumeClaim", "claimName")]:
                if field in v and not v[field].get("optional"):
                    refs.append((kind, v[field][name_key]))
        for c in pod.get("containers", []) + pod.get("initContainers", []):
            for e in (c.get("envFrom") or []):
                for field, kind in [("secretRef", "Secret"), ("configMapRef", "ConfigMap")]:
                    if field in e and not e[field].get("optional"):
                        refs.append((kind, e[field]["name"]))
            for e in (c.get("env") or []):
                for field, kind in [("secretKeyRef", "Secret"), ("configMapKeyRef", "ConfigMap")]:
                    ref = e.get("valueFrom", {}).get(field)
                    if ref and not ref.get("optional"):
                        refs.append((kind, ref["name"]))
        for kind, name in refs:
            target = (kind, key[1], name)
            # Hook-created Agent/App/IAM pull Secrets are prepared by the role's
            # credential flow and chart hook, not rendered as ordinary Secrets.
            generated_pull = name in {"eva-agent-dockerconfig", "ecr-registry"} and any(d.get("kind") in {"Job", "CronJob"} and "ecr" in d.get("metadata", {}).get("name", "") for d in docs if isinstance(d, dict))
            if target not in live and target not in rendered and not generated_pull:
                raise RuntimeError("Missing required Kubernetes resource: " + str(target))
            if kind == "PersistentVolumeClaim" and target in live and live[target].get("status", {}).get("phase") != "Bound":
                raise RuntimeError("Existing PVC is not Bound: " + name)
    nodes = [n for n in query("nodes") if not n["spec"].get("unschedulable") and any(c.get("type") == "Ready" and c.get("status") == "True" for c in n["status"].get("conditions", []))]
    allocated = {n["metadata"]["name"]: {} for n in nodes}
    for p in query("pods"):
        node = p["spec"].get("nodeName")
        if node not in allocated or p.get("status", {}).get("phase") in {"Succeeded", "Failed"}:
            continue
        if p["metadata"].get("namespace") == ns and p["metadata"].get("labels", {}).get("app.kubernetes.io/instance") == release:
            continue  # The shared role replaces only this selected workload.
        for name, amount in gpu_requests(p["spec"]).items():
            allocated[node][name] = allocated[node].get(name, 0) + amount
    for key, obj in rendered.items():
        pod = workload(obj)
        if pod is None or key[0] not in {"Deployment", "StatefulSet"}:
            continue
        required = gpu_requests(pod)
        if not required:
            continue
        strategy = obj["spec"].get("strategy", {})
        if key[0] == "Deployment" and strategy.get("type", "RollingUpdate") != "Recreate" and str(strategy.get("rollingUpdate", {}).get("maxSurge", "25%")) not in {"0", "0%"}:
            raise RuntimeError("GPU replacement requires Recreate or maxSurge: 0")
        for _ in range(obj["spec"].get("replicas", 1)):
            candidates = [n for n in nodes if permits(n, pod) and all(int(n["status"]["allocatable"].get(name, 0)) - allocated[n["metadata"]["name"]].get(name, 0) >= amount for name, amount in required.items())]
            if not candidates:
                raise RuntimeError("Insufficient schedulable GPU resources for " + str(key))
            node = candidates[0]["metadata"]["name"]
            for name, amount in required.items():
                allocated[node][name] = allocated[node].get(name, 0) + amount


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--request", required=True)
    args = p.parse_args()
    request = json.loads(Path(args.request).read_text())
    command = ["helm", "template", request.get("release", "eva-" + request["component"]), request["chart"],
               "-n", request.get("namespace", "eva-" + request["component"])]
    if request["version"]:
        command += ["--version", request["version"]]
    for file in request["values"]:
        command += ["-f", file]
    for setting in request["set"]:
        command += ["--set", setting]
    if request["post_renderer"]:
        command += ["--post-renderer", request["post_renderer"]]
    docs = list(yaml.safe_load_all(run(command)))
    previous = Path(request["previous"])
    old = list(yaml.safe_load_all(previous.read_text())) if previous.exists() else []
    validate(request, docs, old)
    print("[OK] Shared Solution role manifest passed managed-cluster validation")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(str(error) if isinstance(error, RuntimeError) else "Cannot validate the selected Solution manifest; check Kubernetes configuration and permissions", file=sys.stderr)
        sys.exit(1)
