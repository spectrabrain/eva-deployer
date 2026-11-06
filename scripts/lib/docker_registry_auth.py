"""Resolve exact registry credentials for private ORAS configurations."""
import base64
import json
import os
from pathlib import Path
import re
import subprocess
import sys


def registry_auth(registry):
    directory = Path(os.environ.get('DOCKER_CONFIG') or Path.home() / '.docker')
    try:
        config = json.loads((directory / 'config.json').read_text())
        auths = config.get('auths', {})
        auth = next((auths[key] for key in (registry, 'https://' + registry, 'http://' + registry)
                     if isinstance(auths.get(key), dict)
                     and (auths[key].get('auth') or auths[key].get('identitytoken'))), None)
        if auth is not None:
            return {'auths': {registry: auth}}
        helper = config.get('credHelpers', {}).get(registry) or config.get('credsStore')
        if not isinstance(helper, str) or not re.fullmatch(r'[A-Za-z0-9_-]+', helper):
            raise ValueError()
        result = subprocess.run(['docker-credential-' + helper, 'get'], input=registry + '\n',
                                text=True, capture_output=True, check=True, timeout=10)
        credential = json.loads(result.stdout)
        username, secret = credential.get('Username'), credential.get('Secret')
        if not isinstance(username, str) or not username or not isinstance(secret, str) or not secret:
            raise ValueError()
        if username == '<token>':
            auth = {'identitytoken': secret}
        else:
            auth = {'auth': base64.b64encode((username + ':' + secret).encode()).decode()}
        return {'auths': {registry: auth}}
    except Exception:
        raise ValueError('Matching Docker registry credential is unavailable') from None


if __name__ == '__main__':
    try:
        # The caller redirects this secret-bearing output into a private file.
        print(json.dumps(registry_auth(sys.argv[1])))
    except Exception:
        print('Matching Docker registry credential is unavailable', file=sys.stderr)
        sys.exit(1)
