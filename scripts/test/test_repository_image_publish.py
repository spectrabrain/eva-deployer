#!/usr/bin/env python3
"""Exercise image publication with a partially cached multi-platform index."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
PUBLISHER = ROOT / 'scripts/publish/push_images_to_repository.sh'
REGISTRY = 'harbor.test:32080'
DISPATCHER = '211722900905.dkr.ecr.ap-northeast-2.amazonaws.com/spectrabrain/eva-iam/dispatcher:3.3.0-beta.1'

DOCKER = r'''
import json, os, pathlib, sys
args = sys.argv[1:]
with pathlib.Path(os.environ['PUBLISH_CALLS']).open('a') as stream:
    stream.write(json.dumps(args) + '\n')
if args[0] in ('pull', 'push'):
    # Reproduce Docker's partial-index failure unless one platform is selected.
    if args[1:3] != ['--platform', os.environ['EXPECTED_PLATFORM']]:
        print('manifest list/index references unavailable platform content; '
              'NotFound: content digest sha256:missing: not found', file=sys.stderr)
        sys.exit(1)
    if args[0] == 'push' and args[-1] == os.environ.get('FAILED_TARGET'):
        print('selected platform layer unavailable', file=sys.stderr)
        sys.exit(1)
elif args[0] == 'tag':
    assert len(args) == 3, args
else:
    raise AssertionError('unexpected Docker command: ' + repr(args))
'''


class RepositoryImagePublishTests(unittest.TestCase):
    def publish(self, images, *, pull=False, mirror=False, platform=None, failed=None, infra=False):
        with tempfile.TemporaryDirectory(prefix='eva-image-publish-') as directory:
            root = Path(directory)
            binary = root / 'docker'
            binary.write_text('#!' + sys.executable + '\n' + DOCKER)
            binary.chmod(0o700)
            cache = root / 'cache/images'
            cache.mkdir(parents=True)
            image_list = cache / ('infra-images-pulled.txt' if infra else 'images-pulled.txt')
            image_list.write_text('\n'.join(images) + '\n')
            environment = {
                key: value for key, value in os.environ.items()
                if not key.startswith(('REPOSITORY_', 'HARBOR_', 'PULL_', 'MIRROR_'))
            }
            environment.update({
                'DOCKER_CMD': str(binary), 'IMAGE_LIST': str(image_list),
                'EVA_CACHE_ROOT': str(root / 'cache'),
                'REPOSITORY_REGISTRY': REGISTRY, 'REPOSITORY_PROJECT': 'eva',
                'REPOSITORY_AUTO_LOGIN': 'false',
                'REPOSITORY_MIRROR_PATH_IMAGES': str(mirror).lower(),
                'PULL_SOURCE_IMAGES': str(pull).lower(),
                'LOCAL_HARBOR_YML': str(root / 'absent-harbor.yml'),
                'MIRROR_PATH_IMAGES': '', 'REPOSITORY_PASSWORD': '',
                'PUBLISH_CALLS': str(root / 'calls.jsonl'),
                'EXPECTED_PLATFORM': platform or 'linux/amd64',
                'FAILED_TARGET': failed or '',
            })
            if platform is not None:
                environment['PULL_PLATFORM'] = platform
            result = subprocess.run(['bash', str(PUBLISHER)], env=environment, text=True,
                                    capture_output=True, timeout=30)
            calls = [json.loads(line) for line in (root / 'calls.jsonl').read_text().splitlines()]
            mapping = (cache / 'repository-mapping.txt').read_text().splitlines()
            return result, calls, mapping

    def test_cached_product_indices_publish_without_another_pull(self):
        images = [DISPATCHER, 'docker.n8n.io/n8nio/n8n:2.32.7',
                  '211722900905.dkr.ecr.ap-northeast-2.amazonaws.com/spectrabrain/eva-harbor-oras:0.1.0']
        result, calls, mapping = self.publish(images)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse(any(call[0] == 'pull' for call in calls), 'Remote publication repulled cached images')
        targets = [REGISTRY + '/eva/' + image.rsplit('/', 1)[-1] for image in images]
        self.assertEqual([call for call in calls if call[0] == 'push'],
                         [['push', '--platform', 'linux/amd64', target] for target in targets])
        self.assertEqual(mapping, [image + ' ' + target for image, target in zip(images, targets)])

    def test_infra_indices_use_the_same_platform(self):
        images = ['rancher/mirrored-pause:3.6', 'registry.k8s.io/sig-storage/nfsplugin:v4.11.0']
        result, calls, mapping = self.publish(images, infra=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(mapping), len(images))
        self.assertTrue(all(call[1:3] == ['--platform', 'linux/amd64']
                            for call in calls if call[0] == 'push'))

    def test_source_pull_and_dockerhub_mirrors_use_configured_platform(self):
        images = ['busybox:latest', 'bitnami/kubectl:latest']
        result, calls, mapping = self.publish(images, pull=True, mirror=True, platform='linux/arm64')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        transfers = [call for call in calls if call[0] in ('pull', 'push')]
        self.assertEqual(len(transfers), 8)
        self.assertTrue(all(call[1:3] == ['--platform', 'linux/arm64'] for call in transfers))
        self.assertIn(['push', '--platform', 'linux/arm64', REGISTRY + '/library/busybox:latest'], calls)
        self.assertIn(['push', '--platform', 'linux/arm64', REGISTRY + '/bitnami/kubectl:latest'], calls)
        self.assertEqual(len(mapping), 4)

    def test_empty_platform_uses_linux_amd64(self):
        result, calls, _ = self.publish([DISPATCHER], platform='')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(['push', '--platform', 'linux/amd64', REGISTRY + '/eva/dispatcher:3.3.0-beta.1'], calls)

    def test_real_push_failure_stops_without_recording_failed_image(self):
        first = 'redis:7'
        failed = REGISTRY + '/eva/dispatcher:3.3.0-beta.1'
        result, calls, mapping = self.publish([first, DISPATCHER, 'busybox:latest'], failed=failed)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('selected platform layer unavailable', result.stderr)
        self.assertEqual(mapping, [first + ' ' + REGISTRY + '/eva/redis:7'])
        self.assertFalse(any(call[-1].endswith('/busybox:latest') for call in calls))


if __name__ == '__main__':
    unittest.main()
