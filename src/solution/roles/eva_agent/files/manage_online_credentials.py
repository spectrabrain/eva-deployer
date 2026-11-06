#!/usr/bin/env python3
"""Validate Agent ECR access, retire old refresh Jobs and synchronize AWS credentials."""

import argparse
import base64
import configparser
import json
import os
import re
import subprocess
import sys


MANAGED = "eva.spectrabrain.com/agent-aws-credentials"
RELEASE = "eva-agent"
CRONJOB = "eva-agent-ecr-refresh"
HOOK = "eva-agent-ecr-refresh-hook"
NAME = re.compile(r"[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?\Z")


class CredentialError(Exception):
    pass


def run(command, description, payload=None, environment=None):
    try:
        result = subprocess.run(command, input=payload, text=True, capture_output=True,
                                env=environment, timeout=150)
    except (OSError, subprocess.TimeoutExpired):
        raise CredentialError(description) from None
    if result.returncode:
        # Never relay AWS/kubectl output: it may contain credentials or stdin.
        raise CredentialError(description)
    return result.stdout


def document(raw):
    try:
        value = json.loads(raw)
        if not isinstance(value, dict):
            raise ValueError()
        return value
    except (ValueError, TypeError):
        raise CredentialError("Invalid Kubernetes response") from None


def get(args, kind, name):
    raw = run([args.kubectl, "-n", args.namespace, "get", kind, name,
               "--ignore-not-found", "-o", "json"], f"Cannot read Agent {kind}")
    return document(raw) if raw.strip() else None


def identity(value, args, name):
    metadata = value.get("metadata", {})
    if (metadata.get("name") != name or metadata.get("namespace") != args.namespace
            or not metadata.get("uid") or not metadata.get("resourceVersion")):
        raise CredentialError("Agent resource identity is missing or changed; retry installation")
    return metadata


def owned(value, args, kind, name):
    metadata = identity(value, args, name)
    annotations = metadata.get("annotations") or {}
    if (annotations.get("meta.helm.sh/release-name") == getattr(args, "release", RELEASE)
            and annotations.get("meta.helm.sh/release-namespace") == args.namespace):
        return True
    # Helm does not attach release annotations to hook objects. The published
    # hook carries chart/release labels and these explicit lifecycle annotations.
    labels = metadata.get("labels") or {}
    if (kind == "Job" and name == getattr(args, "hook", HOOK)
            and labels.get("app.kubernetes.io/instance") == getattr(args, "release", RELEASE)
            and labels.get("app.kubernetes.io/managed-by") == "Helm"
            and labels.get("helm.sh/chart", "").startswith(getattr(args, "release", RELEASE) + "-")
            and set(annotations.get("helm.sh/hook", "").split(",")) == {"pre-install", "pre-upgrade"}
            and "before-hook-creation" in annotations.get("helm.sh/hook-delete-policy", "").split(",")):
        return True
    tracking = annotations.get("argocd.argoproj.io/tracking-id", "")
    application, separator, resource = tracking.partition(":")
    return (bool(separator) and application.endswith("-" + getattr(args, "release", RELEASE))
            and resource == f"batch/{kind}:{args.namespace}/{name}")


def credential_pair(data):
    if not data.get("AWS_ACCESS_KEY_ID", "").strip() or not data.get("AWS_SECRET_ACCESS_KEY", "").strip():
        raise CredentialError("Agent AWS credentials require an access key and secret key")
    return data


def from_workspace(raw, region):
    parser = configparser.ConfigParser(interpolation=None)
    try:
        parser.read_string(raw if raw.lstrip().startswith("[") else "[default]\n" + raw)
        section = parser["default"]
        credentials = {"AWS_ACCESS_KEY_ID": section.get("aws_access_key_id", "").strip(),
                       "AWS_SECRET_ACCESS_KEY": section.get("aws_secret_access_key", "").strip(),
                       "AWS_SESSION_TOKEN": section.get("aws_session_token", "").strip(),
                       "AWS_REGION": region, "AWS_DEFAULT_REGION": region}
    except (configparser.Error, KeyError, ValueError):
        raise CredentialError("Cannot read the selected Online workspace AWS credential file") from None
    return credential_pair(credentials)


def from_secret(secret, region):
    try:
        data = {key: base64.b64decode(value, validate=True).decode()
                for key, value in secret.get("data", {}).items()
                if key in {"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"}}
    except (ValueError, TypeError):
        raise CredentialError("Invalid external Agent AWS Secret; update its AWS credentials") from None
    data["AWS_REGION"] = data["AWS_DEFAULT_REGION"] = region
    return credential_pair(data)


def validate(args, credentials):
    # Use precisely the selected credential pair, never a host profile, stale
    # session token, role or instance credentials inherited by the installer.
    environment = {key: value for key, value in os.environ.items() if not key.startswith("AWS_")}
    environment.update(credentials)
    environment.update(AWS_CONFIG_FILE=os.devnull, AWS_SHARED_CREDENTIALS_FILE=os.devnull,
                       AWS_EC2_METADATA_DISABLED="true", AWS_MAX_ATTEMPTS="1")
    if not credentials.get("AWS_SESSION_TOKEN"):
        environment.pop("AWS_SESSION_TOKEN", None)
    token = run([args.aws, "ecr", "get-login-password", "--region", args.region,
                 "--cli-connect-timeout", "5", "--cli-read-timeout", "15"],
                "Agent ECR authentication failed. Check the selected AWS key, session token, "
                "ecr:GetAuthorizationToken permission and ECR connectivity. "
                "AWS Secret, refresh Jobs and workloads were left unchanged.", environment=environment)
    if not token.strip():
        raise CredentialError("AWS returned an empty ECR token; Agent credentials were left unchanged")

    repository = getattr(args, "repository", "")
    if repository:
        match = re.fullmatch(r"([0-9]{12})\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com(?:\.cn)?/(.+)", repository)
        if not match:
            raise CredentialError("Invalid selected ECR image repository")
        output = run([args.aws, "ecr", "batch-get-image", "--registry-id", match[1],
                      "--repository-name", match[3], "--image-ids", "imageTag=" + args.image_tag,
                      "--region", match[2], "--cli-connect-timeout", "5", "--cli-read-timeout", "15"],
                     "Selected ECR image is inaccessible; check the AWS key, repository pull policy and Release image tag. Workloads were left unchanged.",
                     environment=environment)
        if not document(output).get("images") or document(output).get("failures"):
            raise CredentialError("Selected Release image is missing or inaccessible; workloads were left unchanged")


def patch(args, kind, name, current, fields):
    metadata = identity(current, args, name)
    fields.setdefault("metadata", {}).update(resourceVersion=metadata["resourceVersion"], uid=metadata["uid"])
    run([args.kubectl, "-n", args.namespace, "patch", kind, name,
         "--type=merge", "--patch-file=/dev/stdin"],
        "Cannot update Agent resource; check permissions or retry a concurrent update", json.dumps(fields))


def resume(args, records):
    changed = False
    for record in records:
        if record.get("name") != getattr(args, "cronjob", CRONJOB) or not record.get("uid"):
            raise CredentialError("Invalid Agent ECR suspension record")
        current = get(args, "cronjob", getattr(args, "cronjob", CRONJOB))
        if current is None:
            continue
        metadata = identity(current, args, getattr(args, "cronjob", CRONJOB))
        # A replacement CronJob belongs to the new chart; preserve its setting.
        if metadata["uid"] != record["uid"]:
            continue
        if not owned(current, args, "CronJob", getattr(args, "cronjob", CRONJOB)):
            raise CredentialError("Cannot verify Agent ECR CronJob ownership before resuming")
        if current.get("spec", {}).get("suspend", False):
            patch(args, "cronjob", getattr(args, "cronjob", CRONJOB), current, {"spec": {"suspend": False}})
            changed = True
    return {"changed": changed}


def cleanup_jobs(args, cronjob):
    jobs = document(run([args.kubectl, "-n", args.namespace, "get", "jobs", "-o", "json"],
                        "Cannot inspect Agent ECR refresh Jobs"))
    selected = []
    for job in jobs.get("items", []):
        metadata = job.get("metadata", {})
        name = metadata.get("name", "")
        owners = metadata.get("ownerReferences", [])
        cronjob_owned = cronjob is not None and any(
            owner.get("kind") == "CronJob" and owner.get("name") == getattr(args, "cronjob", CRONJOB)
            and owner.get("uid") == cronjob["metadata"]["uid"] for owner in owners)
        if cronjob_owned or (name == getattr(args, "hook", HOOK) and owned(job, args, "Job", getattr(args, "hook", HOOK))):
            identity(job, args, name)
            selected.append(job)
        elif name == getattr(args, "hook", HOOK):
            raise CredentialError("Cannot verify Agent ECR hook ownership; no Jobs were deleted")
    for job in selected:
        metadata = job["metadata"]
        options = {"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground",
                   "preconditions": {"uid": metadata["uid"], "resourceVersion": metadata["resourceVersion"]}}
        run([args.kubectl, "delete", "--raw", f"/apis/batch/v1/namespaces/{args.namespace}/jobs/{metadata['name']}",
             "-f", "-"], "Cannot retire an old Agent ECR Job; retry installation", json.dumps(options))
        run([args.kubectl, "-n", args.namespace, "wait", "--for=delete", "job/" + metadata["name"],
             "--timeout=120s"], "Old Agent ECR Job Pods have not stopped; retry before changing credentials")
    return len(selected)


def prepare(args, raw):
    secret = get(args, "secret", args.secret)
    managed = args.secret == "aws-credentials" or secret is None
    if secret is not None:
        metadata = identity(secret, args, args.secret)
        if secret.get("type", "Opaque") != "Opaque":
            raise CredentialError("Agent AWS Secret must be Opaque; existing Secret was preserved")
        managed = managed or (metadata.get("annotations") or {}).get(getattr(args, "managed_annotation", MANAGED)) == getattr(args, "release", RELEASE)
    if secret is None and not raw.strip():
        raise CredentialError("Provision credentials.aws or the selected namespace AWS Secret before installation")
    credentials = (from_workspace(raw, args.region) if managed and (raw.strip() or not getattr(args, "reuse_existing", False))
                   else from_secret(secret, args.region))
    validate(args, credentials)
    if getattr(args, "action", "prepare") == "check":
        return {"changed": False, "managed": managed}
    cronjob = get(args, "cronjob", getattr(args, "cronjob", CRONJOB))
    if cronjob is not None and not owned(cronjob, args, "CronJob", getattr(args, "cronjob", CRONJOB)):
        raise CredentialError("Cannot verify Agent ECR CronJob ownership; existing resources were preserved")

    desired = {key: base64.b64encode(value.encode()).decode() if value else None
               for key, value in credentials.items()}
    # A long-lived key must not inherit an old temporary security token.
    desired["AWS_SECURITY_TOKEN"] = None
    annotations = {getattr(args, "managed_annotation", MANAGED): getattr(args, "release", RELEASE)}
    secret_changed = managed and (secret is None or any(secret.get("data", {}).get(key) != value
                                                       for key, value in desired.items())
                                 or (secret["metadata"].get("annotations") or {}).get(getattr(args, "managed_annotation", MANAGED)) != getattr(args, "release", RELEASE))
    if secret_changed and secret is not None and secret.get("immutable", False):
        raise CredentialError("Agent AWS Secret is immutable; update it before installation")

    suspended = []
    try:
        if cronjob is not None and not cronjob.get("spec", {}).get("suspend", False):
            patch(args, "cronjob", getattr(args, "cronjob", CRONJOB), cronjob, {"spec": {"suspend": True}})
            suspended.append({"name": getattr(args, "cronjob", CRONJOB), "uid": cronjob["metadata"]["uid"]})
        retired = cleanup_jobs(args, cronjob)
        if secret_changed:
            if secret is None:
                value = {"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
                         "metadata": {"name": args.secret, "namespace": args.namespace, "annotations": annotations},
                         "data": {key: value for key, value in desired.items() if value is not None}}
                run([args.kubectl, "create", "-f", "-"],
                    "Cannot create Agent AWS Secret; retry a concurrent creation", json.dumps(value))
            else:
                patch(args, "secret", args.secret, secret, {"metadata": {"annotations": annotations}, "data": desired})
    except CredentialError:
        # Preparation failed before handing the suspension record to Ansible.
        resume(args, suspended)
        raise
    return {"changed": bool(secret_changed or retired or suspended), "managed": managed,
            "retired_jobs": retired, "suspended": suspended}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["prepare", "resume", "check"])
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--secret", default="aws-credentials")
    parser.add_argument("--region", default="ap-northeast-2")
    parser.add_argument("--kubectl", default="kubectl")
    parser.add_argument("--aws", default="aws")
    parser.add_argument("--release", default=RELEASE)
    parser.add_argument("--cronjob", default=CRONJOB)
    parser.add_argument("--hook", default=HOOK)
    parser.add_argument("--managed-annotation", default=MANAGED)
    parser.add_argument("--reuse-existing", action="store_true")
    parser.add_argument("--repository", default="")
    parser.add_argument("--image-tag", default="")
    args = parser.parse_args()
    try:
        if not NAME.fullmatch(args.namespace) or not NAME.fullmatch(args.secret) or not NAME.fullmatch(args.region):
            raise CredentialError("Invalid Agent namespace, Secret name or AWS region")
        raw = sys.stdin.read()
        if args.action in {"prepare", "check"}:
            result = prepare(args, raw)
        else:
            records = json.loads(raw)
            if not isinstance(records, list):
                raise CredentialError("Invalid Agent ECR suspension record")
            result = resume(args, records)
        print(json.dumps(result))
        return 0
    except (CredentialError, ValueError, TypeError, KeyError) as error:
        message = str(error) if isinstance(error, CredentialError) else "Invalid Agent credential or resource input"
        print(message, file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
