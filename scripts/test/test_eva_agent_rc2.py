#!/usr/bin/env python3
"""Agent rc2 source, credential and published chart compatibility checks."""
import base64
import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
import urllib.request
from unittest.mock import patch

import yaml

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
ROLE = ROOT / 'src/solution/roles/eva_agent'
REVISION = 'a292a51542ba3981919b8e0a880c1e3d1794d4a6'
CHART_BASE = 'https://raw.githubusercontent.com/spectrabrain/eva-agent-chart/chartmuseum'
PROFILE_BASE = f'https://raw.githubusercontent.com/spectrabrain/eva-agent-chart/{REVISION}/release/3.3.0'
REGISTRY = 'harbor.test:32080'
ECR = '211722900905.dkr.ecr.ap-northeast-2.amazonaws.com/spectrabrain/eva-agent'
ORAS_IMAGE = '211722900905.dkr.ecr.ap-northeast-2.amazonaws.com/spectrabrain/eva-harbor-oras:0.1.0'
PRIVATE = 'private-agent-fixture-DO-NOT-PRINT'
PROFILES = ['A6000x1', 'L40sx1', 'PRO5000x3', 'PRO6000-MIGx4']


def run(argv, **kwargs):
    return subprocess.run(argv, text=True, capture_output=True, **kwargs)


def ansible(work, tasks, variables):
    playbook = work / 'playbook.yaml'
    playbook.write_text(yaml.safe_dump([{
        'hosts': 'localhost', 'connection': 'local', 'gather_facts': False,
        'vars': variables, 'tasks': tasks,
    }], sort_keys=False))
    environment = dict(os.environ, ANSIBLE_ROLES_PATH=str(ROOT / 'src/solution/roles'),
                       PATH=str(work / 'bin') + ':' + os.environ['PATH'], AGENT_FIXTURE=str(work))
    return run(['ansible-playbook', '-i', 'localhost,', str(playbook)], env=environment)


FAKE_KUBECTL = r'''
import base64, json, os, pathlib, sys
root = pathlib.Path(os.environ['AGENT_FIXTURE'])
args = sys.argv[1:]
with (root / 'argv.jsonl').open('a') as stream:
    stream.write(json.dumps(args) + '\n')
if args[:2] == ['create', 'namespace']:
    print(json.dumps({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': args[2]}}))
elif args[:3] == ['apply', '-f', '-']:
    json.load(sys.stdin)
    print('namespace/eva-agent configured')
elif args[:3] == ['create', '-f', '-']:
    document = json.load(sys.stdin)
    name = document['metadata']['name']
    target = root / (name + '.json')
    assert not target.exists(), 'attempted credential replacement'
    document['data'] = {key: base64.b64encode(value.encode()).decode()
                        for key, value in document.pop('stringData').items()}
    target.write_text(json.dumps(document))
    print(json.dumps(document) if '-o' in args else 'secret/' + name + ' created')
elif 'get' in args and 'secret' in args:
    target = root / (args[args.index('secret') + 1] + '.json')
    if target.exists():
        print('secret/' + target.stem if args[-1] == 'name' else target.read_text())
    elif '--ignore-not-found' not in args:
        sys.exit(1)
else:
    raise AssertionError(args)
'''


class AgentCredentialsTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='eva-agent-credentials-')
        self.addCleanup(temporary.cleanup)
        self.work = Path(temporary.name)
        (self.work / 'bin').mkdir()
        kubectl = self.work / 'bin/kubectl'
        kubectl.write_text('#!' + sys.executable + '\n' + FAKE_KUBECTL)
        kubectl.chmod(0o755)
        (self.work / 'aws_key.ini').write_text('[default]\naws_access_key_id = fixture-access\n'
                                              'aws_secret_access_key = ' + PRIVATE + '\nregion = ap-northeast-2\n')
        self.variables = {
            'eva_agent_namespace': 'eva-agent', 'eva_agent_kubeconfig': '/unused',
            'eva_agent_workspace_chart_values': {}, 'repository_mode': 'online_repository',
            'eva_agent_sync_aws_credentials': 1, 'eva_agent_qdrant_snapshot_source': 'local_pv',
            'aws_key_file': str(self.work / 'aws_key.ini'),
        }

    def execute(self, valid=True):
        marker = self.work / 'workload-stage'
        marker.unlink(missing_ok=True)
        result = ansible(self.work, [
            {'ansible.builtin.include_tasks': str(ROLE / 'tasks/credentials.yaml')},
            {'name': 'Enter subsequent storage and workload preparation',
             'ansible.builtin.copy': {'dest': str(marker), 'content': 'ready'}},
        ], self.variables)
        self.assertEqual(result.returncode == 0, valid, result.stdout + result.stderr)
        self.assertEqual(marker.exists(), valid, 'Invalid credentials reached storage/workload preparation')
        self.assertNotIn(PRIVATE, result.stdout + result.stderr)
        self.assertNotIn(PRIVATE, (self.work / 'argv.jsonl').read_text())
        return result

    def secret(self, name, data, kind='Opaque'):
        target = self.work / (name + '.json')
        target.write_text(json.dumps({'type': kind, 'data': {
            key: base64.b64encode(value.encode()).decode() for key, value in data.items()}}))
        return target

    def test_online_creates_custom_aws_secret_once(self):
        self.variables['eva_agent_workspace_chart_values'] = {'awsCredentialsSecretName': 'custom-aws'}
        self.execute()
        target = self.work / 'custom-aws.json'
        document = json.loads(target.read_text())
        self.assertEqual(document['metadata']['namespace'], 'eva-agent')
        self.assertEqual(base64.b64decode(document['data']['AWS_SECRET_ACCESS_KEY']).decode(), PRIVATE)
        before = target.read_bytes()
        (self.work / 'aws_key.ini').unlink()
        self.execute()
        self.assertEqual(target.read_bytes(), before)

    def test_invalid_existing_aws_secret_is_preserved(self):
        target = self.secret('aws-credentials', {'AWS_ACCESS_KEY_ID': 'old'})
        before = target.read_bytes()
        self.execute(valid=False)
        self.assertEqual(target.read_bytes(), before)

    def test_remote_prepares_http_and_snapshot_credentials(self):
        for representation in ['auth', 'pair']:
            with self.subTest(representation=representation):
                hosts = self.work / 'certs.d'
                hosts.mkdir(exist_ok=True)
                containerd = self.work / 'config.toml'
                containerd.write_text('[plugins."io.containerd.grpc.v1.cri".registry]\nconfig_path = ' + json.dumps(str(hosts)) + '\n')
                config = self.work / 'registries.yaml'
                config.write_text('{}\n')
                auth = {'auth': base64.b64encode(('pull-user:' + PRIVATE).encode()).decode()} if representation == 'auth' else {
                    'username': 'pull-user', 'password': PRIVATE}
                pull = self.secret('eva-harbor-pull', {'.dockerconfigjson': json.dumps({'auths': {REGISTRY: auth}})}, 'kubernetes.io/dockerconfigjson')
                before = pull.read_bytes()
                self.variables.update(repository_mode='remote_repository', eva_agent_sync_aws_credentials=0,
                                      eva_agent_qdrant_snapshot_source='harbor', repository_registry=REGISTRY,
                                      repository_protocol='http', eva_registry_config_path=str(config),
                                      eva_registry_containerd_config=str(containerd),
                                      eva_registry_helper_path=str(self.work / 'registry.py'))
                snapshot = self.work / 'qdrant-snapshot-harbor.json'
                snapshot.unlink(missing_ok=True)
                self.execute()
                self.assertFalse((self.work / 'aws-credentials.json').exists())
                self.assertEqual(pull.read_bytes(), before)
                data = json.loads(snapshot.read_text())['data']
                self.assertEqual(base64.b64decode(data['password']).decode(), PRIVATE)
                self.assertIn('http://' + REGISTRY, (hosts / REGISTRY / 'hosts.toml').read_text())
                before = snapshot.read_bytes()
                self.execute()
                self.assertEqual(snapshot.read_bytes(), before)


class AgentPreparationTests(unittest.TestCase):
    def test_credentials_precede_storage_migrations_and_workload_changes(self):
        tasks = yaml.safe_load((ROLE / 'tasks/dependencies.yaml').read_text())
        names = [task.get('name') for task in tasks]
        credential = names.index('Prepare namespace registry and model credentials before deploying Agent workloads')
        self.assertEqual(sum(task.get('ansible.builtin.include_tasks') == 'credentials.yaml' for task in tasks), 1)
        self.assertLess(names.index('Validate repository mode for EVA Agent dependencies'), credential)
        self.assertLess(names.index('Validate Qdrant snapshot source'), credential)
        for operation in [
            'Ensure NFS share and cache directories exist', 'Configure localhost-only NFS export for EVA Agent share',
            'Reconcile legacy eva-agent-init StorageClass ownership', 'Install eva-agent-init from online Helm repo',
            'Scale down existing eva-agent-vllm deployment', 'Scale down existing eva-agent-qdrant statefulset',
            'Install/Upgrade Qdrant (online)', 'Install/Upgrade eva-agent-vllm (online)',
        ]:
            self.assertLess(credential, names.index(operation), operation)

    def test_source_catalog_and_model_downloads(self):
        with tempfile.TemporaryDirectory(prefix='eva-agent-models-') as temporary:
            work = Path(temporary)
            (work / 'bin').mkdir()
            aws = work / 'bin/aws'
            aws.write_text('#!' + sys.executable + '\nimport os, pathlib, sys\n'
                           'with open(os.environ["AWS_LOG"], "a") as stream: stream.write(" ".join(sys.argv[1:]) + "\\n")\n'
                           'destination = pathlib.Path(sys.argv[-1]); destination.mkdir(parents=True, exist_ok=True)\n'
                           '(destination / "weights.bin").write_bytes(b"model")\n')
            aws.chmod(0o755)
            environment = dict(os.environ, PATH=str(work / 'bin') + ':' + os.environ['PATH'], VERSIONS_QUIET='1',
                               EVA_CACHE_ROOT=str(work / 'cache'), AWS_LOG=str(work / 'aws.log'))
            for version, bucket, trees in [('3.3.0', 'spectrabrain-eva-agent', ['vllm']),
                                           ('3.2.0', 's3-an2-mellerikat-release-eva-agent', ['agent', 'vllm'])]:
                with self.subTest(version=version):
                    environment['EVA_AGENT_RELEASE'] = version
                    result = run(['bash', str(ROOT / 'scripts/download/download_eva_models.sh')], env=environment)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    lines = (work / 'aws.log').read_text().splitlines()
                    self.assertEqual(len(lines), len(trees))
                    for tree in trees:
                        self.assertTrue(any(f's3://{bucket}/{tree}/hf' in line for line in lines))
                    manifest = (work / 'cache/models/manifest.txt').read_text()
                    self.assertIn('agent_models_required: ' + ('false' if version == '3.3.0' else 'true'), manifest)
                    (work / 'aws.log').unlink()
            result = run(['bash', '-c', 'source scripts/lib/load_versions.sh; VERSIONS_QUIET=1 load_deploy_versions; '
                          'eva_agent_source release 3.3.0; eva_agent_source image 3.3.0; eva_agent_source chart 3.2.0'], cwd=ROOT)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.splitlines(), [PROFILE_BASE, ECR, 'https://raw.githubusercontent.com/mellerikat/eva-agent/chartmuseum'])
            result = run(['bash', '-c', 'source scripts/lib/load_versions.sh; '
                          'eva_agent_source snapshot-image 3.3.0; eva_agent_source snapshot-image 3.2.0'], cwd=ROOT)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.splitlines(), [ORAS_IMAGE,
                '339713051385.dkr.ecr.ap-northeast-2.amazonaws.com/mellerikat/release/eva-agent-qdrant-snapshot-sync:0.1.0'])

    def test_obsolete_storage_is_preserved_only_for_matching_release(self):
        spec = importlib.util.spec_from_file_location('agent_storage', ROLE / 'files/preserve_model_storage.py')
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        resource = {'metadata': {'annotations': {'meta.helm.sh/release-name': 'eva-agent',
                                                'meta.helm.sh/release-namespace': 'eva-agent'}},
                    'spec': {'persistentVolumeReclaimPolicy': 'Retain', 'nfs': {'path': '/existing/data'}}}
        before = copy.deepcopy(resource)
        with patch.object(module, 'kubectl', return_value=resource) as command:
            self.assertTrue(module.preserve('eva-agent'))
            mutations = [call.args[0] for call in command.call_args_list if 'annotate' in call.args[0]]
            self.assertEqual(len(mutations), 2)
            self.assertTrue(all('helm.sh/resource-policy=keep' in args for args in mutations))
        self.assertEqual(resource, before)
        for annotations in [{}, {'meta.helm.sh/release-name': 'another'},
                            dict(resource['metadata']['annotations'], **{'helm.sh/resource-policy': 'keep'})]:
            with patch.object(module, 'kubectl', return_value={'metadata': {'annotations': annotations}}) as command:
                self.assertFalse(module.preserve('eva-agent'))
                self.assertTrue(all('get' in call.args[0] for call in command.call_args_list))


class AgentPublishedChartsTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory(prefix='eva-agent-charts-')
        cls.addClassCleanup(cls.temporary.cleanup)
        cls.work = Path(cls.temporary.name)
        cls.helm = shutil.which('helm')
        if os.environ.get('EVA_TEST_HELM_DOWNLOAD') == '1':
            source = (ROOT / 'tools/eva/internal/runtime/bootstrap_online.go').read_text()
            artifact = re.search(r'Name:\s*"helm",\s*URL:\s*"([^"]+)",\s*SHA256:\s*"([0-9a-f]{64})"', source)
            if artifact is None:
                raise RuntimeError('Managed Helm checksum pin is missing')
            url, checksum = artifact.groups()
            data = urllib.request.urlopen(url, timeout=60).read()
            if hashlib.sha256(data).hexdigest() != checksum:
                raise RuntimeError('Managed Helm checksum mismatch')
            with tarfile.open(fileobj=io.BytesIO(data)) as archive:
                member = archive.getmember('linux-amd64/helm')
                if not member.isfile():
                    raise RuntimeError('Managed Helm is not a regular archive member')
                binary = cls.work / 'helm'
                with archive.extractfile(member) as stream:
                    binary.write_bytes(stream.read())
                binary.chmod(0o700)
                cls.helm = str(binary)
        if not cls.helm:
            raise RuntimeError('Set EVA_TEST_HELM_DOWNLOAD=1 or install Helm')
        cls.assets = Path(os.environ.get('EVA_AGENT_TEST_ASSETS', cls.work / 'assets'))
        cls.assets.mkdir(exist_ok=True)
        charts = {
            'eva-agent-3.3.0-rc.2.tgz': 'c5aa4366d8495d16e9d7c83060a21ac8dde154b62d4de6d6e418095ca4839e7e',
            'eva-agent-vllm-3.3.0-rc.2.tgz': 'a8a2ee8f419da6ee77e6ccbcd11aea9f0485758dada57d64f8e80e358054ec7a',
            'qdrant-1.18.2.tgz': '7ccdd09343a4b6f6546309e2d0ca31f8034f83883fd9362388d19b2a74fdcea0',
            'eva-agent-init-1.0.0.tgz': '86c08134d39888156285a23e78e2e2fbc26b88955d01db5543f424c349553169',
        }
        cls.charts = {}
        for filename, digest in charts.items():
            target = cls.assets / filename
            if not target.exists():
                if os.environ.get('EVA_TEST_HELM_DOWNLOAD') != '1':
                    raise RuntimeError('Provide EVA_AGENT_TEST_ASSETS or set EVA_TEST_HELM_DOWNLOAD=1')
                base = 'https://github.com/qdrant/qdrant-helm/releases/download/qdrant-1.18.2' if filename.startswith('qdrant-') else CHART_BASE
                target.write_bytes(urllib.request.urlopen(base + '/' + filename, timeout=30).read())
            if hashlib.sha256(target.read_bytes()).hexdigest() != digest:
                raise RuntimeError('Published Agent chart digest mismatch: ' + filename)
            cls.charts[filename] = target
        cls.profiles = {}
        cls.raw_profiles = {}
        for component, filename in [('eva-agent', 'values-k3s.ecr.yaml'), ('eva-agent', 'values-k3s.harbor.yaml')] + [
                ('eva-agent-vllm', f'values-k3s.{profile}.{source}.yaml') for profile in PROFILES for source in ['docker', 'harbor']] + [
                ('eva-agent-qdrant', f'values-k3s.{source}.yaml') for source in ['s3', 'harbor']] + [
                ('eva-agent-init', 'values-k3s.yaml'), ('eva-agent', 'values-secret.yaml')]:
            data = urllib.request.urlopen(PROFILE_BASE + '/' + component + '/' + filename, timeout=30).read()
            cls.raw_profiles[component, filename] = data
            cls.profiles[component, filename] = yaml.safe_load(data)

    def test_image_discovery_and_snapshot_download_use_current_release(self):
        with tempfile.TemporaryDirectory(prefix='eva-agent-preparation-') as temporary:
            work = Path(temporary)
            (work / 'bin').mkdir()
            (work / 'bin/helm').symlink_to(self.helm)
            cache = work / 'cache'
            for filename, source in self.charts.items():
                destination = cache / ('qdrant' if filename.startswith('qdrant-') else 'eva-agent')
                destination.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(source, destination / filename)
            for (component, filename), data in self.raw_profiles.items():
                destination = cache / 'eva-agent/release/3.3.0' / component
                destination.mkdir(parents=True, exist_ok=True)
                (destination / filename).write_bytes(data)
            docker = work / 'bin/docker'
            docker.write_text('#!/bin/bash\nset -eu\ncase "$1" in\n'
                              'login) cat >/dev/null;;\n'
                              'pull) printf "%s\\n" "${@: -1}" >> "$PULL_LOG";;\n'
                              'tag|push) printf "%s\\n" "$*" >> "$PUBLISH_LOG";;\n'
                              'image) printf "amd64/linux\\n";;\n'
                              '*) exit 1;;\nesac\n')
            docker.chmod(0o755)
            aws = work / 'bin/aws'
            aws.write_text('#!' + sys.executable + '\nimport os, pathlib, sys\n'
                           'with open(os.environ["AWS_LOG"], "a") as stream: stream.write(" ".join(sys.argv[1:]) + "\\n")\n'
                           'if "ecr" in sys.argv: print("fixture-token")\n'
                           'elif "cp" in sys.argv: pathlib.Path(sys.argv[-1]).write_bytes(b"snapshot")\n'
                           'else: raise AssertionError(sys.argv)\n')
            aws.chmod(0o755)
            environment = dict(os.environ, PATH=str(work / 'bin') + ':' + os.environ['PATH'], VERSIONS_QUIET='1',
                               EVA_CACHE_ROOT=str(cache), AWS_LOG=str(work / 'aws.log'), COMPONENTS='eva-agent',
                               DOCKER_CMD=str(docker), PULL_LOG=str(work / 'pulls.log'),
                               PUBLISH_LOG=str(work / 'publish.log'))
            for source in ['local_pv', 'harbor']:
                with self.subTest(source=source):
                    environment['EVA_AGENT_QDRANT_SNAPSHOT_SOURCE'] = source
                    output = run(['bash', str(ROOT / 'scripts/download/download_eva_images.sh')], env=environment)
                    self.assertEqual(output.returncode, 0, output.stdout + output.stderr)
                    images = (cache / 'images/images-all.txt').read_text().splitlines()
                    self.assertIn(ECR + ':3.3.0-rc.2', images)
                    self.assertIn('vllm/vllm-openai:v0.21.0', images)
                    self.assertIn('lmcache/lmstack-router:v0.1.10', images)
                    self.assertEqual(ORAS_IMAGE in images, source == 'harbor')
                    self.assertFalse(any('eva-agent-qdrant-snapshot-sync' in image for image in images))
                    self.assertFalse(any('eva-agent:3.2' in image for image in images))
                    self.assertEqual((cache / 'images/images-all.txt').read_bytes(), (cache / 'images/images-pulled.txt').read_bytes())
                    self.assertFalse((cache / 'images/images-missing.txt').read_text())
                    publish_environment = dict(environment, REPOSITORY_REGISTRY=REGISTRY, REPOSITORY_PROJECT='custom',
                                               REPOSITORY_AUTO_LOGIN='false', REPOSITORY_MIRROR_PATH_IMAGES='false',
                                               PULL_SOURCE_IMAGES='false')
                    output = run(['bash', str(ROOT / 'scripts/publish/push_images_to_repository.sh')], env=publish_environment)
                    self.assertEqual(output.returncode, 0, output.stdout + output.stderr)
                    if source == 'harbor':
                        target_image = REGISTRY + '/custom/eva-harbor-oras:0.1.0'
                        self.assertIn(ORAS_IMAGE + ' ' + target_image, (cache / 'images/repository-mapping.txt').read_text())
                        self.assertIn('tag ' + ORAS_IMAGE + ' ' + target_image, (work / 'publish.log').read_text())
                        self.assertIn('push ' + target_image, (work / 'publish.log').read_text())
                    output = run(['bash', str(ROOT / 'scripts/download/download_qdrant_snapshots.sh')], env=environment)
                    self.assertEqual(output.returncode, 0, output.stdout + output.stderr)
                    self.assertIn('s3://spectrabrain-eva-agent/agent/qdrant/eva_manual_qwen3vl_collections/eva_manual_qwen3vl_test_20260824.snapshot',
                                  (work / 'aws.log').read_text())

    def test_all_profiles_render_with_mode_specific_credentials_and_storage(self):
        dependencies = yaml.safe_load((ROLE / 'tasks/dependencies.yaml').read_text())
        selected = [task for task in dependencies if task['name'] in ['Resolve vLLM modelSpec override', 'Build vLLM k3s override values']]
        tasks = selected + [{'ansible.builtin.copy': {'dest': str(self.work / 'vllm.yaml'),
                                                     'content': '{{ eva_agent_vllm_k3s_override_values | to_nice_yaml }}'}},
                            {'ansible.builtin.template': {'src': str(ROOT / 'src/solution/values/agent-k3s-override.yaml.j2'),
                                                         'dest': str(self.work / 'agent.yaml')}}]
        for profile in PROFILES:
            for mode in ['online_repository', 'remote_repository', 'local_repository']:
                with self.subTest(profile=profile, mode=mode):
                    remote = mode != 'online_repository'
                    profile_values = copy.deepcopy(self.profiles['eva-agent-vllm', f'values-k3s.{profile}.{"harbor" if remote else "docker"}.yaml'])
                    variables = {
                        'repository_mode': mode, 'airgap_mode': remote, 'eva_agent_models_required': False,
                        'harbor_url': REGISTRY,
                        'eva_agent_image_repository': REGISTRY + '/custom/eva-agent' if remote else ECR,
                        'eva_agent_image_tag': '3.3.0-rc.2', 'eva_agent_nfs_share_path': '/data/share/eva-agent',
                        'eva_agent_vllm_profile_values': profile_values,
                        'eva_agent_vllm_image_repository': REGISTRY + '/custom/vllm-openai' if remote else 'vllm/vllm-openai',
                        'eva_agent_vllm_image_tag': 'v0.21.0',
                        'eva_agent_vllm_router_image_repository': REGISTRY + '/custom/lmstack-router' if remote else 'lmcache/lmstack-router',
                        'eva_agent_vllm_router_image_tag': 'v0.1.10', 'eva_agent_aws_credentials_secret_name': 'custom-aws',
                        'eva_agent_repository_pull_secret_name': 'eva-harbor-pull',
                        'eva_agent_repository_pull_secrets': ['eva-harbor-pull'] if remote else [],
                    }
                    result = ansible(self.work, tasks, variables)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    agent_base = self.work / 'agent-base.yaml'
                    agent_base.write_text(yaml.safe_dump(self.profiles['eva-agent', 'values-k3s.harbor.yaml' if remote else 'values-k3s.ecr.yaml']))
                    output = run([self.helm, 'template', 'eva-agent', str(self.charts['eva-agent-3.3.0-rc.2.tgz']), '-n', 'eva-agent',
                                  '-f', str(agent_base), '-f', str(self.work / 'agent.yaml'), '--set', 'awsCredentialsSecretName=custom-aws'])
                    self.assertEqual(output.returncode, 0, output.stderr)
                    resources = list(yaml.safe_load_all(output.stdout))
                    self.assertFalse(any(item['kind'] in ['PersistentVolume', 'PersistentVolumeClaim'] for item in resources))
                    deployment = next(item for item in resources if item['kind'] == 'Deployment')
                    pod = deployment['spec']['template']['spec']
                    self.assertEqual(pod['containers'][0]['image'], variables['eva_agent_image_repository'] + ':3.3.0-rc.2')
                    environment = {item['name']: item.get('value') for item in pod['containers'][0]['env']}
                    self.assertEqual(environment['VISION_ML_BASE_URL'], 'http://eva-vision.eva-vision:8000')
                    self.assertEqual(environment['MODEL_CATALOG_FILE'], 'config/model_catalog.json')
                    self.assertTrue(any(item['kind'] == 'Role' and 'vllm-engine-health' in item['metadata']['name'] for item in resources))
                    if remote:
                        self.assertEqual(pod['imagePullSecrets'], [{'name': 'eva-harbor-pull'}])
                        self.assertNotIn('ecr-refresh', output.stdout)
                    else:
                        self.assertIn('custom-aws', output.stdout)
                        self.assertIn('pre-install,pre-upgrade', output.stdout)
                    output = run([self.helm, 'template', 'eva-agent-vllm', str(self.charts['eva-agent-vllm-3.3.0-rc.2.tgz']),
                                  '-n', 'eva-agent', '-f', str(self.work / 'vllm.yaml')])
                    self.assertEqual(output.returncode, 0, output.stderr)
                    resources = list(yaml.safe_load_all(output.stdout))
                    pv = next(item for item in resources if item['kind'] == 'PersistentVolume')
                    self.assertEqual(pv['spec']['nfs']['path'], '/data/share/eva-agent/vllm-cache')
                    engine = next(item for item in resources if item['kind'] == 'Deployment' and 'deployment-vllm' in item['metadata']['name'])
                    engine_pod = engine['spec']['template']['spec']
                    self.assertEqual(engine_pod['runtimeClassName'], 'nvidia')
                    model = profile_values['servingEngineSpec']['modelSpec'][0]
                    self.assertEqual(engine['spec']['replicas'], model['replicaCount'])
                    requests = engine_pod['containers'][0]['resources']['requests']
                    self.assertEqual(requests.get(model['requestGPUType']), str(model['requestGPU']) if 'requestGPU' in model else None)
                    if remote:
                        self.assertNotIn('initContainers', engine_pod)
                        self.assertEqual(engine_pod['imagePullSecrets'], [{'name': 'eva-harbor-pull'}])
                        self.assertNotIn('s3://', output.stdout)
                        self.assertNotIn('aws-credentials', output.stdout)
                        self.assertNotIn('docker.io/', output.stdout)
                    else:
                        self.assertIn('s3://spectrabrain-eva-agent/vllm/hf', output.stdout)
                        self.assertIn('custom-aws', output.stdout)


    def test_qdrant_snapshot_sources_and_secret_shapes(self):
        dependencies = yaml.safe_load((ROLE / 'tasks/dependencies.yaml').read_text())
        names = ['Point Harbor Qdrant values at the selected registry',
                 'Resolve Harbor registry placeholders in Qdrant values',
                 'Resolve Harbor project placeholder in Qdrant values',
                 'Point Harbor Qdrant snapshot-sync image at the selected repository',
                 'Point Harbor Qdrant snapshot project at the selected repository',
                 'Align Qdrant S3 credentials with the selected Agent AWS Secret']
        tasks = [task for task in dependencies if task['name'] in names]
        tasks.append({'ansible.builtin.include_tasks': str(ROLE / 'tasks/resolve_release_values_placeholders.yaml'),
                      'vars': {'eva_agent_release_values_path': '{{ eva_agent_workdir }}/eva-agent-qdrant/{{ eva_agent_qdrant_values_file }}'},
                      'when': 'airgap_mode | bool'})
        tasks.append({'ansible.builtin.template': {
            'src': str(ROOT / 'src/solution/values/qdrant-k3s-override.yaml.j2'),
            'dest': str(self.work / 'qdrant-override.yaml')}})
        directory = self.work / 'eva-agent-qdrant'
        directory.mkdir(exist_ok=True)
        for source in ['s3', 'harbor']:
            with self.subTest(source=source):
                filename = f'values-k3s.{source}.yaml'
                target = directory / filename
                target.write_text(yaml.safe_dump(self.profiles['eva-agent-qdrant', filename], sort_keys=False))
                variables = {
                    'eva_agent_deploy_version': '3.3.0',
                    'repository_registry': REGISTRY, 'repository_project': 'custom',
                    'airgap_mode': source == 'harbor', 'eva_agent_workdir': str(self.work),
                    'eva_agent_qdrant_snapshot_source': 'harbor' if source == 'harbor' else 'local_pv',
                    'eva_agent_qdrant_values_file': filename,
                    'eva_agent_qdrant_snapshot_harbor_registry_effective': REGISTRY,
                    'eva_agent_aws_credentials_secret_name': 'custom-aws', 'eva_agent_nfs_share_path': '/data/share/eva-agent',
                    'eva_agent_repository_pull_secrets': ['eva-harbor-pull'],
                    'eva_agent_qdrant_image_repository': REGISTRY + '/custom/qdrant' if source == 'harbor' else 'qdrant/qdrant',
                    'eva_agent_qdrant_image_tag': 'v1.18.2',
                    'eva_agent_qdrant_chart_test_image_repository': REGISTRY + '/custom/bci-base',
                    'eva_agent_qdrant_chart_test_image_tag': 'latest',
                }
                result = ansible(self.work, tasks, variables)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                output = run([self.helm, 'template', 'eva-agent-qdrant', str(self.charts['qdrant-1.18.2.tgz']),
                              '-n', 'eva-agent', '-f', str(target), '-f', str(self.work / 'qdrant-override.yaml')])
                self.assertEqual(output.returncode, 0, output.stderr)
                resources = list(yaml.safe_load_all(output.stdout))
                workload = next(item for item in resources if item['kind'] == 'StatefulSet')
                pod = workload['spec']['template']['spec']
                sidecar = next(item for item in pod['containers'] if item['name'] == 'qdrant-snapshot-sync')
                environment = {item['name']: item.get('value') for item in sidecar['env']}
                self.assertEqual(environment['SNAPSHOT_RESTORE_EXISTING'], 'false')
                if source == 'harbor':
                    self.assertEqual(sidecar['image'], REGISTRY + '/custom/eva-harbor-oras:0.1.0')
                    original = self.profiles['eva-agent-qdrant', filename]['sidecarContainers'][0]
                    self.assertEqual(sidecar['command'], original['command'])
                    self.assertEqual(sidecar['args'], original['args'])
                    self.assertIn('oras pull', sidecar['args'][0])
                    self.assertEqual(environment['HARBOR_REGISTRY'], REGISTRY)
                    self.assertEqual(environment['HARBOR_PROJECT'], 'custom')
                    self.assertEqual(pod['imagePullSecrets'], [{'name': 'eva-harbor-pull'}])
                    self.assertNotIn('aws-credentials', output.stdout)
                else:
                    self.assertEqual(sidecar['image'], 'amazon/aws-cli:2.33.8')
                    self.assertEqual(environment['S3_BUCKET'], 'spectrabrain-eva-agent')
                    self.assertEqual(environment['S3_PREFIX'], 'agent/qdrant')
                    self.assertEqual(sidecar['envFrom'][0]['secretRef']['name'], 'custom-aws')
                    self.assertIn('eva_manual_qwen3vl_test_20260824.snapshot', environment['SNAPSHOT_SPECS'])


if __name__ == '__main__':
    unittest.main(verbosity=2)
