#!/usr/bin/env python3
"""Exercise installer MIG arguments and boot policy without GPU/system writes."""
import copy
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
ROLE = ROOT / 'src/infra/roles/gpu_prepare'


class InstallerMIGSelectionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not shutil.which('ansible-playbook'):
            raise unittest.SkipTest('Ansible is required')

    def execute(self, enabled, success=True, full=False):
        with tempfile.TemporaryDirectory(prefix='eva-mig-selection-') as directory:
            work = Path(directory)
            binary = work / 'bin'
            binary.mkdir()
            shutil.copy(ROLE / 'files/prepare_gpu.py', binary / 'eva-gpu-prepare.py')
            smi = binary / 'nvidia-smi'
            smi.write_text(f'''#!{sys.executable}
import json, pathlib, sys
args = sys.argv[1:]
with pathlib.Path({str(work / 'calls.jsonl')!r}).open('a') as output:
    output.write(json.dumps(args) + "\\n")
if '--query-gpu=index,uuid,name,mig.mode.current,display_mode' in args:
    print('0, GPU-test, NVIDIA RTX PRO 6000 Blackwell Server Edition, {"Enabled" if enabled is True else "Disabled"}, Disabled')
elif '-lgi' in args:
    for i in range(4): print(f'| 0 MIG 2g.24gb 67 {{i}} {{i * 2}}:2 |')
elif '-lci' in args:
    for i in range(4): print(f'| 0 {{i}} MIG 2g.24gb 0 0 0:2 |')
else:
    raise AssertionError('Unexpected GPU mutation: ' + repr(args))
''')
            smi.chmod(0o755)
            definitions = yaml.safe_load((ROLE / 'tasks/prepare.yaml').read_text())
            validation = yaml.safe_load((ROLE / 'tasks/main.yaml').read_text())[0]
            selected = {'Load the Release GPU model map', 'Ensure GPU configuration directory',
                        'Save the Release GPU model map', 'Resolve GPU helper MIG selection arguments',
                        'Inspect GPU layout before any device changes', 'Resolve GPU preparation state'}
            tasks = [validation] + [copy.deepcopy(task) for task in definitions if full or task['name'] in selected]

            def isolate(value):
                if isinstance(value, dict):
                    return {key: isolate(item) for key, item in value.items() if key not in ('owner', 'group')}
                if isinstance(value, list):
                    return [isolate(item) for item in value]
                if isinstance(value, str):
                    return value.replace('/usr/local/libexec', str(binary)).replace('/etc/eva', str(work / 'config')).replace('/etc/systemd/system', str(work / 'systemd'))
                return value

            tasks = isolate(tasks)
            for task in tasks:
                if task.get('ansible.builtin.copy', {}).get('src') == 'prepare_gpu.py':
                    task['ansible.builtin.copy']['src'] = str(ROLE / 'files/prepare_gpu.py')
                # Never invoke the host's real systemd, selector or Kubernetes.
                if task['name'] in ('Supply Display Mode Selector only when the GPU needs it',
                                    'Apply missing GPU mode and MIG instances',
                                    'Enable MIG restoration without restarting GPU workloads',
                                    "Verify this target's Kubernetes MIG resources"):
                    for key in list(task):
                        if key.startswith('ansible.builtin.') or key in ('changed_when',):
                            del task[key]
                    task['ansible.builtin.fail'] = {'msg': 'Unexpected GPU or system mutation in whole-GPU path'}
            tasks.append({'ansible.builtin.assert': {'that': ['eva_gpu_state.needs_mig == ' + ('true' if enabled is True else 'false')]}})
            playbook = work / 'playbook.yaml'
            playbook.write_text(yaml.safe_dump([{
                'hosts': 'localhost', 'connection': 'local', 'gather_facts': False,
                'vars': {'eva_gpu_mig_enabled': enabled, 'eva_infra_root': str(ROOT / 'src/infra'),
                         'eva_cache_root': str(work), 'eva_enabled_components': 'agent'},
                'environment': {'PATH': str(binary) + ':' + os.environ['PATH']}, 'tasks': tasks,
            }]))
            environment = dict(os.environ, ANSIBLE_LOG_PATH=os.devnull, ANSIBLE_NOCOLOR='1')
            result = subprocess.run(['ansible-playbook', '-i', 'localhost,', str(playbook)],
                                    capture_output=True, text=True, env=environment, timeout=60)
            output = result.stdout + result.stderr
            self.assertEqual(result.returncode == 0, success, output)
            if success:
                policy = json.loads((work / 'config/gpu-mig-models.json').read_text())
                self.assertIs(policy['_eva_mig_enabled'], enabled)
                self.assertFalse((work / 'systemd/gpu-mig-auto-enable.service').exists())
                if enabled is False:
                    calls = (work / 'calls.jsonl').read_text().splitlines()
                    self.assertEqual(len(calls), 1, calls)
                    # Old boot units do not pass --mig. The saved choice must
                    # still prevent re-enabling MIG after a reboot.
                    boot = subprocess.run([sys.executable, str(binary / 'eva-gpu-prepare.py'),
                                           '--model-map', str(work / 'config/gpu-mig-models.json'),
                                           '--apply', '--gpu-uuid', 'GPU-test'],
                                          capture_output=True, text=True,
                                          env=dict(os.environ, PATH=str(binary) + ':' + os.environ['PATH']), timeout=10)
                    self.assertEqual(boot.returncode, 0, boot.stderr)
                    self.assertFalse(json.loads(boot.stdout)['needs_mig'])
            else:
                self.assertFalse((work / 'calls.jsonl').exists())

    def test_opt_out_skips_gpu_selector_systemd_and_kubernetes_changes(self):
        self.execute(False, full=True)

    def test_opt_in_is_passed_to_inspection_and_saved_for_boot(self):
        self.execute(True)

    def test_string_boolean_is_rejected_before_any_device_command(self):
        self.execute('false', success=False)


if __name__ == '__main__':
    unittest.main(verbosity=2)
