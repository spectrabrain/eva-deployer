#!/usr/bin/env python3
"""Exercise cached index and standalone manifest publication through the backend."""
import base64
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import subprocess
import shutil
import ssl
import sys
import tempfile
import threading
import unittest
from urllib.parse import parse_qs, urlsplit

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
    metadata = json.loads(os.environ.get('IMAGE_METADATA', '{}')).get(args[-1], {})
    single = metadata.get('Descriptor', {}).get('mediaType') in (
        'application/vnd.docker.distribution.manifest.v2+json',
        'application/vnd.oci.image.manifest.v1+json',
    )
    # Reproduce both reported failures: an index needs platform selection,
    # while a single manifest can reject it despite its config being amd64.
    if args[0] == 'push' and single and '--platform' in args:
        print('image was found but does not provide the specified platform (linux/amd64)', file=sys.stderr)
        sys.exit(1)
    if not (args[0] == 'push' and single) and args[1:3] != ['--platform', os.environ['EXPECTED_PLATFORM']]:
        print('manifest list/index references unavailable platform content; '
              'NotFound: content digest sha256:missing: not found', file=sys.stderr)
        sys.exit(1)
    if args[0] == 'push' and args[-1] == os.environ.get('FAILED_TARGET'):
        print(os.environ.get('FAILED_MESSAGE', 'selected platform layer unavailable'), file=sys.stderr)
        sys.exit(1)
elif args[0] == 'tag':
    assert len(args) == 3, args
elif args[:2] == ['image', 'inspect']:
    if args[-1] == os.environ.get('FAILED_INSPECT'):
        print('No such image', file=sys.stderr)
        sys.exit(1)
    print(json.dumps([json.loads(os.environ.get('IMAGE_METADATA', '{}')).get(args[-1], {
        'Os': 'linux', 'Architecture': 'amd64',
        'Descriptor': {'mediaType': 'application/vnd.oci.image.index.v1+json'},
    })]))
else:
    raise AssertionError('unexpected Docker command: ' + repr(args))
'''


class RepositoryImagePublishTests(unittest.TestCase):
    def publish(self, images, *, pull=False, mirror=False, platform=None, failed=None, infra=False,
                metadata=None, failed_inspect=None, recovery=False, failed_message=None,
                plain_http=False, failed_copy=False, missing_oras=False):
        with tempfile.TemporaryDirectory(prefix='eva-image-publish-') as directory:
            root = Path(directory)
            binary = root / 'docker'
            binary.write_text('#!' + sys.executable + '\n' + DOCKER)
            binary.chmod(0o700)
            oras = root / 'oras'
            if not missing_oras:
                oras.write_text('#!' + sys.executable + '\n' + r'''
import json, os, pathlib, sys
args = sys.argv[1:]
with pathlib.Path(os.environ['PUBLISH_CALLS']).open('a') as stream:
    stream.write(json.dumps(['oras'] + args) + '\n')
assert args[:3] == ['cp', '--platform', os.environ['EXPECTED_PLATFORM']], args
assert '@sha256:' in args[-2], 'source must be immutable'
if os.environ.get('FAILED_COPY') == 'true':
    print('source content unavailable', file=sys.stderr)
    sys.exit(1)
''')
                oras.chmod(0o700)
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
                'IMAGE_METADATA': json.dumps(metadata or {}),
                'FAILED_INSPECT': failed_inspect or '',
                'FAILED_MESSAGE': failed_message or 'selected platform layer unavailable',
                'FAILED_COPY': str(failed_copy).lower(),
                'ORAS_CMD': str(oras), 'DOCKER_CONFIG': str(root / 'docker-config'),
                'REPOSITORY_RECOVER_MISSING_CONTENT': str(recovery).lower(),
                'REPOSITORY_PLAIN_HTTP': str(plain_http).lower(),
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
        self.assertIn('selected platform layer unavailable', result.stdout + result.stderr)
        self.assertEqual(mapping, [first + ' ' + REGISTRY + '/eva/redis:7'])
        self.assertFalse(any(call[-1].endswith('/busybox:latest') for call in calls))

    def single_metadata(self, *, os_name='linux', arch='amd64', variant=None, oci=False):
        return {'Os': os_name, 'Architecture': arch, 'Variant': variant,
                'Descriptor': {'digest': 'sha256:' + 'a' * 64,
                               'mediaType': 'application/vnd.oci.image.manifest.v1+json' if oci
                               else 'application/vnd.docker.distribution.manifest.v2+json'}}

    def test_single_manifests_publish_without_platform_selection_or_repull(self):
        for oci in (False, True):
            with self.subTest(oci=oci):
                target = REGISTRY + '/eva/dispatcher:3.3.0-beta.1'
                result, calls, mapping = self.publish([DISPATCHER], metadata={target: self.single_metadata(oci=oci)})
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(['image', 'inspect', target], calls)
                self.assertEqual([c for c in calls if c[0] == 'push'], [['push', target]])
                self.assertFalse(any(c[0] == 'pull' for c in calls))
                self.assertEqual(mapping, [DISPATCHER + ' ' + target])

    def test_mixed_cached_indices_and_single_manifests(self):
        target = REGISTRY + '/eva/dispatcher:3.3.0-beta.1'
        result, calls, mapping = self.publish(['redis:7', DISPATCHER], metadata={target: self.single_metadata()})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual([c for c in calls if c[0] == 'push'],
                         [['push', '--platform', 'linux/amd64', REGISTRY + '/eva/redis:7'], ['push', target]])
        self.assertEqual(len(mapping), 2)

    def test_single_manifest_platform_is_checked_before_push(self):
        target = REGISTRY + '/eva/dispatcher:3.3.0-beta.1'
        for metadata in (self.single_metadata(arch='arm64'), self.single_metadata(os_name='windows'),
                         self.single_metadata(arch=None)):
            with self.subTest(metadata=metadata):
                result, calls, mapping = self.publish([DISPATCHER], metadata={target: metadata})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('does not match linux/amd64', result.stderr)
                self.assertFalse(any(c[0] == 'push' for c in calls))
                self.assertEqual(mapping, [])

    def test_explicit_single_manifest_variant_must_match(self):
        target = REGISTRY + '/eva/dispatcher:3.3.0-beta.1'
        metadata = {target: self.single_metadata(arch='arm', variant='v7')}
        result, calls, mapping = self.publish([DISPATCHER], platform='linux/arm/v7', metadata=metadata)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(['push', target], calls)
        result, calls, mapping = self.publish([DISPATCHER], platform='linux/arm/v6', metadata=metadata)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c[0] == 'push' for c in calls))
        self.assertEqual(mapping, [])

    def test_classic_store_without_descriptor_keeps_platform_selection(self):
        target = REGISTRY + '/eva/dispatcher:3.3.0-beta.1'
        result, calls, _ = self.publish([DISPATCHER], metadata={target: {'Os': 'linux', 'Architecture': 'amd64'}})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(['push', '--platform', 'linux/amd64', target], calls)

    def test_inspect_failure_does_not_push_or_record(self):
        target = REGISTRY + '/eva/dispatcher:3.3.0-beta.1'
        result, calls, mapping = self.publish([DISPATCHER], failed_inspect=target)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c[0] == 'push' for c in calls))
        self.assertEqual(mapping, [])

    def test_single_manifest_push_failure_is_not_retried(self):
        target = REGISTRY + '/eva/dispatcher:3.3.0-beta.1'
        result, calls, mapping = self.publish([DISPATCHER], failed=target,
                                             metadata={target: self.single_metadata()})
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual([c for c in calls if c[0] == 'push'], [['push', target]])
        self.assertEqual(mapping, [])

    def test_dockerhub_mirror_uses_single_manifest_handling(self):
        targets = [REGISTRY + '/eva/busybox:latest', REGISTRY + '/library/busybox:latest']
        result, calls, mapping = self.publish(['busybox:latest'], mirror=True,
                                             metadata={t: self.single_metadata() for t in targets})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual([c for c in calls if c[0] == 'push'], [['push', t] for t in targets])
        self.assertEqual(len(mapping), 2)

    def test_failed_dockerhub_mirror_is_not_recorded_as_published(self):
        failed = REGISTRY + '/library/busybox:latest'
        result, calls, mapping = self.publish(['busybox:latest'], mirror=True, failed=failed)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(mapping, ['busybox:latest ' + REGISTRY + '/eva/busybox:latest'])

    def test_remote_missing_single_manifest_layer_copies_exact_digest(self):
        target = REGISTRY + '/eva/dispatcher:3.3.0-beta.1'
        result, calls, mapping = self.publish([DISPATCHER], failed=target, recovery=True, plain_http=True,
                                             failed_message='NotFound: content digest sha256:' + 'b' * 64 + ': not found',
                                             metadata={target: self.single_metadata()})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        copies = [c for c in calls if c[0] == 'oras']
        self.assertEqual(len(copies), 1)
        self.assertIn('--to-plain-http', copies[0])
        self.assertNotIn('--from-plain-http', copies[0])
        self.assertEqual(copies[0][-2:], [DISPATCHER.rsplit(':', 1)[0] + '@sha256:' + 'a' * 64, target])
        config = copies[0][copies[0].index('--from-registry-config') + 1]
        self.assertEqual(config, copies[0][copies[0].index('--to-registry-config') + 1])
        self.assertTrue(config.endswith('/docker-config/config.json'))
        self.assertFalse(any(c[0] in ('pull', 'rmi', 'prune') for c in calls))
        self.assertEqual(mapping, [DISPATCHER + ' ' + target])

    def test_remote_index_platform_failure_copies_only_requested_platform_over_https(self):
        target = REGISTRY + '/eva/dispatcher:3.3.0-beta.1'
        metadata = self.single_metadata()
        metadata['Descriptor']['mediaType'] = 'application/vnd.oci.image.index.v1+json'
        result, calls, mapping = self.publish([DISPATCHER], failed=target, recovery=True,
                                             failed_message='image was found but does not provide the specified platform (linux/amd64)',
                                             metadata={target: metadata})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        copies = [c for c in calls if c[0] == 'oras']
        self.assertEqual(copies[0][1:4], ['cp', '--platform', 'linux/amd64'])
        self.assertNotIn('--to-plain-http', copies[0])
        self.assertNotIn('--to-insecure', copies[0])
        self.assertEqual(len(mapping), 1)

    def test_offline_missing_content_failure_never_fetches_source(self):
        target = REGISTRY + '/eva/dispatcher:3.3.0-beta.1'
        result, calls, mapping = self.publish([DISPATCHER], failed=target,
                                             failed_message='NotFound: content digest sha256:abc: not found',
                                             metadata={target: self.single_metadata()})
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c[0] == 'oras' for c in calls))
        self.assertEqual(mapping, [])

    def test_registry_permission_failure_is_not_recovered(self):
        target = REGISTRY + '/eva/dispatcher:3.3.0-beta.1'
        result, calls, mapping = self.publish([DISPATCHER], failed=target, recovery=True,
                                             failed_message='unauthorized: authentication required',
                                             metadata={target: self.single_metadata()})
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c[0] == 'oras' for c in calls))
        self.assertEqual(mapping, [])

    def test_recovery_failure_never_records_mapping(self):
        target = REGISTRY + '/eva/dispatcher:3.3.0-beta.1'
        for options in ({'failed_copy': True}, {'missing_oras': True}):
            with self.subTest(options=options):
                result, calls, mapping = self.publish([DISPATCHER], failed=target, recovery=True,
                                                     failed_message='NotFound: content digest sha256:abc: not found',
                                                     metadata={target: self.single_metadata()}, **options)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(mapping, [])

    def test_recovery_requires_cached_descriptor_digest(self):
        target = REGISTRY + '/eva/dispatcher:3.3.0-beta.1'
        metadata = self.single_metadata()
        del metadata['Descriptor']['digest']
        result, calls, mapping = self.publish([DISPATCHER], failed=target, recovery=True,
                                             failed_message='NotFound: content digest sha256:abc: not found',
                                             metadata={target: metadata})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('without its cached manifest digest', result.stderr)
        self.assertFalse(any(c[0] == 'oras' for c in calls))
        self.assertEqual(mapping, [])


ORAS_BINARY = os.environ.get('EVA_TEST_ORAS') or shutil.which('oras')


@unittest.skipUnless(ORAS_BINARY and shutil.which('openssl'), 'real copy fixture requires ORAS and OpenSSL')
class RegistryCopyIntegrationTests(unittest.TestCase):
    """Use real ORAS against private HTTPS source and HTTP destination registries."""

    def test_missing_docker_blob_recovers_single_manifest_and_partial_index(self):
        for index in (False, True):
            with self.subTest(index=index), tempfile.TemporaryDirectory(prefix='eva-registry-copy-') as temporary:
                root = Path(temporary)
                certificate, key = root / 'ca.pem', root / 'key.pem'
                subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
                                '-keyout', str(key), '-out', str(certificate), '-days', '1',
                                '-subj', '/CN=127.0.0.1', '-addext', 'subjectAltName=IP:127.0.0.1'],
                               check=True, capture_output=True, timeout=30)
                credential = base64.b64encode(b'fixture:registry-secret').decode()
                expected_auth = 'Basic ' + credential
                source_state = {'blobs': {}, 'manifests': {}, 'requests': []}
                target_state = {'blobs': {}, 'manifests': {}, 'requests': []}

                def handler(state):
                    class Registry(BaseHTTPRequestHandler):
                        def log_message(self, *args):
                            pass

                        def reply(self, status, data=b'', headers=None):
                            self.send_response(status)
                            self.send_header('Content-Length', str(len(data)))
                            self.send_header('Docker-Distribution-Api-Version', 'registry/2.0')
                            for name, value in (headers or {}).items():
                                self.send_header(name, value)
                            self.end_headers()
                            if self.command != 'HEAD':
                                self.wfile.write(data)

                        def authorized(self):
                            state['requests'].append((self.command, self.path))
                            if self.headers.get('Authorization') != expected_auth:
                                self.reply(401, headers={'WWW-Authenticate': 'Basic realm="fixture"'})
                                return False
                            return True

                        def do_GET(self):
                            if not self.authorized():
                                return
                            path = urlsplit(self.path).path
                            if path == '/v2/':
                                return self.reply(200)
                            if '/blobs/' in path:
                                digest = path.rsplit('/', 1)[-1]
                                data = state['blobs'].get(digest)
                                if data is not None:
                                    return self.reply(200, data, {'Docker-Content-Digest': digest})
                            if '/manifests/' in path:
                                name = path.rsplit('/', 1)[-1]
                                data = state['manifests'].get(name)
                                if data is not None:
                                    return self.reply(200, data, {'Content-Type': json.loads(data)['mediaType'],
                                                                'Docker-Content-Digest': digest_of(data)})
                            self.reply(404)

                        do_HEAD = do_GET

                        def do_POST(self):
                            if not self.authorized():
                                return
                            state['upload'] = b''
                            self.reply(202, headers={'Location': self.path.rstrip('/') + '/fixture'})

                        def do_PATCH(self):
                            if not self.authorized():
                                return
                            state['upload'] += self.rfile.read(int(self.headers.get('Content-Length', 0)))
                            self.reply(202, headers={'Location': self.path})

                        def do_PUT(self):
                            if not self.authorized():
                                return
                            url = urlsplit(self.path)
                            data = self.rfile.read(int(self.headers.get('Content-Length', 0)))
                            if '/manifests/' in url.path:
                                name = url.path.rsplit('/', 1)[-1]
                                state['manifests'][name] = data
                                state['manifests'][digest_of(data)] = data
                            else:
                                data = state.get('upload', b'') + data
                                self.assert_digest(data, parse_qs(url.query)['digest'][0])
                                state['blobs'][digest_of(data)] = data
                            self.reply(201, headers={'Docker-Content-Digest': digest_of(data), 'Location': url.path})

                        def assert_digest(self, data, digest):
                            if digest_of(data) != digest:
                                raise AssertionError('copied blob digest mismatch')
                    return Registry

                def digest_of(data):
                    return 'sha256:' + hashlib.sha256(data).hexdigest()

                def encoded(value):
                    return json.dumps(value, separators=(',', ':')).encode()

                def descriptor(data, media):
                    return {'mediaType': media, 'digest': digest_of(data), 'size': len(data)}

                # A non-empty layer makes this verify actual missing blob transfer.
                layer = b'compressed-layer-fixture'
                config = encoded({'os': 'linux', 'architecture': 'amd64'})
                source_state['blobs'] = {digest_of(config): config, digest_of(layer): layer}
                manifest = encoded({'schemaVersion': 2, 'mediaType': 'application/vnd.oci.image.manifest.v1+json',
                                    'config': descriptor(config, 'application/vnd.oci.image.config.v1+json'),
                                    'layers': [descriptor(layer, 'application/vnd.oci.image.layer.v1.tar+gzip')]})
                source_state['manifests'][digest_of(manifest)] = manifest
                selected = descriptor(manifest, 'application/vnd.oci.image.manifest.v1+json')
                selected['platform'] = {'os': 'linux', 'architecture': 'amd64'}
                unavailable = 'sha256:' + 'b' * 64
                image_index = encoded({'schemaVersion': 2, 'mediaType': 'application/vnd.oci.image.index.v1+json',
                                       'manifests': [selected, {'mediaType': selected['mediaType'], 'digest': unavailable,
                                                               'size': 123, 'platform': {'os': 'linux', 'architecture': 'arm64'}}]})
                cached = image_index if index else manifest
                source_state['manifests'][digest_of(cached)] = cached
                source = ThreadingHTTPServer(('127.0.0.1', 0), handler(source_state))
                target = ThreadingHTTPServer(('127.0.0.1', 0), handler(target_state))
                context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
                context.load_cert_chain(certificate, key)
                source.socket = context.wrap_socket(source.socket, server_side=True)
                threads = [threading.Thread(target=server.serve_forever, daemon=True) for server in (source, target)]
                for thread in threads:
                    thread.start()
                try:
                    source_host = '127.0.0.1:' + str(source.server_port)
                    target_host = '127.0.0.1:' + str(target.server_port)
                    source_image = source_host + '/source/dispatcher:stable'
                    target_image = target_host + '/eva/dispatcher:stable'
                    cache = root / 'cache/images'
                    cache.mkdir(parents=True)
                    images = cache / 'images-pulled.txt'
                    images.write_text(source_image + '\n')
                    docker = root / 'docker'
                    docker.write_text('#!' + sys.executable + '\n' + DOCKER)
                    docker.chmod(0o700)
                    docker_config = root / 'docker-config'
                    docker_config.mkdir()
                    auth_file = docker_config / 'config.json'
                    auth_file.write_text(json.dumps({'auths': {'https://' + source_host: {'auth': credential},
                                                              target_host: {'auth': credential}}}))
                    before = auth_file.read_bytes()
                    environment = dict(os.environ, DOCKER_CMD=str(docker), ORAS_CMD=str(ORAS_BINARY),
                                       DOCKER_CONFIG=str(docker_config), SSL_CERT_FILE=str(certificate),
                                       IMAGE_LIST=str(images), EVA_CACHE_ROOT=str(root / 'cache'),
                                       REPOSITORY_REGISTRY=target_host, REPOSITORY_PROJECT='eva',
                                       REPOSITORY_AUTO_LOGIN='false', REPOSITORY_MIRROR_PATH_IMAGES='false',
                                       REPOSITORY_RECOVER_MISSING_CONTENT='true', REPOSITORY_PLAIN_HTTP='true',
                                       PULL_SOURCE_IMAGES='false', PULL_PLATFORM='linux/amd64',
                                       EXPECTED_PLATFORM='linux/amd64', PUBLISH_CALLS=str(root / 'calls.jsonl'),
                                       FAILED_TARGET=target_image, FAILED_MESSAGE='NotFound: content digest ' + digest_of(layer) + ': not found',
                                       IMAGE_METADATA=json.dumps({target_image: {'Os': 'linux', 'Architecture': 'amd64',
                                                                               'Descriptor': descriptor(cached, json.loads(cached)['mediaType'])}}))
                    result = subprocess.run(['bash', str(PUBLISHER)], env=environment, text=True,
                                            capture_output=True, timeout=30)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertEqual(target_state['manifests']['stable'], manifest)
                    self.assertEqual(target_state['blobs'][digest_of(layer)], layer)
                    self.assertEqual(target_state['blobs'][digest_of(config)], config)
                    self.assertFalse(any(unavailable in path for _, path in source_state['requests']))
                    self.assertFalse(any('/manifests/stable' in path for _, path in source_state['requests']),
                                     'source tag was resolved instead of cached immutable digest')
                    self.assertEqual((cache / 'repository-mapping.txt').read_text().strip(), source_image + ' ' + target_image)
                    self.assertEqual(before, auth_file.read_bytes())
                    self.assertNotIn('registry-secret', result.stdout + result.stderr)
                finally:
                    for server in (source, target):
                        server.shutdown()
                        server.server_close()
                    for thread in threads:
                        thread.join(timeout=5)


if __name__ == '__main__':
    unittest.main()
