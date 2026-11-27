"""Align stopped App images and pull references without changing image content."""

import argparse
import copy
import json
import re
import subprocess
import sys
import uuid
from pathlib import Path

import yaml


class UnsafeReconciliation(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise UnsafeReconciliation(message)


def references(value):
    require(isinstance(value, list), "App imagePullSecrets must be a list.")
    require(all(isinstance(ref, dict) and set(ref) == {"name"}
                and isinstance(ref["name"], str) and ref["name"] for ref in value),
            "App imagePullSecrets contains an invalid reference.")
    return value


def desired_spec(manifest, namespace, release):
    try:
        documents = list(yaml.safe_load_all(Path(manifest).read_text()))
    except (OSError, yaml.YAMLError) as exc:
        raise UnsafeReconciliation("Cannot read the rendered App manifest.") from exc
    resources = []
    for document in documents:
        if document is None:
            continue
        require(isinstance(document, dict), "Invalid rendered App resource.")
        resources.extend(document.get("items", []) if document.get("kind") == "List" else [document])
    deployments = [r for r in resources if isinstance(r, dict) and r.get("kind") == "Deployment"
                   and r.get("metadata", {}).get("name") == release
                   and r.get("metadata", {}).get("namespace", namespace) == namespace]
    require(len(deployments) == 1, "Rendered manifest must identify exactly one App Deployment.")
    try:
        pod_spec = deployments[0]["spec"]["template"]["spec"]
    except (KeyError, TypeError) as exc:
        raise UnsafeReconciliation("Rendered App Pod specification is missing.") from exc
    require(isinstance(pod_spec, dict), "Rendered App Pod specification is invalid.")
    return pod_spec


def desired_references(manifest, namespace, release):
    pod_spec = desired_spec(manifest, namespace, release)
    value = pod_spec.get("imagePullSecrets")
    return references([] if value is None else value)


def image_map(spec):
    images = {}
    for kind in ("containers", "initContainers"):
        containers = spec.get(kind, [])
        require(isinstance(containers, list), "Invalid App containers.")
        for container in containers:
            require(isinstance(container, dict) and isinstance(container.get("name"), str)
                    and isinstance(container.get("image"), str) and container["image"], "Invalid App image identity.")
            key = ("init:" if kind == "initContainers" else "") + container["name"]
            require(key not in images, "Ambiguous App container identity.")
            images[key] = container["image"]
    require(images, "Missing App images.")
    return images


def repository(image):
    require(isinstance(image, str) and image and "://" not in image, "Invalid container image reference.")
    name = image.split("@", 1)[0]
    return name.rsplit(":", 1)[0] if ":" in name.rsplit("/", 1)[-1] else name


def prepare_harbor(namespace, release, expected_uid, manifest, registry, run=subprocess.run):
    """Verify old bytes through the chosen Harbor BEFORE stopping App.

    Old references are used only for local CRI metadata (inspecti, never pull).
    A volume-free Pod verifies Harbor access with the final workload credentials.
    No ECR download, old-Secret restoration, tag substitution or new-version fallback.
    """
    require(bool(re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.-]*(?::[0-9]+)?", registry)), "Invalid selected Harbor host.")

    def invoke(args, body=None):
        result = run(args, input=body, text=True, capture_output=True, timeout=360)
        require(result.returncode == 0, "Harbor App image verification failed; ensure the existing image digest is published in the selected Harbor. No external-registry fallback allowed.")
        return result.stdout

    def kubectl(*args):
        return ["kubectl", "-n", namespace, *args]

    deployment = json.loads(invoke(kubectl("get", "deployment", release, "-o", "json")))
    require(deployment.get("metadata", {}).get("uid") == expected_uid, "App identity changed before Harbor image verification.")
    current_spec = deployment["spec"]["template"]["spec"]
    original = image_map(current_spec)
    proposed = desired_spec(manifest, namespace, release)
    targets = image_map(proposed)
    require(set(original) == set(targets), "Container changes require separate review before Harbor image relocation.")
    require(all(repository(image).startswith(registry + "/") for image in targets.values()),
            "Closed-network App images must all use the selected Harbor.")
    nodes = json.loads(invoke(["kubectl", "get", "nodes", "-o", "json"]))["items"]
    require(len(nodes) == 1, "Automatic cached App image relocation requires a single verified k3s node.")
    node = nodes[0]["metadata"]["name"]
    refs = references(proposed.get("imagePullSecrets") or [])
    verified = {}
    for key, image in original.items():
        status = json.loads(invoke(["k3s", "crictl", "inspecti", image])).get("status") or {}
        old_id = status.get("id", "")
        require(bool(re.fullmatch(r"sha256:[0-9a-f]{64}", old_id)), "Existing App image is not in the local CRI cache; refusing to fetch it from an external registry.")
        digests = {ref.rsplit("@", 1)[-1] for ref in status.get("repoDigests", [])
                   if ref.startswith(repository(image) + "@") and re.fullmatch(r"sha256:[0-9a-f]{64}", ref.rsplit("@", 1)[-1])}
        if "@" in image:
            digests &= {image.rsplit("@", 1)[-1]}
        require(len(digests) == 1, "Existing App manifest digest is missing or ambiguous; refusing a different image for license reading.")
        candidate = repository(targets[key]) + "@" + next(iter(digests))
        name = "eva-app-image-check-" + uuid.uuid4().hex[:16]
        pod = {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "namespace": namespace}, "spec": {
            "nodeName": node, "restartPolicy": "Never", "automountServiceAccountToken": False,
            "activeDeadlineSeconds": 360, "terminationGracePeriodSeconds": 1, "imagePullSecrets": refs,
            "containers": [{"name": "verify", "image": candidate, "imagePullPolicy": "Always",
                            "command": ["/bin/sh", "-c", "sleep 360"],
                            "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                                "capabilities": {"drop": ["ALL"]}},
                            "resources": {"requests": {"cpu": "10m", "memory": "32Mi"},
                                          "limits": {"cpu": "100m", "memory": "128Mi"}}}]}}
        if "tolerations" in current_spec:
            pod["spec"]["tolerations"] = copy.deepcopy(current_spec["tolerations"])
        created = False
        try:
            invoke(kubectl("create", "-f", "-"), json.dumps(pod))
            created = True
            invoke(kubectl("wait", "--for=condition=Ready", "pod/" + name, "--timeout=300s"))
            actual = json.loads(invoke(["k3s", "crictl", "inspecti", candidate])).get("status") or {}
            require(actual.get("id") == old_id, "Harbor image content differs from the existing App; license preparation refused.")
            verified[key] = candidate
        finally:
            if created:
                invoke(kubectl("delete", "pod", name, "--ignore-not-found=true", "--wait=true", "--timeout=60s"))
    return {"changed": False, "status": "existing-images-verified-in-selected-harbor",
            "original_images": original, "images": verified, "pull_references": refs, "registry": registry}


def reconcile(namespace, release, expected_uid, desired, run=subprocess.run, expected_images=None,
              replacement_images=None, registry=None, required_registry=None):
    command = ["kubectl"]

    def invoke(args, body=None):
        result = run(command + args + ["-n", namespace], input=body, text=True, capture_output=True)
        require(result.returncode == 0, "App pull Secret reconciliation Kubernetes operation failed.")
        return result.stdout

    try:
        deployment = json.loads(invoke(["get", "deployment", release, "-o", "json"]))
    except (ValueError, TypeError) as exc:
        raise UnsafeReconciliation("Invalid existing App Deployment response.") from exc
    require(isinstance(deployment, dict), "Invalid existing App Deployment response.")
    metadata = deployment.get("metadata", {})
    require(metadata.get("uid") == expected_uid, "App Deployment identity changed before reconciliation.")
    version = metadata.get("resourceVersion")
    require(isinstance(version, str) and version, "App Deployment resourceVersion is missing.")
    spec = deployment.get("spec", {})
    if required_registry:
        require(all(repository(image).startswith(required_registry + "/")
                    for image in image_map(spec.get("template", {}).get("spec", {})).values()),
                "Refusing to restart App using a registry outside the selected Harbor.")
    if expected_images is not None:
        # Rescue must not put old registry credentials onto a new Helm image,
        # nor patch a running workload and accidentally start a second App.
        require(isinstance(expected_images, dict) and expected_images
                and all(isinstance(k, str) and k and isinstance(v, str) and v
                        for k, v in expected_images.items()), "Invalid original App image identities.")
        current_images = image_map(spec.get("template", {}).get("spec", {}))
        if current_images != expected_images:
            return {"changed": False, "status": "pull-reference-restoration-skipped-image-changed"}
        if spec.get("replicas", 1) != 0:
            return {"changed": False, "status": "pull-reference-restoration-skipped-app-not-stopped"}
    require(spec.get("replicas", 1) == 0, "App must be scaled to zero before pull Secret reconciliation.")
    labels = spec.get("selector", {}).get("matchLabels", {})
    require(isinstance(labels, dict) and labels and all(isinstance(k, str) and isinstance(v, str)
                                                     for k, v in labels.items()),
            "App Deployment Pod selector is missing or invalid.")
    selector = ",".join(k + "=" + v for k, v in sorted(labels.items()))
    try:
        pods = json.loads(invoke(["get", "pods", "-l", selector, "-o", "json"]))
    except (ValueError, TypeError) as exc:
        raise UnsafeReconciliation("Invalid existing App Pod response.") from exc
    require(isinstance(pods, dict), "Invalid existing App Pod response.")
    require(isinstance(pods.get("items"), list) and not pods["items"],
            "App Pods must be fully terminated before pull Secret reconciliation.")
    value = spec.get("template", {}).get("spec", {}).get("imagePullSecrets")
    current = references([] if value is None else value)
    desired = references(desired)
    updates = {"imagePullSecrets": desired}
    if replacement_images is not None:
        require(expected_images is not None and registry, "Harbor image relocation requires original image identities and selected registry.")
        require(isinstance(replacement_images, dict) and set(replacement_images) == set(expected_images),
                "Invalid verified Harbor image identities.")
        require(all(isinstance(image, str) and image.startswith(registry + "/")
                    and re.fullmatch(r"sha256:[0-9a-f]{64}", image.rsplit("@", 1)[-1])
                    for image in replacement_images.values()), "Recovery images must be immutable selected-Harbor references.")
        for kind in ("containers", "initContainers"):
            if kind in spec["template"]["spec"]:
                updates[kind] = copy.deepcopy(spec["template"]["spec"][kind])
                for container in updates[kind]:
                    key = ("init:" if kind == "initContainers" else "") + container["name"]
                    container.update(image=replacement_images[key], imagePullPolicy="IfNotPresent")
    if all(spec["template"]["spec"].get(key) == value for key, value in updates.items()):
        return {"changed": False, "status": "pull-references-already-match-rendered-chart"}
    patch = {"metadata": {"uid": expected_uid, "resourceVersion": version},
             "spec": {"replicas": 0, "template": {"spec": updates}}}
    invoke(["patch", "deployment", release, "--type=merge", "--patch-file=/dev/stdin"], json.dumps(patch))
    return {"changed": True, "status": "pull-references-reconciled-with-app-stopped"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--release", required=True)
    parser.add_argument("--expected-uid", required=True)
    parser.add_argument("--manifest")
    parser.add_argument("--restore-images")
    parser.add_argument("--restore-references")
    parser.add_argument("--harbor-plan")
    parser.add_argument("--prepare-harbor", action="store_true")
    parser.add_argument("--registry")
    parser.add_argument("--required-registry", default="")
    args = parser.parse_args()
    try:
        if args.prepare_harbor:
            require(args.manifest and args.registry and not args.harbor_plan,
                "Harbor image verification requires the rendered manifest and registry.")
            result = prepare_harbor(args.namespace, args.release, args.expected_uid, args.manifest, args.registry)
            print(json.dumps(result))
            return 0
        if args.harbor_plan is not None:
            plan = json.loads(args.harbor_plan)
            require(isinstance(plan, dict) and isinstance(plan.get("original_images"), dict)
                and plan["original_images"] and isinstance(plan.get("images"), dict)
                and plan.get("registry") == args.registry, "Invalid Harbor image verification plan.")
            result = reconcile(args.namespace, args.release, args.expected_uid, references(plan["pull_references"]),
                       expected_images=plan["original_images"], replacement_images=plan["images"], registry=args.registry)
            require(not result["status"].startswith("pull-reference-restoration-skipped"),
                "App image or replicas changed before Harbor relocation; refusing to preserve using old registry images.")
            print(json.dumps(result))
            return 0
        restore = args.restore_images is not None or args.restore_references is not None
        require(bool(args.manifest) != restore, "Choose rendered manifest reconciliation or original pull-reference restoration.")
        if restore:
            require(args.restore_images is not None and args.restore_references is not None,
                    "Original image identities and pull references are both required.")
            expected_images = json.loads(args.restore_images)
            require(isinstance(expected_images, dict) and expected_images,
                    "Original image identities must be a nonempty mapping.")
            desired = references(json.loads(args.restore_references))
        else:
            expected_images = None
            desired = desired_references(args.manifest, args.namespace, args.release)
        result = reconcile(args.namespace, args.release, args.expected_uid, desired,
                           expected_images=expected_images, required_registry=args.required_registry or None)
    except (UnsafeReconciliation, OSError, ValueError, KeyError, TypeError, subprocess.TimeoutExpired) as exc:
        # Rendered resources and kubectl diagnostics may include credentials.
        # Keep failure messages controlled instead of printing those responses.
        print(str(exc) if isinstance(exc, UnsafeReconciliation) else "App pull Secret reconciliation process failed.", file=sys.stderr)
        return 1
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    sys.exit(main())
