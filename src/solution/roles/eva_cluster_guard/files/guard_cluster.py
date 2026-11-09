#!/usr/bin/env python3
"""Guard exact shared-role Helm inputs; never install or change cluster objects."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys

import yaml


def run(command, environment=None):
    try:
        r = subprocess.run(command, text=True, capture_output=True, timeout=180,
                           env=dict(os.environ, **environment) if environment else None)
    except FileNotFoundError:
        raise RuntimeError("Required validation tool not found: " + command[0]) from None
    except subprocess.TimeoutExpired:
        raise RuntimeError("Cluster validation timed out: " + " ".join(command[:4] if command[0] == "kubectl" else command[:3])) from None
    if r.returncode:
        raise RuntimeError("Cluster validation failed: " + " ".join(command[:4] if command[0] == "kubectl" else command[:3]))
    return r.stdout


def optional_field(obj, name, empty):
    """Kubernetes optional maps/lists may be omitted or explicitly null."""
    value = obj.get(name)
    if value is None:
        return empty
    if not isinstance(value, type(empty)):
        raise RuntimeError("Invalid Kubernetes field " + name + ": expected " + type(empty).__name__)
    return value


def error_message(error):
    # Parser/CLI exception messages can include Secret data and Helm --set values.
    if isinstance(error, RuntimeError):
        return str(error)
    return "Cannot validate the selected Solution manifest (" + type(error).__name__ + ")"


def items(kind, namespace=None, environment=None, name=None):
    command = ["kubectl", "--request-timeout=20s", "get", kind, "-o", "json"]
    if name is not None:
        command += [name, "--ignore-not-found"]
    if namespace:
        command += ["-n", namespace]
    elif kind not in {"nodes", "storageclasses", "runtimeclasses", "ingressclasses", "persistentvolumes"}:
        command += ["-A"]
    raw = run(command, environment)
    if not raw.strip():
        return []
    value = json.loads(raw)
    return value["items"] if "items" in value else [value]


def workload(resource):
    if resource.get("kind") in {"Deployment", "StatefulSet", "DaemonSet", "Job"}:
        return resource["spec"]["template"]["spec"]
    if resource.get("kind") == "CronJob":
        return resource["spec"]["jobTemplate"]["spec"]["template"]["spec"]
    return None


def identity(resource, ns):
    meta = resource.get("metadata", {})
    if resource.get("kind") in {"PersistentVolume", "Namespace", "ClusterRole", "ClusterRoleBinding", "CustomResourceDefinition"}:
        return resource.get("kind"), "", meta.get("name")
    return resource.get("kind"), meta.get("namespace", ns), meta.get("name")


def gpu_requests(pod):
    total = {}
    for c in pod.get("containers", []):
        for name, amount in optional_field(optional_field(c, "resources", {}), "limits", {}).items():
            if name.startswith("nvidia.com/"):
                total[name] = total.get(name, 0) + int(amount)
    for c in optional_field(pod, "initContainers", []):
        for name, amount in optional_field(optional_field(c, "resources", {}), "limits", {}).items():
            if name.startswith("nvidia.com/"):
                total[name] = max(total.get(name, 0), int(amount))
    return total


def permits(node, pod):
    labels = optional_field(node["metadata"], "labels", {})
    if not all(labels.get(k) == v for k, v in optional_field(pod, "nodeSelector", {}).items()):
        return False
    for taint in optional_field(optional_field(node, "spec", {}), "taints", []):
        if taint.get("effect") not in {"NoSchedule", "NoExecute"}:
            continue
        if not any((not t.get("effect") or t["effect"] == taint["effect"])
                   and ((t.get("operator") == "Exists" and (not t.get("key") or t["key"] == taint["key"]))
                        or (t.get("operator", "Equal") == "Equal" and t.get("key") == taint["key"] and t.get("value", "") == taint.get("value", "")))
                   for t in optional_field(pod, "tolerations", [])):
            return False
    affinity = optional_field(pod, "affinity", {})
    node_affinity = optional_field(affinity, "nodeAffinity", {})
    required = optional_field(node_affinity, "requiredDuringSchedulingIgnoredDuringExecution", {})
    terms = required.get("nodeSelectorTerms")
    if terms is None:
        return True
    def matches(expression, values):
        actual = values.get(expression["key"])
        expected = optional_field(expression, "values", [])
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
               and all(matches(e, labels) for e in optional_field(term, "matchExpressions", []))
               and all(matches(e, {"metadata.name": node["metadata"]["name"]}) for e in optional_field(term, "matchFields", []))
               for term in terms)


def storage_amount(value):
    from decimal import Decimal, InvalidOperation
    import re
    match = re.fullmatch(r"([+-]?(?:\d+(?:\.\d*)?|\.\d+))([a-zA-Z]*|[eE][+-]?\d+)", str(value))
    if not match:
        raise RuntimeError("Invalid PVC storage quantity")
    number, suffix = match.groups()
    scales = {"": 1, "n": Decimal("1e-9"), "u": Decimal("1e-6"), "m": Decimal("1e-3"),
              "k": 10**3, "K": 10**3, "M": 10**6, "G": 10**9, "T": 10**12, "P": 10**15, "E": 10**18,
              **{unit + "i": 2**power for unit, power in zip("KMGTPE", range(10, 61, 10))}}
    try:
        scale = scales[suffix] if suffix in scales else Decimal(10)**int(suffix[1:])
        return Decimal(number) * scale
    except (KeyError, ValueError, InvalidOperation):
        raise RuntimeError("Invalid PVC storage quantity") from None


def persistent_mounts(pod):
    import re
    def path(value):
        return re.sub(r"/+", "/", value).rstrip("/") or "/"
    volumes = {}
    for volume in optional_field(pod, "volumes", []):
        if "hostPath" in volume:
            volumes[volume["name"]] = ("hostPath", path(volume["hostPath"]["path"]))
        elif "persistentVolumeClaim" in volume:
            volumes[volume["name"]] = ("PersistentVolumeClaim", volume["persistentVolumeClaim"]["claimName"])
    return {(volumes[mount["name"]], path(mount["mountPath"]),
             mount.get("subPath") or "", mount.get("subPathExpr") or "")
            for container in pod.get("containers", [])
            for mount in optional_field(container, "volumeMounts", []) if mount["name"] in volumes}


def validate(request, docs, old, query=None):
    ns = request.get("namespace", "eva-" + request["component"])
    release = request.get("release", "eva-" + request["component"])
    query = query or (lambda kind, namespace=None, name=None: items(kind, namespace, name=name))
    rendered = {identity(d, ns): d for d in docs if isinstance(d, dict)}
    previous = {identity(d, ns): d for d in old if isinstance(d, dict)}
    provided = set(rendered)
    stateful_claims = {}
    warnings = []
    def warn(message):
        if message not in warnings:
            warnings.append(message)
    live = {}
    plurals = {"Secret": "secrets", "ConfigMap": "configmaps", "PersistentVolumeClaim": "persistentvolumeclaims",
               "PersistentVolume": "persistentvolumes", "ServiceAccount": "serviceaccounts"}
    def lookup(key):
        if key not in live:
            rows = query(plurals[key[0]], key[1] or None, key[2])
            live[key] = next((row for row in rows if identity(row, ns) == key), None)
        return live[key]
    def cluster_class(kind, name):
        plural = {"StorageClass": "storageclasses", "RuntimeClass": "runtimeclasses", "IngressClass": "ingressclasses"}[kind]
        rows = query(plural, None, name)
        if rows is None:
            warn(kind + " inspection unavailable; Kubernetes will validate " + name)
            return None
        selected = next((row for row in rows if row["metadata"]["name"] == name), None)
        if selected is None:
            raise RuntimeError("Unknown cluster " + kind + ": " + name)
        return selected
    def check_claim(key):
        claim = lookup(key)
        if claim:
            phase = optional_field(claim, "status", {}).get("phase")
            if phase == "Lost":
                raise RuntimeError("Existing PVC has lost its volume: " + key[2])
            if phase == "Pending":
                warn("PVC " + key[2] + " is Pending; provisioning and binding are handled by Kubernetes")
        return claim
    def check_storage_class(key, spec):
        claim = check_claim(key)
        selected = spec.get("storageClassName")
        if not selected or spec.get("volumeName") or spec.get("selector") or (claim and optional_field(claim, "status", {}).get("phase") == "Bound"):
            return  # Static binding and already-bound PVCs do not require a StorageClass object.
        rows = query("storageclasses", None, selected)
        if rows is None:
            warn("StorageClass inspection unavailable; Kubernetes will validate " + selected)
            return
        if any(row["metadata"]["name"] == selected for row in rows):
            return
        pvs = [obj for obj in rendered.values() if obj.get("kind") == "PersistentVolume"]
        if not any(pv.get("spec", {}).get("storageClassName") == selected for pv in pvs):
            external = query("persistentvolumes", None, None)
            if external is None:
                warn("Cannot inspect static PV binding for " + key[2] + "; Kubernetes will validate it")
                return
            pvs += external
        if not any(pv.get("spec", {}).get("storageClassName") == selected and optional_field(pv, "status", {}).get("phase", "Available") == "Available" for pv in pvs):
            raise RuntimeError("Unknown cluster StorageClass: " + selected)
    # StatefulSet claims and pre-install ECR hooks create their own resources.
    # Built-in default ServiceAccounts are created by the namespace controller.
    for key, obj in rendered.items():
        if key[0] == "StatefulSet":
            start = optional_field(obj["spec"], "ordinals", {}).get("start", 0)
            for claim in optional_field(obj["spec"], "volumeClaimTemplates", []):
                replicas = obj["spec"].get("replicas")
                for ordinal in range(start, start + (1 if replicas is None else replicas)):
                    name = claim["metadata"]["name"] + "-" + key[2] + "-" + str(ordinal)
                    provided.add(("PersistentVolumeClaim", key[1], name))
                    stateful_claims.setdefault(key, []).append((("PersistentVolumeClaim", key[1], name), claim.get("spec", {})))
        if key[0] == "Job" and "ecr" in (key[2] or ""):
            hooks = optional_field(obj.get("metadata", {}), "annotations", {}).get("helm.sh/hook", "").split(",")
            if {"pre-install", "pre-upgrade"} & set(hooks):
                for container in workload(obj).get("containers", []):
                    for env in optional_field(container, "env", []):
                        if env.get("name") == "ECR_SECRET_NAME" and env.get("value"):
                            provided.add(("Secret", key[1], env["value"]))
    for key, obj in previous.items():
        current = rendered.get(key)
        if key[0] in {"PersistentVolume", "PersistentVolumeClaim"}:
            if current is None:
                retained = optional_field(obj.get("metadata", {}), "annotations", {}).get("helm.sh/resource-policy") == "keep"
                if request["component"] == "agent" or (retained and lookup(key)):
                    continue
                raise RuntimeError("The upgrade would remove existing storage " + str(key))
            if key[0] == "PersistentVolumeClaim":
                import copy
                before, after = copy.deepcopy(obj["spec"]), copy.deepcopy(current["spec"])
                amounts = []
                for spec in (before, after):
                    spec["volumeMode"] = spec.get("volumeMode") or "Filesystem"
                    spec["accessModes"] = sorted(spec.get("accessModes") or [])
                    amounts.append(storage_amount(spec["resources"]["requests"].pop("storage")))
                if before != after:
                    raise RuntimeError("Existing PV/PVC changes require a separate migration: " + str(key))
                if amounts[1] < amounts[0]:
                    raise RuntimeError("Existing PVC storage cannot be reduced: " + key[2])
                if amounts[1] > amounts[0]:
                    claim = lookup(key)
                    selected = current["spec"].get("storageClassName") or (claim or {}).get("spec", {}).get("storageClassName")
                    storage_class = cluster_class("StorageClass", selected) if selected else None
                    if storage_class is not None and not storage_class.get("allowVolumeExpansion"):
                        raise RuntimeError("StorageClass does not allow PVC expansion: " + selected)
            else:
                import copy
                before, after = copy.deepcopy(obj["spec"]), copy.deepcopy(current["spec"])
                old_size, new_size = storage_amount(before["capacity"].pop("storage")), storage_amount(after["capacity"].pop("storage"))
                for spec in (before, after):
                    spec["volumeMode"] = spec.get("volumeMode") or "Filesystem"
                    spec["accessModes"] = sorted(spec.get("accessModes") or [])
                if before != after or new_size < old_size:
                    raise RuntimeError("Existing PV/PVC changes require a separate migration: " + str(key))
        if current is None:
            continue
        if key[0] == "StatefulSet" and optional_field(obj["spec"], "volumeClaimTemplates", []) != optional_field(current["spec"], "volumeClaimTemplates", []):
            raise RuntimeError("Existing StatefulSet claim templates require a separate migration: " + str(key))
        old_pod, new_pod = workload(obj), workload(current)
        if old_pod is not None and new_pod is not None and request["component"] != "agent":
            if not persistent_mounts(old_pod).issubset(persistent_mounts(new_pod)):
                raise RuntimeError("Persistent workload volume or persistent mountPath/subPath changes require a separate migration")
    services = None
    for key, obj in rendered.items():
        if key[0] == "StatefulSet":
            for target, spec in stateful_claims.get(key, []):
                check_storage_class(target, spec)
        if key[0] == "PersistentVolumeClaim":
            check_storage_class(key, obj.get("spec", {}))
        if key[0] == "Ingress" and obj.get("spec", {}).get("ingressClassName"):
            cluster_class("IngressClass", obj["spec"]["ingressClassName"])
        if key[0] == "Service":
            ports = {p.get("nodePort") for p in optional_field(obj.get("spec", {}), "ports", []) if p.get("nodePort")}
            if ports:
                if services is None:
                    services = query("services", None, None)
                    if services is None:
                        warn("Global Service inspection unavailable; Kubernetes will validate NodePort allocation")
                for other in services or []:
                    if identity(other, ns) != key and ports & {p.get("nodePort") for p in optional_field(other["spec"], "ports", [])}:
                        raise RuntimeError("NodePort collision with " + str(identity(other, ns)))
        pod = workload(obj)
        if pod is None:
            continue
        if pod.get("runtimeClassName"):
            cluster_class("RuntimeClass", pod["runtimeClassName"])
        refs = [("Secret", p["name"]) for p in optional_field(pod, "imagePullSecrets", [])]
        if pod.get("serviceAccountName") and pod["serviceAccountName"] != "default":
            refs.append(("ServiceAccount", pod["serviceAccountName"]))
        for volume in optional_field(pod, "volumes", []):
            for field, kind, name_key in [("secret", "Secret", "secretName"), ("configMap", "ConfigMap", "name"), ("persistentVolumeClaim", "PersistentVolumeClaim", "claimName")]:
                if field in volume and not volume[field].get("optional"):
                    refs.append((kind, volume[field][name_key]))
        for container in pod.get("containers", []) + optional_field(pod, "initContainers", []):
            for env in optional_field(container, "envFrom", []):
                for field, kind in [("secretRef", "Secret"), ("configMapRef", "ConfigMap")]:
                    if field in env and not env[field].get("optional"):
                        refs.append((kind, env[field]["name"]))
            for env in optional_field(container, "env", []):
                for field, kind in [("secretKeyRef", "Secret"), ("configMapKeyRef", "ConfigMap")]:
                    ref = optional_field(env, "valueFrom", {}).get(field)
                    if ref and not ref.get("optional"):
                        refs.append((kind, ref["name"]))
        for kind, name in refs:
            target = (kind, key[1], name)
            if target not in provided and lookup(target) is None:
                raise RuntimeError("Missing required Kubernetes resource: " + str(target))
            if kind == "PersistentVolumeClaim":
                check_claim(target)
    gpu_workloads = [(key, obj, gpu_requests(workload(obj))) for key, obj in rendered.items()
                     if key[0] in {"Deployment", "StatefulSet"} and (obj["spec"].get("replicas") is None or obj["spec"]["replicas"] > 0)
                     and gpu_requests(workload(obj))]
    if gpu_workloads:
        nodes = query("nodes", None, None)
        for key, obj, required in gpu_workloads:
            for name in required:
                if not any(int(optional_field(optional_field(node, "status", {}), "allocatable", {}).get(name, 0)) > 0 for node in nodes):
                    raise RuntimeError("Unknown cluster GPU resource: " + name)
            candidates = [node for node in nodes if not optional_field(node, "spec", {}).get("unschedulable")
                          and any(c.get("type") == "Ready" and c.get("status") == "True" for c in optional_field(optional_field(node, "status", {}), "conditions", []))
                          and permits(node, workload(obj))
                          and all(int(optional_field(optional_field(node, "status", {}), "allocatable", {}).get(name, 0)) >= amount for name, amount in required.items())]
            if not candidates:
                warn("No current node fits GPU placement for " + key[2] + "; Kubernetes will wait for scheduling capacity")
            strategy = optional_field(obj["spec"], "strategy", {})
            if key in previous and gpu_requests(workload(previous[key])) and key[0] == "Deployment" and strategy.get("type", "RollingUpdate") != "Recreate" and str(optional_field(strategy, "rollingUpdate", {}).get("maxSurge", "25%")) not in {"0", "0%"}:
                warn("GPU rolling upgrade may need spare capacity for " + key[2] + "; use Recreate or maxSurge: 0 when necessary")
    return warnings


def validate_request(request, query):
    previous = Path(request["previous"])
    command = ["helm", "template", request.get("release", "eva-" + request["component"]), request["chart"],
               "-n", request.get("namespace", "eva-" + request["component"]), "--dry-run=server"]
    if previous.exists():
        command += ["--is-upgrade"]
    if request["version"]:
        command += ["--version", request["version"]]
    for file in request["values"]:
        command += ["-f", file]
    for setting in request["set"]:
        command += ["--set", setting]
    if request["post_renderer"]:
        command += ["--post-renderer", request["post_renderer"]]
    docs = list(yaml.safe_load_all(run(command, request.get("environment"))))
    if not any(isinstance(doc, dict) for doc in docs):
        raise RuntimeError("Selected chart rendered no Kubernetes resources")
    old = list(yaml.safe_load_all(previous.read_text())) if previous.exists() else []
    for warning in validate(request, docs, old, query):
        print("[WARN] " + warning)
    print("[OK] " + request["component"] + " shared Solution role manifest passed managed-cluster validation")


def main():
    p = argparse.ArgumentParser(description=__doc__)
    mode = p.add_mutually_exclusive_group(required=True)
    mode.add_argument("--request", action="append", help="Private role render request; repeat to check prepared inputs")
    mode.add_argument("--installed", nargs="+", choices=["iam", "app", "agent", "vision", "vllm", "qdrant"],
                      help="Check existing Helm manifests without changing Kubernetes resources")
    args = p.parse_args()
    cache = {}
    def query_for(environment):
        def query(kind, namespace=None, name=None):
            key = (environment.get("KUBECONFIG", os.environ.get("KUBECONFIG")), kind, namespace, name)
            if key not in cache:
                try:
                    cache[key] = items(kind, namespace, environment, name)
                except RuntimeError:
                    if namespace is not None or kind not in {"services", "storageclasses", "runtimeclasses", "ingressclasses", "persistentvolumes"} or (kind == "persistentvolumes" and name is not None):
                        raise
                    cache[key] = None  # Optional global inspection must not add installation RBAC requirements.
            return cache[key]
        return query
    if args.request:
        for path in args.request:
            request = json.loads(Path(path).read_text())
            validate_request(request, query_for(request.get("environment") or {}))
    else:
        for component in args.installed:
            dependency = component in {"vllm", "qdrant"}
            ns = "eva-agent" if dependency else "eva-" + component
            release = "eva-agent-" + component if dependency else ns
            installed = json.loads(run(["helm", "list", "-n", ns, "--all", "--filter", "^" + release + "$", "-o", "json"]))
            if not any(item.get("name") == release and item.get("status") != "uninstalled" for item in installed):
                print("[SKIP] " + release + " is not installed")
                continue
            docs = list(yaml.safe_load_all(run(["helm", "get", "manifest", release, "-n", ns])))
            if not any(isinstance(doc, dict) for doc in docs):
                print("[SKIP] " + release + " has no existing Kubernetes manifest")
                continue
            for warning in validate({"component": component, "namespace": ns, "release": release}, docs, docs, query_for({})):
                print("[WARN] " + warning)
            print("[OK] " + release + " existing manifest passed managed-cluster validation")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(error_message(error), file=sys.stderr)
        sys.exit(1)
