"""Verify that CDI and k3s startup gates honor MIG readiness."""

from pathlib import Path
import os
import shlex
import shutil
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[1]
PREFLIGHT = ROOT / "src/solution/roles/config/files/verify-mig-before-cdi.sh"


class PreflightTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="eva-mig-cdi-preflight-")
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.link = root / "multi-user.target.wants/gpu-mig-auto-enable.service"
        self.boot_script = root / "gpu-mig-auto-enable.sh"
        self.marker = root / "verify-invoked"
        script = (PREFLIGHT.read_text()
                  .replace("/etc/systemd/system/multi-user.target.wants/gpu-mig-auto-enable.service", str(self.link))
                  .replace("/usr/local/sbin/gpu-mig-auto-enable.sh", str(self.boot_script)))
        self.preflight = root / "verify-mig-before-cdi.sh"
        self.preflight.write_text(script)
        self.preflight.chmod(0o755)

    def run_preflight(self):
        return subprocess.run([str(self.preflight)], capture_output=True, text=True)

    def enable_mig_unit(self, exit_code):
        self.link.parent.mkdir(parents=True)
        self.link.symlink_to(self.boot_script)
        self.boot_script.write_text(
            f"#!/bin/sh\n[ \"$1\" = verify ] || exit 90\nprintf called > {self.marker}\nexit {exit_code}\n"
        )
        self.boot_script.chmod(0o755)

    def test_non_mig_host_skips_check(self):
        self.assertEqual(self.run_preflight().returncode, 0)
        self.assertFalse(self.marker.exists())

    def test_enabled_mig_host_must_verify_successfully(self):
        self.enable_mig_unit(0)
        self.assertEqual(self.run_preflight().returncode, 0)
        self.assertTrue(self.marker.exists())

    def test_enabled_mig_host_rejects_failed_gpu_check(self):
        self.enable_mig_unit(7)
        self.assertEqual(self.run_preflight().returncode, 7)
        self.assertTrue(self.marker.exists())

    def test_enabled_mig_host_rejects_missing_boot_script(self):
        self.link.parent.mkdir(parents=True)
        self.link.symlink_to(self.boot_script)
        self.assertNotEqual(self.run_preflight().returncode, 0)


@unittest.skipUnless(shutil.which("ansible-playbook") or os.environ.get("EVA_TEST_ANSIBLE"),
                     "ansible-playbook is required")
class AnsibleAndSystemdTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="eva-cdi-startup-test-")
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.generator = self.directory / "nvidia-ctk"
        self.output = self.directory / "nvidia.yaml"
        self.generator.write_text(
            f"#!/bin/sh\nprintf generated > {self.output}\n"
        )
        self.generator.chmod(0o755)
        self.preflight = self.directory / "verify-mig-before-cdi.sh"
        self.playbook = os.environ.get("EVA_TEST_ANSIBLE", shutil.which("ansible-playbook"))
        self.tasks = yaml.safe_load((ROOT / "src/solution/roles/config/tasks/gpu_cdi.yaml").read_text())

    def run_generation(self, preflight_exit):
        self.preflight.write_text(f"#!/bin/sh\nexit {preflight_exit}\n")
        self.preflight.chmod(0o755)
        names = ["Verify configured MIG devices before generating CDI", "Generate NVIDIA CDI spec"]
        chosen = []
        for name in names:
            task = dict(next(t for t in self.tasks if t["name"] == name))
            task["ansible.builtin.command"] = (
                str(self.preflight) if name == names[0]
                else f"{self.generator} cdi generate --output={self.output}"
            )
            chosen.append(task)
        play = self.directory / "play.yaml"
        play.write_text(yaml.safe_dump([{
            "hosts": "localhost", "connection": "local", "gather_facts": False,
            "tasks": chosen,
        }], sort_keys=False))
        return subprocess.run([self.playbook, "-i", "localhost,", str(play)],
                              capture_output=True, text=True, timeout=30)

    def test_failed_preflight_blocks_ansible_cdi_generation(self):
        result = self.run_generation(7)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.output.exists())

    def test_successful_preflight_allows_ansible_cdi_generation(self):
        result = self.run_generation(0)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.output.read_text(), "generated")

    def test_k3s_start_checks_cdi_service_result(self):
        task = next(t for t in self.tasks if t["name"] == "Require a successful CDI refresh before k3s starts")
        unit = task["ansible.builtin.copy"]["content"]
        self.assertIn("Wants=nvidia-cdi-refresh.service", unit)
        self.assertIn("After=nvidia-cdi-refresh.service", unit)
        line = next(line for line in unit.splitlines() if line.startswith("ExecStartPre="))
        command = shlex.split(line.partition("=")[2])
        systemctl = self.directory / "systemctl"
        systemctl.write_text(
            '#!/bin/sh\n'
            'case "$*" in\n'
            '  *LoadState*) printf "%s\\n" "$MOCK_CDI_LOAD_STATE" ;;\n'
            '  *Result*) printf "%s\\n" "$MOCK_CDI_RESULT" ;;\n'
            '  *) exit 2 ;;\n'
            'esac\n'
        )
        systemctl.chmod(0o755)
        for load_state, status, expected in [
            ("loaded", "success", 0),
            ("loaded", "exit-code", 1),
            ("not-found", "success", 1),
        ]:
            env = dict(os.environ, PATH=str(self.directory) + os.pathsep + os.environ["PATH"],
                       MOCK_CDI_RESULT=status, MOCK_CDI_LOAD_STATE=load_state)
            result = subprocess.run(command, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, expected)


if __name__ == "__main__":
    unittest.main()
