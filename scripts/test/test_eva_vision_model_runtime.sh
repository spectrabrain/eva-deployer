#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PYTHON_BIN="${PYTHON_BIN:-$REPO_ROOT/.venv/bin/python}"
export PYTHONDONTWRITEBYTECODE=1

"$PYTHON_BIN" - "$REPO_ROOT" <<'PY'
from pathlib import Path
import copy
import base64
import io
import json
import os
import re
import shlex
import stat
import subprocess
import sys
import tarfile
import tempfile

import yaml
from ansible.plugins.test.core import version_compare
from jinja2 import Environment, StrictUndefined

root = Path(sys.argv[1])
role = root / "src/solution/roles/eva_vision"
ansible = os.environ.get("ANSIBLE_PLAYBOOK", str(root / ".venv/bin/ansible-playbook"))
environment = Environment(undefined=StrictUndefined)
environment.filters.update(bool=bool, to_json=json.dumps,
                           ternary=lambda value, yes, no: yes if value else no)
environment.tests["version"] = version_compare
values_template = environment.from_string((root / "src/solution/values/vision-k3s-override.yaml.j2").read_text())
renderer_template = environment.from_string((role / "templates/post-renderer/post-renderer.py.j2").read_text())

defaults = dict(eva_vision_image_repository="registry.example/eva/eva-vision",
                eva_vision_deploy_version="3.3.0",
                eva_vision_image_tag="3.3.0",
                eva_vision_chart_version="3.3.0", repository_registry="harbor.example:32080",
                repository_project="eva", eva_vision_gpu_count=1,
                eva_vision_gpu_resource_name="nvidia.com/gpu",
                eva_vision_storage_class="local-path", eva_vision_pvc_size="20Gi")

# Image tags are independent of Chart and deployment workdir versions.
for mode in ("online_repository", "cloud_repository", "remote_repository", "local_repository"):
    variables = {**defaults, "eva_vision_deploy_version": "3.2.0", "repository_mode": mode}
    assert yaml.safe_load(values_template.render(**variables))["image"]["tag"] == "3.3.0"
    variables["eva_vision_image_tag"] = "3.3.0rc-test"
    assert yaml.safe_load(values_template.render(**variables))["image"]["tag"] == "3.3.0rc-test"
    del variables["eva_vision_image_tag"]
    assert yaml.safe_load(values_template.render(**variables))["image"]["tag"] == "3.3.0"

mock_kubectl = r'''#!/usr/bin/env python3
import json, os, pathlib, sys
import yaml
state = pathlib.Path(os.environ["FIXTURE_ROOT"])
args = sys.argv[1:]
with (state / "calls").open("a") as output:
    output.write(json.dumps(args) + "\n")
if args[:2] == ["get", "secret"]:
    expected = os.environ.get("EXPECTED_SECRET", "")
    assert args[2] == expected, (args[2], expected)
    fixture = state / "existing-secret.json"
    if fixture.exists():
        print(fixture.read_text())
elif args[:2] == ["create", "namespace"]:
    print("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: eva-vision")
elif args[:2] == ["apply", "-f"]:
    assert args == ["apply", "-f", "-"], args
    assert yaml.safe_load(sys.stdin.read())["kind"] == "Namespace"
    print("namespace/eva-vision created")
elif args == ["create", "-f", "-"]:
    data = json.load(sys.stdin)
    assert data["apiVersion"] == "v1" and data["kind"] == "Secret" and data["type"] == "Opaque"
    assert data["metadata"] == {"name": os.environ["EXPECTED_SECRET"], "namespace": "eva-vision"}
    if os.environ["EXPECTED_MODEL_SOURCE"] == "harbor":
        config = yaml.safe_load((state / "target/etc/rancher/k3s/registries.yaml").read_text())
        auth = config["configs"][os.environ["EXPECTED_REGISTRY"]]["auth"]
        assert data["stringData"] == {"HARBOR_USERNAME": auth["username"], "HARBOR_PASSWORD": auth["password"]}
    else:
        assert data["stringData"] == {"AWS_ACCESS_KEY_ID": "fixture-access", "AWS_SECRET_ACCESS_KEY": "fixture-secret"}
    assert not (state / "existing-secret.json").exists(), "Existing credentials must not be written"
    if os.environ.get("CREATE_RACE") == "1":
        (state / "existing-secret.json").write_text('{"data":{"HARBOR_USERNAME":"cm9ib3Q=","HARBOR_PASSWORD":"cmFjZQ=="}}')
        raise SystemExit("Secret already exists; preserve concurrent operator credentials")
    (state / "created").touch()
    print("secret/model-secret created")
elif args == ["get", "deployment", "eva-vision", "-n", "eva-vision", "-o", "json"]:
    print(json.dumps({"apiVersion": "apps/v1", "kind": "Deployment",
                      "metadata": {"name": "eva-vision", "namespace": "eva-vision"}}))
elif args == ["scale", "deployment", "eva-vision", "-n", "eva-vision", "--replicas=0"]:
    (state / "scaled").touch()
    print("deployment.apps/eva-vision scaled")
else:
    raise SystemExit("Unexpected kubectl invocation")
'''

mock_oras = r'''
import json, os, pathlib, sys
state = pathlib.Path(os.environ["FIXTURE_ROOT"])
args = sys.argv[1:]
assert args[0] in {"login", "push"}, args
assert "--insecure" in args and "--plain-http" not in args, args
config = pathlib.Path(args[args.index("--registry-config") + 1])
assert config.is_file() and config.stat().st_mode & 0o777 == 0o600
if args[0] == "login":
    assert "override.example:8443" in args
    assert args[args.index("--username") + 1] == os.environ["EXPECTED_PUBLISH_USER"]
    assert sys.stdin.read() == os.environ["EXPECTED_PUBLISH_PASSWORD"]
else:
    assert args[args.index("--artifact-type") + 1] == "application/vnd.eva-vision.model.v1"
    assert args[-2] == "override.example:8443/models/" + args[-1].lower() + ":latest"
    assert pathlib.Path(args[-1], "weights.bin").read_bytes() == b"fixture weights"
with (state / "published").open("a") as output:
    output.write(json.dumps({"args": args, "cwd": os.getcwd(), "python": os.environ["PYTHON"],
        "harbor_yml": os.environ["LOCAL_HARBOR_YML"],
        "selection": os.environ.get("EVA_VISION_MODELS"),
        "parent": pathlib.Path(f"/proc/{os.getppid()}/cmdline").read_bytes().decode().split("\0")}) + "\n")
'''

with tempfile.TemporaryDirectory(prefix="eva-vision-runtime-test-") as temporary:
    temporary = Path(temporary)
    cases = 0
    # The optional chart is a locally available published 3.3.0 archive/directory.
    # No fetching, model downloads, real Kubernetes calls or registry pushes.
    chart = os.environ.get("EVA_VISION_TEST_CHART", "")
    chart_defaults = {}
    if chart:
        chart_defaults = yaml.safe_load(subprocess.check_output(["helm", "show", "values", chart], text=True))
        model_environment = {key: value for key, value in os.environ.items() if key != "EVA_VISION_MODELS"}
        prepared_models = subprocess.check_output([sys.executable, "-B", str(root / "scripts/lib/vision_models.py"),
            "models"], env=model_environment, text=True).splitlines()
        required_models = set()
        for mar in chart_defaults["appConfig"]["load_models"].split(","):
            name = mar.strip().removesuffix(".mar")
            if name in {"Owl-v2", "ig"}:
                required_models.add("Owl-v2")
            elif name == "Embedding":
                required_models.update(chart_defaults["appConfig"]["embedding"]["enabledModels"].split(","))
            else:
                required_models.add(name)
        assert set(prepared_models) == required_models, "Prepared default weights differ from the published chart's model selection"
        print("[OK] prepared default weights cover the published chart's full model selection")

    # Check the role's image repository calculation, not just the Values fixture.
    main = yaml.safe_load((role / "tasks/main.yaml").read_text())
    lifecycle_names = ["Check existing EVA Vision deployment",
                       "Scale down existing EVA Vision deployment before reinstall"]
    task_names = [task["name"] for task in main]
    runtime_index = task_names.index("Prepare EVA Vision external model runtime")
    assert runtime_index < task_names.index(lifecycle_names[0]) < task_names.index(lifecycle_names[1])
    lifecycle_tasks = [task for task in main if task["name"] in lifecycle_names]
    harbor_tasks = yaml.safe_load((role / "tasks/harbor_model_secret.yaml").read_text())[0]
    assert harbor_tasks["no_log"] is True
    registry_read = harbor_tasks["block"][0]
    assert registry_read["ansible.builtin.slurp"]["src"] == "{{ eva_vision_registry_config_path | default('/etc/rancher/k3s/registries.yaml') }}"
    assert "delegate_to" not in harbor_tasks and "delegate_to" not in registry_read
    assert "lookup(" not in (role / "tasks/harbor_model_secret.yaml").read_text()
    image_expression = next(task for task in main if task["name"] == "Resolve EVA Vision image repository")["ansible.builtin.set_fact"]["eva_vision_image_repository"]
    for mode, expected in [
        ("online_repository", "211722900905.dkr.ecr.ap-northeast-2.amazonaws.com/spectrabrain/eva-vision"),
        ("cloud_repository", "211722900905.dkr.ecr.ap-northeast-2.amazonaws.com/spectrabrain/eva-vision"),
        ("remote_repository", "harbor.example:32080/eva/eva-vision"),
        ("local_repository", "harbor.example:32080/eva/eva-vision")]:
        variables = {key: value for key, value in defaults.items() if key != "eva_vision_image_repository"}
        assert environment.from_string(image_expression).render(**variables, repository_mode=mode).strip() == expected

    # Optional upstream source fixture verifies the real entrypoint sequence with
    # fetch/uv stubs. Nothing is fetched or served; all effects stay in a temp dir.
    source_repo = os.environ.get("EVA_VISION_TEST_SOURCE_REPO", "")
    if source_repo:
        def upstream(path):
            return subprocess.check_output(["git", "-C", source_repo, "show", "HEAD:" + path],
                env={**os.environ, "GIT_NO_LAZY_FETCH": "1"}, text=True)

        assert 'ENTRYPOINT ["./scripts/docker-entrypoint.sh"]' in upstream("Dockerfile")
        app = temporary / "app"
        (app / "scripts").mkdir(parents=True)
        (app / "bin").mkdir()
        entrypoint = app / "scripts/docker-entrypoint.sh"
        entrypoint.write_text(upstream("scripts/docker-entrypoint.sh").replace("cd /app", "cd " + shlex.quote(str(app))))
        fetch = app / "scripts/fetch_models.sh"
        fetch.write_text('#!/usr/bin/env bash\nprintf "fetch\\n" >> "$FLOW_LOG"\n')
        uv = app / "bin/uv"
        uv.write_text('#!/usr/bin/env bash\nprintf "%s\\n" "$*" >> "$FLOW_LOG"\n')
        for executable in [entrypoint, fetch, uv]:
            executable.chmod(0o755)
        (app / "scripts/warmup.py").touch()
        rendered_renderer = renderer_template.render(eva_vision_chart_version="3.3.0",
            eva_vision_progress_deadline_seconds=3600, eva_vision_skip_startup_warmup=True)
        namespace = {"__name__": "fixture"}
        exec(compile(rendered_renderer, "post-renderer", "exec"), namespace)
        command = [arg.replace("/app", str(app)) for arg in namespace["EXPECTED_COMMAND"]]
        command[1] = "-c"  # Do not let a login shell replace the fixture PATH.
        log = app / "flow.log"
        subprocess.run(command + ["--log-profile", "prod", "--foreground"], check=True,
            env={**os.environ, "PATH": str(app / "bin") + os.pathsep + os.environ["PATH"],
                 "MODEL_SOURCE": "harbor", "FLOW_LOG": str(log)})
        assert log.read_text().splitlines() == ["fetch", "run --no-sync ./run.sh archive --models all",
                                               "run --no-sync ./run.sh serve --log-profile prod --foreground"]
        assert not (app / "scripts/warmup.py").exists()
        assert (app / "scripts/warmup.py.disabled").exists()
        print("[OK] upstream entrypoint retains fetch -> offline archive -> offline serve, without warmup")
        if chart:
            fetch.write_text(upstream("scripts/fetch_models.sh"))
            for name in prepared_models:
                model_dir = app / "model_dir" / name
                model_dir.mkdir(parents=True)
                (model_dir / "weights.bin").write_bytes(b"fixture weights")
                if name in {"DARTF", "DARTF-light"}:
                    (model_dir / "sam3.pt").write_bytes(b"fixture weights")
                    (model_dir / "LICENSE").write_text("fixture license")
                if name == "DARTF-light":
                    (model_dir / "onnx").mkdir()
                    for filename in ("vision_int8.onnx", "vision_int8.onnx.data"):
                        (model_dir / "onnx" / filename).write_bytes(b"fixture weights")
            fetch_environment = {**os.environ, "MODEL_SOURCE": "none", "MODEL_DIR": str(app / "model_dir"),
                "TORCHSERVE_LOAD_MODELS": chart_defaults["appConfig"]["load_models"],
                "EMBEDDING_ENABLED_MODELS": chart_defaults["appConfig"]["embedding"]["enabledModels"]}
            result = subprocess.check_output(["bash", str(fetch)], env=fetch_environment, text=True)
            assert set(result.split("done: ", 1)[1].split()) == required_models
            fetch_environment["TORCHSERVE_LOAD_MODELS"] = "DARTF-light.mar"
            result = subprocess.check_output(["bash", str(fetch)], env=fetch_environment, text=True)
            assert result.split("done: ", 1)[1].split() == ["DARTF-light"]
            print("[OK] upstream model fetch accepts all prepared defaults and independent DARTF-light selection")

    def run_case(name, mode, *, helm_set=(), source=None, secret=None,
                 existing=False, bad_keys=False, credentials=False, fail=False,
                 packaged=True, models=None, harbor_config=None, home_mode="sudo",
                 interpreter="explicit", missing_helper=False, failure_message=None,
                 registry_config="managed", registry_key=None, registry_auth=None,
                 existing_data=None, create_race=False):
        global cases
        if not name.startswith(os.environ.get("EVA_VISION_TEST_CASE_PREFIX", "")):
            return
        case = temporary / name
        work = case / "target work"
        work.mkdir(parents=True)
        # A prepared Release deliberately has no scripts. Installed helpers are
        # controller-side, separate from both the Release and target workdir.
        release = case / "releases/3.3.0"
        for component in ("solution", "infra"):
            (release / "src" / component).mkdir(parents=True)
        helper_root = case / "controller tool/libexec/remote-root" if packaged else release
        selected_source = source or ("s3" if mode in {"online_repository", "cloud_repository"} else "harbor")
        for setting in helm_set:
            if setting.startswith("modelSource.type="):
                selected_source = setting.split("=", 1)[1]
        local_publish = mode == "local_repository" and selected_source == "harbor"
        values = yaml.safe_load(values_template.render(**defaults, repository_mode=mode))
        assert values["image"]["tag"] == "3.3.0"
        assert values["imagePullSecrets"]["ExistingSecret"] == "ecr-registry"
        assert "name" not in values["imagePullSecrets"]
        assert values["harbor"]["existingSecret"] == "eva-vision-harbor"
        assert values["modelCache"] == dict(enabled=True, existingClaim="", storageClass="local-path",
                                            size="20Gi", accessModes=["ReadWriteOnce"])
        if source:
            values["modelSource"]["type"] = source
        if secret is not None:
            values["s3" if values["modelSource"]["type"] == "s3" else "harbor"]["existingSecret"] = secret

        def merge(base, override):
            result = copy.deepcopy(base)
            for key, value in override.items():
                result[key] = merge(result.get(key, {}), value) if isinstance(value, dict) else copy.deepcopy(value)
            return result

        values = merge(chart_defaults, values)
        (work / "effective-input-values.yaml").write_text(yaml.safe_dump(values))
        fixture_bin = case / "bin"
        fixture_bin.mkdir()
        kubectl = fixture_bin / "kubectl"
        kubectl.write_text(mock_kubectl)
        kubectl.chmod(0o755)
        expected_secret = values.get(selected_source, {}).get("existingSecret", "")
        selected_registry = values["harbor"]["registry"]
        for setting in helm_set:
            if setting.startswith(selected_source + ".existingSecret="):
                expected_secret = setting.split("=", 1)[1]
            if setting.startswith("harbor.registry="):
                selected_registry = setting.split("=", 1)[1]
        selected_registry = re.sub(r"/+$", "", re.sub(r"^https?://", "", selected_registry.strip()))
        # Every invocation points at a tiny TARGET fixture, never host or
        # controller registry credentials, including public/Online/none paths.
        registry_path = case / "target/etc/rancher/k3s/registries.yaml"
        node_auth = registry_auth if registry_auth is not None else {
            "username": "admin", "password": 'fixture-custom-node-password:"$\\with spaces'}
        if registry_config is not None:
            registry_path.parent.mkdir(parents=True)
            registry_path.write_text(yaml.safe_dump({"configs": {
                registry_key or selected_registry: {"auth": node_auth}}})
                if registry_config == "managed" else registry_config)
        existing_secret_path = case / "existing-secret.json"
        existing_secret_contents = None
        if existing:
            data = ({"AWS_ACCESS_KEY_ID": "Zml4dHVyZQ==", "AWS_SECRET_ACCESS_KEY": "Zml4dHVyZQ=="}
                    if selected_source == "s3" else
                    {"HARBOR_USERNAME": "cm9ib3Q=", "HARBOR_PASSWORD": base64.b64encode(b"fixture-existing-operator-password").decode()})
            existing_secret_contents = json.dumps({"data": existing_data if existing_data is not None else ({} if bad_keys else data)})
            existing_secret_path.write_text(existing_secret_contents)
        credential_root = case / "credentials"
        credential_root.mkdir()
        if credentials:
            (credential_root / "aws_key.ini").write_text("[default]\naws_access_key_id = fixture-access\naws_secret_access_key = fixture-secret\n")
        env = dict(os.environ)
        for key in ("EVA_REMOTE_LIBEXEC_ROOT", "EVA_VISION_MODELS", "LOCAL_HARBOR_YML",
                    "REPOSITORY_USERNAME", "REPOSITORY_PASSWORD", "HARBOR_USERNAME",
                    "HARBOR_PASSWORD", "HARBOR_ADMIN_PASSWORD", "HARBOR_ARTIFACT_MANIFEST"):
            env.pop(key, None)
        env.update(PATH=str(fixture_bin) + os.pathsep + os.environ["PATH"],
                   FIXTURE_ROOT=str(case), EXPECTED_SECRET=expected_secret,
                   EXPECTED_MODEL_SOURCE=selected_source, EXPECTED_REGISTRY=selected_registry,
                   CREATE_RACE="1" if create_race else "0",
                   ANSIBLE_NOCOLOR="1", ANSIBLE_STDOUT_CALLBACK="default")
        # Remote/none see a nonexistent helper root and have NO model weights,
        # ORAS or publisher; successful execution proves those paths are gated.
        if packaged:
            env["EVA_REMOTE_LIBEXEC_ROOT"] = str(helper_root)
        if models is not None:
            env["EVA_VISION_MODELS"] = models
        target_home = case / "original user home"
        default_harbor_yml = target_home / ".local/share/eva-harbor/harbor/harbor.yml"
        expected_harbor_yml = case / "custom harbor/harbor.yml" if harbor_config else default_harbor_yml
        publish_user = "admin" if harbor_config or home_mode != "sudo" else "robot$publisher"
        env["EXPECTED_PUBLISH_USER"] = publish_user
        # Keep mock expectations outside the staged payload; real ORAS has no
        # fixture credential literals in its executable either.
        env["EXPECTED_PUBLISH_PASSWORD"] = "fixture-publish-secret"
        names = []
        if local_publish:
            for relative in ("publish/push_vision_models_to_harbor.sh", "lib/vision_models.py", "lib/docker_registry_auth.py"):
                if missing_helper and relative.startswith("lib/"):
                    continue
                helper = helper_root / "scripts" / relative
                helper.parent.mkdir(parents=True, exist_ok=True)
                helper.write_bytes((root / "scripts" / relative).read_bytes())
                helper.chmod(0o600)  # The role must set executable/readable target modes.
            cache = case / "cache/vision-models"
            # Use the real helper's defaults, not a second hard-coded list. The
            # invalid-selection cases still get a valid default model cache.
            default_env = {key: value for key, value in env.items() if key != "EVA_VISION_MODELS"}
            names = subprocess.check_output([sys.executable, "-B", str(root / "scripts/lib/vision_models.py"),
                "models"], env=default_env, text=True).splitlines()
            if models and ";" not in models:
                names = [model.strip() for model in models.split(",")]
            for model in names:
                directory = cache / "model_dir" / model
                directory.mkdir(parents=True)
                (directory / "weights.bin").write_bytes(b"fixture weights")
            subprocess.run([sys.executable, "-B", str(root / "scripts/lib/vision_models.py"),
                "manifest", "--cache", str(cache), "--models", ",".join(names)], env=default_env,
                check=True, stdout=subprocess.PIPE)
            oras = case / "cache/tools/oras"
            oras.parent.mkdir()
            oras.write_text("#!" + sys.executable + "\n" + mock_oras)
            oras.chmod(0o755)
            getent = fixture_bin / "getent"
            getent.write_text("#!" + sys.executable + "\n" +
                "import os, pathlib, sys\n" +
                "assert sys.argv[1:] == ['passwd', 'vision-user'], sys.argv\n" +
                "pathlib.Path(os.environ['FIXTURE_ROOT'], 'home-looked-up').touch()\n" +
                f"print('vision-user:x:1000:1000:fixture:{target_home}:/bin/bash')\n")
            getent.chmod(0o755)
            if publish_user == "admin":
                expected_harbor_yml.parent.mkdir(parents=True)
                expected_harbor_yml.write_text(yaml.safe_dump(dict(hostname="override.example",
                    https={"port": 8443}, harbor_admin_password="fixture-publish-secret")))
        variables = dict(eva_vision_workdir=str(work), repository_mode=mode,
                         eva_cli_helm_set=list(helm_set), eva_vision_namespace="eva-vision",
                         eva_vision_registry_config_path=str(registry_path),
                         eva_vision_name="eva-vision",
                         eva_vision_kubeconfig=str(case / "kubeconfig"), eva_repo_root=str(release),
                         eva_cache_root=str(case / "cache"), eva_workspace_credentials_root=str(credential_root),
                         repository_username=publish_user,
                         ansible_facts={"env": {"HOME": str(case / "become-root"), "USER": "root"},
                                        "user_id": "root", "python": {"executable": sys.executable}})
        if publish_user != "admin":
            variables["repository_password"] = "fixture-publish-secret"
        if home_mode == "sudo":
            variables["ansible_facts"]["env"]["SUDO_USER"] = "vision-user"
            variables["ansible_user"] = "not-the-original-sudo-user"
        elif home_mode == "ssh":
            variables["ansible_user"] = "vision-user"
        else:
            variables["ansible_facts"]["user_id"] = "vision-user"
        if interpreter == "explicit":
            variables["ansible_python_interpreter"] = sys.executable
            variables["ansible_facts"]["python"]["executable"] = "/must-not-use-fact-python"
        elif interpreter == "auto":
            variables["ansible_python_interpreter"] = "auto_silent"
        if harbor_config == "var":
            variables["local_harbor_yml"] = str(expected_harbor_yml)
            env["LOCAL_HARBOR_YML"] = "/must-not-use-controller-env/harbor.yml"
        elif harbor_config == "env":
            env["LOCAL_HARBOR_YML"] = str(expected_harbor_yml)
        # A caller rescue verifies cleanup even on parsing, validation or create
        # failure, then restores failure status before any deployment lifecycle.
        cleanup_checks = [
            "eva_vision_registry_config_raw | default({}) == {}",
            "eva_vision_registry_config | default({}) == {}",
            "eva_vision_model_secret | default({}) == {} or eva_vision_model_secret.skipped | default(false)",
            "eva_vision_aws_key_raw | default('') == ''",
            "eva_vision_s3_access_key | default('') == ''",
            "eva_vision_s3_secret_key | default('') == ''"]
        playbook = [{"hosts": "localhost", "connection": "local", "gather_facts": False,
                     "vars": variables, "tasks": [{
                         "block": [{"ansible.builtin.include_role": {
                             "name": str(role), "tasks_from": "model_runtime.yaml"}}],
                         "rescue": [{"ansible.builtin.set_fact": {"fixture_runtime_failed": True}}],
                         "always": [{"name": "Verify transient model credentials were cleared",
                                     "ansible.builtin.assert": {"that": cleanup_checks}}]},
                         {"ansible.builtin.fail": {"msg": "Fixture preserves runtime failure status"},
                          "when": "fixture_runtime_failed | default(false)"}] + lifecycle_tasks}]
        playbook_path = case / "playbook.yaml"
        playbook_path.write_text(yaml.safe_dump(playbook))
        process = subprocess.run([ansible, "-i", "localhost,", str(playbook_path)], env=env,
                                 text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        assert (process.returncode != 0) == fail, (name, process.stdout)
        canaries = ["fixture-access", "fixture-secret", "fixture-publish-secret",
                    "fixture-custom-node-password", "fixture-other-registry-password",
                    "fixture-existing-operator-password"]
        if isinstance(node_auth, dict):
            canaries.extend(value for key, value in node_auth.items()
                            if isinstance(value, str) and value.strip() and
                            (key == "password" or value != "admin"))
        encoded_canaries = [base64.b64encode(canary.encode()).decode() for canary in canaries]
        canaries.extend(encoded_canaries)
        assert all(canary not in process.stdout for canary in canaries), (name, process.stdout)
        assert "Verify transient model credentials were cleared" in process.stdout
        assert "All assertions passed" in process.stdout.split("Verify transient model credentials were cleared", 1)[1]
        assert (case / "scaled").exists() == (not fail), (name, process.stdout)
        kubectl_calls = [json.loads(line) for line in (case / "calls").read_text().splitlines()] if (case / "calls").exists() else []
        secret_calls = [call for call in kubectl_calls if call[:2] == ["get", "secret"]]
        source_allowed = selected_source != "s3" or mode in {"online_repository", "cloud_repository"}
        reads_registry = selected_source == "harbor" and bool(expected_secret) and not existing and source_allowed
        assert ("Read target k3s registry configuration for Vision models" in process.stdout) == reads_registry, (name, process.stdout)
        assert secret_calls == ([["get", "secret", expected_secret, "-n", "eva-vision",
                                  "--ignore-not-found=true", "-o", "json"]]
                    if expected_secret and source_allowed else [])
        if existing:
            assert existing_secret_path.read_text() == existing_secret_contents
        if create_race:
            assert existing_secret_path.read_text() == '{"data":{"HARBOR_USERNAME":"cm9ib3Q=","HARBOR_PASSWORD":"cmFjZQ=="}}'
        assert all(canary not in json.dumps(kubectl_calls) for canary in canaries)
        if fail:
            assert not (case / "created").exists() and not (case / "published").exists()
            assert not any(call[:2] == ["get", "deployment"] for call in kubectl_calls)
            if failure_message:
                assert failure_message in process.stdout, (name, process.stdout)
                for guidance in ["Restore or rerun the existing EVA infra registry setup on this target",
                                 "configs[registry].auth", "nonempty string username/password"]:
                    assert guidance in process.stdout, (name, process.stdout)
                assert "Create Secret '" not in process.stdout
                assert not (case / "home-looked-up").exists()
                assert not (work / "model-publisher").exists()
        else:
            resolved = yaml.safe_load((work / "runtime-values.yaml").read_text())
            selected = resolved["modelSource"]["type"]
            created = selected in {"s3", "harbor"} and bool(expected_secret) and not existing
            assert (case / "created").exists() == created
            assert len([call for call in kubectl_calls if call == ["create", "-f", "-"]]) == int(created)
            namespace_calls = [call for call in kubectl_calls if call[:2] == ["create", "namespace"]]
            assert namespace_calls == ([["create", "namespace", "eva-vision", "--dry-run=client", "-o", "yaml"]] if created else [])
            if not created:
                assert all(call[0] == "get" for call in kubectl_calls[:-1])  # Only lifecycle scale may write.
            assert (case / "published").exists() == local_publish
            if local_publish:
                staged = work / "model-publisher/scripts"
                for relative, mode_bits in [("publish/push_vision_models_to_harbor.sh", 0o755),
                                            ("lib/vision_models.py", 0o644),
                                            ("lib/docker_registry_auth.py", 0o644)]:
                    target = staged / relative
                    assert target.read_bytes() == (helper_root / "scripts" / relative).read_bytes()
                    assert stat.S_IMODE(target.stat().st_mode) == mode_bits
                assert not list(staged.rglob("__pycache__"))
                calls = [json.loads(line) for line in (case / "published").read_text().splitlines()]
                assert len(calls) == len(names) + 1
                assert [call["args"][-1] for call in calls[1:]] == names
                for call in calls:
                    assert call["python"] == sys.executable
                    assert call["harbor_yml"] == str(expected_harbor_yml)
                    assert call["selection"] == models
                    assert any(Path(arg).resolve() == staged / "lib/vision_models.py"
                               for arg in call["parent"] if arg.endswith("vision_models.py")), call["parent"]
                    assert "publish" in call["parent"]
                for call in calls[1:]:
                    assert call["cwd"] == str(case / "cache/vision-models/model_dir")
                assert (case / "home-looked-up").exists() == (not harbor_config)
                report = (case / "cache/vision-models/harbor-artifacts.txt").read_text().splitlines()
                assert len(report) == len(names) and all("|sha256:" in line for line in report)
            else:
                assert not (work / "model-publisher").exists()
                assert not (case / "cache").exists() and not helper_root.exists()
                assert not (case / "home-looked-up").exists()
            if chart:
                plugin = case / "plugins/eva-vision-postrenderer"
                plugin.mkdir(parents=True)
                (plugin / "plugin.yaml").write_text((role / "templates/post-renderer/plugin.yaml.j2").read_text())
                renderer = plugin / "post-renderer.py"
                renderer.write_text(renderer_template.render(eva_vision_chart_version="3.3.0",
                    eva_vision_progress_deadline_seconds=3600, eva_vision_skip_startup_warmup=True))
                renderer.chmod(0o755)
                rendered = subprocess.check_output(["helm", "template", "eva-vision", chart,
                    "-f", str(work / "runtime-values.yaml"), "--post-renderer", "eva-vision-postrenderer"],
                    env={**os.environ, "HELM_PLUGINS": str(case / "plugins"),
                         "PATH": str(Path(sys.executable).parent) + os.pathsep + os.environ["PATH"]}, text=True)
                assert all(canary not in rendered for canary in canaries)
                documents = [doc for doc in yaml.safe_load_all(rendered) if doc]
                assert all(isinstance(doc, dict) and doc.get("apiVersion") and doc.get("kind")
                           and doc.get("metadata", {}).get("name") for doc in documents), \
                    "Published Vision chart renders a document without Kubernetes apiVersion/kind/metadata.name"
                assert any(doc["kind"] == "Service" and doc["metadata"]["name"] == "eva-vision"
                           for doc in documents), "Published Vision chart does not render Service/eva-vision"
                deployment = next(doc for doc in documents if doc["kind"] == "Deployment")
                pod = deployment["spec"]["template"]["spec"]
                container = pod["containers"][0]
                assert container["image"] == resolved["image"]["repository"] + ":" + resolved["image"]["tag"]
                assert 'exec /app/scripts/docker-entrypoint.sh "$@"' in container["command"][2]
                assert container["args"] == ["--log-profile", "prod", "--foreground"]
                runtime_env = {item["name"]: item for item in container["env"]}
                assert runtime_env["MODEL_SOURCE"]["value"] == selected
                if expected_secret and selected in {"s3", "harbor"}:
                    keys = ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"] if selected == "s3" else ["HARBOR_USERNAME", "HARBOR_PASSWORD"]
                    for key in keys:
                        assert runtime_env[key] == {"name": key, "valueFrom": {
                            "secretKeyRef": {"name": expected_secret, "key": key}}}
                if selected != "s3":
                    assert not any(key.startswith("AWS_") for key in runtime_env)
                    assert not any(doc["kind"] == "Secret" for doc in documents)
                if selected == "harbor" and not expected_secret:
                    assert "HARBOR_USERNAME" not in runtime_env and "HARBOR_PASSWORD" not in runtime_env
                assert {"name": "model-cache", "mountPath": "/app/model_dir"} in container["volumeMounts"]
                has_models_claim = any(doc["kind"] == "PersistentVolumeClaim" and doc["metadata"]["name"] == "eva-vision-models" for doc in documents)
                assert has_models_claim == (not bool(resolved["modelCache"]["existingClaim"]))
                claim = next(volume for volume in pod["volumes"] if volume["name"] == "model-cache")
                assert claim["persistentVolumeClaim"]["claimName"] == (resolved["modelCache"]["existingClaim"] or "eva-vision-models")
                if mode in {"online_repository", "cloud_repository"}:
                    assert pod["imagePullSecrets"] == [{"name": "ecr-registry"}]
                else:
                    assert not pod["imagePullSecrets"]
        if packaged:
            assert not (release / "scripts").exists()
        # No credential sidecars/files may appear in runtime Values, templates,
        # staged Release/cache or an archive of those payload trees. Fixture
        # registry/AWS/publisher inputs deliberately live outside those trees.
        payload = io.BytesIO()
        with tarfile.open(fileobj=payload, mode="w") as archive:
            for tree in (work, release, case / "cache"):
                if tree.exists():
                    archive.add(tree, arcname=tree.relative_to(case))
        assert all(canary.encode() not in payload.getvalue() for canary in canaries), name
        assert not (case / "selection-injected").exists()
        cases += 1
        print(f"[OK] {name}")

    run_case("online-create-cli-secret", "online_repository", credentials=True,
             helm_set=["s3.existingSecret=cli-s3-secret"], registry_config=None)
    run_case("online-reuse-without-aws-file", "online_repository", existing=True, registry_config=None)
    run_case("online-image-tag-override", "online_repository", existing=True,
             helm_set=["image.tag=3.3.0rc-test"], registry_config=None)
    run_case("online-workspace-secret", "online_repository", secret="workspace-s3", existing=True, registry_config="[")
    run_case("cloud-alias-reuses-secret", "cloud_repository", existing=True, registry_config=None)
    run_case("invalid-existing-secret", "online_repository", existing=True, bad_keys=True, fail=True, registry_config=None)
    run_case("missing-online-credentials", "online_repository", fail=True, registry_config=None)
    run_case("online-workload-identity", "online_repository", secret="", registry_config=None)
    recovery_message = "Cannot prepare Vision Harbor model Secret"
    run_case("remote-default-private-secret", "remote_repository", existing=True, registry_config="[")
    run_case("remote-default-auto-create-from-infra", "remote_repository")
    run_case("remote-default-private-secret-invalid", "remote_repository", existing=True, bad_keys=True, fail=True, registry_config=None)
    run_case("remote-public-no-weights-or-aws", "remote_repository", secret="", registry_config=None)
    run_case("remote-cli-public-no-registry-read", "remote_repository", helm_set=["harbor.existingSecret="], registry_config="[")
    run_case("remote-reuse-model-cache-claim", "remote_repository", existing=True, helm_set=["modelCache.existingClaim=precreated-weights"], registry_config=None)
    run_case("remote-private-secret", "remote_repository", secret="harbor-reader", existing=True, registry_config=None)
    run_case("remote-cli-private-secret", "remote_repository", existing=True,
             helm_set=["harbor.existingSecret=cli-harbor-reader"], registry_config="[")
    run_case("remote-custom-secret-auto-create", "remote_repository", secret="harbor-reader")
    run_case("remote-cli-custom-secret-auto-create", "remote_repository", helm_set=["harbor.existingSecret=cli-harbor-reader"])
    run_case("remote-s3-override-rejected", "remote_repository", helm_set=["modelSource.type=s3"], fail=True, registry_config=None)
    run_case("remote-override-registry-exact-auth", "remote_repository",
             helm_set=["harbor.registry=https://override.example:8443///"],
             registry_auth={"username": "robot$node-reader", "password": "fixture-override-node-password"})
    run_case("remote-registry-normalization", "remote_repository", helm_set=["harbor.registry=  http://harbor.example:32080/  "])
    run_case("remote-other-registry-rejected", "remote_repository", registry_key="other.example:32080", fail=True,
             registry_auth={"username": "admin", "password": "fixture-other-registry-password"}, failure_message=recovery_message)
    run_case("remote-substring-registry-rejected", "remote_repository", registry_key="not-harbor.example:32080", fail=True,
             failure_message=recovery_message)
    run_case("remote-different-port-rejected", "remote_repository", registry_key="harbor.example:443", fail=True,
             failure_message=recovery_message)
    run_case("remote-controller-credentials-not-used", "remote_repository", credentials=True, registry_config=None, fail=True,
             failure_message=recovery_message)
    for label, config in [
            ("missing", None), ("empty", ""), ("malformed", "configs: [fixture-custom-node-password:\"$\\with spaces"),
            ("scalar", "fixture-other-registry-password"), ("sequence", "[]"),
            ("configs-missing", "{}"), ("configs-null", "configs: null"),
            ("configs-sequence", "configs: []"), ("configs-string", "configs: fixture-other-registry-password"),
            ("entry-null", "configs: {harbor.example:32080: null}"),
            ("entry-sequence", "configs: {harbor.example:32080: []}"),
            ("auth-missing", "configs: {harbor.example:32080: {}}"),
            ("auth-scalar", "configs: {harbor.example:32080: {auth: fixture-other-registry-password}}"),
            ("mirror-only", "mirrors: {harbor.example:32080: {endpoint: ['http://harbor.example:32080']}}")]:
        run_case("remote-registry-" + label, "remote_repository", registry_config=config, fail=True,
                 failure_message=recovery_message)
    for label, auth in [
            ("null", None), ("list", []), ("username-missing", {"password": "fixture-other-registry-password"}),
            ("password-missing", {"username": "admin"}),
            ("username-number", {"username": 123, "password": "fixture-other-registry-password"}),
            ("password-number", {"username": "admin", "password": 123}),
            ("username-empty", {"username": "  ", "password": "fixture-other-registry-password"}),
            ("password-empty", {"username": "admin", "password": ""}),
            ("password-null", {"username": "admin", "password": None})]:
        config = yaml.safe_dump({"configs": {"harbor.example:32080": {"auth": auth}}})
        run_case("remote-auth-" + label, "remote_repository", registry_config=config, fail=True,
                 failure_message=recovery_message)
    run_case("remote-existing-empty-decoded-password", "remote_repository", existing=True,
             existing_data={"HARBOR_USERNAME": "cm9ib3Q=", "HARBOR_PASSWORD": "ICA="}, registry_config=None, fail=True)
    run_case("remote-existing-nonstring-password", "remote_repository", existing=True,
             existing_data={"HARBOR_USERNAME": "cm9ib3Q=", "HARBOR_PASSWORD": 123}, registry_config="[", fail=True)
    run_case("remote-create-race-preserves-operator-secret", "remote_repository", create_race=True, fail=True,
             failure_message=recovery_message)
    local_settings = ["harbor.registry=override.example:8443", "harbor.project=models",
                      "harbor.plainHttp=false", "harbor.insecure=true"]
    run_case("local-packaged-publish-effective-harbor", "local_repository", existing=True, helm_set=local_settings, registry_config="[")
    run_case("local-image-tag-override", "local_repository", existing=True,
             helm_set=local_settings + ["image.tag=3.3.0rc-test"], registry_config="[")
    run_case("local-default-auto-create-and-publish", "local_repository", helm_set=local_settings, home_mode="ssh")
    run_case("local-cli-custom-secret-auto-create-and-publish", "local_repository", helm_set=local_settings + ["harbor.existingSecret=cli-model-reader"])
    run_case("local-missing-registry-before-publish", "local_repository", fail=True, helm_set=local_settings,
             registry_config=None, failure_message=recovery_message)
    run_case("local-default-private-secret-invalid", "local_repository", existing=True, bad_keys=True, fail=True, helm_set=local_settings, registry_config=None)
    run_case("local-cli-public-publish", "local_repository", helm_set=local_settings + ["harbor.existingSecret="], registry_config="[")
    run_case("local-checkout-fallback", "local_repository", existing=True, packaged=False, helm_set=local_settings, registry_config=None)
    run_case("local-custom-model-selection", "local_repository", existing=True, models=" OmDet, Owl-v2 ", helm_set=local_settings)
    run_case("local-target-home-harbor-fallback", "local_repository", existing=True, home_mode="ssh", helm_set=local_settings)
    run_case("local-target-fact-home-python", "local_repository", existing=True, home_mode="facts", interpreter="facts", helm_set=local_settings)
    run_case("local-auto-python-env-harbor-override", "local_repository", existing=True, interpreter="auto", harbor_config="env", helm_set=local_settings)
    run_case("local-explicit-harbor-path-wins", "local_repository", existing=True, harbor_config="var", helm_set=local_settings)
    run_case("local-missing-packaged-module", "local_repository", existing=True, missing_helper=True, fail=True, helm_set=local_settings)
    run_case("local-empty-model-selection-rejected", "local_repository", existing=True, models="", fail=True, helm_set=local_settings)
    run_case("local-model-selection-not-shell-evaluated", "local_repository", existing=True,
             models="Owl-v2; touch \"$FIXTURE_ROOT/selection-injected\"", fail=True, helm_set=local_settings)
    run_case("local-none-no-publish", "local_repository", helm_set=["modelSource.type=none"], registry_config=None)
    run_case("online-harbor-no-publish", "online_repository", source="harbor", secret="", registry_config="[")
    run_case("remote-none-no-credentials", "remote_repository", source="none", registry_config="[")

    # Execute the exact jq inventory filter: new PVCs/registry Secret must not
    # widen the deletion set, while unknown resources remain rejected.
    reconciliation = yaml.safe_load((role / "tasks/reconcile_vision.yaml").read_text())[0]["ansible.builtin.shell"]
    query = reconciliation.split("jq -r '\n", 1)[1].split("' \"$rendered_json_file\"", 1)[0]
    inventory = [{"kind": kind, "metadata": {"name": name}} for kind, name in [
        ("ConfigMap", "eva-vision-config"), ("Deployment", "eva-vision"),
        ("Service", "eva-vision"), ("ServiceAccount", "eva-vision-sa"),
        ("PersistentVolumeClaim", "eva-vision-models"), ("PersistentVolumeClaim", "eva-vision-logs"),
        ("Secret", "eva-vision-regcred")]]
    filtered = subprocess.check_output(["jq", "-r", query], input=json.dumps(inventory), text=True)
    assert len(filtered.splitlines()) == 4 and "PersistentVolumeClaim" not in filtered and "Secret\t" not in filtered
    inventory.append({"kind": "Secret", "metadata": {"name": "foreign"}})
    assert "Secret\tforeign" in subprocess.check_output(["jq", "-r", query], input=json.dumps(inventory), text=True)
    assert "persistentvolumeclaim" not in reconciliation.split("kubectl delete", 1)[-1].lower()
    assert cases > 0, "No Vision runtime fixtures matched the requested prefix"
    print(f"EVA Vision model runtime contracts passed ({cases} Ansible cases; published chart renders: {bool(chart)}).")
PY
