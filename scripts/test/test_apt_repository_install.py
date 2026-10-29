#!/usr/bin/env python3
"""Exercise target APT helper routing without changing system repositories."""
import hashlib
import os
from pathlib import Path
import pwd
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
ROLE = ROOT / 'src/infra/roles/base'


class APTRepositoryInstallationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not shutil.which('ansible-playbook'):
            raise unittest.SkipTest('Ansible is required')

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='eva-apt-target-')
        self.addCleanup(temporary.cleanup)
        self.work = Path(temporary.name)
        self.source = self.work / 'controller-only-eva'
        self.called = self.work / 'helper-called'
        self.packages = self.work / 'package-stage'
        self.source.write_text(f'''#!{sys.executable}
import json, os, pathlib, sys
assert sys.argv[1:] == ["internal", "ensure-apt-repositories"]
pathlib.Path({str(self.called)!r}).write_text(str(pathlib.Path(sys.argv[0]).resolve()))
if os.environ["EVA_APT_TEST_RESULT"] == "failed":
    print("APT repository validation failed", file=sys.stderr)
    raise SystemExit(1)
print(json.dumps({{"changed": os.environ["EVA_APT_TEST_RESULT"] == "repaired"}}))
''')
        self.source.chmod(0o755)
        self.variables = {'eva_apt_target_tools_root': str(self.work / 'target-tools')}
        self.inventory = self.work / 'inventory.yaml'
        self.inventory.write_text(yaml.safe_dump({'eva': {'hosts': {'fixture': {
            'ansible_connection': 'local',
            'ansible_python_interpreter': sys.executable,
        }}}}))
        self.environment = dict(os.environ, EVA_TOOL_EXECUTABLE=str(self.source),
                                ANSIBLE_CONFIG=str(ROOT / 'ansible.cfg'),
                                ANSIBLE_NOCOLOR='1', ANSIBLE_LOG_PATH=os.devnull)

    def execute(self, result='healthy', base_gate=False, check=False, success=True):
        task = {'ansible.builtin.import_tasks': str(ROLE / 'tasks/apt_repositories.yaml')}
        if base_gate:
            task = next(t for t in yaml.safe_load((ROLE / 'tasks/main.yaml').read_text())
                        if t.get('ansible.builtin.import_tasks') == 'apt_repositories.yaml')
            task['ansible.builtin.import_tasks'] = str(ROLE / 'tasks/apt_repositories.yaml')
        tasks = [task, {'name': 'Enter package installation after repository validation',
                        'ansible.builtin.copy': {'dest': str(self.packages), 'content': 'ready'}}]
        playbook = self.work / 'playbook.yaml'
        playbook.write_text(yaml.safe_dump([{
            'hosts': 'eva', 'become': False, 'vars': self.variables,
            'environment': {'EVA_APT_TEST_RESULT': result, 'PATH': '/usr/bin:/bin'},
            'tasks': tasks,
        }]))
        self.packages.unlink(missing_ok=True)
        command = ['ansible-playbook', '-i', str(self.inventory), str(playbook)]
        if check:
            command.append('--check')
        completed = subprocess.run(command, env=self.environment, capture_output=True,
                                   text=True, timeout=60)
        output = completed.stdout + completed.stderr
        self.assertEqual(completed.returncode == 0, success, output)
        if not check:
            self.assertEqual(self.packages.exists(), success, output)
        return output

    def test_local_install_repairs_and_continues_without_target_copy(self):
        output = self.execute(result='repaired')
        self.assertEqual(self.called.read_text(), str(self.source))
        self.assertFalse((self.work / 'target-tools').exists())
        self.assertIn('changed: [fixture]', output)

    def test_failed_repository_validation_stops_before_package_installation(self):
        output = self.execute(result='failed', success=False)
        self.assertIn('APT repository validation failed', output)
        self.assertNotIn('from_json', output)

    def test_offline_and_check_mode_skip_repository_repair(self):
        self.environment['EVA_TOOL_EXECUTABLE'] = ''
        for offline, check in ((True, False), (False, True)):
            self.variables['airgap_mode'] = offline
            self.execute(base_gate=True, check=check)
            self.assertFalse(self.called.exists())
            self.assertFalse((self.work / 'target-tools').exists())

    def test_ssh_install_uses_the_exact_transferred_helper_and_reuses_it(self):
        sshd = shutil.which('sshd')
        if not sshd or not shutil.which('ssh-keygen'):
            self.skipTest('OpenSSH server is unavailable')
        key, host_key = self.work / 'client-key', self.work / 'host-key'
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
                with socket.create_connection(('127.0.0.1', port), timeout=0.1):
                    break
            except OSError:
                time.sleep(0.05)
        known_hosts = self.work / 'known_hosts'
        host_public = host_key.with_suffix('.pub').read_text().split()
        known_hosts.write_text(f'[127.0.0.1]:{port} {host_public[0]} {host_public[1]}\n')
        self.inventory.write_text(yaml.safe_dump({'eva': {'hosts': {'fixture': {
            'ansible_connection': 'ssh', 'ansible_host': '127.0.0.1', 'ansible_port': port,
            'ansible_user': user, 'ansible_ssh_private_key_file': str(key),
            'ansible_ssh_common_args': f'-F /dev/null -o UserKnownHostsFile={known_hosts} -o GlobalKnownHostsFile=/dev/null -o ControlMaster=no',
            'ansible_python_interpreter': sys.executable,
        }}}}))
        self.execute(result='repaired')
        checksum = hashlib.sha256(self.source.read_bytes()).hexdigest()
        helper = self.work / 'target-tools' / checksum / 'eva'
        self.assertEqual(helper.read_bytes(), self.source.read_bytes())
        self.assertEqual(self.called.read_text(), str(helper))
        before = helper.stat().st_mtime_ns
        output = self.execute(result='healthy')
        self.assertEqual(helper.stat().st_mtime_ns, before)
        self.assertIn('ok: [fixture]', output)


if __name__ == '__main__':
    unittest.main(verbosity=2)
