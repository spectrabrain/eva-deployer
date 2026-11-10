#!/usr/bin/env python3
"""Conservative k3s HelmChartConfig listener reconciliation and Helm post-rendering.

Only local bundled charts, the default LoadBalancer Service and k3s ServiceLB
are supported. No cluster mutation is made for DNS/disabled ingress. All writes
are create-only or UID/resourceVersion CAS. Existing routers require exact Helm
history and ownership. A legacy App NodePort can be bridged using its verified
Service and endpoint ConfigMap history and a private durable receipt. Unrelated
ConfigMap edits are preserved in the receipt. Failed bridge transitions
restore only unchanged writes from that attempt, after the listener releases the
port. Concurrent operator changes and Argo-only/no-history ownership are refused.
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
OWNED = "eva.spectrabrain.com/traefik-owned-entrypoints"
ENTRY = "traefik.ingress.kubernetes.io/router.entrypoints"
TLS = "traefik.ingress.kubernetes.io/router.tls"
RESOURCE = "helmchartconfigs.helm.cattle.io"
CHART = "helmcharts.helm.cattle.io"
LEGACY = "eva.spectrabrain.com/legacy-app-nodeport"
LEGACY_ENDPOINT_KEYS = frozenset({"BACKEND_HOST", "BACKEND_PORT", "BACKEND_PUBLIC_PORT",
                                  "BACKEND_SECURE", "BACKEND_SECURE_PORT"})


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


def check_redirects(port):
    """Validate known redirect shapes and report whether any redirect is configured."""
    active = bool(port.get("redirectTo"))
    require(port.get("redirectTo") is None or isinstance(port["redirectTo"], (str, dict)), "Unknown Traefik redirectTo schema")
    http = port.get("http")
    require(http is None or isinstance(http, dict), "Unknown Traefik HTTP redirect schema")
    # Chart v34 uses port.redirections; v40 uses port.http.redirections.
    # Both render a redirect only within `with redirections.entryPoint`.
    # {entryPoint: {}} is a normal INACTIVE default, but its outer map is truthy.
    for redirections in (port.get("redirections"), (http or {}).get("redirections")):
        require(redirections is None or isinstance(redirections, dict), "Unknown Traefik redirect schema")
        if redirections is None:
            continue
        require(not (redirections.keys() - {"entryPoint"}), "Unknown Traefik redirect schema")
        entry = redirections.get("entryPoint")
        require(entry is None or isinstance(entry, dict), "Unknown Traefik redirect entryPoint schema")
        active = active or bool(entry)
    return active


def without_redirects(port, inherited, override):
    """Remove HCC redirects; tombstone inherited leaves so Helm cannot merge them back."""
    for path in (("redirectTo",), ("redirections", "entryPoint"), ("http", "redirections", "entryPoint")):
        current, lower = port, inherited
        for key in path:
            current = (current or {}).get(key)
            lower = (lower or {}).get(key)
        if not current:
            continue
        target = override
        for key in path[:-1]:
            if not isinstance(target.get(key), dict):
                target[key] = {}
            target = target[key]
        if lower:
            target[path[-1]] = None
        else:
            target.pop(path[-1], None)


def managed_http_entries(selected, owned_entries, unrelated):
    entries = {item["entrypoint"] for item in selected.values() if item["scheme"] == "http"}
    if entries:
        foreign = unrelated_entrypoints(unrelated) or set()
        # Non-exposed entrypoints still render static redirect arguments.
        entries |= {name for name in owned_entries if name and re.fullmatch(r"eva-h-\d+", name) and name not in foreign}
        if not unrelated:
            entries.add("web")
    return entries


def owned_entrypoints(config):
    annotations = (config or {}).get("metadata", {}).get("annotations", {})
    recorded = json.loads(annotations.get(OWNED, "[]"))
    require(isinstance(recorded, list)
            and all(isinstance(name, str) and re.fullmatch(r"eva-[hs]-\d+", name) for name in recorded),
            "Invalid EVA entrypoint ownership annotation")
    state = json.loads(annotations.get(STATE, "{}"))
    require(isinstance(state, dict), "Invalid EVA endpoint ownership annotation")
    return set(recorded) | {item.get("entrypoint") for item in state.values() if isinstance(item, dict)}


def eva_entrypoints(listener_ports):
    """Recognize EVA's reserved listener names even when ownership records are missing."""
    names = set()
    for name, item in listener_ports.items():
        match = re.fullmatch(r"eva-[hs]-(\d+)", name)
        if not match:
            continue
        number = int(match[1])
        if (0 < number < 65536 and str(number) == match[1]
                and item.get("port") == number and item.get("exposedPort", number) == number
                and item.get("protocol", "TCP") == "TCP"
                and item.get("targetPort") in (None, name, number)):
            names.add(name)
    return names


def unrelated_entrypoints(routers):
    """Return explicit router scopes; None means an implicit/unknown catch-all."""
    entries = set()
    for router in routers:
        if router.get("kind") == "Ingress":
            raw = router.get("metadata", {}).get("annotations", {}).get(ENTRY)
            if not isinstance(raw, str):
                return None
            names = [name.strip() for name in raw.split(",")]
        elif router.get("kind") in ("IngressRoute", "IngressRouteTCP", "IngressRouteUDP"):
            names = router.get("spec", {}).get("entryPoints")
            if not isinstance(names, list):
                return None
        else:
            return None
        if not names or not all(isinstance(name, str) and name and name == name.strip() for name in names):
            return None
        entries.update(names)
    return entries


def recoverable_eva_entrypoints(listener_ports, unrelated):
    foreign = unrelated_entrypoints(unrelated)
    if foreign is None or not foreign <= listener_ports.keys():
        return set()
    return eva_entrypoints(listener_ports) - foreign


def ownership_annotation(config, selected, recovered):
    # Endpoint state changes on port/protocol transitions and reinstallations;
    # retain ownership of disabled listeners so later reuse is still authorized.
    names = owned_entrypoints(config) | recovered | {item["entrypoint"] for item in selected.values()}
    return json.dumps(sorted(name for name in names if name and re.fullmatch(r"eva-[hs]-\d+", name)), separators=(",", ":"))


def plan(defaults, chart, config, endpoints, unrelated, preserve_primary=False):
    """Preserve the entire config, modifying only owned port leaves + state annotation."""
    for source in (chart, config or {}):
        spec = source.get("spec", {})
        require(not spec.get("valuesSecrets"), "Traefik valuesSecrets override cannot be safely reconciled")
        require(not any(re.match(r"^(ports|service|hostNetwork|additionalArguments|globalArguments|deployment|providers)(\.|\[|$)", key) for key in spec.get("set", {})),
                "Traefik spec.set overrides managed listener configuration")
    base = yaml.safe_load(chart.get("spec", {}).get("valuesContent", "")) or {}
    original = yaml.safe_load((config or {}).get("spec", {}).get("valuesContent", "")) or {}
    require(isinstance(defaults, dict) and isinstance(base, dict) and isinstance(original, dict), "Traefik values must be mappings")
    inherited = merge(defaults, base)
    effective = merge(inherited, original)
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
        check_redirects(item)
    updated = copy.deepcopy(original)
    foreign = unrelated_entrypoints(unrelated)
    scoped_unrelated = bool(unrelated) and foreign is not None and foreign <= ports.keys()
    protected = foreign if scoped_unrelated else set()
    owned_entries = (owned_entrypoints(config) | recoverable_eva_entrypoints(ports, unrelated)) - protected
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
        if unrelated and not scoped_unrelated:
            require(not wanted or wanted == [current], "Unrelated Traefik ingresses prevent relocation of shared public listeners")
        # Keep the old primary when it is still needed; otherwise use a stable minimum.
        # Never repurpose an existing listener while migrating live routers:
        # pre-HCC CAS patches must not temporarily route one release's port to
        # another. New ports use previously unbound, dedicated entrypoints.
        primary_port = current if unrelated or preserve_primary or current in wanted else (wanted[0] if wanted else current)
        expose = (primary_port in wanted) or (bool(unrelated) and enabled(ports[primary]))
        primary_override = overrides.setdefault(primary, {})
        primary_override["exposedPort"] = primary_port
        set_exposure(primary_override, expose, modern)
        for number in wanted:
            name = primary if number == primary_port else f"eva-{'s' if scheme == 'https' else 'h'}-{number}"
            if name != primary:
                require(name not in ports or name in owned_entries,
                        f"Existing EVA entrypoint {name} has incompatible listener configuration")
                # Container port equals public port for additional named listeners.
                overrides[name] = merge(overrides.get(name, {}), {"port": number, "exposedPort": number, "protocol": "TCP"})
                set_exposure(overrides[name], True, modern)
            assigned[(scheme, number)] = name
    for item in selected.values():
        item["entrypoint"] = assigned[(item["scheme"], item["port"])]
    require(not protected & {item["entrypoint"] for item in selected.values()},
            "Unrelated Traefik router shares an EVA entrypoint; explicit router isolation is required")
    # Disable obsolete owned extra entrypoints, leaving foreign entries untouched.
    owned_names = {item["entrypoint"] for item in selected.values()}
    for name in owned_entries:
        if name in ports and re.fullmatch(r"eva-[hs]-\d+", name) and name not in owned_names:
            set_exposure(overrides.setdefault(name, {}), False, modern)
    http_entries = managed_http_entries(selected, owned_entries, unrelated)
    has_https = any(item["scheme"] == "https" for item in selected.values())
    retained_redirects = set()
    for name, item in ports.items():
        if check_redirects(item):
            if name in http_entries:
                require(not unrelated or name != "web", "Unrelated Traefik ingresses prevent clearing shared listener redirects")
                without_redirects(item, inherited.get("ports", {}).get(name, {}), overrides.setdefault(name, {}))
            else:
                require(has_https or scoped_unrelated, "Existing redirects on unmanaged listeners must be managed externally")
                retained_redirects.add(name)
                legacy = item.get("redirectTo")
                targets = [legacy if isinstance(legacy, str) else (legacy or {}).get("port"),
                           ((item.get("redirections") or {}).get("entryPoint") or {}).get("to"),
                           (((item.get("http") or {}).get("redirections") or {}).get("entryPoint") or {}).get("to")]
                retained_redirects.update(target for target in targets if isinstance(target, str) and target in ports)
    for field in ("additionalArguments", "globalArguments"):
        arguments = effective.get(field, [])
        filtered = []
        for arg in arguments:
            redirect = re.fullmatch(r"--entrypoints\.([\w-]+)\.http\.redirections\.entrypoint\.(to|scheme|permanent|priority)=(.*)", str(arg), re.I)
            if redirect and redirect[1] in http_entries:
                require(not unrelated or redirect[1] != "web", "Unrelated Traefik ingresses prevent clearing shared listener redirects")
                continue
            if redirect:
                require((has_https or scoped_unrelated) and redirect[1] in ports, "Traefik redirect argument uses an unmanaged listener")
                retained_redirects.add(redirect[1])
                if redirect[2].lower() == "to":
                    retained_redirects.update(name for name, item in ports.items()
                                             if redirect[3] == name or redirect[3].lstrip(":") == str(item.get("exposedPort")))
            else:
                require("entrypoints." not in str(arg).lower() and "entrypoints=" not in str(arg).lower(), "Traefik argument overrides unmanaged entrypoints")
            filtered.append(arg)
        if filtered != arguments:
            updated[field] = filtered
    # A preserved redirect must not silently lose its existing source/target
    # Service exposure merely because no EVA router is attached there.
    for name in retained_redirects:
        if enabled(ports[name]):
            set_exposure(overrides.setdefault(name, {}), True, modern)
    resulting = merge(inherited, updated)
    expected = {}
    bound = {}
    for name, item in resulting["ports"].items():
        if enabled(item):
            number = port_number(item.get("exposedPort", item.get("port")))
            require(number not in bound, "Conflicting Traefik exposed ports")
            bound[number] = name
            expected[name] = {"port": number, "targetPort": item.get("targetPort") or name, "protocol": item.get("protocol", "TCP")}
        container_port = item.get("port")
        for other_name, other in resulting["ports"].items():
            require(other_name == name or not container_port or container_port != other.get("port"), "Conflicting Traefik container entrypoint ports")
    if scoped_unrelated:
        require(all(resulting["ports"][name] == ports[name] for name in protected),
                "Unrelated Traefik router listener configuration must remain unchanged")
    else:
        # An implicit/unknown catch-all would accept traffic on every new entrypoint.
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


def verified_installed(cluster, ingress, maps, component, namespace, release, backup_root=None):
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
    if len(installed) != 2 and component == "app" and meta.get("annotations", {}).get(LEGACY):
        return verified_bridge(cluster, ingress, maps[0], manifest, backup_root)
    require(len(installed) == 2, "Helm history does not contain the exact EVA Ingress and endpoint ConfigMap")
    live = endpoint(maps + [ingress], component, namespace, release)
    recorded = endpoint(installed, component, namespace, release)
    require(live and recorded and endpoint_facts(live) == endpoint_facts(recorded),
            "Installed EVA public endpoint differs from Helm history; manual review required")
    return live


def managed_router(ingress, prior, installed):
    annotations = ingress["metadata"].get("annotations", {})
    entry = annotations.get(ENTRY, "")
    allowed = {"web", "websecure"}
    allowed.add(f"eva-{'s' if installed['scheme'] == 'https' else 'h'}-{installed['port']}")
    if prior:
        previous = prior.get("entrypoint", "")
        require(previous in allowed or re.fullmatch(r"eva-[hs]-\d+", previous),
            "Invalid previous EVA managed entrypoint")
        allowed.add(previous)
    require(not entry or all(part.strip() in allowed for part in entry.split(",")),
            "Existing EVA Ingress has a foreign explicit entrypoint; automatic override refused")
    require(annotations.get(TLS, "false") in ("true", "false"),
            "Existing EVA Ingress has an unknown explicit router TLS setting")


def traefik_routers(cluster):
    routers = []
    for ingress in cluster.get("ingresses", None)["items"]:
        annotations = ingress.get("metadata", {}).get("annotations", {})
        if ingress.get("spec", {}).get("ingressClassName", annotations.get("kubernetes.io/ingress.class", "traefik")) == "traefik":
            routers.append(ingress)
    # Discover installed CRDs rather than treating an absent API as an empty list.
    for group in ("traefik.io", "traefik.containo.us"):
        resources = cluster.run(["kubectl", "api-resources", "--api-group=" + group, "-o", "name"]).splitlines()
        for resource in resources:
            if resource.split(".")[0] in ("ingressroutes", "ingressroutetcps", "ingressrouteudps"):
                routers += cluster.get(resource, None)["items"]
    return routers


def router_identity(router):
    meta = router.get("metadata", {})
    return router.get("apiVersion"), router.get("kind"), meta.get("namespace"), meta.get("name")


def verify_unrelated_routers(cluster, unrelated, originals):
    managed = {router_identity(router) for router in originals.values()}
    before = {router_identity(router): stable(router) for router in unrelated}
    now = {router_identity(router): stable(router) for router in traefik_routers(cluster)
           if router_identity(router) not in managed}
    require(now == before, "Unrelated Traefik routers changed during listener reconciliation; stop and reassess")


def discover(cluster, current, old_state, backup_root=None):
    """Verify installed endpoint facts; legacy missing router annotations are valid."""
    selected = {}
    unrelated = []
    originals = {}
    evidence = {}
    current_key = current["namespace"] + "/" + current["release"]
    for ingress in traefik_routers(cluster):
        if ingress.get("kind") != "Ingress":
            unrelated.append(copy.deepcopy(ingress))
            continue
        annotations = ingress.get("metadata", {}).get("annotations", {})
        key = identity(ingress)
        namespace = ingress["metadata"]["namespace"]
        name = ingress["metadata"]["name"]
        release = annotations.get("meta.helm.sh/release-name")
        is_current = namespace == current["namespace"] and name == current["ingress"]
        objects = cluster.get("configmaps", namespace)["items"]
        candidates = [obj for obj in objects if obj.get("metadata", {}).get("name") in (name, name + "-env")]
        component = "app" if any("BACKEND_HOST" in obj.get("data", {}) for obj in candidates) else "iam"
        candidates = [obj for obj in candidates if ("BACKEND_HOST" if component == "app" else "ISSUER") in obj.get("data", {})]
        bridge = bool(annotations.get(LEGACY))
        other = (verified_installed(cluster, ingress, candidates, component, namespace, release, backup_root)
                 if bridge else endpoint(candidates + [ingress], component, namespace, release) if candidates else None)
        if not other:
            require(not is_current, "Existing current EVA Ingress cannot be verified from its installed public endpoint")
            unrelated.append(copy.deepcopy(ingress))
            continue
        require(not is_current or key == current_key, "Current EVA Ingress has unknown Helm ownership")
        if not bridge:
            other = verified_installed(cluster, ingress, candidates, component, namespace, release, backup_root)
        require(key not in originals, "Multiple live ingresses for another EVA release")
        prior = old_state.get(key)
        if prior and key != current_key:
            require(isinstance(prior, dict) and endpoint_facts(prior) == endpoint_facts(other),
                    "Installed EVA endpoint changed outside listener management; reconcile its installed state first")
        managed_router(ingress, prior, other)
        originals[key] = copy.deepcopy(ingress)
        evidence[key] = copy.deepcopy(candidates[0])
        selected[key] = other
    selected[current_key] = current
    return selected, unrelated, originals, evidence


def stable(obj):
    if obj is None:
        return None
    result = copy.deepcopy(obj)
    result.pop("status", None)
    for key in ("resourceVersion", "managedFields", "generation"):
        result.get("metadata", {}).pop(key, None)
    return result


def live_named(cluster, resource, namespace, name):
    items = [obj for obj in cluster.get(resource, None if resource == "services" else namespace)["items"]
             if obj["metadata"].get("namespace") == namespace and obj["metadata"]["name"] == name]
    require(len(items) <= 1, "Ambiguous legacy App resource identity")
    return items[0] if items else None


def template_matches(live, template):
    """Allow API defaults/allocated addresses, while checking every Helm input."""
    if isinstance(template, dict):
        return isinstance(live, dict) and all(key in live and template_matches(live[key], value)
                                               for key, value in template.items())
    if isinstance(template, list):
        return isinstance(live, list) and len(live) == len(template) and all(
            template_matches(left, right) for left, right in zip(live, template))
    return live == template


def template_difference_paths(live, template, path):
    """Describe Helm-input mismatches without logging ConfigMap or other values."""
    if isinstance(template, dict):
        if not isinstance(live, dict):
            return [path]
        return [difference for key, value in template.items()
                for difference in ([path + "." + key] if key not in live else
                                   template_difference_paths(live[key], value, path + "." + key))]
    if isinstance(template, list):
        if not isinstance(live, list) or len(live) != len(template):
            return [path]
        return [difference for index, (left, right) in enumerate(zip(live, template))
                for difference in template_difference_paths(left, right, path + "[" + str(index) + "]")]
    return [] if live == template else [path]


def legacy_history(manifest, service, configmap):
    """Verify listener inputs; retain the entire live ConfigMap for receipt/CAS checks."""
    namespace = service["metadata"]["namespace"]
    found = {}
    for original in (service, configmap):
        matches = [obj for obj in manifest if obj.get("kind") == original["kind"]
                   and obj.get("metadata", {}).get("namespace", namespace) == namespace
                   and obj.get("metadata", {}).get("name") == original["metadata"]["name"]]
        require(len(matches) == 1, "Legacy App requires the exact Service and ConfigMap in Helm history")
        found[original["kind"]] = matches[0]
    differences = template_difference_paths(service["spec"], found["Service"]["spec"], "Service.spec")
    if service["spec"].get("selector") != found["Service"]["spec"].get("selector"):
        differences.append("Service.spec.selector")
    live_data, stored_data = configmap.get("data"), found["ConfigMap"].get("data")
    if live_data != stored_data:
        if isinstance(live_data, dict) and isinstance(stored_data, dict):
            # This transition only changes the Service/Ingress, not application
            # settings. Operators may have edited unrelated runtime keys since
            # Helm installation. Preserve those edits in the full live snapshot;
            # verified_bridge/verify_endpoint_maps still reject concurrent edits.
            differences.extend("ConfigMap.data." + key
                               for key in sorted((live_data.keys() | stored_data.keys()) & LEGACY_ENDPOINT_KEYS)
                               if key not in live_data or key not in stored_data or live_data[key] != stored_data[key])
        else:
            differences.append("ConfigMap.data")
    if differences:
        fields = sorted(set(differences))
        detail = ", ".join(fields[:12])
        if len(fields) > 12:
            detail += ", ... (" + str(len(fields)) + " fields total)"
        raise Unsafe("Legacy App Service/ConfigMap differs from Helm history; automatic transition refused. "
                     "Mismatched fields: " + detail)


def no_foreign_owner(obj):
    meta = obj["metadata"]
    return (not meta.get("deletionTimestamp") and not meta.get("ownerReferences")
            and not meta.get("labels", {}).get("argocd.argoproj.io/instance")
            and not meta.get("annotations", {}).get("argocd.argoproj.io/tracking-id")
            and meta.get("uid") and meta.get("resourceVersion"))


def assess_legacy_app(cluster, request, planned):
    target = request.get("legacy_app", {})
    if planned["component"] == "app":
        target = dict(enabled=True, ingress_enabled=True, namespace=planned["namespace"],
                      release=planned["release"], host=planned["host"], port=planned["port"],
                      secure=planned["scheme"] == "https", path=planned["paths"][0])
    if not target.get("enabled") or not target.get("ingress_enabled"):
        return None
    namespace, release = target["namespace"], target["release"]
    service = live_named(cluster, "services", namespace, release)
    if not service or service.get("spec", {}).get("type") != "NodePort":
        return None
    port = port_number(target["port"])
    if not any(item.get("nodePort") == port for item in service["spec"].get("ports", [])):
        return None
    # Automatic migration is limited to the existing root HTTP App endpoint.
    require(not target.get("secure") and target.get("path") == "/" and is_ip(target["host"])
            and planned["scheme"] == "http" and planned["port"] == port,
            "Existing App NodePort requires a matching shared root HTTP endpoint for automatic transition")
    spec = service["spec"]
    require(helm_owned(service, namespace, release) and no_foreign_owner(service),
            "Legacy App Service has unknown, active Argo CD or terminating ownership")
    require(len(spec.get("ports", [])) == 1 and spec["ports"][0].get("nodePort") == port
            and spec["ports"][0].get("port") == port and spec["ports"][0].get("protocol", "TCP") == "TCP"
            and spec.get("selector") and spec.get("clusterIP") not in (None, "None")
            and not spec.get("externalIPs") and not spec.get("loadBalancerClass"),
            "Legacy App Service is not the supported single HTTP NodePort")
    require(live_named(cluster, "ingresses", namespace, release) is None,
            "Legacy App already has an Ingress; automatic NodePort transition requires review")
    configmap = live_named(cluster, "configmaps", namespace, release + "-env")
    require(configmap and "BACKEND_HOST" in configmap.get("data", {})
            and helm_owned(configmap, namespace, release) and no_foreign_owner(configmap),
            "Legacy App endpoint ConfigMap has no verified Helm ownership")
    status = json.loads(cluster.run(["helm", "status", release, "-n", namespace, "-o", "json"]))
    require(status.get("name") == release and status.get("namespace") == namespace
            and status.get("info", {}).get("status") == "deployed" and status.get("version"),
            "Legacy App transition requires a deployed Helm release")
    manifest = documents(cluster.run(["helm", "get", "manifest", release, "-n", namespace]))
    legacy_history(manifest, service, configmap)
    converted = copy.deepcopy(spec)
    converted["type"] = "ClusterIP"
    converted.pop("externalTrafficPolicy", None)
    for item in converted["ports"]:
        item.pop("nodePort", None)
    selected = {"component": "app", "namespace": namespace, "release": release,
                "host": target["host"], "scheme": "http", "port": port, "paths": ["/"], "ingress": release}
    return {"service": service, "configmap": configmap, "converted_spec": converted,
            "endpoint": selected, "revision": status["version"]}


def read_receipt(path, root):
    file = Path(path)
    require(root and file.parent == Path(root) and file.name.startswith("before-") and file.suffix == ".json"
            and not any(item.is_symlink() for item in (file, *file.parents)), "Invalid legacy App receipt path")
    descriptor = os.open(file, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor) as stream:
        stat = os.fstat(stream.fileno())
        require(stat.st_uid == os.geteuid() and stat.st_mode & 0o077 == 0,
                "Legacy App receipt must be owned by the installation account and private")
        return json.load(stream)


def legacy_bridge(legacy, selected, backup):
    meta = legacy["service"]["metadata"]
    return {"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": {
        "name": meta["name"], "namespace": meta["namespace"], "labels": {"app.kubernetes.io/managed-by": "Helm"},
        "annotations": {"meta.helm.sh/release-name": selected["release"],
                        "meta.helm.sh/release-namespace": meta["namespace"], LEGACY: backup,
                        ENTRY: selected["entrypoint"], TLS: "false"}},
        "spec": {"ingressClassName": "traefik", "rules": [{"http": {"paths": [{"path": "/", "pathType": "Prefix",
            "backend": {"service": {"name": meta["name"], "port": {"number": selected["port"]}}}}]}}]}}


def save_receipt(path, receipt):
    directory = Path(path).parent
    descriptor, temporary = tempfile.mkstemp(prefix=".receipt-", dir=directory)
    try:
        with os.fdopen(descriptor, "w") as stream:
            json.dump(receipt, stream)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        descriptor = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def bridge_matches(live, legacy):
    expected = legacy["bridge"]
    meta = live.get("metadata", {})
    return (template_matches(live, expected) and live.get("spec") == expected["spec"]
            and meta.get("annotations") == expected["metadata"]["annotations"]
            and meta.get("labels") == expected["metadata"]["labels"]
            and not meta.get("finalizers") and no_foreign_owner(live)
            and (not legacy.get("bridge_uid") or meta.get("uid") == legacy["bridge_uid"]))


def verified_bridge(cluster, ingress, configmap, manifest, root):
    path = ingress["metadata"]["annotations"][LEGACY]
    receipt = read_receipt(path, root)
    legacy = receipt.get("legacy")
    require(legacy and legacy.get("phase") != "restored", "Legacy App receipt is not an active transition")
    original = legacy["service"]
    legacy_history(manifest, original, legacy["configmap"])
    require(stable(configmap) == stable(legacy["configmap"]), "Legacy App endpoint ConfigMap changed after transition")
    service = live_named(cluster, "services", original["metadata"]["namespace"], original["metadata"]["name"])
    wanted = copy.deepcopy(original)
    wanted["spec"] = legacy["converted_spec"]
    require(service and stable(service) == stable(wanted) and no_foreign_owner(service),
            "Legacy App converted Service changed after transition")
    require(bridge_matches(ingress, legacy), "Legacy App bridge differs from its private transition receipt or UID changed")
    # The first create is durable before the UID is recorded; its private,
    # unique receipt annotation binds that interrupted create to this attempt.
    return copy.deepcopy(legacy["endpoint"])


def cas_restore(cluster, resource, current, original):
    meta = current["metadata"]
    operations = [{"op": "test", "path": "/metadata/uid", "value": meta["uid"]},
                  {"op": "test", "path": "/metadata/resourceVersion", "value": meta["resourceVersion"]},
                  {"op": "replace", "path": "/spec", "value": original["spec"]}]
    if resource in (RESOURCE, "ingresses"):
        operations.append({"op": "add", "path": "/metadata/annotations", "value": original["metadata"].get("annotations", {})})
    return json.loads(cluster.run(["kubectl", "patch", resource, meta["name"], "-n", meta["namespace"],
                                  "--type=json", "--patch-file=/dev/stdin", "-o", "json"], json.dumps(operations)))


def cas_delete(cluster, resource, obj):
    meta = obj["metadata"]
    base = "/apis/helm.cattle.io/v1" if resource == RESOURCE else "/apis/networking.k8s.io/v1"
    plural = "helmchartconfigs" if resource == RESOURCE else "ingresses"
    options = {"apiVersion": "v1", "kind": "DeleteOptions", "preconditions":
               {"uid": meta["uid"], "resourceVersion": meta["resourceVersion"]}}
    cluster.run(["kubectl", "delete", "--raw", base + "/namespaces/" + meta["namespace"] + "/" + plural + "/" + meta["name"],
                 "-f", "-"], json.dumps(options))


def restore_legacy_app(cluster, receipt, backup, timeout):
    """Restore only this attempt's exact writes; stop if an operator changed them."""
    legacy = receipt["legacy"]
    original = legacy["service"]
    expected = copy.deepcopy(original)
    expected["spec"] = legacy["converted_spec"]
    live = live_named(cluster, "services", original["metadata"]["namespace"], original["metadata"]["name"])
    require(live and (stable(live) == stable(original) or stable(live) == stable(expected)),
            "App Service changed; automatic NodePort restoration refused")
    bridge = live_named(cluster, "ingresses", original["metadata"]["namespace"], original["metadata"]["name"])
    require(not bridge or bridge_matches(bridge, legacy), "App bridge changed; automatic restoration refused")
    chart = cluster.get(CHART, name="traefik")
    require(chart.get("spec") == receipt["helmchart"].get("spec"), "Traefik chart changed; automatic restoration refused")
    config = cluster.get(RESOURCE, name="traefik", optional=True)
    if legacy.get("written_config"):
        require(config and stable(config) == stable(legacy["written_config"]),
                "Traefik configuration changed; automatic restoration refused")
        if receipt["helmchartconfig"]:
            cas_restore(cluster, RESOURCE, config, receipt["helmchartconfig"])
        else:
            cas_delete(cluster, RESOURCE, config)
    else:
        require(stable(config) == stable(receipt["helmchartconfig"]),
                "Traefik configuration changed before its write was verified; restoration refused")
    # ServiceLB retains hostPorts while terminating. Never reallocate the old
    # NodePort until the new listener and every such hostPort are gone.
    deadline = time.monotonic() + timeout
    while True:
        service = cluster.get("services", name="traefik")
        check_service(service)
        occupied = {item["port"] for item in service["spec"]["ports"]}
        for pod in cluster.get("pods", None)["items"]:
            for container in pod.get("spec", {}).get("containers", []) + pod.get("spec", {}).get("initContainers", []):
                occupied |= {item.get("hostPort") for item in container.get("ports", [])}
        if legacy["endpoint"]["port"] not in occupied:
            break
        require(time.monotonic() < deadline, "Original App NodePort cannot be restored before Traefik releases the port")
        time.sleep(2)
    live = live_named(cluster, "services", original["metadata"]["namespace"], original["metadata"]["name"])
    require(live and (stable(live) == stable(original) or stable(live) == stable(expected)),
            "App Service changed; automatic NodePort restoration refused")
    if stable(live) != stable(original):
        cas_restore(cluster, "services", live, original)
    bridge = live_named(cluster, "ingresses", original["metadata"]["namespace"], original["metadata"]["name"])
    if bridge:
        require(bridge_matches(bridge, legacy),
                "App bridge changed; automatic deletion refused")
        cas_delete(cluster, "ingresses", bridge)
    legacy["phase"] = "restored"
    save_receipt(backup, receipt)


def recover_interrupted_bridge_create(cluster, request):
    # If the process exited after create but before releasing the NodePort,
    # remove only the receipt-proven unused bridge and reassess from scratch.
    for bridge in cluster.get("ingresses", None)["items"]:
        path = bridge.get("metadata", {}).get("annotations", {}).get(LEGACY)
        if not path:
            continue
        receipt = read_receipt(path, request["backup_root"])
        legacy = receipt.get("legacy")
        require(legacy and legacy.get("phase") != "restored", "Invalid live legacy App transition receipt")
        if legacy.get("phase") != "prepared":
            continue
        original = legacy["service"]
        live = live_named(cluster, "services", original["metadata"]["namespace"], original["metadata"]["name"])
        if stable(live) == stable(original):
            restore_legacy_app(cluster, receipt, path, float(request.get("timeout", 300)))


def snapshot(root, chart, config, ingresses, legacy=None):
    root = Path(root)
    require(root.is_absolute() and ".." not in root.parts and all(32 <= ord(char) < 127 for char in str(root)),
            "Traefik backup root must be a safe absolute path")
    require(not any(path.is_symlink() for path in (root, *root.parents)), "Traefik backup path contains a symlink")
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    require(root.stat().st_mode & 0o077 == 0 and not root.is_symlink(), "Traefik backup root must be a private non-symlink directory")
    fd, name = tempfile.mkstemp(prefix="before-", suffix=".json", dir=root)
    with os.fdopen(fd, "w") as stream:
        json.dump({"helmchart": chart, "helmchartconfig": config, "ingresses": list(ingresses.values()), "legacy": legacy}, stream)
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
        # Controllers update observed status independently of our router writes.
        # Keep spec, identity and operator metadata checks exact.
        for obj in (current, target):
            obj.pop("status", None)
            obj.get("metadata", {}).pop("resourceVersion", None)
            obj.get("metadata", {}).pop("managedFields", None)
        require(current == target, "EVA Ingress changed during router migration; stop without rollback")


def write_config(cluster, config, values, selected, backup, ownership, capture=False):
    content = yaml.safe_dump(values, sort_keys=False)
    state = json.dumps(selected, sort_keys=True, separators=(",", ":"))
    if config:
        annotations = config.get("metadata", {}).get("annotations", {})
        before = yaml.safe_load(config.get("spec", {}).get("valuesContent", "")) or {}
        if before == values and annotations.get(STATE) == state and annotations.get(OWNED) == ownership:
            return False, None
    try:
        if config:
            patch = [{"op": "test", "path": "/metadata/resourceVersion", "value": config["metadata"]["resourceVersion"]},
                     {"op": "add", "path": "/spec/valuesContent", "value": content}]
            if capture:
                require(config["metadata"].get("uid"), "Legacy transition requires a stable Traefik configuration UID")
                patch.insert(1, {"op": "test", "path": "/metadata/uid", "value": config["metadata"]["uid"]})
            if "annotations" not in config["metadata"]:
                patch.append({"op": "add", "path": "/metadata/annotations", "value": {STATE: state, OWNED: ownership}})
            else:
                for key, value in ((STATE, state), (OWNED, ownership)):
                    patch.append({"op": "add", "path": "/metadata/annotations/" + key.replace("~", "~0").replace("/", "~1"), "value": value})
            output = cluster.run(["kubectl", "patch", RESOURCE, "traefik", "-n", "kube-system", "--type=json", "--patch-file=/dev/stdin"] + (["-o", "json"] if capture else []), json.dumps(patch))
        else:
            resource = {"apiVersion": "helm.cattle.io/v1", "kind": "HelmChartConfig", "metadata": {"name": "traefik", "namespace": "kube-system", "annotations": {STATE: state, OWNED: ownership}}, "spec": {"valuesContent": content}}
            output = cluster.run(["kubectl", "create", "-f", "-"] + (["-o", "json"] if capture else []), json.dumps(resource))
    except Unsafe as exc:
        raise Unsafe(f"HelmChartConfig CAS/create conflict; no adoption or blind retry. Original backup: {backup}") from exc
    return True, json.loads(output) if capture else backup


def check_service(service):
    require(service and service.get("spec", {}).get("type") == "LoadBalancer", "Traefik is not a LoadBalancer Service; NodePort/external LB unsupported")
    annotations = service.get("metadata", {}).get("annotations", {})
    require(annotations.get("meta.helm.sh/release-name") == "traefik" and annotations.get("meta.helm.sh/release-namespace") == "kube-system", "Foreign Traefik Service ownership")
    require(not service["spec"].get("loadBalancerClass") and not service["spec"].get("externalIPs") and not service["spec"].get("loadBalancerIP"), "External LoadBalancer configuration is unsupported")


def ready(cluster, expected, previous_ports, addresses, http_entries=()):
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
            redirect = re.match(r"--entrypoints\.([\w-]+)\.http\.redirections(?:[.=]|$)", arg, re.I)
            if redirect and redirect[1] in http_entries:
                return False
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
    recover_interrupted_bridge_create(cluster, request)
    chart = cluster.get(CHART, name="traefik", optional=True)
    require(chart, "Bundled k3s Traefik HelmChart is absent")
    defaults = chart_defaults(chart) if defaults is None else defaults
    config = cluster.get(RESOURCE, name="traefik", optional=True)
    annotations = (config or {}).get("metadata", {}).get("annotations", {})
    old_state = json.loads(annotations.get(STATE, "{}"))
    require(isinstance(old_state, dict), "Invalid EVA endpoint ownership annotation")
    selected, unrelated, originals, evidence = discover(cluster, planned, old_state, request["backup_root"])
    legacy = assess_legacy_app(cluster, request, planned)
    if legacy:
        legacy_key = legacy["endpoint"]["namespace"] + "/" + legacy["endpoint"]["release"]
        selected[legacy_key] = legacy["endpoint"]
        evidence[legacy_key] = legacy["configmap"]
    values, selected, expected = plan(defaults, chart, config, selected, unrelated, preserve_primary=bool(originals) or bool(legacy))
    effective = merge(merge(defaults, yaml.safe_load(chart.get("spec", {}).get("valuesContent", "")) or {}), values)
    recovered = recoverable_eva_entrypoints(effective["ports"], unrelated)
    owned_entries = owned_entrypoints(config) | recovered
    ownership = ownership_annotation(config, selected, recovered)
    http_entries = managed_http_entries(selected, owned_entries, unrelated)
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
        if legacy and stable(other_service) == stable(legacy["service"]):
            continue
        require(not occupied & desired_ports, "Requested public port conflicts with another Service NodePort/LoadBalancer")
    before_chart = cluster.get(CHART, name="traefik")
    require(before_chart["metadata"]["resourceVersion"] == chart["metadata"]["resourceVersion"], "Traefik HelmChart changed during assessment; retry")
    # One durable private snapshot contains ALL originals before mutation.
    # Normal router changes never blindly roll back; legacy Service transitions
    # have a separate, exact-write restoration path.
    backup = snapshot(request["backup_root"], chart, config, originals, legacy)
    receipt = read_receipt(backup, request["backup_root"]) if legacy else None
    try:
        verify_unrelated_routers(cluster, unrelated, originals)
        verify_endpoint_maps(cluster, evidence)
        changed = False
        if legacy:
            selected_app = selected[legacy_key]
            legacy["endpoint"] = selected_app
            legacy["phase"] = "prepared"
            meta = legacy["service"]["metadata"]
            legacy["bridge"] = legacy_bridge(legacy, selected_app, backup)
            receipt["legacy"] = legacy
            save_receipt(backup, receipt)
            bridge = json.loads(cluster.run(["kubectl", "create", "-f", "-", "-o", "json"], json.dumps(legacy["bridge"])))
            legacy["bridge_uid"] = bridge["metadata"]["uid"]
            save_receipt(backup, receipt)
            # Recheck the live objects before releasing a working NodePort.
            require(live_named(cluster, "services", meta["namespace"], meta["name"]) == legacy["service"],
                    "Legacy App Service changed before port transition")
            verify_endpoint_maps(cluster, evidence)
            converted = copy.deepcopy(legacy["service"])
            converted["spec"] = legacy["converted_spec"]
            cas_restore(cluster, "services", legacy["service"], converted)
            originals[legacy_key] = bridge
            changed = True
        for key, ingress in originals.items():
            changed = patch_router(cluster, ingress, selected[key]) or changed
        verify_routers(cluster, originals, selected)
        verify_endpoint_maps(cluster, evidence)
        before_chart = cluster.get(CHART, name="traefik")
        require(before_chart["metadata"]["resourceVersion"] == chart["metadata"]["resourceVersion"],
                "Traefik HelmChart changed before listener mutation")
        verify_unrelated_routers(cluster, unrelated, originals)
        config_changed, written_config = write_config(cluster, config, values, selected, backup, ownership, capture=bool(legacy))
        if legacy and config_changed:
            legacy["written_config"] = written_config
            save_receipt(backup, receipt)
        changed = config_changed or changed
    except Exception as exc:
        if legacy:
            recover_legacy_failure(cluster, receipt, backup, timeout, originals, selected, exc)
        raise Unsafe(f"EVA router/HCC CAS migration stopped; no blind retry or rollback. Review private original backup: {backup}") from exc
    try:
        result = converge(cluster, chart, values, selected, originals, evidence, expected, old_ports,
                        addresses, current_key, changed, backup, timeout,
                        ownership, http_entries, unrelated)
        if legacy:
            legacy["phase"] = "active"
            save_receipt(backup, receipt)
        return result
    except Unsafe as exc:
        if legacy:
            recover_legacy_failure(cluster, receipt, backup, timeout, originals, selected, exc)
        raise Unsafe(f"{exc}. Original backup: {backup}") from exc
    except Exception as exc:
        if legacy:
            recover_legacy_failure(cluster, receipt, backup, timeout, originals, selected, exc)
        raise Unsafe(f"Traefik convergence stopped on unexpected private input/controller error; no rollback. Original backup: {backup}") from exc


def recover_legacy_failure(cluster, receipt, backup, timeout, originals, selected, error):
    failure_reason = str(error) if isinstance(error, Unsafe) else "private input or controller error (details withheld)"
    try:
        # Restore only changed routers proven to still match this attempt.
        saved = {(obj["metadata"]["namespace"], obj["metadata"]["name"]): obj for obj in receipt["ingresses"]}
        for key, obj in originals.items():
            meta = obj["metadata"]
            original = saved.get((meta["namespace"], meta["name"]))
            if original and stable(original) != stable(isolate([obj], selected[key])[0]):
                live = live_named(cluster, "ingresses", meta["namespace"], meta["name"])
                require(stable(live) == stable(isolate([obj], selected[key])[0]), "Existing router changed; restoration refused")
                cas_restore(cluster, "ingresses", live, original)
        restore_legacy_app(cluster, receipt, backup, timeout)
    except Exception as restoration:
        reason = str(restoration) if isinstance(restoration, Unsafe) else "private input or controller error (details withheld)"
        raise Unsafe(f"Legacy App listener transition failed; automatic restoration stopped: {reason}. "
                     f"Failure reason: {failure_reason}. Private backup: {backup}") from error
    raise Unsafe(f"Legacy App listener transition failed; original App NodePort restored. "
                 f"Failure reason: {failure_reason}. Private backup: {backup}") from error


def converge(cluster, chart, values, selected, originals, evidence, expected, old_ports,
             addresses, current_key, changed, backup, timeout, ownership, http_entries=(), unrelated=()):
    deadline = time.monotonic() + timeout
    while True:
        observed = cluster.get(RESOURCE, name="traefik")
        require((yaml.safe_load(observed.get("spec", {}).get("valuesContent", "")) or {}) == values
            and observed.get("metadata", {}).get("annotations", {}).get(STATE) == json.dumps(selected, sort_keys=True, separators=(",", ":"))
            and observed.get("metadata", {}).get("annotations", {}).get(OWNED) == ownership,
            f"HelmChartConfig changed during controller convergence; stop and reassess. Original backup: {backup}")
        chart_now = cluster.get(CHART, name="traefik")
        require(chart_now.get("spec") == chart.get("spec"), "HelmChart spec changed during convergence")
        verify_routers(cluster, originals, selected)
        verify_endpoint_maps(cluster, evidence)
        verify_unrelated_routers(cluster, unrelated, originals)
        conditions = chart_now.get("status", {}).get("conditions", [])
        require(not any(c.get("type") == "Failed" and c.get("status") == "True" for c in conditions), f"Traefik Helm controller failed; original backup: {backup}")
        if ready(cluster, expected, old_ports, addresses, http_entries):
            verify_routers(cluster, originals, selected)
            verify_endpoint_maps(cluster, evidence)
            verify_unrelated_routers(cluster, unrelated, originals)
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
    parser.add_argument("--verify-legacy-bridge", metavar="BACKUP_ROOT")
    args = parser.parse_args()
    if args.verify_legacy_bridge:
        ingress = json.loads(sys.stdin.read())
        require(ingress.get("kind") == "Ingress" and ingress.get("metadata", {}).get("annotations", {}).get(LEGACY),
                "A receipt-proven legacy App bridge is required")
        meta = ingress["metadata"]
        namespace, release = meta["namespace"], meta["annotations"]["meta.helm.sh/release-name"]
        cluster = Cluster()
        configmap = live_named(cluster, "configmaps", namespace, release + "-env")
        require(helm_owned(ingress, namespace, release) and configmap and helm_owned(configmap, namespace, release),
                "Legacy bridge ownership mismatch")
        manifest = documents(cluster.run(["helm", "get", "manifest", release, "-n", namespace]))
        verified_bridge(cluster, ingress, configmap, manifest, args.verify_legacy_bridge)
        return
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
