#!/usr/bin/env python3
"""Exercise target jq preparation without installing packages on the test host."""
import copy
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
ROLE = ROOT / 'src/solution/roles/eva_solution_prerequisites'


class SolutionPrerequisiteTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.jq = shutil.which('jq')
        if not shutil.which('ansible-playbook') or not cls.jq:
            raise unittest.SkipTest('Ansible and jq are required')

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='eva-solution-prerequisites-')
        self.addCleanup(temporary.cleanup)
        self.work = Path(temporary.name)
        self.binary = self.work / 'bin'
        self.binary.mkdir()
        self.jq_target = self.binary / 'jq'
        self.jq_target.write_text('#!/bin/sh\nexit 127\n')
        self.jq_target.chmod(0o755)
        self.helper = self.work / 'eva'
        self.called = self.work / 'repository-checked'
        self.helper.write_text('#!/bin/sh\n'
                               '[ "$*" = "internal ensure-apt-repositories" ] || exit 9\n'
                               f'printf checked > "{self.called}"\n'
                               'if [ "$EVA_PREREQUISITE_TEST_FAIL" = yes ]; then\n'
                               '  echo "APT repository validation failed" >&2; exit 1\n'
                               'fi\n'
                               'echo \'{"changed":false}\'\n')
        self.helper.chmod(0o755)
        self.marker = self.work / 'workload-stage'
        definitions = copy.deepcopy(yaml.safe_load((ROLE / 'tasks/main.yaml').read_text()))
        # Keep the actual repository helper, probe and branch conditions. Replace
        # only apt's package mutation with a fixture binary publication.
        installation = definitions[1]['block'][1]
        self.assertEqual(installation.pop('ansible.builtin.apt'), {
            'name': 'jq', 'state': 'present', 'update_cache': False})
        installation['ansible.builtin.copy'] = {
            'src': self.jq, 'dest': str(self.jq_target), 'mode': '0755'}
        definitions.append({'name': 'Enter subsequent workload changes', 'ansible.builtin.copy': {
            'dest': str(self.marker), 'content': 'ready'}})
        self.tasks = definitions

    def execute(self, mode='online_repository', fail=False, check=False):
        play = self.work / 'play.yaml'
        play.write_text(yaml.safe_dump([{
            'hosts': 'localhost', 'connection': 'local', 'gather_facts': False,
            'vars': {'eva_infra_root': str(ROOT / 'src/infra'),
                     'eva_solution_root': str(ROOT / 'src/solution'), 'repository_mode': mode,
                     'ansible_facts': {'system': 'Linux', 'architecture': 'x86_64'}},
            'environment': {'PATH': str(self.binary) + os.pathsep + os.environ['PATH'],
                            'EVA_PREREQUISITE_TEST_FAIL': 'yes' if fail else 'no'},
            'tasks': self.tasks,
        }], sort_keys=False))
        env = dict(os.environ, EVA_TOOL_EXECUTABLE=str(self.helper),
                   ANSIBLE_LOG_PATH=os.devnull, ANSIBLE_NOCOLOR='1')
        command = ['ansible-playbook', '-i', 'localhost,', str(play)]
        if check:
            command.append('--check')
        return subprocess.run(command, env=env, capture_output=True, text=True, timeout=45)

    def test_online_missing_jq_is_prepared_before_workload_changes_and_reused(self):
        result = self.execute()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(self.called.exists())
        self.assertTrue(self.marker.exists())
        self.assertEqual(subprocess.check_output([str(self.jq_target), '-n', '-e', 'true'], text=True).strip(), 'true')
        self.called.unlink()
        self.marker.unlink()
        result = self.execute()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse(self.called.exists(), 'Existing jq must not trigger another APT refresh')
        self.assertTrue(self.marker.exists())

    def test_repository_failure_stops_before_package_or_workload_changes(self):
        result = self.execute(fail=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('APT repository validation failed', result.stdout + result.stderr)
        self.assertTrue(self.called.exists())
        self.assertEqual(subprocess.run([str(self.jq_target)]).returncode, 127)
        self.assertFalse(self.marker.exists())

    def test_existing_jq_is_reused_without_apt_in_online_and_offline_modes(self):
        shutil.copy2(self.jq, self.jq_target)
        for mode in ('online_repository', 'local_repository'):
            with self.subTest(mode=mode):
                self.helper.unlink(missing_ok=True)
                result = self.execute(mode=mode)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertFalse(self.called.exists())

    def test_offline_missing_jq_stops_before_workload_changes_without_network(self):
        self.helper.unlink()
        result = self.execute(mode='local_repository')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('jq is unavailable', result.stdout + result.stderr)
        self.assertFalse(self.called.exists())
        self.assertFalse(self.marker.exists())

    def test_check_mode_does_not_prepare_packages_or_change_workloads(self):
        self.helper.unlink()
        result = self.execute(check=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse(self.called.exists())
        self.assertFalse(self.marker.exists())
        self.assertEqual(subprocess.run([str(self.jq_target)]).returncode, 127)

    def test_config_and_each_jq_consumer_prepare_the_same_prerequisite_first(self):
        for name in ('config', 'eva_agent', 'eva_vision', 'eva_app'):
            with self.subTest(role=name):
                task = yaml.safe_load((ROOT / f'src/solution/roles/{name}/tasks/main.yaml').read_text())[0]
                self.tasks = [task, {'name': 'Enter subsequent workload changes', 'ansible.builtin.copy': {
                    'dest': str(self.marker), 'content': 'ready'}}]
                # Exercise the real role import, not the isolated apt fixture.
                # Offline missing-tool failure proves it cannot reach mutations.
                self.jq_target.write_text('#!/bin/sh\nexit 127\n')
                self.marker.unlink(missing_ok=True)
                result = self.execute(mode='local_repository')
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('jq is unavailable', result.stdout + result.stderr)
                self.assertFalse(self.marker.exists())
                shutil.copy2(self.jq, self.jq_target)
                result = self.execute()
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertTrue(self.marker.exists())
                self.assertFalse(self.called.exists())


if __name__ == '__main__':
    unittest.main(verbosity=2)
