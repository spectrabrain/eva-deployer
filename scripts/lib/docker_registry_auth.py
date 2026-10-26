"""Read only the exact registry auth; never consult stale Harbor installers."""
import json
import os
from pathlib import Path
import sys


def registry_auth(registry):
    directory = Path(os.environ.get('DOCKER_CONFIG') or Path.home() / '.docker')
    try:
        config = json.loads((directory / 'config.json').read_text())
        auths = config.get('auths', {})
        auth = next((auths[key] for key in (registry, 'https://' + registry, 'http://' + registry)
                     if key in auths), None)
        if not isinstance(auth, dict) or not (auth.get('auth') or auth.get('identitytoken')):
            raise ValueError()
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
