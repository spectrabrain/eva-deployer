#!/usr/bin/env python3
"""Prepare one containerd registry host and one namespace pull Secret.

Requests arrive on private stdin. The role persists a missing registry mirror;
this helper never restarts k3s or replaces operator transport or credentials.
"""
import base64
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tomllib

MANAGED_LABELS = {'app.kubernetes.io/managed-by': 'eva-deployer'}
# Non-secret operator notices (Secret replaced/removed), shown by the role.
NOTICES = []


class HelperError(ValueError):
    """A failure whose message names only hosts, paths and Secret names."""


def kubectl(*args, document=None):
    result = subprocess.run(
        ['kubectl', *args], input=json.dumps(document) if document else None,
        text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30,
    )
    if result.returncode:
        # kubectl stderr may echo Secret data; report only the verb.
        raise HelperError('kubectl ' + ' '.join(arg for arg in args[:4] if not arg.startswith('-')) + ' failed')
    return json.loads(result.stdout) if result.stdout.strip() else {}


def endpoint_root(value):
    return value.rstrip('/').removesuffix('/v2')


def selected_endpoint(request):
    registry = request['registry']
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9.-]*(?::[0-9]+)?', registry):
        raise HelperError('Invalid registry host')
    config = request.get('config') or {}
    endpoints = config.get('mirrors', {}).get(registry, {}).get('endpoint', [])
    protocol = request.get('protocol')
    if not protocol:
        protocol = next((value.split('://')[0] for value in endpoints
                         if endpoint_root(value) in ('http://' + registry, 'https://' + registry)), 'http')
    if protocol not in ('http', 'https'):
        raise HelperError('Invalid registry protocol')
    if endpoints and protocol + '://' + registry not in [endpoint_root(value) for value in endpoints]:
        raise HelperError('registries.yaml mirror for ' + registry + ' uses ' + ', '.join(endpoints)
                          + ', not ' + protocol + '://' + registry + '; check repository_protocol')
    return protocol + '://' + registry


def prepare_transport(request):
    endpoint = selected_endpoint(request)
    # HTTPS uses the existing CA and transport configuration. Never enable
    # skip_verify or replace an operator's certificates.
    if endpoint.startswith('https://'):
        return False
    config = tomllib.loads(Path(request['containerd_config']).read_text())
    plugins = config.get('plugins', {})
    registry_config = next((plugins[key].get('registry', {}) for key in (
        'io.containerd.cri.v1.images', 'io.containerd.grpc.v1.cri'
    ) if key in plugins), {})
    root = Path(registry_config.get('config_path', ''))
    if not root.is_absolute() or '..' in root.parts or not root.is_dir():
        raise HelperError('Active containerd registry config_path is unavailable')
    directory = root / request['registry']
    if directory.is_symlink():
        raise HelperError('Registry directory must not be a symlink')
    directory.mkdir(mode=0o755, exist_ok=True)
    hosts = directory / 'hosts.toml'
    if hosts.is_symlink():
        raise HelperError('Registry hosts.toml must not be a symlink')
    if hosts.exists():
        current = tomllib.loads(hosts.read_text())
        matching_hosts = [host for url, host in current.get('host', {}).items() if endpoint_root(url) == endpoint]
        if not any({'pull', 'resolve'}.issubset(host.get('capabilities', [])) for host in matching_hosts) and endpoint_root(current.get('server', '')) != endpoint:
            raise HelperError(str(hosts) + ' does not use ' + endpoint + ' for pull')
        return False
    # Exclusive creation preserves a concurrent operator change.
    with hosts.open('x') as stream:
        os.chmod(hosts, 0o644)
        stream.write(f'server = {json.dumps(endpoint)}\n\n'
                     f'[host.{json.dumps(endpoint)}]\n'
                     '  capabilities = ["pull", "resolve"]\n')
    return True


def registry_credentials(request):
    auth = (request.get('config') or {}).get('configs', {}).get(request['registry'], {}).get('auth', {})
    username, password = request.get('username'), request.get('password')
    if bool(username) != bool(password):
        raise HelperError('Explicit registry credentials must be a complete pair '
                          '(repository_username and repository_password)')
    if not username:
        username, password = auth.get('username'), auth.get('password')
    if bool(username) != bool(password):
        raise HelperError('registries.yaml auth for ' + request['registry'] + ' must have both username and password')
    return username, password


def prepare_secret(request):
    """Keep the namespace pull Secret pointed at the selected Harbor.

    The Secret only carries Harbor pull access for workloads this installation
    manages, so it is always rewritten for the selected registry: a Secret left
    from another Harbor (a moved Main, a former Local Harbor) would otherwise
    block every later installation. Installing against the old Harbor again
    rewrites it back the same way.
    """
    namespace, name, registry = request['namespace'], request['secret'], request['registry']
    username, password = registry_credentials(request)
    existing = kubectl('-n', namespace, 'get', 'secret', name, '--ignore-not-found', '-o', 'json')
    if existing and existing.get('type') != 'kubernetes.io/dockerconfigjson':
        # Same name but not a registry Secret: something else owns it.
        raise HelperError(f'Secret {namespace}/{name} exists with type {existing.get("type")!r}, '
                          'not kubernetes.io/dockerconfigjson; rename it or set a different pull Secret name')
    current = {}
    if existing:
        try:
            current = json.loads(base64.b64decode(existing.get('data', {}).get('.dockerconfigjson', '')))
        except Exception:
            current = {}
    matches = any(key in current.get('auths', {}) for key in (registry, 'http://' + registry, 'https://' + registry))

    if not username:
        if not existing or matches:
            # Public projects and active node credentials can pull without a
            # Secret; an existing Secret for this registry stays as it is.
            # The subsequent Always-pull Pod verifies access before deployment.
            return False
        # Stale Secret for another Harbor and nothing to replace it with.
        kubectl('-n', namespace, 'delete', 'secret', name, '--ignore-not-found')
        NOTICES.append(f'Removed pull Secret {namespace}/{name} that was not for {registry}')
        return True

    encoded = base64.b64encode((username + ':' + password).encode()).decode()
    desired = {'auths': {registry: {'auth': encoded}}}
    if current == desired:
        return False
    if existing:
        kubectl('-n', namespace, 'delete', 'secret', name, '--ignore-not-found')
    kubectl('create', '-f', '-', '-o', 'json', document={
        'apiVersion': 'v1', 'kind': 'Secret', 'type': 'kubernetes.io/dockerconfigjson',
        'metadata': {'name': name, 'namespace': namespace, 'labels': MANAGED_LABELS},
        'stringData': {'.dockerconfigjson': json.dumps(desired)},
    })
    if existing:
        NOTICES.append(f'Replaced pull Secret {namespace}/{name} with credentials for {registry}')
    return True


def main():
    try:
        request = json.load(sys.stdin)
        # Validate the endpoint even if an existing Secret is reused.
        selected_endpoint(request)
        secret_changed = prepare_secret(request)
        transport_changed = prepare_transport(request)
        print(json.dumps({'changed': secret_changed or transport_changed, 'endpoint': selected_endpoint(request),
                          'notices': NOTICES}))
    except HelperError as error:
        print('Target Harbor pull preparation failed: ' + str(error), file=sys.stderr)
        return 1
    except Exception as error:
        # Subprocess diagnostics and parsed credential data must never escape.
        print('Target Harbor pull preparation failed: ' + type(error).__name__, file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
