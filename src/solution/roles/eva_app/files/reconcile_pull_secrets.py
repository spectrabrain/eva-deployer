"""Align existing App pull references with Helm while all App Pods are stopped."""

import argparse
import json
import subprocess
import sys
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


def desired_references(manifest, namespace, release):
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
    value = pod_spec.get("imagePullSecrets")
    return references([] if value is None else value)


def reconcile(namespace, release, expected_uid, desired, run=subprocess.run):
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
    if current == desired:
        return {"changed": False, "status": "pull-references-already-match-rendered-chart"}
    patch = {"metadata": {"uid": expected_uid, "resourceVersion": version},
             "spec": {"replicas": 0, "template": {"spec": {"imagePullSecrets": desired}}}}
    invoke(["patch", "deployment", release, "--type=merge", "--patch-file=/dev/stdin"], json.dumps(patch))
    return {"changed": True, "status": "pull-references-reconciled-with-app-stopped"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--release", required=True)
    parser.add_argument("--expected-uid", required=True)
    parser.add_argument("--manifest", required=True)
    args = parser.parse_args()
    try:
        desired = desired_references(args.manifest, args.namespace, args.release)
        result = reconcile(args.namespace, args.release, args.expected_uid, desired)
    except (UnsafeReconciliation, OSError) as exc:
        # Rendered resources and kubectl diagnostics may include credentials.
        # Keep failure messages controlled instead of printing those responses.
        print(str(exc) if isinstance(exc, UnsafeReconciliation) else "App pull Secret reconciliation process failed.", file=sys.stderr)
        return 1
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    sys.exit(main())
