#!/usr/bin/env bash
# Offline contract test for the published App/IAM pair. No download fallback.
# EVA_APP_CHART_ARCHIVE / EVA_IAM_CHART_ARCHIVE may select explicit local archives.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ANSIBLE_PLAYBOOK="${ANSIBLE_PLAYBOOK:-ansible-playbook}"
HELM="${HELM:-helm}"
PYTHON="${PYTHON:-python3}"
EVA_APP_CHART_ARCHIVE="${EVA_APP_CHART_ARCHIVE:-$REPO_ROOT/out/cache/eva-app/eva-app-3.3.0-beta.3.tgz}"
EVA_IAM_CHART_ARCHIVE="${EVA_IAM_CHART_ARCHIVE:-$REPO_ROOT/out/cache/eva-iam/eva-iam-3.3.0-beta.1.tgz}"

for archive in "$EVA_APP_CHART_ARCHIVE" "$EVA_IAM_CHART_ARCHIVE"; do
  if [[ ! -f "$archive" || ! -r "$archive" ]]; then
    printf '[ERROR] Required local chart archive is missing or unreadable: %s\n' "$archive" >&2
    printf 'Set EVA_APP_CHART_ARCHIVE and EVA_IAM_CHART_ARCHIVE; this test never downloads charts.\n' >&2
    exit 1
  fi
done
for executable in "$ANSIBLE_PLAYBOOK" "$HELM" "$PYTHON"; do
  command -v "$executable" >/dev/null 2>&1 || {
    printf '[ERROR] Required executable was not found: %s\n' "$executable" >&2
    exit 1
  }
done

TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/eva-app-iam-contract.XXXXXXXX")"
trap 'rm -rf "$TMP_ROOT"' EXIT

# Source only the shared functions, never any preparation/download entrypoint.
source "$REPO_ROOT/scripts/lib/load_versions.sh"
for version in 3.1.8 3.2.0 3.3.0-beta.3 3.3.0-beta.1 3.3.0 4.0.0; do
  for component in app iam; do
    printf '%s\t%s\t%s\n' "$component" "$version" "$(eva_product_chart_base "$component" "$version")"
  done
done >"$TMP_ROOT/chart-bases.tsv"

"$PYTHON" - "$REPO_ROOT" "$TMP_ROOT" "$ANSIBLE_PLAYBOOK" "$HELM" \
  "$EVA_APP_CHART_ARCHIVE" "$EVA_IAM_CHART_ARCHIVE" <<'PY_CONTRACT'
from copy import deepcopy
import ipaddress
import os
from pathlib import Path
import shutil
import subprocess
import sys

import yaml


repo, tmp = map(Path, sys.argv[1:3])
ansible, helm = (shutil.which(value) for value in sys.argv[3:5])
archives = dict(zip(("app", "iam"), (str(Path(p).resolve()) for p in sys.argv[5:7])))
versions = {"app": "3.3.0-beta.3", "iam": "3.3.0-beta.1"}
catalog = yaml.safe_load((repo / "src/solution/version.yaml").read_text())
for key, expected in (("eva_app_deploy_version", versions["app"]),
                      ("eva_app_chart_version", versions["app"]),
                      ("eva_iam_chart_version", versions["iam"])):
    assert catalog[key] == expected, f"catalog {key}: {catalog[key]!r} != {expected!r}"
bases = {}
for line in (tmp / "chart-bases.tsv").read_text().splitlines():
    component, version, base = line.split("\t")
    expected = (f"https://mellerikat.github.io/eva-{component}" if version in {"3.1.8", "3.2.0"}
                else f"https://spectrabrain.github.io/eva-{component}-chart")
    assert base == expected, (component, version, base, expected)
    bases[component, version] = base

# Isolated Helm state and no usable kubeconfig. Only client-side show/template
# commands below are permitted; dependencies are never updated or downloaded.
env = os.environ.copy()
for key in ("HELM_CACHE_HOME", "HELM_CONFIG_HOME", "HELM_DATA_HOME", "ANSIBLE_LOCAL_TEMP"):
    directory = tmp / key.lower()
    directory.mkdir()
    env[key] = str(directory)
env.update(KUBECONFIG=str(tmp / "nonexistent-kubeconfig"),
           ANSIBLE_CONFIG=str(tmp / "ansible.cfg"), ANSIBLE_NOCOLOR="1")
(tmp / "ansible.cfg").write_text(
    "[defaults]\nretry_files_enabled = False\nhost_key_checking = False\n"
    "interpreter_python = auto_silent\nforks = 8\n"
)


def helm_output(*args):
    assert args[0] in {"show", "template"}, args
    result = subprocess.run([helm, *args], env=env, text=True, capture_output=True)
    if result.returncode:
        raise AssertionError(f"helm {' '.join(args)} failed:\n{result.stderr}")
    return result.stdout


defaults = {}
for component, archive in archives.items():
    metadata = yaml.safe_load(helm_output("show", "chart", archive))
    for key, expected in (("name", f"eva-{component}"), ("version", versions[component]),
                          ("appVersion", versions[component])):
        assert metadata[key] == expected, (component, key, metadata[key], expected)
    defaults[component] = yaml.safe_load(helm_output("show", "values", archive))
print("[PASS] exact local chart version/appVersion and deploy_versions catalog pins", flush=True)

role_tasks = {
    component: yaml.safe_load((repo / f"src/solution/roles/eva_{component}/tasks/main.yaml").read_text())
    for component in ("app", "iam")
}
allowed = {"ansible.builtin.set_fact", "ansible.builtin.assert", "ansible.builtin.template"}


def task(component, name):
    matches = [t for t in role_tasks[component] if t.get("name") == name]
    assert len(matches) == 1, f"expected exactly one {component} task: {name}"
    selected = deepcopy(matches[0])
    modules = {key for key in selected if key.startswith("ansible.builtin.")}
    assert len(modules) == 1 and modules <= allowed, (name, modules)
    return selected


def tasks(component, *names):
    return [task(component, name) for name in names]


def fact(name, **values):
    return {"name": name, "ansible.builtin.set_fact": values}


def check(name, *expressions):
    return {"name": name, "ansible.builtin.assert": {"that": list(expressions)}}


def render(name, src, dest):
    return {"name": name, "ansible.builtin.template": {"src": str(src), "dest": dest, "mode": "0600"}}


# Evaluate the actual caller's when/vars without importing its deployment role.
# Replacing only the action with set_fact makes accidental kubectl impossible.
def caller_probe(component):
    matches = [t for t in role_tasks[component]
               if t.get("ansible.builtin.import_role", {}).get("name") == "eva_traefik_tls"]
    assert len(matches) == 1, component
    selected = deepcopy(matches[0])
    del selected["ansible.builtin.import_role"]
    selected["ansible.builtin.set_fact"] = {f"fixture_{component}_tls_caller": True}
    selected["name"] = f"Probe actual {component} TLS caller conditions without deployment"
    return selected


# Reuse the production handoff body verbatim, but render as an unprivileged
# template instead of running copy/chown, discovering a live secret, or deploying.
handoff = [t for t in role_tasks["iam"]
           if t.get("name") == "Write eva-iam.yaml (eva-app SSO hand-off) on control node"]
assert len(handoff) == 1
(tmp / "handoff.j2").write_text(handoff[0]["ansible.builtin.copy"]["content"])
(tmp / "app-effective.j2").write_text("{{ eva_app_effective_values | to_nice_yaml(sort_keys=false) }}\n")
(tmp / "snapshot.j2").write_text("""{{ {
  'app_chart_repo': eva_app_chart_repo_url, 'iam_chart_repo': eva_iam_chart_repo_url,
  'app_chart_source': eva_app_chart_source, 'iam_chart_source': eva_iam_chart_source,
  'iam_version_arg': eva_iam_chart_version_arg,
  'app_repository': eva_app_image_repository, 'app_account': eva_app_ecr_account,
  'app_tls_job': eva_app_tls_job_enabled, 'iam_tls': eva_iam_effective_tls_enabled,
  'app_tls_caller': fixture_app_tls_caller, 'iam_tls_caller': fixture_iam_tls_caller,
  'handoff_used': eva_app_handoff_used, 'sso_base_url': eva_iam_app_sso_base_url,
  'iam_user': eva_iam_effective_user_values,
  'app_kubeconfig': eva_app_kubeconfig, 'iam_kubeconfig': eva_iam_kubeconfig
} | to_nice_yaml(sort_keys=false) }}
""")

samples = repo / "tools/eva/internal/workspace/samples"
for layout in ("shared", "separate"):
    sample = yaml.safe_load((samples / f"{layout}-http.yaml").read_text())
    assert sample["iam"]["redis"]["tls"]["enabled"] is False, layout
    for component in ("app", "iam"):
        assert sample[component]["ingress"]["tls"]["enabled"] is False, (layout, component)
        assert sample[component]["ingress"]["tls"]["create"] is False, (layout, component)
print("[PASS] shared/separate HTTP samples explicitly disable ingress and Redis TLS", flush=True)

cases = []


def add_case(name, mode="online_repository", protocol="https", host_kind="dns", **options):
    layout = "shared" if host_kind == "ip" else "separate"
    substitutions = {"<PUBLIC_HOST>": "192.0.2.10", "<APP_HOST>": "app.example.test",
                     "<IAM_HOST>": "iam.example.test", "<TLS_CERT_DIRECTORY>": "/fixture/certs",
                     "<REALM_ADMIN_PASSWORD>": "fixture-realm-password",
                     "<LICENSE_PRODUCT_CODE>": "eva-dev", "<LICENSE_API_KEY>": "fixture-license-key",
                     "<LICENSE_SHARED_KEY>": "fixture-shared-key"}
    text = (samples / f"{layout}-{protocol}.yaml").read_text()
    for placeholder, value in substitutions.items():
        text = text.replace(placeholder, value)
    values = yaml.safe_load(text)
    directory = tmp / name
    for path in (directory, directory / "app", directory / "iam", directory / "state" / name):
        path.mkdir(parents=True, exist_ok=True)
    variables = {
        "fixture_dir": str(directory), "fixture_app_tls_caller": False, "fixture_iam_tls_caller": False,
        "repository_mode": mode, "eva_site_id": "contract-site", "eva_site_state_root": str(directory / "state"),
        "eva_site_config_root": str(directory / "config"), "eva_site_deploy_root": str(tmp),
        "eva_site_values_root": str(directory), "eva_cache_root": str(directory / "cache"),
        "eva_template_values_root": str(repo / "src/solution/values"),
        "eva_app_chart_version": catalog["eva_app_chart_version"],
        "eva_app_deploy_version": catalog["eva_app_deploy_version"],
        "eva_iam_admin_api_client_secret": "fixture-runtime-secret",
        # No live Helm release: resolve the client identity from local chart defaults.
        "eva_iam_resolved_values_result": {"stdout": yaml.safe_dump(defaults["iam"])},
        # These replace only file-discovery I/O, not value/default resolution.
        "eva_iam_values_file_stat": {"stat": {"exists": True}},
        "eva_app_values_file_status": {"stat": {"exists": True}},
        "eva_app_iam_handoff_status": {"stat": {"exists": True}},
        "fixture_facts": {"env": {"HOME": str(directory / "home")},
                          "default_ipv4": {"address": "192.0.2.10"}},
        "ansible_python_interpreter": sys.executable,
    }
    if mode == "remote_repository":
        variables["repository_registry"] = "https://harbor.example.test:8443///"
    if options.get("omit_tls_flags"):
        for component in ("app", "iam"):
            values[component]["ingress"]["tls"].pop("enabled")
            values[component]["ingress"]["tls"].pop("create")
    if "public_port" in options:
        values["iam"]["config"]["publicPort"] = options["public_port"]
        # Exercise the role's redirect default, not a sample's explicit list.
        del values["iam"]["keycloak"]["realmPatch"]["evaApp"]["redirectUris"]
    if "public_url" in options:
        values["iam"]["config"]["publicUrl"] = options["public_url"]
    if "redis_tls" in options:
        # Test the real role-level default path independently of ingress TLS.
        del values["iam"]["redis"]["tls"]["enabled"]
        variables["eva_iam_redis_tls_enabled"] = options["redis_tls"]
    if options.get("account"):
        variables["eva_app_ecr_account"] = options["account"]
    for component in ("app", "iam"):
        if options.get("no_create"):
            values[component]["ingress"]["tls"]["create"] = False
        if options.get("secret_auth"):
            values[component]["ingress"]["tls"]["authType"] = "secret"
        if options.get("no_ingress"):
            values[component]["ingress"]["enabled"] = False
        (directory / f"{component}.yaml").write_text(yaml.safe_dump({name: values[component]}))
    cases.append({"name": name, "mode": mode, "protocol": protocol, "host_kind": host_kind,
                  "values": values, "variables": variables, "options": options})


for mode in ("online_repository", "cloud_repository", "remote_repository", "local_repository"):
    for protocol in ("http", "https"):
        for host_kind in ("dns", "ip"):
            add_case(f"{mode}-{protocol}-{host_kind}", mode, protocol, host_kind)
for host_kind in ("dns", "ip"):
    add_case(f"https-default-flags-{host_kind}", host_kind=host_kind, omit_tls_flags=True)
for protocol in ("http", "https"):
    add_case(f"public-port-{protocol}", protocol=protocol, public_port="32010")
    # Opposite scheme plus different authority/path: publicUrl must win exactly.
    other = "https" if protocol == "http" else "http"
    add_case(f"public-url-{protocol}", protocol=protocol, public_port="32010",
             public_url=f"{other}://public.example.test:32443/identity/")
    add_case(f"redis-independent-{protocol}", protocol=protocol, redis_tls=(protocol == "http"))
add_case("custom-app-account", account="123456789012")
add_case("https-no-create", no_create=True)
add_case("https-secret-auth", secret_auth=True)
add_case("https-no-ingress", no_ingress=True)

fixture_tasks = [fact("Load fixture IAM site file (as wizard host-keyed values)",
                      eva_iam_values_loaded="{{ lookup('ansible.builtin.file', fixture_dir ~ '/iam.yaml') | from_yaml }}")]
fixture_tasks += tasks("iam", "Resolve eva-iam values file", "Select this host's eva-iam values block",
                       "Split eva-iam value overrides from ansible deploy options",
                       "Resolve effective eva-iam user values", "Apply eva-iam deploy options from values file",
                       "Note explicitly supplied eva-iam credentials",
                       "Resolve node-side user for eva-iam hostPath defaults",
                       "Resolve node-side home for eva-iam hostPath defaults",
                       "Set EVA IAM (SSO) install defaults", "Resolve eva-iam workspace paths",
                       "Resolve eva-iam offline directory", "Resolve eva-iam chart source and image repositories",
                       "Resolve the eva-iam chart reference to install from", "Resolve effective eva-iam NodePorts",
                       "Resolve eva-iam generated config for eva-app", "Validate repository mode for EVA IAM",
                       "Validate eva-iam chart version supports publicPort / publicUrl",
                       "Render eva-iam k3s override values on control node",
                       "Resolve deployed EVA IAM App client identity")
fixture_tasks += [caller_probe("iam"), render("Render real IAM handoff body", tmp / "handoff.j2", "{{ eva_iam_app_handoff_path }}")]
fixture_tasks += tasks("app", "Set EVA App install defaults (on-premise)", "Validate repository mode for EVA App",
                       "Validate EVA App chart version supports values-driven images",
                       "Resolve EVA App image repositories", "Resolve EVA App chart source",
                       "Render EVA App k3s override values on control node",
                       "Initialize EVA App generated and override values",
                       "Load EVA App role defaults for effective values", "Load host-specific EVA App values",
                       "Select host-specific EVA App values", "Determine whether Workspace explicitly defines complete EVA App SSO")
fixture_tasks += [fact("Read rendered handoff instead of slurping a deployed node",
                       eva_app_iam_handoff_raw={"content": "{{ lookup('ansible.builtin.file', eva_app_iam_handoff_path) | b64encode }}"})]
fixture_tasks += tasks("app", "Load same-target IAM-to-App handoff", "Validate same-target IAM-to-App handoff metadata",
                       "Require complete Workspace SSO or valid IAM handoff", "Use validated same-target IAM handoff",
                       "Build IAM SSO values for EVA App", "Merge generated and installer EVA App values",
                       "Merge IAM handoff into EVA App values", "Merge Workspace EVA App values", "Merge CLI EVA App values",
                       "Disable ECR refresh resources for repository-backed EVA App",
                       "Determine whether EVA App TLS Job is enabled")
fixture_tasks += [caller_probe("app"), check("Require real handoff and typed TLS facts",
                  "eva_app_handoff_used", "eva_app_tls_job_enabled is boolean", "eva_iam_effective_tls_enabled is boolean"),
                  render("Record actual effective App values", tmp / "app-effective.j2", "{{ fixture_dir }}/app-effective.yaml"),
                  render("Record selected real role facts", tmp / "snapshot.j2", "{{ fixture_dir }}/snapshot.yaml")]

# Check both sides of the shared repository migration boundary using real online
# defaults. These fact-only cases deliberately do not render a mismatched chart.
boundary_tasks = tasks("app", "Set EVA App install defaults (on-premise)", "Resolve EVA App image repositories")
boundary_tasks += tasks("iam", "Resolve node-side user for eva-iam hostPath defaults",
                        "Resolve node-side home for eva-iam hostPath defaults", "Set EVA IAM (SSO) install defaults")
boundary_tasks += [check("Shared chart helper matches actual online defaults",
                        "eva_app_chart_repo_url == fixture_app_base", "eva_iam_chart_repo_url == fixture_iam_base",
                        "eva_app_ecr_account == fixture_account", "eva_app_image_repository == fixture_app_repository")]
inventory = {"all": {"children": {"fixtures": {"hosts": {}}, "boundary": {"hosts": {}}}}}
for case in cases:
    inventory["all"]["children"]["fixtures"]["hosts"][case["name"]] = case["variables"]
for version in ("3.1.8", "3.2.0", "3.3.0", "4.0.0"):
    modern = version in {"3.3.0", "4.0.0"}
    variables = deepcopy(cases[0]["variables"])
    variables.update(eva_app_chart_version=version, eva_iam_chart_version=version,
                     eva_iam_effective_user_values={}, eva_iam_user_values={},
                     fixture_app_base=bases["app", version], fixture_iam_base=bases["iam", version],
                     fixture_account="211722900905" if modern else "339713051385",
                     fixture_app_repository=("211722900905.dkr.ecr.ap-northeast-2.amazonaws.com/spectrabrain/eva-app"
                         if modern else "339713051385.dkr.ecr.ap-northeast-2.amazonaws.com/mellerikat/release/eva-app"))
    inventory["all"]["children"]["boundary"]["hosts"]["boundary-" + version.replace(".", "-")] = variables
common = {"deploy_versions": catalog, "ansible_facts": "{{ fixture_facts }}", "ansible_connection": "local",
          "ansible_become": False}
plays = [{"name": "Offline published App/IAM release contract", "hosts": "fixtures", "gather_facts": False,
          "vars": common, "tasks": fixture_tasks},
         {"name": "Online repository migration boundary (facts only)", "hosts": "boundary", "gather_facts": False,
          "vars": common, "tasks": boundary_tasks}]
for play in plays:
    for selected in play["tasks"]:
        modules = {key for key in selected if key.startswith("ansible.builtin.")}
        assert len(modules) == 1 and modules <= allowed, selected["name"]
(tmp / "inventory.yaml").write_text(yaml.safe_dump(inventory, sort_keys=False))
(tmp / "playbook.yaml").write_text(yaml.safe_dump(plays, sort_keys=False))
print(f"[INFO] executing {len(cases)} real-role render fixtures and 4 repository-boundary fixtures", flush=True)
with (tmp / "ansible.log").open("w") as log:
    result = subprocess.run([ansible, "-i", str(tmp / "inventory.yaml"), str(tmp / "playbook.yaml")],
                            env=env, stdout=log, stderr=subprocess.STDOUT)
if result.returncode:
    print((tmp / "ansible.log").read_text(), file=sys.stderr)
    raise SystemExit(result.returncode)
print("[PASS] selected real role facts/templates, SSO handoff consumer and repository boundaries", flush=True)


def load(path):
    return yaml.safe_load(path.read_text())


def one(resources, kind, suffix):
    matches = [r for r in resources if r["kind"] == kind and r["metadata"]["name"].endswith(suffix)]
    assert len(matches) == 1, (kind, suffix, [r["metadata"]["name"] for r in matches])
    return matches[0]


def pod_containers(resources):
    containers = []
    for resource in resources:
        if resource["kind"] not in {"Deployment", "Job", "CronJob"}:
            continue
        spec = resource["spec"]
        if resource["kind"] == "CronJob":
            spec = spec["jobTemplate"]["spec"]
        pod = spec["template"]["spec"]
        containers.extend(pod.get("containers", []) + pod.get("initContainers", []))
    return containers


def named_image(resources, container_name):
    matches = {c["image"] for c in pod_containers(resources) if c["name"] == container_name}
    assert len(matches) == 1, (container_name, matches)
    return matches.pop()


def protocol_contract(component, resources, site, tls, ingress_enabled, create):
    ingresses = [r for r in resources if r["kind"] == "Ingress"]
    assert len(ingresses) == int(ingress_enabled), (component, "ingress count")
    for ingress in ingresses:
        assert bool(ingress["spec"].get("tls")) == tls, (component, "ingress TLS")
        host = site["app"]["backendHost"] if component == "app" else site["config"]["host"]
        try:
            ipaddress.ip_address(host)
            is_ip = True
        except ValueError:
            is_ip = False
        for rule in ingress["spec"]["rules"]:
            assert rule.get("host") == (None if is_ip else host), (component, rule)
            assert rule["http"]["paths"][0]["path"] == site["ingress"]["path"], (component, rule)
    jobs = [r for r in resources if r["kind"] == "Job" and r["metadata"]["name"].endswith("-tls-job")]
    assert len(jobs) == int(tls and ingress_enabled and create), (component, "TLS Job count")


image_paths = {"keycloak": ("keycloak", "image"), "dispatcher": ("dispatcher", "image"),
               "keycloak-config-cli": ("keycloak", "realmConfig", "image"),
               "postgres": ("database", "internal", "image"), "redis": ("redis", "image"),
               "busybox": ("keycloak", "initContainer", "image"),
               "kubectl": ("ingress", "tls", "job", "kubectlImage")}


def nested(value, path):
    for key in path:
        value = value.get(key, {})
    return value


for case in cases:
    name, site, options = case["name"], case["values"], case["options"]
    try:
        directory = tmp / name
        snapshot = load(directory / "snapshot.yaml")
        app_role = load(directory / "app/app-k3s-override.yaml")
        iam_role = load(directory / "iam/eva-iam-k3s-override.yaml")
        app_effective = load(directory / "app-effective.yaml")
        tls = case["protocol"] == "https"
        ingress_enabled, create = not options.get("no_ingress", False), not options.get("no_create", False)
        caller = tls and ingress_enabled and create and not options.get("secret_auth", False)
        assert snapshot["app_tls_caller"] == caller and snapshot["iam_tls_caller"] == caller
        assert snapshot["app_tls_job"] == (tls and ingress_enabled and create)
        assert snapshot["iam_tls"] == tls and snapshot["handoff_used"] is True
        for component in ("app", "iam"):
            assert snapshot[f"{component}_chart_repo"] == bases[component, versions[component]]
            assert snapshot[f"{component}_kubeconfig"] == str(directory / "home/.kube/config")
        online = case["mode"] in {"online_repository", "cloud_repository"}
        registry = "harbor.example.test:8443" if case["mode"] == "remote_repository" else "localhost:32080"
        app_repository = ("211722900905.dkr.ecr.ap-northeast-2.amazonaws.com/spectrabrain/eva-app"
                          if online else registry + "/eva/eva-app")
        assert snapshot["app_repository"] == app_repository
        account = options.get("account", "211722900905")
        assert snapshot["app_account"] == account and app_role["imagePullSecrets"]["account"] == account
        for component in ("app", "iam"):
            expected_source = f"eva-{component}/eva-{component}" if online else str(directory / "cache" / f"eva-{component}" / f"eva-{component}-{versions[component]}.tgz")
            assert snapshot[f"{component}_chart_source"] == expected_source
        assert snapshot["iam_version_arg"] == (f"--version {versions['iam']}" if online else "")
        resources = {}
        for component in ("app", "iam"):
            arguments = ["template", f"eva-{component}", archives[component], "--namespace", f"eva-{component}"]
            if component == "app":
                arguments += ["-f", str(directory / "app-effective.yaml")]
            else:
                arguments += ["-f", str(directory / "iam/eva-iam-k3s-override.yaml")]
                site_path = directory / "iam-user.yaml"
                site_path.write_text(yaml.safe_dump(snapshot["iam_user"]))
                arguments += ["-f", str(site_path)]
            manifest = helm_output(*arguments)
            (directory / f"{component}-manifest.yaml").write_text(manifest)
            resources[component] = [r for r in yaml.safe_load_all(manifest) if r]
            protocol_contract(component, resources[component], site[component], tls, ingress_enabled, create)
        app_resources, iam_resources = resources["app"], resources["iam"]
        assert named_image(app_resources, "eva-app") == app_repository + ":" + versions["app"]
        assert named_image(app_resources, "eva-app-mysql") == ("mysql" if online else registry + "/eva/mysql") + ":8.0.42-bookworm"
        assert app_effective["imagePullSecrets"]["enabled"] == online
        assert app_effective["imagePullSecrets"]["create"] == online
        assert iam_role["imagePullSecrets"]["enabled"] == online
        assert iam_role["imagePullSecrets"]["create"] == online
        assert app_role["image"]["pullPolicy"] == ("Always" if online else "IfNotPresent")
        app_manifest = (directory / "app-manifest.yaml").read_text()
        if online:
            assert f"{account}.dkr.ecr.ap-northeast-2.amazonaws.com" in app_manifest, "ECR job must use selected account"
        else:
            assert not any(r["kind"] == "CronJob" for r in app_resources + iam_resources)
            for component_resources in resources.values():
                assert all(c["image"].startswith(registry + "/eva/") for c in pod_containers(component_resources))
        # Online IAM must inherit every source/tag, including appVersion fallback,
        # rather than pinning a stale product repository or image tag in the role.
        iam_images = {c["image"] for c in pod_containers(iam_resources)}
        for image_name, path in image_paths.items():
            original = nested(defaults["iam"], path)
            override = nested(iam_role, path)
            assert "tag" not in override, (image_name, "role must preserve chart tag")
            if online:
                assert "repository" not in override, (image_name, "Online role must preserve chart source")
            else:
                assert override["repository"] == registry + "/eva/" + image_name
            if image_name == "kubectl" and not (tls and ingress_enabled and create):
                continue
            expected_repo = original["repository"] if online else registry + "/eva/" + image_name
            assert expected_repo + ":" + str(original.get("tag") or versions["iam"]) in iam_images, image_name
        iam_config = one(iam_resources, "ConfigMap", "eva-iam")["data"]
        app_config = one(app_resources, "ConfigMap", "eva-app-env")["data"]
        expected_url = (options["public_url"].rstrip("/") if "public_url" in options else
                        case["protocol"] + "://" + site["iam"]["config"]["host"] +
                        (":" + options["public_port"] if "public_port" in options else "") +
                        site["iam"]["ingress"]["path"].rstrip("/"))
        assert snapshot["sso_base_url"] == expected_url
        assert iam_config["KC_HOSTNAME_ADMIN_URL"] == expected_url
        assert iam_config["ISSUER"] == expected_url + "/realms/eva-iam"
        assert iam_config["JWKS_URL"] == expected_url + "/realms/eva-iam/protocol/openid-connect/certs"
        assert iam_config["KC_HOSTNAME_STRICT_HTTPS"] == str(tls).lower()
        assert app_config["SSO_BASE_URL"] == expected_url
        assert app_config["SSO_ADMIN_CLIENT_SECRET"] == "fixture-runtime-secret"
        assert app_config["BACKEND_SECURE"] == str(tls).lower()
        assert "tls" not in app_effective["app"].get("sso", {}).get("redis", {}), "Ingress must not force App Redis TLS"
        patch = one(iam_resources, "Job", "-realm-patch")["spec"]["template"]["spec"]["containers"][0]
        patch_env = {entry["name"]: entry.get("value") for entry in patch["env"]}
        assert patch_env["SSL_REQUIRED"] == ("external" if tls else "none")
        assert patch_env["CLEAR_STRICT_TRANSPORT_SECURITY"] == str(not tls).lower()
        if "public_port" in options:
            assert iam_role["keycloak"]["realmPatch"]["evaApp"]["redirectUris"] == [
                case["protocol"] + "://" + site["iam"]["config"]["host"] + ":32010/*"]
        redis_tls = options.get("redis_tls", tls)
        assert iam_role["redis"]["tls"]["enabled"] == options.get("redis_tls", False)
        redis_conf = one(iam_resources, "ConfigMap", "-redis")["data"]["redis.conf"]
        assert ("tls-port " in redis_conf) == redis_tls
        redis_service = one(iam_resources, "Service", "-redis-external")["spec"]["ports"][0]
        assert redis_service["nodePort"] == 32079
        assert redis_service["port"] == (6380 if redis_tls else 6379)
        assert redis_service["targetPort"] == ("redis-tls" if redis_tls else "redis")
        redis_pod = one(iam_resources, "Deployment", "-redis")["spec"]["template"]["spec"]
        assert any(v["name"] == "tls" for v in redis_pod["volumes"]) == redis_tls
        print(f"[PASS] {name}: images, ingress/TLS, runtime URL/SSO and independent Redis", flush=True)
    except Exception as error:
        raise AssertionError(f"release contract fixture {name}: {error}") from error
print(f"[PASS] App {versions['app']} / IAM {versions['iam']}: {len(cases)} offline render contracts + 4 boundary cases", flush=True)
PY_CONTRACT