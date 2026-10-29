#!/usr/bin/env python3
"""Protect obsolete Agent model storage before a chart upgrade removes it."""
import argparse
import json
import subprocess
import sys


def kubectl(args):
    result = subprocess.run(['kubectl', *args], text=True, capture_output=True, timeout=30)
    if result.returncode:
        raise ValueError('Cannot inspect or preserve legacy Agent model storage')
    return json.loads(result.stdout) if result.stdout.strip() else None


def preserve(namespace):
    changed = False
    for kind, name in [('pv', 'eva-agent-model-pvc-storage'), ('pvc', 'eva-agent-models')]:
        scope = ['-n', namespace] if kind == 'pvc' else []
        resource = kubectl([*scope, 'get', kind, name, '--ignore-not-found', '-o', 'json'])
        if not resource:
            continue
        metadata = resource.get('metadata', {})
        annotations = metadata.get('annotations') or {}
        if (annotations.get('meta.helm.sh/release-name') != 'eva-agent'
                or annotations.get('meta.helm.sh/release-namespace') != namespace):
            continue
        if annotations.get('helm.sh/resource-policy') == 'keep':
            continue
        # Only metadata changes. The volume, claim binding and data stay intact.
        kubectl([*scope, 'annotate', kind, name, 'helm.sh/resource-policy=keep', '--overwrite', '-o', 'json'])
        changed = True
    return changed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--namespace', required=True)
    try:
        print(json.dumps({'changed': preserve(parser.parse_args().namespace)}))
    except Exception:
        print('Cannot preserve legacy EVA Agent model storage before upgrade', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
