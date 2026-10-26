#!/usr/bin/env python3
"""Offline regression tests for n8n preparation and existing k3s installs."""
import base64
import copy
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import yaml

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
ROLES = ROOT / 'src/solution/roles'
REGISTRY = 'harbor.test:32080'
PASSWORD = 'private-fixture-password-DO-NOT-PRINT'
spec = importlib.util.spec_from_file_location('registry_helper', ROLES / 'eva_k3s_registry/files/registry.py')
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)

FAKE_KUBECTL = r'''
import json, os, pathlib, sys
state = pathlib.Path(os.environ['N8N_FIXTURE'])
args = sys.argv[1:]
with (state / 'calls.jsonl').open('a') as stream:
    stream.write(json.dumps(args) + '\n')
if args[:2] == ['get', 'nodes']:
    print('fixture-node Ready')
elif args[:2] == ['create', 'namespace']:
    print(json.dumps({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': args[2]}}))
elif args[:2] == ['create', '-f']:
    doc = json.load(sys.stdin)
    if doc['kind'] == 'Secret':
        data = doc.pop('stringData')
        import base64
        doc['data'] = {k: base64.b64encode(v.encode()).decode() for k, v in data.items()}
        (state / 'secret.json').write_text(json.dumps(doc))
    elif doc['kind'] == 'Pod':
        assert doc['spec']['containers'][0]['imagePullPolicy'] == 'Always'
        doc['metadata']['name'] = 'eva-n8n-pull-fixture'
        (state / 'probe.json').write_text(json.dumps(doc))
    print(json.dumps(doc))
elif args[0] == 'apply':
    if args[-1] == '-':
        sys.stdin.read()
        print('namespace/n8n configured')
    else:
        assert (state / 'probe.json').exists(), 'Workload changed before pull verification'
        print('deployment.apps/n8n configured')
elif 'get' in args and 'secret' in args:
    if (state / 'secret.json').exists():
        print('secret/eva-harbor-pull' if args[-1] == 'name' else (state / 'secret.json').read_text())
elif 'get' in args and 'pod' in args:
    print('Failed' if os.environ.get('N8N_PROBE_FAIL') == '1' else 'Succeeded')
elif 'delete' in args and 'pod' in args:
    print('pod deleted')
elif 'rollout' in args:
    print('deployment/n8n successfully rolled out')
else:
    raise AssertionError(args)
'''


class RegistryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='eva-registry-test-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.hosts_root = self.root / 'certs.d'
        self.hosts_root.mkdir()
        self.containerd = self.root / 'config.toml'
        self.containerd.write_text('[plugins."io.containerd.grpc.v1.cri".registry]\nconfig_path = ' + json.dumps(str(self.hosts_root)) + '\n')
        self.request = {'registry': REGISTRY, 'protocol': 'http', 'namespace': 'n8n',
                        'secret': 'eva-harbor-pull', 'containerd_config': str(self.containerd),
                        'config': {'configs': {REGISTRY: {'auth': {'username': 'pull-user', 'password': PASSWORD}}}}}

    def test_http_host_is_scoped_and_idempotent(self):
        other = self.hosts_root / 'other.test'
        other.mkdir()
        (other / 'hosts.toml').write_text('operator data')
        self.assertTrue(helper.prepare_transport(self.request))
        created = self.hosts_root / REGISTRY / 'hosts.toml'
        self.assertIn('http://' + REGISTRY, created.read_text())
        self.assertNotIn(PASSWORD, created.read_text())
        before = created.stat().st_mtime_ns
        self.assertFalse(helper.prepare_transport(self.request))
        self.assertEqual(before, created.stat().st_mtime_ns)
        self.assertEqual((other / 'hosts.toml').read_text(), 'operator data')

    def test_existing_conflicting_host_is_preserved(self):
        directory = self.hosts_root / REGISTRY
        directory.mkdir()
        hosts = directory / 'hosts.toml'
        hosts.write_text('server = "https://' + REGISTRY + '"\n')
        before = hosts.read_bytes()
        with self.assertRaises(ValueError):
            helper.prepare_transport(self.request)
        self.assertEqual(before, hosts.read_bytes())

    def test_existing_k3s_host_with_v2_path_is_reused(self):
        directory = self.hosts_root / REGISTRY
        directory.mkdir()
        hosts = directory / 'hosts.toml'
        hosts.write_text('server = "https://' + REGISTRY + '/v2"\n[host."http://' + REGISTRY + '/v2"]\ncapabilities = ["pull", "resolve"]\n')
        before = hosts.read_bytes()
        self.assertFalse(helper.prepare_transport(self.request))
        self.assertEqual(before, hosts.read_bytes())

    def test_containerd_v2_and_existing_https_mirror(self):
        self.containerd.write_text('[plugins."io.containerd.cri.v1.images".registry]\nconfig_path = ' + json.dumps(str(self.hosts_root)) + '\n')
        self.assertTrue(helper.prepare_transport(self.request))
        request = copy.deepcopy(self.request)
        request['protocol'] = ''
        request['config']['mirrors'] = {REGISTRY: {'endpoint': ['https://' + REGISTRY]}}
        self.assertEqual(helper.selected_endpoint(request), 'https://' + REGISTRY)
        self.assertFalse(helper.prepare_transport(request))

    def test_missing_active_config_and_symlink_are_rejected(self):
        self.containerd.write_text('version = 2\n')
        with self.assertRaises(ValueError):
            helper.prepare_transport(self.request)
        (self.hosts_root / REGISTRY).symlink_to(self.root)
        self.containerd.write_text('[plugins."io.containerd.grpc.v1.cri".registry]\nconfig_path = ' + json.dumps(str(self.hosts_root)) + '\n')
        with self.assertRaises(ValueError):
            helper.prepare_transport(self.request)

    def test_existing_secret_is_reused_without_changes(self):
        docker_config = {'auths': {REGISTRY: {'auth': 'operator-managed'}, 'other.test': {'auth': 'other'}}}
        secret = {'type': 'kubernetes.io/dockerconfigjson', 'data': {
            '.dockerconfigjson': base64.b64encode(json.dumps(docker_config).encode()).decode()}}
        before = copy.deepcopy(secret)
        with patch.object(helper, 'kubectl', return_value=secret) as command:
            self.assertFalse(helper.prepare_secret(self.request))
            self.assertEqual(command.call_count, 1)
        self.assertEqual(before, secret)

    def test_secret_uses_exact_target_auth_and_private_stdin(self):
        with patch.object(helper, 'kubectl', side_effect=[{}, {}]) as command:
            self.assertTrue(helper.prepare_secret(self.request))
            created = command.call_args.kwargs['document']
            config = json.loads(created['stringData']['.dockerconfigjson'])
            self.assertEqual(set(config['auths']), {REGISTRY})
            self.assertEqual(base64.b64decode(config['auths'][REGISTRY]['auth']).decode(), 'pull-user:' + PASSWORD)
            self.assertNotIn(PASSWORD, ' '.join(command.call_args.args))

    def test_incompatible_secret_and_partial_credentials_are_rejected(self):
        with patch.object(helper, 'kubectl', return_value={'type': 'Opaque'}):
            with self.assertRaises(ValueError):
                helper.prepare_secret(self.request)
        self.request['config'] = {}
        self.request['username'] = 'only-user'
        with patch.object(helper, 'kubectl', return_value={}):
            with self.assertRaises(ValueError):
                helper.prepare_secret(self.request)

    def test_no_credentials_can_use_public_or_active_node_auth(self):
        self.request['config'] = {}
        with patch.object(helper, 'kubectl', return_value={}):
            self.assertFalse(helper.prepare_secret(self.request))


class N8nAnsibleTests(unittest.TestCase):
    def setUp(self):
        RegistryTests.setUp(self)
        self.playbook = os.environ.get('ANSIBLE_PLAYBOOK') or shutil.which('ansible-playbook') or str(ROOT / '.venv/bin/ansible-playbook')
        self.assertTrue(Path(self.playbook).is_file(), 'ansible-playbook is required')
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        tool = self.bin / 'kubectl'
        tool.write_text('#!' + sys.executable + '\n' + FAKE_KUBECTL)
        tool.chmod(0o755)
        self.registry_config = self.root / 'registries.yaml'
        self.registry_config.write_text(yaml.safe_dump(self.request['config']))
        self.env = os.environ.copy()
        self.env.update({'PATH': str(self.bin) + os.pathsep + self.env.get('PATH', ''),
                         'ANSIBLE_ROLES_PATH': str(ROLES), 'N8N_FIXTURE': str(self.root),
                         'ANSIBLE_NOCOLOR': '1', 'ANSIBLE_LOCAL_TEMP': str(self.root / 'ansible')})

    def run_role(self, mode='remote_repository', fail=False, overrides=None):
        tasks = yaml.safe_load((ROLES / 'n8n/tasks/main.yaml').read_text())
        stop = next(index for index, task in enumerate(tasks) if task['name'] == 'Wait for n8n API endpoint to become reachable')
        tasks = tasks[:stop]
        manifest = self.root / 'n8n.yaml'
        for task in tasks:
            if task['name'] == 'Render n8n k3s manifest':
                task['ansible.builtin.copy']['dest'] = str(manifest)
            if task['name'] == 'Apply n8n k3s manifest':
                task['ansible.builtin.command']['cmd'] = 'kubectl apply -f ' + str(manifest)
            if 'ansible.builtin.include_tasks' in task:
                task['ansible.builtin.include_tasks'] = str(ROLES / 'n8n/tasks' / task['ansible.builtin.include_tasks'])
        variables = {'repository_mode': mode, 'repository_registry': REGISTRY, 'repository_project': 'eva',
                     'ansible_default_ipv4': {'address': '192.0.2.75'}, 'eva_site_config_root': str(self.root),
                     'eva_registry_config_path': str(self.registry_config),
                     'eva_registry_containerd_config': str(self.containerd),
                     'eva_registry_helper_path': str(self.root / 'registry.py')}
        variables.update(overrides or {})
        plays = [{'hosts': 'all', 'gather_facts': False, 'vars': variables,
                  'tasks': [{'ansible.builtin.include_vars': {'file': str(ROOT / 'src/solution/version.yaml')}}] + tasks}]
        source = self.root / 'play.yaml'
        source.write_text(yaml.safe_dump(plays, sort_keys=False))
        environment = self.env.copy()
        if fail:
            environment['N8N_PROBE_FAIL'] = '1'
        result = subprocess.run([self.playbook, '-i', 'server-a,', '-c', 'local', str(source)], env=environment,
                                text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=80)
        self.assertNotIn(PASSWORD, result.stdout)
        if fail:
            self.assertNotEqual(result.returncode, 0, result.stdout)
        else:
            self.assertEqual(result.returncode, 0, result.stdout)
        return list(yaml.safe_load_all(manifest.read_text())) if manifest.exists() else []

    def test_remote_role_uses_harbor_and_keeps_pvc_settings(self):
        documents = self.run_role()
        deployment = next(doc for doc in documents if doc['kind'] == 'Deployment')
        self.assertEqual(deployment['spec']['strategy']['type'], 'Recreate')
        pod = deployment['spec']['template']['spec']
        container = pod['containers'][0]
        self.assertEqual(container['image'], REGISTRY + '/eva/n8n:2.32.7')
        self.assertEqual(pod['securityContext']['fsGroup'], 1000)
        self.assertEqual(pod['imagePullSecrets'], [{'name': 'eva-harbor-pull'}])
        env = {item['name']: item['value'] for item in container['env']}
        self.assertEqual(env['WEBHOOK_URL'], 'http://192.0.2.75:30678/')
        self.assertEqual(env['N8N_HOST'], '192.0.2.75')
        self.assertEqual(container['readinessProbe']['httpGet']['path'], '/healthz/readiness')
        self.assertEqual(documents[0]['spec']['resources']['requests']['storage'], '20Gi')
        self.assertEqual(documents[-1]['spec']['ports'][0]['nodePort'], 30678)
        registry = yaml.safe_load(self.registry_config.read_text())
        self.assertEqual(registry['configs'], self.request['config']['configs'])
        self.assertEqual(registry['mirrors'][REGISTRY]['endpoint'], ['http://' + REGISTRY])

    def test_infra_registry_merge_preserves_credentials_tls_and_other_mirrors(self):
        existing = {'mirrors': {'other.test': {'endpoint': ['https://other.test']}},
                    'configs': {REGISTRY: {'auth': {'username': 'operator', 'password': PASSWORD},
                                           'tls': {'ca_file': '/etc/operator-ca.pem'}},
                                'other.test': {'auth': {'username': 'other', 'password': 'preserve'}}}}
        self.registry_config.write_text(yaml.safe_dump(existing))
        tasks = yaml.safe_load((ROOT / 'src/infra/roles/k3s/tasks/registry.yaml').read_text())
        for task in tasks[0]['block']:
            if 'ansible.builtin.service' in task:
                del task['ansible.builtin.service']
                task['ansible.builtin.set_fact'] = {'fixture_restart_count': '{{ (fixture_restart_count | default(0) | int) + 1 }}'}
        plays = [{'hosts': 'all', 'gather_facts': False,
                  'vars': {'repository_registry': REGISTRY, 'eva_registry_config_path': str(self.registry_config)},
                  'tasks': tasks + copy.deepcopy(tasks) + [{'ansible.builtin.assert': {'that': ['fixture_restart_count | int == 1']}}]}]
        source = self.root / 'infra.yaml'
        source.write_text(yaml.safe_dump(plays, sort_keys=False))
        result = subprocess.run([self.playbook, '-i', 'server-a,', '-c', 'local', str(source)], env=self.env,
                                text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=40)
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertNotIn(PASSWORD, result.stdout)
        merged = yaml.safe_load(self.registry_config.read_text())
        self.assertEqual(merged['configs'], existing['configs'])
        self.assertEqual(merged['mirrors']['other.test'], existing['mirrors']['other.test'])

    def test_closed_network_precondition_skips_all_external_probes(self):
        tasks = yaml.safe_load((ROOT / 'src/infra/roles/precondition/tasks/main.yaml').read_text())
        selected = []
        for task in tasks:
            if task['name'] in ['Set infra check parameters', 'Select external connectivity probes for the repository mode',
                                'Build AWS CLI download report']:
                selected.append(task)
            elif task['name'] in ['Check public outbound connectivity and fetch public IP',
                                  'Resolve DNS for external service domains', 'Check TCP 443 connectivity for external services',
                                  'Check HTTPS HEAD for external services', 'Check AWS CLI installer partial download']:
                task = {key: value for key, value in task.items() if not key.startswith('ansible.builtin.') and key != 'args'}
                task['ansible.builtin.fail'] = {'msg': 'Remote target attempted an external network probe'}
                selected.append(task)
        source = self.root / 'precondition.yaml'
        source.write_text(yaml.safe_dump([{'hosts': 'all', 'gather_facts': False,
            'vars': {'repository_mode': 'remote_repository', 'eva_site_config_root': str(self.root)},
            'tasks': selected + [{'ansible.builtin.assert': {'that': ['infra_check_urls | length == 0',
                                                                     'infra_aws_cli_download_report.skipped']}}]}], sort_keys=False))
        result = subprocess.run([self.playbook, '-i', 'server-a,', '-c', 'local', str(source)], env=self.env,
                                text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=40)
        self.assertEqual(result.returncode, 0, result.stdout)

    def test_online_image_selection_and_explicit_host(self):
        # The online path has no Harbor preflight Pod.
        (self.root / 'probe.json').write_text('{}')
        documents = self.run_role('online_repository', overrides={'n8n_public_host': 'n8n.example.com'})
        pod = documents[1]['spec']['template']['spec']
        self.assertEqual(pod['containers'][0]['image'], 'docker.n8n.io/n8nio/n8n:2.32.7')
        self.assertEqual(pod['imagePullSecrets'], [])
        calls = (self.root / 'calls.jsonl').read_text()
        self.assertNotIn('eva-n8n-pull-fixture', calls)

    def test_failed_pull_preserves_workload_and_removes_probe(self):
        self.assertEqual(self.run_role(fail=True), [])
        calls = [json.loads(line) for line in (self.root / 'calls.jsonl').read_text().splitlines()]
        self.assertTrue(any('delete' in call and 'eva-n8n-pull-fixture' in call for call in calls))
        self.assertFalse(any(call[0] == 'apply' and call[-1] != '-' for call in calls))


class N8nPreparationTests(unittest.TestCase):
    def test_qdrant_publishing_reuses_exact_external_docker_credential_and_protocol(self):
        with tempfile.TemporaryDirectory(prefix='eva-external-artifacts-') as temp:
            root = Path(temp)
            cache = root / 'cache'
            docker = root / 'docker'
            docker.mkdir()
            config = {'auths': {REGISTRY: {'auth': base64.b64encode(('publisher:' + PASSWORD).encode()).decode()},
                                'other.test': {'auth': 'do-not-copy'}}}
            (docker / 'config.json').write_text(json.dumps(config))
            before = (docker / 'config.json').read_bytes()
            values = cache / 'eva-agent/release/test-release/eva-agent-qdrant/values-k3s.harbor.yaml'
            values.parent.mkdir(parents=True)
            values.write_text('env:\n  - name: SNAPSHOT_SPECS\n    value: |\n      test|snapshot.bin|collection\n')
            snapshots = cache / 'qdrant-snapshots'
            snapshots.mkdir()
            (snapshots / 'snapshot.bin').write_text('fixture')
            oras = root / 'oras'
            oras.write_text('#!' + sys.executable + '\n' + r'''
import json, os, pathlib, sys
args = sys.argv[1:]
assert args[0] != 'login', 'Existing credentials must be reused'
assert '--plain-http' not in args, 'Configured HTTPS must be preserved'
config = json.loads(pathlib.Path(args[args.index('--registry-config') + 1]).read_text())
assert set(config['auths']) == {'harbor.test:32080'}, 'Unrelated credentials leaked'
with pathlib.Path(os.environ['ARTIFACT_CALLS']).open('a') as stream:
    stream.write(json.dumps(args) + '\n')
sys.exit(1 if args[0] == 'manifest' else 0)
''')
            oras.chmod(0o755)
            env = {key: value for key, value in os.environ.items()
                   if not key.startswith(('REPOSITORY_', 'HARBOR_', 'EVA_AGENT_'))}
            env.update({'EVA_CACHE_ROOT': str(cache), 'EVA_AGENT_RELEASE': 'test-release',
                        'REPOSITORY_REGISTRY': REGISTRY, 'REPOSITORY_PROJECT': 'eva',
                        'REPOSITORY_USE_DOCKER_AUTH': 'true', 'REPOSITORY_PLAIN_HTTP': 'false',
                        'DOCKER_CONFIG': str(docker), 'ORAS_CMD': str(oras), 'ARTIFACT_CALLS': str(root / 'calls'),
                        'LOCAL_HARBOR_YML': str(root / 'stale.yml')})
            (root / 'stale.yml').write_text('hostname: harbor.test\nharbor_admin_password: stale-password\n')
            result = subprocess.run(['bash', str(ROOT / 'scripts/publish/push_qdrant_snapshots_to_harbor.sh')], env=env,
                                    text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=30)
            self.assertEqual(result.returncode, 0, result.stdout)
            self.assertNotIn(PASSWORD, result.stdout)
            self.assertEqual(before, (docker / 'config.json').read_bytes())
            self.assertIn(REGISTRY + '/eva/qdrant-snapshots:test', (snapshots / 'harbor-artifacts.txt').read_text())

    def test_n8n_only_download_and_publish_need_no_chart(self):
        with tempfile.TemporaryDirectory(prefix='eva-n8n-images-') as temp:
            root = Path(temp)
            binary = root / 'bin'
            binary.mkdir()
            tool = binary / 'docker'
            tool.write_text('#!/bin/sh\ncase "$1" in\nimage) echo amd64/linux ;;\nlogin) cat >/dev/null ;;\nesac\n')
            tool.chmod(0o755)
            helm = binary / 'helm'
            helm.write_text('#!/bin/sh\necho "n8n should not render a Helm chart" >&2\nexit 1\n')
            helm.chmod(0o755)
            environment = os.environ.copy()
            # Use a different catalogue pin to prove the script does not hardcode a tag.
            catalog = root / 'version.yaml'
            catalog.write_text((ROOT / 'src/solution/version.yaml').read_text().replace('n8n:2.32.7', 'n8n:2.32.8'))
            environment.update({'PATH': str(binary) + os.pathsep + environment.get('PATH', ''),
                                'EVA_CACHE_ROOT': str(root / 'cache'), 'COMPONENTS': 'n8n',
                                'EVA_SOLUTION_VERSION_FILE': str(catalog), 'N8N_IMAGE': 'docker.n8n.io/n8nio/n8n:2.32.8',
                                'REPOSITORY_REGISTRY': REGISTRY, 'REPOSITORY_PROJECT': 'eva',
                                'REPOSITORY_AUTO_LOGIN': 'false', 'REPOSITORY_MIRROR_PATH_IMAGES': 'false'})
            result = subprocess.run(['bash', str(ROOT / 'scripts/download/download_eva_images.sh')], env=environment,
                                    text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=30)
            self.assertEqual(result.returncode, 0, result.stdout)
            images = root / 'cache/images'
            self.assertEqual((images / 'images-pulled.txt').read_text(), 'docker.n8n.io/n8nio/n8n:2.32.8\n')
            result = subprocess.run(['bash', str(ROOT / 'scripts/publish/push_images_to_repository.sh')], env=environment,
                                    text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=30)
            self.assertEqual(result.returncode, 0, result.stdout)
            self.assertIn(REGISTRY + '/eva/n8n:2.32.8', (images / 'repository-mapping.txt').read_text())


if __name__ == '__main__':
    unittest.main()
