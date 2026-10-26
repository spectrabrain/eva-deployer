#!/usr/bin/env python3
"""Exercise Agent tool preparation and Helm post-rendering without a cluster."""
import hashlib
import os
from pathlib import Path
import pwd
import re
import shutil
import socket
import subprocess
import tempfile
import tarfile
import time
import unittest
import urllib.request

import yaml

ROOT = Path(__file__).resolve().parents[2]
ROLE = ROOT / 'src/solution/roles/eva_agent'


class ManagedKustomizeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not shutil.which('ansible-playbook'):
            raise unittest.SkipTest('Ansible is required')
        cls.kustomize = os.environ.get('EVA_TEST_KUSTOMIZE') or shutil.which('kustomize')
        cls.helm = shutil.which('helm')
        # A Snap launcher cannot be relocated into a Runtime fixture. Use its
        # standalone payload when available; CI uses the pinned download below.
        if cls.kustomize and Path(cls.kustomize).resolve().name == 'snap':
            native = Path('/snap/kustomize/current/kustomize')
            cls.kustomize = str(native) if native.is_file() else None
        if os.environ.get('EVA_TEST_HELM_DOWNLOAD') == '1':
            assets = tempfile.TemporaryDirectory(prefix='eva-managed-tools-fixture-')
            cls.addClassCleanup(assets.cleanup)
            spec = (ROOT / 'tools/eva/internal/runtime/bootstrap_online.go').read_text()
            for name in ('kustomize', 'helm'):
                artifact = re.search(r'Name:\s+"' + name + r'",\s+URL:\s+"([^"]+)".*?'
                                     r'SHA256:\s+"([^"]+)".*?ArchivePath:\s+"([^"]+)"', spec, re.S)
                if artifact is None:
                    raise AssertionError('Missing Managed Runtime artifact for ' + name)
                url, checksum, member = artifact.groups()
                archive = Path(assets.name) / (name + '.tar.gz')
                urllib.request.urlretrieve(url, archive)
                if hashlib.sha256(archive.read_bytes()).hexdigest() != checksum:
                    raise AssertionError('Managed Runtime download checksum mismatch for ' + name)
                binary = Path(assets.name) / name
                with tarfile.open(archive) as bundle:
                    binary.write_bytes(bundle.extractfile(member).read())
                binary.chmod(0o755)
                setattr(cls, name, str(binary))
        if not cls.kustomize:
            raise unittest.SkipTest('Use EVA_TEST_KUSTOMIZE or EVA_TEST_HELM_DOWNLOAD=1 for kustomize')

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='eva-agent-target-tools-')
        self.addCleanup(temporary.cleanup)
        self.work = Path(temporary.name)
        self.runtime = self.work / 'controller-runtime'
        self.source = self.runtime / 'bin/selected-kustomize'
        self.source.parent.mkdir(parents=True)
        shutil.copy2(self.kustomize, self.source)
        (self.runtime / 'runtime.yaml').write_text(yaml.safe_dump({
            'schema_version': 'v1', 'version': 'fixture',
            'tools': {'kustomize': 'bin/selected-kustomize'},
        }))
        self.variables = {'eva_agent_workdir': str(self.work / 'agent'),
                          'eva_agent_target_tools_root': str(self.work / 'target-tools')}
        self.plugin = self.work / 'agent/plugin/eva-agent-qdrant'
        self.plugin.mkdir(parents=True)
        # This has the same executable lookup and stdin contract as the
        # upstream plugin; real kustomize must patch the rendered workload.
        (self.plugin / 'post-renderer.sh').write_text('''#!/bin/bash
set -euo pipefail
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
cat > "$tmp_dir/all.yaml"
cat > "$tmp_dir/kustomization.yaml" <<'EOF'
resources:
  - all.yaml
patches:
  - target:
      kind: StatefulSet
    patch: |-
      - op: add
        path: /spec/template/spec/serviceAccountName
        value: sa-eva-agent
EOF
kustomize build "$tmp_dir"
''')
        (self.plugin / 'post-renderer.sh').chmod(0o755)
        self.inventory = self.work / 'inventory.ini'
        self.inventory.write_text('[eva]\nfixture ansible_connection=local\n')
        self.environment = dict(os.environ, EVA_RUNTIME_ROOT=str(self.runtime),
                                ANSIBLE_CONFIG=str(ROOT / 'ansible.cfg'),
                                ANSIBLE_NOCOLOR='1', ANSIBLE_LOG_PATH=os.devnull)
        self.target_path = '/usr/bin:/bin'

    def execute(self, valid=True, wrapper=False):
        tasks = [{'ansible.builtin.import_tasks': str(ROLE / 'tasks/managed_kustomize.yaml')}]
        if wrapper:
            tasks += [{'ansible.builtin.import_tasks': str(ROLE / 'tasks/managed_qdrant_postrenderer.yaml')}]
        marker = self.work / 'workload-stage'
        marker.unlink(missing_ok=True)
        tasks += [{'name': 'Enter subsequent storage and workload preparation',
                   'ansible.builtin.copy': {'dest': str(marker), 'content': 'ready'}}]
        playbook = self.work / 'playbook.yaml'
        playbook.write_text(yaml.safe_dump([{
            'hosts': 'eva', 'gather_facts': False, 'vars': self.variables,
            'environment': {'PATH': self.target_path}, 'tasks': tasks,
        }], sort_keys=False))
        result = subprocess.run(['ansible-playbook', '-i', str(self.inventory), str(playbook)],
                                env=self.environment, text=True, capture_output=True, timeout=60)
        self.assertEqual(result.returncode == 0, valid, result.stdout + result.stderr)
        self.assertEqual(marker.exists(), valid, 'Tool failure reached storage/workload preparation')
        return result

    def test_local_install_uses_validated_controller_binary(self):
        canonical = self.source.with_name('kustomize')
        self.source.rename(canonical)
        (self.runtime / 'runtime.yaml').write_text('schema_version: v1\ntools:\n  kustomize: bin/kustomize\n')
        self.execute()
        self.assertFalse((self.work / 'target-tools').exists())

    def test_invalid_controller_binary_fails_before_storage_or_workloads(self):
        self.source.unlink()
        self.execute(valid=False)
        self.assertFalse((self.work / 'target-tools').exists())

    def test_invalid_descriptor_path_is_rejected(self):
        (self.runtime / 'runtime.yaml').write_text('schema_version: v1\ntools:\n  kustomize: ../outside\n')
        self.execute(valid=False)

    def test_both_helm_postrenderer_interfaces_use_managed_kustomize(self):
        helm = self.helm
        if not helm:
            self.skipTest('Helm is required')
        self.execute(wrapper=True)
        # Preparing the plugin twice must not replace the upstream script
        # with the wrapper itself and produce recursive execution.
        self.execute(wrapper=True)
        chart = self.work / 'chart'
        (chart / 'templates').mkdir(parents=True)
        (chart / 'Chart.yaml').write_text('apiVersion: v2\nname: fixture\nversion: 1.0.0\n')
        (chart / 'templates/statefulset.yaml').write_text('''apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: eva-agent-qdrant
spec:
  template:
    spec:
      containers: []
''')
        environment = dict(os.environ, PATH=self.target_path)
        # File execution is the Helm 3 interface, and also proves the wrapper
        # works without a controller Runtime PATH in the calling process.
        result = subprocess.run([str(self.plugin / 'post-renderer.sh')],
                                input=(chart / 'templates/statefulset.yaml').read_text(),
                                env=environment, text=True, capture_output=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('serviceAccountName: sa-eva-agent', result.stdout)
        version = subprocess.check_output([helm, 'version', '--template', '{{.Version}}'], text=True)
        argument = str(self.plugin / 'post-renderer.sh')
        if version.startswith('v4.'):
            (self.plugin / 'plugin.yaml').write_text('''apiVersion: v1
type: postrenderer/v1
name: eva-agent-qdrant-postrenderer
version: 0.2.0
runtime: subprocess
runtimeConfig:
  platformCommand:
    - command: ${HELM_PLUGIN_DIR}/post-renderer.sh
''')
            argument = 'eva-agent-qdrant-postrenderer'
            environment['HELM_PLUGINS'] = str(self.plugin.parent)
        result = subprocess.run([helm, 'template', 'fixture', str(chart), '--post-renderer', argument],
                                env=environment, text=True, capture_output=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('serviceAccountName: sa-eva-agent', result.stdout)

    def test_ssh_target_receives_binary_without_system_kustomize(self):
        sshd = shutil.which('sshd')
        if not sshd or not shutil.which('ssh-keygen'):
            self.skipTest('OpenSSH server is unavailable')
        key = self.work / 'client-key'
        host_key = self.work / 'host-key'
        for path in (key, host_key):
            subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(path)], check=True)
        authorized = self.work / 'authorized_keys'
        authorized.write_bytes(key.with_suffix('.pub').read_bytes())
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            port = listener.getsockname()[1]
        user = pwd.getpwuid(os.getuid()).pw_name
        config = self.work / 'sshd_config'
        config.write_text(f'''Port {port}
ListenAddress 127.0.0.1
HostKey {host_key}
PidFile {self.work}/sshd.pid
AuthorizedKeysFile {authorized}
StrictModes no
UsePAM no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
PermitRootLogin yes
AllowUsers {user}
Subsystem sftp internal-sftp
''')
        log = (self.work / 'sshd.log').open('w+')
        self.addCleanup(log.close)
        server = subprocess.Popen([sshd, '-D', '-e', '-f', str(config)], stdout=log, stderr=log)
        self.addCleanup(lambda: server.wait(timeout=5))
        self.addCleanup(server.terminate)
        for _ in range(60):
            if server.poll() is not None:
                log.seek(0)
                self.skipTest('Isolated sshd cannot start: ' + log.read().strip())
            try:
                connection = socket.create_connection(('127.0.0.1', port), timeout=0.1)
                connection.close()
                break
            except OSError:
                time.sleep(0.05)
        known_hosts = self.work / 'known_hosts'
        host_public = host_key.with_suffix('.pub').read_text().split()
        known_hosts.write_text(f'[127.0.0.1]:{port} {host_public[0]} {host_public[1]}\n')
        self.inventory.write_text(yaml.safe_dump({'eva': {'hosts': {'fixture': {
            'ansible_host': '127.0.0.1', 'ansible_port': port, 'ansible_user': user,
            'ansible_connection': 'ssh', 'ansible_ssh_private_key_file': str(key),
            'ansible_ssh_common_args': f'-F /dev/null -o UserKnownHostsFile={known_hosts} -o GlobalKnownHostsFile=/dev/null -o ControlMaster=no',
            'ansible_python_interpreter': shutil.which('python3'),
        }}}}))
        # YAML plugin requires the inventory's normal filename suffix.
        renamed = self.inventory.with_suffix('.yaml')
        self.inventory.rename(renamed)
        self.inventory = renamed
        self.execute(wrapper=True)
        checksum = hashlib.sha256(self.source.read_bytes()).hexdigest()
        transferred = self.work / 'target-tools' / checksum / 'kustomize'
        self.assertEqual(transferred.read_bytes(), self.source.read_bytes())
        before = transferred.stat().st_mtime_ns
        self.execute(wrapper=True)
        self.assertEqual(transferred.stat().st_mtime_ns, before, 'Retry replaced the identical tool')
        self.assertIn(str(transferred.parent), (self.plugin / 'post-renderer.sh').read_text())

    def test_tool_check_precedes_credential_and_storage_changes(self):
        tasks = yaml.safe_load((ROLE / 'tasks/dependencies.yaml').read_text())
        names = [task.get('name') for task in tasks]
        tool_check = names.index('Prepare and validate EVA Agent managed kustomize on the target')
        for name in ('Prepare namespace registry and model credentials before deploying Agent workloads',
                     'Ensure NFS share and cache directories exist',
                     'Remove EVA dependency workspace if it exists'):
            self.assertLess(tool_check, names.index(name))


if __name__ == '__main__':
    unittest.main(verbosity=2)
