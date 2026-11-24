#!/usr/bin/env python3
"""Remove only completed, verified Deployment-owned Agent Pods before reinstall."""

import argparse
import json
import re
import subprocess
import sys


DEPLOYMENT = "eva-agent"
SELECTOR = "app.kubernetes.io/name=eva-agent,pod-template-hash"
NAME = re.compile(r"[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?\Z")


class CleanupError(Exception):
    pass


def kubectl(arguments, payload=None):
    try:
        result = subprocess.run(
            ["kubectl", "--request-timeout=30s", *arguments], input=payload,
            text=True, capture_output=True, timeout=40,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise CleanupError("Cannot inspect or clean up completed EVA Agent Pods") from None
    if result.returncode:
        # Do not expose arbitrary cluster output. In particular, never retry a
        # deletion conflict without rechecking identity and ownership.
        raise CleanupError("EVA Agent Pod cleanup failed; check permissions or concurrent changes and retry")
    if not result.stdout.strip():
        return None
    try:
        value = json.loads(result.stdout)
    except ValueError:
        raise CleanupError("Invalid Kubernetes response during EVA Agent Pod cleanup") from None
    if not isinstance(value, dict):
        raise CleanupError("Invalid Kubernetes response during EVA Agent Pod cleanup")
    return value


def get(namespace, kind, name):
    return kubectl(["-n", namespace, "get", kind, name, "--ignore-not-found", "-o", "json"])


def identity(resource, namespace, name=None):
    metadata = resource.get("metadata") or {}
    actual_name = metadata.get("name", "")
    if (not isinstance(actual_name, str) or not NAME.fullmatch(actual_name)
            or (name is not None and actual_name != name)
            or metadata.get("namespace") != namespace
            or not isinstance(metadata.get("uid"), str) or not metadata["uid"]
            or not isinstance(metadata.get("resourceVersion"), str) or not metadata["resourceVersion"]):
        raise CleanupError("EVA Agent resource identity is missing or changed; retry installation")
    return metadata


def controller(metadata, kind):
    owners = [owner for owner in metadata.get("ownerReferences", []) if owner.get("controller") is True]
    if len(owners) != 1:
        return None
    owner = owners[0]
    if (owner.get("apiVersion") != "apps/v1" or owner.get("kind") != kind
            or not isinstance(owner.get("name"), str) or not NAME.fullmatch(owner["name"])
            or not owner.get("uid")):
        return None
    return owner


def completed(pod, namespace):
    metadata = pod.get("metadata") or {}
    labels = metadata.get("labels") or {}
    if (pod.get("apiVersion") != "v1" or pod.get("kind") != "Pod"
            or metadata.get("namespace") != namespace
            or labels.get("app.kubernetes.io/name") != DEPLOYMENT
            or "pod-template-hash" not in labels
            or pod.get("status", {}).get("phase") != "Succeeded"
            or metadata.get("deletionTimestamp") or metadata.get("finalizers")):
        return False
    return controller(metadata, "ReplicaSet") is not None


def paused_deployment(namespace, uid=None):
    deployment = get(namespace, "deployment", DEPLOYMENT)
    if deployment is None:
        raise CleanupError("EVA Agent Deployment disappeared; retry installation")
    metadata = identity(deployment, namespace, DEPLOYMENT)
    replicas = deployment.get("spec", {}).get("replicas")
    if (deployment.get("apiVersion") != "apps/v1" or deployment.get("kind") != "Deployment"
            or metadata.get("deletionTimestamp") or (uid is not None and metadata["uid"] != uid)
            or type(replicas) is not int or replicas != 0):
        raise CleanupError("EVA Agent Deployment changed or is not scaled to zero; check its manager before retrying")
    return metadata["uid"]


def cleanup(namespace):
    deployment_uid = paused_deployment(namespace)
    pods = kubectl(["-n", namespace, "get", "pods", "-l", SELECTOR, "-o", "json"])
    if pods is None or not isinstance(pods.get("items"), list):
        raise CleanupError("Invalid EVA Agent Pod list")
    deleted = []
    for listed in pods["items"]:
        if not completed(listed, namespace):
            continue
        original = identity(listed, namespace)
        # Re-read candidates: a list result is not authority to delete a
        # replacement Pod, a running Pod or one that gained a finalizer.
        pod = get(namespace, "pod", original["name"])
        if pod is None:
            continue
        metadata = identity(pod, namespace, original["name"])
        if metadata["uid"] != original["uid"]:
            raise CleanupError("EVA Agent Pod was replaced; retry installation")
        if not completed(pod, namespace):
            continue
        owner = controller(metadata, "ReplicaSet")
        replica_set = get(namespace, "replicaset", owner["name"])
        if replica_set is None:
            continue
        rs_metadata = identity(replica_set, namespace, owner["name"])
        parent = controller(rs_metadata, "Deployment")
        if (replica_set.get("apiVersion") != "apps/v1" or replica_set.get("kind") != "ReplicaSet"
                or rs_metadata["uid"] != owner["uid"] or parent is None
                or parent["name"] != DEPLOYMENT or parent["uid"] != deployment_uid):
            continue
        # Do not require ReplicaSet replicas=0: its controller may not yet have
        # observed the Deployment scale-down. Never modify the ReplicaSet.
        paused_deployment(namespace, deployment_uid)
        options = {"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {
            "uid": metadata["uid"], "resourceVersion": metadata["resourceVersion"],
        }}
        kubectl(["delete", "--raw", f"/api/v1/namespaces/{namespace}/pods/{metadata['name']}",
                 "-f", "-"], json.dumps(options))
        deleted.append(metadata["name"])
    return {"changed": bool(deleted), "deleted_pods": deleted}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--namespace", required=True)
    args = parser.parse_args()
    if not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", args.namespace):
        parser.error("Invalid Kubernetes namespace")
    try:
        print(json.dumps(cleanup(args.namespace)))
    except CleanupError as error:
        print(str(error), file=sys.stderr)
        return 1
    except (AttributeError, KeyError, TypeError):
        # Keep malformed/unexpected API responses fail-closed as well.
        print("Invalid Kubernetes metadata; completed EVA Agent Pod cleanup stopped", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())