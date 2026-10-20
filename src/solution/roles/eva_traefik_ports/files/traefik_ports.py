#!/usr/bin/env python3
"""Conservative k3s HelmChartConfig listener reconciliation and Helm post-rendering.

Only local bundled charts, the default LoadBalancer Service and k3s ServiceLB
are supported. No cluster mutation is made for DNS/disabled ingress. Failures do
not roll back over concurrent operator changes; the original private snapshot is
reported instead. All Kubernetes writes are create-only or resourceVersion CAS.
Bootstrap requires Helm history and exact Helm ownership on both Ingress and
endpoint ConfigMap. Argo-only installations without Helm history are deliberately
refused; hand off ownership/history before using automatic listener migration.
"""

import argparse
import base64
import copy
import io
import ipaddress
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile
import time
from urllib.parse import urlsplit

import yaml


STATE = "eva.spectrabrain.com/traefik-public-endpoints"
ENTRY = "traefik.ingress.kubernetes.io/router.entrypoints"
TLS = "traefik.ingress.kubernetes.io/router.tls"
RESOURCE = "helmchartconfigs.helm.cattle.io"
CHART = "helmcharts.helm.cattle.io"


class Unsafe(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise Unsafe(message)


def merge(left, right):
    result = copy.deepcopy(left)
    for key, value in right.items():
        result[key] = merge(result[key], value) if isinstance(result.get(key), dict) and isinstance(value, dict) else copy.deepcopy(value)
    return result


def documents(text):
    return [item for item in yaml.safe_load_all(text) if item]


def is_ip(host):
    try:
        ipaddress.ip_address(host)
        return True
    except ValueError:
        return False


def port_number(value):
    require(str(value).isdigit() and 0 < int(value) < 65536, "Rendered public endpoint has no valid public port (not service.port)")
    return int(value)


def endpoint(objects, component, namespace, release):
    ingresses = [obj for obj in objects if obj.get("kind") == "Ingress"]
    if not ingresses:
        return None
    key = "BACKEND_HOST" if component == "app" else "ISSUER"
    maps = [obj.get("data", {}) for obj in objects if obj.get("kind") == "ConfigMap" and key in obj.get("data", {})]
    require(len(maps) == 1, "Cannot uniquely resolve public endpoint from rendered ConfigMap")
    data = maps[0]
    if component == "app":
        host = data["BACKEND_HOST"]
        if not is_ip(host):
            return None
        secure = str(data.get("BACKEND_SECURE", "")).lower()
        require(secure in ("true", "false"), "Invalid rendered BACKEND_SECURE")
        scheme = "https" if secure == "true" else "http"
        public_port = data.get("BACKEND_SECURE_PORT" if scheme == "https" else "BACKEND_PUBLIC_PORT")
    else:
        url = urlsplit(data["ISSUER"])
        host, scheme = url.hostname, url.scheme
        if not is_ip(host):
            return None
        require(scheme in ("http", "https") and host and not url.username and not url.password, "Invalid rendered IAM ISSUER")
        public_port = url.port or (443 if scheme == "https" else 80)
        admin = data.get("KC_HOSTNAME_ADMIN_URL", "")
        require(not admin or data["ISSUER"].startswith(admin.rstrip("/") + "/realms/"), "IAM public URL and ISSUER disagree")
    require(len(ingresses) == 1, "Multiple rendered ingresses are unsupported for IP public listener management")
    ingress = ingresses[0]
    spec = ingress.get("spec", {})
    annotations = ingress.get("metadata", {}).get("annotations", {})
    require(spec.get("ingressClassName", annotations.get("kubernetes.io/ingress.class", "traefik")) == "traefik", "IP ingress does not use Traefik")
    require(bool(spec.get("tls")) == (scheme == "https"), "Rendered public scheme and Ingress TLS disagree")
    require(all(not rule.get("host") for rule in spec.get("rules", [])), "IP endpoint must use hostless Ingress rules")
    paths = [path["path"] for rule in spec.get("rules", []) for path in rule.get("http", {}).get("paths", [])]
    require(paths, "IP ingress has no HTTP paths")
    return {"component": component, "namespace": namespace, "release": release, "host": host,
            "scheme": scheme, "port": port_number(public_port), "paths": paths,
            "ingress": ingress["metadata"]["name"]}


def isolate(objects, selected):
    result = copy.deepcopy(objects)
    found = 0
    for obj in result:
        if obj.get("kind") == "Ingress" and obj["metadata"]["name"] == selected["ingress"]:
            annotations = obj["metadata"].setdefault("annotations", {})
            annotations[ENTRY] = selected["entrypoint"]
            annotations[TLS] = str(selected["scheme"] == "https").lower()
            found += 1
    require(found == 1, "Post-renderer cannot find the verified release ingress")
    return result


def tls_job_input(objects, ingress_name):
    jobs = [obj for obj in objects if obj.get("kind") == "Job" and obj.get("metadata", {}).get("name") == ingress_name + "-tls-job"]
    require(len(jobs) <= 1, "Ambiguous rendered TLS certificate Job")
    host_path = ""
    if jobs:
        for volume in jobs[0].get("spec", {}).get("template", {}).get("spec", {}).get("volumes", []):
            if volume.get("name") == "tls-certs":
                host_path = volume.get("hostPath", {}).get("path", "")
    return {"tls_job_enabled": bool(jobs), "tls_host_path": host_path}


def enabled(port):
    expose = port.get("expose")
    if isinstance(expose, bool):
        return expose
    if isinstance(expose, dict) and isinstance(expose.get("default"), bool):
        require(not any(value for name, value in expose.items() if name != "default"), "Multiple Traefik Services are unsupported")
        return expose["default"]
    raise Unsafe("Unknown installed Traefik ports.expose schema")


def set_exposure(port, value, modern):
    if modern:
        port.setdefault("expose", {})["default"] = value
    else:
        port["expose"] = value


def plan(defaults, chart, config, endpoints, unrelated, preserve_primary=False):
    """Preserve the entire config, modifying only owned port leaves + state annotation."""
    for source in (chart, config or {}):
        spec = source.get("spec", {})
        require(not spec.get("valuesSecrets"), "Traefik valuesSecrets override cannot be safely reconciled")
        require(not any(re.match(r"^(ports|service|hostNetwork|additionalArguments|deployment|providers)(\.|\[|$)", key) for key in spec.get("set", {})),
                "Traefik spec.set overrides managed listener configuration")
    base = yaml.safe_load(chart.get("spec", {}).get("valuesContent", "")) or {}
    original = yaml.safe_load((config or {}).get("spec", {}).get("valuesContent", "")) or {}
    require(isinstance(defaults, dict) and isinstance(base, dict) and isinstance(original, dict), "Traefik values must be mappings")
    effective = merge(merge(defaults, base), original)
    ports = effective.get("ports", {})
    require("web" in ports and "websecure" in ports, "Unknown installed Traefik chart port schema")
    modern = isinstance(defaults.get("ports", {}).get("web", {}).get("expose"), dict)
    enabled(defaults.get("ports", {}).get("web", {}))
    require(effective.get("service", {}).get("type", "LoadBalancer") == "LoadBalancer" and effective.get("service", {}).get("enabled", True), "Only the bundled default LoadBalancer Service is supported")
    require(not effective.get("hostNetwork") and not effective.get("deployment", {}).get("hostPort"), "Traefik hostNetwork/hostPort is unsupported")
    require(not effective.get("service", {}).get("additionalServices"), "Multiple Traefik Services are unsupported")
    require(not effective.get("providers", {}).get("kubernetesGateway", {}).get("enabled"), "Gateway API listeners must be managed externally")
    for item in ports.values():
        require(not item.get("hostPort") and not item.get("nodePort") and not item.get("http3", {}).get("enabled"), "Traefik hostPort/nodePort/HTTP3 overrides are unsupported")
    require(not ports["web"].get("redirectTo") and not ports["web"].get("redirections") and not ports["web"].get("http", {}).get("redirections"), "Existing HTTP entrypoint redirects must be managed externally")
    require(not any("entrypoints." in str(arg).lower() or "entrypoints=" in str(arg).lower() for arg in effective.get("additionalArguments", [])), "Traefik additionalArguments override entrypoints")
    updated = copy.deepcopy(original)
    old_state = json.loads((config or {}).get("metadata", {}).get("annotations", {}).get(STATE, "{}"))
    owned_entries = {item.get("entrypoint") for item in old_state.values() if isinstance(item, dict)}
    overrides = updated.setdefault("ports", {})
    selected = copy.deepcopy(endpoints)
    endpoint_list = list(selected.values())
    for index, left in enumerate(endpoint_list):
        for right in endpoint_list[index + 1:]:
            require(left["port"] != right["port"] or left["scheme"] == right["scheme"] and not set(left["paths"]) & set(right["paths"]),
                    "Hostless EVA ingresses have conflicting public port/path routes")
    assigned = {}
    for scheme, primary in (("http", "web"), ("https", "websecure")):
        wanted = sorted({item["port"] for item in selected.values() if item["scheme"] == scheme})
        current = port_number(ports[primary].get("exposedPort"))
        if unrelated:
            require(not wanted or wanted == [current], "Unrelated Traefik ingresses prevent relocation of shared public listeners")
        # Keep the old primary when it is still needed; otherwise use a stable minimum.
        # Never repurpose an existing listener while migrating live routers:
        # pre-HCC CAS patches must not temporarily route one release's port to
        # another. New ports use previously unbound, dedicated entrypoints.
        primary_port = current if preserve_primary or current in wanted else (wanted[0] if wanted else current)
        expose = (primary_port in wanted) or bool(unrelated)
        primary_override = overrides.setdefault(primary, {})
        primary_override["exposedPort"] = primary_port
        set_exposure(primary_override, expose, modern)
        for number in wanted:
            name = primary if number == primary_port else f"eva-{'s' if scheme == 'https' else 'h'}-{number}"
            if name != primary:
                require(name not in ports or name in owned_entries, "Foreign Traefik entrypoint collision")
                # Container port equals public port for additional named listeners.
                overrides[name] = merge(overrides.get(name, {}), {"port": number, "exposedPort": number, "protocol": "TCP"})
                set_exposure(overrides[name], True, modern)
            assigned[(scheme, number)] = name
    for item in selected.values():
        item["entrypoint"] = assigned[(item["scheme"], item["port"])]
    # Disable obsolete owned extra entrypoints, leaving foreign entries untouched.
    owned_names = {item["entrypoint"] for item in selected.values()}
    for name in original.get("ports", {}):
        if name in owned_entries and re.fullmatch(r"eva-[hs]-\d+", name) and name not in owned_names:
            set_exposure(overrides.setdefault(name, {}), False, modern)
    resulting = merge(effective, updated)
    expected = {}
    bound = {}
    for name, item in resulting["ports"].items():
        if enabled(item):
            number = port_number(item.get("exposedPort", item.get("port")))
            require(number not in bound, "Conflicting Traefik exposed ports")
            bound[number] = name
            expected[name] = {"port": number, "targetPort": item.get("targetPort", name), "protocol": item.get("protocol", "TCP")}
        container_port = item.get("port")
        for other_name, other in resulting["ports"].items():
            require(other_name == name or not container_port or container_port != other.get("port"), "Conflicting Traefik container entrypoint ports")
    # An unmanaged catch-all would accept traffic on every new entrypoint.
    require(not unrelated or not any(name.startswith("eva-") for name in expected), "Unrelated ingress may catch traffic on EVA entrypoints")
    return updated, selected, expected


class Cluster:
    def run(self, args, stdin=None):
        result = subprocess.run(args, input=stdin, text=True, capture_output=True, timeout=90)
        require(result.returncode == 0, "Helm/kubectl command failed (private arguments and output withheld)")
        return result.stdout

    def get(self, resource, namespace="kube-system", name=None, optional=False):
        args = ["kubectl", "get", resource]
        if name:
            args.append(name)
        args += ["-A"] if namespace is None else ["-n", namespace]
        if optional:
            args.append("--ignore-not-found=true")
        text = self.run(args + ["-o", "json"])
        return json.loads(text) if text.strip() else None


def chart_defaults(chart):
    spec = chart.get("spec", {})
    require(chart.get("metadata", {}).get("name") == "traefik" and chart.get("metadata", {}).get("namespace") == "kube-system", "Not the bundled k3s Traefik HelmChart")
    require(spec.get("targetNamespace", "kube-system") == "kube-system", "Unsupported Traefik release namespace")
    if spec.get("chartContent"):
        archive = io.BytesIO(base64.b64decode(spec["chartContent"], validate=True))
    else:
        match = re.fullmatch(r"(?:https?://%\{KUBERNETES_API\}%/static/charts/|/var/lib/rancher/k3s/server/static/charts/)(traefik-[\w.+-]+\.tgz)", spec.get("chart", ""))
        require(match, "Cannot inspect bundled local Traefik chart; remote/custom charts are unsupported")
        archive = Path("/var/lib/rancher/k3s/server/static/charts") / match[1]
        require(archive.is_file(), "Bundled Traefik chart archive is unavailable on this node")
    with tarfile.open(fileobj=archive if isinstance(archive, io.BytesIO) else None, name=str(archive) if isinstance(archive, Path) else None) as tar:
        members = [member for member in tar.getmembers() if member.name == "traefik/values.yaml"]
        require(len(members) == 1, "Unknown bundled Traefik archive layout")
        return yaml.safe_load(tar.extractfile(members[0]).read())


def identity(ingress):
    meta = ingress.get("metadata", {})
    annotations = meta.get("annotations", {})
    return meta.get("namespace", "default") + "/" + annotations.get("meta.helm.sh/release-name", "")


def helm_owned(obj, namespace, release):
    meta = obj.get("metadata", {})
    annotations = meta.get("annotations", {})
    return (meta.get("namespace") == namespace
            and annotations.get("meta.helm.sh/release-name") == release
            and annotations.get("meta.helm.sh/release-namespace") == namespace
            and meta.get("labels", {}).get("app.kubernetes.io/managed-by") == "Helm")


def endpoint_facts(selected):
    return {field: selected.get(field) for field in
            ("component", "namespace", "release", "host", "scheme", "port", "paths", "ingress")}


def verified_installed(cluster, ingress, maps, component, namespace, release):
    require(release and helm_owned(ingress, namespace, release)
            and len(maps) == 1 and helm_owned(maps[0], namespace, release),
            "EVA Ingress/ConfigMap Helm ownership mismatch; unknown ownership cannot be migrated")
    meta = ingress["metadata"]
    require(meta.get("uid") and meta.get("resourceVersion") and not meta.get("deletionTimestamp"),
            "EVA Ingress has no stable UID/resourceVersion or is terminating")
    # Helm annotations alone are not history: an Argo-only release must not be
    # adopted here. Never include a Helm error/manifest in diagnostics.
    try:
        manifest = documents(cluster.run(["helm", "get", "manifest", release, "-n", namespace]))
    except Unsafe as exc:
        raise Unsafe("Verified Helm history is required for existing EVA Ingress migration; Argo-only/no-history ownership needs explicit handoff") from exc
    installed = [obj for obj in manifest if obj.get("kind") in ("Ingress", "ConfigMap")
             and obj.get("metadata", {}).get("namespace", namespace) == namespace
             and (obj.get("kind"), obj.get("metadata", {}).get("name")) in
             (("Ingress", meta["name"]), ("ConfigMap", maps[0]["metadata"]["name"]))]
    require(len(installed) == 2, "Helm history does not contain the exact EVA Ingress and endpoint ConfigMap")
    live = endpoint(maps + [ingress], component, namespace, release)
    recorded = endpoint(installed, component, namespace, release)
    require(live and recorded and endpoint_facts(live) == endpoint_facts(recorded),
            "Installed EVA public endpoint differs from Helm history; manual review required")
    return live


def managed_router(ingress, prior):
    annotations = ingress["metadata"].get("annotations", {})
    entry = annotations.get(ENTRY, "")
    allowed = {"web", "websecure"}
    if prior:
        previous = prior.get("entrypoint", "")
        require(previous in allowed or re.fullmatch(r"eva-[hs]-\d+", previous),
            "Invalid previous EVA managed entrypoint")
        allowed.add(previous)
    require(not entry or all(part.strip() in allowed for part in entry.split(",")),
            "Existing EVA Ingress has a foreign explicit entrypoint; automatic override refused")
    require(annotations.get(TLS, "false") in ("true", "false"),
            "Existing EVA Ingress has an unknown explicit router TLS setting")


def discover(cluster, current, old_state):
    """Verify installed endpoint facts; legacy missing router annotations are valid."""
    selected = {}
    unrelated = []
    originals = {}
    evidence = {}
    current_key = current["namespace"] + "/" + current["release"]
    ingresses = cluster.get("ingresses", None)["items"]
    for ingress in ingresses:
        spec = ingress.get("spec", {})
        annotations = ingress.get("metadata", {}).get("annotations", {})
        if spec.get("ingressClassName", annotations.get("kubernetes.io/ingress.class", "traefik")) != "traefik":
            continue
        key = identity(ingress)
        namespace = ingress["metadata"]["namespace"]
        name = ingress["metadata"]["name"]
        release = annotations.get("meta.helm.sh/release-name")
        is_current = namespace == current["namespace"] and name == current["ingress"]
        objects = cluster.get("configmaps", namespace)["items"]
        candidates = [obj for obj in objects if obj.get("metadata", {}).get("name") in (name, name + "-env")]
        component = "app" if any("BACKEND_HOST" in obj.get("data", {}) for obj in candidates) else "iam"
        candidates = [obj for obj in candidates if ("BACKEND_HOST" if component == "app" else "ISSUER") in obj.get("data", {})]
        other = endpoint(candidates + [ingress], component, namespace, release) if candidates else None
        if not other:
            require(not is_current, "Existing current EVA Ingress cannot be verified from its installed public endpoint")
            unrelated.append(ingress)
            continue
        require(not is_current or key == current_key, "Current EVA Ingress has unknown Helm ownership")
        other = verified_installed(cluster, ingress, candidates, component, namespace, release)
        require(key not in originals, "Multiple live ingresses for another EVA release")
        prior = old_state.get(key)
        if prior and key != current_key:
            require(isinstance(prior, dict) and endpoint_facts(prior) == endpoint_facts(other),
                    "Installed EVA endpoint changed outside listener management; reconcile its installed state first")
        managed_router(ingress, prior)
        originals[key] = copy.deepcopy(ingress)
        evidence[key] = copy.deepcopy(candidates[0])
        selected[key] = other
    # Discover installed CRDs rather than treating an absent API as an empty list.
    for group in ("traefik.io", "traefik.containo.us"):
        resources = cluster.run(["kubectl", "api-resources", "--api-group=" + group, "-o", "name"]).splitlines()
        for resource in resources:
            if resource.split(".")[0] in ("ingressroutes", "ingressroutetcps", "ingressrouteudps"):
                unrelated += cluster.get(resource, None)["items"]
    selected[current_key] = current
    return selected, unrelated, originals, evidence


def snapshot(root, chart, config, ingresses):
    root = Path(root)
    require(root.is_absolute() and ".." not in root.parts and all(32 <= ord(char) < 127 for char in str(root)),
            "Traefik backup root must be a safe absolute path")
    require(not any(path.is_symlink() for path in (root, *root.parents)), "Traefik backup path contains a symlink")
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    require(root.stat().st_mode & 0o077 == 0 and not root.is_symlink(), "Traefik backup root must be a private non-symlink directory")
    fd, name = tempfile.mkstemp(prefix="before-", suffix=".json", dir=root)
    with os.fdopen(fd, "w") as stream:
        json.dump({"helmchart": chart, "helmchartconfig": config, "ingresses": list(ingresses.values())}, stream)
        stream.flush()
        os.fsync(stream.fileno())
    descriptor = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    return name


def patch_router(cluster, ingress, selected):
    """CAS only the two router leaves; never replace annotations/spec or roll back."""
    meta = ingress["metadata"]
    patch = [{"op": "test", "path": "/metadata/resourceVersion", "value": meta["resourceVersion"]},
             {"op": "test", "path": "/metadata/uid", "value": meta["uid"]}]
    for key, value in ((ENTRY, selected["entrypoint"]), (TLS, str(selected["scheme"] == "https").lower())):
        if meta["annotations"].get(key) != value:
            patch.append({"op": "add", "path": "/metadata/annotations/" + key.replace("~", "~0").replace("/", "~1"), "value": value})
    if len(patch) == 2:
        return False
    cluster.run(["kubectl", "patch", "ingresses", meta["name"], "-n", meta["namespace"], "--type=json", "--patch-file=/dev/stdin"], json.dumps(patch))
    return True


def verify_endpoint_maps(cluster, evidence):
    for obj in evidence.values():
        meta = obj["metadata"]
        current = [item for item in cluster.get("configmaps", meta["namespace"])["items"]
                   if item.get("metadata", {}).get("name") == meta["name"]]
        require(current == [obj], "EVA endpoint ConfigMap changed during router migration; stop without rollback")


def verify_routers(cluster, originals, selected):
    live = {(obj["metadata"]["namespace"], obj["metadata"]["name"]): obj
            for obj in cluster.get("ingresses", None)["items"]}
    for key, original in originals.items():
        meta = original["metadata"]
        current = copy.deepcopy(live.get((meta["namespace"], meta["name"]), {}))
        target = isolate([original], selected[key])[0]
        # Only the apiserver's write bookkeeping may differ after our CAS.
        for obj in (current, target):
            obj.get("metadata", {}).pop("resourceVersion", None)
            obj.get("metadata", {}).pop("managedFields", None)
        require(current == target, "EVA Ingress changed during router migration; stop without rollback")


def write_config(cluster, config, values, selected, backup):
    content = yaml.safe_dump(values, sort_keys=False)
    state = json.dumps(selected, sort_keys=True, separators=(",", ":"))
    if config:
        annotations = config.get("metadata", {}).get("annotations", {})
        before = yaml.safe_load(config.get("spec", {}).get("valuesContent", "")) or {}
        if before == values and annotations.get(STATE) == state:
            return False, None
    try:
        if config:
            patch = [{"op": "test", "path": "/metadata/resourceVersion", "value": config["metadata"]["resourceVersion"]},
                     {"op": "add", "path": "/spec/valuesContent", "value": content}]
            if "annotations" not in config["metadata"]:
                patch.append({"op": "add", "path": "/metadata/annotations", "value": {STATE: state}})
            else:
                patch.append({"op": "add", "path": "/metadata/annotations/" + STATE.replace("~", "~0").replace("/", "~1"), "value": state})
            cluster.run(["kubectl", "patch", RESOURCE, "traefik", "-n", "kube-system", "--type=json", "--patch-file=/dev/stdin"], json.dumps(patch))
        else:
            resource = {"apiVersion": "helm.cattle.io/v1", "kind": "HelmChartConfig", "metadata": {"name": "traefik", "namespace": "kube-system", "annotations": {STATE: state}}, "spec": {"valuesContent": content}}
            cluster.run(["kubectl", "create", "-f", "-"], json.dumps(resource))
    except Unsafe as exc:
        raise Unsafe(f"HelmChartConfig CAS/create conflict; no adoption or blind retry. Original backup: {backup}") from exc
    return True, backup


def check_service(service):
    require(service and service.get("spec", {}).get("type") == "LoadBalancer", "Traefik is not a LoadBalancer Service; NodePort/external LB unsupported")
    annotations = service.get("metadata", {}).get("annotations", {})
    require(annotations.get("meta.helm.sh/release-name") == "traefik" and annotations.get("meta.helm.sh/release-namespace") == "kube-system", "Foreign Traefik Service ownership")
    require(not service["spec"].get("loadBalancerClass") and not service["spec"].get("externalIPs") and not service["spec"].get("loadBalancerIP"), "External LoadBalancer configuration is unsupported")


def ready(cluster, expected, previous_ports, addresses):
    service = cluster.get("services", name="traefik")
    check_service(service)
    actual = {item["name"]: {key: item.get(key, "TCP" if key == "protocol" else None) for key in ("port", "targetPort", "protocol")} for item in service["spec"]["ports"]}
    if actual != expected:
        return False
    pods = cluster.get("pods", None)["items"]
    wanted = {item["port"] for item in expected.values()}
    released = set(previous_ports) - wanted
    daemonsets = cluster.get("daemonsets")["items"]
    lbs = [item for item in daemonsets if item.get("metadata", {}).get("labels", {}).get("svccontroller.k3s.cattle.io/svcname") == "traefik"
           and item.get("metadata", {}).get("labels", {}).get("svccontroller.k3s.cattle.io/svcnamespace") == "kube-system"]
    if len(lbs) != 1:
        return False
    lb = lbs[0]
    if lb["metadata"].get("name") != "svclb-traefik-" + service["metadata"]["uid"][:8]:
        return False
    lb_status = lb.get("status", {})
    count = lb_status.get("desiredNumberScheduled", 0)
    if not count or lb_status.get("numberReady") != count or lb_status.get("updatedNumberScheduled") != count:
        return False
    if lb_status.get("observedGeneration", 0) < lb["metadata"].get("generation", 1):
        return False
    lb_ports = {port["hostPort"] for container in lb["spec"]["template"]["spec"]["containers"] for port in container.get("ports", []) if port.get("hostPort")}
    if lb_ports != wanted:
        return False
    lb_pods = []
    for pod in pods:
        meta = pod.get("metadata", {})
        labels = meta.get("labels", {})
        own = (meta.get("namespace") == "kube-system" and labels.get("svccontroller.k3s.cattle.io/svcname") == "traefik"
               and labels.get("svccontroller.k3s.cattle.io/svcnamespace") == "kube-system")
        hostports = {item["hostPort"] for container in pod.get("spec", {}).get("containers", []) + pod.get("spec", {}).get("initContainers", []) for item in container.get("ports", []) if item.get("hostPort")}
        # Terminating pods still reserve hostPorts until actually deleted.
        if own:
            if hostports != wanted or meta.get("deletionTimestamp"):
                return False
            if not any(owner.get("uid") == lb["metadata"]["uid"] and owner.get("kind") == "DaemonSet" for owner in meta.get("ownerReferences", [])):
                return False
            lb_pods.append(pod)
        if hostports & released:
            return False
    if len(lb_pods) != count:
        return False
    if not all(any(c.get("type") == "Ready" and c.get("status") == "True" for c in pod.get("status", {}).get("conditions", [])) for pod in lb_pods):
        return False
    deployments = cluster.get("deployments")["items"]
    traefik = [item for item in deployments if item.get("metadata", {}).get("name") == "traefik"]
    if len(traefik) != 1:
        return False
    deployment = traefik[0]
    if not helm_owned(deployment, "kube-system", "traefik") or not deployment["metadata"].get("uid"):
        return False
    status = deployment.get("status", {})
    if status.get("observedGeneration", 0) < deployment["metadata"].get("generation", 1):
        return False
    wanted_replicas = deployment.get("spec", {}).get("replicas", 1)
    if (not wanted_replicas or status.get("updatedReplicas", 0) != wanted_replicas
            or status.get("availableReplicas", 0) != wanted_replicas
            or status.get("readyReplicas", 0) != wanted_replicas
            or status.get("replicas", 0) != wanted_replicas):
        return False
    containers = [item for item in deployment["spec"]["template"]["spec"]["containers"] if item.get("name") == "traefik"]
    if len(containers) != 1:
        return False
    args = containers[0].get("args", [])
    def exact_addresses(arguments):
        found = {}
        for arg in arguments:
            if arg.lower().startswith("--entrypoints.") and ".address" in arg.lower():
                match = re.fullmatch(r"--entrypoints\.([\w-]+)\.address=:(\d+)(?:/tcp)?", arg.lower())
                if not match or match[1] in found:
                    return False
                found[match[1]] = int(match[2])
        return found == addresses
    if not exact_addresses(args):
        return False
    replica_sets = cluster.get("replicasets")["items"]
    own_sets = {item["metadata"]["uid"] for item in replica_sets
                if any(owner.get("kind") == "Deployment" and owner.get("uid") == deployment["metadata"]["uid"]
                       and owner.get("controller") is True for owner in item["metadata"].get("ownerReferences", []))}
    own_pods = [pod for pod in pods if pod.get("metadata", {}).get("namespace") == "kube-system"
                and any(owner.get("kind") == "ReplicaSet" and owner.get("uid") in own_sets
                        and owner.get("controller") is True for owner in pod["metadata"].get("ownerReferences", []))]
    if len(own_pods) != wanted_replicas:
        return False
    for pod in own_pods:
        if pod["metadata"].get("deletionTimestamp") or not any(c.get("type") == "Ready" and c.get("status") == "True" for c in pod.get("status", {}).get("conditions", [])):
            return False
        current = [item for item in pod.get("spec", {}).get("containers", []) if item.get("name") == "traefik"]
        if len(current) != 1 or current[0].get("args", []) != args or not exact_addresses(current[0].get("args", [])):
            return False
    return True


def reconcile(cluster, request, planned, defaults=None):
    timeout = float(request.get("timeout", 300))
    require(0 <= timeout <= 86400, "Invalid public listener convergence timeout")
    chart = cluster.get(CHART, name="traefik", optional=True)
    require(chart, "Bundled k3s Traefik HelmChart is absent")
    defaults = chart_defaults(chart) if defaults is None else defaults
    config = cluster.get(RESOURCE, name="traefik", optional=True)
    annotations = (config or {}).get("metadata", {}).get("annotations", {})
    old_state = json.loads(annotations.get(STATE, "{}"))
    require(isinstance(old_state, dict), "Invalid EVA endpoint ownership annotation")
    selected, unrelated, originals, evidence = discover(cluster, planned, old_state)
    values, selected, expected = plan(defaults, chart, config, selected, unrelated, preserve_primary=bool(originals))
    effective = merge(merge(defaults, yaml.safe_load(chart.get("spec", {}).get("valuesContent", "")) or {}), values)
    # Even non-exposed entrypoints (e.g. the dashboard) exist in Traefik args.
    addresses = {name: item["port"] for name, item in effective["ports"].items()}
    current_key = planned["namespace"] + "/" + planned["release"]
    service = cluster.get("services", name="traefik")
    check_service(service)
    old_ports = [item["port"] for item in service["spec"]["ports"]]
    initial_pods = cluster.get("pods", None)["items"]
    require(any(pod.get("metadata", {}).get("labels", {}).get("svccontroller.k3s.cattle.io/svcname") == "traefik" for pod in initial_pods), "k3s ServiceLB pods absent; external LB is unsupported")
    desired_ports = {item["port"] for item in expected.values()}
    for pod in initial_pods:
        labels = pod.get("metadata", {}).get("labels", {})
        if pod.get("metadata", {}).get("namespace") == "kube-system" and labels.get("svccontroller.k3s.cattle.io/svcname") == "traefik" and labels.get("svccontroller.k3s.cattle.io/svcnamespace") == "kube-system":
            continue
        occupied = {port.get("hostPort") for container in pod.get("spec", {}).get("containers", []) for port in container.get("ports", [])}
        require(not occupied & desired_ports, "Requested public port conflicts with another workload hostPort")
    for other_service in cluster.get("services", None)["items"]:
        meta = other_service.get("metadata", {})
        if meta.get("namespace") == "kube-system" and meta.get("name") == "traefik":
            continue
        spec = other_service.get("spec", {})
        occupied = {item.get("nodePort") for item in spec.get("ports", [])}
        if spec.get("type") == "LoadBalancer":
            occupied |= {item.get("port") for item in spec.get("ports", [])}
        require(not occupied & desired_ports, "Requested public port conflicts with another Service NodePort/LoadBalancer")
    before_chart = cluster.get(CHART, name="traefik")
    require(before_chart["metadata"]["resourceVersion"] == chart["metadata"]["resourceVersion"], "Traefik HelmChart changed during assessment; retry")
    # One durable private snapshot contains ALL originals, before the first
    # router/HCC mutation. No rollback: a failed CAS may mean an operator won.
    backup = snapshot(request["backup_root"], chart, config, originals)
    try:
        verify_endpoint_maps(cluster, evidence)
        changed = False
        for key, ingress in originals.items():
            changed = patch_router(cluster, ingress, selected[key]) or changed
        verify_routers(cluster, originals, selected)
        verify_endpoint_maps(cluster, evidence)
        before_chart = cluster.get(CHART, name="traefik")
        require(before_chart["metadata"]["resourceVersion"] == chart["metadata"]["resourceVersion"],
                "Traefik HelmChart changed before listener mutation")
        config_changed, _ = write_config(cluster, config, values, selected, backup)
        changed = config_changed or changed
    except Exception as exc:
        raise Unsafe(f"EVA router/HCC CAS migration stopped; no blind retry or rollback. Review private original backup: {backup}") from exc
    try:
        return converge(cluster, chart, values, selected, originals, evidence, expected, old_ports,
                        addresses, current_key, changed, backup, timeout)
    except Unsafe as exc:
        raise Unsafe(f"{exc}. Original backup: {backup}") from exc
    except Exception as exc:
        raise Unsafe(f"Traefik convergence stopped on unexpected private input/controller error; no rollback. Original backup: {backup}") from exc


def converge(cluster, chart, values, selected, originals, evidence, expected, old_ports,
             addresses, current_key, changed, backup, timeout):
    deadline = time.monotonic() + timeout
    while True:
        observed = cluster.get(RESOURCE, name="traefik")
        require((yaml.safe_load(observed.get("spec", {}).get("valuesContent", "")) or {}) == values
            and observed.get("metadata", {}).get("annotations", {}).get(STATE) == json.dumps(selected, sort_keys=True, separators=(",", ":")),
            f"HelmChartConfig changed during controller convergence; stop and reassess. Original backup: {backup}")
        chart_now = cluster.get(CHART, name="traefik")
        require(chart_now.get("spec") == chart.get("spec"), "HelmChart spec changed during convergence")
        verify_routers(cluster, originals, selected)
        verify_endpoint_maps(cluster, evidence)
        conditions = chart_now.get("status", {}).get("conditions", [])
        require(not any(c.get("type") == "Failed" and c.get("status") == "True" for c in conditions), f"Traefik Helm controller failed; original backup: {backup}")
        if ready(cluster, expected, old_ports, addresses):
            verify_routers(cluster, originals, selected)
            verify_endpoint_maps(cluster, evidence)
            return {"changed": changed, "isolated": True, "endpoint": selected[current_key], "backup": backup}
        require(time.monotonic() < deadline, f"Timeout waiting for exact Traefik Service/Deployment and ServiceLB hostPort release; original backup: {backup}")
        time.sleep(2)


def private_write(path, text):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, "w") as stream:
        stream.write(text)


def prepare_renderer(cluster, directory):
    """Helm 3 accepts executables; Helm 4 requires a typed subprocess plugin."""
    version = cluster.run(["helm", "version", "--short"]).strip()
    if version.startswith("v3."):
        return str(directory / "traefik-post-renderer"), {}
    require(version.startswith("v4."), "Unsupported Helm post-renderer API version")
    plugin = directory / "helm-plugins" / "eva-traefik-ports"
    plugin.mkdir(parents=True, exist_ok=True, mode=0o700)
    definition = {"apiVersion": "v1", "type": "postrenderer/v1", "name": "eva-traefik-ports",
                  "version": "1.0.0", "runtime": "subprocess", "runtimeConfig": {"platformCommand": [
                      {"command": str(directory / "traefik-post-renderer")} ]}}
    private_write(str(plugin / "plugin.yaml"), yaml.safe_dump(definition))
    return "eva-traefik-ports", {"HELM_PLUGINS": str(plugin.parent)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--request")
    parser.add_argument("--post-render", nargs="?")
    args = parser.parse_args()
    if args.post_render:
        selected = json.loads(Path(args.post_render).read_text())
        print(yaml.safe_dump_all(isolate(documents(sys.stdin.read()), selected), sort_keys=False))
        return
    require(args.request, "A private render request is required")
    request = json.loads(Path(args.request).read_text())
    command = ["helm", "template", request["release"], request["chart"], "-n", request["namespace"]]
    if request.get("version"):
        command += ["--version", request["version"]]
    for path in request["values"]:
        command += ["-f", path]
    for value in request["set"]:
        command += ["--set", value]
    cluster = Cluster()
    rendered = cluster.run(command)
    private_write(str(Path(args.request).with_name("traefik-target-manifest.yaml")), rendered)
    planned = endpoint(documents(rendered), request["component"], request["namespace"], request["release"])
    renderer, environment = prepare_renderer(cluster, Path(args.request).parent) if planned else (None, {})
    result = reconcile(cluster, request, planned) if planned else {"changed": False, "isolated": False}
    if result["isolated"]:
        private_write(str(Path(args.request).with_name("traefik-router.json")), json.dumps(result["endpoint"]))
        result.update(renderer=renderer, environment=environment)
        result.update(tls_job_input(documents(rendered), planned["ingress"]))
        if request["component"] == "iam":
            data = next(obj["data"] for obj in documents(rendered) if obj.get("kind") == "ConfigMap" and "ISSUER" in obj.get("data", {}))
            result["iam_base_url"] = data["ISSUER"].rsplit("/realms/", 1)[0]
    print(json.dumps(result))


def cli():
    try:
        main()
    except yaml.YAMLError:
        print("Traefik reconciliation stopped: invalid private YAML input (content withheld).", file=sys.stderr)
        return 1
    except subprocess.TimeoutExpired:
        print("Traefik reconciliation stopped: Helm/kubectl command timed out (private arguments withheld).", file=sys.stderr)
        return 1
    except Unsafe as error:
        print("Traefik public listener reconciliation stopped: " + str(error) +
              ". Opt out with eva_traefik_ports_manage=false only if the operator manages public listeners and router isolation.", file=sys.stderr)
        return 1
    except Exception:
        print("Traefik reconciliation stopped: invalid private input or unexpected error (content and command arguments withheld).", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(cli())
