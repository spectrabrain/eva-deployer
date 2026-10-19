"""Check actual Ansible task arguments against isolated GPU command doubles."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[1]
ANSIBLE = os.environ.get("EVA_TEST_ANSIBLE", shutil.which("ansible-playbook"))


@unittest.skipUnless(ANSIBLE, "ansible-playbook is required")
class AnsibleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="eva-mig-ansible-test-")
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.state = self.directory / "state.json"
        self.state.write_text(json.dumps({"mode": "Enabled", "instances": 4}))
        command_double = f"""#!{sys.executable}
import json, sys
from pathlib import Path
sys.path.insert(0, {str(ROOT / 'tests')!r})
from test_gpu_mig_configure import FakeGPU
state_path=Path({str(self.state)!r})
state=json.loads(state_path.read_text())
gpu=FakeGPU(state['mode'], state['instances'], state.get('display','Compute'))
gpu.calls=state.get('calls',[])
gpu.query_failure=state.get('query_failure',False)
name=Path(sys.argv[0]).name
result=gpu.run(['selector' if name=='selector' else name]+sys.argv[1:])
state.update(mode=gpu.mode,instances=len(gpu.instances),display=gpu.display,calls=gpu.calls)
state_path.write_text(json.dumps(state))
print(result.stdout,end='');print(result.stderr,end='',file=sys.stderr)
sys.exit(result.returncode)
"""
        for name in ["nvidia-smi", "selector", "systemctl"]:
            path = self.directory / name
            path.write_text(command_double)
            path.chmod(0o755)
        self.environment = dict(os.environ, PATH=str(self.directory) + os.pathsep + os.environ["PATH"])

    def tasks(self):
        selected = []
        for filename, name in [
            ("enable_mig.yaml", "Enable MIG only where it is currently disabled"),
            ("apply_mig.yaml", "Create only missing MIG instances and preserve prepared GPUs"),
        ]:
            tasks = yaml.safe_load((ROOT / "roles/gpu_mig/tasks" / filename).read_text())
            task = next(t for t in tasks if t["name"] == name)
            expression = task["ansible.builtin.command"]["argv"]
            task["ansible.builtin.command"]["argv"] = expression.replace(
                "'/usr/local/sbin/gpu-mig-configure.py'",
                repr(sys.executable) + ", " + repr(str(ROOT / "roles/gpu_mig/files/gpu-mig-configure.py")),
            )
            selected.append(task)
        return selected

    def run_play(self, tasks, check=False):
        variables = {
            "gpu_mig_enable_mig_workflow": True,
            "gpu_mig_apply_mig_partition": True,
            "gpu_mig_mig_capable_gpu_count": 1,
            "gpu_mig_mig_mode_by_index": {"stdout_lines": ["0, Enabled", "1, N/A"]},
            "gpu_mig_mig_gpu_indices": ["0"],
            "gpu_mig_mig_selector_remote_dir": str(self.directory),
            "gpu_mig_mig_profile_id": "67",
            "gpu_mig_mig_instance_count": 4,
        }
        # The selector executable's path is constructed by the production task.
        shutil.copyfile(self.directory / "selector", self.directory / "displaymodeselector")
        (self.directory / "displaymodeselector").chmod(0o755)
        play = self.directory / "play.yaml"
        play.write_text(yaml.safe_dump([{
            "hosts": "localhost", "connection": "local", "gather_facts": False,
            "vars": variables, "tasks": tasks,
        }], sort_keys=False))
        result = subprocess.run(
            [ANSIBLE, "-i", "localhost,", str(play)] + (["--check"] if check else []),
            env=self.environment, capture_output=True, text=True, timeout=60,
        )
        return result

    def test_prepared_gpu_is_unchanged_in_actual_tasks(self):
        result = self.run_play(self.tasks())
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("changed=0", result.stdout)
        calls = json.loads(self.state.read_text())["calls"]
        self.assertFalse(any(c[0] == "systemctl" or "-mig" in c or "-cgi" in c for c in calls))
        self.assertTrue(all("1" not in c for c in calls if "-i" in c))

    def test_check_mode_does_not_run_gpu_helper(self):
        result = self.run_play(self.tasks(), check=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("calls", json.loads(self.state.read_text()))

    def test_query_failure_is_reported_without_json_parse_error(self):
        self.state.write_text(json.dumps({"mode": "Enabled", "instances": 4, "query_failure": True}))
        result = self.run_play(self.tasks())
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Driver unavailable", result.stdout + result.stderr)
        self.assertNotIn("Unable to convert", result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
