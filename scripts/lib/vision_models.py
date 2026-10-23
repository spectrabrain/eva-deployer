#!/usr/bin/env python3
"""Vision 3.3 model cache and OCI publishing contract.

CLI: models | manifest | validate | download | publish; each accepts --models CSV,
and all but models accept --cache DIR (default $EVA_CACHE_ROOT/vision-models).
Selection defaults to EVA_VISION_MODELS, otherwise DEFAULT_MODELS. Empty selections,
duplicates and lowercase collisions are errors. Validation requires EXACT selection,
not just a subset: use the same selection for prepare, validate and publish.

manifest.json (UTF-8, deterministic JSON, LF terminated):
  {"schema_version":"v1", "source":"s3://spectrabrain-eva-vision/model_dir",
   "models":[{"name":"Owl-v2", "files":[
     {"path":"config.json", "sha256":"<64 lowercase hex>", "size":123}]}]}
File paths are relative to model_dir/<name>, POSIX, sorted and unique. Models
retain selection order. Only nonempty regular, singly linked files are allowed;
every directory must contain files. Unsafe components, known credential files,
.cache, *.lock and *.incomplete are rejected. The manifest and actual tree must
agree exactly, including file sizes and SHA256. No timestamps or source paths.

models prints one original-case name per line. manifest/validate/download/publish
print ONLY JSON {"models":[{"name":str,"files":int}],"model_count":int,
"file_count":int} on success. Errors are sanitized and go to stderr (exit 1).

Publish always pushes <registry>/<project>/<lowercase-name>:latest with artifact
type application/vnd.eva-vision.model.v1 and operand <OriginalCaseDirectory> from
model_dir. HARBOR_ARTIFACT_MANIFEST defaults to <cache>/harbor-artifacts.txt:
  <ref>|<OriginalModelName>|sha256:<SHA256 of exact manifest.json bytes>\n
This is a cache-manifest checksum, NOT an OCI digest. The report is replaced only
after ALL pushes succeed. It is the only optional extra file in the cache root.

Registry env precedence: REPOSITORY_* > HARBOR_*; project defaults to eva, username
to admin (HARBOR_ADMIN_USER/PASSWORD also accepted). PLAIN_HTTP defaults to true
(false for an explicit https:// endpoint); INSECURE defaults to false. Both accept
only true/false. LOCAL_HARBOR_YML defaults to ~/.local/share/eva-harbor/harbor/harbor.yml.
Its admin password is used only for admin and its matching hostname/transport/port.
PyYAML is needed only for that local fallback; all cache operations use stdlib.
ORAS_CMD defaults to $EVA_CACHE_ROOT/tools/oras (offline ORAS 1.3.3).
AWS_REGION defaults to ap-northeast-2; AWS env key pairs take precedence over
AWS_PROFILE (default default). Credentials are never written to this cache.
"""

from __future__ import annotations

import argparse
from contextlib import contextmanager
import fcntl
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tempfile
from urllib.parse import urlsplit


SOURCE = "s3://spectrabrain-eva-vision/model_dir"
ARTIFACT_TYPE = "application/vnd.eva-vision.model.v1"
DEFAULT_MODELS = (
    "Owl-v2", "OmDet", "LLMDet", "RT-DETRV2", "DINOv2Small",
    "E5EmbeddingSmall", "Qwen3VLEmbed2B",
)
MODEL_NAME = re.compile(r"[A-Za-z0-9]+(?:(?:__?|-+)[A-Za-z0-9]+)*\Z")
COMPONENT = re.compile(r"[A-Za-z0-9_.-]+\Z")
SECRET_NAMES = {
    ".aws", ".docker", ".ssh", ".git", ".cache", ".locks", ".env", ".netrc",
    ".npmrc", "credentials", "credentials.json", "credentials.yaml",
    "credentials.yml", "aws_key.ini", "harbor.yml", "harbor.yaml", "id_rsa",
    "id_ed25519", "id_ecdsa", "authorized_keys",
}
ROOT = Path(__file__).resolve().parents[2]


class ModelError(Exception):
    """A safe, credential-free error suitable for CLI output."""


def selection(value: str) -> list[str]:
    names = [name.strip() for name in value.split(",")]
    if not names or any(len(name) > 128 or not MODEL_NAME.fullmatch(name) for name in names):
        raise ModelError("Invalid model selection: use comma-separated directory names")
    if len({name.lower() for name in names}) != len(names):
        raise ModelError("Duplicate model name or lowercase repository collision")
    for name in names:
        safe_component(name)
    return names


def safe_component(name: str) -> None:
    lower = name.lower()
    if (not COMPONENT.fullmatch(name) or name in {".", ".."}
            or lower in SECRET_NAMES or lower.startswith(".env.")
            or lower.endswith((".lock", ".incomplete", ".pem", ".key", ".p12", ".pfx"))):
        raise ModelError("Unsafe model file or directory name")


def check_parents(path: Path) -> None:
    # Do not resolve away symlinks before checking them, including cache ancestors.
    if ".." in path.parts:
        raise ModelError("Parent traversal is not allowed in cache or report paths")
    for part in reversed((path, *path.parents)):
        if part.is_symlink():
            raise ModelError("Symlinks are not allowed in cache or report paths")


def directory(path: Path) -> None:
    if not stat.S_ISDIR(path.lstat().st_mode):
        raise ModelError("Expected a real model directory")


def regular(path: Path, *, nonempty: bool = True) -> None:
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or (nonempty and info.st_size <= 0):
        raise ModelError("Expected a safe regular file (model files must be nonempty)")


def digest(path: Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            hasher.update(chunk)
    return hasher.hexdigest()


def scan_files(path: Path, relative: str = "") -> list[dict]:
    directory(path)
    files = []
    for child in sorted(path.iterdir()):
        safe_component(child.name)
        key = f"{relative}/{child.name}" if relative else child.name
        if stat.S_ISDIR(child.lstat().st_mode):
            files.extend(scan_files(child, key))
        else:
            regular(child)  # Rejects symlinks, devices, FIFOs and hardlinks.
            files.append({"path": key, "sha256": digest(child), "size": child.stat().st_size})
    if not files:
        raise ModelError("Empty model directories are not valid")
    return sorted(files, key=lambda entry: entry["path"])


def scan_cache(cache: Path, names: list[str]) -> dict:
    check_parents(cache)
    directory(cache)
    if {entry.name for entry in cache.iterdir()} - {"model_dir", "manifest.json", "harbor-artifacts.txt"}:
        raise ModelError("Unexpected files in model cache root")
    for name in ("manifest.json", "harbor-artifacts.txt"):
        if (cache / name).exists() or (cache / name).is_symlink():
            regular(cache / name, nonempty=name == "manifest.json")
    model_dir = cache / "model_dir"
    directory(model_dir)
    if {entry.name for entry in model_dir.iterdir()} != set(names):
        raise ModelError("Cached model directories do not match the exact selection")
    return {"schema_version": "v1", "source": SOURCE,
            "models": [{"name": name, "files": scan_files(model_dir / name)} for name in names]}


def unique_keys(pairs: list[tuple]) -> dict:
    result = {}
    for key, value in pairs:
        if key in result:
            raise ModelError("Duplicate manifest JSON key")
        result[key] = value
    return result


def validate(cache: Path, names: list[str]) -> dict:
    check_parents(cache)
    regular(cache / "manifest.json")
    manifest = json.loads((cache / "manifest.json").read_text(), object_pairs_hook=unique_keys)
    actual = scan_cache(cache, names)
    # Compare deterministic JSON, not Python equality (True must not equal size 1).
    if json.dumps(manifest, sort_keys=True) != json.dumps(actual, sort_keys=True):
        raise ModelError("Manifest schema, selection or file checksum mismatch")
    return actual


def atomic_write(path: Path, content: str) -> None:
    check_parents(path)
    if path.exists():
        regular(path, nonempty=False)
    fd, temporary = tempfile.mkstemp(prefix=".vision-report-", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as stream:
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def generate(cache: Path, names: list[str]) -> dict:
    manifest = scan_cache(cache, names)
    atomic_write(cache / "manifest.json", json.dumps(manifest, indent=2) + "\n")
    return manifest


@contextmanager
def cache_lock(cache: Path):
    check_parents(cache)
    cache.parent.mkdir(parents=True, exist_ok=True)
    path = cache.parent / f".{cache.name}.lock"
    fd = os.open(path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
            raise ModelError("Unsafe cache lock")
        fcntl.flock(fd, fcntl.LOCK_EX)
        yield
    finally:
        os.close(fd)


def run(command: list[str], *, cwd: Path | None = None, password: str | None = None) -> None:
    # Third-party tools can echo credentials/absolute paths, even on failure.
    # Never forward their output or include CalledProcessError (argv) in errors.
    try:
        subprocess.run(command, cwd=cwd, input=password.encode() if password is not None else None,
                       stdin=subprocess.DEVNULL if password is None else None,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
    except (OSError, subprocess.CalledProcessError):
        raise ModelError("External model transfer command failed (output suppressed)") from None


def download(cache: Path, names: list[str]) -> dict:
    if cache.exists():
        directory(cache)
    aws = ["aws", "--region", os.environ.get("AWS_REGION") or "ap-northeast-2"]
    if not (os.environ.get("AWS_ACCESS_KEY_ID") and os.environ.get("AWS_SECRET_ACCESS_KEY")):
        aws += ["--profile", os.environ.get("AWS_PROFILE") or "default"]
    # A fresh tree ensures sync cannot bless stale files/models from older runs.
    with tempfile.TemporaryDirectory(prefix=".vision-models-stage-", dir=cache.parent) as temporary:
        stage = Path(temporary) / "vision-models"
        (stage / "model_dir").mkdir(parents=True)
        for name in names:
            target = stage / "model_dir" / name
            target.mkdir()
            run(aws + ["s3", "sync", f"{SOURCE}/{name}/", str(target),
                       "--exclude", ".cache/*", "--only-show-errors", "--no-progress"])
        manifest = generate(stage, names)
        # generate already scans/checksums the entire clean tree. Do not read
        # multi-GB weights a second time just to compare our own serialization.
        backup = Path(temporary) / "previous"
        try:
            if cache.exists():
                os.replace(cache, backup)
            os.replace(stage, cache)
        except BaseException:
            if backup.exists() and not cache.exists():
                os.replace(backup, cache)
            raise
    return manifest


def env(*names: str, default: str = "") -> str:
    return next((os.environ[name] for name in names if os.environ.get(name)), default)


def boolean(value: str) -> bool:
    if value not in {"true", "false"}:
        raise ModelError("Registry transport flags must be true or false")
    return value == "true"


def registry(value: str) -> str:
    if not value or value.startswith("-") or any(char.isspace() for char in value):
        raise ModelError("REPOSITORY_REGISTRY or HARBOR_REGISTRY is required")
    parsed = urlsplit(value if "://" in value else "//" + value)
    if (parsed.scheme not in {"", "http", "https"} or parsed.path not in {"", "/"}
            or parsed.query or parsed.fragment or parsed.username or parsed.password or not parsed.hostname):
        raise ModelError("Invalid registry endpoint (no credentials or paths allowed)")
    host = parsed.hostname
    if ":" in host:
        ipaddress.IPv6Address(host)
    elif not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.-]*", host):
        raise ModelError("Invalid registry hostname")
    if parsed.port is not None and parsed.port == 0:
        raise ModelError("Invalid registry port")
    return parsed.netloc.lower()


def local_password(endpoint: str, plain_http: bool, username: str) -> str:
    path = Path(env("LOCAL_HARBOR_YML", default=str(Path.home() / ".local/share/eva-harbor/harbor/harbor.yml")))
    if username != "admin" or not path.is_file():
        return ""
    try:
        import yaml  # Only the optional, installed local Harbor fallback needs YAML.

        config = yaml.safe_load(path.read_text())
        if not isinstance(config, dict) or not isinstance(config.get("hostname"), str):
            return ""
        transport = config.get("http" if plain_http else "https", {})
        if not isinstance(transport, dict):
            return ""
        port = transport.get("port", 32080 if plain_http else 443)
        expected = registry(f"{config['hostname']}:{port}")
        password = config.get("harbor_admin_password")
        if endpoint == expected and isinstance(password, str):
            return password
    except ImportError:
        raise ModelError("Local Harbor credential fallback requires PyYAML") from None
    except Exception:
        # YAML parser exceptions can contain the original secret-bearing line.
        raise ModelError("Unable to read local Harbor credential configuration") from None
    return ""


@contextmanager
def private_registry_config(cache_root: Path, cache: Path):
    # TMPDIR may point inside a cache/archive; deliberately do not trust it here.
    for parent in (Path("/tmp"), Path("/var/tmp")):
        if not any(parent.resolve().is_relative_to(root.resolve()) for root in (cache_root, cache)):
            with tempfile.TemporaryDirectory(prefix="eva-vision-oras-", dir=parent) as temporary:
                config = Path(temporary) / "registry.json"
                config.touch(mode=0o600)
                config.write_text('{"auths":{}}\n')
                yield config
            return
    raise ModelError("No credential temporary directory outside the cache is available")


def publish(cache: Path, names: list[str], cache_root: Path) -> dict:
    manifest = validate(cache, names)  # All models checked before login or any push.
    raw_registry = env("REPOSITORY_REGISTRY", "HARBOR_REGISTRY")
    endpoint = registry(raw_registry)
    project = env("REPOSITORY_PROJECT", "HARBOR_PROJECT", default="eva")
    if not re.fullmatch(r"[a-z0-9]+(?:[._-][a-z0-9]+)*", project):
        raise ModelError("Invalid repository project (one lowercase project required)")
    username = env("REPOSITORY_USERNAME", "HARBOR_USERNAME", "HARBOR_ADMIN_USER", default="admin")
    if username.startswith("-") or any(ord(char) < 32 for char in username):
        raise ModelError("Invalid repository username")
    plain_http = boolean(env("REPOSITORY_PLAIN_HTTP", "HARBOR_PLAIN_HTTP",
                             default="false" if raw_registry.startswith("https://") else "true"))
    insecure = boolean(env("REPOSITORY_INSECURE", "HARBOR_INSECURE", default="false"))
    password = env("REPOSITORY_PASSWORD", "HARBOR_PASSWORD", "HARBOR_ADMIN_PASSWORD")
    if not password:
        password = local_password(endpoint, plain_http, username)
    if not password:
        raise ModelError("Explicit repository credentials are required for this registry")
    oras = env("ORAS_CMD", default=str(cache_root / "tools/oras"))
    report = Path(env("HARBOR_ARTIFACT_MANIFEST", default=str(cache / "harbor-artifacts.txt"))).absolute()
    check_parents(report)
    if report.is_relative_to(cache) and report != cache / "harbor-artifacts.txt":
        raise ModelError("Artifact report must not overwrite model data or the cache manifest")
    if report.exists():
        regular(report, nonempty=False)
    directory(report.parent)
    checksum = digest(cache / "manifest.json")
    lines = []
    with private_registry_config(cache_root, cache) as config:
        flags = ["--registry-config", str(config)]
        if plain_http:
            flags.append("--plain-http")
        if insecure:
            flags.append("--insecure")
        run([oras, "login", *flags, endpoint, "--username", username, "--password-stdin"], password=password)
        for name in names:
            ref = f"{endpoint}/{project}/{name.lower()}:latest"
            # No manifest-exists skip: latest may exist with different contents.
            # ORAS handles content-addressed blob reuse itself.
            run([oras, "push", *flags, "--artifact-type", ARTIFACT_TYPE, ref, name], cwd=cache / "model_dir")
            lines.append(f"{ref}|{name}|sha256:{checksum}\n")
    atomic_write(report, "".join(lines))
    return manifest


def summary(manifest: dict) -> dict:
    models = [{"name": record["name"], "files": len(record["files"])} for record in manifest["models"]]
    return {"models": models, "model_count": len(models), "file_count": sum(record["files"] for record in models)}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest="command", required=True)
    cache_root = Path(env("EVA_CACHE_ROOT", default=str(ROOT / "out/cache"))).expanduser().absolute()
    for command in ("models", "manifest", "validate", "download", "publish"):
        child = commands.add_parser(command)
        child.add_argument("--models", default=os.environ.get("EVA_VISION_MODELS", ",".join(DEFAULT_MODELS)))
        if command != "models":
            child.add_argument("--cache", type=Path, default=cache_root / "vision-models")
    args = parser.parse_args()
    try:
        names = selection(args.models)
        if args.command == "models":
            print("\n".join(names))
            return 0
        cache = args.cache.expanduser().absolute()
        with cache_lock(cache):
            if args.command == "manifest":
                manifest = generate(cache, names)
            elif args.command == "validate":
                manifest = validate(cache, names)
            elif args.command == "download":
                manifest = download(cache, names)
            else:
                manifest = publish(cache, names, cache_root)
        print(json.dumps(summary(manifest), separators=(",", ":")))
        return 0
    except ModelError as error:
        print(f"[ERROR] {error}", file=sys.stderr)
    except (OSError, ValueError, RecursionError):
        print("[ERROR] Vision model operation failed: invalid data or filesystem error", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())