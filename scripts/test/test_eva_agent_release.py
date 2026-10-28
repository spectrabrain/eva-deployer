#!/usr/bin/env python3
"""Agent release source, credential and published chart compatibility checks."""
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
CHART_BASE = 'https://raw.githubusercontent.com/spectrabrain/eva-agent-chart/chartmuseum'
PROFILE_BASE = 'https://raw.githubusercontent.com/spectrabrain/eva-agent-chart/develop/release/3.3.0'
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
    document['metadata'].update(uid='secret-' + name, resourceVersion='1')
    if 'stringData' in document:
        document['data'] = {key: base64.b64encode(value.encode()).decode()
                            for key, value in document.pop('stringData').items()}
    target.write_text(json.dumps(document))
    print(json.dumps(document) if '-o' in args else 'secret/' + name + ' created')
elif 'get' in args and 'jobs' in args:
    print(json.dumps({'items': [json.loads(p.read_text()) for p in root.glob('job-*.json')]}))
elif 'get' in args and ('secret' in args or 'cronjob' in args):
    kind = 'secret' if 'secret' in args else 'cronjob'
    name = args[args.index(kind) + 1]
    target = root / (('' if kind == 'secret' else kind + '-') + name + '.json')
    if target.exists():
        print('secret/' + target.stem if args[-1] == 'name' else target.read_text())
    elif '--ignore-not-found' not in args:
        sys.exit(1)
elif 'patch' in args:
    kind, name = args[args.index('patch') + 1:args.index('patch') + 3]
    target = root / (('' if kind == 'secret' else kind + '-') + name + '.json')
    current = json.loads(target.read_text())
    update = json.load(sys.stdin)
    assert update['metadata']['uid'] == current['metadata']['uid']
    assert update['metadata']['resourceVersion'] == current['metadata']['resourceVersion']
    if os.environ.get('FAIL_AGENT_PATCH') == kind:
        print('private-agent-fixture-DO-NOT-PRINT', file=sys.stderr)
        sys.exit(1)
    def merge(left, right):
        for key, value in right.items():
            if value is None:
                left.pop(key, None)
            elif isinstance(value, dict):
                merge(left.setdefault(key, {}), value)
            else:
                left[key] = value
    merge(current, update)
    current['metadata']['resourceVersion'] = str(int(current['metadata']['resourceVersion']) + 1)
    target.write_text(json.dumps(current))
    print(kind + '/' + name + ' patched')
elif args[:2] == ['delete', '--raw']:
    name = args[2].rsplit('/', 1)[1]
    target = root / ('job-' + name + '.json')
    current = json.loads(target.read_text())
    options = json.load(sys.stdin)
    assert options['preconditions']['uid'] == current['metadata']['uid']
    assert options['preconditions']['resourceVersion'] == current['metadata']['resourceVersion']
    assert options['propagationPolicy'] == 'Foreground'
    target.unlink()
    print('job/' + name + ' deleted')
elif 'wait' in args:
    pass
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
        aws = self.work / 'bin/aws'
        aws.write_text('#!' + sys.executable + '\n' + r'''
import json, os, pathlib, sys
root = pathlib.Path(os.environ['AGENT_FIXTURE'])
with (root / 'argv.jsonl').open('a') as stream:
    stream.write(json.dumps(['aws'] + sys.argv[1:]) + '\n')
environment = {key: value for key, value in os.environ.items() if key.startswith('AWS_')}
(root / 'aws-environment.json').write_text(json.dumps(environment))
if environment.get('AWS_ACCESS_KEY_ID') == 'invalid-access':
    print('UnrecognizedClientException private-agent-fixture-DO-NOT-PRINT', file=sys.stderr)
    sys.exit(1)
assert environment.get('AWS_SECRET_ACCESS_KEY')
print('private-agent-fixture-DO-NOT-PRINT')
''')
        aws.chmod(0o755)
        (self.work / 'aws_key.ini').write_text('[default]\naws_access_key_id = fixture-access\n'
                                              'aws_secret_access_key = ' + PRIVATE + '\nregion = ap-northeast-2\n')
        self.variables = {
            'eva_agent_namespace': 'eva-agent', 'eva_agent_kubeconfig': '/unused',
            'eva_agent_workspace_chart_values': {}, 'repository_mode': 'online_repository',
            'eva_agent_sync_aws_credentials': 1, 'eva_agent_qdrant_snapshot_source': 'local_pv',
            'aws_key_file': str(self.work / 'aws_key.ini'),
            'eva_agent_credentials_helper_dir': str(self.work / 'helper'),
        }

    def execute(self, valid=True, fail_workload=False):
        marker = self.work / 'workload-stage'
        marker.unlink(missing_ok=True)
        preparation = [
            {'ansible.builtin.include_role': {'name': 'eva_agent', 'tasks_from': 'credentials'}},
            {'name': 'Enter subsequent storage and workload preparation',
             'ansible.builtin.copy': {'dest': str(marker), 'content': 'ready'}},
        ]
        if fail_workload:
            preparation.append({'ansible.builtin.fail': {'msg': 'Fixture workload failure'}})
        result = ansible(self.work, [{
            'block': preparation,
            'always': [{'ansible.builtin.include_tasks': str(ROLE / 'tasks/resume_ecr.yaml'),
                        'when': '(eva_agent_ecr_suspended | default([])) | length > 0'}],
        }], self.variables)
        self.assertEqual(result.returncode == 0, valid, result.stdout + result.stderr)
        self.assertEqual(marker.exists(), valid or fail_workload,
                         'Invalid credentials reached storage/workload preparation')
        self.assertNotIn(PRIVATE, result.stdout + result.stderr)
        self.assertNotIn(PRIVATE, (self.work / 'argv.jsonl').read_text())
        return result

    def secret(self, name, data, kind='Opaque'):
        target = self.work / (name + '.json')
        target.write_text(json.dumps({'type': kind, 'metadata': {
            'name': name, 'namespace': 'eva-agent', 'uid': 'secret-' + name, 'resourceVersion': '1'}, 'data': {
            key: base64.b64encode(value.encode()).decode() for key, value in data.items()}}))
        return target

    def test_online_rotates_managed_custom_aws_secret(self):
        self.variables['eva_agent_workspace_chart_values'] = {'awsCredentialsSecretName': 'custom-aws'}
        self.execute()
        target = self.work / 'custom-aws.json'
        document = json.loads(target.read_text())
        self.assertEqual(document['metadata']['namespace'], 'eva-agent')
        self.assertEqual(base64.b64decode(document['data']['AWS_SECRET_ACCESS_KEY']).decode(), PRIVATE)
        before = target.read_bytes()
        self.execute()
        self.assertEqual(target.read_bytes(), before)
        (self.work / 'aws_key.ini').write_text('aws_access_key_id = rotated-access\n'
                                              'aws_secret_access_key = ' + PRIVATE + '\n')
        self.execute()
        self.assertEqual(base64.b64decode(json.loads(target.read_text())['data']['AWS_ACCESS_KEY_ID']).decode(),
                         'rotated-access')

    def test_external_custom_secret_is_validated_and_preserved(self):
        self.variables['eva_agent_workspace_chart_values'] = {'awsCredentialsSecretName': 'custom-aws'}
        target = self.secret('custom-aws', {'AWS_ACCESS_KEY_ID': 'external-access', 'AWS_SECRET_ACCESS_KEY': PRIVATE})
        before = target.read_bytes()
        (self.work / 'aws_key.ini').unlink()
        self.execute()
        self.assertEqual(target.read_bytes(), before)
        self.assertEqual(json.loads((self.work / 'aws-environment.json').read_text())['AWS_ACCESS_KEY_ID'], 'external-access')

    def test_invalid_existing_aws_secret_is_preserved(self):
        target = self.secret('aws-credentials', {'AWS_ACCESS_KEY_ID': 'old'}, 'kubernetes.io/dockerconfigjson')
        before = target.read_bytes()
        self.execute(valid=False)
        self.assertEqual(target.read_bytes(), before)

    def resource(self, kind, name, **spec):
        target = self.work / (kind.lower() + '-' + name + '.json')
        target.write_text(json.dumps({'kind': kind, 'metadata': {
            'name': name, 'namespace': 'eva-agent', 'uid': kind + '-' + name, 'resourceVersion': '1',
            'annotations': {'meta.helm.sh/release-name': 'eva-agent', 'meta.helm.sh/release-namespace': 'eva-agent'}},
            'spec': spec}))
        return target

    def cronjob_and_jobs(self, suspended=False):
        cronjob = self.resource('CronJob', 'eva-agent-ecr-refresh', suspend=suspended)
        job = self.resource('Job', 'eva-agent-ecr-refresh-123')
        value = json.loads(job.read_text())
        value['metadata']['ownerReferences'] = [{'kind': 'CronJob', 'name': 'eva-agent-ecr-refresh',
                                                 'uid': 'CronJob-eva-agent-ecr-refresh'}]
        job.write_text(json.dumps(value))
        hook = self.resource('Job', 'eva-agent-ecr-refresh-hook')
        unrelated = self.resource('Job', 'eva-agent-qdrant-retention-123')
        return cronjob, job, hook, unrelated

    def test_default_secret_rotates_and_removes_old_tokens_without_touching_other_fields(self):
        target = self.secret('aws-credentials', {'AWS_ACCESS_KEY_ID': 'old', 'AWS_SECRET_ACCESS_KEY': 'old',
                                                 'AWS_SESSION_TOKEN': 'old', 'AWS_SECURITY_TOKEN': 'old',
                                                 'other': 'preserve'})
        value = json.loads(target.read_text())
        value['metadata']['annotations'] = {'external-annotation': 'preserve'}
        target.write_text(json.dumps(value))
        self.execute()
        value = json.loads(target.read_text())
        self.assertEqual(base64.b64decode(value['data']['AWS_SECRET_ACCESS_KEY']).decode(), PRIVATE)
        self.assertNotIn('AWS_SESSION_TOKEN', value['data'])
        self.assertNotIn('AWS_SECURITY_TOKEN', value['data'])
        self.assertEqual(base64.b64decode(value['data']['other']).decode(), 'preserve')
        self.assertEqual(value['metadata']['annotations']['external-annotation'], 'preserve')

    def test_scoped_jobs_retired_before_secret_patch_and_cronjob_resumed(self):
        self.secret('aws-credentials', {'AWS_ACCESS_KEY_ID': 'old', 'AWS_SECRET_ACCESS_KEY': 'old'})
        cronjob, job, hook, unrelated = self.cronjob_and_jobs()
        self.execute()
        self.assertFalse(job.exists())
        self.assertFalse(hook.exists())
        self.assertTrue(unrelated.exists())
        self.assertFalse(json.loads(cronjob.read_text())['spec']['suspend'])
        commands = [json.loads(line) for line in (self.work / 'argv.jsonl').read_text().splitlines()]
        aws = next(i for i, command in enumerate(commands) if command[0] == 'aws')
        suspension = next(i for i, command in enumerate(commands) if 'patch' in command and 'cronjob' in command)
        deletes = [i for i, command in enumerate(commands) if command[:2] == ['delete', '--raw']]
        update = next(i for i, command in enumerate(commands) if 'patch' in command and 'secret' in command)
        self.assertLess(aws, suspension)
        self.assertLess(suspension, min(deletes))
        self.assertLess(max(deletes), update)

    def test_bad_incoming_key_stops_before_mutating_jobs_secret_or_workloads(self):
        target = self.secret('aws-credentials', {'AWS_ACCESS_KEY_ID': 'old', 'AWS_SECRET_ACCESS_KEY': 'old'})
        resources = [target, *self.cronjob_and_jobs()]
        before = [p.read_bytes() for p in resources]
        (self.work / 'aws_key.ini').write_text('aws_access_key_id = invalid-access\naws_secret_access_key = ' + PRIVATE + '\n')
        result = self.execute(valid=False)
        self.assertIn('Agent ECR authentication failed', result.stdout)
        self.assertEqual([p.read_bytes() for p in resources], before)

    def test_external_invalid_key_is_not_replaced_with_valid_workspace_key(self):
        self.variables['eva_agent_workspace_chart_values'] = {'awsCredentialsSecretName': 'custom-aws'}
        target = self.secret('custom-aws', {'AWS_ACCESS_KEY_ID': 'invalid-access', 'AWS_SECRET_ACCESS_KEY': PRIVATE})
        before = target.read_bytes()
        self.execute(valid=False)
        self.assertEqual(target.read_bytes(), before)

    def test_deliberately_suspended_cronjob_stays_suspended(self):
        cronjob, _, _, _ = self.cronjob_and_jobs(suspended=True)
        self.execute()
        self.assertTrue(json.loads(cronjob.read_text())['spec']['suspend'])

    def test_schedule_restored_after_later_workload_failure(self):
        cronjob, _, _, _ = self.cronjob_and_jobs()
        self.execute(valid=False, fail_workload=True)
        self.assertFalse(json.loads(cronjob.read_text())['spec']['suspend'])

    def test_concurrent_secret_update_failure_restores_schedule_and_preserves_secret(self):
        target = self.secret('aws-credentials', {'AWS_ACCESS_KEY_ID': 'old', 'AWS_SECRET_ACCESS_KEY': 'old'})
        before = target.read_bytes()
        cronjob, _, _, _ = self.cronjob_and_jobs()
        with patch.dict(os.environ, FAIL_AGENT_PATCH='secret'):
            self.execute(valid=False)
        self.assertEqual(target.read_bytes(), before)
        self.assertFalse(json.loads(cronjob.read_text())['spec']['suspend'])

    def test_host_profile_and_stale_environment_tokens_do_not_override_selected_key(self):
        with patch.dict(os.environ, AWS_ACCESS_KEY_ID='host-access', AWS_SECRET_ACCESS_KEY='host-secret',
                        AWS_PROFILE='unrelated', AWS_SESSION_TOKEN='stale', AWS_SECURITY_TOKEN='stale'):
            self.execute()
        environment = json.loads((self.work / 'aws-environment.json').read_text())
        self.assertEqual(environment['AWS_ACCESS_KEY_ID'], 'fixture-access')
        self.assertNotIn('AWS_PROFILE', environment)
        self.assertNotIn('AWS_SESSION_TOKEN', environment)
        self.assertNotIn('AWS_SECURITY_TOKEN', environment)

    def test_temporary_key_and_selected_ecr_region_are_synchronized(self):
        self.variables['eva_agent_workspace_chart_values'] = {'ecrRefresh': {'region': 'us-west-2'}}
        with (self.work / 'aws_key.ini').open('a') as stream:
            stream.write('aws_session_token = temporary-token\n')
        self.execute()
        data = json.loads((self.work / 'aws-credentials.json').read_text())['data']
        self.assertEqual(base64.b64decode(data['AWS_SESSION_TOKEN']).decode(), 'temporary-token')
        self.assertEqual(base64.b64decode(data['AWS_DEFAULT_REGION']).decode(), 'us-west-2')

    def test_helm_hook_without_release_annotations_is_retired(self):
        hook = self.resource('Job', 'eva-agent-ecr-refresh-hook')
        value = json.loads(hook.read_text())
        value['metadata']['annotations'] = {'helm.sh/hook': 'pre-install,pre-upgrade',
                                           'helm.sh/hook-delete-policy': 'before-hook-creation,hook-succeeded'}
        value['metadata']['labels'] = {'app.kubernetes.io/instance': 'eva-agent',
                                      'app.kubernetes.io/managed-by': 'Helm', 'helm.sh/chart': 'eva-agent-3.3.0'}
        hook.write_text(json.dumps(value))
        self.execute()
        self.assertFalse(hook.exists())

    def test_unknown_hook_is_preserved_and_schedule_restored(self):
        cronjob, job, hook, unrelated = self.cronjob_and_jobs()
        value = json.loads(hook.read_text())
        value['metadata']['annotations'] = {}
        hook.write_text(json.dumps(value))
        self.execute(valid=False)
        self.assertTrue(hook.exists())
        self.assertTrue(job.exists())
        self.assertTrue(unrelated.exists())
        self.assertFalse(json.loads(cronjob.read_text())['spec']['suspend'])

    def test_legacy_argocd_cronjob_is_verified_and_resumed(self):
        cronjob, job, _, _ = self.cronjob_and_jobs()
        value = json.loads(cronjob.read_text())
        value['metadata']['annotations'] = {
            'argocd.argoproj.io/tracking-id': 'site-eva-agent:batch/CronJob:eva-agent/eva-agent-ecr-refresh'}
        cronjob.write_text(json.dumps(value))
        self.execute()
        self.assertFalse(job.exists())
        self.assertFalse(json.loads(cronjob.read_text())['spec']['suspend'])

    def test_same_job_name_prefix_with_different_owner_uid_is_preserved(self):
        _, job, _, _ = self.cronjob_and_jobs()
        value = json.loads(job.read_text())
        value['metadata']['ownerReferences'][0]['uid'] = 'unrelated-cronjob-uid'
        job.write_text(json.dumps(value))
        self.execute()
        self.assertTrue(job.exists())

    def test_immutable_secret_fails_before_stopping_schedule_or_jobs(self):
        target = self.secret('aws-credentials', {'AWS_ACCESS_KEY_ID': 'old', 'AWS_SECRET_ACCESS_KEY': 'old'})
        value = json.loads(target.read_text())
        value['immutable'] = True
        target.write_text(json.dumps(value))
        resources = [target, *self.cronjob_and_jobs()]
        before = [p.read_bytes() for p in resources]
        self.execute(valid=False)
        self.assertEqual([p.read_bytes() for p in resources], before)

    def test_cloud_alias_repairs_managed_secret_with_missing_credential_field(self):
        self.variables['repository_mode'] = 'cloud_repository'
        target = self.secret('aws-credentials', {'AWS_ACCESS_KEY_ID': 'old'})
        self.execute()
        data = json.loads(target.read_text())['data']
        self.assertEqual(base64.b64decode(data['AWS_SECRET_ACCESS_KEY']).decode(), PRIVATE)

    def test_resuming_does_not_modify_operator_replacement_cronjob(self):
        cronjob = self.resource('CronJob', 'eva-agent-ecr-refresh', suspend=True)
        before = cronjob.read_bytes()
        environment = dict(os.environ, AGENT_FIXTURE=str(self.work),
                           PATH=str(self.work / 'bin') + ':' + os.environ['PATH'])
        result = run([sys.executable, str(ROLE / 'files/manage_online_credentials.py'), 'resume',
                      '--namespace', 'eva-agent'], input=json.dumps([
                          {'name': 'eva-agent-ecr-refresh', 'uid': 'previous-cronjob-uid'}]), env=environment)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(json.loads(result.stdout)['changed'])
        self.assertEqual(cronjob.read_bytes(), before)

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
    def test_install_and_preparation_use_the_same_release_profiles(self):
        for version, expected in [
                ('3.3.0', PROFILE_BASE),
                ('3.2.0', 'https://raw.githubusercontent.com/mellerikat/eva-agent/chartmuseum/release/3.2.0'),
                ('3.3.0', 'https://profiles.test/custom')]:
            with self.subTest(version=version, expected=expected), tempfile.TemporaryDirectory() as temporary:
                work = Path(temporary)
                variables = {
                    'eva_agent_deploy_version': version, 'eva_agent_chart_version': version,
                    'eva_agent_vllm_chart_version': version,
                    'qdrant_chart_version': '1.18.2', 'eva_agent_vllm_profile': 'L40sx1',
                    'eva_cache_root': str(work / 'cache'), 'eva_site_deploy_root': str(work / 'deploy'),
                    'eva_agent_cache_root': str(work / 'cache'),
                    'eva_agent_offline_dir': str(work / 'cache/eva-agent'),
                    'ansible_facts': {'env': {'HOME': str(work)}},
                }
                if expected.endswith('/custom'):
                    variables['eva_agent_chartmuseum_base_url'] = expected
                for filename in ['dependencies.yaml', 'agent.yaml']:
                    definitions = yaml.safe_load((ROLE / 'tasks' / filename).read_text())
                    defaults = next(task for task in definitions
                                    if 'eva_agent_chartmuseum_base_url' in task.get('ansible.builtin.set_fact', {}))
                    tasks = [defaults, {'ansible.builtin.assert': {
                        'that': ['eva_agent_chartmuseum_base_url == ' + json.dumps(expected)]}}]
                    result = ansible(work, tasks, variables)
                    self.assertEqual(result.returncode, 0, filename + '\n' + result.stdout + result.stderr)

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

    def test_role_restores_ecr_schedule_after_dependency_and_agent_installation(self):
        tasks = yaml.safe_load((ROLE / 'tasks/main.yaml').read_text())
        install = next(task for task in tasks if 'block' in task)
        self.assertEqual([task['ansible.builtin.import_tasks'] for task in install['block']],
                         ['dependencies.yaml', 'agent.yaml'])
        self.assertEqual(install['always'][0]['ansible.builtin.include_tasks'], 'resume_ecr.yaml')

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
            'eva-agent-3.3.0.tgz': 'b54ffc0f9b66f9794c90a59098f5e396b8de3442ce6121813208b26acfff196f',
            'eva-agent-vllm-3.3.0.tgz': '0fbaf274a0665d08acca5161b141f405e12280d4675b077f8d84ae0127eb06b4',
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
                              'image) if [[ "$#" -gt 3 ]]; then printf "amd64/linux\\n"; '
                              'else printf \'[{"Os":"linux","Architecture":"amd64"}]\\n\'; fi;;\n'
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
                    self.assertIn(ECR + ':3.3.0', images)
                    self.assertFalse(any(image.startswith(ECR + ':') and image != ECR + ':3.3.0' for image in images))
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
                        self.assertIn('push --platform linux/amd64 ' + target_image, (work / 'publish.log').read_text())
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
                        'eva_agent_image_tag': '3.3.0', 'eva_agent_nfs_share_path': '/data/share/eva-agent',
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
                    output = run([self.helm, 'template', 'eva-agent', str(self.charts['eva-agent-3.3.0.tgz']), '-n', 'eva-agent',
                                  '-f', str(agent_base), '-f', str(self.work / 'agent.yaml'), '--set', 'awsCredentialsSecretName=custom-aws'])
                    self.assertEqual(output.returncode, 0, output.stderr)
                    resources = list(yaml.safe_load_all(output.stdout))
                    self.assertFalse(any(item['kind'] in ['PersistentVolume', 'PersistentVolumeClaim'] for item in resources))
                    deployment = next(item for item in resources if item['kind'] == 'Deployment')
                    pod = deployment['spec']['template']['spec']
                    self.assertEqual(pod['containers'][0]['image'], variables['eva_agent_image_repository'] + ':3.3.0')
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
                    output = run([self.helm, 'template', 'eva-agent-vllm', str(self.charts['eva-agent-vllm-3.3.0.tgz']),
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
                self.assertEqual(workload['metadata']['name'], 'eva-agent-qdrant')
                claims = workload['spec']['volumeClaimTemplates']
                self.assertEqual(
                    {claim['metadata']['name'] + '-' + workload['metadata']['name'] + '-0' for claim in claims},
                    {'eva-agent-qdrant-storage-eva-agent-qdrant-0',
                     'eva-agent-qdrant-snapshots-eva-agent-qdrant-0'})
                for claim in claims:
                    self.assertEqual(claim['spec']['storageClassName'], 'eva-agent-sc-bs')
                    self.assertEqual(claim['spec']['accessModes'], ['ReadWriteOnce'])
                    self.assertEqual(claim['spec']['resources']['requests']['storage'], '10Gi')
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
