#!/usr/bin/env python3
"""Prepare or renew namespace-local ECR auth without exposing token material."""

import argparse
import base64
import json
import os
import re
import shutil
import subprocess
import sys


REGISTRY = re.compile(r"(\d{12}\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com(?:\.cn)?)/[^\s]+\Z")
NAME = re.compile(r"[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?\Z")
MANAGED_REGISTRY = "eva.spectrabrain.com/vision-ecr-registry"


class AuthError(Exception):
    pass


def run(command, description, payload=None):
    try:
        result = subprocess.run(command, input=payload, capture_output=True, text=True, timeout=120)
    except (OSError, subprocess.TimeoutExpired):
        raise AuthError(description) from None
    if result.returncode:
        # AWS/kubectl errors can include credentials or the input Secret.
        raise AuthError(description)
    return result.stdout


def read_config(secret):
    try:
        if secret.get("type") != "kubernetes.io/dockerconfigjson":
            raise ValueError()
        encoded = secret["data"][".dockerconfigjson"]
        config = json.loads(base64.b64decode(encoded, validate=True))
        if not isinstance(config, dict) or not isinstance(config.get("auths"), dict):
            raise ValueError()
        return config
    except (KeyError, TypeError, ValueError):
        raise AuthError("The selected image pull Secret must contain a valid Docker registry config") from None


def has_credentials(config, registry):
    entry = config["auths"].get(registry)
    if not isinstance(entry, dict):
        return False
    try:
        if entry.get("auth"):
            user, password = base64.b64decode(entry["auth"], validate=True).decode().split(":", 1)
        else:
            user, password = entry.get("username"), entry.get("password")
        return user == "AWS" and isinstance(password, str) and bool(password.strip())
    except (TypeError, ValueError):
        return False


def reconcile(args):
    match = REGISTRY.fullmatch(args.repository)
    if not match or not NAME.fullmatch(args.namespace) or not NAME.fullmatch(args.secret):
        raise AuthError("Invalid Vision ECR registry, namespace or Secret name")
    registry, region = match.groups()
    kubectl = shutil.which(args.kubectl) or args.kubectl
    aws = shutil.which(args.aws) or args.aws
    command = [kubectl, "-n", args.namespace]
    raw = run(command + ["get", "secret", args.secret, "--ignore-not-found=true", "-o", "json"],
              "Cannot read the selected Vision image pull Secret")
    try:
        secret = json.loads(raw) if raw.strip() else None
        if secret is not None and not isinstance(secret, dict):
            raise ValueError()
    except ValueError:
        raise AuthError("Invalid Kubernetes image pull Secret response") from None
    if secret is None and args.renew_only:
        return {"changed": False, "managed": False}
    if secret is None and args.secret != "ecr-registry":
        raise AuthError("Provision the custom image pull Secret in the Vision namespace before installation")
    if secret is not None:
        metadata = secret.get("metadata", {})
        if not isinstance(metadata, dict) or not isinstance(metadata.get("annotations") or {}, dict):
            raise AuthError("Invalid image pull Secret metadata")
        if metadata.get("name") != args.secret or metadata.get("namespace") != args.namespace:
            raise AuthError("Image pull Secret identity does not match the requested namespace and name")
        if args.renew_only and (metadata.get("annotations") or {}).get(MANAGED_REGISTRY) != registry:
            return {"changed": False, "managed": False}
        config = read_config(secret)
        managed = args.secret == "ecr-registry" or (metadata.get("annotations") or {}).get(MANAGED_REGISTRY) == registry
        if not managed:
            if not has_credentials(config, registry):
                raise AuthError("The custom image pull Secret has no usable credentials for the selected ECR registry")
            return {"changed": False, "managed": False}
    else:
        config = {"auths": {}}

    token = run([aws, "ecr", "get-login-password", "--region", region,
                 "--profile", os.environ.get("AWS_PROFILE", "default")],
                "Cannot obtain an ECR login token; check target AWS credentials and ecr:GetAuthorizationToken permission").strip()
    if not token:
        raise AuthError("AWS returned an empty ECR login token")
    config["auths"][registry] = {"auth": base64.b64encode(("AWS:" + token).encode()).decode()}
    encoded = base64.b64encode(json.dumps(config).encode()).decode()
    annotations = {MANAGED_REGISTRY: registry}
    if secret is None:
        # Secret creation is atomic so a concurrent writer is never overwritten.
        namespace = {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": args.namespace}}
        run([kubectl, "apply", "-f", "-"], "Cannot prepare the Vision namespace", json.dumps(namespace))
        document = {"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
                    "metadata": {"name": args.secret, "namespace": args.namespace, "annotations": annotations},
                    "data": {".dockerconfigjson": encoded}}
        run([kubectl, "create", "-f", "-"],
            "Cannot create the Vision image pull Secret; if another writer created it, retry installation", json.dumps(document))
        changed = True
    else:
        changed = (secret["data"][".dockerconfigjson"] != encoded
                   or (secret["metadata"].get("annotations") or {}).get(MANAGED_REGISTRY) != registry)
        if changed:
            version = secret["metadata"].get("resourceVersion")
            if not version:
                raise AuthError("Image pull Secret has no resource version; refusing an unsafe update")
            patch = {"metadata": {"resourceVersion": version, "annotations": annotations},
                     "data": {".dockerconfigjson": encoded}}
            run(command + ["patch", "secret", args.secret, "--type=merge", "--patch-file=/dev/stdin"],
                "Cannot renew the Vision image pull Secret; check Kubernetes permissions or retry a concurrent update",
                json.dumps(patch))
    return {"changed": changed, "managed": True, "kubectl": kubectl, "aws": aws}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("namespace", "secret", "repository"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--kubectl", default="kubectl")
    parser.add_argument("--aws", default="aws")
    parser.add_argument("--renew-only", action="store_true")
    try:
        print(json.dumps(reconcile(parser.parse_args())))
        return 0
    except AuthError as error:
        print(f"Vision ECR authentication stopped: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
