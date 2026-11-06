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


def kubectl(*args, document=None):
    result = subprocess.run(
        ['kubectl', *args], input=json.dumps(document) if document else None,
        text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30,
    )
    if result.returncode:
        raise ValueError('Kubernetes registry credential operation failed')
    return json.loads(result.stdout) if result.stdout.strip() else {}


def endpoint_root(value):
    return value.rstrip('/').removesuffix('/v2')


def selected_endpoint(request):
    registry = request['registry']
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9.-]*(?::[0-9]+)?', registry):
        raise ValueError('Invalid registry host')
    config = request.get('config') or {}
    endpoints = config.get('mirrors', {}).get(registry, {}).get('endpoint', [])
    protocol = request.get('protocol')
    if not protocol:
        protocol = next((value.split('://')[0] for value in endpoints
                         if endpoint_root(value) in ('http://' + registry, 'https://' + registry)), 'http')
    if protocol not in ('http', 'https'):
        raise ValueError('Invalid registry protocol')
    if endpoints and protocol + '://' + registry not in [endpoint_root(value) for value in endpoints]:
        raise ValueError('Existing registry mirror uses a different endpoint or protocol')
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
        raise ValueError('Active containerd registry config_path is unavailable')
    directory = root / request['registry']
    if directory.is_symlink():
        raise ValueError('Registry directory must not be a symlink')
    directory.mkdir(mode=0o755, exist_ok=True)
    hosts = directory / 'hosts.toml'
    if hosts.is_symlink():
        raise ValueError('Registry hosts.toml must not be a symlink')
    if hosts.exists():
        current = tomllib.loads(hosts.read_text())
        matching_hosts = [host for url, host in current.get('host', {}).items() if endpoint_root(url) == endpoint]
        if not any({'pull', 'resolve'}.issubset(host.get('capabilities', [])) for host in matching_hosts) and endpoint_root(current.get('server', '')) != endpoint:
            raise ValueError('Existing registry hosts.toml has conflicting transport')
        return False
    # Exclusive creation preserves a concurrent operator change.
    with hosts.open('x') as stream:
        os.chmod(hosts, 0o644)
        stream.write(f'server = {json.dumps(endpoint)}\n\n'
                     f'[host.{json.dumps(endpoint)}]\n'
                     '  capabilities = ["pull", "resolve"]\n')
    return True


def prepare_secret(request):
    namespace, name, registry = request['namespace'], request['secret'], request['registry']
    existing = kubectl('-n', namespace, 'get', 'secret', name, '--ignore-not-found', '-o', 'json')
    if existing:
        if existing.get('type') != 'kubernetes.io/dockerconfigjson':
            raise ValueError('Existing pull Secret has an incompatible type')
        config = json.loads(base64.b64decode(existing['data']['.dockerconfigjson']))
        if not any(key in config.get('auths', {}) for key in (registry, 'http://' + registry, 'https://' + registry)):
            raise ValueError('Existing pull Secret does not match this registry')
        return False
    auth = (request.get('config') or {}).get('configs', {}).get(registry, {}).get('auth', {})
    username, password = request.get('username'), request.get('password')
    if bool(username) != bool(password):
        raise ValueError('Explicit registry credentials must be a complete pair')
    if not username:
        username, password = auth.get('username'), auth.get('password')
    if bool(username) != bool(password):
        raise ValueError('Registry username and password must both be provided')
    if not username:
        # Public projects and active node credentials can pull without a Secret.
        # The subsequent Always-pull Pod verifies access before deployment.
        return False
    encoded = base64.b64encode((username + ':' + password).encode()).decode()
    kubectl('create', '-f', '-', '-o', 'json', document={
        'apiVersion': 'v1', 'kind': 'Secret', 'type': 'kubernetes.io/dockerconfigjson',
        'metadata': {'name': name, 'namespace': namespace},
        'stringData': {'.dockerconfigjson': json.dumps({'auths': {registry: {'auth': encoded}}})},
    })
    return True


def main():
    try:
        request = json.load(sys.stdin)
        # Validate the endpoint even if an existing Secret is reused.
        selected_endpoint(request)
        secret_changed = prepare_secret(request)
        transport_changed = prepare_transport(request)
        print(json.dumps({'changed': secret_changed or transport_changed, 'endpoint': selected_endpoint(request)}))
    except Exception:
        # Subprocess diagnostics and parsed credential data must never escape.
        print('Target Harbor transport or pull credential preparation failed', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
