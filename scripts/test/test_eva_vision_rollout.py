#!/usr/bin/env python3
"""Exercise Vision rollout budgets and mode-specific failures without a live cluster."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
MAIN = ROOT / 'src/solution/roles/eva_vision/tasks/main.yaml'
MODES = ['online_repository', 'cloud_repository', 'remote_repository', 'local_repository']
KUBECTL = r'''
import json, os, pathlib, sys
args = sys.argv[1:]
with (pathlib.Path(os.environ['VISION_FIXTURE']) / 'calls.jsonl').open('a') as stream:
    stream.write(json.dumps(args) + '\n')
if args[:2] == ['rollout', 'status']:
    budget = int(next(arg for arg in args if arg.startswith('--timeout=')).split('=')[1][:-1])
    # Reproduce a successful 42-minute first image pull exceeding the former 30-minute budget.
    if budget < 2520 or os.environ['VISION_ROLLOUT_FAIL'] == '1':
        print('error: timed out waiting for the condition', file=sys.stderr)
        raise SystemExit(1)
    print('deployment "eva-vision" successfully rolled out')
elif args[:2] == ['get', 'pod']:
    if any(arg.startswith('custom-columns=') for arg in args):
        print('NAME PHASE READY WAITING TERMINATED IMAGE IMAGE_ID')
        print('eva-vision-fixture Pending false ContainerCreating <none> vision:3.3.0 <none>')
    else:
        print('eva-vision-fixture 0/1 ContainerCreating')
elif args[:2] == ['get', 'events']:
    print('Normal Pulling pod/eva-vision-fixture Pulling image vision:3.3.0')
elif args[:2] == ['describe', 'pod']:
    print('Name: eva-vision-fixture\nState: Waiting\nReason: ContainerCreating')
else:
    raise AssertionError(args)
'''


class VisionRolloutTests(unittest.TestCase):
    def execute(self, mode, *, fail=False, timeout=None, deadline=None):
        with tempfile.TemporaryDirectory(prefix='eva-vision-rollout-') as temporary:
            work = Path(temporary)
            binary = work / 'bin'
            binary.mkdir()
            kubectl = binary / 'kubectl'
            kubectl.write_text('#!' + sys.executable + '\n' + KUBECTL)
            kubectl.chmod(0o700)
            definitions = yaml.safe_load(MAIN.read_text())
            tasks = [next(task for task in definitions if task['name'] == name) for name in [
                'Resolve derived EVA Vision install paths', 'Wait for EVA Vision deployment rollout']]
            variables = {
                'repository_mode': mode, 'eva_vision_name': 'eva-vision', 'eva_vision_namespace': 'eva-vision',
                'eva_vision_deploy_version': '3.3.0', 'eva_vision_chart_version': '3.3.0',
                'eva_vision_offline_dir': str(work / 'cache'), 'eva_vision_kubeconfig': str(work / 'kubeconfig'),
                'eva_vision_image_repository': 'fallback.test/eva-vision', 'eva_vision_image_tag': '3.3.0',
                'eva_vision_runtime_values': {'image': {'repository': 'selected.test/eva-vision', 'tag': '3.3.0'}},
            }
            if timeout is not None:
                variables['eva_vision_rollout_timeout'] = timeout
            if deadline is not None:
                variables['eva_vision_progress_deadline_seconds'] = deadline
            tasks.insert(1, {'ansible.builtin.assert': {'that': [
                'eva_vision_progress_deadline_seconds | int == ' + str(deadline or 7200)]}})
            playbook = work / 'play.yaml'
            playbook.write_text(yaml.safe_dump([{'hosts': 'localhost', 'connection': 'local', 'gather_facts': False,
                                                'vars': variables, 'tasks': tasks}], sort_keys=False))
            environment = {**os.environ, 'PATH': str(binary) + os.pathsep + os.environ['PATH'],
                           'VISION_FIXTURE': str(work), 'VISION_ROLLOUT_FAIL': '1' if fail else '0',
                           'ANSIBLE_NOCOLOR': '1', 'ANSIBLE_STDOUT_CALLBACK': 'default'}
            ansible = os.environ.get('ANSIBLE_PLAYBOOK') or shutil.which('ansible-playbook')
            self.assertTrue(ansible, 'ansible-playbook is required')
            result = subprocess.run([ansible, '-i', 'localhost,', str(playbook)], env=environment,
                                    text=True, capture_output=True, timeout=60)
            calls = [json.loads(line) for line in (work / 'calls.jsonl').read_text().splitlines()]
            return result, calls

    def test_first_large_image_pull_fits_the_default_budget_in_all_modes(self):
        for mode in MODES:
            with self.subTest(mode=mode):
                result, calls = self.execute(mode)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(calls, [['rollout', 'status', 'deployment/eva-vision', '-n', 'eva-vision', '--timeout=7200s']])

    def test_failure_reports_the_selected_mode_image_and_container_stage(self):
        for mode in MODES:
            with self.subTest(mode=mode):
                result, calls = self.execute(mode, fail=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('repository_mode=' + mode, result.stdout)
                self.assertIn('image=selected.test/eva-vision:3.3.0', result.stdout)
                self.assertIn('ContainerCreating', result.stdout)
                self.assertIn('IMAGE_ID', result.stdout)
                self.assertIn('명령 종료 후에도 계속될 수', result.stdout)
                if mode in ['online_repository', 'cloud_repository']:
                    self.assertIn('Online 이미지', result.stdout)
                    self.assertNotIn('Harbor', result.stdout)
                    self.assertNotIn('remote_repository', result.stdout)
                else:
                    self.assertIn('Harbor의 이미지', result.stdout)
                    self.assertNotIn('Online 이미지', result.stdout)
                self.assertEqual(len(calls), 5)

    def test_explicit_wait_and_progress_budgets_are_preserved(self):
        result, calls = self.execute('online_repository', timeout='1800s', deadline=9000)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('--timeout=1800s', calls[0])
        self.assertIn('did not complete within 1800s', result.stdout)


if __name__ == '__main__':
    unittest.main(verbosity=2)
