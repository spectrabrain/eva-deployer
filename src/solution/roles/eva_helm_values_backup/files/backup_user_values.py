#!/usr/bin/env python3
"""Save a private, revision-pinned copy of an existing Helm release's input Values."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tempfile
from datetime import datetime, timezone


class BackupError(Exception):
    """A safe message that never contains Helm Values or subprocess diagnostics."""


def require(condition, message):
    if not condition:
        raise BackupError(message)


def segment(value):
    require(re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]*", value) is not None,
            "Invalid backup path identifier.")
    return value


def private_directory(path):
    """Do not follow symlinks, including in pre-existing ancestors."""
    require(path.is_absolute() and ".." not in path.parts and path != Path("/"),
            "Backup root must be an absolute non-root path without '..'.")
    for item in reversed([path, *path.parents]):
        if item.is_symlink():
            raise BackupError("Backup path must not contain symlinks.")
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.geteuid(),
            "Backup directory must be owned by the executing user.")
    path.chmod(0o700)


def helm(arguments):
    try:
        result = subprocess.run(["helm", *arguments], stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=120, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise BackupError("Helm backup query could not complete; installation stopped.") from None
    require(result.returncode == 0,
            "Helm backup query failed; installation stopped (diagnostics suppressed).")
    return result.stdout


def release_record(namespace, release):
    # --all here selects release STATES (including failed/pending), not computed Values.
    raw = helm(["list", "--namespace", namespace, "--all", "--filter",
                "^" + re.escape(release) + "$", "--max", "2", "--output", "json"])
    try:
        records = json.loads(raw)
    except (ValueError, UnicodeError):
        raise BackupError("Helm release lookup returned invalid JSON.") from None
    require(isinstance(records, list) and len(records) <= 1,
            "Helm release lookup returned an ambiguous result.")
    if not records:
        return None
    record = records[0]
    require(isinstance(record, dict) and record.get("name") == release
            and record.get("namespace") == namespace,
            "Helm release lookup returned an unexpected identity.")
    require(re.fullmatch(r"[1-9][0-9]*", str(record.get("revision", ""))) is not None,
            "Helm release lookup returned an invalid revision.")
    require(all(isinstance(record.get(key), str) and record[key]
                for key in ("chart", "status")),
            "Helm release lookup returned incomplete metadata.")
    return record


def backup(namespace, release, component, root, site):
    for value in (namespace, release, component, site):
        segment(value)
    before = release_record(namespace, release)
    if before is None:
        return {"status": "absent", "namespace": namespace, "release": release}

    root = Path(root)
    private_directory(root)
    parent = root
    for part in (site, namespace, release):
        parent = parent / part
        private_directory(parent)
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    directory = Path(tempfile.mkdtemp(prefix=timestamp + "-", dir=parent))
    values_path = directory / (component + "-user-values.yaml")
    partial_path = directory / (component + "-user-values.yaml.partial")
    revision = str(before["revision"])
    # Intentionally NO --all: preserve stored user overrides, not chart defaults.
    values = helm(["get", "values", release, "--namespace", namespace,
                   "--revision", revision, "--output", "yaml"])
    require(bool(values.strip()), "Helm returned empty Values output; installation stopped.")
    with partial_path.open("xb") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(values)
        stream.flush()
        os.fsync(stream.fileno())
    after = release_record(namespace, release)
    require(before == after,
            "Helm release changed during backup; installation stopped. Retry after other upgrades finish.")
    partial_path.rename(values_path)
    metadata = {
        "created_at": timestamp, "site": site, "namespace": namespace,
        "release": release, "component": component, "revision": revision,
        "chart": before["chart"], "app_version": before.get("app_version", ""),
        "release_status": before["status"], "values_kind": "user-supplied",
        "values_file": values_path.name,
        "sha256": hashlib.sha256(values).hexdigest(),
        "sensitive": True,
    }
    metadata_partial = directory / "metadata.json.partial"
    with metadata_partial.open("x", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        json.dump(metadata, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    metadata_partial.rename(directory / "metadata.json")
    descriptor = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    return {"status": "saved", "namespace": namespace, "release": release,
            "revision": revision, "backup_directory": str(directory),
            "values_file": str(values_path)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("namespace", "release", "component", "root", "site"):
        parser.add_argument("--" + name, required=True)
    options = parser.parse_args()
    # Applies before even temporary files/directories are created.
    os.umask(0o077)
    try:
        result = backup(**vars(options))
    except BackupError as error:
        print(str(error), file=sys.stderr)
        return 1
    except Exception:
        # Paths, command output and Values can contain credentials. Never print a traceback.
        print("Helm Values backup failed; installation stopped (details suppressed).", file=sys.stderr)
        return 1
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    sys.exit(main())
