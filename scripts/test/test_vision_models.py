#!/usr/bin/env python3
"""Offline contract tests: tiny local models and fake AWS/ORAS, no network."""

import hashlib
import base64
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
HELPER = ROOT / "scripts/lib/vision_models.py"
DOWNLOAD = ROOT / "scripts/download/download_vision_models.sh"
PUBLISH = ROOT / "scripts/publish/push_vision_models_to_harbor.sh"
DEFAULTS = ["Owl-v2", "OmDet", "LLMDet", "RT-DETRV2", "DARTF", "DARTF-light",
            "DINOv2Small", "E5EmbeddingSmall", "Qwen3VLEmbed2B"]
SOURCE = "s3://spectrabrain-eva-vision/model_dir"
SECRET = "private-password-DO-NOT-PRINT"
ACCESS_KEY = "private-aws-access-DO-NOT-PRINT"
SECRET_KEY = "private-aws-secret-DO-NOT-PRINT"


# The fixtures record argv (never passwords) and intentionally print a secret
# on tool failure, so the tests prove external error output is not forwarded.
FAKE_AWS = r'''
import json
import os
from pathlib import Path
import shutil
import sys

args = sys.argv[1:]
with open(os.environ["FAKE_AWS_LOG"], "a") as stream:
    stream.write(json.dumps(args) + "\n")
assert args[args.index("s3") + 1] == "sync", "Only S3 reads are allowed"
source, target = args[args.index("sync") + 1:args.index("sync") + 3]
assert source.startswith("s3://spectrabrain-eva-vision/model_dir/")
name = source.rstrip("/").rsplit("/", 1)[1]
assert "--exclude" in args and args[args.index("--exclude") + 1] == ".cache/*"
assert "--profile" not in args if os.environ.get("AWS_ACCESS_KEY_ID") and os.environ.get("AWS_SECRET_ACCESS_KEY") else "--profile" in args
assert os.environ.get("AWS_SECRET_ACCESS_KEY", "") not in args
assert "--delete" not in args
if name == os.environ.get("FAKE_EMPTY"):
    raise SystemExit(0)
destination = Path(target)
shutil.copytree(Path(os.environ["FAKE_MODELS"]) / name, destination,
                symlinks=True, dirs_exist_ok=True, ignore=shutil.ignore_patterns(".cache"))
if name == os.environ.get("FAKE_FAIL"):
    print(os.environ.get("REPOSITORY_PASSWORD", "private-password-DO-NOT-PRINT"))
    print(os.environ.get("AWS_SECRET_ACCESS_KEY", "private-aws-secret-DO-NOT-PRINT"), file=sys.stderr)
    raise SystemExit(7)
'''

FAKE_ORAS = r'''
import json
import os
from pathlib import Path
import stat
import sys

args = sys.argv[1:]
config = Path(args[args.index("--registry-config") + 1])
root = Path(os.environ["EVA_CACHE_ROOT"])
assert not config.is_relative_to(root)
assert not config.is_relative_to(root / "vision-models")
assert stat.S_IMODE(config.parent.stat().st_mode) == 0o700
assert stat.S_IMODE(config.stat().st_mode) == 0o600
if args[0] == "login":
    assert "--password-stdin" in args
    password = sys.stdin.read()
    assert password == os.environ["FAKE_EXPECTED_PASSWORD"]
    assert password not in args
    config.write_text(json.dumps({"auths": {"private": password}}))
elif args[0] == "push":
    assert args[args.index("--artifact-type") + 1] == "application/vnd.eva-vision.model.v1"
    assert Path.cwd() == root / "vision-models/model_dir"
    ref, name = args[-2:]
    assert not Path(name).is_absolute() and "/" not in name and ":" not in name
    assert (Path.cwd() / name).is_dir()
    assert ref.endswith("/" + name.lower() + ":latest")
    auths = json.loads(config.read_text())["auths"]
    if os.environ.get("REPOSITORY_USE_DOCKER_AUTH") == "true":
        import base64
        assert set(auths) == {"harbor.test:32080"}
        assert base64.b64decode(auths["harbor.test:32080"]["auth"]).decode() == "pull-user:" + os.environ["FAKE_EXPECTED_PASSWORD"]
    else:
        assert auths["private"] == os.environ["FAKE_EXPECTED_PASSWORD"]
else:
    raise AssertionError("No latest existence probes or project creation allowed")
with open(os.environ["FAKE_ORAS_LOG"], "a") as stream:
    stream.write(json.dumps({"args": args, "cwd": str(Path.cwd()), "config": str(config)}) + "\n")
if args[0] == "push" and args[-1] == os.environ.get("FAKE_PUSH_FAIL"):
    print(os.environ["FAKE_EXPECTED_PASSWORD"])
    print(os.environ["FAKE_EXPECTED_PASSWORD"], file=sys.stderr)
    raise SystemExit(9)
'''


class VisionModelsTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="vision-model-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.cache_root = self.root / "cache"
        self.cache_root.mkdir()
        self.cache = self.cache_root / "vision-models"
        self.models = self.root / "source"
        self.models.mkdir()
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.aws_log = self.root / "aws.jsonl"
        self.oras_log = self.root / "oras.jsonl"
        # Use this test's interpreter in the fakes and shell entry points too.
        self.write_fake(self.bin / "aws", FAKE_AWS)
        self.write_fake(self.cache_root / "tools/oras", FAKE_ORAS)
        self.env = {key: value for key, value in os.environ.items()
                    if not key.startswith(("AWS_", "HARBOR_", "REPOSITORY_", "EVA_VISION_", "FAKE_"))
                    and key not in {"ORAS_CMD", "LOCAL_HARBOR_YML", "PYTHON", "EVA_CACHE_ROOT"}}
        self.env.update({
            "PATH": str(self.bin) + os.pathsep + self.env.get("PATH", ""),
            "PYTHON": sys.executable, "PYTHONDONTWRITEBYTECODE": "1",
            "EVA_CACHE_ROOT": str(self.cache_root), "FAKE_MODELS": str(self.models),
            "FAKE_AWS_LOG": str(self.aws_log), "FAKE_ORAS_LOG": str(self.oras_log),
            "AWS_ACCESS_KEY_ID": ACCESS_KEY, "AWS_SECRET_ACCESS_KEY": SECRET_KEY,
            "AWS_SESSION_TOKEN": "private-session-token", "AWS_PROFILE": "unused-profile",
            "REPOSITORY_REGISTRY": "harbor.test:32080", "REPOSITORY_PROJECT": "eva",
            "REPOSITORY_USERNAME": "admin", "REPOSITORY_PASSWORD": SECRET,
            "FAKE_EXPECTED_PASSWORD": SECRET, "LOCAL_HARBOR_YML": str(self.root / "absent.yml"),
            "TMPDIR": str(self.cache_root),
        })
        for name in DEFAULTS:
            folder = self.models / name
            (folder / "nested").mkdir(parents=True)
            (folder / "config.json").write_text('{"model":"test"}\n')
            (folder / "nested/weights.bin").write_bytes(b"tiny-local-weights")
            (folder / ".cache").mkdir()
            (folder / ".cache/unwanted.lock").write_text("excluded HF cache")

    def write_fake(self, path, code):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(f"#!{sys.executable}\n" + code)
        path.chmod(0o755)

    def call(self, command, *, ok=True, updates=None, args=()):
        environment = self.env.copy()
        for key, value in (updates or {}).items():
            if value is None:
                environment.pop(key, None)
            else:
                environment[key] = value
        if command == "download":
            argv = ["bash", str(DOWNLOAD)]
        elif command == "publish":
            argv = ["bash", str(PUBLISH)]
        else:
            argv = [sys.executable, "-B", str(HELPER), command]
        result = subprocess.run(argv + list(args), env=environment, cwd=self.root,
                                capture_output=True, text=True, timeout=15)
        if ok:
            self.assertEqual(result.returncode, 0, result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout)
        for secret in (SECRET, ACCESS_KEY, SECRET_KEY, "private-session-token"):
            self.assertNotIn(secret, result.stdout + result.stderr)
        self.assertNotIn(str(self.root), result.stdout + result.stderr)
        self.assertNotIn("Traceback", result.stderr)
        return result

    def prepare(self, names="Owl-v2,OmDet"):
        self.env["EVA_VISION_MODELS"] = names
        return self.call("download")

    @staticmethod
    def calls(path):
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def snapshot(self):
        return {str(path.relative_to(self.cache)): path.read_bytes()
                for path in self.cache.rglob("*") if path.is_file()}

    def assert_clean_credentials(self):
        for path in self.cache.rglob("*"):
            if path.is_file():
                for secret in (SECRET, ACCESS_KEY, SECRET_KEY, "private-session-token"):
                    self.assertNotIn(secret.encode(), path.read_bytes())
        for entry in self.calls(self.oras_log):
            self.assertFalse(Path(entry["config"]).exists())

    def test_default_models_and_manifest_contract(self):
        result = self.call("download")
        self.assertEqual(json.loads(result.stdout)["model_count"], len(DEFAULTS))
        self.assertEqual(self.call("models").stdout.splitlines(), DEFAULTS)
        manifest_bytes = (self.cache / "manifest.json").read_bytes()
        manifest = json.loads(manifest_bytes)
        self.assertEqual(set(manifest), {"schema_version", "source", "models"})
        self.assertEqual(manifest["schema_version"], "v1")
        self.assertEqual(manifest["source"], SOURCE)
        self.assertEqual([entry["name"] for entry in manifest["models"]], DEFAULTS)
        for entry in manifest["models"]:
            self.assertEqual(set(entry), {"name", "files"})
            self.assertEqual(len(entry["files"]), 2)
            for record in entry["files"]:
                self.assertEqual(set(record), {"path", "size", "sha256"})
                path = self.cache / "model_dir" / entry["name"] / record["path"]
                self.assertEqual(record["size"], path.stat().st_size)
                self.assertEqual(record["sha256"], hashlib.sha256(path.read_bytes()).hexdigest())
                self.assertFalse(Path(record["path"]).is_absolute())
        self.assertNotIn(str(self.root).encode(), manifest_bytes)
        self.assertFalse(list(self.cache.rglob(".cache")))
        self.assertEqual(json.loads(self.call("validate").stdout)["file_count"], 2 * len(DEFAULTS))
        self.call("manifest")
        self.assertEqual((self.cache / "manifest.json").read_bytes(), manifest_bytes)
        self.assertEqual(len(self.calls(self.aws_log)), len(DEFAULTS))
        self.assert_clean_credentials()

    def test_selection_cli_optional_dartf_and_profile_region(self):
        self.env["EVA_VISION_MODELS"] = "Owl-v2"
        self.call("download", args=("--models", " OmDet, DARTF "), updates={
            "AWS_ACCESS_KEY_ID": None, "AWS_SECRET_ACCESS_KEY": None,
            "AWS_PROFILE": "read-only", "AWS_REGION": "ap-northeast-2",
        })
        calls = self.calls(self.aws_log)
        self.assertEqual(len(calls), 2)
        for args, name in zip(calls, ["OmDet", "DARTF"]):
            self.assertEqual(args[args.index("--profile") + 1], "read-only")
            self.assertEqual(args[args.index("--region") + 1], "ap-northeast-2")
            self.assertEqual(args[args.index("sync") + 1], f"{SOURCE}/{name}/")
        self.call("validate", args=("--models", "OmDet,DARTF"))
        self.call("validate", ok=False)  # Stale/default selection cannot be blessed.

    def test_invalid_selections_are_rejected_before_aws(self):
        for value in ("", " ", "../Owl-v2", "Owl/v2", "--help", "Owl-v2,owl-v2",
                      "OmDet,OmDet", "OmDet,", "OmDet,,DARTF", "bad|name", "Owl-v2\nX",
                      "credentials", "id_rsa", "Model_", "Model-", "Model___X", "Model-_X", "x" * 129):
            with self.subTest(value=value):
                self.call("download", ok=False, updates={"EVA_VISION_MODELS": value})
        self.assertFalse(self.aws_log.exists())
        self.assertFalse(self.cache.exists())

    def test_failed_or_empty_sync_preserves_previous_complete_cache(self):
        self.prepare()
        before = self.snapshot()
        (self.models / "Owl-v2/config.json").write_text("changed source")
        for failure in ({"FAKE_FAIL": "OmDet"}, {"FAKE_EMPTY": "OmDet"}):
            with self.subTest(failure=failure):
                self.call("download", ok=False, updates=failure)
                self.assertEqual(self.snapshot(), before)
                self.call("validate")
                self.assertFalse(list(self.cache_root.glob(".vision-models-stage-*")))
        self.assert_clean_credentials()

    def test_failed_initial_download_never_leaves_partial_cache(self):
        self.call("download", ok=False, updates={"FAKE_FAIL": "OmDet"})
        self.assertFalse(self.cache.exists())
        self.assertFalse(list(self.cache_root.glob(".vision-models-stage-*")))

    def test_successful_refresh_drops_stale_models_files_and_report(self):
        self.prepare()
        self.call("publish")
        (self.models / "Owl-v2/nested/weights.bin").unlink()
        (self.models / "Owl-v2/nested").rmdir()
        self.call("download", updates={"EVA_VISION_MODELS": "Owl-v2"})
        self.assertEqual({path.name for path in (self.cache / "model_dir").iterdir()}, {"Owl-v2"})
        self.assertFalse((self.cache / "model_dir/Owl-v2/nested/weights.bin").exists())
        self.assertFalse((self.cache / "harbor-artifacts.txt").exists())
        self.call("validate", ok=False)  # Prior two-model selection is no longer valid.

    def test_download_rejects_symlink_and_partial_source_without_replacing_cache(self):
        self.prepare()
        before = self.snapshot()
        unsafe = self.models / "OmDet/unsafe"
        for mode in ("symlink", "lock", "incomplete", "credentials", "empty"):
            with self.subTest(mode=mode):
                if mode == "symlink":
                    unsafe.symlink_to(self.models / "Owl-v2/config.json")
                    target = unsafe
                elif mode == "empty":
                    target = unsafe
                    target.mkdir()
                else:
                    target = unsafe.with_name({"lock": "weights.lock", "incomplete": "weights.incomplete",
                                               "credentials": "credentials.json"}[mode])
                    target.write_text("untrusted data")
                self.call("download", ok=False)
                self.assertEqual(self.snapshot(), before)
                if target.is_dir() and not target.is_symlink():
                    target.rmdir()
                else:
                    target.unlink()

    def test_validate_and_publish_reject_empty_corrupt_and_linked_trees_before_login(self):
        self.prepare("Owl-v2")
        pristine = self.snapshot()
        weight = self.cache / "model_dir/Owl-v2/nested/weights.bin"
        for mode in ("corrupt", "zero", "missing", "symlink", "directory-link", "hardlink",
                     "fifo", "lock", "incomplete", "unsafe", "secret", "extra", "empty-directory"):
            with self.subTest(mode=mode):
                shutil.rmtree(self.cache)
                for name, content in pristine.items():
                    path = self.cache / name
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_bytes(content)
                if mode == "corrupt":
                    weight.write_bytes(b"corrupted")
                elif mode == "zero":
                    weight.write_bytes(b"")
                elif mode == "missing":
                    weight.unlink()
                elif mode in {"symlink", "hardlink", "fifo"}:
                    weight.unlink()
                    if mode == "symlink":
                        weight.symlink_to(self.models / "Owl-v2/nested/weights.bin")
                    elif mode == "hardlink":
                        os.link(self.models / "Owl-v2/nested/weights.bin", weight)
                    else:
                        os.mkfifo(weight)
                elif mode == "directory-link":
                    shutil.rmtree(weight.parent)
                    weight.parent.symlink_to(self.models / "Owl-v2/nested", target_is_directory=True)
                elif mode == "empty-directory":
                    (weight.parent / "empty").mkdir()
                elif mode == "extra":
                    (self.cache / "model_dir/StaleModel").mkdir()
                else:
                    filename = {"lock": "weights.lock", "incomplete": "weights.incomplete",
                                "unsafe": "bad|name", "secret": ".env"}[mode]
                    (weight.parent / filename).write_text("bad")
                self.call("validate", ok=False)
                self.call("publish", ok=False)
                self.assertFalse(self.oras_log.exists())

    def test_manifest_schema_tampering_and_unlisted_files_fail(self):
        self.prepare("Owl-v2")
        path = self.cache / "manifest.json"
        original = path.read_text()
        for mode in ("source", "version", "path", "duplicate", "bool-size", "extra-field", "bad-json"):
            with self.subTest(mode=mode):
                data = json.loads(original)
                if mode == "source":
                    data["source"] = "s3://other/model_dir"
                elif mode == "version":
                    data["schema_version"] = 1
                elif mode == "path":
                    data["models"][0]["files"][0]["path"] = "../outside"
                elif mode == "bool-size":
                    data["models"][0]["files"][0]["size"] = True
                elif mode == "extra-field":
                    data["password"] = SECRET
                if mode == "duplicate":
                    path.write_text(original.replace('"schema_version": "v1",', '"schema_version": "bad", "schema_version": "v1",'))
                elif mode == "bad-json":
                    path.write_text('{"password":"' + SECRET + '"')
                else:
                    path.write_text(json.dumps(data))
                self.call("validate", ok=False)
        path.write_text(original)
        (self.cache / "model_dir/Owl-v2/new.bin").write_bytes(b"unlisted")
        self.call("validate", ok=False)

    def test_cache_and_manifest_symlink_ancestors_are_rejected(self):
        self.prepare("Owl-v2")
        actual = self.root / "actual"
        self.cache.rename(actual)
        self.cache.symlink_to(actual, target_is_directory=True)
        self.call("validate", ok=False)
        self.call("download", ok=False)
        self.cache.unlink()
        actual.rename(self.cache)
        manifest = self.cache / "manifest.json"
        actual_manifest = self.root / "actual.json"
        manifest.rename(actual_manifest)
        manifest.symlink_to(actual_manifest)
        self.call("validate", ok=False)

    def test_publish_original_case_directory_latest_and_safe_atomic_report(self):
        self.prepare()
        self.call("publish")
        self.call("publish")  # Existing latest MUST be updated, not skipped.
        entries = self.calls(self.oras_log)
        self.assertEqual([entry["args"][0] for entry in entries], ["login", "push", "push"] * 2)
        pushes = [entry for entry in entries if entry["args"][0] == "push"]
        self.assertEqual([entry["args"][-1] for entry in pushes], ["Owl-v2", "OmDet"] * 2)
        for entry in entries:
            self.assertIn("--plain-http", entry["args"])
            self.assertNotIn("--insecure", entry["args"])
            self.assertNotIn(SECRET, entry["args"])
        checksum = hashlib.sha256((self.cache / "manifest.json").read_bytes()).hexdigest()
        report = (self.cache / "harbor-artifacts.txt").read_text()
        self.assertEqual(report, "".join(f"harbor.test:32080/eva/{name.lower()}:latest|{name}|sha256:{checksum}\n"
                                        for name in ["Owl-v2", "OmDet"]))
        self.assertNotIn(str(self.root), report)
        self.call("validate")
        self.assert_clean_credentials()

    def test_harbor_aliases_tls_insecure_and_external_report(self):
        self.prepare("Owl-v2")
        report = self.root / "artifacts.txt"
        report.touch()  # Callers may use mktemp for their explicit output path.
        self.call("publish", updates={
            "REPOSITORY_REGISTRY": None, "REPOSITORY_PROJECT": None,
            "REPOSITORY_USERNAME": None, "REPOSITORY_PASSWORD": None,
            "HARBOR_REGISTRY": "https://registry.test:443/", "HARBOR_PROJECT": "same-project",
            "HARBOR_USERNAME": "robot$eva", "HARBOR_PASSWORD": SECRET,
            "HARBOR_INSECURE": "true", "HARBOR_ARTIFACT_MANIFEST": str(report),
        })
        self.assertTrue(report.read_text().startswith("registry.test:443/same-project/owl-v2:latest|Owl-v2|"))
        for entry in self.calls(self.oras_log):
            self.assertIn("--insecure", entry["args"])
            self.assertNotIn("--plain-http", entry["args"])
        self.assert_clean_credentials()

    def test_local_harbor_fallback_only_for_matching_endpoint(self):
        self.prepare("Owl-v2")
        harbor = self.root / "harbor.yml"
        harbor.write_text('hostname: harbor.test\nhttp:\n  port: 32080\nharbor_admin_password: "' + SECRET + '"\n')
        self.env.pop("REPOSITORY_PASSWORD")
        self.env["LOCAL_HARBOR_YML"] = str(harbor)
        self.call("publish", ok=False, updates={"REPOSITORY_REGISTRY": "unrelated.test:32080"})
        self.call("publish", ok=False, updates={"REPOSITORY_REGISTRY": "harbor.test:443"})
        self.assertFalse(self.oras_log.exists())
        self.call("publish")
        self.assert_clean_credentials()

    def test_remote_external_harbor_reuses_docker_auth_and_ignores_stale_compose(self):
        self.prepare("Owl-v2")
        docker = self.root / "docker"
        docker.mkdir()
        config = {"auths": {"harbor.test:32080": {"auth": base64.b64encode(("pull-user:" + SECRET).encode()).decode()},
                            "other.test": {"auth": "unrelated-credential"}}}
        (docker / "config.json").write_text(json.dumps(config))
        before = (docker / "config.json").read_bytes()
        harbor = self.root / "old-harbor.yml"
        harbor.write_text('hostname: harbor.test\nhttp:\n  port: 32080\nharbor_admin_password: stale-password\n')
        self.call("publish", updates={"REPOSITORY_PASSWORD": None, "REPOSITORY_USE_DOCKER_AUTH": "true",
                                      "DOCKER_CONFIG": str(docker), "LOCAL_HARBOR_YML": str(harbor)})
        self.assertEqual(before, (docker / "config.json").read_bytes())
        self.assertTrue(all(entry["args"][0] == "push" for entry in self.calls(self.oras_log)))
        self.assert_clean_credentials()

    def test_failed_push_preserves_report_and_removes_private_config(self):
        self.prepare()
        self.call("publish")
        before = (self.cache / "harbor-artifacts.txt").read_bytes()
        self.call("publish", ok=False, updates={"FAKE_PUSH_FAIL": "OmDet"})
        self.assertEqual((self.cache / "harbor-artifacts.txt").read_bytes(), before)
        self.assert_clean_credentials()

    def test_invalid_registry_project_flags_and_report_do_not_login(self):
        self.prepare("Owl-v2")
        for updates in (
            {"REPOSITORY_REGISTRY": "https://user:password@harbor.test"},
            {"REPOSITORY_REGISTRY": "harbor.test/path"},
            {"REPOSITORY_REGISTRY": "harbor.test?token=secret"},
            {"REPOSITORY_REGISTRY": "harbor.test\n"},
            {"REPOSITORY_PROJECT": "eva/other-project"},
            {"REPOSITORY_PROJECT": "EVA"}, {"REPOSITORY_PLAIN_HTTP": "yes"},
            {"HARBOR_ARTIFACT_MANIFEST": str(self.cache / "manifest.json")},
            {"HARBOR_ARTIFACT_MANIFEST": str(self.cache / "model_dir/Owl-v2/config.json")},
            {"HARBOR_ARTIFACT_MANIFEST": str(self.cache / "model_dir/../outside.txt")},
        ):
            with self.subTest(updates=updates):
                self.call("publish", ok=False, updates=updates)
        self.assertFalse(self.oras_log.exists())


if __name__ == "__main__":
    unittest.main()
