#!/usr/bin/env python3
"""Exercise image transport retries and real download backends without a daemon."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SUSE = 'registry.suse.com/bci/bci-base:latest'
TLS_TIMEOUT = ('Error response from daemon: failed to authorize: failed to fetch anonymous token: '
               'Get "https://scc.suse.com/api/registry/authorize": net/http: TLS handshake timeout')


class DockerPullTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='eva-pull-test-')
        self.addCleanup(self.temporary.cleanup)
        self.work = Path(self.temporary.name)
        self.bin = self.work / 'bin'
        self.bin.mkdir()
        self.cache = self.work / 'cache'
        self.tmp = self.work / 'tmp'
        self.tmp.mkdir()
        self.env = dict(os.environ, PATH=str(self.bin) + ':' + os.environ['PATH'],
                        EVA_CACHE_ROOT=str(self.cache), TMPDIR=str(self.tmp),
                        MOCK_STATE=str(self.work / 'state.json'), MOCK_SLEEP=str(self.work / 'sleep.log'),
                        MOCK_IMAGE=SUSE, MOCK_ERROR=TLS_TIMEOUT, MOCK_FAILURES='2',
                        DOCKER_CMD=str(self.bin / 'docker'), COMPONENTS='n8n',
                        VERSIONS_QUIET='1', EVA_MANAGED_IMAGE_EXPORT='true')
        self.executable('docker', '''import json, os, pathlib, sys
p = pathlib.Path(os.environ['MOCK_STATE'])
counts = json.loads(p.read_text()) if p.exists() else {}
if sys.argv[1] == 'pull':
    image = sys.argv[-1]
    counts[image] = counts.get(image, 0) + 1
    p.write_text(json.dumps(counts))
    if image == os.environ['MOCK_IMAGE'] and counts[image] <= int(os.environ['MOCK_FAILURES']):
        print(os.environ['MOCK_ERROR'], file=sys.stderr)
        sys.exit(17)
    print('Digest: sha256:fixture; platform=' + ' '.join(sys.argv[2:-1]))
elif sys.argv[1:3] == ['image', 'inspect']:
    print('amd64/linux')
else:
    raise AssertionError(sys.argv)
''')
        self.executable('helm', 'pass\n')
        self.executable('sleep', '''import os, pathlib, sys
with pathlib.Path(os.environ['MOCK_SLEEP']).open('a') as f:
    f.write(sys.argv[1] + '\\n')
''')

    def executable(self, name, body):
        script = self.bin / name
        script.write_text('#!' + sys.executable + '\n' + body)
        script.chmod(0o755)

    def reset(self):
        for name in ('state.json', 'sleep.log'):
            (self.work / name).unlink(missing_ok=True)

    def pull(self):
        return subprocess.run(['bash', '-c',
                               'set -euo pipefail; source "$1"; eva_docker_pull_image "$2" --platform linux/amd64',
                               'test', str(ROOT / 'scripts/lib/docker_pull.sh'), self.env['MOCK_IMAGE']],
                              env=self.env, capture_output=True, text=True, timeout=10)

    def attempts(self, image):
        return json.loads((self.work / 'state.json').read_text())[image]

    def test_transient_errors_recover(self):
        for message in [TLS_TIMEOUT, 'unexpected EOF', 'connection reset by peer',
                        'received unexpected HTTP status: 503 Service Unavailable']:
            with self.subTest(message=message):
                self.reset()
                self.env['MOCK_ERROR'] = message
                result = self.pull()
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(self.attempts(SUSE), 3)
                self.assertEqual((self.work / 'sleep.log').read_text(), '3\n6\n')
                self.assertIn('platform=--platform linux/amd64', result.stdout)
                self.assertEqual(list(self.tmp.iterdir()), [])

    def test_permanent_errors_do_not_retry(self):
        for message in ['unauthorized: authentication required', 'denied: requested access',
                        'toomanyrequests: You have reached your unauthenticated pull rate limit',
                        'manifest unknown', 'no matching manifest for linux/amd64',
                        'no space left on device', 'image not found: registry.test/example:503']:
            with self.subTest(message=message):
                self.reset()
                self.env['MOCK_ERROR'] = message
                result = self.pull()
                self.assertEqual(result.returncode, 17, result.stdout + result.stderr)
                self.assertEqual(self.attempts(SUSE), 1)
                self.assertFalse((self.work / 'sleep.log').exists())
                self.assertEqual(list(self.tmp.iterdir()), [])

    def test_transport_retries_are_bounded(self):
        self.env['MOCK_FAILURES'] = '20'
        result = self.pull()
        self.assertEqual(result.returncode, 17)
        self.assertEqual(self.attempts(SUSE), 3)
        self.assertIn(SUSE, result.stderr)
        self.assertEqual(list(self.tmp.iterdir()), [])

    def test_download_backends_recover_and_report_missing_images(self):
        n8n = next(line.split(':', 1)[1].strip() for line in
                   (ROOT / 'src/solution/version.yaml').read_text().splitlines()
                   if line.startswith('n8n_image:'))
        for script, image, missing in [
            ('download_eva_images.sh', n8n, 'images-missing.txt'),
            ('download_infra_images.sh', 'rancher/mirrored-pause:3.6', 'infra-images-missing.txt'),
            ('download_n8n_images.sh', n8n, None),
        ]:
            for failures in (2, 20):
                with self.subTest(script=script, failures=failures):
                    self.reset()
                    self.env.update(MOCK_IMAGE=image, MOCK_FAILURES=str(failures))
                    result = subprocess.run(['bash', str(ROOT / 'scripts/download' / script)],
                                            env=self.env, capture_output=True, text=True, timeout=10)
                    self.assertEqual(result.returncode == 0, failures == 2,
                                     result.stdout + result.stderr)
                    self.assertEqual(self.attempts(image), 3)
                    if missing:
                        self.assertEqual((self.cache / 'images' / missing).read_text(),
                                         '' if failures == 2 else image + '\n')
                    if failures == 20:
                        self.assertIn(image, result.stderr)
                    self.assertEqual(list(self.tmp.iterdir()), [])


if __name__ == '__main__':
    unittest.main()
